package mcp

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestHTTPTransportStripsHeadersOnCrossHostRedirect: a 307 to another host
// must not carry the configured auth headers — the stdlib strips only a fixed
// few, so custom MCP headers (x-api-key, ...) would otherwise reach the
// redirected host.
func TestHTTPTransportStripsHeadersOnCrossHostRedirect(t *testing.T) {
	var gotAuth, gotAPIKey string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/rpc", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	tr, err := newHTTPTransport(&MCPServerConfig{
		URL:                source.URL,
		Headers:            map[string]string{"x-api-key": "secret"},
		AuthorizationToken: "token",
	})
	if err != nil {
		t.Fatalf("newHTTPTransport: %v", err)
	}
	if _, err := tr.call(context.Background(), 1, "tools/list", nil); err != nil {
		t.Fatalf("call through the redirect: %v", err)
	}
	if gotAuth != "" || gotAPIKey != "" {
		t.Fatalf("credentials leaked to the redirected host: Authorization=%q x-api-key=%q", gotAuth, gotAPIKey)
	}
}

// TestHTTPTransportSameHostRedirectKeepsHeaders: a hop inside the same
// authority (gateway re-routing) keeps the configured headers.
func TestHTTPTransportSameHostRedirectKeepsHeaders(t *testing.T) {
	var gotAuth, gotAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/final" {
			http.Redirect(w, r, "/final", http.StatusTemporaryRedirect)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer srv.Close()

	tr, err := newHTTPTransport(&MCPServerConfig{
		URL:                srv.URL,
		Headers:            map[string]string{"x-api-key": "secret"},
		AuthorizationToken: "token",
	})
	if err != nil {
		t.Fatalf("newHTTPTransport: %v", err)
	}
	if _, err := tr.call(context.Background(), 1, "tools/list", nil); err != nil {
		t.Fatalf("call through the redirect: %v", err)
	}
	if gotAuth != "Bearer token" || gotAPIKey != "secret" {
		t.Fatalf("same-host redirect must keep credentials, got Authorization=%q x-api-key=%q", gotAuth, gotAPIKey)
	}
}

// TestHTTPTransportResponseLimit: a JSON answer over maxJSONResponseBytes is
// a protocol error, not an unbounded read.
func TestHTTPTransportResponseLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(bytes.Repeat([]byte("x"), int(maxJSONResponseBytes)+1))
	}))
	defer srv.Close()

	tr, err := newHTTPTransport(&MCPServerConfig{URL: srv.URL})
	if err != nil {
		t.Fatalf("newHTTPTransport: %v", err)
	}
	_, err = tr.call(context.Background(), 1, "tools/list", nil)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want the oversized-response error", err)
	}
}

func TestParseAbsoluteURLRejectsNonAbsoluteURLs(t *testing.T) {
	// A URL that fails to parse and a scheme without a host must both be
	// rejected: neither is a usable streamable-HTTP endpoint.
	for _, raw := range []string{"http://[::1", "http:", "/tmp/sock"} {
		if u, err := parseAbsoluteURL(raw); err == nil {
			t.Fatalf("parseAbsoluteURL(%q) = %v, want an error", raw, u)
		}
	}
}

func TestNewHTTPTransportAuthorizationHeaderRules(t *testing.T) {
	// No token: the request carries no Authorization header at all. A token
	// never overwrites a custom Authorization entry the config already set.
	var gotPlain, gotCustom string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/custom" {
			gotCustom = r.Header.Get("Authorization")
		} else {
			gotPlain = r.Header.Get("Authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer srv.Close()

	tr, err := newHTTPTransport(&MCPServerConfig{URL: srv.URL + "/plain"})
	if err != nil {
		t.Fatalf("newHTTPTransport: %v", err)
	}
	if _, err := tr.call(context.Background(), 1, "tools/list", nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	if gotPlain != "" {
		t.Fatalf("a config without a token must not send Authorization, got %q", gotPlain)
	}

	tr2, err := newHTTPTransport(&MCPServerConfig{
		URL:                srv.URL + "/custom",
		AuthorizationToken: "tok",
		Headers:            map[string]string{"Authorization": "Custom abc"},
	})
	if err != nil {
		t.Fatalf("newHTTPTransport: %v", err)
	}
	if _, err := tr2.call(context.Background(), 1, "tools/list", nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	if gotCustom != "Custom abc" {
		t.Fatalf("custom Authorization overwritten, got %q", gotCustom)
	}
}

func TestHTTPTransportCallRejectsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			w.WriteHeader(299)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
			return
		}
		w.WriteHeader(500)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	tr, err := newHTTPTransport(&MCPServerConfig{URL: srv.URL + "/err"})
	if err != nil {
		t.Fatalf("newHTTPTransport: %v", err)
	}
	_, err = tr.call(context.Background(), 1, "tools/list", nil)
	if err == nil || !strings.Contains(err.Error(), "server responded 500") {
		t.Fatalf("err = %v, want the server-responded error", err)
	}

	tr2, err := newHTTPTransport(&MCPServerConfig{URL: srv.URL + "/ok"})
	if err != nil {
		t.Fatalf("newHTTPTransport: %v", err)
	}
	if _, err := tr2.call(context.Background(), 1, "tools/list", nil); err != nil {
		t.Fatalf("status 299 must be accepted, got %v", err)
	}
}

func TestHTTPTransportCallJSONResponseExactlyAtLimit(t *testing.T) {
	// A JSON answer of exactly maxJSONResponseBytes sits on the boundary: it
	// must be decoded, not rejected (the cap is "larger than").
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		const head = `{"jsonrpc":"2.0","id":1,"result":{"pad":"`
		pad := strings.Repeat("a", int(maxJSONResponseBytes)-len(head)-len(`"}}`))
		_, _ = w.Write([]byte(head + pad + `"}}`))
	}))
	defer srv.Close()

	tr, err := newHTTPTransport(&MCPServerConfig{URL: srv.URL})
	if err != nil {
		t.Fatalf("newHTTPTransport: %v", err)
	}
	res, err := tr.call(context.Background(), 1, "tools/list", nil)
	if err != nil {
		t.Fatalf("response of exactly maxJSONResponseBytes rejected: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("empty result")
	}
}

