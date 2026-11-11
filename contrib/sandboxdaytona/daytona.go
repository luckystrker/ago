// Package sandboxdaytona is the Daytona backend of the toolpacks execution seam
// (Python parity: ag2/extensions/daytona DaytonaSandbox and DaytonaEnvironment).
// A Sandbox is one Daytona cloud sandbox that shell commands and code snippets
// run in; an Environment is the toolpacks.SandboxFactory that hands one out per
// call, caching sandboxes by their resolved parameters so state persists across
// tool calls:
//
//	env, _ := sandboxdaytona.NewEnvironment(sandboxdaytona.WithImage("python:3.12"))
//	defer env.Close()
//	shell, _ := toolpacks.NewShellTool(toolpacks.WithShellSandbox(env))
//
// The module is stdlib-only: it speaks Daytona's REST API directly — the control
// plane (create, poll, delete a sandbox) at the API URL and the toolbox (execute
// a command, upload, download and delete files) at the sandbox's
// toolboxProxyUrl. The API key and URL come from the options or from
// DAYTONA_API_KEY / DAYTONA_API_URL / DAYTONA_TARGET, as in the Python SDK.
package sandboxdaytona

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/toolpacks"
	"github.com/luckystrker/ago/tools"
)

// Defaults.
const (
	// DefaultAPIURL is the Daytona Cloud API (the SDK default).
	DefaultAPIURL  = "https://app.daytona.io/api"
	DefaultWorkdir = "/workspace"
	// DefaultTimeout is the default per-exec timeout (Python: timeout=60).
	DefaultTimeout = 60 * time.Second
	// DefaultCreateTimeout bounds creating a sandbox and waiting for it to
	// start (the SDK's create timeout).
	DefaultCreateTimeout = 60 * time.Second
	// DefaultMaxOutput caps the reported output in characters.
	//
	// AGO-DEVIATION: Python's DaytonaSandbox returns the output unbounded; AGo
	// caps it like the local sandbox does.
	DefaultMaxOutput = 100_000
	maxFileBytes     = 64 << 20
)

// execGrace is added to the exec timeout for the HTTP round trip, so the
// server's own timeout answer wins over the client's cut-off.
var execGrace = 10 * time.Second

// ErrNotFound matches (errors.Is) a 404 answer and a missing file.
var ErrNotFound = errors.New("sandboxdaytona: not found")

// APIError is an error response of the Daytona API.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("daytona API error %d: %s", e.Status, e.Message)
}

// Is lets errors.Is(err, ErrNotFound) match a 404.
func (e *APIError) Is(target error) bool {
	return target == ErrNotFound && e.Status == http.StatusNotFound
}

// Resources are the sandbox resource limits (Python parity: DaytonaResources),
// applied only when an Image is set; a snapshot carries its own profile. Zero
// means unset.
type Resources struct {
	CPU    int
	Memory int
	Disk   int
}

// Config shapes one Sandbox. Every field is a concrete value; Variable
// resolution lives on Environment. Start from DefaultConfig.
type Config struct {
	// APIKey authenticates both planes; empty reads DAYTONA_API_KEY.
	APIKey string
	// APIURL is the control-plane URL; empty reads DAYTONA_API_URL, then DefaultAPIURL.
	APIURL string
	// Target is the runner region; empty reads DAYTONA_TARGET, then the account default.
	Target string
	// Snapshot creates the sandbox from a snapshot; exclusive with Image.
	Snapshot string
	// Image creates the sandbox from a Docker image (built from `FROM <image>`).
	Image string
	// EnvVars are the sandbox environment.
	EnvVars map[string]string
	// Resources applies with Image only.
	Resources Resources
	// Timeout is the default per-exec timeout; at least one second.
	Timeout time.Duration
	// CreateTimeout bounds creation and the wait for the started state.
	CreateTimeout time.Duration
	// Workdir is the working directory commands run in and relative paths
	// resolve against.
	Workdir string
	// MaxOutput caps the reported output in characters.
	MaxOutput int
	// PollInterval is the delay between state checks while a sandbox starts.
	PollInterval time.Duration
	// HTTPClient overrides the HTTP client (tests, proxies, TLS settings).
	HTTPClient *http.Client `json:"-"`
}

