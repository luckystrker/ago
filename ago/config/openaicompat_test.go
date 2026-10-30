package config

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
	"testing"

	agocontext "ago/context"
	"ago/events"
	"ago/response"
	"ago/stream"
	"ago/tools"
)

func f64ptr(v float64) *float64 { return &v }
func i64ptr(v int64) *int64     { return &v }
func boolptr(v bool) *bool      { return &v }
func strptr(v string) *string   { return &v }
func intPtr(v int) *int         { return &v }

// newCaptureConv returns a conversation context and a pointer to every event
// sent through it (chunks included — the subscription sees transient events).
func newCaptureConv() (*agocontext.Context, *[]events.Event) {
	ms := stream.NewMemoryStream()
	var got []events.Event
	ms.Subscribe(func(ev events.Event) error {
		got = append(got, ev)
		return nil
	}, stream.SubscribeOptions{})
	return agocontext.New(ms), &got
}

// captureServer records one request and answers body with the given status.
func captureServer(t *testing.T, status int, body string) (*httptest.Server, *[]byte) {
	t.Helper()
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request: %v", err)
		}
		captured = b
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &captured
}

func requestJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("captured request is not JSON: %v", err)
	}
	return m
}

// --- BuildMessages -------------------------------------------------------------

func TestBuildMessagesSystemPrompt(t *testing.T) {
	msgs, err := BuildMessages([]string{"you are", "helpful"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	if msgs[0]["role"] != "system" || msgs[0]["content"] != "you are\nhelpful" {
		t.Fatalf("bad system message: %+v", msgs[0])
	}
}

func TestBuildMessagesUserRequest(t *testing.T) {
	req := events.ModelRequest{Parts: []events.Input{
		&events.TextInput{Content: "hello"},
	}}
	msgs, err := BuildMessages(nil, []events.Event{&req})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want system + user, got %d", len(msgs))
	}
	// A single text part collapses to a plain string (Python parity).
	if msgs[1]["role"] != "user" || msgs[1]["content"] != "hello" {
		t.Fatalf("bad user message: %+v", msgs[1])
	}
}

func TestBuildMessagesMultiplePartsBecomesArray(t *testing.T) {
	req := events.ModelRequest{Parts: []events.Input{
		&events.TextInput{Content: "look"},
		&events.DataInput{Data: map[string]any{"k": 1}},
	}}
	msgs, err := BuildMessages(nil, []events.Event{&req})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	parts, ok := msgs[1]["content"].([]map[string]any)
	if !ok {
		t.Fatalf("want array content, got %T", msgs[1]["content"])
	}
	if len(parts) != 2 {
		t.Fatalf("want 2 parts, got %d", len(parts))
	}
	text := parts[0]
	if text["type"] != "text" || text["text"] != "look" {
		t.Fatalf("bad text part: %+v", text)
	}
	data := parts[1]
	if data["type"] != "text" || data["text"] != `{"k":1}` {
		t.Fatalf("DataInput must travel as JSON text, got %+v", data)
	}
}

func TestBuildMessagesImageParts(t *testing.T) {
	bin := events.BinaryInput{Data: []byte{1, 2, 3}, MediaType: "image/png", Kind: events.BinaryImage}
	url := events.UrlInput{URL: "https://example.com/cat.png", Kind: events.BinaryImage}
	req := events.ModelRequest{Parts: []events.Input{&bin, &url}}
	msgs, err := BuildMessages(nil, []events.Event{&req})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	parts := msgs[1]["content"].([]map[string]any)
	b64 := parts[0]["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(b64, "data:image/png;base64,") {
		t.Fatalf("binary image must be a data URL, got %q", b64)
	}
	if parts[1]["image_url"].(map[string]any)["url"] != "https://example.com/cat.png" {
		t.Fatalf("url image must pass through: %+v", parts[1])
	}
}

func TestBuildMessagesUnsupportedInputs(t *testing.T) {
	cases := []struct {
		name string
		part events.Input
	}{
		{"binary audio", &events.BinaryInput{Data: []byte{1}, MediaType: "audio/wav", Kind: events.BinaryAudio}},
		{"url video", &events.UrlInput{URL: "https://x/v.mp4", Kind: events.BinaryVideo}},
		{"file id", &events.FileIdInput{FileID: "file-1"}},
		// The Input interface is sealed to the events package, so an unmapped
		// foreign part cannot exist; an unclassified binary input hits the
		// same fallthrough.
		{"unclassified binary", &events.BinaryInput{Data: []byte{1}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := events.ModelRequest{Parts: []events.Input{tc.part}}
			_, err := BuildMessages(nil, []events.Event{&req})
			if !errors.Is(err, ErrUnsupportedInput) {
				t.Fatalf("want ErrUnsupportedInput, got %v", err)
			}
		})
	}
}

func TestBuildMessagesAssistantReplay(t *testing.T) {
	resp := events.ModelResponse{
		Message: &events.ModelMessage{Content: "let me check"},
		ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "call_1", Name: "lookup", Arguments: `{"q":"x"}`},
		}},
	}
	msgs, err := BuildMessages(nil, []events.Event{&resp})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := msgs[1]
	if m["role"] != "assistant" || m["content"] != "let me check" {
		t.Fatalf("bad assistant message: %+v", m)
	}
	calls := m["tool_calls"].([]map[string]any)
	if len(calls) != 1 {
		t.Fatalf("want 1 replayed call, got %d", len(calls))
	}
	if calls[0]["id"] != "call_1" || calls[0]["type"] != "function" {
		t.Fatalf("bad replayed call: %+v", calls[0])
	}
	fn := calls[0]["function"].(map[string]any)
	if fn["name"] != "lookup" || fn["arguments"] != `{"q":"x"}` {
		t.Fatalf("bad replayed function: %+v", fn)
	}
}

