package toolpacks

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
)

// ShellTool runs shell commands as local subprocesses (Python parity:
// ag2/tools/shell/tool.py SandboxShellTool over sandbox/local.py LocalSandbox).
// By default it runs commands as local subprocesses; WithShellSandbox runs them
// in any other backend on the Sandbox seam (sandbox.go).
//
// Security contract: the command line comes from the model and executes on this
// host without isolation — real filesystem, real network, real credentials the
// child environment carries. The tool is a convenience, not a security
// boundary; the block patterns are advisory and bypassable. Gate it with human
// approval middleware and hand secrets only through WithShellEnv.
//
// A ShellTool is safe for concurrent use after construction: every call runs its
// own subprocess and the configuration is immutable. A call runs under the
// current turn context (upstream parity 153ef4cdd): canceling the turn kills the
// running command tree, and the interrupted call reports ErrCommandInterrupted
// instead of a result. Close stops every in-flight command and waits for it
// before removing an auto-created working directory; a caller-supplied workdir
// is never deleted. A call racing Close fails with ErrSandboxClosedWhileRunning,
// a call that starts after Close with ErrSandboxClosed.
type ShellTool struct {
	toolName    string
	description string
	workdir     string
	autoDir     bool
	timeout     time.Duration
	env         map[string]string
	inheritEnv  bool
	maxOutput   int
	blocked     []string
	allowed     []string
	ignore      []string
	restricted  bool
	readonly    bool
	middleware  []middleware.Middleware
	tool        *tools.FunctionTool
	sandbox     localSandbox
	// factory, when set by WithShellSandbox, runs every command in a pluggable
	// backend instead of a local subprocess; localOnly names the options that
	// only configure the local path and are therefore refused alongside it.
	factory    SandboxFactory
	localOnly  []string
	timeoutSet bool
}

var _ tools.Tool = (*ShellTool)(nil)

// ShellOption configures NewShellTool.
type ShellOption func(*ShellTool) error

// WithShellSandbox runs every command in a pluggable execution backend (a
// container, a cloud sandbox, an in-memory fake) instead of a local subprocess
// (Python parity: SandboxShellTool(environment)). Command filters
// (WithShellAllowed, WithShellReadonly, WithShellBlockPatterns, WithShellIgnore),
// the timeout and the middleware apply unchanged; the options that configure
// the local subprocess (WithShellWorkdir, WithShellEnv, WithShellInheritEnv,
// WithShellMaxOutput) belong to the backend and make NewShellTool fail when
// combined with this one. An unrestricted command reaches the backend as
// ["sh", "-c", command]; a restricted one as its checked argv. Close closes the
// factory when it owns resources (io.Closer).
func WithShellSandbox(factory SandboxFactory) ShellOption {
	return func(s *ShellTool) error {
		if factory == nil {
			return errors.New("toolpacks: shell sandbox factory must not be nil")
		}
		s.factory = factory
		return nil
	}
}

// WithShellName overrides the tool name (Python parity: the name kwarg).
func WithShellName(name string) ShellOption {
	return func(s *ShellTool) error {
		if name == "" {
			return errors.New("toolpacks: shell tool name must not be empty")
		}
		s.toolName = name
		return nil
	}
}

// WithShellDescription overrides the tool description verbatim (the default one
// names the working directory; Python parity: the description kwarg, with the
// {workdir} placeholder filled in).
func WithShellDescription(description string) ShellOption {
	return func(s *ShellTool) error { s.description = description; return nil }
}

// WithShellWorkdir sets the directory commands run in (Python parity:
// LocalEnvironment(path)). A missing directory is created; Close never deletes a
// caller-supplied workdir.
func WithShellWorkdir(dir string) ShellOption {
	return func(s *ShellTool) error {
		abs, err := absDir(dir)
		if err != nil {
			return err
		}
		s.workdir, s.autoDir = abs, false
		s.localOnly = append(s.localOnly, "WithShellWorkdir")
		return nil
	}
}

// WithShellTimeout bounds one command (Python parity: LocalEnvironment timeout).
// Must be positive; defaults to 60s.
func WithShellTimeout(d time.Duration) ShellOption {
	return func(s *ShellTool) error {
		if d <= 0 {
			return errors.New("toolpacks: shell timeout must be > 0")
		}
		s.timeout, s.timeoutSet = d, true
		return nil
	}
}

