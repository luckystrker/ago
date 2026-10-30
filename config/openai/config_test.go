package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/response"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
)

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }
func b(v bool) *bool         { return &v }

// captured records the last request a test server saw.
type captured struct {
	header http.Header
	path   string
	body   []byte
}

// captureServer answers every request with status/body (contentType is
// "application/json" or "text/event-stream") and records the last request.
func captureServer(t *testing.T, contentType string, status int, body string) (*httptest.Server, *captured) {
	t.Helper()
	var got captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		got = captured{header: r.Header.Clone(), path: r.URL.Path, body: b}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func bodyJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("captured request is not JSON: %v", err)
	}
	return m
}

func wireTools(body map[string]any) []any {
	tools, ok := body["tools"].([]any)
	if !ok {
		return nil
	}
	return tools
}

// callOnce creates the client and performs one call over a fresh capturing
// conversation. It returns the response and every event the client published.
func callOnce(t *testing.T, cfg *OpenAICompletionsConfig, history []events.Event, opts config.CallOptions) (*events.ModelResponse, *[]events.Event) {
	t.Helper()
	ms := stream.NewMemoryStream()
	var sent []events.Event
	ms.Subscribe(func(ev events.Event) error {
		sent = append(sent, ev)
		return nil
	}, stream.SubscribeOptions{})
	conv := agocontext.New(ms)
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	resp, err := client.Call(context.Background(), history, conv, opts)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	return resp, &sent
}

// --- config contract -----------------------------------------------------------

func TestNewOpenAICompletionsDefaults(t *testing.T) {
	cfg := NewOpenAICompletions("gpt-4o")
	if cfg.Model() != "gpt-4o" {
		t.Fatalf("model: %q", cfg.Model())
	}
	if cfg.Provider() != config.ProviderOpenAI {
		t.Fatalf("provider: %q", cfg.Provider())
	}
	if cfg.MaxRetries != 4 {
		t.Fatalf("MaxRetries default must be 4, got %d", cfg.MaxRetries)
	}
	if cfg.Streaming {
		t.Fatal("Streaming default must be false")
	}
	if cfg.Timeout != 0 {
		t.Fatalf("Timeout default must be 0, got %s", cfg.Timeout)
	}
	if cfg.Temperature != nil || cfg.TopP != nil || cfg.MaxTokens != nil ||
		cfg.MaxCompletionTokens != nil || cfg.Seed != nil || cfg.ParallelToolCalls != nil {
		t.Fatalf("pointer defaults must be nil: %+v", cfg)
	}
	if cfg.ResponseFormat != nil || cfg.DefaultHeaders != nil || cfg.ExtraBody != nil {
		t.Fatalf("map defaults must be nil: %+v", cfg)
	}
	if cfg.Stop != nil || cfg.User != "" || cfg.Organization != "" || cfg.Project != "" {
		t.Fatalf("string/slice defaults must be empty: %+v", cfg)
	}
	if cfg.BaseURL != "" {
		t.Fatalf("BaseURL default must be empty (DefaultBaseURL applies at Create), got %q", cfg.BaseURL)
	}

	// The config satisfies the ModelConfig contract.
	var mc config.ModelConfig = cfg
	if mc.Model() != "gpt-4o" || mc.Provider() != config.ProviderOpenAI {
		t.Fatalf("ModelConfig contract mismatch: %v %v", mc.Model(), mc.Provider())
	}
}

func TestDefaultBaseURL(t *testing.T) {
	if DefaultBaseURL != "https://api.openai.com/v1" {
		t.Fatalf("DefaultBaseURL: %q", DefaultBaseURL)
	}
}

func TestCreateMissingKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	cfg := NewOpenAICompletions("m")
	_, err := cfg.Create()
	if !errors.Is(err, config.ErrNotProvided) {
		t.Fatalf("want ErrNotProvided, got %v", err)
	}
}

func TestCreateMissingModel(t *testing.T) {
	cfg := NewOpenAICompletions("")
	cfg.APIKey = "k"
	_, err := cfg.Create()
	if !errors.Is(err, config.ErrNotProvided) {
		t.Fatalf("want ErrNotProvided, got %v", err)
	}
}