func TestBuildMessagesAssistantWithoutContent(t *testing.T) {
	resp := events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
		{ID: "call_1", Name: "do", Arguments: "{}"},
	}}}
	msgs, err := BuildMessages(nil, []events.Event{&resp})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := msgs[1]
	if _, ok := m["content"]; ok {
		t.Fatalf("content must be omitted when the response carried none: %+v", m)
	}
}

func TestBuildMessagesToolResults(t *testing.T) {
	res := events.ToolResultsEvent{Results: []events.ToolResultEvent{
		{ParentID: "call_1", Result: events.NewToolResult(false, "42 degrees")},
		{ParentID: "call_2", Result: events.NewToolResult(false, "line one", map[string]any{"n": 2})},
	}}
	msgs, err := BuildMessages(nil, []events.Event{&res})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 3 { // system + two tool messages
		t.Fatalf("want 3 messages, got %d", len(msgs))
	}
	first := msgs[1]
	if first["role"] != "tool" || first["tool_call_id"] != "call_1" || first["content"] != "42 degrees" {
		t.Fatalf("bad tool message: %+v", first)
	}
	second := msgs[2]
	// Multiple parts concatenate; DataInput travels as JSON.
	if second["content"] != "line one\n{\"n\":2}" {
		t.Fatalf("bad concatenated tool content: %+v", second)
	}
}

func TestBuildMessagesToolResultUnsupportedPart(t *testing.T) {
	res := events.ToolResultsEvent{Results: []events.ToolResultEvent{
		{ParentID: "call_1", Result: events.ToolResult{Parts: []events.Input{
			&events.BinaryInput{Data: []byte{1}, Kind: events.BinaryImage, MediaType: "image/png"},
		}}},
	}}
	_, err := BuildMessages(nil, []events.Event{&res})
	if !errors.Is(err, ErrUnsupportedInput) {
		t.Fatalf("want ErrUnsupportedInput, got %v", err)
	}
}

