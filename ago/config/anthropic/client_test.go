package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ago/config"
	agocontext "ago/context"
	"ago/events"
	"ago/response"
	"ago/stream"
	"ago/tools"
)

func captureServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var requests []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		requests = append(requests, m)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

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

// newTestClient builds a config pointing at srv with a fixed API key.
func newTestClient(srv *httptest.Server, mutate func(*AnthropicConfig)) config.LLMClient {
	t := srv // not used; kept for symmetry
	_ = t
	cfg := NewAnthropic("claude-sonnet")
	cfg.APIKey = "test-key"
	cfg.BaseURL = srv.URL
	if mutate != nil {
		mutate(cfg)
	}
	cl, err := cfg.Create()
	if err != nil {
		panic(fmt.Sprintf("Create: %v", err))
	}
	return cl
}

const textResponse = `{
  "id": "msg_1",
  "model": "claude-sonnet-4",
  "type": "message",
  "role": "assistant",
  "content": [{"type": "text", "text": "Hi there"}],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 12, "output_tokens": 34}
}`

func TestCallText(t *testing.T) {
	conv, sent := newCaptureConv()
	var gotPath, gotMethod string
	srv, reqs := captureServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("x-api-key = %q", r.Header.Get("x-api-key"))
		}
		if v := r.Header.Get("anthropic-version"); v != APIVersion {
			t.Errorf("anthropic-version = %q", v)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, textResponse)
	})
	cl := newTestClient(srv, nil)

	resp, err := cl.Call(context.Background(), []events.Event{
		&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "hello"}}},
	}, conv, config.CallOptions{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if gotPath != messagesPath || gotMethod != http.MethodPost {
		t.Fatalf("request = %s %s", gotMethod, gotPath)
	}
	if len(*reqs) != 1 {
		t.Fatalf("got %d requests", len(*reqs))
	}
	body := (*reqs)[0]
	if body["model"] != "claude-sonnet" {
		t.Fatalf("model = %v", body["model"])
	}
	if body["max_tokens"] != float64(4096) {
		t.Fatalf("max_tokens = %v", body["max_tokens"])
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", msgs)
	}
	m := msgs[0].(map[string]any)
	if m["role"] != "user" || m["content"] != "hello" {
		t.Fatalf("user turn = %v", m)
	}

	if resp.Message == nil || resp.Message.Content != "Hi there" {
		t.Fatalf("response message = %+v", resp.Message)
	}
	if resp.FinishReason != "stop" {
		t.Fatalf("FinishReason = %q, want stop (end_turn)", resp.FinishReason)
	}
	if resp.ResponseID != "msg_1" || resp.Model != "claude-sonnet-4" || resp.Provider != "anthropic" {
		t.Fatalf("tracing = %q/%q/%q", resp.ResponseID, resp.Model, resp.Provider)
	}
	if resp.Usage.PromptTokens == nil || *resp.Usage.PromptTokens != 12 ||
		resp.Usage.CompletionTokens == nil || *resp.Usage.CompletionTokens != 34 ||
		resp.Usage.TotalTokens == nil || *resp.Usage.TotalTokens != 46 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	// The durable answer was announced on the stream.
	sentText := false
	for _, ev := range *sent {
		if m, ok := events.AsValue(ev).(events.ModelMessage); ok && m.Content == "Hi there" {
			sentText = true
		}
	}
	if !sentText {
		t.Fatalf("ModelMessage not sent: %+v", *sent)
	}
}

func TestCallSystemPromptAndSchemaPrompt(t *testing.T) {
	srv, reqs := captureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, textResponse)
	})
	conv, _ := newCaptureConv()
	conv.AddPrompt("you are", "helpful")
	// Caching off renders the system prompt as a plain string, which is the
	// easiest shape to assert the composition on.
	cl := newTestClient(srv, func(c *AnthropicConfig) { c.PromptCaching = false })

	schema := response.NewPromptedSchema(response.RawSchema(map[string]any{
		"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string"}},
	}))
	if _, err := cl.Call(context.Background(), []events.Event{
		&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "hello"}}},
	}, conv, config.CallOptions{ResponseSchema: schema}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	body := (*reqs)[0]
	system, ok := body["system"].(string)
	if !ok {
		t.Fatalf("system = %T", body["system"])
	}
	if !strings.HasPrefix(system, "you are\nhelpful") {
		t.Fatalf("system must join the prompt lines first, got %q", system)
	}
	if !strings.Contains(system, "json") {
		t.Fatalf("the schema's JSON requirement must be appended, got %q", system)
	}
}

