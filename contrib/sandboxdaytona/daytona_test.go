package sandboxdaytona

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/toolpacks"
	"github.com/luckystrker/ago/tools"
)

type call struct {
	Method, Path, Query, Auth, ContentType string
	Body                                   []byte
}

func (c call) String() string {
	if c.Query != "" {
		return c.Method + " " + c.Path + "?" + c.Query
	}
	return c.Method + " " + c.Path
}

func (c call) JSON(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(c.Body, &m); err != nil {
		t.Fatalf("%s: body is not JSON: %v (%q)", c, err, c.Body)
	}
	return m
}

// fakeDaytona implements the slice of the Daytona REST API the backend uses:
// the control plane under /sandbox and the toolbox under /toolbox/{id}/....
type fakeDaytona struct {
	t   *testing.T
	srv *httptest.Server

	mu    sync.Mutex
	calls []call
	next  int
	// states are the successive state answers for a freshly created sandbox
	// (POST then each GET); the last one repeats.
	states      []string
	errorReason string
	files       map[string][]byte
	// run answers process/execute.
	run func(command string, body map[string]any) (status int, result string, code int)
	// onPoll, when set, is called with the poll count on every GET /sandbox/{id}.
	onPoll func(n int)
	// omitProxy leaves toolboxProxyUrl out of sandbox responses; proxyStatus and
	// proxyBody then shape GET /sandbox/{id}/toolbox-proxy-url (default: the
	// fake's own toolbox URL).
	omitProxy bool
	// omitProxyOnGet drops toolboxProxyUrl from poll responses only.
	omitProxyOnGet bool
	// forced injects an API error for a matching request.
	forced      func(c call) (status int, msg string)
	proxyStatus int
	proxyBody   map[string]any
}

func newFakeDaytona(t *testing.T) *fakeDaytona {
	t.Helper()
	f := &fakeDaytona{t: t, states: []string{"started"}, files: map[string][]byte{},
		run: func(string, map[string]any) (int, string, int) { return 200, "", 0 }}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDaytona) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.String()
	}
	return out
}

func (f *fakeDaytona) Find(prefix string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if strings.HasPrefix(c.Method+" "+c.Path, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeDaytona) state(n int) string {
	if n >= len(f.states) {
		n = len(f.states) - 1
	}
	return f.states[n]
}

func (f *fakeDaytona) handle(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c := call{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), b}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	sandboxJSON := func(id string, n int) map[string]any {
		m := map[string]any{"id": id, "state": f.state(n)}
		if !f.omitProxy {
			m["toolboxProxyUrl"] = f.srv.URL + "/toolbox"
		}
		if f.state(n) == "error" {
			m["errorReason"] = f.errorReason
		}
		return m
	}
	if f.forced != nil {
		if status, msg := f.forced(c); status != 0 {
			reply(status, map[string]string{"message": msg})
			return
		}
	}
	p := r.URL.Path
	switch {
	case r.Method == http.MethodPost && p == "/sandbox":
		f.mu.Lock()
		f.next++
		id := fmt.Sprintf("sb-%d", f.next)
		f.mu.Unlock()
		reply(200, sandboxJSON(id, 0))
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/toolbox-proxy-url"):
		status, body := f.proxyStatus, f.proxyBody
		if status == 0 {
			status, body = 200, map[string]any{"url": f.srv.URL + "/toolbox"}
		}
		reply(status, body)
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/sandbox/"):
		polls := len(f.Find("GET " + p)) // this request is already recorded
		if f.onPoll != nil {
			f.onPoll(polls)
		}
		m := sandboxJSON(strings.TrimPrefix(p, "/sandbox/"), polls)
		if f.omitProxyOnGet {
			delete(m, "toolboxProxyUrl")
		}
		reply(200, m)
	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/sandbox/"):
		reply(200, map[string]string{"state": "destroyed"})
	case strings.HasPrefix(p, "/toolbox/"):
		rest := strings.SplitN(strings.TrimPrefix(p, "/toolbox/"), "/", 2)
		endpoint := rest[1]
		switch {
		case r.Method == http.MethodPost && endpoint == "process/execute":
			var body map[string]any
			_ = json.Unmarshal(b, &body)
			status, result, code := f.run(fmt.Sprint(body["command"]), body)
			if status != 200 {
				reply(status, map[string]string{"message": result})
				return
			}
			reply(200, map[string]any{"result": result, "exitCode": code})
		case r.Method == http.MethodPost && endpoint == "files/folder":
			w.WriteHeader(200)
		case r.Method == http.MethodPost && endpoint == "files/upload-v2":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				// the body was consumed above: re-parse from the recorded bytes
				req2, _ := http.NewRequest("POST", "/", strings.NewReader(string(b)))
				req2.Header.Set("Content-Type", r.Header.Get("Content-Type"))
				if err := req2.ParseMultipartForm(1 << 20); err != nil {
					reply(400, map[string]string{"message": "bad multipart"})
					return
				}
				file, _, err := req2.FormFile("file")
				if err != nil {
					reply(400, map[string]string{"message": "no file field"})
					return
				}
				content, _ := io.ReadAll(file)
				f.mu.Lock()
				f.files[r.URL.Query().Get("path")] = content
				f.mu.Unlock()
				w.WriteHeader(200)
				return
			}
			w.WriteHeader(500)
		case r.Method == http.MethodGet && endpoint == "files/download":
			f.mu.Lock()
			content, ok := f.files[r.URL.Query().Get("path")]
			f.mu.Unlock()
			if !ok {
				reply(404, map[string]string{"message": "file not found"})
				return
			}
			w.Write(content)
		case r.Method == http.MethodDelete && endpoint == "files":
			f.mu.Lock()
			_, ok := f.files[r.URL.Query().Get("path")]
			delete(f.files, r.URL.Query().Get("path"))
			f.mu.Unlock()
			if !ok {
				reply(404, map[string]string{"message": "file not found"})
				return
			}
			w.WriteHeader(200)
		default:
			reply(404, map[string]string{"message": "unhandled toolbox " + r.Method + " " + endpoint})
		}
	default:
		reply(404, map[string]string{"message": "unhandled " + r.Method + " " + p})
	}
}