func TestHTTPTransportCallOverSSESkipsServerToClientRequest(t *testing.T) {
	// A server-to-client request that shares the awaited id must be skipped:
	// only a response (no method, id present) is the reply.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"sampling/createMessage\",\"params\":{}}\n\n")
		fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"ok\":true}}\n\n")
	}))
	defer srv.Close()

	tr, err := newHTTPTransport(&MCPServerConfig{URL: srv.URL})
	if err != nil {
		t.Fatalf("newHTTPTransport: %v", err)
	}
	res, err := tr.call(context.Background(), 1, "tools/list", nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if string(res) != `{"ok":true}` {
		t.Fatalf("result = %s, want the reply's result", res)
	}
}

func TestHTTPTransportNotifyStatusHandling(t *testing.T) {
	var code int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
	}))
	defer srv.Close()

	tr, err := newHTTPTransport(&MCPServerConfig{URL: srv.URL})
	if err != nil {
		t.Fatalf("newHTTPTransport: %v", err)
	}
	code = 200
	if err := tr.notify(context.Background(), "notifications/initialized", nil); err != nil {
		t.Fatalf("200: err = %v, want nil", err)
	}
	code = 299
	if err := tr.notify(context.Background(), "notifications/initialized", nil); err != nil {
		t.Fatalf("299: err = %v, want nil", err)
	}
	code = 500
	err = tr.notify(context.Background(), "notifications/initialized", nil)
	if err == nil || !strings.Contains(err.Error(), "rejected with status 500") {
		t.Fatalf("err = %v, want the rejected-notification error", err)
	}
}

func TestHTTPTransportCloseSendsSessionDelete(t *testing.T) {
	var deletes int32
	var gotSession string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			atomic.AddInt32(&deletes, 1)
			gotSession = r.Header.Get("Mcp-Session-Id")
		}
	}))
	defer srv.Close()

	tr, err := newHTTPTransport(&MCPServerConfig{URL: srv.URL})
	if err != nil {
		t.Fatalf("newHTTPTransport: %v", err)
	}
	tr.sessionID = "sess-1"
	if err := tr.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if atomic.LoadInt32(&deletes) != 1 {
		t.Fatalf("close sent %d DELETEs, want 1", deletes)
	}
	if gotSession != "sess-1" {
		t.Fatalf("DELETE carried Mcp-Session-Id %q, want sess-1", gotSession)
	}
	if tr.sessionID != "" {
		t.Fatalf("session id not cleared, got %q", tr.sessionID)
	}

	// Without a session no DELETE is sent.
	tr2, err := newHTTPTransport(&MCPServerConfig{URL: srv.URL})
	if err != nil {
		t.Fatalf("newHTTPTransport: %v", err)
	}
	if err := tr2.close(); err != nil {
		t.Fatalf("close without session: %v", err)
	}
	if atomic.LoadInt32(&deletes) != 1 {
		t.Fatalf("session-less close sent a DELETE, want none (now %d)", deletes)
	}

	// An unreachable server must not panic close: the transport error is
	// swallowed and the call stays nil.
	tr3, err := newHTTPTransport(&MCPServerConfig{URL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("newHTTPTransport: %v", err)
	}
	tr3.sessionID = "sess-2"
	if err := tr3.close(); err != nil {
		t.Fatalf("close on unreachable server: %v", err)
	}
}