// WithShellEnv adds environment variables merged over the inherited environment
// for every command (Python parity: LocalEnvironment env_vars). Use this to pass
// specific variables (a secret the command needs, a runner configuration) instead
// of opening the whole parent environment.
func WithShellEnv(env map[string]string) ShellOption {
	return func(s *ShellTool) error {
		for k, v := range env {
			if k == "" {
				return errors.New("toolpacks: shell env key must not be empty")
			}
			s.env[k] = v
		}
		s.localOnly = append(s.localOnly, "WithShellEnv")
		return nil
	}
}

// WithShellInheritEnv restores full parent-environment inheritance for every
// command (the Python default). Opt-in only: the command line comes from the
// model, and with the full set one `env` hands it every secret of this process
// (API keys included). Without this option the child sees just the whitelisted
// non-secret variables plus whatever WithShellEnv passed.
func WithShellInheritEnv() ShellOption {
	return func(s *ShellTool) error {
		s.inheritEnv = true
		s.localOnly = append(s.localOnly, "WithShellInheritEnv")
		return nil
	}
}

// WithShellMaxOutput caps the reported output in characters; the excess is cut
// with a trailing notice (Python parity: LocalSandbox max_output).
func WithShellMaxOutput(n int) ShellOption {
	return func(s *ShellTool) error {
		if n <= 0 {
			return errors.New("toolpacks: shell max_output must be > 0")
		}
		s.maxOutput = n
		s.localOnly = append(s.localOnly, "WithShellMaxOutput")
		return nil
	}
}

// WithShellBlockPatterns refuses commands containing any of the patterns,
// case-insensitive substring match. Python matches its blocked list word by word
// against the argv in restricted mode, which only exists because the restricted
// argv runs without a shell; the unrestricted Go tool runs through a shell,
// where a word match cannot see what the shell expands, so the patterns are
// matched against the raw command line instead. In restricted mode (switched on
// by WithShellAllowed, WithShellReadonly or WithShellIgnore) the same list is
// matched word by word against the checked argv, including the program's base
// name, exactly as Python does.
// Like Python's own caveat says, this is an advisory filter, not a security
// boundary — use an isolated container for that. It is bypassable by design
// (a program that runs another program, an encoding trick, a substring that
// never appears literally). The real limiter is WithShellAllowed (restricted
// mode, checked argv executed without a shell); use it, not this list, when
// the command must be constrained.
// See docs/DECISIONS.md.
func WithShellBlockPatterns(patterns ...string) ShellOption {
	return func(s *ShellTool) error {
		for _, p := range patterns {
			if p == "" {
				return errors.New("toolpacks: shell block pattern must not be empty")
			}
		}
		s.blocked = append(s.blocked, patterns...)
		return nil
	}
}

// WithShellAllowed sets a whitelist of command prefixes and switches on
// restricted mode: the command is split into argv once and executed without a
// shell, so pipes, redirects, globs, variables and brace expansion are not
// available and nothing can run after the check (Python parity: the allowed
// kwarg and ShellAdapter.restricted).
//
// A prefix is matched word by word: "git log" matches "git log -5" but not
// "git -c x log"; "git" matches every git invocation.
func WithShellAllowed(prefixes ...string) ShellOption {
	return func(s *ShellTool) error {
		if len(prefixes) == 0 {
			return errors.New("toolpacks: shell allow list must not be empty")
		}
		for _, p := range prefixes {
			if strings.TrimSpace(p) == "" {
				return errors.New("toolpacks: shell allow pattern must not be empty")
			}
		}
		s.allowed = append(s.allowed, prefixes...)
		return nil
	}
}

// WithShellReadonly restricts the tool to the commands that cannot write files
// or run other programs (cat, ls, grep, ...; see ReadonlyCommands). It switches
// on restricted mode. An explicit WithShellAllowed list takes precedence
// (Python parity: readonly is ignored when allowed is given).
func WithShellReadonly() ShellOption {
	return func(s *ShellTool) error { s.readonly = true; return nil }
}

// WithShellIgnore refuses a command whose literal path arguments leave the
// working directory or match one of the glob patterns (Python parity: the
// ignore kwarg and filter.check_ignore). It switches on restricted mode, so the
// checked argv is exactly what runs. Tokens are read with POSIX quoting and
// resolved through symlinks, so "cat ../secret", "cat .env" (with ".env"
// ignored) and "prog --output=.env" are all refused before execution.
func WithShellIgnore(patterns ...string) ShellOption {
	return func(s *ShellTool) error {
		if len(patterns) == 0 {
			return errors.New("toolpacks: shell ignore list must not be empty")
		}
		for _, p := range patterns {
			if p == "" {
				return errors.New("toolpacks: shell ignore pattern must not be empty")
			}
		}
		s.ignore = append(s.ignore, patterns...)
		return nil
	}
}

