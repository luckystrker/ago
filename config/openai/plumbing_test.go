package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/response"
)

const chatOK = `{"id":"x","model":"m","choices":[{"finish_reason":"stop","message":{"content":"hi"}}]}`

// TestChatConfigTypedOptionsOnWire: each typed option of OpenAICompletionsConfig
// travels under its wire name; unset ones stay off the wire.
func TestChatConfigTypedOptionsOnWire(t *testing.T) {
	cases := map[string]struct {
		set  func(*OpenAICompletionsConfig)
		key  string
		want any
	}{
		"tool_choice":       {func(c *OpenAICompletionsConfig) { c.ToolChoice = "required" }, "tool_choice", "required"},
		"reasoning_effort":  {func(c *OpenAICompletionsConfig) { c.ReasoningEffort = "xhigh" }, "reasoning_effort", "xhigh"},
		"frequency_penalty": {func(c *OpenAICompletionsConfig) { c.FrequencyPenalty = f64(0.25) }, "frequency_penalty", 0.25},
		"presence_penalty":  {func(c *OpenAICompletionsConfig) { c.PresencePenalty = f64(0.75) }, "presence_penalty", 0.75},
		"n":                 {func(c *OpenAICompletionsConfig) { c.N = i64(3) }, "n", float64(3)},
		"logprobs":          {func(c *OpenAICompletionsConfig) { c.Logprobs = b(true) }, "logprobs", true},
		"top_logprobs":      {func(c *OpenAICompletionsConfig) { c.TopLogprobs = i64(4) }, "top_logprobs", float64(4)},
		"logit_bias":        {func(c *OpenAICompletionsConfig) { c.LogitBias = map[string]int64{"1": 5} }, "logit_bias", map[string]any{"1": float64(5)}},
		"metadata":          {func(c *OpenAICompletionsConfig) { c.Metadata = map[string]string{"a": "b"} }, "metadata", map[string]any{"a": "b"}},
		"modalities":        {func(c *OpenAICompletionsConfig) { c.Modalities = []string{"text"} }, "modalities", []any{"text"}},
		"prediction": {func(c *OpenAICompletionsConfig) { c.Prediction = map[string]any{"type": "content", "content": "p"} }, "prediction",
			map[string]any{"type": "content", "content": "p"}},
		"prompt_cache_key":     {func(c *OpenAICompletionsConfig) { c.PromptCacheKey = "k1" }, "prompt_cache_key", "k1"},
		"prompt_cache_options": {func(c *OpenAICompletionsConfig) { c.PromptCacheOptions = map[string]any{"mode": "x"} }, "prompt_cache_options", map[string]any{"mode": "x"}},
		"safety_identifier":    {func(c *OpenAICompletionsConfig) { c.SafetyIdentifier = "sid" }, "safety_identifier", "sid"},
		"service_tier":         {func(c *OpenAICompletionsConfig) { c.ServiceTier = "priority" }, "service_tier", "priority"},
		"store":                {func(c *OpenAICompletionsConfig) { c.Store = b(true) }, "store", true},
		"verbosity":            {func(c *OpenAICompletionsConfig) { c.Verbosity = "high" }, "verbosity", "high"},
		"web_search_options": {func(c *OpenAICompletionsConfig) { c.WebSearchOptions = map[string]any{"search_context_size": "high"} }, "web_search_options",
			map[string]any{"search_context_size": "high"}},
		"seed":                  {func(c *OpenAICompletionsConfig) { c.Seed = i64(11) }, "seed", float64(11)},
		"user":                  {func(c *OpenAICompletionsConfig) { c.User = "u" }, "user", "u"},
		"max_completion_tokens": {func(c *OpenAICompletionsConfig) { c.MaxCompletionTokens = i64(55) }, "max_completion_tokens", float64(55)},
		"parallel_tool_calls":   {func(c *OpenAICompletionsConfig) { c.ParallelToolCalls = b(false) }, "parallel_tool_calls", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv, got := captureServer(t, "application/json", 200, chatOK)
			cfg := NewOpenAICompletions("gpt-5")
			cfg.APIKey, cfg.BaseURL = "k", srv.URL
			tc.set(cfg)
			callOnce(t, cfg, nil, config.CallOptions{})
			v, ok := bodyJSON(t, got.body)[tc.key]
			if !ok || !reflect.DeepEqual(v, tc.want) {
				t.Fatalf("body[%q] = %#v, want %#v", tc.key, v, tc.want)
			}
		})
	}

	// Unset options stay off the wire; an empty prompt_cache_options is not a setting.
	srv, got := captureServer(t, "application/json", 200, chatOK)
	cfg := NewOpenAICompletions("gpt-5")
	cfg.APIKey, cfg.BaseURL = "k", srv.URL
	cfg.PromptCacheOptions = map[string]any{}
	callOnce(t, cfg, nil, config.CallOptions{})
	body := bodyJSON(t, got.body)
	for _, key := range []string{"tool_choice", "reasoning_effort", "n", "logprobs", "metadata", "service_tier", "store",
		"verbosity", "prompt_cache_key", "prompt_cache_options", "web_search_options", "frequency_penalty"} {
		if _, ok := body[key]; ok {
			t.Errorf("unset option %q was sent", key)
		}
	}
}