func TestBuildMessagesCompactionSummary(t *testing.T) {
	sum := events.CompactionSummary{Content: "earlier stuff"}
	msgs, err := BuildMessages(nil, []events.Event{&sum})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msgs[1]["role"] != "user" || msgs[1]["content"] != "[Summary of earlier conversation]\nearlier stuff" {
		t.Fatalf("bad summary message: %+v", msgs[1])
	}
}

func TestBuildMessagesSkipsNonTranscriptEvents(t *testing.T) {
	evs := []events.Event{
		&events.UsageEvent{Kind: events.UsageKindModelCall},
		&events.ModelMessageChunk{Content: "partial"},
		&events.ToolCallEvent{ID: "call_1", Name: "x", Arguments: "{}"}, // standalone: replayed via ModelResponse only
		&events.HumanMessage{Content: "hi"},
		&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "real"}}},
	}
	msgs, err := BuildMessages([]string{"sys"}, evs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 2 { // system + the one real user turn; everything else skipped
		t.Fatalf("want 2 messages, got %d: %+v", len(msgs), msgs)
	}
}

// --- ToolToAPI / EnsureObjectSchema ---------------------------------------------

func TestToolToAPI(t *testing.T) {
	schema := tools.Schema{
		Name:        "get_weather",
		Description: "weather lookup",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
	}
	got, err := ToolToAPI(schema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["type"] != "function" {
		t.Fatalf("bad tool type: %+v", got)
	}
	fn := got["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["description"] != "weather lookup" {
		t.Fatalf("bad function envelope: %+v", fn)
	}
	params := fn["parameters"].(map[string]any)
	if params["additionalProperties"] != false {
		t.Fatalf("parameters must gain additionalProperties=false: %+v", params)
	}

	// Pointer form is accepted too.
	if _, err := ToolToAPI(&schema); err != nil {
		t.Fatalf("pointer schema must be accepted: %v", err)
	}
}

func TestToolToAPIOmitDescription(t *testing.T) {
	// An empty description is dropped rather than sent as "" (Python parity).
	schema := tools.Schema{Name: "no_desc"}
	got, err := ToolToAPI(schema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fn := got["function"].(map[string]any)
	if _, ok := fn["description"]; ok {
		t.Fatalf("empty description must be omitted, got %+v", fn)
	}
}

func TestToolToAPIUnsupported(t *testing.T) {
	deferLoader := tools.Schema{Name: "lazy", DeferLoading: true}
	if _, err := ToolToAPI(deferLoader); !errors.Is(err, ErrUnsupportedTool) {
		t.Fatalf("defer_loading must fail fast, got %v", err)
	}
	hosted := hostedSchema{toolType: "web_search"}
	if _, err := ToolToAPI(hosted); !errors.Is(err, ErrUnsupportedTool) {
		t.Fatalf("hosted tools must fail fast, got %v", err)
	}
	// A real carrier fails the same way: chat completions has no hosted mapping
	// (the Responses API, which this port does not carry, is their only host).
	if _, err := ToolToAPI(tools.FileSearchToolSchema{StoreNames: []string{"s"}}); !errors.Is(err, ErrUnsupportedTool) {
		t.Fatalf("real hosted carriers must fail fast, got %v", err)
	}
	if _, err := ToolToAPI(42); !errors.Is(err, ErrUnsupportedTool) {
		t.Fatalf("unsupported schema kinds must fail fast, got %v", err)
	}
	if _, err := ToolToAPI((*tools.Schema)(nil)); !errors.Is(err, ErrUnsupportedTool) {
		t.Fatalf("nil schema must fail fast, got %v", err)
	}
}

type hostedSchema struct{ toolType string }

func (h hostedSchema) SchemaType() string { return h.toolType }

func TestEnsureObjectSchema(t *testing.T) {
	if got := EnsureObjectSchema(nil); got["type"] != "object" || got["additionalProperties"] != false {
		t.Fatalf("nil params must yield the empty object schema: %+v", got)
	}
	in := map[string]any{"type": "string", "properties": map[string]any{"a": map[string]any{"type": "number"}}, "additionalProperties": true}
	got := EnsureObjectSchema(in)
	if got["type"] != "object" {
		t.Fatalf("type must be forced to object, got %+v", got["type"])
	}
	if got["additionalProperties"] != true {
		t.Fatalf("explicit additionalProperties must survive, got %+v", got["additionalProperties"])
	}
	if _, ok := got["properties"].(map[string]any); !ok {
		t.Fatalf("properties must survive: %+v", got)
	}
	// The input map must not be mutated.
	if in["type"] != "string" {
		t.Fatalf("input map was mutated: %+v", in)
	}
}

// --- NormalizeUsage -------------------------------------------------------------

func TestNormalizeUsage(t *testing.T) {
	usage := NormalizeUsage(map[string]any{
		"prompt_tokens":     float64(10),
		"completion_tokens": float64(5.4), // fractional → rounded (D-006)
		"total_tokens":      float64(15),
		"prompt_tokens_details": map[string]any{
			"cached_tokens":      float64(4),
			"cache_write_tokens": float64(1),
		},
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": float64(2),
		},
	})
	if usage.PromptTokens == nil || *usage.PromptTokens != 10 {
		t.Fatalf("bad prompt tokens: %+v", usage.PromptTokens)
	}
	if usage.CompletionTokens == nil || *usage.CompletionTokens != 5 {
		t.Fatalf("completion tokens must round, got %+v", usage.CompletionTokens)
	}
	if usage.TotalTokens == nil || *usage.TotalTokens != 15 {
		t.Fatalf("bad total tokens: %+v", usage.TotalTokens)
	}
	if usage.CacheReadInputTokens == nil || *usage.CacheReadInputTokens != 4 {
		t.Fatalf("cached_tokens must map to CacheReadInputTokens: %+v", usage.CacheReadInputTokens)
	}
	if usage.CacheCreationInputTokens == nil || *usage.CacheCreationInputTokens != 1 {
		t.Fatalf("cache_write_tokens must map to CacheCreationInputTokens: %+v", usage.CacheCreationInputTokens)
	}
	if usage.ThinkingTokens == nil || *usage.ThinkingTokens != 2 {
		t.Fatalf("reasoning_tokens must map to ThinkingTokens: %+v", usage.ThinkingTokens)
	}
}

func TestNormalizeUsageEmpty(t *testing.T) {
	usage := NormalizeUsage(map[string]any{})
	if usage != (events.Usage{}) {
		t.Fatalf("empty usage must normalize to zero Usage, got %+v", usage)
	}
}

func TestNormalizeUsageJSONNumber(t *testing.T) {
	usage := NormalizeUsage(map[string]any{"prompt_tokens": json.Number("12")})
	if usage.PromptTokens == nil || *usage.PromptTokens != 12 {
		t.Fatalf("json.Number values must be read: %+v", usage.PromptTokens)
	}
}

// --- RunChatCompat: validation ---------------------------------------------------

func TestRunChatCompatNotProvided(t *testing.T) {
	cases := []CompatParams{
		{BaseURL: "http://x", APIKey: "k"}, // no model
		{BaseURL: "http://x", Model: "m"},  // no key
		{APIKey: "k", Model: "m"},          // no base url
	}
	for i, p := range cases {
		_, err := RunChatCompat(context.Background(), p, nil, nil, CallOptions{})
		if !errors.Is(err, ErrNotProvided) {
			t.Fatalf("case %d: want ErrNotProvided, got %v", i, err)
		}
	}
}

func TestRunChatCompatUnsupportedInputFailsFast(t *testing.T) {
	srv, captured := captureServer(t, 200, `{}`)
	p := CompatParams{APIKey: "k", BaseURL: srv.URL, Model: "m"}
	req := events.ModelRequest{Parts: []events.Input{&events.FileIdInput{FileID: "f-1"}}}
	_, err := RunChatCompat(context.Background(), p, []events.Event{&req}, nil, CallOptions{})
	if !errors.Is(err, ErrUnsupportedInput) {
		t.Fatalf("want ErrUnsupportedInput, got %v", err)
	}
	if len(*captured) != 0 {
		t.Fatal("no request may reach the API on an unmappable history")
	}
}

// --- RunChatCompat: non-streaming -------------------------------------------------

const completionBody = `{
  "id": "cmpl-1",
  "model": "gpt-test-actual",
  "choices": [{
    "finish_reason": "tool_calls",
    "message": {
      "content": "checking the weather",
      "tool_calls": [
        {"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"SF\"}"}}
      ]
    }
  }],
  "usage": {
    "prompt_tokens": 10, "completion_tokens": 5.4, "total_tokens": 15,
    "prompt_tokens_details": {"cached_tokens": 4},
    "completion_tokens_details": {"reasoning_tokens": 2}
  }
}`

func TestRunChatCompatOnce(t *testing.T) {
	srv, captured := captureServer(t, 200, completionBody)
	conv, sent := newCaptureConv()

	p := CompatParams{
		APIKey:            "test-key",
		BaseURL:           srv.URL,
		Path:              "/v1/chat/completions",
		Model:             "gpt-test",
		Provider:          ProviderOpenAI,
		Temperature:       f64ptr(0.5),
		TopP:              f64ptr(0.9),
		MaxTokens:         i64ptr(100),
		Stop:              []string{"END"},
		ParallelToolCalls: boolptr(false),
		ExtraHeaders:      map[string]string{"X-Custom": "1"},
		Extra:             map[string]any{"user": "u-1"},
	}
	req := events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "weather?"}}}
	history := []events.Event{&req}
	conv.AddPrompt("be brief")

	resp, err := RunChatCompat(context.Background(), p, history, conv, CallOptions{
		Tools: []any{tools.Schema{Name: "get_weather", Description: "w", Parameters: map[string]any{
			"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}},
		}}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Request wire shape.
	body := requestJSON(t, *captured)
	if body["model"] != "gpt-test" {
		t.Fatalf("model not sent: %+v", body["model"])
	}
	// JSON numbers decode as float64.
	if body["temperature"] != float64(0.5) || body["top_p"] != float64(0.9) || body["max_tokens"] != float64(100) {
		t.Fatalf("sampling params not sent: %+v", body)
	}
	if body["parallel_tool_calls"] != false {
		t.Fatalf("parallel_tool_calls not sent: %+v", body)
	}
	stop, ok := body["stop"].([]any)
	if !ok || len(stop) != 1 || stop[0] != "END" {
		t.Fatalf("stop not sent: %+v", body["stop"])
	}
	if _, ok := body["stream"]; ok {
		t.Fatal("non-streaming request must not carry stream=true")
	}
	if body["user"] != "u-1" {
		t.Fatalf("extra body fields not merged: %+v", body)
	}
	msgs := body["messages"].([]any)
	sys := msgs[0].(map[string]any)
	if sys["content"] != "be brief" {
		t.Fatalf("system prompt from conv.PromptLines() not sent: %+v", sys)
	}
	toolsWire := body["tools"].([]any)
	fn := toolsWire[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("tools not sent: %+v", toolsWire)
	}

	// Response normalization.
	if resp.Content() != "checking the weather" {
		t.Fatalf("bad content: %q", resp.Content())
	}
	if resp.FinishReason != "tool_calls" || resp.ResponseID != "cmpl-1" || resp.Model != "gpt-test-actual" {
		t.Fatalf("bad tracing fields: %+v", resp)
	}
	if resp.Provider != "openai" {
		t.Fatalf("provider not stamped: %q", resp.Provider)
	}
	if len(resp.ToolCalls.Calls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(resp.ToolCalls.Calls))
	}
	call := resp.ToolCalls.Calls[0]
	if call.ID != "call_1" || call.Name != "get_weather" || call.Arguments != `{"city":"SF"}` {
		t.Fatalf("bad tool call: %+v", call)
	}
	if resp.Usage.PromptTokens == nil || *resp.Usage.PromptTokens != 10 {
		t.Fatalf("usage not normalized: %+v", resp.Usage)
	}
	if resp.Usage.ThinkingTokens == nil || *resp.Usage.ThinkingTokens != 2 {
		t.Fatalf("thinking tokens not normalized: %+v", resp.Usage)
	}

	// The final message is published on the stream (transient, like Python).
	var contents []string
	for _, ev := range *sent {
		if mc, ok := events.AsValue(ev).(events.ModelMessage); ok {
			contents = append(contents, mc.Content)
		}
	}
	if len(contents) != 1 || contents[0] != "checking the weather" {
		t.Fatalf("ModelMessage not sent to conv: %v", contents)
	}
}

func TestRunChatCompatOnceAPIError(t *testing.T) {
	srv, _ := captureServer(t, 401, `{"error": "bad key"}`)
	p := CompatParams{APIKey: "k", BaseURL: srv.URL, Model: "m"}
	_, err := RunChatCompat(context.Background(), p, nil, nil, CallOptions{})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want an error carrying the status, got %v", err)
	}
}

