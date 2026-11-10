package gemini

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/luckystrker/ago/config"
)

const geminiOK = `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}]}`

// countingTransport counts requests and delegates to the default transport.
type countingTransport struct{ n atomic.Int32 }

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return http.DefaultTransport.RoundTrip(r)
}

// TestInjectedHTTPClientChatAndStream: the caller's client carries the plain
// and the streaming call.
func TestInjectedHTTPClientChatAndStream(t *testing.T) {
	srv, _ := captureServer(t, 200, geminiOK)
	rt := &countingTransport{}
	hc := &http.Client{Transport: rt}
	for _, streaming := range []bool{false, true} {
		cl := startClient(t, newTestConfig(srv.URL, WithHTTPClient(hc), WithStreaming(streaming)))
		if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
			t.Fatalf("streaming=%v: %v", streaming, err)
		}
	}
	if rt.n.Load() != 2 {
		t.Fatalf("injected client saw %d requests, want 2", rt.n.Load())
	}
}

func TestCloneHTTPClient(t *testing.T) {
	hc := &http.Client{}
	cfg := newTestConfig("http://x")
	cp, err := cfg.Clone(map[string]any{"http_client": hc})
	if err != nil || cp.(*GeminiConfig).httpClient != hc {
		t.Fatalf("clone did not carry the client: %v", err)
	}
	if cfg.httpClient != nil {
		t.Fatal("original mutated")
	}
	if _, err := cfg.Clone(map[string]any{"http_client": "nope"}); err == nil {
		t.Fatal("wrong type must fail")
	}
	cleared, _ := cp.Clone(map[string]any{"http_client": nil})
	if cleared.(*GeminiConfig).httpClient != nil {
		t.Fatal("nil must clear")
	}
}

// TestAPIErrorTyped: errors.As finds *config.APIError on both call paths.
func TestAPIErrorTyped(t *testing.T) {
	srv, _ := captureServer(t, 429, `{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED"}}`)
	for _, streaming := range []bool{false, true} {
		cl := startClient(t, newTestConfig(srv.URL, WithStreaming(streaming)))
		_, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
		var apiErr *config.APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("streaming=%v: want *config.APIError, got %T: %v", streaming, err, err)
		}
		if apiErr.Provider != config.ProviderGemini || apiErr.StatusCode != 429 || !apiErr.Retryable || apiErr.Message != "quota" {
			t.Fatalf("streaming=%v: %+v", streaming, apiErr)
		}
		if !errors.Is(err, config.ErrRateLimited) {
			t.Fatalf("streaming=%v: errors.Is(ErrRateLimited) failed", streaming)
		}
	}
}
