package ollama

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/luckystrker/ago/config"
)

const ollamaOK = `{"model":"m","message":{"role":"assistant","content":"hi"},"done":true,"done_reason":"stop"}`

// TestPenaltiesOnWire: both penalties travel in the options block; unset
// ones stay off the wire.
func TestPenaltiesOnWire(t *testing.T) {
	srv, got := captureServer(t, 200, ollamaOK)
	cl := startClient(t, NewOllama("m", WithHost(srv.URL), WithFrequencyPenalty(0.3), WithPresencePenalty(-0.2)))
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatal(err)
	}
	opts, _ := got.JSON(t)["options"].(map[string]any)
	if opts["frequency_penalty"] != 0.3 || opts["presence_penalty"] != -0.2 {
		t.Fatalf("options = %+v", opts)
	}

	srv2, got2 := captureServer(t, 200, ollamaOK)
	cl = startClient(t, NewOllama("m", WithHost(srv2.URL), WithTemperature(0.1)))
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatal(err)
	}
	opts, _ = got2.JSON(t)["options"].(map[string]any)
	if _, ok := opts["frequency_penalty"]; ok {
		t.Fatalf("unset penalty was sent: %+v", opts)
	}
	if _, ok := opts["presence_penalty"]; ok {
		t.Fatalf("unset penalty was sent: %+v", opts)
	}
}

func TestCloneCarriesPenaltiesAndClient(t *testing.T) {
	hc := &http.Client{}
	cfg := NewOllama("m")
	cp, err := cfg.Clone(map[string]any{"frequency_penalty": 0.5, "presence_penalty": f64(0.6), "http_client": hc})
	if err != nil {
		t.Fatal(err)
	}
	got := cp.(*OllamaConfig)
	if *got.frequencyPenalty != 0.5 || *got.presencePenalty != 0.6 || got.httpClient != hc {
		t.Fatalf("overrides lost: %+v", got)
	}
	if cfg.frequencyPenalty != nil || cfg.httpClient != nil {
		t.Fatal("original mutated")
	}
	if _, err := cfg.Clone(map[string]any{"http_client": 3}); err == nil {
		t.Fatal("wrong type must fail")
	}
}

type countingTransport struct{ n atomic.Int32 }

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return http.DefaultTransport.RoundTrip(r)
}

func TestInjectedHTTPClient(t *testing.T) {
	srv, _ := captureServer(t, 200, ollamaOK)
	rt := &countingTransport{}
	cl := startClient(t, NewOllama("m", WithHost(srv.URL), WithHTTPClient(&http.Client{Transport: rt})))
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatal(err)
	}
	if rt.n.Load() != 1 {
		t.Fatalf("injected client saw %d requests, want 1", rt.n.Load())
	}
}

func TestAPIErrorTyped(t *testing.T) {
	srv, _ := captureServer(t, 404, `{"error":"model 'm' not found"}`)
	cl := startClient(t, NewOllama("m", WithHost(srv.URL)))
	_, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
	var apiErr *config.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *config.APIError, got %T: %v", err, err)
	}
	if apiErr.Provider != config.ProviderOllama || apiErr.StatusCode != 404 || apiErr.Retryable || apiErr.Message != "model 'm' not found" {
		t.Fatalf("%+v", apiErr)
	}
	if !errors.Is(err, config.ErrNotFound) {
		t.Fatal("errors.Is(ErrNotFound) failed")
	}
}
