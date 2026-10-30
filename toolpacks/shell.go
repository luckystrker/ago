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
// ag2/tools/shell/tool.py SandboxShellTool over sandbox/local.py LocalSandbox —
// the local backend only; Docker/Daytona backends are deferred, see
// docs/DECISIONS.md).
//
// A ShellTool is safe for concurrent use after construction: every call runs its
// own subprocess and the configuration is immutable. Close removes an auto-created
// working directory; a caller-supplied workdir is never deleted.
type ShellTool struct {
	toolName    string
	description string
	workdir     string
	autoDir     bool
	timeout     time.Duration
	env         map[string]string
	maxOutput   int
	blocked     []string
	tool        *tools.FunctionTool
}

var _ tools.Tool = (*ShellTool)(nil)

// ShellOption configures NewShellTool.
type ShellOption func(*ShellTool) error

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
		s.timeout = d
		return nil
	}
}

// WithShellEnv adds environment variables merged over the parent environment for
// every command (Python parity: LocalEnvironment env_vars).
func WithShellEnv(env map[string]string) ShellOption {
	return func(s *ShellTool) error {
		for k, v := range env {
			if k == "" {
				return errors.New("toolpacks: shell env key must not be empty")
			}
			s.env[k] = v
		}
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
		return nil
	}
}

// WithShellBlockPatterns refuses commands containing any of the patterns,
// case-insensitive substring match. Python matches its blocked list word by word
// against the argv in restricted mode, which only exists because the restricted
// argv runs without a shell; the Go tool always runs through a shell, where a
// word match cannot see what the shell expands, so the patterns are matched
// against the raw command line instead. Like Python's own caveat says, this is an
// advisory filter, not a security boundary — use an isolated container for that.
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
	description := s.description
	if description == "" {
		description = "Execute a shell command in the working directory: " + s.workdir
	}
	type shellArgs struct {
		Command string `json:"command" agschema:"description=The shell command to run,required"`
	}
	fn := func(args shellArgs) (string, error) { return s.run(args.Command), nil }
	ft, err := tools.NewFunctionTool(s.toolName, description, fn)
	if err != nil {
		return nil, fmt.Errorf("toolpacks: shell tool: %w", err)
	}
	s.tool = ft
	return s, nil
}

// run executes one command and returns the model-facing output (Python parity:
// ShellAdapter.run — a refusal is an ordinary output line, not an error).
func (s *ShellTool) run(command string) string {
	lower := strings.ToLower(command)
	for _, p := range s.blocked {
		if strings.Contains(lower, strings.ToLower(p)) {
			return fmt.Sprintf("Command not allowed: %q", command)
		}
	}
	res := runLocalCommand(shellArgv(command), s.workdir, s.env, s.timeout, s.maxOutput)
	return formatExit(res)
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

// Close removes an auto-created working directory; it is idempotent and leaves a
// caller-supplied workdir alone (Python parity: LocalEnvironment cleanup, minus
// the atexit backstop a Go library cannot register).
func (s *ShellTool) Close() error {
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