// WithShellMiddleware adds tool-scoped middleware, the Go spelling of the
// Python tool's middleware= kwarg: the first entry is the outermost layer of
// the tool's own chain, and agent middleware still wraps it from outside. Use
// it for per-tool approval, telemetry or argument rewriting without wrapping
// every tool the agent has.
func WithShellMiddleware(mw ...middleware.Middleware) ShellOption {
	return func(s *ShellTool) error {
		for _, m := range mw {
			if m == nil {
				return errors.New("toolpacks: shell middleware must not be nil")
			}
		}
		s.middleware = append(s.middleware, mw...)
		return nil
	}
}

// NewShellTool builds the run_shell_command tool: local execution through the
// platform shell (cmd /c on Windows, sh -c elsewhere). The result carries stdout
// and stderr; a non-zero exit appends an "[exit code: N]" line, a timeout is
// reported with exit code 124, a refused command is an ordinary result string.
func NewShellTool(opts ...ShellOption) (*ShellTool, error) {
	dir, err := os.MkdirTemp("", "ago_shell_")
	if err != nil {
		return nil, fmt.Errorf("toolpacks: shell workdir: %w", err)
	}
	s := &ShellTool{
		toolName:  "run_shell_command",
		workdir:   dir,
		autoDir:   true,
		timeout:   defaultExecTimeout,
		env:       map[string]string{},
		maxOutput: defaultMaxOutput,
	}
	for _, opt := range opts {
		if err := opt(s); err != nil {
			return nil, err
		}
	}
	// readonly is a preset for the whitelist: only applied when the caller gave
	// no explicit allowed list (Python parity: a readonly flag never overrides
	// an allowed list). Any filter switches on restricted mode.
	//
	// AGO-DEVIATION: Python treats a blocked list alone as restricted mode too.
	// The Go WithShellBlockPatterns option predates restricted mode and its
	// documented substring contract is kept for existing callers, so only
	// WithShellAllowed, WithShellReadonly and WithShellIgnore switch it on. In
	// restricted mode the blocked list is matched word by word, as in Python.
	if s.factory != nil {
		if len(s.localOnly) > 0 {
			_ = os.RemoveAll(dir) // the unused auto workdir
			return nil, fmt.Errorf("toolpacks: shell: %s configure the local subprocess and cannot be combined with WithShellSandbox", strings.Join(s.localOnly, ", "))
		}
		// The backend owns the working directory; the tool only reads it for the
		// description and the ignore filter.
		if s.autoDir {
			_ = os.RemoveAll(s.workdir)
		}
		s.workdir, s.autoDir = factoryWorkdir(s.factory), false
	}
	if s.readonly && s.allowed == nil {
		s.allowed = ReadonlyCommands()
	}
	if s.allowed != nil || s.ignore != nil {
		s.restricted = true
	}
	description := s.description
	if description == "" && s.factory != nil {
		description = "Execute a shell command in the sandbox working directory: " + s.workdir + "."
		if s.restricted {
			description += " Restricted mode: the command runs without a shell, so pipes, redirects, " +
				"chaining, globs and variables are not available."
		}
	}
	if description == "" {
		// AGO-DEVIATION: Python's default description names only the workdir;
		// AGo's default also states the execution contract, because the model
		// reads this text and must not assume an isolated or disposable
		// environment (security audit 2026-10-31, C-1).
		description = "Execute an arbitrary shell command on this machine in the working directory: " +
			s.workdir + ". The command is model-controlled and runs without isolation, with full " +
			"filesystem and network access. This tool is not a security boundary; every call " +
			"requires human approval."
		if s.restricted {
			description += " Restricted mode: the command runs without a shell, so pipes, redirects, " +
				"chaining, globs and variables are not available."
		}
	}
	type shellArgs struct {
		Command string `json:"command" agschema:"description=The shell command to run,required"`
	}
	fn := func(args shellArgs, cx *agocontext.Context) (string, error) { return s.run(cx, args.Command) }
	ft, err := tools.NewFunctionTool(s.toolName, description, fn, tools.WithMiddleware(s.middleware...))
	if err != nil {
		return nil, fmt.Errorf("toolpacks: shell tool: %w", err)
	}
	s.tool = ft
	return s, nil
}

