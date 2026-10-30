package ollama

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
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

type capturedRequest struct {
	Path     string
	Headers  http.Header
	Body     []byte
	authSeen bool
}

// captureServer records the request and answers with the given status and body.
func captureServer(t *testing.T, status int, body string) (*httptest.Server, *capturedRequest) {
	t.Helper()
	cap := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request: %v", err)
		}
		cap.Path = r.URL.Path
		cap.Headers = r.Header.Clone()
		cap.Body = b
		_, cap.authSeen = r.Header["Authorization"]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

func (c *capturedRequest) JSON(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(c.Body, &m); err != nil {
		t.Fatalf("captured request is not JSON: %v", err)
	}
	return m
}

type fakeHosted struct{}

func (fakeHosted) SchemaType() string { return "web_search" }

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

func startClient(t *testing.T, c *OllamaConfig) *client {
	t.Helper()
	cl, err := c.Create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return cl.(*client)
}

// --- config -------------------------------------------------------------------

func TestOllamaCreateValidation(t *testing.T) {
	t.Setenv(EnvAPIKey, "")

	if _, err := NewOllama("").Create(); !errors.Is(err, config.ErrNotProvided) {
		t.Fatalf("missing model: want ErrNotProvided, got %v", err)
	}
	// Streaming is not implemented in this port: Create refuses.
	if _, err := NewOllama("m", WithStreaming(true)).Create(); !errors.Is(err, config.ErrUnsupportedInput) {
		t.Fatalf("streaming: want ErrUnsupportedInput, got %v", err)
	}
	// A missing key is legal (local server): no error, no header.
	cl, err := NewOllama("m").Create()
	if err != nil {
		t.Fatalf("create without key: %v", err)
	}
	if cl.(*client).apiKey != "" {
		t.Fatalf("key must stay empty, got %q", cl.(*client).apiKey)
	}
	// Env fallback fills the bearer token.
	t.Setenv(EnvAPIKey, "env-key")
	cl, err = NewOllama("m").Create()
	if err != nil {
		t.Fatalf("create with env key: %v", err)
	}
	if cl.(*client).apiKey != "env-key" {
		t.Fatalf("env key: %q", cl.(*client).apiKey)
	}
}

func TestOllamaProviderModelAndDefaults(t *testing.T) {
	c := NewOllama("llama3")
	if c.Provider() != config.ProviderOllama {
		t.Fatalf("provider: %v", c.Provider())
	}
	if c.Model() != "llama3" {
		t.Fatalf("model: %q", c.Model())
	}
	cl := startClient(t, c)
	if cl.host != DefaultHost {
		t.Fatalf("default host: %q", cl.host)
	}
}

func TestOllamaClone(t *testing.T) {
	base := NewOllama("m", WithTemperature(0.2), WithSeed(5), WithStop("S1", "S2"))
	cc, err := base.Clone(map[string]any{
		"model":       "m2",
		"host":        "http://box:11434",
		"api_key":     "k2",
		"temperature": 0.9,
		"top_p":       f64(0.5),
		"max_tokens":  64,
		"stop":        []string{"X"},
		"seed":        i64(7),
		"streaming":   false,
	})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	got := cc.(*OllamaConfig)
	if got.model != "m2" || got.host != "http://box:11434" || got.apiKey != "k2" ||
		got.temperature == nil || *got.temperature != 0.9 ||
		got.topP == nil || *got.topP != 0.5 ||
		got.maxTokens == nil || *got.maxTokens != 64 ||
		got.seed == nil || *got.seed != 7 {
		t.Fatalf("overrides not applied: %+v", got)
	}
	if len(got.stop) != 1 || got.stop[0] != "X" {
		t.Fatalf("stop: %v", got.stop)
	}
	if len(base.stop) != 2 || base.stop[0] != "S1" {
		t.Fatalf("base mutated: %v", base.stop)
	}
	if _, err := base.Clone(map[string]any{"nope": 1}); err == nil {
		t.Fatal("unknown override must fail")
	}
	if _, err := base.Clone(map[string]any{"seed": "seven"}); err == nil {
		t.Fatal("type-mismatched override must fail")
	}
}

// --- request shape --------------------------------------------------------------