func TestCreateEnvKeyFallback(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "env-key")
	srv, captured := captureServer(t, "application/json", 200,
		`{"id":"cmpl-1","model":"m","choices":[{"finish_reason":"stop","message":{"content":"ok"}}]}`)
	cfg := NewOpenAICompletions("m")
	cfg.BaseURL = srv.URL
	cfg.MaxRetries = 0

	if _, err := cfg.Create(); err != nil {
		t.Fatalf("env key must satisfy Create: %v", err)
	}
	if _, _ = callOnce(t, cfg, nil, config.CallOptions{}); len(captured.body) == 0 {
		t.Fatal("no request reached the server")
	}
	if auth := captured.header.Get("Authorization"); auth != "Bearer env-key" {
		t.Fatalf("env key must authenticate the request, got %q", auth)
	}
}

// --- non-streaming call -----------------------------------------------------------

func TestCallTextCompletion(t *testing.T) {
	srv, captured := captureServer(t, "application/json", 200, `{
	  "id": "cmpl-1",
	  "model": "gpt-test-actual",
	  "choices": [{"finish_reason": "stop", "message": {"content": "hello there"}}],
	  "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	}`)
	cfg := NewOpenAICompletions("gpt-test")
	cfg.APIKey = "test-key"
	cfg.BaseURL = srv.URL
	cfg.MaxRetries = 0
	cfg.Temperature = f64(0.5)
	cfg.TopP = f64(0.9)
	cfg.MaxTokens = i64(100)
	cfg.MaxCompletionTokens = i64(200)
	cfg.Seed = i64(42)
	cfg.User = "u-1"
	cfg.Stop = []string{"END"}
	cfg.ParallelToolCalls = b(false)
	cfg.Organization = "org-1"
	cfg.Project = "proj-1"
	cfg.DefaultHeaders = map[string]string{"X-Custom": "1"}
	cfg.ExtraBody = map[string]any{"metadata": map[string]any{"k": "v"}}

	req := &events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "hi"}}}
	resp, sent := callOnce(t, cfg, []events.Event{req}, config.CallOptions{})

	// Request wire shape.
	c := *captured
	if c.path != "/chat/completions" {
		t.Fatalf("request path: %q", c.path)
	}
	if auth := c.header.Get("Authorization"); auth != "Bearer test-key" {
		t.Fatalf("authorization: %q", auth)
	}
	if org := c.header.Get("OpenAI-Organization"); org != "org-1" {
		t.Fatalf("organization header: %q", org)
	}
	if proj := c.header.Get("OpenAI-Project"); proj != "proj-1" {
		t.Fatalf("project header: %q", proj)
	}
	if c.header.Get("X-Custom") != "1" {
		t.Fatalf("default headers not sent: %v", c.header)
	}

	body := bodyJSON(t, c.body)
	if body["model"] != "gpt-test" {
		t.Fatalf("model not sent: %v", body["model"])
	}
	if body["temperature"] != float64(0.5) || body["top_p"] != float64(0.9) || body["max_tokens"] != float64(100) {
		t.Fatalf("typed sampling params not sent: %+v", body)
	}
	if body["max_completion_tokens"] != float64(200) || body["seed"] != float64(42) || body["user"] != "u-1" {
		t.Fatalf("extra-carried params not sent: %+v", body)
	}
	if body["parallel_tool_calls"] != false {
		t.Fatalf("parallel_tool_calls not sent: %+v", body)
	}
	if stop, ok := body["stop"].([]any); !ok || len(stop) != 1 || stop[0] != "END" {
		t.Fatalf("stop not sent: %+v", body["stop"])
	}
	if body["metadata"].(map[string]any)["k"] != "v" {
		t.Fatalf("extra body not merged: %+v", body)
	}
	if _, ok := body["stream"]; ok {
		t.Fatal("non-streaming request must not carry stream=true")
	}
	msgs := body["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "" {
		t.Fatalf("empty prompt must yield an empty system message: %+v", first)
	}
	user := msgs[1].(map[string]any)
	if user["role"] != "user" || user["content"] != "hi" {
		t.Fatalf("user turn not sent: %+v", user)
	}

	// Response normalization.
	if resp.Content() != "hello there" {
		t.Fatalf("content: %q", resp.Content())
	}
	if resp.FinishReason != "stop" || resp.ResponseID != "cmpl-1" || resp.Model != "gpt-test-actual" {
		t.Fatalf("tracing fields: %+v", resp)
	}
	if resp.Provider != "openai" {
		t.Fatalf("provider: %q", resp.Provider)
	}
	if resp.Usage.PromptTokens == nil || *resp.Usage.PromptTokens != 10 ||
		resp.Usage.CompletionTokens == nil || *resp.Usage.CompletionTokens != 5 ||
		resp.Usage.TotalTokens == nil || *resp.Usage.TotalTokens != 15 {
		t.Fatalf("usage not normalized through the engine: %+v", resp.Usage)
	}
	if len(*sent) != 1 {
		t.Fatalf("want only the final ModelMessage on the stream, got %d events", len(*sent))
	}
	if msg, ok := events.AsValue((*sent)[0]).(events.ModelMessage); !ok || msg.Content != "hello there" {
		t.Fatalf("ModelMessage not published: %v", *sent)
	}
}

