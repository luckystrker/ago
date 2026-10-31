package mcpserver

// Tests for the deployment gates (security audit 2026-10-31, items H-3, M-6,
// L-2-mcpserver, L-4): the Authorize hook, the GET listener-stream cap, the
// stdio exchange deadline, and the Host/Origin allowlists.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- ServerConfig.Authorize (H-3) ------------------------------------------------

// postJSON sends one raw JSON-RPC message with optional extra headers and
// decodes the reply (rawPost plus header control, which the auth tests need).
func postJSON(t *testing.T, s *Server, msg string, hdr map[string]string) rpcResponse {
	t.Helper()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	req, err := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(msg))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", msg, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var out rpcResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return out
}

// TestAuthorizeGate pins the refusal: initialize and tools/* are answered
// with the fixed unauthorized error (the hook's own error text stays off the
// wire), ping never reaches the hook, and over HTTP the hook can read the
// caller's request through RequestFromContext.
func TestAuthorizeGate(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	sawAuth := ""
	s, err := NewServer(ServerConfig{
		Ask: func(context.Context, string) (string, error) { return "ok", nil },
		Authorize: func(ctx context.Context, method string) error {
			mu.Lock()
			defer mu.Unlock()
			methods = append(methods, method)
			if r := RequestFromContext(ctx); r != nil {
				sawAuth = r.Header.Get("Authorization")
			}
			return errors.New("denied: bad token")
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// initialize and tools/list are refused before anything else runs.
	for _, method := range []string{"initialize", "tools/list"} {
		resp := postJSON(t, s, fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q}`, method),
			map[string]string{"Authorization": "Bearer nope"})
		if resp.Error == nil || resp.Error.Code != codeUnauthorized {
			t.Errorf("%s = %+v, want code %d", method, resp, codeUnauthorized)
		}
	}
	// tools/call is refused too — ahead of the handshake gate and of the
	// tool lookup, and without the hook's error detail on the wire.
	resp := postJSON(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/call",
		"params":{"name":"ask","arguments":{"message":"x"}}}`,
		map[string]string{"Authorization": "Bearer nope"})
	if resp.Error == nil || resp.Error.Code != codeUnauthorized {
		t.Errorf("tools/call = %+v, want code %d", resp, codeUnauthorized)
	}
	if resp.Error != nil && strings.Contains(resp.Error.Message, "bad token") {
		t.Errorf("refusal message = %q, want the hook's detail kept off the wire", resp.Error.Message)
	}

	// ping stays open and never reaches the hook.
	resp = postJSON(t, s, `{"jsonrpc":"2.0","id":3,"method":"ping"}`, nil)
	if resp.Error != nil {
		t.Errorf("ping = %+v, want allowed", resp)
	}

	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(methods) != fmt.Sprint([]string{"initialize", "tools/list", "tools/call"}) {
		t.Errorf("hook saw %v, want the three gated calls only", methods)
	}
	if sawAuth != "Bearer nope" {
		t.Errorf("hook Authorization = %q, want the request's header via RequestFromContext", sawAuth)
	}
}

// TestAuthorizeRoundTrip pins the allow path: a hook that checks the bearer
// token lets the full exchange through.
func TestAuthorizeRoundTrip(t *testing.T) {
	s, err := NewServer(ServerConfig{
		Ask: func(_ context.Context, userText string) (string, error) {
			return "reply: " + userText, nil
		},
		Authorize: func(ctx context.Context, method string) error {
			r := RequestFromContext(ctx)
			if r == nil {
				return errors.New("no request context")
			}
			if r.Header.Get("Authorization") != "Bearer secret" {
				return errors.New("wrong token")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	hdr := map[string]string{"Authorization": "Bearer secret"}
	if resp := postJSON(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize",
		"params":{"protocolVersion":"`+protocolVersion+`"}}`, hdr); resp.Error != nil {
		t.Fatalf("initialize with token = %+v", resp)
	}
	if resp := postJSON(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, hdr); resp.Error != nil {
		t.Fatalf("tools/list with token = %+v", resp)
	}
	resp := postJSON(t, s, `{"jsonrpc":"2.0","id":3,"method":"tools/call",
		"params":{"name":"ask","arguments":{"message":"hi"}}}`, hdr)
	if resp.Error != nil {
		t.Fatalf("tools/call with token = %+v", resp)
	}
	var result struct {
		Content []contentBlock `json:"content"`
	}
	if err := json.Unmarshal(mustRaw(t, resp.Result), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "reply: hi" {
		t.Errorf("ask result = %+v, want the bridge's reply", result)
	}
	// A wrong token is refused with the same fixed message.
	wrong := postJSON(t, s, `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`,
		map[string]string{"Authorization": "Bearer wrong"})
	if wrong.Error == nil || wrong.Error.Code != codeUnauthorized {
		t.Errorf("tools/list wrong token = %+v, want code %d", wrong, codeUnauthorized)
	}
}

// TestAuthorizeNotificationOverHTTP pins the refused-notification arm: a
// notification of a gated method is dropped, not answered (202, empty body).
func TestAuthorizeNotificationOverHTTP(t *testing.T) {
	s, err := NewServer(ServerConfig{
		Ask:       func(context.Context, string) (string, error) { return "ok", nil },
		Authorize: func(context.Context, string) error { return errors.New("denied") },
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := http.Post(ts.URL, "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","method":"tools/list"}`))
	if err != nil {
		t.Fatalf("notification post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("refused notification status = %d, want 202", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(strings.TrimSpace(string(body))) != 0 {
		t.Errorf("refused notification body = %q, want empty", body)
	}
}

// --- stdio exchange deadline (L-4) ------------------------------------------------

// stdioHarness drives serveIO over pipes: send writes one request line, recv
// reads the next reply line.
type stdioHarness struct {
	inW  *io.PipeWriter
	out  chan string
	done chan error
}

func newStdioHarness(t *testing.T, s *Server) *stdioHarness {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := &stdioHarness{inW: inW, out: make(chan string, 8), done: make(chan error, 1)}
	go func() {
		err := s.serveIO(inR, outW)
		outW.Close()
		h.done <- err
	}()
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			h.out <- sc.Text()
		}
	}()
	t.Cleanup(func() { _ = h.inW.Close() })
	return h
}

func (h *stdioHarness) send(t *testing.T, line string) {
	t.Helper()
	if _, err := h.inW.Write([]byte(line + "\n")); err != nil {
		t.Fatalf("write request: %v", err)
	}
}

func (h *stdioHarness) recv(t *testing.T) string {
	t.Helper()
	select {
	case line := <-h.out:
		return line
	case <-time.After(10 * time.Second):
		t.Fatal("no reply within 10s")
		return ""
	}
}

// TestStdioCallTimeout pins the exchange deadline: a handler stuck until its
// context dies fails its own reply, and the loop reads on — the next exchange
// still answers. Without the deadline the second request would never run.
func TestStdioCallTimeout(t *testing.T) {
	unblocked := make(chan struct{})
	s, err := NewServer(ServerConfig{
		Ask: func(ctx context.Context, _ string) (string, error) {
			<-ctx.Done() // a stuck handler only the exchange deadline can stop
			close(unblocked)
			return "", ctx.Err()
		},
		CallTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	h := newStdioHarness(t, s)

	// The handshake first, so the stuck call is not refused by it.
	h.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+protocolVersion+`"}}`)
	if got := h.recv(t); !strings.Contains(got, `"result"`) {
		t.Fatalf("initialize = %s", got)
	}

	// One line per message: stdio is newline-delimited, so the request JSON
	// cannot wrap.
	h.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ask","arguments":{"message":"stuck"}}}`)
	got := h.recv(t)
	if !strings.Contains(got, `"isError":true`) || !strings.Contains(got, "context deadline exceeded") {
		t.Errorf("stuck ask = %s, want an isError result naming the deadline", got)
	}
	select {
	case <-unblocked:
	case <-time.After(10 * time.Second):
		t.Fatal("the exchange deadline never fired")
	}

	// The loop read on: a later exchange still answers.
	h.send(t, `{"jsonrpc":"2.0","id":3,"method":"ping"}`)
	if got := h.recv(t); !strings.Contains(got, `"id":3`) || !strings.Contains(got, `"result"`) {
		t.Errorf("ping after timeout = %s, want a live loop", got)
	}
}

// TestAuthorizeOverStdio pins the hook on the second transport: the refusal
// is a JSON-RPC error line, and RequestFromContext is nil — there is no HTTP
// request to read credentials from over stdio.
func TestAuthorizeOverStdio(t *testing.T) {
	sawRequest := false
	s, err := NewServer(ServerConfig{
		Ask: func(context.Context, string) (string, error) { return "ok", nil },
		Authorize: func(ctx context.Context, method string) error {
			if RequestFromContext(ctx) != nil {
				sawRequest = true
			}
			if method != "initialize" {
				return errors.New("unexpected method " + method)
			}
			return errors.New("denied")
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	h := newStdioHarness(t, s)
	h.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+protocolVersion+`"}}`)
	var resp rpcResponse
	if err := json.Unmarshal([]byte(h.recv(t)), &resp); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != codeUnauthorized {
		t.Errorf("initialize = %+v, want code %d", resp, codeUnauthorized)
	}
	if sawRequest {
		t.Error("stdio hook saw an HTTP request in RequestFromContext")
	}
}

// --- GET listener-stream cap (M-6) ------------------------------------------------

// TestGETStreamLimit pins the cap: streams over MaxStreams answer 503 without
// holding a slot, and a freed slot serves again.
func TestGETStreamLimit(t *testing.T) {
	s, err := NewServer(ServerConfig{
		Ask:        func(context.Context, string) (string, error) { return "ok", nil },
		MaxStreams: 1,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// The first stream holds the one slot (Do returns once the handler
	// wrote its headers, which happens after acquiring the slot).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	first, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("first stream: %v", err)
	}
	defer first.Body.Close()

	// Over the cap: refused, and the refusal holds no slot.
	second, err := http.Get(ts.URL)
	if err != nil {
		t.Fatalf("second stream: %v", err)
	}
	_, _ = io.Copy(io.Discard, second.Body)
	second.Body.Close()
	if second.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("second stream status = %d, want %d", second.StatusCode, http.StatusServiceUnavailable)
	}

	// Freed by the first client going away, a slot serves again. The freed
	// check closes the body rather than draining it: a held-open stream
	// never sends EOF, so a drain would block on purpose.
	cancel()
	first.Body.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		third, err := http.Get(ts.URL)
		if err == nil {
			code := third.StatusCode
			third.Body.Close()
			if code == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the stream slot was never freed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// --- Host/Origin allowlists (L-2) --------------------------------------------------

// TestHostAndOriginAllowlists pins both fences: listed values are served,
// unlisted Host and browser Origin values answer 403, requests without Origin
// (native clients) pass, and an unconfigured server serves anything.
func TestHostAndOriginAllowlists(t *testing.T) {
	newGated := func(t *testing.T) *httptest.Server {
		t.Helper()
		s, err := NewServer(ServerConfig{
			Ask:            func(context.Context, string) (string, error) { return "ok", nil },
			AllowedHosts:   []string{"ago.test", "localhost"},
			AllowedOrigins: []string{"https://app.test"},
		})
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		ts := httptest.NewServer(s.Handler())
		t.Cleanup(ts.Close)
		return ts
	}
	status := func(t *testing.T, ts *httptest.Server, host, origin string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, ts.URL,
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		req.Host = host
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	ts := newGated(t)
	// Listed host, no Origin — an ordinary native client.
	if c := status(t, ts, "ago.test", ""); c != http.StatusOK {
		t.Errorf("listed host = %d, want 200", c)
	}
	// A full host:port entry matches its own entry, not the bare one.
	if c := status(t, ts, "localhost:8080", ""); c != http.StatusForbidden {
		t.Errorf("host:port against bare entry = %d, want 403 (whole-value match)", c)
	}
	// A listed origin passes; case-insensitive scheme and host.
	if c := status(t, ts, "ago.test", "https://APP.test"); c != http.StatusOK {
		t.Errorf("listed origin = %d, want 200", c)
	}
	// An unlisted Host is refused — the DNS-rebinding reach.
	if c := status(t, ts, "evil.example", ""); c != http.StatusForbidden {
		t.Errorf("unlisted host = %d, want 403", c)
	}
	// A host:port entry admits exactly that host:port.
	if c := status(t, ts, "localhost", "https://app.test:8443"); c != http.StatusForbidden {
		t.Errorf("unlisted origin port = %d, want 403", c)
	}
	if c := status(t, ts, "ago.test", "https://evil.test"); c != http.StatusForbidden {
		t.Errorf("unlisted origin = %d, want 403", c)
	}
	// No prefix or suffix tricks.
	if c := status(t, ts, "ago.test.evil.io", ""); c != http.StatusForbidden {
		t.Errorf("suffix trick host = %d, want 403", c)
	}

	// Unconfigured: any Host and any Origin are served (the historical
	// behavior — the fences are opt-in).
	plain, err := NewServer(ServerConfig{
		Ask: func(context.Context, string) (string, error) { return "ok", nil },
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	plainTS := httptest.NewServer(plain.Handler())
	defer plainTS.Close()
	if c := status(t, plainTS, "evil.example", "https://evil.test"); c != http.StatusOK {
		t.Errorf("unconfigured server = %d, want 200 (checks off by default)", c)
	}
}