// DefaultConfig returns the defaults of the Python DaytonaEnvironment.
func DefaultConfig() Config {
	return Config{
		Timeout:       DefaultTimeout,
		CreateTimeout: DefaultCreateTimeout,
		Workdir:       DefaultWorkdir,
		MaxOutput:     DefaultMaxOutput,
		PollInterval:  500 * time.Millisecond,
	}
}

// resolveEnv fills APIKey, APIURL and Target from the environment and checks the
// config.
func (c *Config) validate() error {
	if c.APIKey == "" {
		c.APIKey = os.Getenv("DAYTONA_API_KEY")
	}
	if c.APIURL == "" {
		c.APIURL = os.Getenv("DAYTONA_API_URL")
	}
	if c.APIURL == "" {
		c.APIURL = DefaultAPIURL
	}
	c.APIURL = strings.TrimRight(c.APIURL, "/")
	if c.Target == "" {
		c.Target = os.Getenv("DAYTONA_TARGET")
	}
	switch {
	case c.APIKey == "":
		return errors.New("sandboxdaytona: no API key (set one with WithAPIKey or DAYTONA_API_KEY)")
	case c.Snapshot != "" && c.Image != "":
		return errors.New("sandboxdaytona: specify either a snapshot or an image, not both")
	case c.Timeout < time.Second:
		return errors.New("sandboxdaytona: timeout must be >= 1 second")
	case c.CreateTimeout <= 0:
		return errors.New("sandboxdaytona: create timeout must be > 0")
	case !path.IsAbs(c.Workdir):
		return fmt.Errorf("sandboxdaytona: workdir %q must be an absolute path inside the sandbox", c.Workdir)
	case c.MaxOutput <= 0:
		return errors.New("sandboxdaytona: max output must be > 0")
	case c.PollInterval <= 0:
		return errors.New("sandboxdaytona: poll interval must be > 0")
	}
	c.Workdir = path.Clean(c.Workdir)
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{}
	}
	return nil
}

// createBody renders the POST /sandbox request (Python parity: the three
// params shapes of DaytonaEnvironment.open; auto-stop is disabled).
func (c Config) createBody() map[string]any {
	body := map[string]any{"autoStopInterval": 0}
	if len(c.EnvVars) > 0 {
		body["env"] = c.EnvVars
	}
	if c.Target != "" {
		body["target"] = c.Target
	}
	switch {
	case c.Snapshot != "":
		body["snapshot"] = c.Snapshot
	case c.Image != "":
		body["buildInfo"] = map[string]any{"dockerfileContent": "FROM " + c.Image + "\n"}
		if c.Resources.CPU > 0 {
			body["cpu"] = c.Resources.CPU
		}
		if c.Resources.Memory > 0 {
			body["memory"] = c.Resources.Memory
		}
		if c.Resources.Disk > 0 {
			body["disk"] = c.Resources.Disk
		}
	}
	return body
}

// client is a minimal Daytona REST client.
type client struct {
	cfg Config
}

// do sends a request with the bearer token; non-2xx answers become *APIError.
func (c *client) do(ctx context.Context, method, rawURL string, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	msg := strings.TrimSpace(string(raw))
	var parsed struct {
		Message any `json:"message"`
	}
	if json.Unmarshal(raw, &parsed) == nil && parsed.Message != nil {
		msg = fmt.Sprint(parsed.Message)
	}
	return nil, &APIError{Status: resp.StatusCode, Message: msg}
}

