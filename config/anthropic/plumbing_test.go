package anthropic

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/luckystrker/ago/config"
)

// countingTransport counts requests and delegates to the default transport.
type countingTransport struct{ n atomic.Int32 }

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return http.DefaultTransport.RoundTrip(r)
}

const minimalStream = "event: message_start\n" +
	"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"c\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

// TestExtraBodyReachesWireAndWins: extra_body carries an unmapped field
// (Python's only route to `thinking`, for one) and a colliding key replaces
// the derived one, as the SDK's merge does.
func TestExtraBodyReachesWireAndWins(t *testing.T) {
	srv, requests := captureServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, textResponse)
	})
	cl := newTestClient(srv, func(c *AnthropicConfig) {
		c.ServiceTier = "standard_only"
		c.ExtraBody = map[string]any{"service_tier": "priority", "vendor_flag": true}
	})
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	body := (*requests)[0]
	if body["service_tier"] != "priority" || body["vendor_flag"] != true {
		t.Fatalf("extra_body not merged last: %+v", body)
	}
}

// TestExtraBodyCloned: Clone copies the map and accepts an override for it.
func TestExtraBodyCloned(t *testing.T) {
	cfg := NewAnthropic("m")
	cfg.ExtraBody = map[string]any{"a": 1}
	cp, err := cfg.Clone(nil)
	if err != nil {
		t.Fatal(err)
	}
	got := cp.(*AnthropicConfig)
	got.ExtraBody["a"] = 2
	if cfg.ExtraBody["a"] != 1 {
		t.Fatal("Clone shared the extra_body map")
	}
	cp2, err := cfg.Clone(map[string]any{"extra_body": map[string]any{"b": 1}, "metadata": nil})
	if err != nil {
		t.Fatal(err)
	}
	if cp2.(*AnthropicConfig).ExtraBody["b"] != 1 {
		t.Fatalf("extra_body override lost: %+v", cp2.(*AnthropicConfig).ExtraBody)
	}
}

// TestInjectedHTTPClientChatStreamAndFiles: the caller's client carries the
// plain call, the streaming call and the Files client.
func TestInjectedHTTPClientChatStreamAndFiles(t *testing.T) {
	srv, _ := captureServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == messagesPath && r.Header.Get("Accept") == "text/event-stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, minimalStream)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, textResponse)
	})
	rt := &countingTransport{}
	hc := &http.Client{Transport: rt}

	cl := newTestClient(srv, func(c *AnthropicConfig) { c.HTTPClient = hc })
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatalf("chat: %v", err)
	}
	cl = newTestClient(srv, func(c *AnthropicConfig) { c.HTTPClient, c.Streaming = hc, true })
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if rt.n.Load() != 2 {
		t.Fatalf("chat/stream used the injected client %d times, want 2", rt.n.Load())
	}

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) }))
	t.Cleanup(plain.Close)
	cfg := NewAnthropic("claude-sonnet")
	cfg.APIKey, cfg.BaseURL, cfg.HTTPClient = "k", plain.URL, hc
	fc, err := cfg.CreateFilesClient()
	if err != nil {
		t.Fatal(err)
	}
	_ = fc.Delete(context.Background(), "file_1")
	if rt.n.Load() != 3 {
		t.Fatalf("files client bypassed the injected client (%d requests)", rt.n.Load())
	}
}

// TestAPIErrorTypedOnChatStreamAndFiles: errors.As finds *config.APIError on
// every Anthropic call path; the files sentinel keeps matching.
func TestAPIErrorTypedOnChatStreamAndFiles(t *testing.T) {
	srv, _ := captureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	})
	check := func(name string, err error) {
		t.Helper()
		var apiErr *config.APIError
		if !errors.As(err, &apiErr) {
			t.Errorf("%s: want *config.APIError, got %T: %v", name, err, err)
			return
		}
		if apiErr.Provider != config.ProviderAnthropic || apiErr.StatusCode != 429 || !apiErr.Retryable ||
			apiErr.Type != "rate_limit_error" || apiErr.Message != "slow down" {
			t.Errorf("%s: %+v", name, apiErr)
		}
		if !errors.Is(err, config.ErrRateLimited) {
			t.Errorf("%s: errors.Is(ErrRateLimited) failed", name)
		}
	}
	cl := newTestClient(srv, func(c *AnthropicConfig) { c.MaxRetries = 0 })
	_, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
	check("chat", err)
	cl = newTestClient(srv, func(c *AnthropicConfig) { c.MaxRetries, c.Streaming = 0, true })
	_, err = cl.Call(context.Background(), nil, nil, config.CallOptions{})
	check("stream", err)

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	}))
	t.Cleanup(failing.Close)
	fc := filesTestClient(failing)
	err = fc.Delete(context.Background(), "file_1")
	check("files", err)
	if !errors.Is(err, ErrFilesAPI) {
		t.Errorf("files: ErrFilesAPI sentinel lost: %v", err)
	}
}

// TestFilesClientRetriesOn409: the files transport shares the SDK retry set.
func TestFilesClientRetriesOn409(t *testing.T) {
	old := retryBaseDelay
	retryBaseDelay = 1
	t.Cleanup(func() { retryBaseDelay = old })
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusConflict)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	fc := filesTestClient(srv)
	fc.maxRetries = 1
	if err := fc.Delete(context.Background(), "file_1"); err != nil || calls.Load() != 2 {
		t.Fatalf("409 must be retried once: err=%v calls=%d", err, calls.Load())
	}
}
