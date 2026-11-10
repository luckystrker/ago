package toolpacks

import (
	stdcontext "context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// LocalSandbox is the local-subprocess Sandbox (Python parity: sandbox/local.py
// LocalSandbox): argv runs as a child process of this host in one working
// directory, with the same environment whitelist, output cap, timeout and
// process-tree kill as ShellTool and CodeRunner. It offers no isolation — it
// exists so the host is one backend among many on the Sandbox seam, usable by
// CodeAdapter and the skills script runner, and wrappable in a SingletonFactory.
//
// A LocalSandbox is safe for concurrent use. Close stops every in-flight
// command and waits for it, then removes an auto-created working directory; a
// caller-supplied directory is left alone. A call racing Close fails with
// ErrSandboxClosedWhileRunning, a call after it with ErrSandboxClosed.
type LocalSandbox struct {
	mu        sync.Mutex
	dir       string // real (symlink-resolved) working directory
	autoDir   bool
	timeout   time.Duration
	maxOutput int
	env       map[string]string
	inherit   bool
	sb        localSandbox
}

var (
	_ Sandbox     = (*LocalSandbox)(nil)
	_ FileRemover = (*LocalSandbox)(nil)
)

// LocalSandboxOption configures NewLocalSandbox.
type LocalSandboxOption func(*LocalSandbox) error

// WithLocalSandboxDir sets an existing working directory; Close never deletes
// it. Without it the sandbox creates a temporary directory and removes it on
// Close.
func WithLocalSandboxDir(dir string) LocalSandboxOption {
	return func(s *LocalSandbox) error {
		abs, err := absDir(dir)
		if err != nil {
			return err
		}
		s.dir, s.autoDir = abs, false
		return nil
	}
}

// WithLocalSandboxTimeout sets the default per-command timeout (default 60s).
func WithLocalSandboxTimeout(d time.Duration) LocalSandboxOption {
	return func(s *LocalSandbox) error {
		if d <= 0 {
			return errors.New("toolpacks: local sandbox timeout must be > 0")
		}
		s.timeout = d
		return nil
	}
}

// WithLocalSandboxMaxOutput caps the reported output in characters.
func WithLocalSandboxMaxOutput(n int) LocalSandboxOption {
	return func(s *LocalSandbox) error {
		if n <= 0 {
			return errors.New("toolpacks: local sandbox max_output must be > 0")
		}
		s.maxOutput = n
		return nil
	}
}

// WithLocalSandboxEnv adds variables merged over the inherited environment for
// every command.
func WithLocalSandboxEnv(env map[string]string) LocalSandboxOption {
	return func(s *LocalSandbox) error {
		for k, v := range env {
			if k == "" {
				return errors.New("toolpacks: local sandbox env key must not be empty")
			}
			s.env[k] = v
		}
		return nil
	}
}

// WithLocalSandboxInheritEnv opts into inheriting the whole parent environment
// (default: only the non-secret whitelist, see ShellTool's WithShellInheritEnv).
func WithLocalSandboxInheritEnv() LocalSandboxOption {
	return func(s *LocalSandbox) error { s.inherit = true; return nil }
}

// NewLocalSandbox builds a local-subprocess sandbox.
func NewLocalSandbox(opts ...LocalSandboxOption) (*LocalSandbox, error) {
	s := &LocalSandbox{
		timeout:   defaultExecTimeout,
		maxOutput: defaultMaxOutput,
		env:       map[string]string{},
	}
	for _, opt := range opts {
		if err := opt(s); err != nil {
			return nil, err
		}
	}
	if s.dir == "" {
		dir, err := os.MkdirTemp("", "ago_sandbox_")
		if err != nil {
			return nil, fmt.Errorf("toolpacks: local sandbox workdir: %w", err)
		}
		s.dir, s.autoDir = dir, true
	}
	// Pin the real path so file containment checks do not misfire on a
	// symlinked directory (macOS /tmp, for one).
	if resolved, err := filepath.EvalSymlinks(s.dir); err == nil {
		s.dir = resolved
	}
	return s, nil
}

// Workdir returns the working directory (slash-separated).
func (s *LocalSandbox) Workdir() string { return filepath.ToSlash(s.dir) }

// HostWorkdir returns the working directory as a host path.
func (s *LocalSandbox) HostWorkdir() string { return s.dir }

// Exec runs argv in the working directory.
func (s *LocalSandbox) Exec(ctx stdcontext.Context, argv []string, opts ExecOptions) (ExecResult, error) {
	env := make(map[string]string, len(s.env)+len(opts.Env))
	for k, v := range s.env {
		env[k] = v
	}
	for k, v := range opts.Env {
		env[k] = v
	}
	timeout := s.timeout
	if opts.Timeout > 0 {
		timeout = opts.Timeout
	}
	res, err := s.sb.call(ctx, argv, s.dir, env, s.inherit, timeout, s.maxOutput)
	return ExecResult(res), err
}

// within resolves a relative sandbox path onto the working directory and
// refuses anything that leaves it, symlinks included.
func (s *LocalSandbox) within(rel string) (string, error) {
	if !validSandboxPath(rel) {
		return "", fmt.Errorf("%w: %q", ErrSandboxPath, rel)
	}
	joined := filepath.Join(s.dir, filepath.FromSlash(rel))
	resolved, err := resolveExisting(joined)
	if err != nil {
		return "", err
	}
	if !withinRoot(s.dir, resolved) {
		return "", fmt.Errorf("%w: %q", ErrSandboxPath, rel)
	}
	return resolved, nil
}

// PutFile writes content under the working directory, creating parents.
func (s *LocalSandbox) PutFile(_ stdcontext.Context, path string, content []byte) error {
	if s.sb.isClosed() {
		return ErrSandboxClosed
	}
	target, err := s.within(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.WriteFile(target, content, 0o600)
}

// GetFile reads a file under the working directory.
func (s *LocalSandbox) GetFile(_ stdcontext.Context, path string) ([]byte, error) {
	if s.sb.isClosed() {
		return nil, ErrSandboxClosed
	}
	target, err := s.within(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(target)
}

// RemoveFile deletes a file under the working directory; a missing file is not
// an error.
func (s *LocalSandbox) RemoveFile(_ stdcontext.Context, path string) error {
	target, err := s.within(path)
	if err != nil {
		return err
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Close stops every in-flight command, waits for it, then removes an
// auto-created working directory. Idempotent.
func (s *LocalSandbox) Close() error {
	s.sb.shutdown()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.autoDir {
		return nil
	}
	s.autoDir = false
	return os.RemoveAll(s.dir)
}
