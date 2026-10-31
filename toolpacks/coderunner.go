package toolpacks

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
)

// CodeRunner executes code snippets as local subprocesses (Python parity:
// ag2/tools/code/tool.py SandboxCodeTool over sandbox/local.py LocalSandbox +
// adapter CodeAdapter DEFAULT_RUNNERS — the local backend only; Docker/Daytona
// sandboxes are deferred, see docs/DECISIONS.md).
//
// Like Python's SandboxCodeTool requires its backend to be passed explicitly, the
// Go constructor is named LocalCodeRunner so nobody wires model-written code to a
// real shell by accident: this tool runs whatever the model sends on this machine.
//
// Security contract: there is no sandbox. The snippet comes from the model and
// executes on this host with the tool's timeout, output cap and language list as
// the only limits — real filesystem, real network, real credentials the child
// environment carries. The tool is a convenience, not a security boundary; gate
// it with human approval middleware and hand secrets only through WithCodeEnv.
//
// A CodeRunner is safe for concurrent use after construction. Close removes an
// auto-created working directory (Python parity: LocalEnvironment cleanup=True).
type CodeRunner struct {
	toolName   string
	workdir    string
	autoDir    bool
	timeout    time.Duration
	env        map[string]string
	inheritEnv bool
	maxOutput  int
	languages  []string
	pythonBin  string
	tool       *tools.FunctionTool
}

var _ tools.Tool = (*CodeRunner)(nil)

// CodeRunnerOption configures NewLocalCodeRunner.
type CodeRunnerOption func(*CodeRunner) error

// WithCodeName overrides the tool name (Python parity: the name kwarg).
func WithCodeName(name string) CodeRunnerOption {
	return func(r *CodeRunner) error {
		if name == "" {
			return errors.New("toolpacks: code runner name must not be empty")
		}
		r.toolName = name
		return nil
	}
}

// WithCodeWorkdir sets the directory snippets run in (Python parity:
// LocalEnvironment(path)). A missing directory is created; Close never deletes a
// caller-supplied workdir.
func WithCodeWorkdir(dir string) CodeRunnerOption {
	return func(r *CodeRunner) error {
		abs, err := absDir(dir)
		if err != nil {
			return err
		}
		r.workdir, r.autoDir = abs, false
		return nil
	}
}

// WithCodeTimeout bounds one snippet (Python parity: LocalEnvironment timeout).
// Must be positive; defaults to 60s.
func WithCodeTimeout(d time.Duration) CodeRunnerOption {
	return func(r *CodeRunner) error {
		if d <= 0 {
			return errors.New("toolpacks: code runner timeout must be > 0")
		}
		r.timeout = d
		return nil
	}
}

// WithCodeEnv adds environment variables merged over the inherited environment
// for every snippet (Python parity: LocalEnvironment env_vars). Use this to pass
// specific variables (a secret the snippet needs, an interpreter configuration)
// instead of opening the whole parent environment.
func WithCodeEnv(env map[string]string) CodeRunnerOption {
	return func(r *CodeRunner) error {
		for k, v := range env {
			if k == "" {
				return errors.New("toolpacks: code runner env key must not be empty")
			}
			r.env[k] = v
		}
		return nil
	}
}

// WithCodeInheritEnv restores full parent-environment inheritance for every
// snippet (the Python default). Opt-in only: the snippet comes from the model,
// and with the full set one `os.environ` dump hands it every secret of this
// process (API keys included). Without this option the child sees just the
// whitelisted non-secret variables plus whatever WithCodeEnv passed.
func WithCodeInheritEnv() CodeRunnerOption {
	return func(r *CodeRunner) error { r.inheritEnv = true; return nil }
}

// WithCodeMaxOutput caps the reported output in characters (Python parity:
// LocalSandbox max_output).
func WithCodeMaxOutput(n int) CodeRunnerOption {
	return func(r *CodeRunner) error {
		if n <= 0 {
			return errors.New("toolpacks: code runner max_output must be > 0")
		}
		r.maxOutput = n
		return nil
	}
}

// WithCodeLanguages restricts the accepted languages. Must be a subset of the
// built-in runners (python, bash, javascript); "sh" is accepted as an alias of
// "bash" (Python parity: the languages kwarg — Literal python/bash/javascript/
// typescript; the TypeScript runner needs ts-node and is deferred).
func WithCodeLanguages(langs ...string) CodeRunnerOption {
	return func(r *CodeRunner) error {
		if len(langs) == 0 {
			return errors.New("toolpacks: code runner languages must not be empty")
		}
		for _, l := range langs {
			if builtinCodeLanguage(l) == "" {
				return fmt.Errorf("toolpacks: code runner: unsupported language %q", l)
			}
		}
		r.languages = langs
		return nil
	}
}