func TestCallSamplingParams(t *testing.T) {
	srv, reqs := captureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, textResponse)
	})
	cl := newTestClient(srv, func(c *AnthropicConfig) {
		c.Temperature = f64ptr(0.25)
		c.TopP = f64ptr(0.9)
		c.StopSequences = []string{"END", "STOP"}
	})
	_, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	body := (*reqs)[0]
	if body["temperature"] != 0.25 || body["top_p"] != 0.9 {
		t.Fatalf("sampling params = %v/%v", body["temperature"], body["top_p"])
	}
	stops := body["stop_sequences"].([]any)
	if len(stops) != 2 || stops[0] != "END" || stops[1] != "STOP" {
		t.Fatalf("stop_sequences = %v", stops)
	}
}

func TestCallToolUseRoundTrip(t *testing.T) {
	toolResponse := `{
	  "id": "msg_2",
	  "model": "claude-sonnet-4",
	  "content": [{"type": "tool_use", "id": "toolu_9", "name": "get_weather", "input": {"city": "Paris", "unit": "c"}}],
	  "stop_reason": "tool_use",
	  "usage": {"input_tokens": 20, "output_tokens": 10}
	}`
	srv, reqs := captureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, toolResponse)
	})
	cl := newTestClient(srv, nil)

	resp, err := cl.Call(context.Background(), []events.Event{
		&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "weather in Paris?"}}},
	}, nil, config.CallOptions{
		Tools: []any{tools.Schema{
			Name:        "get_weather",
			Description: "current weather",
			Parameters:  map[string]any{"properties": map[string]any{"city": map[string]any{"type": "string"}}},
		}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.FinishReason != "tool_use" {
		t.Fatalf("FinishReason = %q", resp.FinishReason)
	}
	if len(resp.ToolCalls.Calls) != 1 {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	call := resp.ToolCalls.Calls[0]
	if call.ID != "toolu_9" || call.Name != "get_weather" || call.Arguments != `{"city": "Paris", "unit": "c"}` {
		t.Fatalf("call = %+v", call)
	}

	// The wire request carried the tool.
	wireTools := (*reqs)[0]["tools"].([]any)
	wt := wireTools[0].(map[string]any)
	if wt["name"] != "get_weather" || wt["description"] != "current weather" {
		t.Fatalf("wire tool = %v", wt)
	}
	schema := wt["input_schema"].(map[string]any)
	if schema["type"] != "object" {
		t.Fatalf("input_schema must be an object schema, got %v", schema)
	}

	// Replay the turn: assistant tool_use + user tool_result.
	history := []events.Event{
		&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "weather in Paris?"}}},
		resp,
		&events.ToolResultsEvent{Results: []events.ToolResultEvent{
			events.FromResultCall(call, events.NewToolResult(false, "sunny, 21C")),
		}},
	}
	if _, err := cl.Call(context.Background(), history, nil, config.CallOptions{}); err != nil {
		t.Fatalf("replay Call: %v", err)
	}
	msgs := (*reqs)[1]["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("replay turns = %d: %v", len(msgs), msgs)
	}
	assistant := msgs[1].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("turn 1 role = %v", assistant["role"])
	}
	blocks := assistant["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("assistant content = %v", blocks)
	}
	use := blocks[0].(map[string]any)
	if use["type"] != "tool_use" || use["id"] != "toolu_9" || use["name"] != "get_weather" {
		t.Fatalf("tool_use block = %v", use)
	}
	input := use["input"].(map[string]any)
	if input["city"] != "Paris" || input["unit"] != "c" {
		t.Fatalf("tool_use input = %v (arguments must arrive as JSON)", input)
	}
	user := msgs[2].(map[string]any)
	if user["role"] != "user" {
		t.Fatalf("turn 2 role = %v", user["role"])
	}
	results := user["content"].([]any)
	result := results[0].(map[string]any)
	if result["type"] != "tool_result" || result["tool_use_id"] != "toolu_9" || result["content"] != "sunny, 21C" {
		t.Fatalf("tool_result block = %v", result)
	}
}

