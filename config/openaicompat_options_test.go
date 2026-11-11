package config

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

const okCompletion = `{"id":"x","model":"m","choices":[{"finish_reason":"stop","message":{"content":"hi"}}]}`

// TestRunChatCompatTypedOptionsOnWire: every typed Chat Completions option
// reaches the request body under its wire name.
func TestRunChatCompatTypedOptionsOnWire(t *testing.T) {
	cases := map[string]struct {
		set  func(*CompatParams)
		key  string
		want any
	}{
		"tool_choice string": {func(p *CompatParams) { p.ToolChoice = "required" }, "tool_choice", "required"},
		"tool_choice object": {func(p *CompatParams) {
			p.ToolChoice = map[string]any{"type": "function", "function": map[string]any{"name": "f"}}
		}, "tool_choice", map[string]any{"type": "function", "function": map[string]any{"name": "f"}}},
		"reasoning_effort":      {func(p *CompatParams) { p.ReasoningEffort = "high" }, "reasoning_effort", "high"},
		"seed":                  {func(p *CompatParams) { p.Seed = i64ptr(7) }, "seed", float64(7)},
		"max_completion_tokens": {func(p *CompatParams) { p.MaxCompletionTokens = i64ptr(99) }, "max_completion_tokens", float64(99)},
		"frequency_penalty":     {func(p *CompatParams) { p.FrequencyPenalty = f64ptr(0.5) }, "frequency_penalty", 0.5},
		"presence_penalty":      {func(p *CompatParams) { p.PresencePenalty = f64ptr(-0.5) }, "presence_penalty", -0.5},
		"user":                  {func(p *CompatParams) { p.User = "u-1" }, "user", "u-1"},
		"n":                     {func(p *CompatParams) { p.N = i64ptr(2) }, "n", float64(2)},
		"logprobs":              {func(p *CompatParams) { p.Logprobs = boolptr(true) }, "logprobs", true},
		"top_logprobs":          {func(p *CompatParams) { p.TopLogprobs = i64ptr(5) }, "top_logprobs", float64(5)},
		"logit_bias":            {func(p *CompatParams) { p.LogitBias = map[string]int64{"50256": -100} }, "logit_bias", map[string]any{"50256": float64(-100)}},
		"metadata":              {func(p *CompatParams) { p.Metadata = map[string]string{"k": "v"} }, "metadata", map[string]any{"k": "v"}},
		"modalities":            {func(p *CompatParams) { p.Modalities = []string{"text", "audio"} }, "modalities", []any{"text", "audio"}},
		"prediction": {func(p *CompatParams) { p.Prediction = map[string]any{"type": "content", "content": "x"} }, "prediction",
			map[string]any{"type": "content", "content": "x"}},
		"prompt_cache_key":     {func(p *CompatParams) { p.PromptCacheKey = "tenant-1" }, "prompt_cache_key", "tenant-1"},
		"prompt_cache_options": {func(p *CompatParams) { p.PromptCacheOptions = map[string]any{"mode": "auto"} }, "prompt_cache_options", map[string]any{"mode": "auto"}},
		"safety_identifier":    {func(p *CompatParams) { p.SafetyIdentifier = "hash" }, "safety_identifier", "hash"},
		"service_tier":         {func(p *CompatParams) { p.ServiceTier = "flex" }, "service_tier", "flex"},
		"store":                {func(p *CompatParams) { p.Store = boolptr(false) }, "store", false},
		"verbosity":            {func(p *CompatParams) { p.Verbosity = "low" }, "verbosity", "low"},
		"web_search_options": {func(p *CompatParams) { p.WebSearchOptions = map[string]any{"search_context_size": "low"} }, "web_search_options",
			map[string]any{"search_context_size": "low"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv, captured := captureServer(t, 200, okCompletion)
			p := CompatParams{APIKey: "k", BaseURL: srv.URL, Model: "m"}
			tc.set(&p)
			if _, err := RunChatCompat(context.Background(), p, nil, nil, CallOptions{}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got, ok := requestJSON(t, *captured)[tc.key]
			if !ok || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("body[%q] = %#v, want %#v", tc.key, got, tc.want)
			}
		})
	}
}