func TestRunChatCompatResponseSchema(t *testing.T) {
	srv, captured := captureServer(t, 200, `{"id":"x","model":"m","choices":[{"finish_reason":"stop","message":{"content":"{}"}}]}`)
	p := CompatParams{APIKey: "k", BaseURL: srv.URL, Model: "m"}
	schema := response.RawSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
			"opt": map[string]any{"anyOf": []any{
				map[string]any{"type": "null"},
				map[string]any{"type": "string"},
			}},
		},
	}, response.WithRawName("Out"), response.WithRawDescription("a test schema"))

	_, err := RunChatCompat(context.Background(), p, nil, nil, CallOptions{ResponseSchema: schema})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := requestJSON(t, *captured)
	rf, ok := body["response_format"].(map[string]any)
	if !ok || rf["type"] != "json_schema" {
		t.Fatalf("response_format not derived from the schema: %+v", body["response_format"])
	}
	inner := rf["json_schema"].(map[string]any)
	if inner["name"] != "Out" || inner["description"] != "a test schema" || inner["strict"] != true {
		t.Fatalf("bad json_schema envelope: %+v", inner)
	}
	st := inner["schema"].(map[string]any)
	if st["additionalProperties"] != false {
		t.Fatal("strictify must add additionalProperties=false to the root object")
	}
	req, ok := st["required"].([]any)
	if !ok || len(req) != 2 {
		t.Fatalf("strictify must list all properties as required: %+v", st["required"])
	}
	names := []string{req[0].(string), req[1].(string)}
	sort.Strings(names)
	if names[0] != "name" || names[1] != "opt" {
		t.Fatalf("bad required list: %v", names)
	}
	optProp := st["properties"].(map[string]any)["opt"].(map[string]any)
	anyOf := optProp["anyOf"].([]any)
	if len(anyOf) != 2 {
		t.Fatalf("anyOf must survive strictify: %+v", optProp)
	}
}