// TestChatConfigExtraBodyWinsOverTypedOptions: extra_body merges last.
func TestChatConfigExtraBodyWinsOverTypedOptions(t *testing.T) {
	srv, got := captureServer(t, "application/json", 200, chatOK)
	cfg := NewOpenAICompletions("gpt-5")
	cfg.APIKey, cfg.BaseURL = "k", srv.URL
	cfg.ReasoningEffort = "low"
	cfg.Seed = i64(1)
	cfg.ExtraBody = map[string]any{"reasoning_effort": "high", "seed": 2, "vendor_flag": true}
	callOnce(t, cfg, nil, config.CallOptions{})
	body := bodyJSON(t, got.body)
	if body["reasoning_effort"] != "high" || body["seed"] != float64(2) || body["vendor_flag"] != true {
		t.Fatalf("extra_body must override typed options: %+v", body)
	}
}

func TestChatConfigDefaultQueryAndHeaders(t *testing.T) {
	srv, got := captureServer(t, "application/json", 200, chatOK)
	var gotQuery string
	h := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		h.ServeHTTP(w, r)
	})
	cfg := NewOpenAICompletions("gpt-5")
	cfg.APIKey, cfg.BaseURL = "k", srv.URL
	cfg.DefaultQuery = map[string]string{"api-version": "2025-06-01"}
	cfg.DefaultHeaders = map[string]string{"X-Trace": "t1"}
	callOnce(t, cfg, nil, config.CallOptions{})
	if gotQuery != "api-version=2025-06-01" {
		t.Fatalf("query = %q", gotQuery)
	}
	if got.header.Get("X-Trace") != "t1" {
		t.Fatalf("header missing: %v", got.header)
	}
}

func TestChatConfigCloneNewKeys(t *testing.T) {
	cfg := NewOpenAICompletions("gpt-5")
	hc := &http.Client{}
	cp, err := cfg.Clone(map[string]any{
		"tool_choice":      "none",
		"reasoning_effort": "low",
		"n":                i64(2),
		"logit_bias":       map[string]int64{"1": 1},
		"metadata":         map[string]string{"a": "b"},
		"modalities":       []string{"text"},
		"store":            b(true),
		"http_client":      hc,
		"default_query":    map[string]string{"q": "1"},
		"verbosity":        "low",
	})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	got := cp.(*OpenAICompletionsConfig)
	if got.ToolChoice != "none" || got.ReasoningEffort != "low" || *got.N != 2 || got.HTTPClient != hc ||
		got.DefaultQuery["q"] != "1" || got.Verbosity != "low" || !*got.Store || got.Modalities[0] != "text" {
		t.Fatalf("overrides not applied: %+v", got)
	}
	if cfg.ToolChoice != nil || cfg.HTTPClient != nil {
		t.Fatal("original config mutated")
	}
	if _, err := cfg.Clone(map[string]any{"n": 2}); err == nil {
		t.Fatal("a wrongly typed override must fail")
	}
	cleared, err := got.Clone(map[string]any{"n": nil, "http_client": nil})
	if err != nil || cleared.(*OpenAICompletionsConfig).N != nil || cleared.(*OpenAICompletionsConfig).HTTPClient != nil {
		t.Fatalf("nil must clear: %v %+v", err, cleared)
	}
}

// countingTransport counts requests and delegates to the default transport.
type countingTransport struct{ n atomic.Int32 }

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return http.DefaultTransport.RoundTrip(r)
}

func sseOrJSONServer(t *testing.T, chat, sse string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, sse)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, chat)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestInjectedHTTPClientChatAndStream: the caller's client carries Chat
// Completions calls, plain and streaming.
func TestInjectedHTTPClientChatAndStream(t *testing.T) {
	srv := sseOrJSONServer(t, chatOK,
		"data: {\"id\":\"x\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	rt := &countingTransport{}
	cfg := NewOpenAICompletions("gpt-5")
	cfg.APIKey, cfg.BaseURL, cfg.HTTPClient = "k", srv.URL, &http.Client{Transport: rt}
	callOnce(t, cfg, nil, config.CallOptions{})
	cfg.Streaming = true
	callOnce(t, cfg, nil, config.CallOptions{})
	if rt.n.Load() != 2 {
		t.Fatalf("injected client saw %d requests, want 2", rt.n.Load())
	}
}

// TestInjectedHTTPClientResponses: plain and streaming Responses calls plus
// the Files client built from the same config use the injected client.
func TestInjectedHTTPClientResponses(t *testing.T) {
	srv := sseOrJSONServer(t, responsesBody(),
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"model\":\"m\",\"status\":\"completed\",\"output\":[]}}\n\n")
	rt := &countingTransport{}
	cfg := NewOpenAIResponses("gpt-5")
	cfg.APIKey, cfg.BaseURL, cfg.HTTPClient = "k", srv.URL, &http.Client{Transport: rt}
	client, err := cfg.Create()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatalf("responses: %v", err)
	}
	cfg.Streaming = true
	client, _ = cfg.Create()
	if _, err := client.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatalf("responses stream: %v", err)
	}
	if rt.n.Load() != 2 {
		t.Fatalf("injected client saw %d responses requests, want 2", rt.n.Load())
	}

	fc, err := cfg.CreateFilesClient()
	if err != nil {
		t.Fatal(err)
	}
	_ = fc.Delete(context.Background(), "file-1") // the answer is irrelevant, the route is
	if rt.n.Load() != 3 {
		t.Fatalf("files client bypassed the injected client (%d requests)", rt.n.Load())
	}
}