func TestConvertMessagesShapes(t *testing.T) {
	t.Run("multi-part user turn", func(t *testing.T) {
		turns, err := ConvertMessages([]events.Event{&events.ModelRequest{Parts: []events.Input{
			&events.TextInput{Content: "look"},
			&events.DataInput{Data: map[string]any{"k": 1}},
			&events.BinaryInput{Data: []byte{1, 2, 3}, MediaType: "image/png", Kind: events.BinaryImage},
			&events.UrlInput{URL: "https://example.com/cat.png", Kind: events.BinaryImage},
		}}})
		if err != nil {
			t.Fatalf("ConvertMessages: %v", err)
		}
		if len(turns) != 1 {
			t.Fatalf("turns = %d", len(turns))
		}
		blocks := turns[0]["content"].([]map[string]any)
		if len(blocks) != 4 {
			t.Fatalf("content blocks = %d", len(blocks))
		}
		if blocks[0]["type"] != "text" || blocks[0]["text"] != "look" {
			t.Fatalf("text block = %v", blocks[0])
		}
		if blocks[1]["type"] != "text" || blocks[1]["text"] != `{"k":1}` {
			t.Fatalf("DataInput must travel as JSON text, got %v", blocks[1])
		}
		img := blocks[2]
		if img["type"] != "image" {
			t.Fatalf("image block = %v", img)
		}
		src := img["source"].(map[string]any)
		if src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] != "AQID" {
			t.Fatalf("base64 source = %v", src)
		}
		url := blocks[3]
		if url["type"] != "image" || url["source"].(map[string]any)["type"] != "url" ||
			url["source"].(map[string]any)["url"] != "https://example.com/cat.png" {
			t.Fatalf("url source = %v", url)
		}
	})

	t.Run("compaction summary", func(t *testing.T) {
		turns, err := ConvertMessages([]events.Event{&events.CompactionSummary{Content: "earlier stuff"}})
		if err != nil {
			t.Fatalf("ConvertMessages: %v", err)
		}
		if turns[0]["role"] != "user" || turns[0]["content"] != "[Summary of earlier conversation]\nearlier stuff" {
			t.Fatalf("summary turn = %v", turns[0])
		}
	})

	t.Run("orphan tool_use dropped, orphan tool_result dropped", func(t *testing.T) {
		resolved := events.NewToolCallEvent("ok")
		resolved.ID = "call-ok"
		unresolved := events.NewToolCallEvent("lost")
		unresolved.ID = "call-lost"
		history := []events.Event{
			&events.ModelResponse{
				Message:   &events.ModelMessage{Content: "working"},
				ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{resolved, unresolved}},
			},
			// The result for "call-lost" never persisted; an extra result for a
			// trimmed call arrives too.
			&events.ToolResultsEvent{Results: []events.ToolResultEvent{
				events.FromResultCall(resolved, events.NewToolResult(false, "done")),
				{ParentID: "call-trimmed", Result: events.NewToolResult(false, "ghost")},
			}},
		}
		turns, err := ConvertMessages(history)
		if err != nil {
			t.Fatalf("ConvertMessages: %v", err)
		}
		if len(turns) != 2 {
			t.Fatalf("turns = %d: %v", len(turns), turns)
		}
		assistant := turns[0]["content"].([]map[string]any)
		if len(assistant) != 2 || assistant[0]["type"] != "text" || assistant[1]["id"] != "call-ok" {
			t.Fatalf("assistant turn = %v (orphan tool_use must be dropped, text kept)", assistant)
		}
		user := turns[1]["content"].([]map[string]any)
		if len(user) != 1 || user[0]["tool_use_id"] != "call-ok" {
			t.Fatalf("user turn = %v (orphan tool_result must be dropped)", user)
		}
	})

	t.Run("loose result event not duplicated by the wrapper", func(t *testing.T) {
		call := events.NewToolCallEvent("f")
		call.ID = "call-1"
		result := events.FromResultCall(call, events.NewToolResult(false, "out"))
		history := []events.Event{
			&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{call}}},
			result, // the tool's own event (persisted before the batch)
			&events.ToolResultsEvent{Results: []events.ToolResultEvent{result}},
		}
		turns, err := ConvertMessages(history)
		if err != nil {
			t.Fatalf("ConvertMessages: %v", err)
		}
		if len(turns) != 2 {
			t.Fatalf("turns = %d: %v", len(turns), turns)
		}
		user := turns[1]["content"].([]map[string]any)
		if len(user) != 1 {
			t.Fatalf("duplicate tool_result emitted: %v", user)
		}
	})

	t.Run("loose error event carries is_error", func(t *testing.T) {
		call := events.NewToolCallEvent("f")
		call.ID = "call-e"
		history := []events.Event{
			&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{call}}},
			events.FromErrorCall(call, errors.New("boom")), // the wrapper was lost
		}
		turns, err := ConvertMessages(history)
		if err != nil {
			t.Fatalf("ConvertMessages: %v", err)
		}
		user := turns[1]["content"].([]map[string]any)
		if user[0]["is_error"] != true || user[0]["content"] != "boom" {
			t.Fatalf("error tool_result = %v", user[0])
		}
	})
}

