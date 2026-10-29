package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"ago/config"
	agocontext "ago/context"
	"ago/events"
	"ago/stream"
	"ago/tools"
)

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

// captureServer records the request (URL path, headers, body) and answers with
// the given status and body.
func captureServer(t *testing.T, status int, body string) (*httptest.Server, *capturedRequest) {
	t.Helper()
	cap := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request: %v", err)
		}
		cap.Path = r.URL.Path
		cap.RawQuery = r.URL.RawQuery
		cap.Headers = r.Header.Clone()
		cap.Body = b
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

type capturedRequest struct {
	Path     string
	RawQuery string
	Headers  http.Header
	Body     []byte
}

func (c *capturedRequest) JSON(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(c.Body, &m); err != nil {
		t.Fatalf("captured request is not JSON: %v", err)
	}
	return m
}

// sseServer answers a streaming request with the given raw data chunks.
func sseServer(t *testing.T, chunks ...string) (*httptest.Server, *capturedRequest) {
	t.Helper()
	cap := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request: %v", err)
		}
		cap.Path = r.URL.Path
		cap.RawQuery = r.URL.RawQuery
		cap.Headers = r.Header.Clone()
		cap.Body = b
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, chunk := range chunks {
			// SSE frames multi-line payloads as consecutive data: lines (the
			// scanner joins them back with newline).
			for _, line := range strings.Split(chunk, "\n") {
				fmt.Fprintf(w, "data: %s\n", line)
			}
			fmt.Fprint(w, "\n")
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

type fakeProto struct {
	name, description, prompt string
	schema                    map[string]any
}

func (f fakeProto) Name() string                                          { return f.name }
func (f fakeProto) Description() string                                   { return f.description }
func (f fakeProto) JSONSchema() map[string]any                            { return f.schema }
func (f fakeProto) SystemPrompt() string                                  { return f.prompt }
func (f fakeProto) Validate(s string, _ *agocontext.Context) (any, error) { return s, nil }

type fakeHosted struct{}

func (fakeHosted) SchemaType() string { return "web_search" }

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

// newTestConfig builds a config pointed at srv with an explicit key.
func newTestConfig(srvURL string, opts ...Option) *GeminiConfig {
	all := append([]Option{WithBaseURL(srvURL)}, opts...)
	return NewGemini("test-key", "gemini-test", all...)
}

// startClient is Create without the ceremony (all test configs are valid).
func startClient(t *testing.T, c *GeminiConfig) *client {
	t.Helper()
	cl, err := c.Create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return cl.(*client)
}

// --- config -------------------------------------------------------------------

func TestGeminiCreateValidation(t *testing.T) {
	t.Setenv(EnvAPIKey, "")

	if _, err := NewGemini("k", "").Create(); !errors.Is(err, config.ErrNotProvided) {
		t.Fatalf("missing model: want ErrNotProvided, got %v", err)
	}
	if _, err := NewGemini("", "m").Create(); !errors.Is(err, config.ErrNotProvided) {
		t.Fatalf("missing key: want ErrNotProvided, got %v", err)
	}
	// Env fallback satisfies the key requirement.
	t.Setenv(EnvAPIKey, "env-key")
	if _, err := NewGemini("", "m").Create(); err != nil {
		t.Fatalf("env fallback: %v", err)
	}
	// An explicit key wins over the environment.
	c := NewGemini("explicit", "m")
	cl, err := c.Create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := cl.(*client).apiKey; got != "explicit" {
		t.Fatalf("explicit key must win over the environment, got %q", got)
	}
}

func TestGeminiProviderAndModel(t *testing.T) {
	c := NewGemini("k", "gemini-2")
	if c.Provider() != config.ProviderGemini {
		t.Fatalf("provider: %v", c.Provider())
	}
	if c.Model() != "gemini-2" {
		t.Fatalf("model: %q", c.Model())
	}
}

func TestGeminiClone(t *testing.T) {
	base := NewGemini("k", "m", WithTemperature(0.4), WithTopK(8), WithStopSequences("a", "b"))
	cc, err := base.Clone(map[string]any{
		"model":             "m2",
		"api_key":           "k2",
		"base_url":          "https://alt.example",
		"temperature":       0.9,
		"top_p":             f64(0.5),
		"top_k":             3,
		"max_output_tokens": i64(64),
		"stop_sequences":    []string{"x"},
		"streaming":         true,
	})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	got := cc.(*GeminiConfig)
	if got.model != "m2" || got.apiKey != "k2" || got.baseURL != "https://alt.example" ||
		got.temperature == nil || *got.temperature != 0.9 ||
		got.topP == nil || *got.topP != 0.5 ||
		got.topK == nil || *got.topK != 3 ||
		got.maxOutputTokens == nil || *got.maxOutputTokens != 64 ||
		!got.streaming {
		t.Fatalf("overrides not applied: %+v", got)
	}
	if len(got.stopSequences) != 1 || got.stopSequences[0] != "x" {
		t.Fatalf("stop sequences: %v", got.stopSequences)
	}
	// The base config stays untouched (deep copy).
	if len(base.stopSequences) != 2 || base.stopSequences[0] != "a" {
		t.Fatalf("base mutated: %v", base.stopSequences)
	}
	// nil clears an optional field.
	cc2, err := base.Clone(map[string]any{"temperature": nil})
	if err != nil {
		t.Fatalf("clone nil: %v", err)
	}
	if cc2.(*GeminiConfig).temperature != nil {
		t.Fatal("nil override must clear the field")
	}
	// Unknown keys fail.
	if _, err := base.Clone(map[string]any{"nope": 1}); err == nil {
		t.Fatal("unknown override must fail")
	}
	// Wrong value types fail.
	if _, err := base.Clone(map[string]any{"temperature": "hot"}); err == nil {
		t.Fatal("type-mismatched override must fail")
	}
}

// --- request shape --------------------------------------------------------------

func TestGeminiRequestShape(t *testing.T) {
	srv, cap := captureServer(t, 200, `{"candidates":[]}`)
	cl := startClient(t, newTestConfig(srv.URL,
		WithTemperature(0.3), WithTopP(0.8), WithTopK(20),
		WithMaxOutputTokens(256), WithStopSequences("END"),
	))
	img := &events.BinaryInput{Data: []byte{1, 2, 3}, MediaType: "image/png", Kind: events.BinaryImage}
	urlImg := &events.UrlInput{URL: "https://example.com/cat.png", Kind: events.BinaryImage}
	req := &events.ModelRequest{Parts: []events.Input{
		&events.TextInput{Content: "what is this"},
		img,
		urlImg,
		&events.DataInput{Data: map[string]any{"k": 1}},
	}}
	conv, _ := newCaptureConv()
	conv.AddPrompt("you are", "helpful")

	tool := tools.Schema{
		Name:        "lookup",
		Description: "Finds things",
		Parameters: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"q": map[string]any{"type": "string"}},
			"additionalProperties": false,
		},
	}
	_, err := cl.Call(context.Background(), []events.Event{req}, conv, config.CallOptions{
		Tools:          []any{tool},
		ResponseSchema: fakeProto{name: "Out", schema: map[string]any{"type": "object"}},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}

	if cap.Path != "/models/gemini-test:generateContent" {
		t.Fatalf("path: %s", cap.Path)
	}
	if got := cap.Headers.Get("x-goog-api-key"); got != "test-key" {
		t.Fatalf("api key header: %q", got)
	}
	body := cap.JSON(t)

	// systemInstruction from the prompt lines.
	si, ok := body["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatalf("missing systemInstruction: %v", body)
	}
	siParts := si["parts"].([]any)
	if siParts[0].(map[string]any)["text"] != "you are\nhelpful" {
		t.Fatalf("bad systemInstruction: %v", si)
	}

	// contents: the user turn maps text / inlineData / fileData / json-text.
	contents := body["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("want 1 content, got %d", len(contents))
	}
	c0 := contents[0].(map[string]any)
	if c0["role"] != "user" {
		t.Fatalf("role: %v", c0["role"])
	}
	parts := c0["parts"].([]any)
	if len(parts) != 4 {
		t.Fatalf("want 4 parts, got %d", len(parts))
	}
	if parts[0].(map[string]any)["text"] != "what is this" {
		t.Fatalf("text part: %v", parts[0])
	}
	inline := parts[1].(map[string]any)["inlineData"].(map[string]any)
	if inline["mimeType"] != "image/png" || inline["data"] != "AQID" {
		t.Fatalf("inlineData: %v", inline)
	}
	fileData := parts[2].(map[string]any)["fileData"].(map[string]any)
	if fileData["fileUri"] != "https://example.com/cat.png" || fileData["mimeType"] != "image/png" {
		t.Fatalf("fileData: %v", fileData)
	}
	if parts[3].(map[string]any)["text"] != `{"k":1}` {
		t.Fatalf("DataInput part: %v", parts[3])
	}

	// generationConfig carries the sampling fields and the response schema.
	gen := body["generationConfig"].(map[string]any)
	if gen["temperature"] != 0.3 || gen["topP"] != 0.8 || gen["topK"] != float64(20) ||
		gen["maxOutputTokens"] != float64(256) {
		t.Fatalf("generationConfig: %v", gen)
	}
	stops := gen["stopSequences"].([]any)
	if len(stops) != 1 || stops[0] != "END" {
		t.Fatalf("stopSequences: %v", gen)
	}
	if gen["responseMimeType"] != "application/json" {
		t.Fatalf("responseMimeType: %v", gen)
	}
	if _, ok := gen["responseJsonSchema"].(map[string]any); !ok {
		t.Fatalf("responseJsonSchema: %v", gen)
	}

	// tools: one functionDeclarations block, additionalProperties stripped.
	wireTools := body["tools"].([]any)
	if len(wireTools) != 1 {
		t.Fatalf("want 1 tool, got %d", len(wireTools))
	}
	decls := wireTools[0].(map[string]any)["functionDeclarations"].([]any)
	if len(decls) != 1 {
		t.Fatalf("want 1 declaration, got %d", len(decls))
	}
	decl := decls[0].(map[string]any)
	if decl["name"] != "lookup" || decl["description"] != "Finds things" {
		t.Fatalf("declaration: %v", decl)
	}
	params := decl["parameters"].(map[string]any)
	if _, banned := params["additionalProperties"]; banned {
		t.Fatalf("additionalProperties must be stripped: %v", params)
	}
}

