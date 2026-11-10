package toolpacks

import (
	stdcontext "context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync/atomic"
	"time"

	agocontext "github.com/luckystrker/ago/context"
)

// This file is the pluggable execution-backend seam (Python parity:
// ag2/tools/sandbox base.py Sandbox, factory.py SandboxFactory and
// ag2/tools/code/environment CodeEnvironment). A backend — the local
// subprocess runner (LocalSandbox), a container, a cloud sandbox, an in-memory
// fake — implements Sandbox; a SandboxFactory hands one out per call; and
// ShellTool (WithShellSandbox) and CodeRunner (NewSandboxCodeToolFromFactory,
// NewSandboxCodeTool) run through it. Docker and Daytona backends live in
// contrib modules on this seam.

// ErrSandboxUnsupported reports an optional Sandbox operation the backend does
// not provide (Python parity: put_file raising NotImplementedError). A backend
// returns it wrapped, so callers test with errors.Is.
var ErrSandboxUnsupported = errors.New("toolpacks: sandbox does not support this operation")

// ErrSandboxPath reports a file path a sandbox refuses: empty, absolute, or
// escaping the working directory (Python parity: put_file rejects absolute
// paths to keep writes confined to the workdir).
var ErrSandboxPath = errors.New("toolpacks: sandbox path must be relative and stay inside the working directory")

// ExecOptions are the per-call knobs of Sandbox.Exec.
type ExecOptions struct {
	// Env is merged over the sandbox's own environment for this call. Nil
	// leaves the sandbox environment as configured.
	Env map[string]string
	// Timeout bounds the call; zero uses the backend's default. On expiry the
	// backend must stop the process and report ExitCode 124.
	Timeout time.Duration
}

// ExecResult is the outcome of one Sandbox.Exec call (Python parity:
// sandbox.base.ExecResult): the combined stdout+stderr, trimmed and possibly
// truncated by the backend, and a POSIX exit code (0 success, 124 timeout, 127
// command not found).
type ExecResult struct {
	Output   string
	ExitCode int
}

// Sandbox is the low-level execution backend: it runs an argv inside a working
// directory and returns the combined output with an exit code. Shell and code
// semantics are layered on top (ShellTool, CodeAdapter), so a new backend is one
// new Sandbox.
//
// A Sandbox must be safe for concurrent use: one conversation may run several
// tool calls at once. Close releases the backend resources (a container, a
// remote sandbox) and is idempotent; a call after Close should fail rather than
// start work.
//
// Paths of PutFile, GetFile and RemoveFile are slash-separated and relative to
// Workdir.
type Sandbox interface {
	// Workdir is the directory commands run in, as seen inside the sandbox
	// (slash-separated even on a Windows host, so the same path is meaningful
	// across backends).
	Workdir() string
	// HostWorkdir is the host-side view of Workdir, "" when the working
	// directory is not visible on the host (a remote sandbox).
	HostWorkdir() string
	// Exec runs argv. argv is a plain argv list — no shell parsing happens at
	// this layer; callers that need shell features pass ["sh", "-c", cmd]. A
	// non-zero exit or a timeout is a result, not an error; the error return is
	// for a call that produced no result (canceled ctx, closed sandbox,
	// transport failure).
	Exec(ctx stdcontext.Context, argv []string, opts ExecOptions) (ExecResult, error)
	// PutFile uploads content to path inside the sandbox, creating parent
	// directories. Backends without a file API return ErrSandboxUnsupported.
	PutFile(ctx stdcontext.Context, path string, content []byte) error
	// GetFile downloads the file at path. Backends without a file API return
	// ErrSandboxUnsupported.
	GetFile(ctx stdcontext.Context, path string) ([]byte, error)
	// Close releases the backend resources.
	Close() error
}

// FileRemover is the optional Sandbox capability of deleting a file natively.
// Without it, callers fall back to `rm -f` through Exec (Python parity:
// SandboxBase.remove_file). Removing a missing file is not an error.
type FileRemover interface {
	RemoveFile(ctx stdcontext.Context, path string) error
}

// removeSandboxFile deletes path best-effort: natively when the sandbox can,
// otherwise by `rm -f` in its working directory.
func removeSandboxFile(ctx stdcontext.Context, sb Sandbox, p string) error {
	if r, ok := sb.(FileRemover); ok {
		return r.RemoveFile(ctx, p)
	}
	if !validSandboxPath(p) {
		return fmt.Errorf("%w: %q", ErrSandboxPath, p)
	}
	_, err := sb.Exec(ctx, []string{"rm", "-f", path.Join(sb.Workdir(), p)}, ExecOptions{})
	return err
}

