package toolpacks

import (
	stdcontext "context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	agocontext "github.com/luckystrker/ago/context"
)

// memSandbox is an in-memory Sandbox: it interprets a handful of argv shapes
// instead of running processes, so tests can drive the shell and code tools
// through the seam without a host interpreter.
type memSandbox struct {
	mu     sync.Mutex
	files  map[string][]byte
	execs  [][]string
	opts   []ExecOptions
	closed bool
}

func (m *memSandbox) Workdir() string     { return "/work" }
func (m *memSandbox) HostWorkdir() string { return "" }

func (m *memSandbox) Exec(_ stdcontext.Context, argv []string, opts ExecOptions) (ExecResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.execs = append(m.execs, append([]string(nil), argv...))
	m.opts = append(m.opts, opts)
	switch {
	case len(argv) == 3 && argv[0] == "python" && argv[1] == "-c":
		return ExecResult{Output: "py:" + argv[2]}, nil
	case len(argv) == 3 && argv[0] == "sh" && argv[1] == "-c":
		if argv[2] == "false" {
			return ExecResult{Output: "", ExitCode: 1}, nil
		}
		return ExecResult{Output: "sh:" + argv[2]}, nil
	case len(argv) == 2 && argv[0] == "node":
		body, ok := m.files[strings.TrimPrefix(argv[1], "/work/")]
		if !ok {
			return ExecResult{Output: "no such file", ExitCode: 2}, nil
		}
		return ExecResult{Output: "node:" + string(body)}, nil
	case len(argv) == 3 && argv[0] == "rm" && argv[1] == "-f":
		delete(m.files, strings.TrimPrefix(argv[2], "/work/"))
		return ExecResult{}, nil
	case len(argv) >= 1 && argv[0] == "ls":
		return ExecResult{Output: "ls:" + strings.Join(argv[1:], ",")}, nil
	}
	return ExecResult{Output: "not found", ExitCode: 127}, nil
}

