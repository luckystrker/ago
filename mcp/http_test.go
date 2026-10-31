package mcp

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
