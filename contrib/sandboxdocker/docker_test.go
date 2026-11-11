package sandboxdocker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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

// call is one recorded Engine API request.
type call struct {
	Method string
	Path   string
	Query  string
	Body   []byte
}

func (c call) String() string {
	q := ""
	if c.Query != "" {
		q = "?" + c.Query
	}
	return c.Method + " " + c.Path + q
}

func (c call) JSON(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(c.Body, &m); err != nil {
		t.Fatalf("%s: body is not JSON: %v (%q)", c, err, c.Body)
	}
	return m
}

// fakeDocker speaks enough of the Docker Engine API for the sandbox: container
// create/start/stop/remove, image pull, exec create/start/inspect, archive
// put/get. Exec results come from the scripted run func.
type fakeDocker struct {
	t      *testing.T
	sock   string
	mu     sync.Mutex
	calls  []call
	nextID int
	execs  map[string][]string // exec id -> Cmd
	files  map[string][]byte   // absolute container path -> content
	// missingImages are answered with 404 on create until pulled.
	missingImages map[string]bool
	pullError     string
	// run answers an exec: stdout, stderr, exit code. block makes it wait for
	// the request context (a command that never finishes).
	run   func(cmd []string) (stdout, stderr string, code int, block bool)
	apiFn func(c call) (status int, msg string) // optional forced API error
}

func newFakeDocker(t *testing.T) *fakeDocker {
	t.Helper()
	dir, err := os.MkdirTemp("", "dk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &fakeDocker{
		t: t, sock: filepath.Join(dir, "d.sock"),
		execs: map[string][]string{}, files: map[string][]byte{}, missingImages: map[string]bool{},
		run: func(cmd []string) (string, string, int, bool) { return "", "", 0, false },
	}
	l, err := net.Listen("unix", f.sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(f.handle))
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	return f
}

func (f *fakeDocker) host() string { return "unix://" + f.sock }

func (f *fakeDocker) record(r *http.Request) call {
	b, _ := io.ReadAll(r.Body)
	c := call{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: b}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	return c
}

func (f *fakeDocker) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.String()
	}
	return out
}

// Find returns the recorded calls whose "METHOD path" starts with prefix.
func (f *fakeDocker) Find(prefix string) []call {
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

func (f *fakeDocker) handle(w http.ResponseWriter, r *http.Request) {
	c := f.record(r)
	if f.apiFn != nil {
		if status, msg := f.apiFn(c); status != 0 {
			writeJSON(w, status, map[string]string{"message": msg})
			return
		}
	}
	p := r.URL.Path
	switch {
	case r.Method == http.MethodPost && p == "/containers/create":
		var req createRequest
		_ = json.Unmarshal(c.Body, &req)
		f.mu.Lock()
		missing := f.missingImages[req.Image]
		f.nextID++
		id := fmt.Sprintf("c%d", f.nextID)
		f.mu.Unlock()
		if missing {
			writeJSON(w, 404, map[string]string{"message": "No such image: " + req.Image})
			return
		}
		writeJSON(w, 201, map[string]string{"Id": id})
	case r.Method == http.MethodPost && p == "/images/create":
		img := r.URL.Query().Get("fromImage") + ":" + r.URL.Query().Get("tag")
		f.mu.Lock()
		delete(f.missingImages, img)
		delete(f.missingImages, r.URL.Query().Get("fromImage"))
		f.mu.Unlock()
		w.WriteHeader(200)
		fmt.Fprintln(w, `{"status":"Pulling"}`)
		if f.pullError != "" {
			fmt.Fprintf(w, `{"error":%q}`+"\n", f.pullError)
		}
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/start") && strings.HasPrefix(p, "/containers/"):
		w.WriteHeader(204)
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/stop"):
		w.WriteHeader(204)
	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/containers/"):
		w.WriteHeader(204)
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/exec"):
		var req execRequest
		_ = json.Unmarshal(c.Body, &req)
		f.mu.Lock()
		f.nextID++
		id := fmt.Sprintf("e%d", f.nextID)
		f.execs[id] = req.Cmd
		f.mu.Unlock()
		writeJSON(w, 201, map[string]string{"Id": id})
	case r.Method == http.MethodPost && strings.HasPrefix(p, "/exec/") && strings.HasSuffix(p, "/start"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/exec/"), "/start")
		f.mu.Lock()
		cmd := f.execs[id]
		f.mu.Unlock()
		stdout, stderr, code, block := f.run(cmd)
		if block {
			<-r.Context().Done()
			return
		}
		f.mu.Lock()
		f.execs[id+"/code"] = []string{fmt.Sprint(code)}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		w.WriteHeader(200)
		if stdout != "" {
			w.Write(frame(1, stdout))
		}
		if stderr != "" {
			w.Write(frame(2, stderr))
		}
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/exec/") && strings.HasSuffix(p, "/json"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/exec/"), "/json")
		f.mu.Lock()
		code := "0"
		if v := f.execs[id+"/code"]; len(v) > 0 {
			code = v[0]
		}
		f.mu.Unlock()
		fmt.Fprintf(w, `{"ExitCode":%s,"Running":false}`, code)
	case r.Method == http.MethodPut && strings.HasSuffix(p, "/archive"):
		dir := r.URL.Query().Get("path")
		tr := tar.NewReader(bytes.NewReader(c.Body))
		for {
			h, err := tr.Next()
			if err != nil {
				break
			}
			if h.Typeflag == tar.TypeReg {
				b, _ := io.ReadAll(tr)
				f.mu.Lock()
				f.files[dir+"/"+h.Name] = b
				f.mu.Unlock()
			}
		}
		w.WriteHeader(200)
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/archive"):
		target := r.URL.Query().Get("path")
		f.mu.Lock()
		content, ok := f.files[target]
		f.mu.Unlock()
		if !ok {
			writeJSON(w, 404, map[string]string{"message": "Could not find the file " + target})
			return
		}
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		_ = tw.WriteHeader(&tar.Header{Name: filepath.Base(target), Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg})
		_, _ = tw.Write(content)
		_ = tw.Close()
		w.Write(buf.Bytes())
	default:
		writeJSON(w, 404, map[string]string{"message": "unhandled " + r.Method + " " + p})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// frame builds one multiplexed stream frame.
func frame(stream byte, payload string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(payload)))
	return append(h, payload...)
}