// validSandboxPath reports whether p is a usable relative sandbox path:
// non-empty, not absolute, no ".." segment.
func validSandboxPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) || (len(p) >= 2 && p[1] == ':') {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// SandboxFactory produces a Sandbox per call (Python parity: SandboxFactory).
// It is the only place per-conversation parameters (an image, credentials, a
// tenant) are resolved: backends receive concrete values, and a factory reads
// cx.Variable when it needs them. cx may be nil.
//
// Open returns the sandbox together with a release func the caller invokes once
// the call is done — the Go spelling of Python's async context manager, so a
// factory that starts a container per call stops it in release, while a factory
// that shares one long-lived sandbox returns a no-op release.
type SandboxFactory interface {
	Open(ctx stdcontext.Context, cx *agocontext.Context) (sb Sandbox, release func() error, err error)
}

// WorkdirAware is the optional SandboxFactory capability of knowing its working
// directory before any sandbox is opened: ShellTool puts it into the
// description the model reads. A factory without it is assumed to work in the
// conventional /workspace (Python parity: WorkdirAware).
type WorkdirAware interface {
	Workdir() string
}

// defaultSandboxWorkdir is assumed for a factory that does not say otherwise.
const defaultSandboxWorkdir = "/workspace"

// SingletonFactory wraps one Sandbox as a SandboxFactory: every Open yields the
// same sandbox and release is a no-op, so the caller owns the sandbox's
// lifecycle. Close closes the wrapped sandbox (Python parity: SingletonFactory
// plus LocalEnvironment.aclose).
type SingletonFactory struct {
	sb Sandbox
}

var (
	_ SandboxFactory = (*SingletonFactory)(nil)
	_ WorkdirAware   = (*SingletonFactory)(nil)
	_ io.Closer      = (*SingletonFactory)(nil)
)

// NewSingletonFactory wraps sb.
func NewSingletonFactory(sb Sandbox) (*SingletonFactory, error) {
	if sb == nil {
		return nil, errors.New("toolpacks: singleton factory: sandbox must not be nil")
	}
	return &SingletonFactory{sb: sb}, nil
}

// Sandbox returns the wrapped sandbox.
func (f *SingletonFactory) Sandbox() Sandbox { return f.sb }

// Open yields the wrapped sandbox.
func (f *SingletonFactory) Open(_ stdcontext.Context, _ *agocontext.Context) (Sandbox, func() error, error) {
	return f.sb, func() error { return nil }, nil
}

// Workdir reports the wrapped sandbox's working directory: the host path when
// the sandbox has one (so host-side checks work), else the sandbox-side path.
func (f *SingletonFactory) Workdir() string {
	if host := f.sb.HostWorkdir(); host != "" {
		return host
	}
	return f.sb.Workdir()
}

// Close closes the wrapped sandbox.
func (f *SingletonFactory) Close() error { return f.sb.Close() }

// factoryWorkdir is the working directory a tool shows for a factory.
func factoryWorkdir(f SandboxFactory) string {
	if w, ok := f.(WorkdirAware); ok {
		return w.Workdir()
	}
	return defaultSandboxWorkdir
}

// closeFactory closes a factory that owns resources (io.Closer); others have
// nothing to release at this level.
func closeFactory(f SandboxFactory) error {
	if c, ok := f.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// withSandbox opens a sandbox from f for one call and always releases it.
func withSandbox(ctx stdcontext.Context, cx *agocontext.Context, f SandboxFactory, fn func(Sandbox) error) (err error) {
	sb, release, oerr := f.Open(ctx, cx)
	if oerr != nil {
		return oerr
	}
	if release != nil {
		defer func() {
			if rerr := release(); rerr != nil && err == nil {
				err = rerr
			}
		}()
	}
	return fn(sb)
}

// --- code environment ---------------------------------------------------------

// CodeRunResult is the outcome of one CodeEnvironment.Run (Python parity:
// CodeRunResult): combined output and a POSIX exit code.
type CodeRunResult struct {
	Output   string
	ExitCode int
}

// CodeEnvironment is a backend that runs source code on behalf of the code tool
// (Python parity: CodeEnvironment protocol). Implementations may target a local
// subprocess, a container, a remote sandbox, or anything else; CodeAdapter is
// the implementation that works on every Sandbox.
type CodeEnvironment interface {
	// SupportedLanguages lists the languages the environment will run; they are
	// surfaced in the tool description.
	SupportedLanguages() []string
	// Run executes code in language. cx is the active conversation context
	// (nil-able) so a backend can resolve per-conversation Variables.
	Run(ctx stdcontext.Context, cx *agocontext.Context, code, language string) (CodeRunResult, error)
}

// CodeAdapter is the CodeEnvironment over any SandboxFactory (Python parity:
// CodeAdapter): the language matrix lives here once, backends only implement
// Sandbox.Exec, plus PutFile for file-mode languages. It is safe for concurrent
// use when its factory is.
type CodeAdapter struct {
	factory   SandboxFactory
	languages []string
	runners   map[string]LanguageRunner
	timeout   time.Duration
}

var (
	_ CodeEnvironment = (*CodeAdapter)(nil)
	_ io.Closer       = (*CodeAdapter)(nil)
)

// CodeAdapterOption configures NewCodeAdapter.
type CodeAdapterOption func(*CodeAdapter) error

// WithAdapterLanguages restricts the accepted languages (default python and
// bash). Aliases (py, sh, js, ts) are accepted.
func WithAdapterLanguages(langs ...string) CodeAdapterOption {
	return func(a *CodeAdapter) error {
		if len(langs) == 0 {
			return errors.New("toolpacks: code adapter languages must not be empty")
		}
		a.languages = langs
		return nil
	}
}

// WithAdapterRunners overrides or extends the built-in language table (Python
// parity: runners=).
func WithAdapterRunners(runners map[string]LanguageRunner) CodeAdapterOption {
	return func(a *CodeAdapter) error {
		for lang, runner := range runners {
			if strings.TrimSpace(lang) == "" {
				return errors.New("toolpacks: code adapter runner language must not be empty")
			}
			if err := runner.validate(); err != nil {
				return fmt.Errorf("toolpacks: code adapter runner %q: %w", lang, err)
			}
			a.runners[normalizeCodeLanguage(lang)] = runner
		}
		return nil
	}
}

// WithAdapterTimeout sets the per-call timeout passed to Sandbox.Exec; zero
// leaves the backend's default.
func WithAdapterTimeout(d time.Duration) CodeAdapterOption {
	return func(a *CodeAdapter) error {
		if d < 0 {
			return errors.New("toolpacks: code adapter timeout must not be negative")
		}
		a.timeout = d
		return nil
	}
}

// NewCodeAdapter builds a CodeEnvironment over factory.
func NewCodeAdapter(factory SandboxFactory, opts ...CodeAdapterOption) (*CodeAdapter, error) {
	if factory == nil {
		return nil, errors.New("toolpacks: code adapter: sandbox factory must not be nil")
	}
	a := &CodeAdapter{
		factory:   factory,
		languages: []string{"python", "bash"},
		runners:   cloneLanguageRunners(defaultLanguageRunners),
	}
	for _, opt := range opts {
		if err := opt(a); err != nil {
			return nil, err
		}
	}
	for _, l := range a.languages {
		if _, ok := a.runners[normalizeCodeLanguage(l)]; !ok {
			return nil, fmt.Errorf("toolpacks: code adapter: no runner registered for language %q", l)
		}
	}
	return a, nil
}

// SupportedLanguages lists the accepted languages.
func (a *CodeAdapter) SupportedLanguages() []string { return append([]string(nil), a.languages...) }

// Close closes the factory when it owns resources.
func (a *CodeAdapter) Close() error { return closeFactory(a.factory) }

// adapterFileSeq numbers the temp snippet files of file-mode languages.
var adapterFileSeq atomic.Uint64

// Run executes code (Python parity: CodeAdapter.run): inline languages append
// the snippet to their argv; file languages upload it with PutFile, run the
// interpreter on it and remove it best-effort afterwards. A language that is
// not enabled is reported as an exit-2 result, not an error.
func (a *CodeAdapter) Run(ctx stdcontext.Context, cx *agocontext.Context, code, language string) (CodeRunResult, error) {
	lang := normalizeCodeLanguage(language)
	enabled := false
	for _, l := range a.languages {
		if normalizeCodeLanguage(l) == lang {
			enabled = true
			break
		}
	}
	if !enabled {
		return CodeRunResult{
			Output:   fmt.Sprintf("Language %q is not enabled. Available: %v", language, a.languages),
			ExitCode: 2,
		}, nil
	}
	runner := a.runners[lang]
	var res ExecResult
	err := withSandbox(ctx, cx, a.factory, func(sb Sandbox) error {
		opts := ExecOptions{Timeout: a.timeout}
		if len(runner.InlineArgv) > 0 {
			var xerr error
			res, xerr = sb.Exec(ctx, append(append([]string(nil), runner.InlineArgv...), code), opts)
			return xerr
		}
		name := fmt.Sprintf("ago_%d_%d.%s", time.Now().UnixNano(), adapterFileSeq.Add(1), runner.FileExtension)
		if perr := sb.PutFile(ctx, name, []byte(code)); perr != nil {
			return perr
		}
		// Best-effort cleanup so a persistent sandbox does not accumulate temp
		// scripts; it never fails the run, and runs even when ctx is canceled.
		defer func() { _ = removeSandboxFile(stdcontext.WithoutCancel(ctx), sb, name) }()
		var xerr error
		res, xerr = sb.Exec(ctx, append(append([]string(nil), runner.FileRunnerArgv...), path.Join(sb.Workdir(), name)), opts)
		return xerr
	})
	if err != nil {
		return CodeRunResult{}, err
	}
	return CodeRunResult{Output: res.Output, ExitCode: res.ExitCode}, nil
}