func TestCallUsageNormalizationDetails(t *testing.T) {
	srv, _ := captureServer(t, "application/json", 200, `{
	  "id": "cmpl-u",
	  "model": "m",
	  "choices": [{"finish_reason": "stop", "message": {"content": "ok"}}],
	  "usage": {
	    "prompt_tokens": 100, "completion_tokens": 20.6, "total_tokens": 120,
	    "prompt_tokens_details": {"cached_tokens": 80, "cache_write_tokens": 7},
	    "completion_tokens_details": {"reasoning_tokens": 9}
	  }
	}`)
	cfg := NewOpenAICompletions("m")
	cfg.APIKey = "k"
	cfg.BaseURL = srv.URL
	cfg.MaxRetries = 0

	resp, _ := callOnce(t, cfg, nil, config.CallOptions{})
	u := resp.Usage
	if u.PromptTokens == nil || *u.PromptTokens != 100 {
		t.Fatalf("prompt tokens: %+v", u.PromptTokens)
	}
	if u.CompletionTokens == nil || *u.CompletionTokens != 21 {
		t.Fatalf("fractional completion tokens must round: %+v", u.CompletionTokens)
	}
	if u.TotalTokens == nil || *u.TotalTokens != 120 {
		t.Fatalf("total tokens: %+v", u.TotalTokens)
	}
	if u.CacheReadInputTokens == nil || *u.CacheReadInputTokens != 80 {
		t.Fatalf("cached_tokens must map to CacheReadInputTokens: %+v", u.CacheReadInputTokens)
	}
	if u.CacheCreationInputTokens == nil || *u.CacheCreationInputTokens != 7 {
		t.Fatalf("cache_write_tokens must map to CacheCreationInputTokens: %+v", u.CacheCreationInputTokens)
	}
	if u.ThinkingTokens == nil || *u.ThinkingTokens != 9 {
		t.Fatalf("reasoning_tokens must map to ThinkingTokens: %+v", u.ThinkingTokens)
	}
}