func TestRunChatCompatPromptedSchemaUsesSystemPrompt(t *testing.T) {
	srv, captured := captureServer(t, 200, `{"id":"x","model":"m","choices":[{"finish_reason":"stop","message":{"content":"ok"}}]}`)
	p := CompatParams{APIKey: "k", BaseURL: srv.URL, Model: "m"}
	inner := response.RawSchema(map[string]any{"type": "object", "properties": map[string]any{}}, response.WithRawName("Out"))
	schema := response.NewPromptedSchema(inner)

	_, err := RunChatCompat(context.Background(), p, nil, nil, CallOptions{ResponseSchema: schema})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := requestJSON(t, *captured)
	if _, ok := body["response_format"]; ok {
		t.Fatal("a prompted schema has no native response_format")
	}
	sys := body["messages"].([]any)[0].(map[string]any)
	if !strings.Contains(sys["content"].(string), "JSON schema") {
		t.Fatalf("schema system prompt not appended: %q", sys["content"])
	}
}

func TestRunChatCompatExplicitResponseFormatWins(t *testing.T) {
	srv, captured := captureServer(t, 200, `{"id":"x","model":"m","choices":[{"finish_reason":"stop","message":{"content":"ok"}}]}`)
	p := CompatParams{
		APIKey: "k", BaseURL: srv.URL, Model: "m",
		ResponseFormat: map[string]any{"type": "json_object"},
	}
	schema := response.RawSchema(map[string]any{"type": "object", "properties": map[string]any{}})
	if _, err := RunChatCompat(context.Background(), p, nil, nil, CallOptions{ResponseSchema: schema}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := requestJSON(t, *captured)
	if body["response_format"].(map[string]any)["type"] != "json_object" {
		t.Fatalf("explicit ResponseFormat must win: %+v", body["response_format"])
	}
}

// --- RunChatCompat: streaming ------------------------------------------------------

const streamBody = "data: {\"id\":\"chatcmpl-1\",\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n" +
	"\n" +
	"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n" +
	"\n" +
	"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n" +
	"\n" +
	"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n" +
	"\n" +
	"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\"}}]}}]}\n" +
	"\n" +
	"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"SF\\\"}\"}}]}}]}\n" +
	"\n" +
	"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n" +
	"\n" +
	"data: {\"id\":\"chatcmpl-1\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n" +
	"\n" +
	"data: [DONE]\n" +
	"\n"

func TestRunChatCompatStream(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		captured = b
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, streamBody)
	}))
	defer srv.Close()

	conv, sent := newCaptureConv()
	p := CompatParams{
		APIKey: "k", BaseURL: srv.URL, Model: "m",
		Provider:  ProviderZAI,
		Streaming: true,
	}
	resp, err := RunChatCompat(context.Background(), p, nil, conv, CallOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	body := requestJSON(t, captured)
	if body["stream"] != true {
		t.Fatal("streaming request must carry stream=true")
	}
	so, ok := body["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Fatalf("stream_options.include_usage must be sent: %+v", body["stream_options"])
	}

	if resp.Content() != "Hello" {
		t.Fatalf("stream content must accumulate, got %q", resp.Content())
	}
	if resp.Message == nil {
		t.Fatal("the synthesized response must carry the message")
	}
	if len(resp.ToolCalls.Calls) != 1 {
		t.Fatalf("tool-call deltas must merge to one call, got %d", len(resp.ToolCalls.Calls))
	}
	call := resp.ToolCalls.Calls[0]
	if call.ID != "call_1" || call.Name != "get_weather" || call.Arguments != `{"city":"SF"}` {
		t.Fatalf("bad merged call: %+v", call)
	}
	if resp.FinishReason != "tool_calls" || resp.ResponseID != "chatcmpl-1" || resp.Model != "m1" {
		t.Fatalf("bad tracing fields: %+v", resp)
	}
	if resp.Provider != "zai" {
		t.Fatalf("provider not stamped: %q", resp.Provider)
	}
	if resp.Usage.PromptTokens == nil || *resp.Usage.PromptTokens != 7 || resp.Usage.CompletionTokens == nil || *resp.Usage.CompletionTokens != 3 {
		t.Fatalf("usage from the final chunk missing: %+v", resp.Usage)
	}

	// Progress events on the stream, in order.
	var got []string
	for _, ev := range *sent {
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

func TestRunChatCompatStreamToolCallIndices(t *testing.T) {
	// Two calls interleaved by index; the first chunk arrives after the second.
	stream := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call_b\",\"function\":{\"name\":\"tool_b\",\"arguments\":\"1\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"function\":{\"name\":\"tool_a\",\"arguments\":\"0\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, stream)
	}))
	defer srv.Close()

	p := CompatParams{APIKey: "k", BaseURL: srv.URL, Model: "m", Streaming: true}
	resp, err := RunChatCompat(context.Background(), p, nil, nil, CallOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.ToolCalls.Calls) != 2 {
		t.Fatalf("want 2 merged calls, got %d", len(resp.ToolCalls.Calls))
	}
	if resp.ToolCalls.Calls[0].ID != "call_a" || resp.ToolCalls.Calls[1].ID != "call_b" {
		t.Fatalf("calls must merge by index: %+v", resp.ToolCalls.Calls)
	}
}

