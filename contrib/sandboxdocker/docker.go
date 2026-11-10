// Package sandboxdocker is the Docker backend of the toolpacks execution seam
// (Python parity: ag2/extensions/docker DockerSandbox and DockerEnvironment).
// A Sandbox is one long-lived container that shell commands and code snippets
// exec into; an Environment is the toolpacks.SandboxFactory that hands one out
// per call, caching containers by their resolved parameters so state (created
// files, installed packages) persists across tool calls:
//
//	env, _ := sandboxdocker.NewEnvironment(sandboxdocker.WithImage("python:3.12-slim"))
//	defer env.Close()
//	shell, _ := toolpacks.NewShellTool(toolpacks.WithShellSandbox(env), toolpacks.WithShellAllowed("git"))
//	code, _ := toolpacks.NewSandboxCodeToolFromFactory(env, toolpacks.WithCodeLanguages("python"))
//
// The module is stdlib-only: the daemon is driven through the Docker Engine
// HTTP API over its unix socket (DOCKER_HOST, default
// unix:///var/run/docker.sock) or plain tcp://.
package sandboxdocker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/toolpacks"
	"github.com/luckystrker/ago/tools"
)

// Defaults (Python parity: the DockerEnvironment keyword defaults).
const (
	DefaultImage       = "python:3.12-slim"
	DefaultWorkdir     = "/workspace"
	DefaultNetworkMode = "none"
	DefaultMemLimit    = "512m"
	DefaultTimeout     = 60 * time.Second
	// DefaultMaxOutput caps the reported output in characters.
	//
	// AGO-DEVIATION: Python's DockerSandbox returns the exec output unbounded;
	// AGo caps it like the local sandbox does, so one chatty command cannot
	// grow memory or flood the model's context.
	DefaultMaxOutput = 100_000
	// maxFileBytes bounds one GetFile download.
	maxFileBytes = 64 << 20
	// stopGrace is the seconds a container gets to stop before it is killed
	// (Python parity: stop(timeout=1)).
	stopGrace = 1
)

// Config shapes one Sandbox. Every field is a concrete value; Variable
// resolution lives on Environment. Start from DefaultConfig.
type Config struct {
	// Host is the Docker daemon address; empty uses DOCKER_HOST, then DefaultHost.
	Host string
	// Image is the image the container runs (pulled when missing).
	Image string
	// EnvVars are the container's environment.
	EnvVars map[string]string
	// Timeout is the default per-exec timeout; at least one second.
	Timeout time.Duration
	// NetworkMode is the container network ("none", "bridge", "host", ...).
	NetworkMode string
	// MemLimit is the memory limit as docker-py writes it ("512m", "2g");
	// empty means no limit.
	MemLimit string
	// CPUQuota is the CFS quota in microseconds per 100ms period; 0 means none.
	CPUQuota int64
	// User runs commands as this user ("" = the image default).
	User string
	// AutoRemove lets the daemon remove the container when it stops; without it
	// Close and a timeout restart remove it explicitly.
	AutoRemove bool
	// HostPath bind-mounts this host directory at Workdir (read-write).
	HostPath string
	// Workdir is the working directory inside the container.
	Workdir string
	// MaxOutput caps the reported output in characters.
	MaxOutput int
}

// DefaultConfig returns the defaults of the Python DockerEnvironment.
func DefaultConfig() Config {
	return Config{
		Image:       DefaultImage,
		Timeout:     DefaultTimeout,
		NetworkMode: DefaultNetworkMode,
		MemLimit:    DefaultMemLimit,
		AutoRemove:  true,
		Workdir:     DefaultWorkdir,
		MaxOutput:   DefaultMaxOutput,
	}
}

// validate checks the config and fills the derived values.
func (c *Config) validate() error {
	if c.Timeout < time.Second {
		return errors.New("sandboxdocker: timeout must be >= 1 second")
	}
	if c.Image == "" {
		return errors.New("sandboxdocker: image must not be empty")
	}
	if !path.IsAbs(c.Workdir) {
		return fmt.Errorf("sandboxdocker: workdir %q must be an absolute path inside the container", c.Workdir)
	}
	c.Workdir = path.Clean(c.Workdir)
	if c.MaxOutput <= 0 {
		return errors.New("sandboxdocker: max output must be > 0")
	}
	if c.MemLimit != "" {
		if _, err := parseBytes(c.MemLimit); err != nil {
			return err
		}
	}
	if c.HostPath != "" {
		abs, err := filepath.Abs(c.HostPath)
		if err != nil {
			return fmt.Errorf("sandboxdocker: host path: %w", err)
		}
		c.HostPath = abs
	}
	return nil
}