func TestInjectedHTTPClientChatFilesAndDecisions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	rt := &countingTransport{}
	hc := &http.Client{Transport: rt}

	cfg := NewOpenAICompletions("gpt-5")
	cfg.APIKey, cfg.BaseURL, cfg.HTTPClient = "k", srv.URL, hc
	fc, err := cfg.CreateFilesClient()
	if err != nil {
		t.Fatal(err)
	}
	if err := fc.Delete(context.Background(), "file-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if rt.n.Load() != 1 {
		t.Fatalf("chat config's files client bypassed the injected client (%d)", rt.n.Load())
	}

	dcfg := NewOpenAIDecisions()
	dcfg.APIKey, dcfg.BaseURL, dcfg.HTTPClient = "k", srv.URL, hc
	dcfg.QuestionInstructions = "Is it ok?"
	dc, err := dcfg.Create()
	if err != nil {
		t.Fatal(err)
	}
	schema := response.RawSchema(map[string]any{"type": "boolean"}, response.WithRawName("answer"))
	_, _ = dc.Call(context.Background(), nil, nil, config.CallOptions{ResponseSchema: schema})
	if rt.n.Load() != 2 {
		t.Fatalf("decisions client bypassed the injected client (%d)", rt.n.Load())
	}
}

// TestAPIErrorTypedOnEveryOpenAIClient: errors.As finds *config.APIError on
// Chat, Responses (plain and stream), Decisions and Files.
func TestAPIErrorTypedOnEveryOpenAIClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"type":"rate_limit_error","message":"slow down"}}`)
	}))
	t.Cleanup(srv.Close)

	check := func(name string, err error) {
		t.Helper()
		var apiErr *config.APIError
		if !errors.As(err, &apiErr) {
			t.Errorf("%s: want *config.APIError, got %T: %v", name, err, err)
			return
		}
		if apiErr.Provider != config.ProviderOpenAI || apiErr.StatusCode != 429 || !apiErr.Retryable || apiErr.Message != "slow down" {
			t.Errorf("%s: %+v", name, apiErr)
		}
		if !errors.Is(err, config.ErrRateLimited) {
			t.Errorf("%s: errors.Is(ErrRateLimited) failed", name)
		}
	}
	call := func(c config.LLMClient, opts config.CallOptions) error {
		_, err := c.Call(context.Background(), []events.Event{
			&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "go"}}}}, nil, opts)
		return err
	}

	chat := NewOpenAICompletions("gpt-5")
	chat.APIKey, chat.BaseURL, chat.MaxRetries = "k", srv.URL, 0
	c, _ := chat.Create()
	check("chat", call(c, config.CallOptions{}))
	chat.Streaming = true
	c, _ = chat.Create()
	check("chat stream", call(c, config.CallOptions{}))

	resp := NewOpenAIResponses("gpt-5")
	resp.APIKey, resp.BaseURL, resp.MaxRetries = "k", srv.URL, 0
	c, _ = resp.Create()
	check("responses", call(c, config.CallOptions{}))
	resp.Streaming = true
	c, _ = resp.Create()
	check("responses stream", call(c, config.CallOptions{}))

	dec := NewOpenAIDecisions()
	dec.APIKey, dec.BaseURL, dec.MaxRetries = "k", srv.URL, 0
	dec.QuestionInstructions = "Is it ok?"
	c, _ = dec.Create()
	check("decisions", call(c, config.CallOptions{
		ResponseSchema: response.RawSchema(map[string]any{"type": "boolean"}, response.WithRawName("answer"))}))

	fc, _ := chat.CreateFilesClient()
	err := fc.Delete(context.Background(), "file-1")
	check("files", err)
	if !errors.Is(err, ErrFilesAPI) {
		t.Errorf("files: ErrFilesAPI sentinel lost: %v", err)
	}
}

func TestResponsesDefaultQuery(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, responsesBody())
	}))
	t.Cleanup(srv.Close)
	cfg := NewOpenAIResponses("gpt-5")
	cfg.APIKey, cfg.BaseURL, cfg.DefaultQuery = "k", srv.URL, map[string]string{"api-version": "v9"}
	c, _ := cfg.Create()
	if _, err := c.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatal(err)
	}
	if gotQuery != "api-version=v9" {
		t.Fatalf("query = %q", gotQuery)
	}
}