func TestRunChatCompatStreamAPIError(t *testing.T) {
	srv, _ := captureServer(t, 500, `{"error":"down"}`)
	p := CompatParams{APIKey: "k", BaseURL: srv.URL, Model: "m", Streaming: true, MaxRetries: 0}
	_, err := RunChatCompat(context.Background(), p, nil, nil, CallOptions{})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("want an error carrying the status, got %v", err)
	}
}

func TestRunChatCompatStreamInBandError(t *testing.T) {
	// An {"error": ...} chunk inside a 200 stream fails the call — Python
	// parity (the SDK iterator raises instead of returning an empty response).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"+
			"data: {\"error\":{\"message\":\"The server is overloaded\",\"type\":\"server_error\"}}\n\n"+
			"data: [DONE]\n\n")
	}))
	defer srv.Close()

	p := CompatParams{APIKey: "k", BaseURL: srv.URL, Model: "m", Streaming: true}
	_, err := RunChatCompat(context.Background(), p, nil, nil, CallOptions{})
	if !errors.Is(err, ErrStreamError) {
		t.Fatalf("in-band error chunk must fail the call, got %v", err)
	}
	if !strings.Contains(err.Error(), "server_error") || !strings.Contains(err.Error(), "The server is overloaded") {
		t.Fatalf("error must carry the API detail, got %v", err)
	}
}