var memPattern = regexp.MustCompile(`^(\d+)([kmgt]?)b?$`)

// parseBytes reads a docker-py style size ("512m", "2g", "1024", "64kb").
func parseBytes(s string) (int64, error) {
	m := memPattern.FindStringSubmatch(strings.ToLower(strings.TrimSpace(s)))
	if m == nil {
		return 0, fmt.Errorf("sandboxdocker: invalid memory limit %q", s)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("sandboxdocker: invalid memory limit %q: %w", s, err)
	}
	shift := map[string]uint{"": 0, "k": 10, "m": 20, "g": 30, "t": 40}[m[2]]
	return n << shift, nil
}

// Sandbox is one Docker container as a toolpacks.Sandbox (Python parity:
// DockerSandbox). The container is created on the first Exec/PutFile/GetFile
// (or Start) and stopped — and removed when AutoRemove is off — on Close. It is
// safe for concurrent use.
type Sandbox struct {
	cfg Config
	eng *engine

	mu     sync.Mutex
	id     string // running container, "" before creation and after a timeout restart
	closed bool
}

var (
	_ toolpacks.Sandbox     = (*Sandbox)(nil)
	_ toolpacks.FileRemover = (*Sandbox)(nil)
)

// NewSandbox builds a sandbox; no container exists until first use.
func NewSandbox(cfg Config) (*Sandbox, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	eng, err := newEngine(cfg.Host)
	if err != nil {
		return nil, err
	}
	cfg.EnvVars = cloneMap(cfg.EnvVars)
	return &Sandbox{cfg: cfg, eng: eng}, nil
}

// Workdir is the working directory inside the container.
func (s *Sandbox) Workdir() string { return s.cfg.Workdir }

// HostWorkdir is the bind-mounted host directory, "" when there is none.
func (s *Sandbox) HostWorkdir() string { return s.cfg.HostPath }

// errClosed is returned by calls on a closed sandbox.
func errClosed() error { return fmt.Errorf("%w: docker sandbox", toolpacks.ErrSandboxClosed) }

// Start creates the container when it does not exist yet (Python parity:
// __aenter__); Environment.Open calls it so a failure surfaces at open time.
func (s *Sandbox) Start(ctx context.Context) error {
	_, err := s.container(ctx)
	return err
}

// container returns the running container id, creating it on first use. The
// lock is held across the daemon calls so concurrent first callers create one
// container, not several.
func (s *Sandbox) container(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", errClosed()
	}
	if s.id != "" {
		return s.id, nil
	}
	req := createRequest{
		Image:      s.cfg.Image,
		Cmd:        []string{"sh", "-c", "mkdir -p " + shellQuote(s.cfg.Workdir) + " && sleep infinity"},
		Env:        envList(s.cfg.EnvVars),
		WorkingDir: s.cfg.Workdir,
		User:       s.cfg.User,
		HostConfig: createHostConfig{
			NetworkMode: s.cfg.NetworkMode,
			CPUQuota:    s.cfg.CPUQuota,
			AutoRemove:  s.cfg.AutoRemove,
		},
	}
	if s.cfg.MemLimit != "" {
		req.HostConfig.Memory, _ = parseBytes(s.cfg.MemLimit) // validated in NewSandbox
	}
	if s.cfg.HostPath != "" {
		req.HostConfig.Binds = []string{s.cfg.HostPath + ":" + s.cfg.Workdir + ":rw"}
	}
	id, err := s.eng.createContainer(ctx, req)
	if err != nil {
		return "", fmt.Errorf("sandboxdocker: start container: %w", err)
	}
	s.id = id
	return id, nil
}

// restart drops a container that outlived its command (after a timeout): it is
// stopped, removed when AutoRemove is off, and the next call creates a fresh
// one (Python parity: _restart_container).
func (s *Sandbox) restart() {
	s.mu.Lock()
	old := s.id
	s.id = ""
	s.mu.Unlock()
	if old != "" {
		_ = s.teardown(old)
	}
}

// teardown stops a container and removes it unless the daemon does.
func (s *Sandbox) teardown(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := s.eng.stopContainer(ctx, id, stopGrace)
	if !s.cfg.AutoRemove {
		err = errors.Join(err, s.eng.removeContainer(ctx, id))
	}
	return err
}