// TestRunChatCompatZeroOptionsNotSent: an unset option must not appear on the
// wire, so existing callers see no change.
func TestRunChatCompatZeroOptionsNotSent(t *testing.T) {
	srv, captured := captureServer(t, 200, okCompletion)
	p := CompatParams{APIKey: "k", BaseURL: srv.URL, Model: "m"}
	if _, err := RunChatCompat(context.Background(), p, nil, nil, CallOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := requestJSON(t, *captured)
	for _, key := range []string{"tool_choice", "reasoning_effort", "seed", "max_completion_tokens", "frequency_penalty",
		"presence_penalty", "user", "n", "logprobs", "top_logprobs", "logit_bias", "metadata", "modalities", "prediction",
		"prompt_cache_key", "prompt_cache_options", "safety_identifier", "service_tier", "store", "verbosity", "web_search_options"} {
		if _, ok := body[key]; ok {
			t.Errorf("unset option %q was sent", key)
		}
	}
}

// TestRunChatCompatExtraOverridesTypedOptions: extra_body merges last, as the
// OpenAI SDK does.
func TestRunChatCompatExtraOverridesTypedOptions(t *testing.T) {
	srv, captured := captureServer(t, 200, okCompletion)
	p := CompatParams{APIKey: "k", BaseURL: srv.URL, Model: "m", ReasoningEffort: "low", ServiceTier: "auto",
		Extra: map[string]any{"reasoning_effort": "high", "custom": 1}}
	if _, err := RunChatCompat(context.Background(), p, nil, nil, CallOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := requestJSON(t, *captured)
	if body["reasoning_effort"] != "high" || body["service_tier"] != "auto" || body["custom"] != float64(1) {
		t.Fatalf("extra must win over typed options: %+v", body)
	}
}

// TestRunChatCompatQueryAndHeaders: default_query rides on the URL and
// extra headers on the request.
func TestRunChatCompatQueryAndHeaders(t *testing.T) {
	var gotQuery, gotHeader, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		gotHeader = r.Header.Get("X-Trace")
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, okCompletion)
	}))
	t.Cleanup(srv.Close)
	p := CompatParams{APIKey: "k", BaseURL: srv.URL, Model: "m",
		Query:        map[string]string{"api-version": "2025-01-01", "tenant": "a b"},
		ExtraHeaders: map[string]string{"X-Trace": "t-1"}}
	if _, err := RunChatCompat(context.Background(), p, nil, nil, CallOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotQuery != "api-version=2025-01-01&tenant=a+b" {
		t.Fatalf("query = %q", gotQuery)
	}
	if gotHeader != "t-1" || gotAuth != "Bearer k" {
		t.Fatalf("headers = %q / %q", gotHeader, gotAuth)
	}
}

// TestRunChatCompatMapTool: a provider hook maps a tool the dialect cannot
// carry; an unhandled tool still falls through to ToolToAPI.
func TestRunChatCompatMapTool(t *testing.T) {
	srv, captured := captureServer(t, 200, okCompletion)
	hosted := hostedFake{}
	p := CompatParams{APIKey: "k", BaseURL: srv.URL, Model: "m",
		MapTool: func(tool any) (map[string]any, bool, error) {
			if _, ok := tool.(hostedFake); ok {
				return map[string]any{"type": "retrieval"}, true, nil
			}
			return nil, false, nil
		}}
	if _, err := RunChatCompat(context.Background(), p, nil, nil, CallOptions{Tools: []any{hosted}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wireTools, _ := requestJSON(t, *captured)["tools"].([]any)
	if len(wireTools) != 1 || wireTools[0].(map[string]any)["type"] != "retrieval" {
		t.Fatalf("hook output missing: %+v", wireTools)
	}

	p.MapTool = func(any) (map[string]any, bool, error) { return nil, false, nil }
	if _, err := RunChatCompat(context.Background(), p, nil, nil, CallOptions{Tools: []any{hosted}}); !errors.Is(err, ErrUnsupportedTool) {
		t.Fatalf("an unhandled hosted tool must still fail fast, got %v", err)
	}
	boom := errors.New("boom")
	p.MapTool = func(any) (map[string]any, bool, error) { return nil, false, boom }
	if _, err := RunChatCompat(context.Background(), p, nil, nil, CallOptions{Tools: []any{hosted}}); !errors.Is(err, boom) {
		t.Fatalf("hook error must surface, got %v", err)
	}
}

type hostedFake struct{}

func (hostedFake) SchemaType() string { return "fake" }

// countingTransport records requests and delegates to the default transport.
type countingTransport struct{ n atomic.Int32 }

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return http.DefaultTransport.RoundTrip(r)
}

// TestRunChatCompatInjectedHTTPClient: a caller-supplied client carries both
// the plain and the streaming call.
func TestRunChatCompatInjectedHTTPClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"id\":\"x\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, okCompletion)
	}))
	t.Cleanup(srv.Close)

	rt := &countingTransport{}
	p := CompatParams{APIKey: "k", BaseURL: srv.URL, Model: "m", HTTPClient: &http.Client{Transport: rt}}
	if _, err := RunChatCompat(context.Background(), p, nil, nil, CallOptions{}); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if rt.n.Load() != 1 {
		t.Fatalf("chat did not use the injected client (%d requests)", rt.n.Load())
	}
	p.Streaming = true
	if _, err := RunChatCompat(context.Background(), p, nil, nil, CallOptions{}); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if rt.n.Load() != 2 {
		t.Fatalf("stream did not use the injected client (%d requests)", rt.n.Load())
	}
}

// TestRunChatCompatAPIErrorTyped: both call paths return an *APIError.
func TestRunChatCompatAPIErrorTyped(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		srv, _ := captureServer(t, 429, `{"error":{"type":"rate_limit_error","message":"slow down"}}`)
		p := CompatParams{APIKey: "k", BaseURL: srv.URL, Model: "m", Provider: ProviderZAI, Streaming: streaming}
		_, err := RunChatCompat(context.Background(), p, nil, nil, CallOptions{})
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("streaming=%v: want *APIError, got %T: %v", streaming, err, err)
		}
		if apiErr.Provider != ProviderZAI || apiErr.StatusCode != 429 || !apiErr.Retryable ||
			apiErr.Type != "rate_limit_error" || apiErr.Message != "slow down" {
			t.Fatalf("streaming=%v: %+v", streaming, apiErr)
		}
		if !errors.Is(err, ErrRateLimited) {
			t.Fatalf("streaming=%v: errors.Is(ErrRateLimited) failed", streaming)
		}
	}
}
