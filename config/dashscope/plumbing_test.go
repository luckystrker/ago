package dashscope

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/luckystrker/ago/config"
)

func dashClient(t *testing.T, srvURL string, mutate func(*DashScopeConfig)) config.LLMClient {
	t.Helper()
	cfg := NewDashScope("qwen-plus")
	cfg.BaseURL, cfg.APIKey = srvURL, "test-key"
	if mutate != nil {
		mutate(cfg)
	}
	cl, err := cfg.Create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return cl
}

// TestSamplingKnobsOnWire: stop, seed and both penalties reach the body;
// unset ones stay off the wire.
func TestSamplingKnobsOnWire(t *testing.T) {
	cases := map[string]struct {
		set  func(*DashScopeConfig)
		key  string
		want any
	}{
		"stop":              {func(c *DashScopeConfig) { c.Stop = []string{"END"} }, "stop", []any{"END"}},
		"seed":              {func(c *DashScopeConfig) { c.Seed = i64ptr(9) }, "seed", float64(9)},
		"frequency_penalty": {func(c *DashScopeConfig) { c.FrequencyPenalty = f64ptr(0.4) }, "frequency_penalty", 0.4},
		"presence_penalty":  {func(c *DashScopeConfig) { c.PresencePenalty = f64ptr(-0.4) }, "presence_penalty", -0.4},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _, _, _, captured := captureServer(t, http.StatusOK, dashscopeCompletionBody)
			cl := dashClient(t, srv.URL, tc.set)
			if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
				t.Fatalf("call: %v", err)
			}
			got, ok := requestJSON(t, *captured)[tc.key]
			if !ok || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("body[%q] = %#v, want %#v", tc.key, got, tc.want)
			}
		})
	}

	srv, _, _, _, captured := captureServer(t, http.StatusOK, dashscopeCompletionBody)
	cl := dashClient(t, srv.URL, nil)
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatal(err)
	}
	body := requestJSON(t, *captured)
	for _, k := range []string{"stop", "seed", "frequency_penalty", "presence_penalty"} {
		if _, ok := body[k]; ok {
			t.Errorf("unset knob %q was sent", k)
		}
	}
}

type countingTransport struct{ n atomic.Int32 }

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return http.DefaultTransport.RoundTrip(r)
}

func TestInjectedHTTPClientChatAndStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "text/event-stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"id\":\"x\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(dashscopeCompletionBody))
	}))
	t.Cleanup(srv.Close)
	rt := &countingTransport{}
	hc := &http.Client{Transport: rt}
	for _, streaming := range []bool{false, true} {
		cl := dashClient(t, srv.URL, func(c *DashScopeConfig) { c.HTTPClient, c.Streaming = hc, streaming })
		if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
			t.Fatalf("streaming=%v: %v", streaming, err)
		}
	}
	if rt.n.Load() != 2 {
		t.Fatalf("injected client saw %d requests, want 2", rt.n.Load())
	}
}

func TestAPIErrorTyped(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		srv, _, _, _, _ := captureServer(t, http.StatusServiceUnavailable, `{"error":{"type":"server_error","message":"busy"}}`)
		cl := dashClient(t, srv.URL, func(c *DashScopeConfig) { c.Streaming = streaming })
		_, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
		var apiErr *config.APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("streaming=%v: want *config.APIError, got %T: %v", streaming, err, err)
		}
		if apiErr.Provider != config.ProviderDashScope || apiErr.StatusCode != 503 || !apiErr.Retryable || apiErr.Message != "busy" {
			t.Fatalf("streaming=%v: %+v", streaming, apiErr)
		}
		if !errors.Is(err, config.ErrServerError) {
			t.Fatalf("streaming=%v: errors.Is(ErrServerError) failed", streaming)
		}
	}
}

func TestCloneNewKnobs(t *testing.T) {
	hc := &http.Client{}
	cfg := NewDashScope("qwen-plus")
	cp, err := cfg.Clone(map[string]any{"http_client": hc, "stop": []string{"x"}, "seed": 3, "frequency_penalty": 0.1, "presence_penalty": f64ptr(0.2)})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	got := cp.(*DashScopeConfig)
	if got.HTTPClient != hc || got.Stop[0] != "x" || *got.Seed != 3 || *got.FrequencyPenalty != 0.1 || *got.PresencePenalty != 0.2 {
		t.Fatalf("overrides lost: %+v", got)
	}
	if cfg.Seed != nil || cfg.HTTPClient != nil {
		t.Fatal("original mutated")
	}
	if _, err := cfg.Clone(map[string]any{"stop": "x"}); err == nil {
		t.Fatal("wrongly typed override must fail")
	}
}
