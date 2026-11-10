package mistral

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luckystrker/ago/config"
)

func boolPtr(v bool) *bool { return &v }

func mistralClient(t *testing.T, srvURL string, mutate func(*MistralConfig)) config.LLMClient {
	t.Helper()
	cfg := NewMistral("mistral-large-latest")
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

// TestSamplingKnobsOnWire: every typed Mistral knob reaches the body under
// its wire name.
func TestSamplingKnobsOnWire(t *testing.T) {
	cases := map[string]struct {
		set  func(*MistralConfig)
		key  string
		want any
	}{
		"stop":                {func(c *MistralConfig) { c.Stop = []string{"END"} }, "stop", []any{"END"}},
		"random_seed":         {func(c *MistralConfig) { c.RandomSeed = i64ptr(3) }, "random_seed", float64(3)},
		"presence_penalty":    {func(c *MistralConfig) { c.PresencePenalty = f64ptr(0.1) }, "presence_penalty", 0.1},
		"frequency_penalty":   {func(c *MistralConfig) { c.FrequencyPenalty = f64ptr(0.2) }, "frequency_penalty", 0.2},
		"repetition_penalty":  {func(c *MistralConfig) { c.RepetitionPenalty = f64ptr(1.1) }, "repetition_penalty", 1.1},
		"top_k":               {func(c *MistralConfig) { c.TopK = i64ptr(40) }, "top_k", float64(40)},
		"min_tokens":          {func(c *MistralConfig) { c.MinTokens = i64ptr(8) }, "min_tokens", float64(8)},
		"n":                   {func(c *MistralConfig) { c.N = i64ptr(2) }, "n", float64(2)},
		"top_logprobs":        {func(c *MistralConfig) { c.TopLogprobs = i64ptr(3) }, "top_logprobs", float64(3)},
		"service_tier":        {func(c *MistralConfig) { c.ServiceTier = "standard_only" }, "service_tier", "standard_only"},
		"metadata":            {func(c *MistralConfig) { c.Metadata = map[string]any{"a": 1, "b": "x"} }, "metadata", map[string]any{"a": float64(1), "b": "x"}},
		"parallel_tool_calls": {func(c *MistralConfig) { c.ParallelToolCalls = boolPtr(false) }, "parallel_tool_calls", false},
		"tool_choice":         {func(c *MistralConfig) { c.ToolChoice = "any" }, "tool_choice", "any"},
		"reasoning_effort":    {func(c *MistralConfig) { c.ReasoningEffort = "high" }, "reasoning_effort", "high"},
		"prompt_cache_key":    {func(c *MistralConfig) { c.PromptCacheKey = "k" }, "prompt_cache_key", "k"},
		"safe_prompt":         {func(c *MistralConfig) { c.SafePrompt = true }, "safe_prompt", true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _, _, _, captured := captureServer(t, http.StatusOK, mistralCompletionBody)
			cl := mistralClient(t, srv.URL, tc.set)
			if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
				t.Fatalf("call: %v", err)
			}
			got, ok := requestJSON(t, *captured)[tc.key]
			if !ok || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("body[%q] = %#v, want %#v", tc.key, got, tc.want)
			}
		})
	}

	srv, _, _, _, captured := captureServer(t, http.StatusOK, mistralCompletionBody)
	cl := mistralClient(t, srv.URL, nil)
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatal(err)
	}
	body := requestJSON(t, *captured)
	for _, k := range []string{"stop", "random_seed", "presence_penalty", "frequency_penalty", "repetition_penalty", "top_k",
		"min_tokens", "n", "top_logprobs", "service_tier", "metadata", "parallel_tool_calls", "tool_choice", "reasoning_effort",
		"prompt_cache_key", "safe_prompt", "seed"} {
		if _, ok := body[k]; ok {
			t.Errorf("unset knob %q was sent", k)
		}
	}
}

// TestExtraBodyReachesWireAndWins: extra_body carries a field the config has
// no slot for, and a colliding key replaces the derived one (SDK merge).
func TestExtraBodyReachesWireAndWins(t *testing.T) {
	srv, _, _, _, captured := captureServer(t, http.StatusOK, mistralCompletionBody)
	cl := mistralClient(t, srv.URL, func(c *MistralConfig) {
		c.TopK = i64ptr(10)
		c.ExtraBody = map[string]any{"top_k": 40, "vendor_flag": true}
	})
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	body := requestJSON(t, *captured)
	if body["top_k"] != float64(40) || body["vendor_flag"] != true {
		t.Fatalf("extra_body not merged last: %+v", body)
	}
}