func TestOllamaRequestShape(t *testing.T) {
	srv, cap := captureServer(t, 200, `{"model": "llama3", "message": {"role": "assistant", "content": "hi"}}`)
	cl := startClient(t, NewOllama("llama3",
		WithHost(srv.URL), WithAPIKey("secret"),
		WithTemperature(0.4), WithTopP(0.9), WithMaxTokens(64), WithStop("END"), WithSeed(7),
	))
	img := &events.BinaryInput{Data: []byte{0xff, 0xd8}, MediaType: "image/png", Kind: events.BinaryImage}
	req := &events.ModelRequest{Parts: []events.Input{
		&events.TextInput{Content: "look"},
		img,
		&events.DataInput{Data: map[string]any{"k": 1}},
	}}
	conv, _ := newCaptureConv()
	conv.AddPrompt("be terse")

	tool := tools.Schema{
		Name:        "lookup",
		Description: "Finds things",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"q": map[string]any{"type": "string"}},
		},
	}
	schema := map[string]any{"type": "object"}
	_, err := cl.Call(context.Background(), []events.Event{req}, conv, config.CallOptions{
		Tools:          []any{tool},
		ResponseSchema: fakeProto{schema: schema},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}

	if cap.Path != "/api/chat" {
		t.Fatalf("path: %s", cap.Path)
	}
	if got := cap.Headers.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("authorization: %q", got)
	}
	body := cap.JSON(t)

	if body["model"] != "llama3" || body["stream"] != false {
		t.Fatalf("envelope: model=%v stream=%v", body["model"], body["stream"])
	}

	// Messages: one system line, then one user message per text part with the
	// image attached to the last, DataInput as JSON text.
	messages := body["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("want 3 messages, got %d: %v", len(messages), messages)
	}
	sys := messages[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "be terse" {
		t.Fatalf("system message: %v", sys)
	}
	u0 := messages[1].(map[string]any)
	if u0["role"] != "user" || u0["content"] != "look" {
		t.Fatalf("first user message: %v", u0)
	}
	u1 := messages[2].(map[string]any)
	if u1["content"] != `{"k":1}` {
		t.Fatalf("second user message content: %v", u1["content"])
	}
	images := u1["images"].([]any)
	if len(images) != 1 || images[0] != base64.StdEncoding.EncodeToString([]byte{0xff, 0xd8}) {
		t.Fatalf("images: %v", images)
	}

	// options carries the sampling fields; MaxTokens maps to num_predict.
	opts := body["options"].(map[string]any)
	if opts["temperature"] != 0.4 || opts["top_p"] != 0.9 ||
		opts["num_predict"] != float64(64) || opts["seed"] != float64(7) {
		t.Fatalf("options: %v", opts)
	}
	if stops := opts["stop"].([]any); len(stops) != 1 || stops[0] != "END" {
		t.Fatalf("stop: %v", opts)
	}

	// tools: the function envelope.
	wireTools := body["tools"].([]any)
	fn := wireTools[0].(map[string]any)
	if fn["type"] != "function" {
		t.Fatalf("tool type: %v", fn)
	}
	inner := fn["function"].(map[string]any)
	if inner["name"] != "lookup" || inner["description"] != "Finds things" {
		t.Fatalf("tool function: %v", inner)
	}
	params := inner["parameters"].(map[string]any)
	if params["type"] != "object" {
		t.Fatalf("parameters must be the object schema: %v", params)
	}

	// Structured output rides on format (the raw schema, not an envelope).
	format := body["format"].(map[string]any)
	if format["type"] != "object" {
		t.Fatalf("format: %v", format)
	}
}