// Exec runs argv in the container's working directory. A timeout restarts the
// container (a command still running inside it would otherwise live on) and is
// reported as exit code 124; a daemon API error is a result with exit code 1
// (Python parity); a broken connection or a canceled ctx is an error.
func (s *Sandbox) Exec(ctx context.Context, argv []string, opts toolpacks.ExecOptions) (toolpacks.ExecResult, error) {
	if len(argv) == 0 {
		return toolpacks.ExecResult{ExitCode: 2}, nil
	}
	id, err := s.container(ctx)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			return toolpacks.ExecResult{Output: "Docker API error: " + apiErr.Error(), ExitCode: 1}, nil
		}
		return toolpacks.ExecResult{}, err
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = s.cfg.Timeout
	}
	ectx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, dropped, code, err := s.eng.exec(ectx, id, execRequest{
		AttachStdout: true, AttachStderr: true,
		Cmd:        argv,
		WorkingDir: s.cfg.Workdir,
		Env:        envList(opts.Env),
	}, 2*s.cfg.MaxOutput)
	if err != nil {
		if ctx.Err() != nil {
			return toolpacks.ExecResult{}, ctx.Err()
		}
		if errors.Is(ectx.Err(), context.DeadlineExceeded) {
			s.restart()
			return toolpacks.ExecResult{Output: fmt.Sprintf("Execution timed out after %gs", timeout.Seconds()), ExitCode: 124}, nil
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			return toolpacks.ExecResult{Output: "Docker API error: " + apiErr.Error(), ExitCode: 1}, nil
		}
		return toolpacks.ExecResult{}, err
	}
	return toolpacks.ExecResult{Output: s.render(out, dropped), ExitCode: code}, nil
}

// render turns the raw stream into the reported output: invalid UTF-8
// replaced, whitespace trimmed, then capped at MaxOutput characters.
func (s *Sandbox) render(out []byte, dropped int64) string {
	text := strings.TrimSpace(strings.ToValidUTF8(string(out), "�"))
	if dropped > 0 {
		text += fmt.Sprintf("\n[output truncated: dropped %d bytes beyond the %d-byte buffer cap]", dropped, 2*s.cfg.MaxOutput)
	}
	if total := utf8.RuneCountInString(text); total > s.cfg.MaxOutput {
		r := []rune(text)
		text = string(r[:s.cfg.MaxOutput]) + fmt.Sprintf("\n[truncated: showing first %d of %d chars]", s.cfg.MaxOutput, total)
	}
	return text
}

// target resolves a sandbox-relative path under the working directory; an
// absolute path or one leaving the working directory is refused.
//
// AGO-DEVIATION: Python refuses only absolute paths; a ".." that climbs out of
// the working directory is refused here too, like every other AGo sandbox.
func (s *Sandbox) target(rel string) (string, error) {
	if rel == "" || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) {
		return "", fmt.Errorf("%w: %q", toolpacks.ErrSandboxPath, rel)
	}
	clean := path.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, "../") || clean == "." {
		return "", fmt.Errorf("%w: %q", toolpacks.ErrSandboxPath, rel)
	}
	return clean, nil
}

// PutFile uploads content to path under the working directory through the
// archive API, creating missing parent directories.
func (s *Sandbox) PutFile(ctx context.Context, p string, content []byte) error {
	rel, err := s.target(p)
	if err != nil {
		return err
	}
	id, err := s.container(ctx)
	if err != nil {
		return err
	}
	return s.eng.putFile(ctx, id, s.cfg.Workdir, rel, content)
}

// GetFile downloads the file at path under the working directory. A missing
// file matches os.ErrNotExist.
func (s *Sandbox) GetFile(ctx context.Context, p string) ([]byte, error) {
	rel, err := s.target(p)
	if err != nil {
		return nil, err
	}
	id, err := s.container(ctx)
	if err != nil {
		return nil, err
	}
	b, err := s.eng.getFile(ctx, id, path.Join(s.cfg.Workdir, rel), maxFileBytes)
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("%w: %w", os.ErrNotExist, err)
	}
	return b, err
}

// RemoveFile deletes a file by `rm -f` in the container (the Engine API has no
// delete call); a missing file is not an error.
func (s *Sandbox) RemoveFile(ctx context.Context, p string) error {
	rel, err := s.target(p)
	if err != nil {
		return err
	}
	res, err := s.Exec(ctx, []string{"rm", "-f", path.Join(s.cfg.Workdir, rel)}, toolpacks.ExecOptions{})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("sandboxdocker: remove %s: %s", p, res.Output)
	}
	return nil
}