func TestGeminiParameterlessToolSchema(t *testing.T) {
	srv, cap := captureServer(t, 200, `{"candidates":[]}`)
	cl := startClient(t, newTestConfig(srv.URL))
	_, err := cl.Call(context.Background(), nil, nil, config.CallOptions{
		Tools: []any{tools.Schema{Name: "ping"}, tools.Schema{Name: "empty", Parameters: map[string]any{}}},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	decls := cap.JSON(t)["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)
	for _, d := range decls {
		params := d.(map[string]any)["parameters"].(map[string]any)
		if params["type"] != "object" {
			t.Fatalf("parameters must normalize to the object schema: %v", params)
		}
		if _, ok := params["properties"]; !ok {
			t.Fatalf("missing properties: %v", params)
		}
	}
}

func TestGeminiNoSystemInstructionWhenPromptEmpty(t *testing.T) {
	srv, cap := captureServer(t, 200, `{"candidates":[]}`)
	cl := startClient(t, newTestConfig(srv.URL))
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if _, ok := cap.JSON(t)["systemInstruction"]; ok {
		t.Fatal("empty prompt must omit systemInstruction")
	}
}

// --- responses -------------------------------------------------------------------

func TestGeminiTextResponse(t *testing.T) {
	srv, _ := captureServer(t, 200, `{
		"candidates": [{
			"content": {"role": "model", "parts": [
				{"text": "quiet thought", "thought": true},
				{"text": "Hello there"}
			]},
			"finishReason": "STOP"
		}],
		"usageMetadata": {"promptTokenCount": 9, "candidatesTokenCount": 4, "totalTokenCount": 13, "cachedContentTokenCount": 3},
		"modelVersion": "gemini-test-001",
		"responseId": "resp-1"
	}`)
	cl := startClient(t, newTestConfig(srv.URL))
	conv, sent := newCaptureConv()
	req := &events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "hi"}}}

	resp, err := cl.Call(context.Background(), []events.Event{req}, conv, config.CallOptions{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if resp.Content() != "Hello there" {
		t.Fatalf("content: %q", resp.Content())
	}
	if resp.Message == nil {
		t.Fatal("message must be set")
	}
	if resp.Model != "gemini-test" || resp.Provider != "gemini" {
		t.Fatalf("tracing: model=%q provider=%q", resp.Model, resp.Provider)
	}
	if resp.FinishReason != "stop" {
		t.Fatalf("finish reason must be lowercased: %q", resp.FinishReason)
	}
	if resp.ResponseID != "resp-1" {
		t.Fatalf("response id: %q", resp.ResponseID)
	}
	want := events.Usage{
		PromptTokens: i64(9), CompletionTokens: i64(4), TotalTokens: i64(13),
		CacheReadInputTokens: i64(3),
	}
	if !reflect.DeepEqual(resp.Usage, want) {
		t.Fatalf("usage: %+v", resp.Usage)
	}
	if len(resp.ToolCalls.Calls) != 0 {
		t.Fatalf("unexpected calls: %v", resp.ToolCalls.Calls)
	}
	// Progress: the thought becomes ModelReasoning, the answer ModelMessage; no
	// chunk events on the non-streaming path.
	if len(*sent) != 2 {
		t.Fatalf("want ModelReasoning + ModelMessage, got %d events: %v", len(*sent), *sent)
	}
	if (*sent)[0].(*events.ModelReasoning).Content != "quiet thought" {
		t.Fatalf("reasoning: %v", (*sent)[0])
	}
	if (*sent)[1].(*events.ModelMessage).Content != "Hello there" {
		t.Fatalf("message: %v", (*sent)[1])
	}
}

func TestGeminiUsageToolUsePromptTokenCount(t *testing.T) {
	srv, _ := captureServer(t, 200, `{
		"candidates": [{"content": {"parts": [{"text": "ok"}]}, "finishReason": "STOP"}],
		"usageMetadata": {"promptTokenCount": 10, "toolUsePromptTokenCount": 7, "candidatesTokenCount": 5,
			"totalTokenCount": 22, "thoughtsTokenCount": 2}
	}`)
	cl := startClient(t, newTestConfig(srv.URL))
	resp, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	// toolUsePromptTokenCount folds into the prompt; thoughts stay separate
	// (brief mapping — see normalizeUsage).
	want := events.Usage{
		PromptTokens: i64(17), CompletionTokens: i64(5), TotalTokens: i64(22), ThinkingTokens: i64(2),
	}
	if !reflect.DeepEqual(resp.Usage, want) {
		t.Fatalf("usage: %+v", resp.Usage)
	}
}

func TestGeminiBlockedResponse(t *testing.T) {
	// A content filter answers with no candidates; the call still returns a
	// response (Python parity: `response.candidates or ()`).
	srv, _ := captureServer(t, 200, `{"promptFeedback": {"blockReason": "SAFETY"}}`)
	cl := startClient(t, newTestConfig(srv.URL))
	resp, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if resp.Message != nil || resp.FinishReason != "" || len(resp.ToolCalls.Calls) != 0 {
		t.Fatalf("empty response expected: %+v", resp)
	}
}

func TestGeminiHTTPError(t *testing.T) {
	srv, _ := captureServer(t, 400, `{"error": {"code": 400, "message": "bad request", "status": "INVALID_ARGUMENT"}}`)
	cl := startClient(t, newTestConfig(srv.URL))
	_, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("want 400 in error, got %v", err)
	}
}

// --- tool round trip ---------------------------------------------------------------

func TestGeminiToolCallResponse(t *testing.T) {
	srv, _ := captureServer(t, 200, `{
		"candidates": [{
			"content": {"role": "model", "parts": [
				{"text": "looking it up"},
				{"functionCall": {"name": "lookup", "args": {"q": "go"}}}
			]},
			"finishReason": "STOP"
		}],
		"usageMetadata": {"promptTokenCount": 5, "candidatesTokenCount": 3, "totalTokenCount": 8}
	}`)
	cl := startClient(t, newTestConfig(srv.URL))
	conv, sent := newCaptureConv()
	resp, err := cl.Call(context.Background(), nil, conv, config.CallOptions{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	calls := resp.ToolCalls.Calls
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(calls))
	}
	call := calls[0]
	if call.Name != "lookup" {
		t.Fatalf("name: %q", call.Name)
	}
	if call.ID == "" {
		t.Fatal("the port must synthesize a call id (the API returns none)")
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
		t.Fatalf("arguments must be a JSON string: %q (%v)", call.Arguments, err)
	}
	if args["q"] != "go" {
		t.Fatalf("args: %v", args)
	}
	if len(*sent) != 1 || (*sent)[0].EventType() != "ModelMessage" {
		t.Fatalf("progress events: %v", *sent)
	}
}

func TestGeminiToolRoundTripReplay(t *testing.T) {
	srv, cap := captureServer(t, 200, `{"candidates": []}`)
	cl := startClient(t, newTestConfig(srv.URL))

	previous := &events.ModelResponse{
		Message: &events.ModelMessage{Content: "looking it up"},
		ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "call-1", Name: "lookup", Arguments: `{"q":"go"}`},
		}},
	}
	results := &events.ToolResultsEvent{Results: []events.ToolResultEvent{
		// Multiple results of one batch travel as functionResponse parts of one
		// user turn; each response.result is the text, or a list for several parts.
		events.FromResultCall(previous.ToolCalls.Calls[0], events.NewToolResult(false, "found it")),
	}}
	req := &events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "hi"}}}

	if _, err := cl.Call(context.Background(), []events.Event{req, previous, results}, nil, config.CallOptions{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	contents := cap.JSON(t)["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("want user/model/user contents, got %d", len(contents))
	}
	model := contents[1].(map[string]any)
	if model["role"] != "model" {
		t.Fatalf("replayed model role: %v", model["role"])
	}
	mParts := model["parts"].([]any)
	if len(mParts) != 2 {
		t.Fatalf("want text + functionCall parts, got %d", len(mParts))
	}
	if mParts[0].(map[string]any)["text"] != "looking it up" {
		t.Fatalf("replayed text: %v", mParts[0])
	}
	fc := mParts[1].(map[string]any)["functionCall"].(map[string]any)
	if fc["name"] != "lookup" {
		t.Fatalf("replayed call name: %v", fc)
	}
	if args, ok := fc["args"].(map[string]any); !ok || args["q"] != "go" {
		t.Fatalf("replayed call args must be an object: %v", fc["args"])
	}

	user := contents[2].(map[string]any)
	if user["role"] != "user" {
		t.Fatalf("result role: %v", user["role"])
	}
	uParts := user["parts"].([]any)
	if len(uParts) != 1 {
		t.Fatalf("want 1 functionResponse part, got %d", len(uParts))
	}
	fr := uParts[0].(map[string]any)["functionResponse"].(map[string]any)
	if fr["name"] != "lookup" {
		t.Fatalf("functionResponse name: %v", fr)
	}
	responseMap := fr["response"].(map[string]any)
	if responseMap["result"] != "found it" {
		t.Fatalf("functionResponse result: %v", responseMap)
	}
}

func TestGeminiToolResultMultiPartIsList(t *testing.T) {
	srv, cap := captureServer(t, 200, `{"candidates": []}`)
	cl := startClient(t, newTestConfig(srv.URL))
	call := events.NewToolCallEvent("calc")
	results := &events.ToolResultsEvent{Results: []events.ToolResultEvent{
		events.FromResultCall(call, events.ToolResult{Parts: []events.Input{
			events.TextInput{Content: "part one"},
			&events.DataInput{Data: 42},
		}}),
	}}
	if _, err := cl.Call(context.Background(), []events.Event{results}, nil, config.CallOptions{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	parts := cap.JSON(t)["contents"].([]any)[0].(map[string]any)["parts"].([]any)
	fr := parts[0].(map[string]any)["functionResponse"].(map[string]any)
	result, ok := fr["response"].(map[string]any)["result"].([]any)
	if !ok || len(result) != 2 || result[0] != "part one" || result[1] != "42" {
		t.Fatalf("multi-part result must be a list: %v", fr)
	}
}

// --- unsupported inputs / tools -----------------------------------------------------

func TestGeminiUnsupportedInput(t *testing.T) {
	srv, _ := captureServer(t, 200, `{}`)
	cl := startClient(t, newTestConfig(srv.URL))
	req := &events.ModelRequest{Parts: []events.Input{&events.FileIdInput{FileID: "files/abc"}}}
	_, err := cl.Call(context.Background(), []events.Event{req}, nil, config.CallOptions{})
	if !errors.Is(err, config.ErrUnsupportedInput) {
		t.Fatalf("FileIdInput: want ErrUnsupportedInput, got %v", err)
	}
}

func TestGeminiUnsupportedTool(t *testing.T) {
	srv, _ := captureServer(t, 200, `{}`)
	cl := startClient(t, newTestConfig(srv.URL))
	_, err := cl.Call(context.Background(), nil, nil, config.CallOptions{Tools: []any{fakeHosted{}}})
	if !errors.Is(err, config.ErrUnsupportedTool) {
		t.Fatalf("hosted tool: want ErrUnsupportedTool, got %v", err)
	}
}

// --- streaming ------------------------------------------------------------------------

func TestGeminiStreamResponse(t *testing.T) {
	chunks := []string{
		`{"candidates": [{"content": {"role": "model", "parts": [{"text": "Hel"}]}}]}`,
		`{"candidates": [{"content": {"role": "model", "parts": [{"text": "lo"}]}}]}`,
		`{"candidates": [{"content": {"role": "model", "parts": [
			{"text": "thinking", "thought": true},
			{"functionCall": {"name": "lookup", "args": {"q": "go"}}}
		]}, "finishReason": "STOP"}]}`,
		`{"candidates": [{"content": {"role": "model"}}],
		  "usageMetadata": {"promptTokenCount": 10, "candidatesTokenCount": 5, "totalTokenCount": 15, "thoughtsTokenCount": 3},
		  "modelVersion": "gemini-test-001", "responseId": "rid-1"}`,
	}
	srv, cap := sseServer(t, chunks...)
	cl := startClient(t, newTestConfig(srv.URL, WithStreaming(true)))
	conv, sent := newCaptureConv()

	resp, err := cl.Call(context.Background(), nil, conv, config.CallOptions{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if cap.Path != "/models/gemini-test:streamGenerateContent" || cap.RawQuery != "alt=sse" {
		t.Fatalf("stream endpoint: %s?%s", cap.Path, cap.RawQuery)
	}
	if got := cap.Headers.Get("x-goog-api-key"); got != "test-key" {
		t.Fatalf("api key header: %q", got)
	}
	if resp.Content() != "Hello" {
		t.Fatalf("content: %q", resp.Content())
	}
	if resp.FinishReason != "stop" || resp.ResponseID != "rid-1" {
		t.Fatalf("finish=%q responseID=%q", resp.FinishReason, resp.ResponseID)
	}
	want := events.Usage{PromptTokens: i64(10), CompletionTokens: i64(5), TotalTokens: i64(15), ThinkingTokens: i64(3)}
	if !reflect.DeepEqual(resp.Usage, want) {
		t.Fatalf("usage: %+v", resp.Usage)
	}
	if len(resp.ToolCalls.Calls) != 1 || resp.ToolCalls.Calls[0].Name != "lookup" {
		t.Fatalf("streamed calls: %v", resp.ToolCalls.Calls)
	}
	// Progress: two text chunks, one reasoning, one final message.
	var kinds []string
	for _, ev := range *sent {
		kinds = append(kinds, ev.EventType())
	}
	wantKinds := []string{"ModelMessageChunk", "ModelMessageChunk", "ModelReasoning", "ModelMessage"}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Fatalf("progress events: %v", kinds)
	}
}

func TestGeminiStreamHTTPError(t *testing.T) {
	srv, _ := captureServer(t, 503, "overloaded")
	cl := startClient(t, newTestConfig(srv.URL, WithStreaming(true)))
	_, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("want 503 in error, got %v", err)
	}
}