func TestOllamaImagesWithoutText(t *testing.T) {
	srv, cap := captureServer(t, 200, `{"message": {"content": "ok"}}`)
	cl := startClient(t, NewOllama("m", WithHost(srv.URL)))
	img := &events.BinaryInput{Data: []byte{1}, MediaType: "image/png", Kind: events.BinaryImage}
	req := &events.ModelRequest{Parts: []events.Input{img}}
	if _, err := cl.Call(context.Background(), []events.Event{req}, nil, config.CallOptions{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	messages := cap.JSON(t)["messages"].([]any)
	last := messages[len(messages)-1].(map[string]any)
	if last["role"] != "user" || last["content"] != "" {
		t.Fatalf("image-only turn: %v", last)
	}
	if _, ok := last["images"]; !ok {
		t.Fatalf("missing images: %v", last)
	}
}

func TestOllamaNoAuthHeaderWithoutKey(t *testing.T) {
	t.Setenv(EnvAPIKey, "")
	srv, cap := captureServer(t, 200, `{"message": {"content": "ok"}}`)
	cl := startClient(t, NewOllama("m", WithHost(srv.URL)))
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if cap.authSeen {
		t.Fatal("no Authorization header may be sent without a key")
	}
}

func TestOllamaSystemPromptFromResponseSchema(t *testing.T) {
	srv, cap := captureServer(t, 200, `{"message": {"content": "ok"}}`)
	cl := startClient(t, NewOllama("m", WithHost(srv.URL)))
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{
		ResponseSchema: fakeProto{prompt: "answer in json", schema: map[string]any{"type": "object"}},
	}); err != nil {
		t.Fatalf("call: %v", err)
	}
	messages := cap.JSON(t)["messages"].([]any)
	last := messages[len(messages)-1].(map[string]any)
	if last["role"] != "system" || last["content"] != "answer in json" {
		t.Fatalf("schema system prompt missing: %v", last)
	}
}

// --- responses -------------------------------------------------------------------

func TestOllamaTextResponse(t *testing.T) {
	srv, _ := captureServer(t, 200, `{
		"model": "llama3:latest",
		"message": {"role": "assistant", "content": "hello", "thinking": "hmm"},
		"done_reason": "stop",
		"prompt_eval_count": 11,
		"eval_count": 7
	}`)
	cl := startClient(t, NewOllama("llama3", WithHost(srv.URL)))
	conv, sent := newCaptureConv()
	req := &events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "hi"}}}

	resp, err := cl.Call(context.Background(), []events.Event{req}, conv, config.CallOptions{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if resp.Content() != "hello" || resp.Message == nil {
		t.Fatalf("content: %q", resp.Content())
	}
	if resp.Model != "llama3:latest" || resp.Provider != "ollama" {
		t.Fatalf("tracing: model=%q provider=%q", resp.Model, resp.Provider)
	}
	if resp.FinishReason != "stop" {
		t.Fatalf("finish reason: %q", resp.FinishReason)
	}
	want := events.Usage{PromptTokens: i64(11), CompletionTokens: i64(7), TotalTokens: i64(18)}
	if !reflect.DeepEqual(resp.Usage, want) {
		t.Fatalf("usage: %+v", resp.Usage)
	}
	if len(*sent) != 2 {
		t.Fatalf("want ModelReasoning + ModelMessage, got %v", *sent)
	}
	if (*sent)[0].(*events.ModelReasoning).Content != "hmm" {
		t.Fatalf("reasoning: %v", (*sent)[0])
	}
	if (*sent)[1].(*events.ModelMessage).Content != "hello" {
		t.Fatalf("message: %v", (*sent)[1])
	}
}

func TestOllamaUsageDefaultsToZero(t *testing.T) {
	srv, _ := captureServer(t, 200, `{"model": "m", "message": {"content": "x"}}`)
	cl := startClient(t, NewOllama("m", WithHost(srv.URL)))
	resp, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	// Missing counters count as zero (Python parity: `or 0`).
	want := events.Usage{PromptTokens: i64(0), CompletionTokens: i64(0), TotalTokens: i64(0)}
	if !reflect.DeepEqual(resp.Usage, want) {
		t.Fatalf("usage: %+v", resp.Usage)
	}
}

func TestOllamaToolCallResponse(t *testing.T) {
	srv, _ := captureServer(t, 200, `{
		"model": "llama3",
		"message": {"role": "assistant", "content": "", "tool_calls": [
			{"function": {"name": "lookup", "arguments": {"q": "go", "n": 2}}}
		]},
		"done_reason": "stop"
	}`)
	cl := startClient(t, NewOllama("llama3", WithHost(srv.URL)))
	resp, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
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
		t.Fatal("the port must synthesize a call id (Ollama returns none)")
	}
	// The wire object serializes to the event's arguments JSON string.
	var args map[string]any
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
		t.Fatalf("arguments must be a JSON string: %q (%v)", call.Arguments, err)
	}
	if args["q"] != "go" || args["n"] != float64(2) {
		t.Fatalf("args: %v", args)
	}
	if resp.Message != nil {
		t.Fatal("empty content must leave the message unset")
	}
}

func TestOllamaToolCallArgumentsDefault(t *testing.T) {
	srv, _ := captureServer(t, 200, `{"model": "m", "message": {"tool_calls": [
		{"function": {"name": "ping", "arguments": {}}},
		{"function": {"name": "pong", "arguments": null}}
	]}}`)
	cl := startClient(t, NewOllama("m", WithHost(srv.URL)))
	resp, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(resp.ToolCalls.Calls) != 2 {
		t.Fatalf("calls: %v", resp.ToolCalls.Calls)
	}
	for _, c := range resp.ToolCalls.Calls {
		if c.Arguments != "{}" {
			t.Fatalf("empty/missing arguments must become %q: %q", "{}", c.Arguments)
		}
	}
}

func TestOllamaHTTPError(t *testing.T) {
	srv, _ := captureServer(t, 404, `{"error": "model not found"}`)
	cl := startClient(t, NewOllama("m", WithHost(srv.URL)))
	_, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("want 404 in error, got %v", err)
	}
}

// --- tool round trip ---------------------------------------------------------------