func TestConvertMessagesUnsupportedInputs(t *testing.T) {
	cases := []struct {
		name string
		part events.Input
	}{
		{"binary audio", &events.BinaryInput{Data: []byte{1}, MediaType: "audio/wav", Kind: events.BinaryAudio}},
		{"binary document without pdf media type", &events.BinaryInput{Data: []byte{1}, MediaType: "text/html", Kind: events.BinaryDocument}},
		{"image with unsupported media type", &events.BinaryInput{Data: []byte{1}, MediaType: "image/bmp", Kind: events.BinaryImage}},
		{"url video", &events.UrlInput{URL: "https://x/v.mp4", Kind: events.BinaryVideo}},
		{"file id", &events.FileIdInput{FileID: "file-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ConvertMessages([]events.Event{&events.ModelRequest{Parts: []events.Input{tc.part}}})
			if !errors.Is(err, config.ErrUnsupportedInput) {
				t.Fatalf("want ErrUnsupportedInput, got %v", err)
			}
		})
	}
}

func TestConvertToolsHostedSchemaRejected(t *testing.T) {
	hosted := fakeToolSchema{schemaType: "web_search"}
	_, err := ConvertTools([]any{hosted})
	if !errors.Is(err, config.ErrUnsupportedTool) {
		t.Fatalf("want ErrUnsupportedTool, got %v", err)
	}
	_, err = ConvertTools([]any{42})
	if !errors.Is(err, config.ErrUnsupportedTool) {
		t.Fatalf("want ErrUnsupportedTool, got %v", err)
	}
}

func TestConvertToolsDeferLoading(t *testing.T) {
	got, err := ConvertTools([]any{tools.Schema{Name: "lazy", DeferLoading: true}})
	if err != nil {
		t.Fatalf("ConvertTools: %v", err)
	}
	if got[0]["defer_loading"] != true {
		t.Fatalf("defer_loading = %v", got[0]["defer_loading"])
	}
}

type fakeToolSchema struct{ schemaType string }

func (f fakeToolSchema) SchemaType() string { return f.schemaType }

const sseStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_s","model":"claude-sonnet-4","role":"assistant","content":[],"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Paris\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":25}}

