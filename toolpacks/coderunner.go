package toolpacks

import (
	stdcontext "context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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
// A CodeRunner is safe for concurrent use after construction. A call runs under
// the current turn context (upstream parity 153ef4cdd): canceling the turn kills
// the running snippet's process tree, and the interrupted call reports
// ErrCommandInterrupted instead of a result. Close stops every in-flight snippet
// and waits for it before removing an auto-created working directory (Python
// parity: LocalEnvironment cleanup=True); a caller-supplied workdir is left
// alone. A call racing Close fails with ErrSandboxClosedWhileRunning, a call
// that starts after Close with ErrSandboxClosed.
type CodeRunner struct {
	toolName   string
	workdir    string
	autoDir    bool
	timeout    time.Duration
	env        map[string]string
	inheritEnv bool
	maxOutput  int
	languages  []string
	runners    map[string]LanguageRunner
	pythonBin  string
	tool       *tools.FunctionTool
	sandbox    localSandbox
}

var _ tools.Tool = (*CodeRunner)(nil)

// CodeRunnerOption configures NewLocalCodeRunner.
type CodeRunnerOption func(*CodeRunner) error

// LanguageRunner describes how one language maps to a subprocess argv
// (Python parity: sandbox/adapter/code.py LanguageRunner). Exactly one mode:
//
//   - Inline: InlineArgv is set; the snippet is appended as the last argument
//     (python -c <code>, bash -c <code>).
//   - File: FileExtension and FileRunnerArgv are set; the snippet is written to
//     a temp file in the workdir and the file path is appended (node <file>,
//     ts-node <file>).
type LanguageRunner struct {
	InlineArgv     []string
	FileExtension  string
	FileRunnerArgv []string
}

// validate enforces the either-inline-or-file contract (Python parity:
// LanguageRunner.__post_init__).
func (r LanguageRunner) validate() error {
	inline := len(r.InlineArgv) > 0
	fileMode := r.FileExtension != "" || len(r.FileRunnerArgv) > 0
	if inline == fileMode {
		return errors.New("toolpacks: LanguageRunner must be either inline (InlineArgv) or file-based (FileExtension + FileRunnerArgv), not both or neither")
	}
	if fileMode && (r.FileExtension == "" || len(r.FileRunnerArgv) == 0) {
		return errors.New("toolpacks: file-mode LanguageRunner requires both FileExtension and FileRunnerArgv")
	}
	return nil
}

// defaultLanguageRunners is the built-in runner table (Python parity:
// DEFAULT_RUNNERS). The python and bash entries are the default spellings:
// their interpreters are resolved on PATH at call time, so a custom runner that
// names an interpreter explicitly keeps that interpreter.
var defaultLanguageRunners = map[string]LanguageRunner{
	"python":     {InlineArgv: []string{"python", "-c"}},
	"bash":       {InlineArgv: []string{"bash", "-c"}},
	"javascript": {FileExtension: "js", FileRunnerArgv: []string{"node"}},
}

// DefaultLanguageRunners returns a copy of the built-in runner table.
func DefaultLanguageRunners() map[string]LanguageRunner {
	return cloneLanguageRunners(defaultLanguageRunners)
}

func cloneLanguageRunners(src map[string]LanguageRunner) map[string]LanguageRunner {
	out := make(map[string]LanguageRunner, len(src))
	for lang, runner := range src {
		runner.InlineArgv = append([]string(nil), runner.InlineArgv...)
		runner.FileRunnerArgv = append([]string(nil), runner.FileRunnerArgv...)
		out[lang] = runner
	}
	return out
}

// WithCodeRunners overrides or extends the built-in runner table (Python
// parity: the runners= kwarg merged over DEFAULT_RUNNERS). Use it to register a
// new language or to pick a different interpreter (tsx instead of ts-node).
// A language registered here can then be enabled with WithCodeLanguages.
func WithCodeRunners(runners map[string]LanguageRunner) CodeRunnerOption {
	return func(r *CodeRunner) error {
		if len(runners) == 0 {
			return errors.New("toolpacks: code runner table must not be empty")
		}
		for lang, runner := range runners {
			if strings.TrimSpace(lang) == "" {
				return errors.New("toolpacks: code runner language must not be empty")
			}
			if err := runner.validate(); err != nil {
				return fmt.Errorf("toolpacks: code runner %q: %w", lang, err)
			}
			r.runners[normalizeCodeLanguage(lang)] = runner
		}
		return nil
	}
}

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

// WithCodeLanguages restricts the accepted languages. A language must have a
// runner in the built-in table or one registered by WithCodeRunners, so the
// option order does not matter: validation happens when the runner is built.
// "sh", "py", "js" and "ts" are accepted aliases of their canonical names
// (Python parity: the languages kwarg).
func WithCodeLanguages(langs ...string) CodeRunnerOption {
	return func(r *CodeRunner) error {
		if len(langs) == 0 {
			return errors.New("toolpacks: code runner languages must not be empty")
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
		runners:   cloneLanguageRunners(defaultLanguageRunners),
	}
	for _, opt := range opts {
		if err := opt(r); err != nil {
			return nil, err
		}
	}
	for _, l := range r.languages {
		if _, ok := r.runners[normalizeCodeLanguage(l)]; !ok {
			return nil, fmt.Errorf("toolpacks: code runner: unsupported language %q", l)
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
	fn := func(args codeArgs, cx *agocontext.Context) (string, error) {
		return r.run(cx, args.Code, args.Language)
	}
	ft, err := tools.NewFunctionTool(r.toolName, description, fn)
	if err != nil {
		return nil, fmt.Errorf("toolpacks: code runner: %w", err)
	}
	r.tool = ft
	return r, nil
}

// normalizeCodeLanguage canonicalizes a language name; "" stays "". Aliases:
// py→python, sh→bash, js→javascript, ts→typescript.
func normalizeCodeLanguage(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "python", "py":
		return "python"
	case "bash", "sh":
		return "bash"
	case "javascript", "js":
		return "javascript"
	case "typescript", "ts":
		return "typescript"
	}
	return strings.ToLower(strings.TrimSpace(lang))
}

// run executes one snippet (Python parity: CodeAdapter.run + SandboxCodeTool
// run_code formatting). An unsupported language or a missing interpreter is an
// ordinary output line, not a tool error. The snippet runs under the current
// turn context: canceling the turn kills the process tree and the interrupted
// call reports ErrCommandInterrupted; on a closed runner the call reports
// ErrSandboxClosed.
func (r *CodeRunner) run(cx *agocontext.Context, code, language string) (string, error) {
	if r.sandbox.isClosed() {
		return "", ErrSandboxClosed
	}
	lang := normalizeCodeLanguage(language)
	runner, ok := r.runners[lang]
	if !ok {
		return fmt.Sprintf("Unsupported language: %q. Supported languages: %s.",
			language, strings.Join(r.languages, ", ")), nil
	}
	allowed := false
	for _, l := range r.languages {
		if normalizeCodeLanguage(l) == lang {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Sprintf("Language %q is not enabled. Supported languages: %s.",
			language, strings.Join(r.languages, ", ")), nil
	}
	argv, err := r.argv(lang, runner, code)
	if err != nil {
		return err.Error(), nil
	}
	res, err := r.sandbox.call(cx.TurnContext(), argv, r.workdir, r.env, r.inheritEnv, r.timeout, r.maxOutput)
	if err != nil {
		return "", err
	}
	return formatExit(res), nil
}

// argv builds the subprocess command for one snippet (Python parity:
// LanguageRunner resolution in DEFAULT_RUNNERS — inline languages append the
// snippet, file languages write it into the workdir and append the path).
func (r *CodeRunner) argv(lang string, runner LanguageRunner, code string) ([]string, error) {
	if len(runner.InlineArgv) > 0 {
		switch lang {
		case "python":
			// The built-in python runner is resolved through findPython so the
			// python3/python/py fallback (and the Windows convention) still
			// applies. A custom runner naming its interpreter is used verbatim.
			if slices.Equal(runner.InlineArgv, []string{"python", "-c"}) {
				bin := r.pythonBin
				if bin == "" {
					bin = findPython()
				}
				if bin == "" {
					return nil, errors.New("Command not found: python (no python3/python/py on PATH)")
				}
				return []string{bin, "-c", code}, nil
			}
		case "bash":
			if slices.Equal(runner.InlineArgv, []string{"bash", "-c"}) {
				if sh := findShell(); sh != "" {
					return []string{sh, "-c", code}, nil
				}
				return nil, errors.New("Command not found: bash")
			}
		}
		return append(append([]string(nil), runner.InlineArgv...), code), nil
	}
	name := fmt.Sprintf("ago_code_%d.%s", time.Now().UnixNano(), runner.FileExtension)
	path := filepath.Join(r.workdir, name)
	if err := os.WriteFile(path, []byte(code), 0o600); err != nil {
		return nil, fmt.Errorf("writing snippet: %w", err)
	}
	return append(append([]string(nil), runner.FileRunnerArgv...), path), nil
}

// findShell reports a POSIX shell able to execute snippets, "" when none works
// (Python parity: inline_argv=("bash", "-c"), sh as the fallback). Candidates are
// verified by execution rather than by name lookup alone: on Windows the first
// bash on PATH is often the WSL launcher stub, which LookPath accepts but cannot
// run when no distribution provides /bin/bash. The Git for Windows install is
// discovered through git.exe the way findPython applies Windows conventions.
// Cached for the process lifetime: one probe per process, not per snippet.
var findShell = sync.OnceValue(func() string {
	return pickShell(shellDirs())
})

// pickShell returns the first candidate in dirs that both exists as an
// executable file and passes the execution probe, "" when none does. The two
// checks are not interchangeable: the file check alone would elect a candidate
// that exists but cannot run a snippet (the WSL launcher stub profile), the
// probe alone would elect one that runs but is not the regular file the lookup
// contract promises.
func pickShell(dirs []string) string {
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
}

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
	res, err := runLocalCommand(stdcontext.Background(), []string{shell, "-c", "exit 0"}, "", nil, false, shellProbeTimeout, defaultMaxOutput)
	return err == nil && res.ExitCode == 0
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

// Close stops every in-flight snippet and waits for it, then removes an
// auto-created working directory; idempotent, and a caller-supplied workdir is
// left alone (Python parity: LocalSandbox.aclose — commands must be dead before
// their workdir goes away). A call that Close stopped reports
// ErrSandboxClosedWhileRunning; a call that starts afterwards reports
// ErrSandboxClosed.
func (r *CodeRunner) Close() error {
	r.sandbox.shutdown()
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