func TestOllamaToolRoundTripReplay(t *testing.T) {
	srv, cap := captureServer(t, 200, `{"model": "m", "message": {"content": "done"}}`)
	cl := startClient(t, NewOllama("m", WithHost(srv.URL)))

	previous := &events.ModelResponse{
		Message: &events.ModelMessage{Content: "calling"},
		ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "call-1", Name: "lookup", Arguments: `{"q":"go"}`},
		}},
	}
	results := &events.ToolResultsEvent{Results: []events.ToolResultEvent{
		events.FromResultCall(previous.ToolCalls.Calls[0], events.NewToolResult(false, "found it")),
	}}
	req := &events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "hi"}}}

	if _, err := cl.Call(context.Background(), []events.Event{req, previous, results}, nil, config.CallOptions{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	messages := cap.JSON(t)["messages"].([]any)
	// system-less conv: user, assistant (with tool_calls), tool.
	if len(messages) != 3 {
		t.Fatalf("want 3 messages, got %d: %v", len(messages), messages)
	}
	assistant := messages[1].(map[string]any)
	if assistant["role"] != "assistant" || assistant["content"] != "calling" {
		t.Fatalf("assistant message: %v", assistant)
	}
	calls := assistant["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("replayed calls: %v", calls)
	}
	inner := calls[0].(map[string]any)["function"].(map[string]any)
	if inner["name"] != "lookup" {
		t.Fatalf("replayed call name: %v", inner)
	}
	// The wire carries the arguments as an OBJECT, not a string.
	if args, ok := inner["arguments"].(map[string]any); !ok || args["q"] != "go" {
		t.Fatalf("replayed arguments must be an object: %v (%T)", inner["arguments"], inner["arguments"])
	}

	toolMsg := messages[2].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["content"] != "found it" {
		t.Fatalf("tool message: %v", toolMsg)
	}
	if toolMsg["tool_name"] != "lookup" {
		t.Fatalf("tool_name: %v", toolMsg)
	}
}

func TestOllamaEmptyToolResultYieldsEmptyMessage(t *testing.T) {
	srv, cap := captureServer(t, 200, `{"message": {}}`)
	cl := startClient(t, NewOllama("m", WithHost(srv.URL)))
	call := events.NewToolCallEvent("calc")
	results := &events.ToolResultsEvent{Results: []events.ToolResultEvent{
		events.FromResultCall(call, events.ToolResult{Parts: []events.Input{}}),
	}}
	if _, err := cl.Call(context.Background(), []events.Event{results}, nil, config.CallOptions{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	messages := cap.JSON(t)["messages"].([]any)
	toolMsg := messages[0].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["content"] != "" {
		t.Fatalf("empty result must yield one empty tool message: %v", toolMsg)
	}
}

// --- unsupported inputs / tools -----------------------------------------------------

func TestOllamaUnsupportedInput(t *testing.T) {
	srv, _ := captureServer(t, 200, `{}`)
	cl := startClient(t, NewOllama("m", WithHost(srv.URL)))
	cases := []struct {
		name string
		part events.Input
	}{
		{"audio", &events.BinaryInput{Data: []byte{1}, MediaType: "audio/wav", Kind: events.BinaryAudio}},
		{"file id", &events.FileIdInput{FileID: "file-1"}},
	}
	for _, tc := range cases {
		req := &events.ModelRequest{Parts: []events.Input{tc.part}}
		if _, err := cl.Call(context.Background(), []events.Event{req}, nil, config.CallOptions{}); !errors.Is(err, config.ErrUnsupportedInput) {
			t.Fatalf("%s: want ErrUnsupportedInput, got %v", tc.name, err)
		}
	}
	// Audio inside a tool result fails the same way.
	results := &events.ToolResultsEvent{Results: []events.ToolResultEvent{
		events.FromResultCall(events.NewToolCallEvent("t"), events.ToolResult{Parts: []events.Input{
			&events.BinaryInput{Data: []byte{1}, MediaType: "audio/wav", Kind: events.BinaryAudio},
		}}),
	}}
	if _, err := cl.Call(context.Background(), []events.Event{results}, nil, config.CallOptions{}); !errors.Is(err, config.ErrUnsupportedInput) {
		t.Fatalf("tool result audio: want ErrUnsupportedInput, got %v", err)
	}
}

func TestOllamaUnsupportedTool(t *testing.T) {
	srv, _ := captureServer(t, 200, `{}`)
	cl := startClient(t, NewOllama("m", WithHost(srv.URL)))
	_, err := cl.Call(context.Background(), nil, nil, config.CallOptions{Tools: []any{fakeHosted{}}})
	if !errors.Is(err, config.ErrUnsupportedTool) {
		t.Fatalf("hosted tool: want ErrUnsupportedTool, got %v", err)
	}
}

// fakeProto is a minimal response.Proto for request-shape tests.
type fakeProto struct {
	name, description, prompt string
	schema                    map[string]any
}

func (f fakeProto) Name() string               { return f.name }
func (f fakeProto) Description() string        { return f.description }
func (f fakeProto) JSONSchema() map[string]any { return f.schema }
func (f fakeProto) SystemPrompt() string       { return f.prompt }
func (f fakeProto) Validate(s string, _ *agocontext.Context) (any, error) {
	return s, nil
}