func TestCallToolRoundTrip(t *testing.T) {
	var calls atomic.Int32
	var secondBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			fmt.Fprint(w, `{
			  "id": "cmpl-t1",
			  "model": "m",
			  "choices": [{"finish_reason": "tool_calls", "message": {
			    "content": "",
			    "tool_calls": [{"id": "call_1", "type": "function",
			      "function": {"name": "get_weather", "arguments": "{\"city\":\"SF\"}"}}]
			  }}],
			  "usage": {"prompt_tokens": 9, "completion_tokens": 4, "total_tokens": 13}
			}`)
			return
		}
		secondBody = b
		fmt.Fprint(w, `{"id":"cmpl-t2","model":"m","choices":[{"finish_reason":"stop","message":{"content":"72F and sunny"}}]}`)
	}))
	t.Cleanup(srv.Close)

	cfg := NewOpenAICompletions("m")
	cfg.APIKey = "k"
	cfg.BaseURL = srv.URL
	cfg.MaxRetries = 0
	cfg.Temperature = f64(0.2)
	convMS := stream.NewMemoryStream()
	conv := agocontext.New(convMS)
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Turn 1: the model asks for the weather tool.
	req := &events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "weather in SF?"}}}
	resp1, err := client.Call(context.Background(), []events.Event{req}, conv, config.CallOptions{
		Tools: []any{tools.Schema{
			Name:        "get_weather",
			Description: "current weather",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if len(resp1.ToolCalls.Calls) != 1 {
		t.Fatalf("want 1 tool call, got %+v", resp1.ToolCalls)
	}
	call1 := resp1.ToolCalls.Calls[0]
	if call1.ID != "call_1" || call1.Name != "get_weather" || call1.Arguments != `{"city":"SF"}` {
		t.Fatalf("bad tool call: %+v", call1)
	}

	// Turn 2: the executor answered; ToolResultsEvent drives the next call.
	results := events.ToolResultsEvent{Results: []events.ToolResultEvent{
		events.FromResultCall(call1, events.NewToolResult(false, "72F")),
	}}
	history2 := []events.Event{req, resp1, &results}
	resp2, err := client.Call(context.Background(), history2, conv, config.CallOptions{
		Tools: []any{tools.Schema{Name: "get_weather", Parameters: map[string]any{
			"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}},
		}}},
	})
	if err != nil {
		t.Fatalf("call 2: %v", err)
	}
	if resp2.Content() != "72F and sunny" {
		t.Fatalf("call 2 content: %q", resp2.Content())
	}

	// The second request replayed the assistant turn and the tool answers.
	body := bodyJSON(t, secondBody)
	msgs := body["messages"].([]any)
	if len(msgs) < 4 {
		t.Fatalf("want system+user+assistant+tool, got %d messages", len(msgs))
	}
	assistant := msgs[2].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("message 2 must be the assistant replay: %+v", assistant)
	}
	replayed := assistant["tool_calls"].([]any)[0].(map[string]any)
	if replayed["id"] != "call_1" {
		t.Fatalf("tool call not replayed: %+v", replayed)
	}
	toolMsg := msgs[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" || toolMsg["content"] != "72F" {
		t.Fatalf("tool result must travel as a role=tool message: %+v", toolMsg)
	}
	toolsWire := wireTools(body)
	if len(toolsWire) != 1 {
		t.Fatalf("tool schema not sent on the follow-up call: %+v", body["tools"])
	}
}

func TestCallResponseSchemaStrictFormat(t *testing.T) {
	srv, captured := captureServer(t, "application/json", 200,
		`{"id":"cmpl-s","model":"m","choices":[{"finish_reason":"stop","message":{"content":"{}"}}]}`)
	cfg := NewOpenAICompletions("m")
	cfg.APIKey = "k"
	cfg.BaseURL = srv.URL
	cfg.MaxRetries = 0

	schema := response.RawSchema(
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "title": "Name"},
				"note": map[string]any{"type": "string", "default": ""},
			},
		},
		response.WithRawName("Out"),
		response.WithRawDescription("structured answer"),
	)
	if _, _ = callOnce(t, cfg, nil, config.CallOptions{ResponseSchema: schema}); len(captured.body) == 0 {
		t.Fatal("no request reached the server")
	}

	body := bodyJSON(t, captured.body)
	rf, ok := body["response_format"].(map[string]any)
	if !ok || rf["type"] != "json_schema" {
		t.Fatalf("response_format not derived from the schema: %+v", body["response_format"])
	}
	inner := rf["json_schema"].(map[string]any)
	if inner["name"] != "Out" || inner["strict"] != true || inner["description"] != "structured answer" {
		t.Fatalf("bad json_schema envelope: %+v", inner)
	}
	st := inner["schema"].(map[string]any)
	if st["additionalProperties"] != false {
		t.Fatal("schema must be strictified on the wire")
	}
	props := st["properties"].(map[string]any)
	if _, ok := props["name"].(map[string]any)["title"]; ok {
		t.Fatal("title must not reach the wire")
	}
	if _, ok := props["note"].(map[string]any)["default"]; ok {
		t.Fatal("default must not reach the wire")
	}
	req, ok := st["required"].([]any)
	if !ok || len(req) != 2 {
		t.Fatalf("all properties must be required: %+v", st["required"])
	}
	names := []string{req[0].(string), req[1].(string)}
	sort.Strings(names)
	if names[0] != "name" || names[1] != "note" {
		t.Fatalf("bad required list: %v", names)
	}
}