// TestExtraBodyCloned: Clone copies the map, so mutating the copy leaves the
// original untouched.
func TestExtraBodyCloned(t *testing.T) {
	cfg := NewMistral("m")
	cfg.ExtraBody = map[string]any{"a": 1}
	cp, err := cfg.Clone(nil)
	if err != nil {
		t.Fatal(err)
	}
	got := cp.(*MistralConfig)
	got.ExtraBody["a"] = 2
	if cfg.ExtraBody["a"] != 1 {
		t.Fatal("Clone shared the extra_body map")
	}
	if cp2, err := cfg.Clone(map[string]any{"extra_body": map[string]any{"b": 1}}); err != nil || cp2.(*MistralConfig).ExtraBody["b"] != 1 {
		t.Fatalf("extra_body override: %v", err)
	}
}

func TestHTTPHeadersOnWire(t *testing.T) {
	srv, _, _, header, _ := captureServer(t, http.StatusOK, mistralCompletionBody)
	cl := mistralClient(t, srv.URL, func(c *MistralConfig) { c.HTTPHeaders = map[string]string{"X-Trace": "t-1"} })
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatal(err)
	}
	if (*header).Get("X-Trace") != "t-1" {
		t.Fatalf("headers: %v", *header)
	}
}

// TestTimeoutCapsRequest: the missing Mistral request timeout now cuts off a
// server that never answers.
func TestTimeoutCapsRequest(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	cl := mistralClient(t, srv.URL, func(c *MistralConfig) { c.Timeout = 50 * time.Millisecond })
	start := time.Now()
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err == nil {
		t.Fatal("a hung server must fail once Timeout elapses")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("call outlived the timeout by far: %v", elapsed)
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
		_, _ = w.Write([]byte(mistralCompletionBody))
	}))
	t.Cleanup(srv.Close)
	rt := &countingTransport{}
	hc := &http.Client{Transport: rt}
	for _, streaming := range []bool{false, true} {
		cl := mistralClient(t, srv.URL, func(c *MistralConfig) { c.HTTPClient, c.Streaming = hc, streaming })
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
		srv, _, _, _, _ := captureServer(t, http.StatusUnauthorized, `{"message":"Unauthorized","request_id":"r"}`)
		cl := mistralClient(t, srv.URL, func(c *MistralConfig) { c.Streaming = streaming })
		_, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
		var apiErr *config.APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("streaming=%v: want *config.APIError, got %T: %v", streaming, err, err)
		}
		if apiErr.Provider != config.ProviderMistral || apiErr.StatusCode != 401 || apiErr.Retryable || apiErr.Message != "Unauthorized" {
			t.Fatalf("streaming=%v: %+v", streaming, apiErr)
		}
		if !errors.Is(err, config.ErrAuthentication) {
			t.Fatalf("streaming=%v: errors.Is(ErrAuthentication) failed", streaming)
		}
	}
}

func TestCloneNewKnobs(t *testing.T) {
	hc := &http.Client{}
	cfg := NewMistral("m")
	cp, err := cfg.Clone(map[string]any{
		"timeout": 2 * time.Second, "http_client": hc, "http_headers": map[string]string{"H": "v"}, "stop": []string{"x"},
		"random_seed": 4, "presence_penalty": 0.5, "frequency_penalty": f64ptr(0.6), "repetition_penalty": 1.2, "top_k": 10,
		"min_tokens": 2, "n": 3, "top_logprobs": 1, "service_tier": "auto", "metadata": map[string]any{"a": 1},
		"parallel_tool_calls": true, "tool_choice": "none", "reasoning_effort": "low", "prompt_cache_key": "k",
	})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	got := cp.(*MistralConfig)
	if got.Timeout != 2*time.Second || got.HTTPClient != hc || got.HTTPHeaders["H"] != "v" || got.Stop[0] != "x" || *got.RandomSeed != 4 ||
		*got.PresencePenalty != 0.5 || *got.FrequencyPenalty != 0.6 || *got.RepetitionPenalty != 1.2 || *got.TopK != 10 ||
		*got.MinTokens != 2 || *got.N != 3 || *got.TopLogprobs != 1 || got.ServiceTier != "auto" || got.Metadata["a"] != 1 ||
		!*got.ParallelToolCalls || got.ToolChoice != "none" || got.ReasoningEffort != "low" || got.PromptCacheKey != "k" {
		t.Fatalf("overrides lost: %+v", got)
	}
	if cfg.Timeout != 0 || cfg.RandomSeed != nil {
		t.Fatal("original mutated")
	}
	if _, err := cfg.Clone(map[string]any{"timeout": 2}); err == nil {
		t.Fatal("wrongly typed override must fail")
	}
}