func (m *memSandbox) PutFile(_ stdcontext.Context, p string, content []byte) error {
	if !validSandboxPath(p) {
		return fmt.Errorf("%w: %q", ErrSandboxPath, p)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[p] = append([]byte(nil), content...)
	return nil
}

func (m *memSandbox) GetFile(_ stdcontext.Context, p string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.files[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return b, nil
}

func (m *memSandbox) Close() error { m.mu.Lock(); m.closed = true; m.mu.Unlock(); return nil }

// memFactory opens one memSandbox per call and counts opens and releases, the
// way a per-call container factory would.
type memFactory struct {
	mu       sync.Mutex
	sb       *memSandbox
	opens    int
	releases int
	closed   bool
}

func newMemFactory() *memFactory {
	return &memFactory{sb: &memSandbox{files: map[string][]byte{}}}
}

func (f *memFactory) Open(_ stdcontext.Context, _ *agocontext.Context) (Sandbox, func() error, error) {
	f.mu.Lock()
	f.opens++
	f.mu.Unlock()
	return f.sb, func() error { f.mu.Lock(); f.releases++; f.mu.Unlock(); return nil }, nil
}

func (f *memFactory) Workdir() string { return "/work" }
func (f *memFactory) Close() error    { f.mu.Lock(); f.closed = true; f.mu.Unlock(); return f.sb.Close() }

// TestSandboxSeamShellRunsThroughBackend: the shell tool executes through a
// fake in-memory backend — commands as sh -c, restricted commands as their
// checked argv, filters and exit-code formatting intact, per-call lifecycle
// balanced, Close reaching the backend.
func TestSandboxSeamShellRunsThroughBackend(t *testing.T) {
	f := newMemFactory()
	s, err := NewShellTool(WithShellSandbox(f))
	if err != nil {
		t.Fatalf("NewShellTool(WithShellSandbox): %v", err)
	}
	_, cx := newTestContext()
	if got := resultPartText(t, callTool(t, s, cx, s.Name(), `{"command":"echo hi"}`)); got != "sh:echo hi" {
		t.Fatalf("output = %q, want the backend's sh -c result", got)
	}
	if got := resultPartText(t, callTool(t, s, cx, s.Name(), `{"command":"false"}`)); got != "[exit code: 1]" {
		t.Fatalf("failing command = %q, want the exit-code suffix", got)
	}
	if want := []string{"sh", "-c", "echo hi"}; strings.Join(f.sb.execs[0], "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want %q", f.sb.execs[0], want)
	}
	if f.opens != f.releases || f.opens != 2 {
		t.Fatalf("opens=%d releases=%d, want a balanced lifecycle of 2", f.opens, f.releases)
	}
	if schemas, _ := s.Schemas(cx); !strings.Contains(fmt.Sprint(schemas), "/work") {
		t.Fatalf("schema %v does not name the backend's working directory", schemas)
	}

	// Restricted mode: the filter applies unchanged and the argv reaches the
	// backend without a shell.
	r, err := NewShellTool(WithShellSandbox(f), WithShellAllowed("ls"))
	if err != nil {
		t.Fatal(err)
	}
	if got := resultPartText(t, callTool(t, r, cx, r.Name(), `{"command":"ls -la /x"}`)); got != "ls:-la,/x" {
		t.Fatalf("restricted allowed command = %q", got)
	}
	if got := resultPartText(t, callTool(t, r, cx, r.Name(), `{"command":"rm -rf /"}`)); !strings.Contains(got, "Command not allowed") {
		t.Fatalf("restricted refused command = %q", got)
	}
	if n := len(f.sb.execs); n != 3 { // two earlier calls + the allowed ls; rm never reached the backend
		t.Fatalf("backend saw %d commands, want 3 (the refused one must not reach it)", n)
	}

	// Local-only options conflict with a backend.
	if _, err := NewShellTool(WithShellSandbox(f), WithShellEnv(map[string]string{"A": "b"})); err == nil {
		t.Fatal("WithShellEnv + WithShellSandbox must fail: the backend owns the environment")
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if !f.closed || !f.sb.closed {
		t.Fatalf("Close did not reach the backend (factory=%v sandbox=%v)", f.closed, f.sb.closed)
	}
	if _, err := s.run(cx, "echo late"); !errors.Is(err, ErrSandboxClosed) {
		t.Fatalf("call after Close: err = %v, want ErrSandboxClosed", err)
	}
}

// TestSandboxSeamCodeRunsThroughBackend: the code tool executes through the
// same fake — inline languages as argv, file languages via PutFile + exec +
// cleanup — and also over a hand-written CodeEnvironment.
func TestSandboxSeamCodeRunsThroughBackend(t *testing.T) {
	f := newMemFactory()
	c, err := NewSandboxCodeToolFromFactory(f, WithCodeLanguages("python", "javascript"))
	if err != nil {
		t.Fatalf("NewSandboxCodeToolFromFactory: %v", err)
	}
	_, cx := newTestContext()
	if got := resultPartText(t, callTool(t, c, cx, c.Name(), `{"code":"print(1)","language":"python"}`)); got != "py:print(1)" {
		t.Fatalf("inline python = %q", got)
	}
	got := resultPartText(t, callTool(t, c, cx, c.Name(), `{"code":"log(2)","language":"javascript"}`))
	if got != "node:log(2)" {
		t.Fatalf("file-mode javascript = %q, want the uploaded snippet executed", got)
	}
	if len(f.sb.files) != 0 {
		t.Fatalf("temp snippet files left behind: %v", f.sb.files)
	}
	got = resultPartText(t, callTool(t, c, cx, c.Name(), `{"code":"x","language":"bash"}`))
	if !strings.Contains(got, "is not enabled") || !strings.HasSuffix(got, "[exit code: 2]") {
		t.Fatalf("disabled language = %q, want the not-enabled notice with exit code 2", got)
	}
	if f.opens != f.releases {
		t.Fatalf("opens=%d releases=%d", f.opens, f.releases)
	}
	if !strings.Contains(fmt.Sprint(c.Schemas(cx)), "python, javascript") {
		t.Fatal("description does not list the supported languages")
	}
	if err := c.Close(); err != nil || !f.closed {
		t.Fatalf("Close: %v, factory closed=%v", err, f.closed)
	}

	// A hand-written environment works without any Sandbox.
	custom, err := NewSandboxCodeTool(fakeCodeEnv{}, WithCodeName("exec_snippet"))
	if err != nil {
		t.Fatal(err)
	}
	if got := resultPartText(t, callTool(t, custom, cx, "exec_snippet", `{"code":"1+1","language":"calc"}`)); got != "calc:1+1\n[exit code: 4]" {
		t.Fatalf("custom environment = %q", got)
	}
	// Adapter and local-only options cannot be mixed with a ready environment.
	for _, opt := range []CodeRunnerOption{WithCodeLanguages("python"), WithCodeWorkdir(t.TempDir())} {
		if _, err := NewSandboxCodeTool(fakeCodeEnv{}, opt); err == nil {
			t.Fatal("an option that cannot apply must fail construction")
		}
	}
	if _, err := NewSandboxCodeToolFromFactory(f, WithCodeEnv(map[string]string{"A": "b"})); err == nil {
		t.Fatal("local-only option must fail over a factory")
	}
}

type fakeCodeEnv struct{}

func (fakeCodeEnv) SupportedLanguages() []string { return []string{"calc"} }
func (fakeCodeEnv) Run(_ stdcontext.Context, _ *agocontext.Context, code, language string) (CodeRunResult, error) {
	return CodeRunResult{Output: language + ":" + code, ExitCode: 4}, nil
}

// TestLocalSandboxThroughTheSeam: the local backend implements the same
// interface — exec, file upload/download/removal confined to the workdir, and a
// Close that removes the auto-created directory.
func TestLocalSandboxThroughTheSeam(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	sb, err := NewLocalSandbox()
	if err != nil {
		t.Fatal(err)
	}
	ctx := stdcontext.Background()
	if err := sb.PutFile(ctx, "sub/a.txt", []byte("payload")); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	res, err := sb.Exec(ctx, []string{"sh", "-c", "cat sub/a.txt; echo $X"}, ExecOptions{Env: map[string]string{"X": "envval"}})
	if err != nil || res.ExitCode != 0 || res.Output != "payloadenvval" { // cat has no trailing newline
		t.Fatalf("Exec = %+v, %v", res, err)
	}
	if b, err := sb.GetFile(ctx, "sub/a.txt"); err != nil || string(b) != "payload" {
		t.Fatalf("GetFile = %q, %v", b, err)
	}
	for _, bad := range []string{"../x", "/etc/passwd", ""} {
		if err := sb.PutFile(ctx, bad, nil); !errors.Is(err, ErrSandboxPath) {
			t.Fatalf("PutFile(%q) = %v, want ErrSandboxPath", bad, err)
		}
	}
	if err := sb.RemoveFile(ctx, "sub/a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := sb.RemoveFile(ctx, "sub/a.txt"); err != nil {
		t.Fatalf("removing a missing file must not fail: %v", err)
	}
	if _, err := sb.GetFile(ctx, "sub/a.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("GetFile after remove: %v", err)
	}

	// The local sandbox serves the code adapter like any other backend.
	f, err := NewSingletonFactory(sb)
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewCodeAdapter(f, WithAdapterLanguages("bash"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Run(ctx, nil, "echo via_adapter", "sh")
	if err != nil || out.ExitCode != 0 || out.Output != "via_adapter" {
		t.Fatalf("adapter over LocalSandbox = %+v, %v", out, err)
	}

	dir := sb.HostWorkdir()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("auto workdir survived Close: %v", err)
	}
	if _, err := sb.Exec(ctx, []string{"true"}, ExecOptions{}); !errors.Is(err, ErrSandboxClosed) {
		t.Fatalf("Exec after Close = %v, want ErrSandboxClosed", err)
	}
}

// releaseFactory is a factory whose release func fails, like a container that
// cannot be stopped.
type releaseFactory struct {
	sb         Sandbox
	openErr    error
	releaseErr error
	released   int
}

func (f *releaseFactory) Open(stdcontext.Context, *agocontext.Context) (Sandbox, func() error, error) {
	if f.openErr != nil {
		return nil, nil, f.openErr
	}
	return f.sb, func() error { f.released++; return f.releaseErr }, nil
}

// TestWithSandboxReleaseError: the error of stopping the sandbox surfaces when
// the call itself succeeded, never replaces the call's own error, and release
// runs on every path.
func TestWithSandboxReleaseError(t *testing.T) {
	ctx := stdcontext.Background()
	errRelease := errors.New("container stop failed")
	errCall := errors.New("exec failed")
	sb := &memSandbox{files: map[string][]byte{}}

	f := &releaseFactory{sb: sb, releaseErr: errRelease}
	if err := withSandbox(ctx, nil, f, func(Sandbox) error { return nil }); !errors.Is(err, errRelease) {
		t.Fatalf("successful call + failing release = %v, want the release error", err)
	}
	f = &releaseFactory{sb: sb, releaseErr: errRelease}
	if err := withSandbox(ctx, nil, f, func(Sandbox) error { return errCall }); !errors.Is(err, errCall) || errors.Is(err, errRelease) {
		t.Fatalf("failing call + failing release = %v, want the call error only", err)
	}
	if f.released != 1 {
		t.Fatalf("release ran %d times on the error path, want 1", f.released)
	}
	f = &releaseFactory{sb: sb}
	if err := withSandbox(ctx, nil, f, func(Sandbox) error { return nil }); err != nil || f.released != 1 {
		t.Fatalf("clean call = %v, released %d", err, f.released)
	}
	errOpen := errors.New("no capacity")
	f = &releaseFactory{openErr: errOpen}
	called := false
	if err := withSandbox(ctx, nil, f, func(Sandbox) error { called = true; return nil }); !errors.Is(err, errOpen) || called || f.released != 0 {
		t.Fatalf("open failure = %v (callback ran=%v, released=%d)", err, called, f.released)
	}
	// A factory may return a nil release.
	nilRelease := nilReleaseFactory{sb}
	if err := withSandbox(ctx, nil, nilRelease, func(Sandbox) error { return nil }); err != nil {
		t.Fatalf("nil release func: %v", err)
	}

	// Through the tool: the stop failure reaches the model as a tool error and
	// does not turn into a result.
	shell, err := NewShellTool(WithShellSandbox(&releaseFactory{sb: sb, releaseErr: errRelease}))
	if err != nil {
		t.Fatal(err)
	}
	_, cx := newTestContext()
	if got := resultPartText(t, callTool(t, shell, cx, shell.Name(), `{"command":"echo hi"}`)); !strings.Contains(got, "TOOL_ERROR") || !strings.Contains(got, "container stop failed") {
		t.Fatalf("shell with failing release = %q, want the release error", got)
	}
}

type nilReleaseFactory struct{ sb Sandbox }

func (f nilReleaseFactory) Open(stdcontext.Context, *agocontext.Context) (Sandbox, func() error, error) {
	return f.sb, nil, nil
}

func TestValidSandboxPath(t *testing.T) {
	for p, want := range map[string]bool{
		"a.txt":     true,
		"sub/dir/a": true,
		"..a":       true, // not a ".." segment
		"":          false,
		"/abs":      false,
		`\win`:      false,
		"c:":        false,
		`c:\x`:      false,
		"C:/x":      false,
		"../x":      false,
		"a/../b":    false,
		"a/..":      false,
	} {
		if got := validSandboxPath(p); got != want {
			t.Errorf("validSandboxPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestSingletonFactoryWorkdirAndNil(t *testing.T) {
	if _, err := NewSingletonFactory(nil); err == nil {
		t.Fatal("a nil sandbox must be rejected")
	}
	// A sandbox without a host view reports its sandbox-side path.
	mem, _ := NewSingletonFactory(&memSandbox{files: map[string][]byte{}})
	if got := mem.Workdir(); got != "/work" {
		t.Fatalf("Workdir over a remote-style sandbox = %q, want /work", got)
	}
	// A host-backed sandbox reports the host path, so host-side checks work.
	local, err := NewLocalSandbox()
	if err != nil {
		t.Fatal(err)
	}
	lf, _ := NewSingletonFactory(local)
	if got := lf.Workdir(); got != local.HostWorkdir() {
		t.Fatalf("Workdir over LocalSandbox = %q, want the host path %q", got, local.HostWorkdir())
	}
	if lf.Sandbox() != Sandbox(local) {
		t.Fatal("Sandbox() must return the wrapped sandbox")
	}
	_ = lf.Close()
	// A factory without WorkdirAware assumes /workspace.
	if got := factoryWorkdir(&releaseFactory{}); got != "/workspace" {
		t.Fatalf("default workdir = %q", got)
	}
}

// TestAdapterTimeoutZeroValues: zero timeout leaves the backend default (the
// option is accepted and Exec sees 0); a positive one is passed through; a
// negative one is rejected.
func TestAdapterTimeoutZeroValues(t *testing.T) {
	f := newMemFactory()
	ctx := stdcontext.Background()
	for _, tc := range []struct {
		opts []CodeAdapterOption
		want time.Duration
	}{
		{nil, 0},
		{[]CodeAdapterOption{WithAdapterTimeout(0)}, 0},
		{[]CodeAdapterOption{WithAdapterTimeout(7 * time.Second)}, 7 * time.Second},
	} {
		a, err := NewCodeAdapter(f, tc.opts...)
		if err != nil {
			t.Fatalf("NewCodeAdapter: %v", err)
		}
		f.sb.opts = nil
		if _, err := a.Run(ctx, nil, "x", "python"); err != nil {
			t.Fatal(err)
		}
		if got := f.sb.opts[0].Timeout; got != tc.want {
			t.Fatalf("Exec timeout = %v, want %v", got, tc.want)
		}
	}
	if _, err := NewCodeAdapter(f, WithAdapterTimeout(-time.Second)); err == nil {
		t.Fatal("a negative timeout must be rejected")
	}
}

// TestCheckIgnorePosix: the ignore guard over a remote sandbox's POSIX workdir
// is purely lexical — escapes and patterns are refused, absolute tokens replace
// the workdir (posixpath.join), and nothing is read from the host filesystem.
func TestCheckIgnorePosix(t *testing.T) {
	patterns := []string{".env", "*.pem", "secrets"}
	for _, tc := range []struct {
		cmd, workdir string
		denied       string // "" = allowed
	}{
		{"cat notes.txt", "/workspace", ""},
		{"cat sub/dir/ok.go", "/workspace", ""},
		{"cat .env", "/workspace", "/workspace/.env"},
		{"cat sub/.env", "/workspace", "/workspace/sub/.env"},
		{"cat 'a b/../.env'", "/workspace", "/workspace/.env"},
		{"cat key.pem", "/workspace", "/workspace/key.pem"},
		{"cat deep/key.pem", "/workspace", "/workspace/deep/key.pem"},
		{"ls secrets/x", "/workspace", "/workspace/secrets/x"},
		{"prog --output=.env", "/workspace", "/workspace/.env"},
		{"prog -o.env", "/workspace", "/workspace/.env"},
		{"cat ../outside", "/workspace", "/outside"},
		{"cat /etc/passwd", "/workspace", "/etc/passwd"},
		{"cat /workspace/ok.txt", "/workspace", ""},
		{"cat /workspace/../etc/passwd", "/workspace", "/etc/passwd"},
		{"cat /workspace2/x", "/workspace", "/workspace2/x"}, // sibling prefix is not inside
		{"cat ok.txt", "/workspace/", ""},                    // trailing slash normalised
		{"cat x", "/", ""},
		{"cat /etc/passwd", "/", ""},
		{"cat unbalanced 'quote", "/workspace", ""}, // falls back to whitespace split
	} {
		got := checkIgnorePosix(tc.cmd, tc.workdir, patterns)
		want := ""
		if tc.denied != "" {
			want = "Access denied: " + tc.denied
		}
		if got != want {
			t.Errorf("checkIgnorePosix(%q, %q) = %q, want %q", tc.cmd, tc.workdir, got, want)
		}
	}
	if got := checkIgnorePosix("cat .env", "/workspace", nil); got != "" {
		t.Fatalf("no patterns must allow everything, got %q", got)
	}
}

// TestShellIgnoreRemoteWorkdirDoesNotTouchHost: for a container-style factory
// the guard checks the POSIX path lexically. The workdir here also exists on the
// host with a symlink pointing outside; the host-mode guard would refuse it, the
// remote one must not look at the host at all.
func TestShellIgnoreRemoteWorkdirDoesNotTouchHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses symlinks")
	}
	hostDir := t.TempDir()
	if err := os.Symlink("/etc", filepath.Join(hostDir, "link")); err != nil {
		t.Fatal(err)
	}
	f := newMemFactory()
	remote := &workdirFactory{memFactory: f, dir: hostDir}
	s, err := NewShellTool(WithShellSandbox(remote), WithShellAllowed("ls"), WithShellIgnore(".env"))
	if err != nil {
		t.Fatal(err)
	}
	_, cx := newTestContext()
	if got := resultPartText(t, callTool(t, s, cx, s.Name(), `{"command":"ls link/passwd"}`)); got != "ls:link/passwd" {
		t.Fatalf("remote guard = %q: the host symlink must not be resolved", got)
	}
	if got := resultPartText(t, callTool(t, s, cx, s.Name(), `{"command":"ls .env"}`)); !strings.HasPrefix(got, "Access denied:") {
		t.Fatalf("remote guard must still refuse the ignored name, got %q", got)
	}
	if got := resultPartText(t, callTool(t, s, cx, s.Name(), `{"command":"ls ../x"}`)); !strings.HasPrefix(got, "Access denied:") {
		t.Fatalf("remote guard must refuse an escape, got %q", got)
	}

	// A host-backed singleton keeps the physical check: the same symlink is refused.
	local, err := NewLocalSandbox(WithLocalSandboxDir(hostDir))
	if err != nil {
		t.Fatal(err)
	}
	sf, _ := NewSingletonFactory(local)
	hs, err := NewShellTool(WithShellSandbox(sf), WithShellAllowed("ls"), WithShellIgnore(".env"))
	if err != nil {
		t.Fatal(err)
	}
	if got := resultPartText(t, callTool(t, hs, cx, hs.Name(), `{"command":"ls link/passwd"}`)); !strings.HasPrefix(got, "Access denied:") {
		t.Fatalf("host-backed guard must follow the symlink out of the workdir, got %q", got)
	}
	// A singleton over a sandbox without a host view is remote.
	mem, _ := NewSingletonFactory(&memSandbox{files: map[string][]byte{}})
	if hostBacked(mem) || !hostBacked(sf) || hostBacked(remote) {
		t.Fatalf("hostBacked: mem=%v local=%v factory=%v", hostBacked(mem), hostBacked(sf), hostBacked(remote))
	}
}

// workdirFactory is a non-singleton factory that reports a chosen workdir.
type workdirFactory struct {
	*memFactory
	dir string
}

func (w *workdirFactory) Workdir() string { return w.dir }