event: message_stop
data: {"type":"message_stop"}
`

func TestCallStream(t *testing.T) {
	conv, sent := newCaptureConv()
	srv, reqs := captureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseStream)
	})
	cl := newTestClient(srv, func(c *AnthropicConfig) { c.Streaming = true })

	resp, err := cl.Call(context.Background(), []events.Event{
		&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "hello"}}},
	}, conv, config.CallOptions{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if body := (*reqs)[0]; body["stream"] != true {
		t.Fatalf("stream flag = %v", body["stream"])
	}
	if resp.Message == nil || resp.Message.Content != "Hello world" {
		t.Fatalf("accumulated message = %+v", resp.Message)
	}
	if len(resp.ToolCalls.Calls) != 1 {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	call := resp.ToolCalls.Calls[0]
	if call.ID != "toolu_1" || call.Name != "get_weather" || call.Arguments != `{"city":"Paris"}` {
		t.Fatalf("streamed call = %+v", call)
	}
	if resp.FinishReason != "tool_use" {
		t.Fatalf("FinishReason = %q", resp.FinishReason)
	}
	if resp.ResponseID != "msg_s" || resp.Model != "claude-sonnet-4" || resp.Provider != "anthropic" {
		t.Fatalf("tracing = %q/%q/%q", resp.ResponseID, resp.Model, resp.Provider)
	}
	// input_tokens from message_start, output_tokens from message_delta.
	if resp.Usage.PromptTokens == nil || *resp.Usage.PromptTokens != 10 ||
		resp.Usage.CompletionTokens == nil || *resp.Usage.CompletionTokens != 25 ||
		resp.Usage.TotalTokens == nil || *resp.Usage.TotalTokens != 35 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	// The chunks and the final message reached the conversation.
	var chunks []string
	for _, ev := range *sent {
		switch e := events.AsValue(ev).(type) {
		case events.ModelMessageChunk:
			chunks = append(chunks, e.Content)
		}
	}
	if len(chunks) != 2 || chunks[0] != "Hello" || chunks[1] != " world" {
		t.Fatalf("chunks = %v", chunks)
	}
}

func TestCallStreamUsageFinalShape(t *testing.T) {
	// Real stream shape (Python fixture parity): message_start carries the
	// input side with an output_tokens:1 placeholder, message_delta the final
	// output side. The final usage replaces the output fields — accumulating
	// them would double-count the placeholder (input 100, output 300 must not
	// read 301).
	srv, _ := captureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\n"+
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_u\",\"model\":\"claude-sonnet-4\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":100,\"output_tokens\":1,\"output_tokens_details\":{\"thinking_tokens\":0}}}}\n\n"+
			"event: message_delta\n"+
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":300,\"output_tokens_details\":{\"thinking_tokens\":50}}}\n\n"+
			"event: message_stop\n"+
			"data: {\"type\":\"message_stop\"}\n\n")
	})
	cl := newTestClient(srv, func(c *AnthropicConfig) { c.Streaming = true })

	resp, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	u := resp.Usage
	if u.PromptTokens == nil || *u.PromptTokens != 100 ||
		u.CompletionTokens == nil || *u.CompletionTokens != 300 ||
		u.TotalTokens == nil || *u.TotalTokens != 400 {
		t.Fatalf("usage = %+v, want 100/300/400", u)
	}
	if u.ThinkingTokens == nil || *u.ThinkingTokens != 50 {
		t.Fatalf("thinking tokens = %v, want 50 (the delta replaces the start's placeholder)", u.ThinkingTokens)
	}
}

func TestCallStreamInBandError(t *testing.T) {
	// An `error` event mid-stream (e.g. overloaded_error) fails the call —
	// Python parity: the SDK iterator raises instead of returning an empty
	// response.
	srv, _ := captureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\n"+
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_e\",\"model\":\"claude-sonnet-4\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n"+
			"event: content_block_delta\n"+
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n"+
			"event: error\n"+
			"data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n")
	})
	cl := newTestClient(srv, func(c *AnthropicConfig) { c.Streaming = true })

	_, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
	if !errors.Is(err, config.ErrStreamError) {
		t.Fatalf("in-band error event must fail the call, got %v", err)
	}
	if !strings.Contains(err.Error(), "overloaded_error") || !strings.Contains(err.Error(), "Overloaded") {
		t.Fatalf("error must carry the API detail, got %v", err)
	}
}

func TestCallCacheControl(t *testing.T) {
	t.Run("enabled", func(t *testing.T) {
		srv, reqs := captureServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, textResponse)
		})
		conv, _ := newCaptureConv()
		conv.AddPrompt("sys")
		cl := newTestClient(srv, func(c *AnthropicConfig) {
			c.PromptCaching = true
		})
		_, err := cl.Call(context.Background(), nil, conv, config.CallOptions{Tools: []any{
			tools.Schema{Name: "a"}, tools.Schema{Name: "b"},
		}})
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		body := (*reqs)[0]
		system, ok := body["system"].([]any)
		if !ok || len(system) != 1 {
			t.Fatalf("system = %v, want one text block", body["system"])
		}
		block := system[0].(map[string]any)
		if block["type"] != "text" || block["text"] != "sys" {
			t.Fatalf("system block = %v", block)
		}
		cc := block["cache_control"].(map[string]any)
		if cc["type"] != "ephemeral" {
			t.Fatalf("system cache_control = %v", cc)
		}
		wireTools := body["tools"].([]any)
		if _, has := wireTools[0].(map[string]any)["cache_control"]; has {
			t.Fatal("only the last tool carries cache_control")
		}
		last := wireTools[1].(map[string]any)["cache_control"].(map[string]any)
		if last["type"] != "ephemeral" {
			t.Fatalf("last tool cache_control = %v", last)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		srv, reqs := captureServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, textResponse)
		})
		conv, _ := newCaptureConv()
		conv.AddPrompt("sys")
		cl := newTestClient(srv, func(c *AnthropicConfig) { c.PromptCaching = false })
		_, err := cl.Call(context.Background(), nil, conv, config.CallOptions{Tools: []any{
			tools.Schema{Name: "a"},
		}})
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		body := (*reqs)[0]
		if body["system"] != "sys" {
			t.Fatalf("system = %v, want a plain string", body["system"])
		}
		wireTools := body["tools"].([]any)
		if _, has := wireTools[0].(map[string]any)["cache_control"]; has {
			t.Fatal("cache_control leaked with PromptCaching disabled")
		}
	})
}

func TestCallAPIError(t *testing.T) {
	srv, _ := captureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: Field required"}}`)
	})
	cl := newTestClient(srv, nil)
	_, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "max_tokens: Field required") {
		t.Fatalf("error should carry the API message, got %v", err)
	}
}