func newEnv(t *testing.T, f *fakeDocker, opts ...Option) *Environment {
	t.Helper()
	env, err := NewEnvironment(append([]Option{WithHost(f.host())}, opts...)...)
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	t.Cleanup(func() { env.Close() })
	return env
}

func TestContainerCreateBodyAndLifecycle(t *testing.T) {
	f := newFakeDocker(t)
	host := t.TempDir()
	env := newEnv(t, f,
		WithImage("img:1"), WithEnvVars(map[string]string{"B": "2", "A": "1"}),
		WithNetworkMode("bridge"), WithMemLimit("256m"), WithCPUQuota(50000), WithUser("nobody"),
		WithAutoRemove(false), WithHostPath(host), WithWorkdir("/work"))
	ctx := context.Background()
	sb, release, err := env.Open(ctx, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	creates := f.Find("POST /containers/create")
	if len(creates) != 1 {
		t.Fatalf("creates = %d, want 1", len(creates))
	}
	body := creates[0].JSON(t)
	wantHC := map[string]any{
		"NetworkMode": "bridge", "Memory": float64(256 << 20), "CpuQuota": float64(50000),
		"AutoRemove": false, "Binds": []any{host + ":/work:rw"},
	}
	if got, _ := json.Marshal(body["HostConfig"]); !jsonEqual(t, got, wantHC) {
		t.Fatalf("HostConfig = %s, want %v", got, wantHC)
	}
	if body["Image"] != "img:1" || body["WorkingDir"] != "/work" || body["User"] != "nobody" {
		t.Fatalf("create body = %v", body)
	}
	if got, _ := json.Marshal(body["Env"]); string(got) != `["A=1","B=2"]` {
		t.Fatalf("Env = %s, want sorted KEY=VALUE", got)
	}
	if got := fmt.Sprint(body["Cmd"]); got != "[sh -c mkdir -p '/work' && sleep infinity]" {
		t.Fatalf("Cmd = %s", got)
	}
	if sb.Workdir() != "/work" || sb.HostWorkdir() != host {
		t.Fatalf("Workdir=%q HostWorkdir=%q", sb.Workdir(), sb.HostWorkdir())
	}
	if env.Workdir() != "/work" {
		t.Fatalf("Environment.Workdir = %q", env.Workdir())
	}
	if len(f.Find("POST /containers/c1/start")) != 1 {
		t.Fatalf("container was not started: %v", f.Calls())
	}

	if err := env.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	stops := f.Find("POST /containers/c1/stop")
	if len(stops) != 1 || stops[0].Query != "t=1" {
		t.Fatalf("stop calls = %v, want one with t=1", stops)
	}
	if rm := f.Find("DELETE /containers/c1"); len(rm) != 1 || rm[0].Query != "force=true" {
		t.Fatalf("remove calls = %v, want one force=true (auto_remove off)", rm)
	}
	// Idempotent: a second Close sends nothing more.
	n := len(f.Calls())
	if err := env.Close(); err != nil || len(f.Calls()) != n {
		t.Fatalf("second Close: %v, calls %d -> %d", err, n, len(f.Calls()))
	}
}

func TestAutoRemoveSkipsExplicitRemove(t *testing.T) {
	f := newFakeDocker(t)
	env := newEnv(t, f) // defaults: auto_remove on
	if _, _, err := env.Open(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	body := f.Find("POST /containers/create")[0].JSON(t)
	hc := body["HostConfig"].(map[string]any)
	if hc["AutoRemove"] != true || hc["NetworkMode"] != "none" || hc["Memory"] != float64(512<<20) {
		t.Fatalf("default HostConfig = %v, want AutoRemove, network none, 512m", hc)
	}
	if body["Image"] != "python:3.12-slim" || body["WorkingDir"] != "/workspace" {
		t.Fatalf("default body = %v", body)
	}
	if _, ok := hc["Binds"]; ok {
		t.Fatalf("no host path: Binds must be absent, got %v", hc["Binds"])
	}
	env.Close()
	if len(f.Find("POST /containers/c1/stop")) != 1 || len(f.Find("DELETE ")) != 0 {
		t.Fatalf("with auto_remove the daemon removes: %v", f.Calls())
	}
}

func TestImagePulledWhenMissing(t *testing.T) {
	f := newFakeDocker(t)
	f.missingImages["reg.example/team/img:v2"] = true
	env := newEnv(t, f, WithImage("reg.example/team/img:v2"))
	if _, _, err := env.Open(context.Background(), nil); err != nil {
		t.Fatalf("Open with a missing image: %v", err)
	}
	pulls := f.Find("POST /images/create")
	if len(pulls) != 1 || pulls[0].Query != "fromImage=reg.example%2Fteam%2Fimg&tag=v2" {
		t.Fatalf("pull calls = %v", pulls)
	}
	if n := len(f.Find("POST /containers/create")); n != 2 {
		t.Fatalf("create attempts = %d, want 2 (before and after the pull)", n)
	}
	// A pull error in the stream fails the open.
	f2 := newFakeDocker(t)
	f2.missingImages["bad:latest"] = true
	f2.pullError = "pull access denied"
	env2 := newEnv(t, f2, WithImage("bad:latest"))
	if _, _, err := env2.Open(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "pull access denied") {
		t.Fatalf("Open with a failing pull = %v", err)
	}
}

func TestExecRequestsAndOutput(t *testing.T) {
	f := newFakeDocker(t)
	f.run = func(cmd []string) (string, string, int, bool) {
		return "out-line\n", "err-line\n", 3, false
	}
	env := newEnv(t, f, WithWorkdir("/w"))
	sb, _, err := env.Open(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := sb.Exec(context.Background(), []string{"prog", "a b"}, toolpacks.ExecOptions{Env: map[string]string{"Z": "9", "Y": "8"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "out-line\nerr-line" || res.ExitCode != 3 {
		t.Fatalf("result = %+v, want stdout then stderr, trimmed, exit 3", res)
	}
	ex := f.Find("POST /containers/c1/exec")[0].JSON(t)
	if got, _ := json.Marshal(ex); !jsonEqual(t, got, map[string]any{
		"AttachStdout": true, "AttachStderr": true, "Cmd": []any{"prog", "a b"},
		"WorkingDir": "/w", "Env": []any{"Y=8", "Z=9"},
	}) {
		t.Fatalf("exec body = %s", got)
	}
	start := f.Find("POST /exec/e")[0].JSON(t)
	if start["Detach"] != false || start["Tty"] != false {
		t.Fatalf("exec start body = %v", start)
	}
	if len(f.Find("GET /exec/")) != 1 {
		t.Fatalf("exit code not inspected: %v", f.Calls())
	}
	if res, err := sb.Exec(context.Background(), nil, toolpacks.ExecOptions{}); err != nil || res.ExitCode != 2 {
		t.Fatalf("empty argv = %+v, %v, want exit 2", res, err)
	}
}

func TestExecOutputLimits(t *testing.T) {
	f := newFakeDocker(t)
	f.run = func([]string) (string, string, int, bool) { return strings.Repeat("x", 500), "", 0, false }
	env := newEnv(t, f, WithMaxOutput(100))
	sb, _, _ := env.Open(context.Background(), nil)
	res, err := sb.Exec(context.Background(), []string{"x"}, toolpacks.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// 500 bytes arrive; 2x100 are kept, 300 counted as dropped; the rendered text
	// (200 + the 69-char buffer notice) is then cut to 100 characters.
	if want := strings.Repeat("x", 100) + "\n[truncated: showing first 100 of 269 chars]"; res.Output != want {
		t.Fatalf("output = %q, want %q", res.Output, want)
	}
}

func TestReadMultiplexed(t *testing.T) {
	stream := append(frame(1, "ab"), frame(2, "cd")...)
	stream = append(stream, frame(1, "efgh")...)
	got, dropped, err := readMultiplexed(bytes.NewReader(stream), 5)
	if err != nil || string(got) != "abcde" || dropped != 3 {
		t.Fatalf("readMultiplexed = %q, %d, %v; want abcde, 3", got, dropped, err)
	}
	if _, _, err := readMultiplexed(bytes.NewReader(stream[:5]), 100); err == nil {
		t.Fatal("a truncated header must be an error")
	}
	if _, _, err := readMultiplexed(bytes.NewReader(stream[:9]), 100); err == nil {
		t.Fatal("a truncated payload must be an error")
	}
}

func TestExecTimeoutRestartsContainer(t *testing.T) {
	f := newFakeDocker(t)
	var n int
	f.run = func(cmd []string) (string, string, int, bool) {
		n++
		if n == 1 {
			return "", "", 0, true // never finishes
		}
		return "second", "", 0, false
	}
	env := newEnv(t, f, WithAutoRemove(false))
	sb, _, _ := env.Open(context.Background(), nil)
	res, err := sb.Exec(context.Background(), []string{"sleep", "9"}, toolpacks.ExecOptions{Timeout: 150 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 124 || res.Output != "Execution timed out after 0.15s" {
		t.Fatalf("timeout result = %+v", res)
	}
	if len(f.Find("POST /containers/c1/stop")) != 1 || len(f.Find("DELETE /containers/c1")) != 1 {
		t.Fatalf("the hung container must be stopped and removed: %v", f.Calls())
	}
	// The next call runs in a fresh container.
	res, err = sb.Exec(context.Background(), []string{"echo"}, toolpacks.ExecOptions{})
	if err != nil || res.Output != "second" {
		t.Fatalf("after restart = %+v, %v", res, err)
	}
	if n := len(f.Find("POST /containers/create")); n != 2 {
		t.Fatalf("containers created = %d, want 2", n)
	}
	// A canceled caller context is an error, not a timeout result, and no restart.
	f.run = func([]string) (string, string, int, bool) { return "", "", 0, true }
	cctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if _, err := sb.Exec(cctx, []string{"x"}, toolpacks.ExecOptions{Timeout: 30 * time.Second}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled exec = %v, want context.Canceled", err)
	}
	if n := len(f.Find("POST /containers/create")); n != 2 {
		t.Fatalf("a canceled call must not restart the container (creates=%d)", n)
	}
}

func TestAPIErrorIsAResult(t *testing.T) {
	f := newFakeDocker(t)
	env := newEnv(t, f)
	sb, _, _ := env.Open(context.Background(), nil)
	f.apiFn = func(c call) (int, string) {
		if strings.HasSuffix(c.Path, "/exec") {
			return 500, "daemon exploded"
		}
		return 0, ""
	}
	res, err := sb.Exec(context.Background(), []string{"x"}, toolpacks.ExecOptions{})
	if err != nil || res.ExitCode != 1 || !strings.Contains(res.Output, "Docker API error") || !strings.Contains(res.Output, "daemon exploded") {
		t.Fatalf("API error = %+v, %v", res, err)
	}
	// A dead daemon is an error.
	bad, _ := NewSandbox(func() Config { c := DefaultConfig(); c.Host = "unix:///nonexistent/docker.sock"; return c }())
	if _, err := bad.Exec(context.Background(), []string{"x"}, toolpacks.ExecOptions{}); err == nil {
		t.Fatal("an unreachable daemon must be an error")
	}
}

func TestPutGetFileThroughArchiveAPI(t *testing.T) {
	f := newFakeDocker(t)
	env := newEnv(t, f)
	sb, _, _ := env.Open(context.Background(), nil)
	ctx := context.Background()
	if err := sb.PutFile(ctx, "sub/dir/a.txt", []byte("payload")); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	puts := f.Find("PUT /containers/c1/archive")
	if len(puts) != 1 || puts[0].Query != "path=%2Fworkspace" {
		t.Fatalf("put calls = %v", puts)
	}
	tr := tar.NewReader(bytes.NewReader(puts[0].Body))
	var names []string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
	}
	if strings.Join(names, ",") != "sub/,sub/dir/,sub/dir/a.txt" {
		t.Fatalf("archive entries = %v, want parent dirs then the file", names)
	}
	if string(f.files["/workspace/sub/dir/a.txt"]) != "payload" {
		t.Fatalf("daemon stored %v", f.files)
	}
	got, err := sb.GetFile(ctx, "sub/dir/a.txt")
	if err != nil || string(got) != "payload" {
		t.Fatalf("GetFile = %q, %v", got, err)
	}
	if g := f.Find("GET /containers/c1/archive"); len(g) != 1 || g[0].Query != "path=%2Fworkspace%2Fsub%2Fdir%2Fa.txt" {
		t.Fatalf("get calls = %v", g)
	}
	if _, err := sb.GetFile(ctx, "missing.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("GetFile of a missing file = %v, want os.ErrNotExist", err)
	}
	for _, bad := range []string{"", "/etc/passwd", "../x", "a/../../x", "."} {
		if err := sb.PutFile(ctx, bad, nil); !errors.Is(err, toolpacks.ErrSandboxPath) {
			t.Errorf("PutFile(%q) = %v, want ErrSandboxPath", bad, err)
		}
		if _, err := sb.GetFile(ctx, bad); !errors.Is(err, toolpacks.ErrSandboxPath) {
			t.Errorf("GetFile(%q) = %v, want ErrSandboxPath", bad, err)
		}
	}
	// RemoveFile is `rm -f` in the container.
	if err := sb.(toolpacks.FileRemover).RemoveFile(ctx, "sub/dir/a.txt"); err != nil {
		t.Fatal(err)
	}
	var rm []string
	f.mu.Lock()
	for _, cmd := range f.execs {
		if len(cmd) > 0 && cmd[0] == "rm" {
			rm = cmd
		}
	}
	f.mu.Unlock()
	if strings.Join(rm, " ") != "rm -f /workspace/sub/dir/a.txt" {
		t.Fatalf("remove exec = %v (execs %v)", rm, f.execs)
	}
}

func TestEnvironmentCachesByResolvedParameters(t *testing.T) {
	f := newFakeDocker(t)
	env := newEnv(t, f,
		WithImageVariable(tools.NewVariable("tenant_image", "default:1")),
		WithEnvVarsVariable(tools.NewVariable("tenant_env", map[string]string{"D": "1"})),
		WithNetworkModeVariable(tools.NewVariable("net", "none")))
	ctx := context.Background()
	cxA := agocontext.New(stream.NewMemoryStream())
	_ = cxA.SetVariable("tenant_image", "tenant-a:1")
	_ = cxA.SetVariable("tenant_env", map[string]any{"TENANT": "a"})
	cxB := agocontext.New(stream.NewMemoryStream())
	_ = cxB.SetVariable("tenant_image", "tenant-b:1")
	cxD := agocontext.New(stream.NewMemoryStream()) // nothing set: the defaults

	open := func(cx *agocontext.Context) toolpacks.Sandbox {
		t.Helper()
		sb, _, err := env.Open(ctx, cx)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return sb
	}
	a1, a2, b, d := open(cxA), open(cxA), open(cxB), open(cxD)
	if a1 != a2 {
		t.Fatal("the same resolved values must reuse one sandbox")
	}
	if a1 == b || a1 == d || b == d {
		t.Fatal("distinct resolved values must get distinct sandboxes")
	}
	creates := f.Find("POST /containers/create")
	if len(creates) != 3 {
		t.Fatalf("containers created = %d, want 3 (a, b, defaults)", len(creates))
	}
	first := creates[0].JSON(t)
	if first["Image"] != "tenant-a:1" || fmt.Sprint(first["Env"]) != "[TENANT=a]" {
		t.Fatalf("tenant a body = %v", first)
	}
	if imgs := []any{creates[1].JSON(t)["Image"], creates[2].JSON(t)["Image"]}; fmt.Sprint(imgs) != "[tenant-b:1 default:1]" {
		t.Fatalf("images = %v", imgs)
	}

	// No context: a Variable cannot be resolved, as in Python.
	if _, _, err := env.Open(ctx, nil); err == nil || !strings.Contains(err.Error(), "no Context") {
		t.Fatalf("Open(nil) with Variables = %v", err)
	}
	// A required variable without a default.
	strict := newEnv(t, f, WithImageVariable(tools.NewVariable("must_have")))
	if _, _, err := strict.Open(ctx, cxD); !errors.Is(err, tools.ErrNotFound) {
		t.Fatalf("missing variable = %v, want tools.ErrNotFound", err)
	}
	// A mistyped value.
	_ = cxD.SetVariable("tenant_image", 42)
	if _, _, err := env.Open(ctx, cxD); err == nil {
		t.Fatal("a non-string image variable must fail")
	}

	// Close tears down all three and a closed sandbox is replaced on next Open.
	if err := env.Close(); err != nil {
		t.Fatal(err)
	}
	if n := len(f.Find("POST /containers/c1/stop")) + len(f.Find("POST /containers/c2/stop")) + len(f.Find("POST /containers/c3/stop")); n != 3 {
		t.Fatalf("stopped %d containers, want 3", n)
	}
	if _, err := a1.Exec(ctx, []string{"x"}, toolpacks.ExecOptions{}); !errors.Is(err, toolpacks.ErrSandboxClosed) {
		t.Fatalf("Exec on a closed sandbox = %v, want ErrSandboxClosed", err)
	}
	if a3 := open(cxA); a3 == a1 {
		t.Fatal("Open after Close must build a fresh sandbox")
	}
}

func TestConcurrentOpenCreatesOneContainer(t *testing.T) {
	f := newFakeDocker(t)
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
	if n := len(f.Find("POST /containers/create")); n != 1 {
		t.Fatalf("containers created = %d, want exactly 1", n)
	}
}

func TestConfigValidation(t *testing.T) {
	for name, opt := range map[string]Option{
		"timeout < 1s":   WithTimeout(500 * time.Millisecond),
		"empty image":    WithImage(""),
		"relative wd":    WithWorkdir("work"),
		"bad mem":        WithMemLimit("lots"),
		"zero maxoutput": WithMaxOutput(0),
		"bad host":       WithHost("ftp://x"),
	} {
		if _, err := NewEnvironment(opt); err == nil {
			t.Errorf("%s: want a constructor error", name)
		}
	}
	for in, want := range map[string]int64{"1024": 1024, "512m": 512 << 20, "2G": 2 << 30, "64kb": 64 << 10, "1t": 1 << 40, "7b": 7} {
		if got, err := parseBytes(in); err != nil || got != want {
			t.Errorf("parseBytes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := newEngine("tcp://127.0.0.1:2375"); err != nil {
		t.Errorf("tcp host: %v", err)
	}
	t.Setenv("DOCKER_HOST", "unix:///x/y.sock")
	if e, err := newEngine(""); err != nil || e.base != "http://docker" {
		t.Errorf("DOCKER_HOST fallback: %v", err)
	}
}

// --- end to end through the toolpacks tools -----------------------------------

func toolCtx() *agocontext.Context { return agocontext.New(stream.NewMemoryStream()) }

// callTool drives one tool call through the real stream machinery.
func callTool(t *testing.T, tool tools.Tool, name, args string) string {
	t.Helper()
	cx := toolCtx()
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

func TestShellAndCodeRunThroughDocker(t *testing.T) {
	f := newFakeDocker(t)
	f.run = func(cmd []string) (string, string, int, bool) {
		switch {
		case len(cmd) == 3 && cmd[0] == "sh" && cmd[1] == "-c":
			return "shell:" + cmd[2], "", 0, false
		case len(cmd) == 3 && cmd[0] == "python" && cmd[1] == "-c":
			return "py:" + cmd[2] + "\n", "warn", 1, false
		case len(cmd) == 2 && cmd[0] == "node":
			return "node:" + string(f.files[cmd[1]]), "", 0, false
		case len(cmd) == 1 && cmd[0] == "ls":
			return "listing", "", 0, false
		}
		return "", "", 0, false
	}
	env := newEnv(t, f)

	shell, err := toolpacks.NewShellTool(toolpacks.WithShellSandbox(env))
	if err != nil {
		t.Fatal(err)
	}
	if got := callTool(t, shell, shell.Name(), `{"command":"echo hi"}`); got != "shell:echo hi" {
		t.Fatalf("shell = %q", got)
	}

	code, err := toolpacks.NewSandboxCodeToolFromFactory(env, toolpacks.WithCodeLanguages("python", "javascript"))
	if err != nil {
		t.Fatal(err)
	}
	if got := callTool(t, code, code.Name(), `{"code":"print(1)","language":"python"}`); got != "py:print(1)\nwarn\n[exit code: 1]" {
		t.Fatalf("python = %q", got)
	}
	if got := callTool(t, code, code.Name(), `{"code":"log(2)","language":"javascript"}`); got != "node:log(2)" {
		t.Fatalf("javascript (file mode: put archive, node, rm) = %q", got)
	}
	var sawRemove bool
	f.mu.Lock()
	for _, cmd := range f.execs {
		if len(cmd) == 3 && cmd[0] == "rm" && cmd[1] == "-f" && strings.HasPrefix(cmd[2], "/workspace/ago_") {
			sawRemove = true
		}
	}
	f.mu.Unlock()
	if !sawRemove {
		t.Fatal("the temp snippet file was not removed")
	}
	// One container served all three tools.
	if n := len(f.Find("POST /containers/create")); n != 1 {
		t.Fatalf("containers created = %d, want 1 (shared through the environment)", n)
	}

	// The restricted shell with the ignore guard over the container's POSIX
	// workdir: refused paths never reach the daemon.
	guarded, err := toolpacks.NewShellTool(toolpacks.WithShellSandbox(env), toolpacks.WithShellAllowed("ls"), toolpacks.WithShellIgnore(".env"))
	if err != nil {
		t.Fatal(err)
	}
	before := len(f.Find("POST /containers/c1/exec"))
	if got := callTool(t, guarded, guarded.Name(), `{"command":"ls"}`); got != "listing" {
		t.Fatalf("guarded ls = %q", got)
	}
	for _, cmd := range []string{"ls .env", "ls ../x", "ls /etc/passwd"} {
		if got := callTool(t, guarded, guarded.Name(), `{"command":`+fmt.Sprintf("%q", cmd)+`}`); !strings.HasPrefix(got, "Access denied:") {
			t.Fatalf("%q = %q, want Access denied", cmd, got)
		}
	}
	if after := len(f.Find("POST /containers/c1/exec")); after != before+1 {
		t.Fatalf("exec calls grew by %d, want 1 (refused commands never reach the container)", after-before)
	}
	if err := shell.Close(); err != nil {
		t.Fatal(err)
	}
}

func jsonEqual(t *testing.T, got []byte, want any) bool {
	t.Helper()
	wb, _ := json.Marshal(want)
	var g, w any
	_ = json.Unmarshal(got, &g)
	_ = json.Unmarshal(wb, &w)
	return fmt.Sprint(g) == fmt.Sprint(w)
}

// TestStartNotModifiedIsFine: a 304 on start means the container already runs
// (the daemon's answer to a repeated start); any other start failure aborts.
func TestStartNotModifiedIsFine(t *testing.T) {
	f := newFakeDocker(t)
	f.apiFn = func(c call) (int, string) {
		if strings.HasSuffix(c.Path, "/start") {
			return http.StatusNotModified, ""
		}
		return 0, ""
	}
	env := newEnv(t, f)
	if _, _, err := env.Open(context.Background(), nil); err != nil {
		t.Fatalf("Open with a 304 on start: %v", err)
	}
	f2 := newFakeDocker(t)
	f2.apiFn = func(c call) (int, string) {
		if strings.HasSuffix(c.Path, "/start") {
			return 500, "cannot start"
		}
		return 0, ""
	}
	env2 := newEnv(t, f2)
	if _, _, err := env2.Open(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "cannot start") {
		t.Fatalf("Open with a failing start = %v", err)
	}
}

// TestPullSplitsTheTagAtTheLastColon: a registry port is not a tag.
func TestPullSplitsTheTagAtTheLastColon(t *testing.T) {
	for image, wantQuery := range map[string]string{
		"localhost:5000/team/img":      "fromImage=localhost%3A5000%2Fteam%2Fimg&tag=latest",
		"localhost:5000/team/img:v2":   "fromImage=localhost%3A5000%2Fteam%2Fimg&tag=v2",
		"img":                          "fromImage=img&tag=latest",
		"python:3.12-slim":             "fromImage=python&tag=3.12-slim",
		"reg.example:443/a/b/c:latest": "fromImage=reg.example%3A443%2Fa%2Fb%2Fc&tag=latest",
	} {
		f := newFakeDocker(t)
		f.missingImages[image] = true
		env := newEnv(t, f, WithImage(image))
		if _, _, err := env.Open(context.Background(), nil); err != nil {
			t.Fatalf("%s: Open: %v", image, err)
		}
		pulls := f.Find("POST /images/create")
		if len(pulls) != 1 || pulls[0].Query != wantQuery {
			t.Errorf("%s: pull calls = %v, want query %s", image, pulls, wantQuery)
		}
	}
}

func TestAPIErrorShapeAndNotFound(t *testing.T) {
	if !errors.Is(&APIError{Status: 404}, ErrNotFound) || errors.Is(&APIError{Status: 500}, ErrNotFound) {
		t.Fatal("only a 404 APIError matches ErrNotFound")
	}
	if errors.Is(&APIError{Status: 404}, os.ErrNotExist) {
		t.Fatal("an APIError must not match unrelated sentinels")
	}
	// The daemon's {"message": ...} body is unwrapped; any other body is kept verbatim.
	f := newFakeDocker(t)
	eng, err := newEngine(f.host())
	if err != nil {
		t.Fatal(err)
	}
	f.apiFn = func(c call) (int, string) { return 500, "daemon exploded" }
	err = eng.call(context.Background(), http.MethodPost, "/anything", nil, nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 500 || apiErr.Message != "daemon exploded" {
		t.Fatalf("API error = %#v", err)
	}
	f.apiFn = nil
	if _, err := eng.do(context.Background(), http.MethodGet, "/nope", nil, "", nil); !errors.As(err, &apiErr) ||
		apiErr.Status != 404 || !strings.Contains(apiErr.Message, "unhandled GET /nope") {
		t.Fatalf("404 error = %v", err)
	}
	// A request with a body carries its content type; a query is appended.
	f.run = func([]string) (string, string, int, bool) { return "", "", 0, false }
	if err := eng.call(context.Background(), http.MethodPost, "/containers/x/stop", url.Values{"t": {"1"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.Find("POST /containers/x/stop"); len(got) != 1 || got[0].Query != "t=1" {
		t.Fatalf("stop call = %v", got)
	}
}

func TestIgnoreStatus(t *testing.T) {
	if err := ignoreStatus(&APIError{Status: 404}, 404, 304); err != nil {
		t.Fatalf("404 must be ignored: %v", err)
	}
	if err := ignoreStatus(&APIError{Status: 304}, 404, 304); err != nil {
		t.Fatalf("304 must be ignored: %v", err)
	}
	if err := ignoreStatus(&APIError{Status: 500}, 404, 304); err == nil {
		t.Fatal("500 must not be ignored")
	}
	plain := errors.New("boom")
	if err := ignoreStatus(plain, 404); !errors.Is(err, plain) {
		t.Fatalf("a non-API error passes through, got %v", err)
	}
	if ignoreStatus(nil, 404) != nil {
		t.Fatal("nil stays nil")
	}
}

// TestGetFileSizeLimit: a download exactly at the limit is returned, one byte
// over is refused.
func TestGetFileSizeLimit(t *testing.T) {
	f := newFakeDocker(t)
	eng, _ := newEngine(f.host())
	f.files["/workspace/five"] = []byte("12345")
	f.files["/workspace/six"] = []byte("123456")
	ctx := context.Background()
	if b, err := eng.getFile(ctx, "c1", "/workspace/five", 5); err != nil || string(b) != "12345" {
		t.Fatalf("at the limit: %q, %v", b, err)
	}
	if _, err := eng.getFile(ctx, "c1", "/workspace/six", 5); err == nil || !strings.Contains(err.Error(), "download limit") {
		t.Fatalf("one byte over: %v", err)
	}
}

func TestFileOperationErrorsSurface(t *testing.T) {
	f := newFakeDocker(t)
	env := newEnv(t, f)
	sb, _, _ := env.Open(context.Background(), nil)
	ctx := context.Background()
	f.apiFn = func(c call) (int, string) {
		if strings.HasSuffix(c.Path, "/archive") {
			return 500, "archive broke"
		}
		return 0, ""
	}
	if err := sb.PutFile(ctx, "a.txt", []byte("x")); err == nil || !strings.Contains(err.Error(), "archive broke") {
		t.Fatalf("PutFile with a failing archive API = %v", err)
	}
	f.apiFn = nil
	// RemoveFile: a daemon error and a non-zero rm are both errors.
	f.run = func(cmd []string) (string, string, int, bool) { return "rm: cannot remove", "", 1, false }
	if err := sb.(toolpacks.FileRemover).RemoveFile(ctx, "a.txt"); err == nil || !strings.Contains(err.Error(), "cannot remove") {
		t.Fatalf("RemoveFile with a failing rm = %v", err)
	}
	if err := sb.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sb.(toolpacks.FileRemover).RemoveFile(ctx, "a.txt"); !errors.Is(err, toolpacks.ErrSandboxClosed) {
		t.Fatalf("RemoveFile on a closed sandbox = %v", err)
	}
}

func TestConfigBoundaries(t *testing.T) {
	if _, err := NewEnvironment(WithTimeout(time.Second)); err != nil {
		t.Fatalf("a timeout of exactly 1s is valid: %v", err)
	}
	f := newFakeDocker(t)
	// Output exactly at the cap is not truncated; one more character is.
	f.run = func([]string) (string, string, int, bool) { return strings.Repeat("z", 50), "", 0, false }
	env := newEnv(t, f, WithMaxOutput(50))
	sb, _, _ := env.Open(context.Background(), nil)
	if res, _ := sb.Exec(context.Background(), []string{"x"}, toolpacks.ExecOptions{}); res.Output != strings.Repeat("z", 50) {
		t.Fatalf("output at the cap = %q", res.Output)
	}
	f.run = func([]string) (string, string, int, bool) { return strings.Repeat("z", 51), "", 0, false }
	if res, _ := sb.Exec(context.Background(), []string{"x"}, toolpacks.ExecOptions{}); !strings.Contains(res.Output, "[truncated: showing first 50 of 51 chars]") {
		t.Fatalf("output over the cap = %q", res.Output)
	}
	// An empty-string variable value is refused for image and network mode.
	cx := agocontext.New(stream.NewMemoryStream())
	_ = cx.SetVariable("img", "")
	bad := newEnv(t, f, WithImageVariable(tools.NewVariable("img")))
	if _, _, err := bad.Open(context.Background(), cx); err == nil || !strings.Contains(err.Error(), "non-empty string") {
		t.Fatalf("empty image variable = %v", err)
	}
}