// WithCodePythonBin pins the python interpreter binary (default: python3, then
// python, then py — whichever LookPath finds first).
func WithCodePythonBin(bin string) CodeRunnerOption {
	return func(r *CodeRunner) error {
		if bin == "" {
			return errors.New("toolpacks: code runner python binary must not be empty")
		}
		r.pythonBin = bin
		return nil
	}
}

// NewLocalCodeRunner builds the run_code tool: snippets execute in a
// (non-isolated) local working directory through the platform interpreter —
// python -c, bash -c, or a node script file. The result carries stdout and
// stderr; a non-zero exit appends an "[exit code: N]" line.
func NewLocalCodeRunner(opts ...CodeRunnerOption) (*CodeRunner, error) {
	dir, err := os.MkdirTemp("", "ago_code_")
	if err != nil {
		return nil, fmt.Errorf("toolpacks: code runner workdir: %w", err)
	}
	r := &CodeRunner{
		toolName:  "run_code",
		workdir:   dir,
		autoDir:   true,
		timeout:   defaultExecTimeout,
		env:       map[string]string{},
		maxOutput: defaultMaxOutput,
		languages: []string{"python", "bash"},
	}
	for _, opt := range opts {
		if err := opt(r); err != nil {
			return nil, err
		}
	}
	// AGO-DEVIATION: Python's default description says "Execute code in a
	// sandboxed environment"; the local backend has no sandbox, and the wording
	// misleads the model into treating the host as isolated and disposable.
	// AGo states the real contract instead (security audit 2026-10-31, C-2).
	description := "Execute code directly on this machine in the working directory: " +
		r.workdir + ". The snippet is model-controlled and runs without isolation, with full " +
		"filesystem and network access. This tool is not a security boundary; every call " +
		"requires human approval. Supported languages: " + strings.Join(r.languages, ", ") + "."
	type codeArgs struct {
		Code string `json:"code" agschema:"description=The code to execute,required"`
		// Both fields are mandatory: untagged non-pointer fields become required
		// arguments (Python parity: run_code(code, language) with no defaults).
		Language string `json:"language" agschema:"description=The language of the snippet,required"`
	}
	fn := func(args codeArgs) (string, error) { return r.run(args.Code, args.Language), nil }
	ft, err := tools.NewFunctionTool(r.toolName, description, fn)
	if err != nil {
		return nil, fmt.Errorf("toolpacks: code runner: %w", err)
	}
	r.tool = ft
	return r, nil
}

// builtinCodeLanguage normalizes a language name to a supported one; "" when
// unsupported. "sh" is an alias of "bash".
func builtinCodeLanguage(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "python", "py":
		return "python"
	case "bash", "sh":
		return "bash"
	case "javascript", "js":
		return "javascript"
	}
	return ""
}