// Close stops the container (removing it when AutoRemove is off) and releases
// the daemon connection. Idempotent; later calls fail with ErrSandboxClosed.
func (s *Sandbox) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	id := s.id
	s.id = ""
	s.mu.Unlock()
	var err error
	if id != "" {
		err = s.teardown(id)
	}
	s.eng.close()
	return err
}

// --- environment -------------------------------------------------------------

// Environment is the toolpacks.SandboxFactory over Docker (Python parity:
// DockerEnvironment). It owns the containers: Open starts one per distinct set
// of resolved parameters and reuses it afterwards; Close tears them all down.
// Safe for concurrent use.
type Environment struct {
	cfg   Config
	bound map[string]tools.Variable // "image", "env_vars", "network_mode"

	mu    sync.Mutex
	cache map[string]*Sandbox
}

var (
	_ toolpacks.SandboxFactory = (*Environment)(nil)
	_ toolpacks.WorkdirAware   = (*Environment)(nil)
	_ io.Closer                = (*Environment)(nil)
)

// Option configures NewEnvironment.
type Option func(*Environment)

// WithHost sets the Docker daemon address (unix:///path or tcp://host:port).
func WithHost(host string) Option { return func(e *Environment) { e.cfg.Host = host } }

// WithImage sets the container image (default python:3.12-slim).
func WithImage(image string) Option { return func(e *Environment) { e.cfg.Image = image } }

// WithEnvVars sets the container environment.
func WithEnvVars(env map[string]string) Option {
	return func(e *Environment) { e.cfg.EnvVars = cloneMap(env) }
}

// WithTimeout sets the default per-exec timeout (at least one second).
func WithTimeout(d time.Duration) Option { return func(e *Environment) { e.cfg.Timeout = d } }

// WithNetworkMode sets the container network (default "none": no network).
func WithNetworkMode(mode string) Option { return func(e *Environment) { e.cfg.NetworkMode = mode } }

// WithMemLimit sets the memory limit ("512m" default; "" for none).
func WithMemLimit(limit string) Option { return func(e *Environment) { e.cfg.MemLimit = limit } }

// WithCPUQuota sets the CFS quota in microseconds per 100ms period.
func WithCPUQuota(quota int64) Option { return func(e *Environment) { e.cfg.CPUQuota = quota } }

// WithUser runs commands as the given user.
func WithUser(user string) Option { return func(e *Environment) { e.cfg.User = user } }

// WithAutoRemove sets whether the daemon removes a stopped container itself
// (default true); when false Close removes it explicitly.
func WithAutoRemove(v bool) Option { return func(e *Environment) { e.cfg.AutoRemove = v } }

// WithHostPath bind-mounts a host directory at the working directory.
func WithHostPath(dir string) Option { return func(e *Environment) { e.cfg.HostPath = dir } }

// WithWorkdir sets the working directory inside the container.
func WithWorkdir(dir string) Option { return func(e *Environment) { e.cfg.Workdir = dir } }

// WithMaxOutput caps the reported output in characters.
func WithMaxOutput(n int) Option { return func(e *Environment) { e.cfg.MaxOutput = n } }

// WithImageVariable resolves the image from a conversation Variable on every
// Open (Python parity: image=Variable(...)); a distinct value gets its own
// container.
func WithImageVariable(v tools.Variable) Option { return func(e *Environment) { e.bind("image", v) } }

// WithEnvVarsVariable resolves the environment (a map of strings) from a
// conversation Variable on every Open.
func WithEnvVarsVariable(v tools.Variable) Option {
	return func(e *Environment) { e.bind("env_vars", v) }
}

// WithNetworkModeVariable resolves the network mode from a conversation
// Variable on every Open.
func WithNetworkModeVariable(v tools.Variable) Option {
	return func(e *Environment) { e.bind("network_mode", v) }
}

func (e *Environment) bind(param string, v tools.Variable) {
	if e.bound == nil {
		e.bound = map[string]tools.Variable{}
	}
	e.bound[param] = v
}