// --- core contract ------------------------------------------------------------------

func TestProviderValues(t *testing.T) {
	want := map[Provider]string{
		ProviderOpenAI: "openai", ProviderAnthropic: "anthropic", ProviderGemini: "gemini",
		ProviderBedrock: "bedrock", ProviderDashScope: "dashscope", ProviderMistral: "mistral",
		ProviderOllama: "ollama", ProviderTypeSafe: "typesafe", ProviderVertexAI: "vertexai",
		ProviderXAI: "xai", ProviderZAI: "zai",
	}
	for p, v := range want {
		if string(p) != v {
			t.Fatalf("provider %q has value %q, want %q", v, string(p), v)
		}
	}
}

// fakeConfig proves the ModelConfig/LLMClient contracts are implementable as declared.
type fakeConfig struct{ model string }

func (f fakeConfig) Provider() Provider { return ProviderOpenAI }
func (f fakeConfig) Model() string      { return f.model }
func (f fakeConfig) Create() (LLMClient, error) {
	return fakeClient{}, nil
}
func (f fakeConfig) Clone(overrides map[string]any) (ModelConfig, error) {
	m := f.model
	if v, ok := overrides["model"].(string); ok {
		m = v
	}
	return fakeConfig{model: m}, nil
}

type fakeClient struct{}

func (fakeClient) Call(ctx context.Context, messages []events.Event, conv *agocontext.Context, opts CallOptions) (*events.ModelResponse, error) {
	return &events.ModelResponse{}, nil
}

func TestModelConfigContract(t *testing.T) {
	var cfg ModelConfig = fakeConfig{model: "m1"}
	if cfg.Model() != "m1" {
		t.Fatal("bad model")
	}
	cloned, err := cfg.Clone(map[string]any{"model": "m2"})
	if err != nil {
		t.Fatalf("clone failed: %v", err)
	}
	if cloned.Model() != "m2" {
		t.Fatal("override not applied")
	}
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if _, err := client.Call(context.Background(), nil, nil, CallOptions{}); err != nil {
		t.Fatalf("call failed: %v", err)
	}
}