func TestCallAPIError(t *testing.T) {
	srv, _ := captureServer(t, "application/json", 401, `{"error":{"message":"bad key"}}`)
	cfg := NewOpenAICompletions("m")
	cfg.APIKey = "k"
	cfg.BaseURL = srv.URL
	cfg.MaxRetries = 0
	ms := stream.NewMemoryStream()
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = client.Call(context.Background(), nil, agocontext.New(ms), config.CallOptions{})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want an error carrying the status, got %v", err)
	}
}

// --- streaming call -----------------------------------------------------------------

const streamBody = "data: {\"id\":\"chatcmpl-s1\",\"model\":\"gpt-stream-actual\",\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"chatcmpl-s1\",\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-s1\",\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-s1\",\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-s1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\"}}]}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-s1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"SF\\\"}\"}}]}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-s1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
	"data: {\"id\":\"chatcmpl-s1\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\n" +
	"data: [DONE]\n\n"

func TestCallStreamingWithToolDeltas(t *testing.T) {
	srv, captured := captureServer(t, "text/event-stream", 200, streamBody)
	cfg := NewOpenAICompletions("gpt-test")
	cfg.APIKey = "k"
	cfg.BaseURL = srv.URL
	cfg.MaxRetries = 0
	cfg.Streaming = true

	ms := stream.NewMemoryStream()
	var sent []events.Event
	ms.Subscribe(func(ev events.Event) error {
		sent = append(sent, ev)
		return nil
	}, stream.SubscribeOptions{})
	conv := agocontext.New(ms)
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	resp, err := client.Call(context.Background(), nil, conv, config.CallOptions{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}

	// The request asked for a stream with usage.
	body := bodyJSON(t, captured.body)
	if body["stream"] != true {
		t.Fatal("streaming request must carry stream=true")
	}
	so, ok := body["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Fatalf("stream_options.include_usage must be sent: %+v", body["stream_options"])
	}

	// The synthesized response carries the merged content and tool call.
	if resp.Content() != "Hello" {
		t.Fatalf("stream content must accumulate, got %q", resp.Content())
	}
	if len(resp.ToolCalls.Calls) != 1 {
		t.Fatalf("tool-call deltas must merge to one call, got %d", len(resp.ToolCalls.Calls))
	}
	call := resp.ToolCalls.Calls[0]
	if call.ID != "call_1" || call.Name != "get_weather" || call.Arguments != `{"city":"SF"}` {
		t.Fatalf("bad merged call: %+v", call)
	}
	if resp.FinishReason != "tool_calls" || resp.ResponseID != "chatcmpl-s1" || resp.Model != "gpt-stream-actual" {
		t.Fatalf("tracing fields: %+v", resp)
	}
	if resp.Provider != "openai" {
		t.Fatalf("provider: %q", resp.Provider)
	}
	if resp.Usage.PromptTokens == nil || *resp.Usage.PromptTokens != 7 ||
		resp.Usage.CompletionTokens == nil || *resp.Usage.CompletionTokens != 3 {
		t.Fatalf("usage from the final chunk: %+v", resp.Usage)
	}

	// Progress events reached the conversation, in order.
	var got []string
	for _, ev := range sent {
		switch e := events.AsValue(ev).(type) {
		case events.ModelMessageChunk:
			got = append(got, "chunk:"+e.Content)
		case events.ModelReasoning:
			got = append(got, "reasoning:"+e.Content)
		case events.ModelMessage:
			got = append(got, "message:"+e.Content)
		}
	}
	want := []string{"chunk:Hel", "chunk:lo", "reasoning:thinking", "message:Hello"}
	if len(got) != len(want) {
		t.Fatalf("want events %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d: want %q, got %q", i, want[i], got[i])
		}
	}
}