// NewEnvironment builds the factory; no container exists until the first Open.
func NewEnvironment(opts ...Option) (*Environment, error) {
	e := &Environment{cfg: DefaultConfig(), cache: map[string]*Sandbox{}}
	for _, opt := range opts {
		opt(e)
	}
	probe := e.cfg
	if err := probe.validate(); err != nil {
		return nil, err
	}
	if _, err := newEngine(probe.Host); err != nil {
		return nil, err
	}
	e.cfg = probe
	return e, nil
}

// Workdir is the working directory inside the containers (toolpacks.WorkdirAware).
func (e *Environment) Workdir() string { return e.cfg.Workdir }

// resolve applies the Variable bindings over the configured values.
func (e *Environment) resolve(cx *agocontext.Context) (Config, error) {
	cfg := e.cfg
	cfg.EnvVars = cloneMap(cfg.EnvVars)
	for _, param := range []string{"image", "env_vars", "network_mode"} {
		v, ok := e.bound[param]
		if !ok {
			continue
		}
		if cx == nil {
			return cfg, fmt.Errorf("sandboxdocker: %s given as Variable but no Context is available to resolve it "+
				"(variables resolve only when the sandbox is driven from an agent)", param)
		}
		val, err := lookupVariable(cx, param, v)
		if err != nil {
			return cfg, err
		}
		switch param {
		case "image", "network_mode":
			str, ok := val.(string)
			if !ok || str == "" {
				return cfg, fmt.Errorf("sandboxdocker: variable for %s must be a non-empty string, got %v", param, val)
			}
			if param == "image" {
				cfg.Image = str
			} else {
				cfg.NetworkMode = str
			}
		case "env_vars":
			b, err := json.Marshal(val)
			if err != nil {
				return cfg, fmt.Errorf("sandboxdocker: variable for env_vars: %w", err)
			}
			var env map[string]string
			if err := json.Unmarshal(b, &env); err != nil {
				return cfg, fmt.Errorf("sandboxdocker: variable for env_vars must be a map of strings: %w", err)
			}
			cfg.EnvVars = env
		}
	}
	return cfg, nil
}

// lookupVariable resolves a Variable the way Python's resolve_variable does:
// the variable's name (or the parameter name), then its default, else an error.
func lookupVariable(cx *agocontext.Context, param string, v tools.Variable) (any, error) {
	key := v.Name
	if key == "" {
		key = param
	}
	if val, ok := cx.Variable(key); ok {
		return val, nil
	}
	if v.HasDefault {
		return v.Default, nil
	}
	return nil, fmt.Errorf("%w: variable %q for parameter %q is not provided and has no default", tools.ErrNotFound, key, param)
}

// Open returns the sandbox for the resolved parameters, starting its container
// on first use; later opens with the same values reuse it. The release func is
// a no-op: the environment owns the lifecycle (Python parity: the sandbox is
// not closed on scope exit). cx may be nil when no parameter is a Variable.
func (e *Environment) Open(ctx context.Context, cx *agocontext.Context) (toolpacks.Sandbox, func() error, error) {
	cfg, err := e.resolve(cx)
	if err != nil {
		return nil, nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, nil, err
	}
	key, err := cacheKey(cfg)
	if err != nil {
		return nil, nil, err
	}
	e.mu.Lock()
	sb := e.cache[key]
	if sb == nil || sb.isClosed() {
		sb, err = NewSandbox(cfg)
		if err != nil {
			e.mu.Unlock()
			return nil, nil, err
		}
		e.cache[key] = sb
	}
	e.mu.Unlock()
	if err := sb.Start(ctx); err != nil {
		return nil, nil, err
	}
	return sb, func() error { return nil }, nil
}

func (s *Sandbox) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Close tears down every cached container. Idempotent; the environment can be
// opened again afterwards, which starts fresh containers.
func (e *Environment) Close() error {
	e.mu.Lock()
	sandboxes := make([]*Sandbox, 0, len(e.cache))
	for _, sb := range e.cache {
		sandboxes = append(sandboxes, sb)
	}
	e.cache = map[string]*Sandbox{}
	e.mu.Unlock()
	var errs []error
	for _, sb := range sandboxes {
		errs = append(errs, sb.Close())
	}
	return errors.Join(errs...)
}

// cacheKey identifies a container by everything that shapes it.
func cacheKey(cfg Config) (string, error) {
	b, err := json.Marshal(cfg) // map keys are sorted: a stable key
	return string(b), err
}

// --- helpers -----------------------------------------------------------------

func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// envList renders an environment map as sorted KEY=VALUE entries.
func envList(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// shellQuote single-quotes s for sh.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