func TestNormalizeUsage(t *testing.T) {
	u := NormalizeUsage(wireUsage{
		InputTokens:              i64ptr(100),
		OutputTokens:             i64ptr(20),
		CacheReadInputTokens:     i64ptr(80),
		CacheCreationInputTokens: i64ptr(5),
	})
	if *u.PromptTokens != 100 || *u.CompletionTokens != 20 || *u.TotalTokens != 120 {
		t.Fatalf("basic counts = %+v", u)
	}
	if *u.CacheReadInputTokens != 80 || *u.CacheCreationInputTokens != 5 {
		t.Fatalf("cache counts = %+v", u)
	}
	if u.ThinkingTokens != nil {
		t.Fatalf("thinking_tokens must be absent, got %v", *u.ThinkingTokens)
	}
	empty := NormalizeUsage(wireUsage{})
	if empty.PromptTokens != nil || empty.CompletionTokens != nil || empty.TotalTokens != nil {
		t.Fatalf("absent counts stay nil, got %+v", empty)
	}
	withThinking := NormalizeUsage(wireUsage{OutputTokens: i64ptr(9), OutputTokensDetails: &struct {
		ThinkingTokens *int64 `json:"thinking_tokens"`
	}{ThinkingTokens: i64ptr(3)}})
	if withThinking.ThinkingTokens == nil || *withThinking.ThinkingTokens != 3 {
		t.Fatalf("thinking_tokens = %v", withThinking.ThinkingTokens)
	}
}

func TestMapStopReason(t *testing.T) {
	cases := map[string]string{
		"end_turn":   "stop",
		"tool_use":   "tool_use",
		"max_tokens": "length",
		"pause_turn": "pause_turn", // unknown reasons pass through
	}
	for in, want := range cases {
		if got := MapStopReason(in); got != want {
			t.Fatalf("MapStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}