func TestCallStreamingExplicitStreamOptionsWin(t *testing.T) {
	srv, captured := captureServer(t, "text/event-stream", 200, "data: [DONE]\n\n")
	cfg := NewOpenAICompletions("m")
	cfg.APIKey = "k"
	cfg.BaseURL = srv.URL
	cfg.MaxRetries = 0
	cfg.Streaming = true
	cfg.ExtraBody = map[string]any{"stream_options": map[string]any{"include_usage": false}}

	if _, _ = callOnce(t, cfg, nil, config.CallOptions{}); len(captured.body) == 0 {
		t.Fatal("no request reached the server")
	}
	body := bodyJSON(t, captured.body)
	so := body["stream_options"].(map[string]any)
	if so["include_usage"] != false {
		t.Fatalf("ExtraBody stream_options must win: %+v", so)
	}
}

// --- Clone ---------------------------------------------------------------------------

func TestCloneOverrides(t *testing.T) {
	cfg := NewOpenAICompletions("m1")
	cfg.APIKey = "k1"
	cfg.Temperature = f64(0.1)
	cfg.MaxRetries = 2
	cfg.Stop = []string{"A"}

	cloned, err := cfg.Clone(map[string]any{
		"Model":        "m2",
		"temperature":  f64(0.9),
		"Streaming":    true,
		"max_retries":  7,
		"Organization": "org-9",
	})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	c2, ok := cloned.(*OpenAICompletionsConfig)
	if !ok {
		t.Fatalf("Clone must return the same config type, got %T", cloned)
	}
	if c2.Model() != "m2" {
		t.Fatalf("model override: %q", c2.Model())
	}
	if c2.Temperature == nil || *c2.Temperature != 0.9 {
		t.Fatalf("temperature override: %+v", c2.Temperature)
	}
	if !c2.Streaming || c2.MaxRetries != 7 || c2.Organization != "org-9" {
		t.Fatalf("overrides not applied: %+v", c2)
	}
	// Untouched fields carry over.
	if c2.APIKey != "k1" || len(c2.Stop) != 1 || c2.Stop[0] != "A" {
		t.Fatalf("carried fields lost: %+v", c2)
	}
	// The original is untouched.
	if cfg.Model() != "m1" || *cfg.Temperature != 0.1 || cfg.MaxRetries != 2 {
		t.Fatalf("original config mutated: %+v", cfg)
	}
	// The clone still satisfies the contract.
	var mc config.ModelConfig = cloned
	if mc.Provider() != config.ProviderOpenAI {
		t.Fatalf("clone provider: %v", mc.Provider())
	}
	client, err := mc.Create()
	if err != nil {
		t.Fatalf("clone create: %v", err)
	}
	if client == nil {
		t.Fatal("clone produced no client")
	}
}

func TestCloneErrors(t *testing.T) {
	cfg := NewOpenAICompletions("m")
	cases := []struct {
		name      string
		overrides map[string]any
	}{
		{"unknown key", map[string]any{"nope": 1}},
		{"string for *float64", map[string]any{"temperature": 0.5}},
		{"float64 for *int64", map[string]any{"seed": 1.0}},
		{"string for bool", map[string]any{"streaming": "yes"}},
		{"string for duration", map[string]any{"timeout": "30s"}},
		{"float64 for int", map[string]any{"max_retries": 2.0}},
		{"string for []string", map[string]any{"stop": "END"}},
		{"string for map", map[string]any{"extra_body": "{}"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := cfg.Clone(tc.overrides)
			if err == nil {
				t.Fatalf("want an error, got %v", c)
			}
			if c != nil {
				t.Fatalf("failed clone must return no config, got %v", c)
			}
		})
	}
	// Timeout keeps its Go type.
	if _, err := cfg.Clone(map[string]any{"timeout": 30 * time.Second}); err != nil {
		t.Fatalf("time.Duration override must be accepted: %v", err)
	}
}