// run executes one snippet (Python parity: CodeAdapter.run + SandboxCodeTool
// run_code formatting). An unsupported language or a missing interpreter is an
// ordinary output line, not a tool error.
func (r *CodeRunner) run(code, language string) string {
	lang := builtinCodeLanguage(language)
	if lang == "" {
		return fmt.Sprintf("Unsupported language: %q. Supported languages: %s.",
			language, strings.Join(r.languages, ", "))
	}
	allowed := false
	for _, l := range r.languages {
		if builtinCodeLanguage(l) == lang {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Sprintf("Language %q is not enabled. Supported languages: %s.",
			language, strings.Join(r.languages, ", "))
	}
	argv, err := r.argv(lang, code)
	if err != nil {
		return err.Error()
	}
	return formatExit(runLocalCommand(argv, r.workdir, r.env, r.inheritEnv, r.timeout, r.maxOutput))
}

// argv builds the subprocess command for one snippet (Python parity:
// DEFAULT_RUNNERS — python/bash inline, javascript through a file written into
// the workdir).
func (r *CodeRunner) argv(lang, code string) ([]string, error) {
	switch lang {
	case "python":
		bin := r.pythonBin
		if bin == "" {
			bin = findPython()
		}
		if bin == "" {
			return nil, errors.New("Command not found: python (no python3/python/py on PATH)")
		}
		return []string{bin, "-c", code}, nil
	case "bash":
		// bash first (Python parity: inline_argv=("bash", "-c")), sh as the
		// portable fallback on systems without bash; findShell has verified the
		// interpreter actually executes.
		if sh := findShell(); sh != "" {
			return []string{sh, "-c", code}, nil
		}
		return nil, errors.New("Command not found: bash")
	default: // javascript
		name := fmt.Sprintf("ago_code_%d.js", time.Now().UnixNano())
		path := filepath.Join(r.workdir, name)
		if err := os.WriteFile(path, []byte(code), 0o600); err != nil {
			return nil, fmt.Errorf("writing snippet: %w", err)
		}
		return []string{"node", path}, nil
	}
}

// findShell reports a POSIX shell able to execute snippets, "" when none works
// (Python parity: inline_argv=("bash", "-c"), sh as the fallback). Candidates are
// verified by execution rather than by name lookup alone: on Windows the first
// bash on PATH is often the WSL launcher stub, which LookPath accepts but cannot
// run when no distribution provides /bin/bash. The Git for Windows install is
// discovered through git.exe the way findPython applies Windows conventions.
// Cached for the process lifetime: one probe per process, not per snippet.
var findShell = sync.OnceValue(func() string {
	dirs := shellDirs()
	for _, name := range []string{"bash", "sh"} {
		for _, dir := range dirs {
			if runtime.GOOS == "windows" {
				// Windows cannot execute an extensionless file; the command
				// name resolves through the executable extensions.
				if p := filepath.Join(dir, name+".exe"); isExecutableFile(p) && shellWorks(p) {
					return p
				}
				continue
			}
			if p := filepath.Join(dir, name); isExecutableFile(p) && shellWorks(p) {
				return p
			}
		}
	}
	return ""
})

// shellDirs lists the directories searched for a shell, best candidate first:
// the shell directories of a Git for Windows install ahead of PATH, where
// otherwise the WSL launcher stub in System32 wins.
func shellDirs() []string {
	var dirs []string
	if runtime.GOOS == "windows" {
		if git, err := exec.LookPath("git"); err == nil {
			// …\Git\cmd\git.exe implies …\Git\usr\bin and …\Git\bin.
			root := filepath.Dir(filepath.Dir(git))
			for _, sub := range []string{filepath.Join("usr", "bin"), "bin"} {
				d := filepath.Join(root, sub)
				if fi, err := os.Stat(d); err == nil && fi.IsDir() {
					dirs = append(dirs, d)
				}
			}
		}
	}
	return append(dirs, filepath.SplitList(os.Getenv("PATH"))...)
}

// isExecutableFile reports whether path is a regular executable file (on Unix
// this includes the executable permission bits).
func isExecutableFile(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	return runtime.GOOS == "windows" || fi.Mode().Perm()&0o111 != 0
}

// shellWorks reports whether the shell executes a trivial command; the WSL
// launcher stub fails here, printing its diagnostics and exiting non-zero.
func shellWorks(shell string) bool {
	return runLocalCommand([]string{shell, "-c", "exit 0"}, "", nil, false, shellProbeTimeout, defaultMaxOutput).ExitCode == 0
}

// shellProbeTimeout bounds one findShell verification.
const shellProbeTimeout = 5 * time.Second

// findPython discovers a python interpreter on PATH (Windows convention first
// where it applies). Empty when none is found — the caller reports it the way a
// missing command is reported everywhere else in this package.
func findPython() string {
	names := []string{"python3", "python"}
	if runtime.GOOS == "windows" {
		names = []string{"python", "py", "python3"}
	}
	for _, n := range names {
		if _, err := exec.LookPath(n); err == nil {
			return n
		}
	}
	return ""
}

// Name returns the tool name.
func (r *CodeRunner) Name() string { return r.tool.Name() }

// DeclaredInCode reports a program-declared tool (framework contract).
func (r *CodeRunner) DeclaredInCode() bool { return r.tool.DeclaredInCode() }

// Workdir returns the directory snippets run in.
func (r *CodeRunner) Workdir() string { return r.workdir }

// Close removes an auto-created working directory; idempotent, and a
// caller-supplied workdir is left alone.
func (r *CodeRunner) Close() error {
	if !r.autoDir || r.workdir == "" {
		return nil
	}
	r.autoDir = false
	return os.RemoveAll(r.workdir)
}

// Schemas declares the single run_code function.
func (r *CodeRunner) Schemas(cx *agocontext.Context) ([]any, error) { return r.tool.Schemas(cx) }

// Register subscribes the code execution on the conversation stream.
func (r *CodeRunner) Register(cx *agocontext.Context, mw []middleware.Middleware) ([]stream.SubID, error) {
	return r.tool.Register(cx, mw)
}
