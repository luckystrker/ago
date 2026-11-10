package mcp

import (
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"testing"
)

func TestProxyIsHonoured(t *testing.T) {
	// The proxy answers as the MCP server for a host that exists nowhere: the
	// call can only succeed by travelling through it.
	srv := newScenarioHTTP(false)
	proxy := httptest.NewServer(srv)
	defer proxy.Close()

	cl, err := New(&MCPServerConfig{URL: "http://mcp.proxied.test/mcp", Proxy: proxy.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cl.Close()
	out, err := cl.CallTool("caps", nil)
	if err != nil {
		t.Fatalf("CallTool through the proxy: %v", err)
	}
	if got := firstText(t, out); got != "{}" {
		t.Errorf("result = %q", got)
	}
	for _, r := range srv.requests("initialize") {
		if r.Host != "mcp.proxied.test" {
			t.Errorf("proxy saw host %q, want the target host", r.Host)
		}
	}
	if len(srv.requests("initialize")) == 0 {
		t.Fatal("the proxy saw no request")
	}
}

func TestProxyValidation(t *testing.T) {
	for _, bad := range []string{"not a url", "ftp://proxy.example:21", "://x"} {
		if _, err := New(&MCPServerConfig{URL: "http://127.0.0.1:1/mcp", Proxy: bad}); !errors.Is(err, ErrUnsupportedConfig) {
			t.Errorf("Proxy %q = %v, want ErrUnsupportedConfig", bad, err)
		}
	}
}

func TestTLSVerification(t *testing.T) {
	srv := httptest.NewUnstartedServer(newScenarioHTTP(false))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the refused handshake is expected noise
	srv.StartTLS()
	defer srv.Close()

	t.Run("default refuses a self-signed certificate", func(t *testing.T) {
		cl, err := New(&MCPServerConfig{URL: srv.URL})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer cl.Close()
		_, err = cl.CallTool("caps", nil)
		var unknown x509.UnknownAuthorityError
		if !errors.As(err, &unknown) {
			t.Fatalf("err = %v, want an x509 unknown-authority failure", err)
		}
	})
	t.Run("InsecureSkipVerify lets it through", func(t *testing.T) {
		cl, err := New(&MCPServerConfig{URL: srv.URL, InsecureSkipVerify: true})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer cl.Close()
		out, err := cl.CallTool("caps", nil)
		if err != nil {
			t.Fatalf("CallTool with verification off: %v", err)
		}
		if got := firstText(t, out); got != "{}" {
			t.Errorf("result = %q", got)
		}
	})
}