// run executes one command and returns the model-facing output (Python parity:
// ShellAdapter.run — a refusal is an ordinary output line, not an error). The
// command runs under the current turn context: canceling the turn kills the
// process tree and the interrupted call reports ErrCommandInterrupted; on a
// closed tool the call reports ErrSandboxClosed.
func (s *ShellTool) run(cx *agocontext.Context, command string) (string, error) {
	if s.sandbox.isClosed() {
		return "", ErrSandboxClosed
	}
	argv, err := s.argvFor(command)
	if err != nil {
		return fmt.Sprintf("Command not allowed (unbalanced quotes): %q", command), nil
	}
	if s.restricted {
		if denied := s.filter(command, argv); denied != "" {
			return denied, nil
		}
	} else {
		lower := strings.ToLower(command)
		for _, p := range s.blocked {
			if strings.Contains(lower, strings.ToLower(p)) {
				return fmt.Sprintf("Command not allowed: %q", command), nil
			}
		}
	}
	if s.factory != nil {
		return s.runInBackend(cx, argv)
	}
	res, err := s.sandbox.call(cx.TurnContext(), argv, s.workdir, s.env, s.inheritEnv, s.timeout, s.maxOutput)
	if err != nil {
		return "", err
	}
	return formatExit(res), nil
}

// runInBackend executes the checked argv in a sandbox opened from the factory
// (Python parity: ShellAdapter.run — open, exec, format).
func (s *ShellTool) runInBackend(cx *agocontext.Context, argv []string) (string, error) {
	ctx := cx.TurnContext()
	var opts ExecOptions
	if s.timeoutSet {
		opts.Timeout = s.timeout
	}
	var res ExecResult
	err := withSandbox(ctx, cx, s.factory, func(sb Sandbox) error {
		var xerr error
		res, xerr = sb.Exec(ctx, argv, opts)
		return xerr
	})
	if err != nil {
		return "", err
	}
	return formatExit(res), nil
}

// argvFor builds the exec argv for one command. In restricted mode it is the
// checked argv itself, executed without a shell, so nothing can expand or chain
// after the filter (Python parity: ShellAdapter._argv). Unrestricted mode keeps
// the platform shell (Python parity: ["sh","-c",command]).
func (s *ShellTool) argvFor(command string) ([]string, error) {
	if !s.restricted {
		if s.factory != nil {
			return []string{"sh", "-c", command}, nil // backends are POSIX
		}
		return shellArgv(command), nil
	}
	argv, err := splitCommand(command)
	if err != nil {
		return nil, err
	}
	return argv, nil
}

// filter applies the restricted-mode command policy and returns the refusal
// text, or "" when the command may run (Python parity: ShellAdapter._filter).
func (s *ShellTool) filter(command string, argv []string) string {
	if s.allowed != nil {
		permitted := false
		for _, pattern := range s.allowed {
			if matchesArgv(pattern, argv) {
				permitted = true
				break
			}
		}
		if !permitted {
			return fmt.Sprintf("Command not allowed: %q", command)
		}
	}
	if containsShellOperator(command) {
		return fmt.Sprintf("Command not allowed (shell syntax is not available in restricted mode): %q", command)
	}
	if argvBlocked(s.blocked, argv) {
		return fmt.Sprintf("Command not allowed: %q", command)
	}
	if s.ignore != nil {
		if denied := checkIgnore(command, s.workdir, s.ignore); denied != "" {
			return denied
		}
	}
	return ""
}

// shellArgv wraps the command for the platform shell.
func shellArgv(command string) []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/c", command}
	}
	return []string{"sh", "-c", command}
}

// Name returns the tool name.
func (s *ShellTool) Name() string { return s.tool.Name() }

// DeclaredInCode reports a program-declared tool (framework contract).
func (s *ShellTool) DeclaredInCode() bool { return s.tool.DeclaredInCode() }

// Workdir returns the directory commands run in.
func (s *ShellTool) Workdir() string { return s.workdir }

// Close stops every in-flight command and waits for it, then removes an
// auto-created working directory; it is idempotent and leaves a caller-supplied
// workdir alone (Python parity: LocalSandbox.aclose — commands must be dead
// before their workdir goes away, minus the atexit backstop a Go library cannot
// register). A call that Close stopped reports ErrSandboxClosedWhileRunning; a
// call that starts afterwards reports ErrSandboxClosed.
func (s *ShellTool) Close() error {
	s.sandbox.shutdown()
	if s.factory != nil {
		return closeFactory(s.factory)
	}
	if !s.autoDir || s.workdir == "" {
		return nil
	}
	s.autoDir = false
	return os.RemoveAll(s.workdir)
}

// Schemas declares the single run_shell_command function.
func (s *ShellTool) Schemas(cx *agocontext.Context) ([]any, error) { return s.tool.Schemas(cx) }

// Register subscribes the shell execution on the conversation stream.
func (s *ShellTool) Register(cx *agocontext.Context, mw []middleware.Middleware) ([]stream.SubID, error) {
	return s.tool.Register(cx, mw)
}