func newEnv(t *testing.T, f *fakeDaytona, opts ...Option) *Environment {
	t.Helper()
	env, err := NewEnvironment(append([]Option{
		WithAPIKey("key-123"), WithAPIURL(f.srv.URL), WithPollInterval(5 * time.Millisecond),
	}, opts...)...)
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	t.Cleanup(func() { env.Close() })
	return env
}

func TestCreateFromImageWithResources(t *testing.T) {
	f := newFakeDaytona(t)
	f.states = []string{"creating", "starting", "started"} // POST, then two polls
	env := newEnv(t, f, WithImage("python:3.12"), WithEnvVars(map[string]string{"A": "1"}),
		WithResources(Resources{CPU: 2, Memory: 4, Disk: 10}), WithTarget("eu"))
	sb, release, err := env.Open(context.Background(), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	creates := f.Find("POST /sandbox")
	if len(creates) != 1 {
		t.Fatalf("creates = %v", f.Calls())
	}
	c := creates[0]
	if c.Auth != "Bearer key-123" {
		t.Fatalf("Authorization = %q", c.Auth)
	}
	want := map[string]any{
		"autoStopInterval": float64(0), "env": map[string]any{"A": "1"}, "target": "eu",
		"buildInfo": map[string]any{"dockerfileContent": "FROM python:3.12\n"},
		"cpu":       float64(2), "memory": float64(4), "disk": float64(10),
	}
	if got, _ := json.Marshal(c.JSON(t)); !sameJSON(t, got, want) {
		t.Fatalf("create body = %s, want %v", got, want)
	}
	if polls := f.Find("GET /sandbox/sb-1"); len(polls) != 2 {
		t.Fatalf("state polls = %d, want 2 (creating, starting -> started)", len(polls))
	}
	// The workdir is created before first use.
	if mk := f.Find("POST /toolbox/sb-1/files/folder"); len(mk) != 1 || mk[0].Query != "mode=755&path=%2Fworkspace" {
		t.Fatalf("workdir creation = %v", mk)
	}
	if sb.Workdir() != "/workspace" || sb.HostWorkdir() != "" || env.Workdir() != "/workspace" {
		t.Fatalf("Workdir=%q HostWorkdir=%q", sb.Workdir(), sb.HostWorkdir())
	}
}

func TestCreateFromSnapshotIgnoresResources(t *testing.T) {
	f := newFakeDaytona(t)
	env := newEnv(t, f, WithSnapshot("snap-1"), WithResources(Resources{CPU: 8}))
	if _, _, err := env.Open(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	body := f.Find("POST /sandbox")[0].JSON(t)
	if got, _ := json.Marshal(body); !sameJSON(t, got, map[string]any{"autoStopInterval": 0, "snapshot": "snap-1"}) {
		t.Fatalf("snapshot create body = %s, want only snapshot + auto-stop (resources are image-only)", got)
	}
	// Neither snapshot nor image: the account default.
	f2 := newFakeDaytona(t)
	env2 := newEnv(t, f2)
	env2.Open(context.Background(), nil)
	if got, _ := json.Marshal(f2.Find("POST /sandbox")[0].JSON(t)); !sameJSON(t, got, map[string]any{"autoStopInterval": 0}) {
		t.Fatalf("default create body = %s", got)
	}
}

func TestCreateFailureDeletesTheSandbox(t *testing.T) {
	f := newFakeDaytona(t)
	f.states = []string{"creating", "error"}
	f.errorReason = "image build failed"
	env := newEnv(t, f)
	_, _, err := env.Open(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "image build failed") {
		t.Fatalf("Open = %v, want the error reason", err)
	}
	if len(f.Find("DELETE /sandbox/sb-1")) != 1 {
		t.Fatalf("a failed sandbox must be deleted, not leaked: %v", f.Calls())
	}
}

// TestSandboxThatNeverStartsIsCanceledDeterministically: the wait ends by an
// explicit cancel after the second poll — no timing involved — and the error is
// always the typed "did not start" one wrapping the context error; the half
// created sandbox is deleted.
func TestSandboxThatNeverStartsIsCanceledDeterministically(t *testing.T) {
	f := newFakeDaytona(t)
	f.states = []string{"creating"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.onPoll = func(n int) {
		if n == 2 {
			cancel()
		}
	}
	env := newEnv(t, f)
	_, _, err := env.Open(ctx, nil)
	if err == nil || !strings.Contains(err.Error(), `did not start (state "creating")`) || !errors.Is(err, context.Canceled) {
		t.Fatalf("Open = %v, want the typed did-not-start error wrapping context.Canceled", err)
	}
	if len(f.Find("DELETE /sandbox/sb-1")) != 1 {
		t.Fatalf("a sandbox that never started must be deleted: %v", f.Calls())
	}
}

// TestCreateTimeoutIsAlwaysTheTypedError: whichever request or wait the create
// deadline lands in, the outcome is one typed error wrapping DeadlineExceeded
// (the deadline may fall inside the POST, inside a poll, or between polls).
func TestCreateTimeoutIsAlwaysTheTypedError(t *testing.T) {
	f := newFakeDaytona(t)
	f.states = []string{"creating"}
	env := newEnv(t, f, WithCreateTimeout(40*time.Millisecond))
	_, _, err := env.Open(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "did not start") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Open = %v, want did-not-start wrapping context.DeadlineExceeded", err)
	}
}

// TestHTTPClientOptionIsUsed: WithHTTPClient routes every call — control plane
// and toolbox — through the supplied client.
func TestHTTPClientOptionIsUsed(t *testing.T) {
	f := newFakeDaytona(t)
	rt := &countingTransport{base: http.DefaultTransport}
	env := newEnv(t, f, WithHTTPClient(&http.Client{Transport: rt}))
	sb, _, err := env.Open(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sb.Exec(context.Background(), []string{"ls"}, toolpacks.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, want := rt.count(), len(f.Calls()); got != want || got < 3 {
		t.Fatalf("custom client carried %d requests, the server saw %d (want all, at least create, folder, execute)", got, want)
	}
	// The default client is installed when none is given.
	plain := newEnv(t, f)
	if plain.cfg.HTTPClient == nil {
		t.Fatal("no default HTTP client")
	}
}

type countingTransport struct {
	mu   sync.Mutex
	n    int
	base http.RoundTripper
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.base.RoundTrip(r)
}

func (c *countingTransport) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// TestToolboxProxyURLResolvedLikeTheSDK: when the sandbox response carries no
// toolboxProxyUrl the backend asks GET /sandbox/{id}/toolbox-proxy-url (the
// SDK's _ensure_toolbox_proxy_url) and uses that URL; a failing or empty answer
// aborts the open instead of guessing an address.
func TestToolboxProxyURLResolvedLikeTheSDK(t *testing.T) {
	f := newFakeDaytona(t)
	f.omitProxy = true
	env := newEnv(t, f)
	sb, _, err := env.Open(context.Background(), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := f.Find("GET /sandbox/sb-1/toolbox-proxy-url"); len(got) != 1 || got[0].Auth != "Bearer key-123" {
		t.Fatalf("proxy URL lookups = %v", got)
	}
	if _, err := sb.Exec(context.Background(), []string{"ls"}, toolpacks.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(f.Find("POST /toolbox/sb-1/process/execute")) != 1 {
		t.Fatalf("exec did not go through the resolved proxy: %v", f.Calls())
	}
	// A normal response never triggers the lookup.
	g := newFakeDaytona(t)
	genv := newEnv(t, g)
	genv.Open(context.Background(), nil)
	if len(g.Find("GET /sandbox/sb-1/toolbox-proxy-url")) != 0 {
		t.Fatal("proxy URL lookup although the response carried one")
	}
	for name, setup := range map[string]func(*fakeDaytona){
		"404":   func(x *fakeDaytona) { x.proxyStatus, x.proxyBody = 404, map[string]any{"message": "gone"} },
		"empty": func(x *fakeDaytona) { x.proxyStatus, x.proxyBody = 200, map[string]any{"url": ""} },
	} {
		bad := newFakeDaytona(t)
		bad.omitProxy = true
		setup(bad)
		benv := newEnv(t, bad)
		if _, _, err := benv.Open(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "toolbox proxy URL") {
			t.Errorf("%s: Open = %v, want a proxy URL error", name, err)
		}
		if len(bad.Find("DELETE /sandbox/sb-1")) != 1 {
			t.Errorf("%s: the unusable sandbox must be deleted: %v", name, bad.Calls())
		}
	}
}

func TestExecRequestAndResult(t *testing.T) {
	f := newFakeDaytona(t)
	f.run = func(command string, body map[string]any) (int, string, int) { return 200, "out text\n", 3 }
	env := newEnv(t, f, WithWorkdir("/w"))
	sb, _, _ := env.Open(context.Background(), nil)
	res, err := sb.Exec(context.Background(), []string{"python", "-c", "print('a b')", ""}, toolpacks.ExecOptions{
		Timeout: 30 * time.Second, Env: map[string]string{"K": "v"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "out text\n" || res.ExitCode != 3 {
		t.Fatalf("result = %+v", res)
	}
	ex := f.Find("POST /toolbox/sb-1/process/execute")[0]
	want := map[string]any{"command": `python -c 'print('"'"'a b'"'"')' ''`, "cwd": "/w", "timeout": 30, "envs": map[string]any{"K": "v"}}
	if got, _ := json.Marshal(ex.JSON(t)); !sameJSON(t, got, want) {
		t.Fatalf("exec body = %s, want %v", got, want)
	}
	if ex.Auth != "Bearer key-123" {
		t.Fatalf("toolbox Authorization = %q", ex.Auth)
	}
	// Default timeout comes from the config; no envs key without an env.
	if _, err := sb.Exec(context.Background(), []string{"ls"}, toolpacks.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	second := f.Find("POST /toolbox/sb-1/process/execute")[1].JSON(t)
	if second["timeout"] != float64(60) || second["command"] != "ls" {
		t.Fatalf("second exec = %v", second)
	}
	if _, ok := second["envs"]; ok {
		t.Fatalf("envs must be absent without an environment: %v", second)
	}
	if res, err := sb.Exec(context.Background(), nil, toolpacks.ExecOptions{}); err != nil || res.ExitCode != 2 {
		t.Fatalf("empty argv = %+v, %v", res, err)
	}
}

func TestExecErrorMapping(t *testing.T) {
	f := newFakeDaytona(t)
	env := newEnv(t, f, WithMaxOutput(10))
	sb, _, _ := env.Open(context.Background(), nil)
	ctx := context.Background()
	for _, tc := range []struct {
		status   int
		msg      string
		wantCode int
		wantSub  string
	}{
		{429, "slow down", 1, "Daytona rate limit exceeded: slow down"},
		{408, "deadline", 124, "Execution timed out: deadline"},
		{504, "gateway", 124, "Execution timed out"},
		{500, "boom", 1, "Daytona error: daytona API error 500: boom"},
	} {
		f.run = func(string, map[string]any) (int, string, int) { return tc.status, tc.msg, 0 }
		res, err := sb.Exec(ctx, []string{"x"}, toolpacks.ExecOptions{})
		if err != nil || res.ExitCode != tc.wantCode || !strings.Contains(res.Output, tc.wantSub) {
			t.Errorf("status %d = %+v, %v; want code %d containing %q", tc.status, res, err, tc.wantCode, tc.wantSub)
		}
	}
	// Output cap.
	f.run = func(string, map[string]any) (int, string, int) { return 200, strings.Repeat("y", 40), 0 }
	if res, _ := sb.Exec(ctx, []string{"x"}, toolpacks.ExecOptions{}); res.Output != strings.Repeat("y", 10)+"\n[truncated: showing first 10 of 40 chars]" {
		t.Fatalf("capped output = %q", res.Output)
	}
	// A toolbox that never answers hits the client-side cut-off as exit 124
	// (the server's own timeout answer would normally arrive first).
	old := execGrace
	execGrace = 0
	defer func() { execGrace = old }()
	f.run = nil
	stall := newFakeDaytona(t)
	stall.run = func(string, map[string]any) (int, string, int) {
		time.Sleep(1500 * time.Millisecond)
		return 200, "late", 0
	}
	envS := newEnv(t, stall)
	sbS, _, err := envS.Open(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := sbS.Exec(ctx, []string{"sleep", "9"}, toolpacks.ExecOptions{Timeout: time.Second})
	if err != nil || res.ExitCode != 124 || res.Output != "Execution timed out after 1s" {
		t.Fatalf("stalled exec = %+v, %v", res, err)
	}
	// A canceled caller context is an error, never a result.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := sb.Exec(cctx, []string{"x"}, toolpacks.ExecOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled exec = %v", err)
	}
}

func TestFilesUploadDownloadDelete(t *testing.T) {
	f := newFakeDaytona(t)
	env := newEnv(t, f)
	sb, _, _ := env.Open(context.Background(), nil)
	ctx := context.Background()
	if err := sb.PutFile(ctx, "a.txt", []byte("top")); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if err := sb.PutFile(ctx, "sub/dir/b.txt", []byte("nested")); err != nil {
		t.Fatalf("PutFile nested: %v", err)
	}
	ups := f.Find("POST /toolbox/sb-1/files/upload-v2")
	if len(ups) != 2 || ups[0].Query != "path=%2Fworkspace%2Fa.txt" || !strings.HasPrefix(ups[0].ContentType, "multipart/form-data") {
		t.Fatalf("uploads = %v", ups)
	}
	if string(f.files["/workspace/a.txt"]) != "top" || string(f.files["/workspace/sub/dir/b.txt"]) != "nested" {
		t.Fatalf("stored files = %v", f.files)
	}
	// Folders: the workdir at startup, none for a top-level file, the parent for a nested one.
	var folders []string
	for _, c := range f.Find("POST /toolbox/sb-1/files/folder") {
		folders = append(folders, c.Query)
	}
	if fmt.Sprint(folders) != "[mode=755&path=%2Fworkspace mode=755&path=%2Fworkspace%2Fsub%2Fdir]" {
		t.Fatalf("folder calls = %v", folders)
	}
	got, err := sb.GetFile(ctx, "sub/dir/b.txt")
	if err != nil || string(got) != "nested" {
		t.Fatalf("GetFile = %q, %v", got, err)
	}
	if _, err := sb.GetFile(ctx, "nope.txt"); !errors.Is(err, os.ErrNotExist) || !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetFile missing = %v, want os.ErrNotExist and ErrNotFound", err)
	}
	rm := sb.(toolpacks.FileRemover)
	if err := rm.RemoveFile(ctx, "a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.files["/workspace/a.txt"]; ok {
		t.Fatal("file survived RemoveFile")
	}
	if err := rm.RemoveFile(ctx, "a.txt"); err != nil {
		t.Fatalf("removing a missing file must not fail: %v", err)
	}
	for _, bad := range []string{"", "/etc/passwd", "../x", "a/../../x", "."} {
		if err := sb.PutFile(ctx, bad, nil); !errors.Is(err, toolpacks.ErrSandboxPath) {
			t.Errorf("PutFile(%q) = %v, want ErrSandboxPath", bad, err)
		}
		if _, err := sb.GetFile(ctx, bad); !errors.Is(err, toolpacks.ErrSandboxPath) {
			t.Errorf("GetFile(%q) = %v", bad, err)
		}
		if err := rm.RemoveFile(ctx, bad); !errors.Is(err, toolpacks.ErrSandboxPath) {
			t.Errorf("RemoveFile(%q) = %v", bad, err)
		}
	}
}

func TestEnvironmentCacheVariablesAndClose(t *testing.T) {
	f := newFakeDaytona(t)
	env := newEnv(t, f,
		WithVariable("image", tools.NewVariable("tenant_image", "default:1")),
		WithVariable("api_key", tools.NewVariable("tenant_key", "key-123")),
		WithVariable("env_vars", tools.NewVariable("tenant_env", map[string]string{"D": "1"})))
	ctx := context.Background()
	newCx := func(vars map[string]any) *agocontext.Context {
		cx := agocontext.New(stream.NewMemoryStream())
		for k, v := range vars {
			_ = cx.SetVariable(k, v)
		}
		return cx
	}
	cxA := newCx(map[string]any{"tenant_image": "a:1", "tenant_key": "key-A", "tenant_env": map[string]any{"T": "a"}})
	cxB := newCx(map[string]any{"tenant_image": "b:1"})
	open := func(cx *agocontext.Context) toolpacks.Sandbox {
		t.Helper()
		sb, _, err := env.Open(ctx, cx)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return sb
	}
	a1, a2, b := open(cxA), open(cxA), open(cxB)
	if a1 != a2 || a1 == b {
		t.Fatalf("cache: a1==a2 %v, a1==b %v", a1 == a2, a1 == b)
	}
	creates := f.Find("POST /sandbox")
	if len(creates) != 2 {
		t.Fatalf("sandboxes created = %d, want 2", len(creates))
	}
	if creates[0].Auth != "Bearer key-A" || creates[1].Auth != "Bearer key-123" {
		t.Fatalf("per-tenant credentials: %q / %q", creates[0].Auth, creates[1].Auth)
	}
	first := creates[0].JSON(t)
	if fmt.Sprint(first["env"]) != "map[T:a]" || fmt.Sprint(first["buildInfo"]) != "map[dockerfileContent:FROM a:1\n]" {
		t.Fatalf("tenant a create body = %v", first)
	}
	if _, _, err := env.Open(ctx, nil); err == nil || !strings.Contains(err.Error(), "no Context") {
		t.Fatalf("Open(nil) with Variables = %v", err)
	}
	strict := newEnv(t, f, WithVariable("snapshot", tools.NewVariable("must_have")))
	if _, _, err := strict.Open(ctx, cxB); !errors.Is(err, tools.ErrNotFound) {
		t.Fatalf("missing variable = %v", err)
	}
	// image and snapshot together are refused once resolved.
	both := newEnv(t, f, WithSnapshot("s"), WithVariable("image", tools.NewVariable("i", "img:1")))
	if _, _, err := both.Open(ctx, cxB); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("snapshot + image = %v", err)
	}

	if err := env.Close(); err != nil {
		t.Fatal(err)
	}
	if n := len(f.Find("DELETE /sandbox/sb-1")) + len(f.Find("DELETE /sandbox/sb-2")); n != 2 {
		t.Fatalf("deleted %d sandboxes, want 2: %v", n, f.Calls())
	}
	if _, err := a1.Exec(ctx, []string{"x"}, toolpacks.ExecOptions{}); !errors.Is(err, toolpacks.ErrSandboxClosed) {
		t.Fatalf("Exec after Close = %v", err)
	}
	if err := env.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if open(cxA) == a1 {
		t.Fatal("Open after Close must create a fresh sandbox")
	}
}

func TestConcurrentOpenCreatesOneSandbox(t *testing.T) {
	f := newFakeDaytona(t)
	env := newEnv(t, f)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sb, _, err := env.Open(context.Background(), nil)
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := sb.Exec(context.Background(), []string{"x"}, toolpacks.ExecOptions{}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := len(f.Find("POST /sandbox")); n != 1 {
		t.Fatalf("sandboxes created = %d, want exactly 1", n)
	}
}

func TestConfigValidation(t *testing.T) {
	t.Setenv("DAYTONA_API_KEY", "")
	if _, err := NewEnvironment(); err == nil {
		t.Fatal("no API key anywhere must fail")
	}
	t.Setenv("DAYTONA_API_KEY", "env-key")
	t.Setenv("DAYTONA_API_URL", "http://env.example/api/")
	t.Setenv("DAYTONA_TARGET", "us")
	e, err := NewEnvironment()
	if err != nil {
		t.Fatalf("key from DAYTONA_API_KEY: %v", err)
	}
	cfg, err := e.resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.validate(); err != nil || cfg.APIKey != "env-key" || cfg.APIURL != "http://env.example/api" || cfg.Target != "us" {
		t.Fatalf("env fallbacks: %+v, %v", cfg, err)
	}
	t.Setenv("DAYTONA_API_URL", "")
	cfg2, _ := e.resolve(nil)
	if cfg2.validate(); cfg2.APIURL != DefaultAPIURL {
		t.Fatalf("default URL = %q", cfg2.APIURL)
	}
	for name, opts := range map[string][]Option{
		"snapshot and image": {WithSnapshot("s"), WithImage("i")},
		"timeout < 1s":       {WithTimeout(time.Second / 2)},
		"relative workdir":   {WithWorkdir("w")},
		"zero max output":    {WithMaxOutput(0)},
		"zero create":        {WithCreateTimeout(0)},
		"zero poll":          {WithPollInterval(0)},
		"unknown variable":   {WithVariable("timeout", tools.NewVariable("x"))},
	} {
		if _, err := NewEnvironment(opts...); err == nil {
			t.Errorf("%s: want a constructor error", name)
		}
	}
	for in, want := range map[string]string{
		"ls": "ls", "a b": "'a b'", "": "''", "it's": `'it'"'"'s'`, "a=b/c.d-e_f@g%h+i:j,k": "a=b/c.d-e_f@g%h+i:j,k", "$HOME": "'$HOME'", "a;b": "'a;b'",
	} {
		if got := shellWord(in); got != want {
			t.Errorf("shellWord(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- end to end through the toolpacks tools -----------------------------------

func callTool(t *testing.T, tool tools.Tool, name, args string) string {
	t.Helper()
	cx := agocontext.New(stream.NewMemoryStream())
	subs, err := tool.Register(cx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range subs {
		defer cx.Stream.Unsubscribe(id)
	}
	ch := make(chan events.Event, 8)
	sub := cx.Stream.Subscribe(func(ev events.Event) error { ch <- ev; return nil }, stream.SubscribeOptions{})
	defer cx.Stream.Unsubscribe(sub)
	call := events.NewToolCallEvent(name)
	call.Arguments = args
	if err := cx.Stream.Send(call); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-ch:
			switch res := events.AsValue(ev).(type) {
			case events.ToolResultEvent:
				if txt, ok := events.AsValue(res.Result.Parts[0]).(events.TextInput); ok {
					return txt.Content
				}
			case events.ToolErrorEvent:
				return "TOOL_ERROR: " + res.Error
			}
		case <-deadline:
			t.Fatal("tool call timed out")
		}
	}
}

func TestShellAndCodeRunThroughDaytona(t *testing.T) {
	f := newFakeDaytona(t)
	f.run = func(command string, body map[string]any) (int, string, int) {
		switch {
		case strings.HasPrefix(command, "sh -c "):
			return 200, "shell:" + command, 0
		case strings.HasPrefix(command, "python -c "):
			return 200, "py:" + command, 1
		case strings.HasPrefix(command, "node /workspace/ago_"):
			f.mu.Lock()
			defer f.mu.Unlock()
			for p, c := range f.files {
				if strings.HasPrefix(command, "node "+p) {
					return 200, "node:" + string(c), 0
				}
			}
		case command == "ls":
			return 200, "listing", 0
		}
		return 200, "", 0
	}
	env := newEnv(t, f)

	shell, err := toolpacks.NewShellTool(toolpacks.WithShellSandbox(env))
	if err != nil {
		t.Fatal(err)
	}
	if got := callTool(t, shell, shell.Name(), `{"command":"echo hi"}`); got != "shell:sh -c 'echo hi'" {
		t.Fatalf("shell = %q", got)
	}
	code, err := toolpacks.NewSandboxCodeToolFromFactory(env, toolpacks.WithCodeLanguages("python", "javascript"))
	if err != nil {
		t.Fatal(err)
	}
	if got := callTool(t, code, code.Name(), `{"code":"print(1)","language":"python"}`); got != "py:python -c 'print(1)'\n[exit code: 1]" {
		t.Fatalf("python = %q", got)
	}
	if got := callTool(t, code, code.Name(), `{"code":"log(2)","language":"javascript"}`); got != "node:log(2)" {
		t.Fatalf("javascript (upload, node, delete) = %q", got)
	}
	if len(f.files) != 0 {
		t.Fatalf("snippet file left behind: %v", f.files)
	}
	if len(f.Find("DELETE /toolbox/sb-1/files")) != 1 {
		t.Fatalf("temp file deletion: %v", f.Calls())
	}
	if n := len(f.Find("POST /sandbox")); n != 1 {
		t.Fatalf("sandboxes created = %d, want 1 shared", n)
	}

	guarded, err := toolpacks.NewShellTool(toolpacks.WithShellSandbox(env), toolpacks.WithShellAllowed("ls"), toolpacks.WithShellIgnore(".env"))
	if err != nil {
		t.Fatal(err)
	}
	if got := callTool(t, guarded, guarded.Name(), `{"command":"ls"}`); got != "listing" {
		t.Fatalf("guarded ls = %q", got)
	}
	before := len(f.Find("POST /toolbox/sb-1/process/execute"))
	for _, cmd := range []string{"ls .env", "ls ../x"} {
		if got := callTool(t, guarded, guarded.Name(), `{"command":`+fmt.Sprintf("%q", cmd)+`}`); !strings.HasPrefix(got, "Access denied:") {
			t.Fatalf("%q = %q", cmd, got)
		}
	}
	if after := len(f.Find("POST /toolbox/sb-1/process/execute")); after != before {
		t.Fatalf("refused commands reached the sandbox (%d new executes)", after-before)
	}
}

func sameJSON(t *testing.T, got []byte, want any) bool {
	t.Helper()
	wb, _ := json.Marshal(want)
	var g, w any
	_ = json.Unmarshal(got, &g)
	_ = json.Unmarshal(wb, &w)
	return fmt.Sprint(g) == fmt.Sprint(w)
}

func TestAPIErrorShapeAndNotFound(t *testing.T) {
	if !errors.Is(&APIError{Status: 404}, ErrNotFound) || errors.Is(&APIError{Status: 500}, ErrNotFound) {
		t.Fatal("only a 404 APIError matches ErrNotFound")
	}
	if errors.Is(&APIError{Status: 404}, os.ErrNotExist) {
		t.Fatal("an APIError must not match unrelated sentinels")
	}
	f := newFakeDaytona(t)
	f.forced = func(c call) (int, string) { return 500, "kaboom" }
	cl := &client{cfg: Config{APIKey: "k", APIURL: f.srv.URL, HTTPClient: &http.Client{}}}
	var apiErr *APIError
	err := cl.jsonCall(context.Background(), http.MethodGet, f.srv.URL+"/x", nil, nil)
	if !errors.As(err, &apiErr) || apiErr.Status != 500 || apiErr.Message != "kaboom" {
		t.Fatalf("API error = %#v", err)
	}
}

// TestCreateErrorsThatAreNotTimeouts: a failing create call is reported as
// itself, not as a start timeout; a failure of the workdir preparation or of a
// file upload surfaces too.
func TestCreateErrorsThatAreNotTimeouts(t *testing.T) {
	f := newFakeDaytona(t)
	f.forced = func(c call) (int, string) {
		if c.Method == http.MethodPost && c.Path == "/sandbox" {
			return 500, "no capacity"
		}
		return 0, ""
	}
	_, _, err := newEnv(t, f).Open(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "no capacity") || strings.Contains(err.Error(), "did not start") {
		t.Fatalf("create failure = %v", err)
	}
	g := newFakeDaytona(t)
	g.forced = func(c call) (int, string) {
		if strings.HasSuffix(c.Path, "files/folder") {
			return 500, "disk full"
		}
		return 0, ""
	}
	if _, _, err := newEnv(t, g).Open(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "prepare workdir") {
		t.Fatalf("workdir failure = %v", err)
	}
	if len(g.Find("DELETE /sandbox/sb-1")) != 1 {
		t.Fatalf("a sandbox without a usable workdir must be deleted: %v", g.Calls())
	}
	h := newFakeDaytona(t)
	sb, _, err := newEnv(t, h).Open(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	h.forced = func(c call) (int, string) {
		if strings.HasSuffix(c.Path, "files/upload-v2") {
			return 500, "upload broke"
		}
		return 0, ""
	}
	if err := sb.PutFile(context.Background(), "a.txt", []byte("x")); err == nil || !strings.Contains(err.Error(), "upload broke") {
		t.Fatalf("PutFile with a failing upload = %v", err)
	}
}

// TestProxyURLKeptAcrossPolls: a toolboxProxyUrl given by the create response
// survives poll responses that omit it, so no lookup is needed.
func TestProxyURLKeptAcrossPolls(t *testing.T) {
	f := newFakeDaytona(t)
	f.states = []string{"creating", "started"}
	f.omitProxyOnGet = true
	sb, _, err := newEnv(t, f).Open(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Find("GET /sandbox/sb-1/toolbox-proxy-url")) != 0 {
		t.Fatalf("lookup although the create response had the URL: %v", f.Calls())
	}
	if _, err := sb.Exec(context.Background(), []string{"ls"}, toolpacks.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(f.Find("POST /toolbox/sb-1/process/execute")) != 1 {
		t.Fatalf("exec did not use the kept proxy URL: %v", f.Calls())
	}
}

func TestResourcesAndTimeoutBoundaries(t *testing.T) {
	f := newFakeDaytona(t)
	env := newEnv(t, f, WithImage("img:1"), WithResources(Resources{CPU: 2}))
	sb, _, err := env.Open(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	body := f.Find("POST /sandbox")[0].JSON(t)
	if body["cpu"] != float64(2) {
		t.Fatalf("cpu = %v", body["cpu"])
	}
	for _, k := range []string{"memory", "disk"} {
		if _, ok := body[k]; ok {
			t.Fatalf("%s must be absent when unset: %v", k, body)
		}
	}
	// A sub-second timeout still reaches the API as one second.
	if _, err := sb.Exec(context.Background(), []string{"x"}, toolpacks.ExecOptions{Timeout: 100 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if got := f.Find("POST /toolbox/sb-1/process/execute")[0].JSON(t)["timeout"]; got != float64(1) {
		t.Fatalf("timeout = %v, want 1", got)
	}
	// Output exactly at the cap is not truncated; one more character is.
	capped := newEnv(t, f, WithMaxOutput(20))
	csb, _, _ := capped.Open(context.Background(), nil)
	f.run = func(string, map[string]any) (int, string, int) { return 200, strings.Repeat("q", 20), 0 }
	if res, _ := csb.Exec(context.Background(), []string{"x"}, toolpacks.ExecOptions{}); res.Output != strings.Repeat("q", 20) {
		t.Fatalf("output at the cap = %q", res.Output)
	}
	f.run = func(string, map[string]any) (int, string, int) { return 200, strings.Repeat("q", 21), 0 }
	if res, _ := csb.Exec(context.Background(), []string{"x"}, toolpacks.ExecOptions{}); !strings.Contains(res.Output, "[truncated: showing first 20 of 21 chars]") {
		t.Fatalf("output over the cap = %q", res.Output)
	}
	// Timeouts: exactly one second is valid; an API message of any shape is shown.
	if _, err := NewEnvironment(WithAPIKey("k"), WithTimeout(time.Second)); err != nil {
		t.Fatalf("1s timeout: %v", err)
	}
	g := newFakeDaytona(t)
	g.forced = func(c call) (int, string) { return 400, "bad request" }
	cl := &client{cfg: Config{APIKey: "k", HTTPClient: &http.Client{}}}
	var apiErr *APIError
	if err := cl.jsonCall(context.Background(), http.MethodGet, g.srv.URL+"/x", nil, nil); !errors.As(err, &apiErr) || apiErr.Message != "bad request" {
		t.Fatalf("message unwrapping: %v", err)
	}
}

// TestVariableBoundConstruction: a Variable stands in for the static value at
// construction time — the key, and the image or snapshot — while a plain
// missing key still fails.
func TestVariableBoundConstruction(t *testing.T) {
	t.Setenv("DAYTONA_API_KEY", "")
	if _, err := NewEnvironment(WithVariable("api_key", tools.NewVariable("k"))); err != nil {
		t.Fatalf("key supplied by a Variable: %v", err)
	}
	if _, err := NewEnvironment(WithAPIKey("")); err == nil {
		t.Fatal("no key and no Variable must fail")
	}
	// Both statics set is invalid... unless one of them is replaced by a Variable.
	if _, err := NewEnvironment(WithAPIKey("k"), WithSnapshot("s"), WithImage("i")); err == nil {
		t.Fatal("snapshot and image together must fail")
	}
	if _, err := NewEnvironment(WithAPIKey("k"), WithSnapshot("s"), WithImage("i"), WithVariable("image", tools.NewVariable("i2", "x"))); err != nil {
		t.Fatalf("a Variable for the image defers the check to Open: %v", err)
	}
	if _, err := NewEnvironment(WithAPIKey("k"), WithSnapshot("s"), WithImage("i"), WithVariable("snapshot", tools.NewVariable("s2", "x"))); err != nil {
		t.Fatalf("a Variable for the snapshot defers the check to Open: %v", err)
	}
}

func TestShellWordCharacterClasses(t *testing.T) {
	for _, bare := range []string{"a", "z", "A", "Z", "0", "9", "az-AZ_09"} {
		if got := shellWord(bare); got != bare {
			t.Errorf("shellWord(%q) = %q, want it bare", bare, got)
		}
	}
	for _, quoted := range []string{"`", "{", "[", "a b", "~", "!"} {
		if got := shellWord(quoted); got != "'"+quoted+"'" {
			t.Errorf("shellWord(%q) = %q, want it quoted", quoted, got)
		}
	}
}