// jsonCall sends a JSON request and decodes a JSON response into out.
func (c *client) jsonCall(ctx context.Context, method, rawURL string, in, out any) error {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	resp, err := c.do(ctx, method, rawURL, ct, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// sandboxInfo is the part of a Sandbox response the backend reads.
type sandboxInfo struct {
	ID              string `json:"id"`
	State           string `json:"state"`
	ErrorReason     string `json:"errorReason"`
	ToolboxProxyURL string `json:"toolboxProxyUrl"`
}

// create creates a sandbox, waits until it is started and resolves its toolbox
// proxy URL. Whenever the create timeout or the caller's context ends the wait —
// whether the clock ran out between polls or inside a request — the error is the
// same typed "did not start" error wrapping the context error, so callers and
// tests see one outcome regardless of timing.
func (c *client) create(ctx context.Context) (sandboxInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.CreateTimeout)
	defer cancel()
	notStarted := func(info sandboxInfo) error {
		return fmt.Errorf("sandboxdaytona: sandbox %q did not start (state %q): %w", info.ID, info.State, ctx.Err())
	}
	var info sandboxInfo
	if err := c.jsonCall(ctx, http.MethodPost, c.cfg.APIURL+"/sandbox", c.cfg.createBody(), &info); err != nil {
		if ctx.Err() != nil {
			return info, notStarted(info)
		}
		return info, err
	}
	for {
		switch info.State {
		case "started":
			if err := c.ensureProxyURL(ctx, &info); err != nil {
				if ctx.Err() != nil {
					return info, notStarted(info)
				}
				return info, err
			}
			return info, nil
		case "error", "build_failed", "destroyed", "destroying":
			return info, fmt.Errorf("sandboxdaytona: sandbox %s entered state %q: %s", info.ID, info.State, info.ErrorReason)
		}
		select {
		case <-ctx.Done():
			return info, notStarted(info)
		case <-time.After(c.cfg.PollInterval):
		}
		proxy := info.ToolboxProxyURL
		if err := c.jsonCall(ctx, http.MethodGet, c.cfg.APIURL+"/sandbox/"+url.PathEscape(info.ID), nil, &info); err != nil {
			if ctx.Err() != nil {
				return info, notStarted(info)
			}
			return info, err
		}
		if info.ToolboxProxyURL == "" {
			info.ToolboxProxyURL = proxy
		}
	}
}

// ensureProxyURL fills the toolbox proxy URL when the sandbox response carried
// none, the way the SDK does (Daytona._ensure_toolbox_proxy_url): one GET of
// /sandbox/{id}/toolbox-proxy-url. There is no guessed fallback.
func (c *client) ensureProxyURL(ctx context.Context, info *sandboxInfo) error {
	if info.ToolboxProxyURL != "" {
		return nil
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := c.jsonCall(ctx, http.MethodGet, c.cfg.APIURL+"/sandbox/"+url.PathEscape(info.ID)+"/toolbox-proxy-url", nil, &out); err != nil {
		return fmt.Errorf("sandboxdaytona: toolbox proxy URL of sandbox %s: %w", info.ID, err)
	}
	if out.URL == "" {
		return fmt.Errorf("sandboxdaytona: toolbox proxy URL of sandbox %s is empty", info.ID)
	}
	info.ToolboxProxyURL = out.URL
	return nil
}

// delete deletes a sandbox; an already deleted one is not an error.
func (c *client) delete(ctx context.Context, id string) error {
	err := c.jsonCall(ctx, http.MethodDelete, c.cfg.APIURL+"/sandbox/"+url.PathEscape(id), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// Sandbox is one Daytona sandbox as a toolpacks.Sandbox (Python parity:
// DaytonaSandbox). It is created on first use (or Start) and deleted on Close.
// Safe for concurrent use.
type Sandbox struct {
	cfg Config
	cl  *client

	mu     sync.Mutex
	info   sandboxInfo // zero ID before creation
	closed bool
}

var (
	_ toolpacks.Sandbox     = (*Sandbox)(nil)
	_ toolpacks.FileRemover = (*Sandbox)(nil)
)

// NewSandbox builds a sandbox; nothing exists in the cloud until first use.
func NewSandbox(cfg Config) (*Sandbox, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg.EnvVars = cloneMap(cfg.EnvVars)
	return &Sandbox{cfg: cfg, cl: &client{cfg: cfg}}, nil
}

// Workdir is the working directory inside the sandbox.
func (s *Sandbox) Workdir() string { return s.cfg.Workdir }

// HostWorkdir is empty: a cloud sandbox has no host-side view.
func (s *Sandbox) HostWorkdir() string { return "" }

func errClosed() error { return fmt.Errorf("%w: daytona sandbox", toolpacks.ErrSandboxClosed) }

// Start creates the sandbox when it does not exist yet (Python parity:
// __aenter__).
func (s *Sandbox) Start(ctx context.Context) error {
	_, err := s.sandbox(ctx)
	return err
}

// sandbox returns the started sandbox, creating it on first use. The lock is
// held across creation so concurrent first callers create one sandbox.
func (s *Sandbox) sandbox(ctx context.Context) (sandboxInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return sandboxInfo{}, errClosed()
	}
	if s.info.ID != "" {
		return s.info, nil
	}
	info, err := s.cl.create(ctx)
	if err != nil {
		// A half-created sandbox must not leak: delete what the API gave us.
		if info.ID != "" {
			dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_ = s.cl.delete(dctx, info.ID)
			cancel()
		}
		return sandboxInfo{}, fmt.Errorf("sandboxdaytona: create sandbox: %w", err)
	}
	// The workdir must exist before commands run in it or files land in it.
	//
	// AGO-DEVIATION: Python never creates the workdir (its exec passes no cwd, so
	// the SDK's default directory is used and files are uploaded by absolute
	// path). AGo runs commands with cwd = Workdir, so it creates the directory
	// once, through the toolbox files/folder call, right after the sandbox starts.
	if err := s.mkdir(ctx, info, s.cfg.Workdir); err != nil {
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = s.cl.delete(dctx, info.ID)
		cancel()
		return sandboxInfo{}, fmt.Errorf("sandboxdaytona: prepare workdir: %w", err)
	}
	s.info = info
	return info, nil
}

// toolbox is the URL of one toolbox endpoint of a sandbox.
func (s *Sandbox) toolbox(info sandboxInfo, endpoint string, query url.Values) string {
	u := strings.TrimRight(info.ToolboxProxyURL, "/") + "/" + url.PathEscape(info.ID) + "/" + endpoint
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

// mkdir creates a directory (and parents) in the sandbox.
func (s *Sandbox) mkdir(ctx context.Context, info sandboxInfo, dir string) error {
	resp, err := s.cl.do(ctx, http.MethodPost, s.toolbox(info, "files/folder", url.Values{"path": {dir}, "mode": {"755"}}), "", nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Exec runs argv in the working directory. Python joins the argv into a shell
// command line (shlex.join) because Daytona's execute takes a string; so does
// this. A timeout is reported as exit code 124, a rate limit or API error as a
// result with exit code 1; a canceled ctx or a broken connection is an error.
//
// AGO-DEVIATION: Python passes no working directory and drops the per-call
// environment; AGo sends the working directory (created at startup) as cwd and
// the environment as the execute request's envs, so a relative path resolves
// where the tools say the workdir is.
func (s *Sandbox) Exec(ctx context.Context, argv []string, opts toolpacks.ExecOptions) (toolpacks.ExecResult, error) {
	if len(argv) == 0 {
		return toolpacks.ExecResult{ExitCode: 2}, nil
	}
	info, err := s.sandbox(ctx)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			return apiResult(apiErr), nil
		}
		return toolpacks.ExecResult{}, err
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = s.cfg.Timeout
	}
	secs := int(timeout.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}
	body := map[string]any{"command": shellJoin(argv), "cwd": s.cfg.Workdir, "timeout": secs}
	if len(opts.Env) > 0 {
		body["envs"] = opts.Env
	}
	rctx, cancel := context.WithTimeout(ctx, time.Duration(secs)*time.Second+execGrace)
	defer cancel()
	var out struct {
		Result   string `json:"result"`
		ExitCode int    `json:"exitCode"`
	}
	if err := s.cl.jsonCall(rctx, http.MethodPost, s.toolbox(info, "process/execute", nil), body, &out); err != nil {
		if ctx.Err() != nil {
			return toolpacks.ExecResult{}, ctx.Err()
		}
		if errors.Is(rctx.Err(), context.DeadlineExceeded) {
			return toolpacks.ExecResult{Output: fmt.Sprintf("Execution timed out after %ds", secs), ExitCode: 124}, nil
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			if apiErr.Status == http.StatusRequestTimeout || apiErr.Status == http.StatusGatewayTimeout {
				return toolpacks.ExecResult{Output: "Execution timed out: " + apiErr.Message, ExitCode: 124}, nil
			}
			return apiResult(apiErr), nil
		}
		return toolpacks.ExecResult{}, err
	}
	return toolpacks.ExecResult{Output: s.cap(out.Result), ExitCode: out.ExitCode}, nil
}

// apiResult maps an API failure to the result Python reports for it.
func apiResult(e *APIError) toolpacks.ExecResult {
	if e.Status == http.StatusTooManyRequests {
		return toolpacks.ExecResult{Output: "Daytona rate limit exceeded: " + e.Message, ExitCode: 1}
	}
	return toolpacks.ExecResult{Output: "Daytona error: " + e.Error(), ExitCode: 1}
}

// cap truncates the output to MaxOutput characters with a notice.
func (s *Sandbox) cap(out string) string {
	out = strings.ToValidUTF8(out, "�")
	if total := utf8.RuneCountInString(out); total > s.cfg.MaxOutput {
		r := []rune(out)
		return string(r[:s.cfg.MaxOutput]) + fmt.Sprintf("\n[truncated: showing first %d of %d chars]", s.cfg.MaxOutput, total)
	}
	return out
}

// target resolves a sandbox-relative path under the working directory.
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
	return path.Join(s.cfg.Workdir, clean), nil
}

// PutFile uploads content to path under the working directory, creating missing
// parent directories first.
func (s *Sandbox) PutFile(ctx context.Context, p string, content []byte) error {
	target, err := s.target(p)
	if err != nil {
		return err
	}
	info, err := s.sandbox(ctx)
	if err != nil {
		return err
	}
	if dir := path.Dir(target); dir != s.cfg.Workdir {
		if err := s.mkdir(ctx, info, dir); err != nil {
			return err
		}
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", path.Base(target))
	if err != nil {
		return err
	}
	if _, err := part.Write(content); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}
	resp, err := s.cl.do(ctx, http.MethodPost, s.toolbox(info, "files/upload-v2", url.Values{"path": {target}}), mw.FormDataContentType(), &buf)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// GetFile downloads the file at path under the working directory. A missing
// file matches os.ErrNotExist.
func (s *Sandbox) GetFile(ctx context.Context, p string) ([]byte, error) {
	target, err := s.target(p)
	if err != nil {
		return nil, err
	}
	info, err := s.sandbox(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := s.cl.do(ctx, http.MethodGet, s.toolbox(info, "files/download", url.Values{"path": {target}}), "", nil)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w: %w", os.ErrNotExist, err)
		}
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxFileBytes {
		return nil, fmt.Errorf("sandboxdaytona: %s exceeds the %d-byte download limit", p, maxFileBytes)
	}
	return b, nil
}

// RemoveFile deletes a file through the toolbox API; a missing file is not an
// error (Python parity: DaytonaNotFoundError is suppressed).
func (s *Sandbox) RemoveFile(ctx context.Context, p string) error {
	target, err := s.target(p)
	if err != nil {
		return err
	}
	info, err := s.sandbox(ctx)
	if err != nil {
		return err
	}
	resp, err := s.cl.do(ctx, http.MethodDelete, s.toolbox(info, "files", url.Values{"path": {target}}), "", nil)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	resp.Body.Close()
	return nil
}

// Close deletes the sandbox. Idempotent; later calls fail with ErrSandboxClosed.
func (s *Sandbox) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	id := s.info.ID
	s.info = sandboxInfo{}
	s.mu.Unlock()
	if id == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.cl.delete(ctx, id)
}

func (s *Sandbox) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// --- environment -------------------------------------------------------------

// variableParams are the parameters that accept a Variable (Python parity:
// api_key, api_url, target, snapshot, image, env_vars).
var variableParams = map[string]bool{
	"api_key": true, "api_url": true, "target": true, "snapshot": true, "image": true, "env_vars": true,
}

// Environment is the toolpacks.SandboxFactory over Daytona (Python parity:
// DaytonaEnvironment). Open creates one sandbox per distinct set of resolved
// parameters and reuses it afterwards; Close deletes them all. Safe for
// concurrent use.
type Environment struct {
	cfg   Config
	bound map[string]tools.Variable
	err   error // an invalid option, reported by NewEnvironment

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

// WithAPIKey sets the API key (default: DAYTONA_API_KEY).
func WithAPIKey(key string) Option { return func(e *Environment) { e.cfg.APIKey = key } }

// WithAPIURL sets the control-plane URL (default: DAYTONA_API_URL, then Daytona Cloud).
func WithAPIURL(u string) Option { return func(e *Environment) { e.cfg.APIURL = u } }

// WithTarget sets the runner region (default: DAYTONA_TARGET).
func WithTarget(target string) Option { return func(e *Environment) { e.cfg.Target = target } }

// WithSnapshot creates sandboxes from a snapshot (exclusive with WithImage).
func WithSnapshot(name string) Option { return func(e *Environment) { e.cfg.Snapshot = name } }

// WithImage creates sandboxes from a Docker image (exclusive with WithSnapshot).
func WithImage(image string) Option { return func(e *Environment) { e.cfg.Image = image } }

// WithEnvVars sets the sandbox environment.
func WithEnvVars(env map[string]string) Option {
	return func(e *Environment) { e.cfg.EnvVars = cloneMap(env) }
}

// WithResources sets the resource limits (applied with an image only).
func WithResources(r Resources) Option { return func(e *Environment) { e.cfg.Resources = r } }

// WithTimeout sets the default per-exec timeout (at least one second).
func WithTimeout(d time.Duration) Option { return func(e *Environment) { e.cfg.Timeout = d } }

// WithCreateTimeout bounds sandbox creation and the wait for it to start.
func WithCreateTimeout(d time.Duration) Option {
	return func(e *Environment) { e.cfg.CreateTimeout = d }
}

// WithWorkdir sets the working directory inside the sandbox.
func WithWorkdir(dir string) Option { return func(e *Environment) { e.cfg.Workdir = dir } }

// WithMaxOutput caps the reported output in characters.
func WithMaxOutput(n int) Option { return func(e *Environment) { e.cfg.MaxOutput = n } }

// WithPollInterval sets the delay between state checks while a sandbox starts.
func WithPollInterval(d time.Duration) Option { return func(e *Environment) { e.cfg.PollInterval = d } }

// WithHTTPClient overrides the HTTP client.
func WithHTTPClient(c *http.Client) Option { return func(e *Environment) { e.cfg.HTTPClient = c } }

// WithVariable resolves one parameter from a conversation Variable on every
// Open (Python parity: a Variable kwarg): "api_key", "api_url", "target",
// "snapshot", "image" or "env_vars". The variable's name defaults to the
// parameter name; its default applies when the conversation has none; a distinct
// resolved value gets its own sandbox.
func WithVariable(param string, v tools.Variable) Option {
	return func(e *Environment) {
		if !variableParams[param] {
			e.err = errors.Join(e.err, fmt.Errorf("sandboxdaytona: no variable-capable parameter %q", param))
			return
		}
		if e.bound == nil {
			e.bound = map[string]tools.Variable{}
		}
		e.bound[param] = v
	}
}

// NewEnvironment builds the factory; nothing exists in the cloud until the
// first Open. With a Variable bound to a credential or to the image/snapshot
// the matching static value may be left empty.
func NewEnvironment(opts ...Option) (*Environment, error) {
	e := &Environment{cfg: DefaultConfig(), cache: map[string]*Sandbox{}}
	for _, opt := range opts {
		opt(e)
	}
	if e.err != nil {
		return nil, e.err
	}
	// Validate what is knowable now; bound parameters are checked at Open.
	probe := e.cfg
	if _, ok := e.bound["api_key"]; ok && probe.APIKey == "" {
		probe.APIKey = "bound"
	}
	_, snapVar := e.bound["snapshot"]
	_, imgVar := e.bound["image"]
	if snapVar || imgVar {
		probe.Snapshot, probe.Image = "", ""
	}
	if err := probe.validate(); err != nil {
		return nil, err
	}
	if e.cfg.HTTPClient == nil {
		e.cfg.HTTPClient = &http.Client{}
	}
	return e, nil
}

// Workdir is the working directory inside the sandboxes (toolpacks.WorkdirAware).
func (e *Environment) Workdir() string { return e.cfg.Workdir }

func (e *Environment) resolve(cx *agocontext.Context) (Config, error) {
	cfg := e.cfg
	cfg.EnvVars = cloneMap(cfg.EnvVars)
	for _, param := range []string{"api_key", "api_url", "target", "snapshot", "image", "env_vars"} {
		v, ok := e.bound[param]
		if !ok {
			continue
		}
		if cx == nil {
			return cfg, fmt.Errorf("sandboxdaytona: %s given as Variable but no Context is available to resolve it "+
				"(variables resolve only when the sandbox is driven from an agent)", param)
		}
		val, err := lookupVariable(cx, param, v)
		if err != nil {
			return cfg, err
		}
		if param == "env_vars" {
			b, err := json.Marshal(val)
			if err != nil {
				return cfg, fmt.Errorf("sandboxdaytona: variable for env_vars: %w", err)
			}
			var env map[string]string
			if err := json.Unmarshal(b, &env); err != nil {
				return cfg, fmt.Errorf("sandboxdaytona: variable for env_vars must be a map of strings: %w", err)
			}
			cfg.EnvVars = env
			continue
		}
		str, ok := val.(string)
		if !ok {
			return cfg, fmt.Errorf("sandboxdaytona: variable for %s must be a string, got %v", param, val)
		}
		switch param {
		case "api_key":
			cfg.APIKey = str
		case "api_url":
			cfg.APIURL = str
		case "target":
			cfg.Target = str
		case "snapshot":
			cfg.Snapshot = str
		case "image":
			cfg.Image = str
		}
	}
	return cfg, nil
}

// lookupVariable resolves a Variable like Python's resolve_variable.
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

// Open returns the sandbox for the resolved parameters, creating it on first
// use; later opens with the same values reuse it. The release func is a no-op:
// the environment owns the lifecycle. cx may be nil when no parameter is a
// Variable.
func (e *Environment) Open(ctx context.Context, cx *agocontext.Context) (toolpacks.Sandbox, func() error, error) {
	cfg, err := e.resolve(cx)
	if err != nil {
		return nil, nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, nil, err
	}
	key := cacheKey(cfg)
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

// Close deletes every cached sandbox. Idempotent; the environment can be opened
// again afterwards, which creates fresh sandboxes.
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

// cacheKey identifies a sandbox by everything that shapes it; the key is hashed
// so the API key does not sit in the map as plain text.
func cacheKey(cfg Config) string {
	b, _ := json.Marshal(cfg) // map keys are sorted: a stable key; HTTPClient is excluded
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// --- helpers -----------------------------------------------------------------

func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// shellJoin renders argv as one POSIX shell command line (Python: shlex.join):
// a word of safe characters stays bare, anything else is single-quoted.
func shellJoin(argv []string) string {
	words := make([]string, len(argv))
	for i, a := range argv {
		words[i] = shellWord(a)
	}
	return strings.Join(words, " ")
}

func shellWord(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%+=:,./-_", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
