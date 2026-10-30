package openai

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

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/response"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
)

// responsesUsage is the usage fixture of the AG2 hosted-tool tests (helpers.py).
var responsesUsage = map[string]any{
	"input_tokens":  100,
	"output_tokens": 20,
	"total_tokens":  120,
	"input_tokens_details": map[string]any{
		"cached_tokens":      80,
		"cache_write_tokens": 7,
	},
	"output_tokens_details": map[string]any{
		"reasoning_tokens": 10,
	},
}

// completedResponse is one Responses payload without output items (the shape
// the stream's response.created / response.completed events carry).
func completedResponse() map[string]any {
	return map[string]any{
		"id": "resp_1", "object": "response", "created_at": 0,
		"model": "gpt-test-actual", "status": "completed",
		"parallel_tool_calls": true, "tool_choice": "auto",
		"tools": []any{}, "output": []any{}, "usage": responsesUsage,
	}
}

// responsesBody renders one Responses payload with the given output items
// (AG2 helpers.py response()).
func responsesBody(output ...map[string]any) string {
	raw := completedResponse()
	raw["output"] = output
	b, err := json.Marshal(raw)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// sseBody renders one payload per event as an SSE stream body (AG2
// helpers.py sse).
func sseBody(payloads ...map[string]any) string {
	var sb strings.Builder
	for _, p := range payloads {
		b, err := json.Marshal(p)
		if err != nil {
			panic(err)
		}
		typ, _ := p["type"].(string)
		fmt.Fprintf(&sb, "event: %s\ndata: %s\n\n", typ, b)
	}
	return sb.String()
}

// replayServer answers every request with the next body (the last one
// repeats) and records every request body in order. It returns the server URL.
func replayServer(t *testing.T, contentType string, bodies []string, seen *[][]byte) string {
	t.Helper()
	idx := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		*seen = append(*seen, b)
		w.Header().Set("Content-Type", contentType)
		if idx >= len(bodies) {
			idx = len(bodies) - 1
		}
		fmt.Fprint(w, bodies[idx])
		idx++
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// responsesCall creates the client and performs one call over a fresh
// capturing conversation (prompted "be brief"). It returns the response and
// every event the client published; a failing call fails the test.
func responsesCall(t *testing.T, cfg *OpenAIResponsesConfig, history []events.Event, opts config.CallOptions) (*events.ModelResponse, *[]events.Event) {
	t.Helper()
	resp, sent, err := responsesCallErr(t, cfg, history, opts)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	return resp, sent
}

// responsesCallErr is responsesCall for tests that exercise the error path.
func responsesCallErr(t *testing.T, cfg *OpenAIResponsesConfig, history []events.Event, opts config.CallOptions) (*events.ModelResponse, *[]events.Event, error) {
	t.Helper()
	ms := stream.NewMemoryStream()
	var sent []events.Event
	ms.Subscribe(func(ev events.Event) error {
		sent = append(sent, ev)
		return nil
	}, stream.SubscribeOptions{})
	conv := agocontext.New(ms)
	conv.AddPrompt("be brief")
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	resp, err := client.Call(context.Background(), history, conv, opts)
	return resp, &sent, err
}

func wantToken(t *testing.T, got *int64, want int64, label string) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("usage %s: want %d, got %v", label, want, got)
	}
}

// --- config contract -------------------------------------------------------------

func TestNewOpenAIResponsesDefaults(t *testing.T) {
	cfg := NewOpenAIResponses("gpt-5")
	if cfg.Model() != "gpt-5" || cfg.Provider() != config.ProviderOpenAI {
		t.Fatalf("identity: %q %q", cfg.Model(), cfg.Provider())
	}
	if cfg.MaxRetries != 4 {
		t.Fatalf("MaxRetries default must be 4, got %d", cfg.MaxRetries)
	}
	if cfg.Streaming {
		t.Fatal("Streaming default must be false")
	}
	if cfg.Store != nil || cfg.ParallelToolCalls != nil || cfg.Temperature != nil || cfg.MaxOutputTokens != nil {
		t.Fatalf("pointer defaults must be nil: %+v", cfg)
	}
	var mc config.ModelConfig = cfg
	if mc.Model() != "gpt-5" {
		t.Fatalf("ModelConfig contract mismatch: %q", mc.Model())
	}
}

func TestResponsesCreateNotProvided(t *testing.T) {
	t.Setenv(envAPIKey, "")
	if _, err := NewOpenAIResponses("").Create(); !errors.Is(err, config.ErrNotProvided) {
		t.Fatalf("no model: want ErrNotProvided, got %v", err)
	}
	cfg := NewOpenAIResponses("gpt-5")
	if _, err := cfg.Create(); !errors.Is(err, config.ErrNotProvided) {
		t.Fatalf("no key: want ErrNotProvided, got %v", err)
	}
}

func TestResponsesCloneOverrides(t *testing.T) {
	cfg := NewOpenAIResponses("gpt-5")
	cfg.APIKey = "k"
	cfg.Temperature = f64(0.2)
	cfg.Streaming = true

	cp, err := cfg.Clone(map[string]any{"Model": "gpt-5-mini", "temperature": f64(0.8), "streaming": false, "api_key": nil})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if cp.Model() != "gpt-5-mini" {
		t.Fatalf("clone model: %q", cp.Model())
	}
	cloned := cp.(*OpenAIResponsesConfig)
	if cloned.APIKey != "" {
		t.Fatalf("clone api key must be cleared, got %q", cloned.APIKey)
	}
	if cloned.Temperature == nil || *cloned.Temperature != 0.8 || cloned.Streaming {
		t.Fatalf("clone overrides not applied: %+v", cloned)
	}
	// The original stays untouched.
	if cfg.Model() != "gpt-5" || *cfg.Temperature != 0.2 || !cfg.Streaming || cfg.APIKey != "k" {
		t.Fatalf("original mutated: %+v", cfg)
	}
	if _, err := cfg.Clone(map[string]any{"nope": 1}); err == nil {
		t.Fatal("unknown override key must error")
	}
}

// --- non-streaming round trip with a tool call -------------------------------------

func TestResponsesNonStreamToolRoundTrip(t *testing.T) {
	body := responsesBody(
		map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": "checking the weather", "annotations": []any{}}},
		},
		map[string]any{
			"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": `{"city":"SF"}`,
		},
	)
	srv, got := captureServer(t, "application/json", http.StatusOK, body)

	cfg := NewOpenAIResponses("gpt-test")
	cfg.APIKey = "test-key"
	cfg.BaseURL = srv.URL + "/v1"
	cfg.Organization = "org-1"

	weather := tools.Schema{Name: "get_weather", Description: "w", Parameters: map[string]any{
		"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}},
	}}
	req := events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "weather?"}}}

	resp, sent := responsesCall(t, cfg, []events.Event{&req}, config.CallOptions{Tools: []any{weather}})

	// Request wire shape.
	if got.path != "/v1/responses" {
		t.Fatalf("path: %q", got.path)
	}
	if auth := got.header.Get("Authorization"); auth != "Bearer test-key" {
		t.Fatalf("auth header: %q", auth)
	}
	if org := got.header.Get("OpenAI-Organization"); org != "org-1" {
		t.Fatalf("org header: %q", org)
	}
	b := bodyJSON(t, got.body)
	if b["model"] != "gpt-test" || b["store"] != true || b["parallel_tool_calls"] != true {
		t.Fatalf("model/store/parallel defaults not sent: %+v", b)
	}
	if b["instructions"] != "be brief" {
		t.Fatalf("instructions not sent: %+v", b["instructions"])
	}
	input := b["input"].([]any)
	first := input[0].(map[string]any)
	if first["role"] != "user" {
		t.Fatalf("first input item must be the user turn: %+v", first)
	}
	part := first["content"].([]any)[0].(map[string]any)
	if part["type"] != "input_text" || part["text"] != "weather?" {
		t.Fatalf("user text part: %+v", part)
	}
	wire := wireTools(b)
	if len(wire) != 1 {
		t.Fatalf("tools not sent: %+v", wire)
	}
	fn := wire[0].(map[string]any)
	if fn["type"] != "function" || fn["name"] != "get_weather" || fn["description"] != "w" {
		t.Fatalf("function tool wire shape: %+v", fn)
	}
	params := fn["parameters"].(map[string]any)
	if params["type"] != "object" || params["additionalProperties"] != false {
		t.Fatalf("parameters not object-normalized: %+v", params)
	}
	if _, ok := b["text"]; ok {
		t.Fatal("no response schema: text config must not be sent")
	}

	// Response parse.
	if resp.ResponseID != "resp_1" || resp.Model != "gpt-test-actual" || resp.FinishReason != "completed" {
		t.Fatalf("tracing fields: %+v", resp)
	}
	if resp.Provider != "openai" {
		t.Fatalf("provider: %q", resp.Provider)
	}
	if resp.Message == nil || resp.Message.Content != "checking the weather" {
		t.Fatalf("message: %+v", resp.Message)
	}
	if len(resp.ToolCalls.Calls) != 1 {
		t.Fatalf("tool calls: %+v", resp.ToolCalls)
	}
	call := resp.ToolCalls.Calls[0]
	if call.ID != "call_1" || call.Name != "get_weather" || call.Arguments != `{"city":"SF"}` {
		t.Fatalf("tool call: %+v", call)
	}
	wantToken(t, resp.Usage.PromptTokens, 100, "prompt")
	wantToken(t, resp.Usage.CompletionTokens, 20, "completion")
	wantToken(t, resp.Usage.TotalTokens, 120, "total")
	wantToken(t, resp.Usage.CacheReadInputTokens, 80, "cache read")
	wantToken(t, resp.Usage.CacheCreationInputTokens, 7, "cache write")
	wantToken(t, resp.Usage.ThinkingTokens, 10, "thinking")

	// The message also went out on the stream.
	if len(*sent) != 1 {
		t.Fatalf("stream events: %d", len(*sent))
	}
	if msg, ok := (*sent)[0].(*events.ModelMessage); !ok || msg.Content != "checking the weather" {
		t.Fatalf("stream event: %+v", (*sent)[0])
	}
}

// --- streaming round trip with a tool call ------------------------------------------

func TestResponsesStreamToolRoundTrip(t *testing.T) {
	completed := completedResponse()
	body := sseBody(
		map[string]any{"type": "response.created", "response": completed},
		map[string]any{"type": "response.output_text.delta", "delta": "checking "},
		map[string]any{"type": "response.output_text.delta", "delta": "the weather"},
		map[string]any{"type": "response.output_item.done", "item": map[string]any{
			"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": `{"city":"SF"}`,
		}},
		map[string]any{"type": "response.completed", "response": completed},
	)
	srv, got := captureServer(t, "text/event-stream", http.StatusOK, body)

	cfg := NewOpenAIResponses("gpt-test")
	cfg.APIKey = "test-key"
	cfg.BaseURL = srv.URL + "/v1"
	cfg.Streaming = true

	req := events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "weather?"}}}
	resp, sent := responsesCall(t, cfg, []events.Event{&req}, config.CallOptions{})

	if got.path != "/v1/responses" {
		t.Fatalf("path: %q", got.path)
	}
	b := bodyJSON(t, got.body)
	if b["stream"] != true {
		t.Fatal("streaming request must carry stream=true")
	}

	// Two chunks, then the combined message.
	chunks := 0
	var combined strings.Builder
	for _, ev := range *sent {
		switch e := events.AsValue(ev).(type) {
		case events.ModelMessageChunk:
			chunks++
			combined.WriteString(e.Content)
		case events.ModelMessage:
			if e.Content != "checking the weather" {
				t.Fatalf("final message: %+v", e)
			}
		default:
			t.Fatalf("unexpected stream event %T", ev)
		}
	}
	if chunks != 2 || combined.String() != "checking the weather" {
		t.Fatalf("chunks: %d %q", chunks, combined.String())
	}
	if resp.Message == nil || resp.Message.Content != "checking the weather" {
		t.Fatalf("response message: %+v", resp.Message)
	}
	if len(resp.ToolCalls.Calls) != 1 || resp.ToolCalls.Calls[0].ID != "call_1" {
		t.Fatalf("stream tool call: %+v", resp.ToolCalls)
	}
	if resp.FinishReason != "completed" || resp.ResponseID != "resp_1" || resp.Model != "gpt-test-actual" {
		t.Fatalf("tracing fields: %+v", resp)
	}
	wantToken(t, resp.Usage.TotalTokens, 120, "total")
}

// --- hosted web_search: wire schema, events and replay -------------------------------

func TestResponsesHostedWebSearch(t *testing.T) {
	// The response carries a hosted web_search_call with its sources (present
	// because the request asked for the include).
	searchItem := map[string]any{
		"id": "ws_1", "type": "web_search_call", "status": "completed",
		"action": map[string]any{
			"type": "search", "queries": []any{"ago"},
			"sources": []any{map[string]any{"type": "url", "url": "https://ago.example"}},
		},
	}
	payload := responsesBody(searchItem)
	var seen [][]byte
	srvURL := replayServer(t, "application/json", []string{payload, payload}, &seen)

	cfg := NewOpenAIResponses("gpt-test")
	cfg.APIKey = "test-key"
	cfg.BaseURL = srvURL + "/v1"
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	ms := stream.NewMemoryStream()
	var sent []events.Event
	ms.Subscribe(func(ev events.Event) error {
		sent = append(sent, ev)
		return nil
	}, stream.SubscribeOptions{})
	conv := agocontext.New(ms)

	maxUses := 3
	webSearch := tools.WebSearchToolSchema{
		SearchContextSize: "low",
		MaxUses:           &maxUses,
		UserLocation:      &tools.UserLocation{City: strPtr("San Francisco")},
		AllowedDomains:    []string{"wikipedia.org"},
	}
	req := events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "search"}}}

	// Turn 1: the hosted schema maps into tools[] and the item comes back.
	if _, err := client.Call(context.Background(), []events.Event{&req}, conv, config.CallOptions{Tools: []any{webSearch}}); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	first := bodyJSON(t, seen[0])
	wire := wireTools(first)
	if len(wire) != 1 {
		t.Fatalf("hosted tool not sent: %+v", wire)
	}
	ws := wire[0].(map[string]any)
	if ws["type"] != "web_search" || ws["search_context_size"] != "low" || ws["max_uses"] != float64(3) {
		t.Fatalf("web_search wire shape: %+v", ws)
	}
	loc := ws["user_location"].(map[string]any)
	if loc["type"] != "approximate" || loc["city"] != "San Francisco" {
		t.Fatalf("user_location wire shape: %+v", loc)
	}
	if _, has := loc["region"]; has {
		t.Fatal("unset location fields must not travel")
	}
	filters := ws["filters"].(map[string]any)
	if domains := filters["allowed_domains"].([]any); len(domains) != 1 || domains[0] != "wikipedia.org" {
		t.Fatalf("filters wire shape: %+v", filters)
	}
	includes := first["include"].([]any)
	if len(includes) != 1 || includes[0] != "web_search_call.action.sources" {
		t.Fatalf("include: %+v", includes)
	}

	// The hosted item became a call/result event pair on the stream.
	var callEv *events.BuiltinToolCallEvent
	var resultEv *events.BuiltinToolResultEvent
	for i := range sent {
		switch e := events.AsValue(sent[i]).(type) {
		case events.BuiltinToolCallEvent:
			callEv = &e
		case events.BuiltinToolResultEvent:
			resultEv = &e
		}
	}
	if callEv == nil || callEv.ID != "ws_1" || callEv.Name != tools.WebSearchToolName {
		t.Fatalf("builtin call event: %+v", callEv)
	}
	if callEv.Arguments == "" || !strings.Contains(callEv.Arguments, `"search"`) {
		t.Fatalf("call arguments must carry the action: %q", callEv.Arguments)
	}
	if _, ok := callEv.VendorMetadata["item"].(map[string]any); !ok {
		t.Fatalf("call event must keep the raw item for replay: %+v", callEv.VendorMetadata)
	}
	if resultEv == nil || resultEv.ParentID != "ws_1" || resultEv.Name != tools.WebSearchToolName {
		t.Fatalf("builtin result event: %+v", resultEv)
	}
	if len(resultEv.Result.Parts) != 1 {
		t.Fatalf("result parts: %+v", resultEv.Result.Parts)
	}
	if u, ok := events.AsValue(resultEv.Result.Parts[0]).(events.UrlInput); !ok || u.URL != "https://ago.example" {
		t.Fatalf("result part: %+v", resultEv.Result.Parts[0])
	}
	if resultEv.Result.Metadata["action_type"] != "search" || resultEv.Result.Metadata["status"] != "completed" {
		t.Fatalf("result metadata: %+v", resultEv.Result.Metadata)
	}

	// Turn 2: the hosted item replays into input[] (with the original request
	// and the events of turn 1 as the history).
	history := []events.Event{&req, callEv, resultEv}
	if _, err := client.Call(context.Background(), history, conv, config.CallOptions{Tools: []any{webSearch}}); err != nil {
		t.Fatalf("call 2: %v", err)
	}
	second := bodyJSON(t, seen[1])
	input := second["input"].([]any)
	var replayed map[string]any
	for _, item := range input {
		if m, ok := item.(map[string]any); ok && m["type"] == "web_search_call" {
			replayed = m
		}
	}
	if replayed == nil {
		t.Fatalf("web_search_call not replayed: %+v", input)
	}
	if replayed["id"] != "ws_1" || replayed["status"] != "completed" {
		t.Fatalf("replayed item: %+v", replayed)
	}
	if _, ok := replayed["action"].(map[string]any); !ok {
		t.Fatalf("replayed item lost its action: %+v", replayed)
	}
}

// --- input-mapping details ------------------------------------------------------------

func TestResponsesInputMapping(t *testing.T) {
	history := []events.Event{
		&events.ModelRequest{Parts: []events.Input{
			&events.TextInput{Content: "read this"},
			&events.FileIdInput{FileID: "file-1", Filename: "ignored.pdf"},
		}},
		&events.CompactionSummary{Content: "earlier stuff"},
		&events.ModelResponse{Message: &events.ModelMessage{Content: "hi"},
			ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{{ID: "call_9", Name: "f", Arguments: "{}"}}}},
		&events.ToolResultsEvent{Results: []events.ToolResultEvent{
			{ParentID: "call_9", Result: events.ToolResult{Parts: []events.Input{&events.TextInput{Content: "result text"}}}},
			{ParentID: "call_8", Result: events.ToolResult{Parts: []events.Input{
				&events.TextInput{Content: "a"}, &events.DataInput{Data: map[string]any{"b": 1}},
			}}},
		}},
	}
	out, err := responsesInput(history)
	if err != nil {
		t.Fatalf("responsesInput: %v", err)
	}
	// 1: two user messages (one per part), 2: summary, 3: assistant + function_call,
	// 4: two function_call_outputs.
	if len(out) != 7 {
		t.Fatalf("input item count: %d: %+v", len(out), out)
	}
	u0 := out[0].(map[string]any)
	if u0["role"] != "user" {
		t.Fatalf("item 0: %+v", u0)
	}
	textPart := u0["content"].([]any)[0].(map[string]any)
	if textPart["type"] != "input_text" || textPart["text"] != "read this" {
		t.Fatalf("text part: %+v", textPart)
	}
	// Each part travels as its own user message: the file reference is item 1.
	u1 := out[1].(map[string]any)
	filePart := u1["content"].([]any)[0].(map[string]any)
	// A file_id reference carries no filename (API mutual-exclusion).
	if filePart["type"] != "input_file" || filePart["file_id"] != "file-1" {
		t.Fatalf("FileIdInput part: %+v", filePart)
	}
	if _, has := filePart["filename"]; has {
		t.Fatal("file_id must not carry a filename")
	}

	summary := out[2].(map[string]any)
	text := summary["content"].([]any)[0].(map[string]any)
	if !strings.HasPrefix(text["text"].(string), "[Summary of earlier conversation]") {
		t.Fatalf("summary part: %+v", text)
	}

	assistant := out[3].(map[string]any)
	if assistant["role"] != "assistant" || assistant["content"] != "hi" {
		t.Fatalf("assistant item: %+v", assistant)
	}
	fc := out[4].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_9" || fc["name"] != "f" {
		t.Fatalf("function_call item: %+v", fc)
	}

	// A single text result collapses to a plain string (the common wire form).
	single := out[5].(map[string]any)
	if single["type"] != "function_call_output" || single["call_id"] != "call_9" || single["output"] != "result text" {
		t.Fatalf("single-part output: %+v", single)
	}
	// Multiple parts travel as a block array.
	multi := out[6].(map[string]any)
	blocks := multi["output"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("multi-part output: %+v", blocks)
	}
	if blocks[1].(map[string]any)["text"] != `{"b":1}` {
		t.Fatalf("DataInput block: %+v", blocks[1])
	}
}

func TestResponsesUnsupportedInputFailsFast(t *testing.T) {
	srv, got := captureServer(t, "application/json", http.StatusOK, responsesBody())
	cfg := NewOpenAIResponses("gpt-test")
	cfg.APIKey = "k"
	cfg.BaseURL = srv.URL
	req := events.ModelRequest{Parts: []events.Input{
		&events.BinaryInput{Data: []byte("x"), MediaType: "audio/wav", Kind: events.BinaryAudio},
	}}
	_, _, err := responsesCallErr(t, cfg, []events.Event{&req}, config.CallOptions{})
	if !errors.Is(err, config.ErrUnsupportedInput) {
		t.Fatalf("want ErrUnsupportedInput, got %v", err)
	}
	if got.path != "" {
		t.Fatal("no request may reach the API on an unmappable history")
	}
}

// --- hosted shell and unsupported schemas -------------------------------------------

func TestResponsesHostedShellMapping(t *testing.T) {
	auto := tools.ShellToolSchema{Environment: &tools.ShellEnvironment{Auto: &tools.ContainerAuto{
		NetworkPolicy: &tools.NetworkPolicy{AllowedDomains: []string{"example.com"}},
	}}}
	wire, err := toolToResponsesAPI(auto)
	if err != nil {
		t.Fatalf("auto: %v", err)
	}
	env := wire["environment"].(map[string]any)
	if env["type"] != "container_auto" {
		t.Fatalf("auto environment: %+v", env)
	}
	policy := env["network_policy"].(map[string]any)
	if policy["type"] != "allowlist" {
		t.Fatalf("network policy: %+v", policy)
	}

	ref := tools.ShellToolSchema{Environment: &tools.ShellEnvironment{Reference: &tools.ContainerReference{ContainerID: "c-1"}}}
	wire, err = toolToResponsesAPI(ref)
	if err != nil {
		t.Fatalf("reference: %v", err)
	}
	if wire["environment"].(map[string]any)["container_id"] != "c-1" {
		t.Fatalf("reference environment: %+v", wire)
	}

	// A shell without a hosted environment would run client-side — rejected.
	if _, err := toolToResponsesAPI(tools.ShellToolSchema{}); !errors.Is(err, config.ErrUnsupportedTool) {
		t.Fatalf("local shell: want ErrUnsupportedTool, got %v", err)
	}
}

func TestResponsesUnsupportedHostedSchemas(t *testing.T) {
	for _, schema := range []any{
		tools.WebFetchToolSchema{},
		tools.MemoryToolSchema{},
	} {
		if _, err := toolToResponsesAPI(schema); !errors.Is(err, config.ErrUnsupportedTool) {
			t.Fatalf("%T: want ErrUnsupportedTool, got %v", schema, err)
		}
	}
	// mcp blocked_tools has no wire key.
	mcp := tools.MCPServerToolSchema{ServerURL: "https://mcp", ServerLabel: "m", BlockedTools: []string{"t"}}
	if _, err := toolToResponsesAPI(mcp); !errors.Is(err, config.ErrUnsupportedTool) {
		t.Fatalf("blocked_tools: want ErrUnsupportedTool, got %v", err)
	}
}

func TestResponsesFunctionDeferLoading(t *testing.T) {
	wire, err := toolToResponsesAPI(tools.Schema{Name: "f", Parameters: map[string]any{}, DeferLoading: true})
	if err != nil {
		t.Fatalf("defer_loading: %v", err)
	}
	if wire["defer_loading"] != true {
		t.Fatalf("defer_loading not sent: %+v", wire)
	}
	if _, has := wire["function"]; has {
		t.Fatal("responses function tools carry name at top level, not a function object")
	}
}

// --- strict-schema text format ------------------------------------------------------

func TestResponsesStrictSchemaTextFormat(t *testing.T) {
	srv, got := captureServer(t, "application/json", http.StatusOK, responsesBody())
	cfg := NewOpenAIResponses("gpt-test")
	cfg.APIKey = "test-key"
	cfg.BaseURL = srv.URL + "/v1"

	schema := response.RawSchema(map[string]any{
		"type":  "object",
		"title": "T",
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "title": "N", "default": "x"},
		},
	}, response.WithRawName("Person"), response.WithRawDescription("A person"))

	req := events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "go"}}}
	_, sent := responsesCall(t, cfg, []events.Event{&req}, config.CallOptions{ResponseSchema: schema})
	if len(*sent) != 0 {
		t.Fatalf("no text was produced, stream must stay quiet: %+v", *sent)
	}

	b := bodyJSON(t, got.body)
	text := b["text"].(map[string]any)
	format := text["format"].(map[string]any)
	if format["type"] != "json_schema" || format["name"] != "Person" || format["strict"] != true {
		t.Fatalf("format: %+v", format)
	}
	if format["description"] != "A person" {
		t.Fatalf("format description: %+v", format)
	}
	inner := format["schema"].(map[string]any)
	if inner["additionalProperties"] != false {
		t.Fatalf("strict schema: %+v", inner)
	}
	required := inner["required"].([]any)
	if len(required) != 1 || required[0] != "name" {
		t.Fatalf("strict required: %+v", required)
	}
	prop := inner["properties"].(map[string]any)["name"].(map[string]any)
	if _, has := prop["title"]; has || prop["type"] != "string" {
		t.Fatalf("annotation keys must be stripped: %+v", prop)
	}
	if b["instructions"] != "be brief" {
		t.Fatalf("instructions: %+v", b["instructions"])
	}
}

// --- store=false forces encrypted reasoning ------------------------------------------

func TestResponsesStoreFalseIncludes(t *testing.T) {
	srv, got := captureServer(t, "application/json", http.StatusOK, responsesBody())
	cfg := NewOpenAIResponses("gpt-test")
	cfg.APIKey = "test-key"
	cfg.BaseURL = srv.URL + "/v1"

	cp, err := cfg.Clone(map[string]any{"Store": b(false)})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	req := events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "go"}}}
	if _, _, callErr := responsesCallErr(t, cp.(*OpenAIResponsesConfig), []events.Event{&req}, config.CallOptions{}); callErr != nil {
		t.Fatalf("call: %v", callErr)
	}
	b := bodyJSON(t, got.body)
	if b["store"] != false {
		t.Fatalf("store must be false: %+v", b["store"])
	}
	includes := b["include"].([]any)
	found := false
	for _, inc := range includes {
		if inc == "reasoning.encrypted_content" {
			found = true
		}
	}
	if !found {
		t.Fatalf("stateless mode must ask for encrypted reasoning: %+v", includes)
	}
}

// --- usage normalization --------------------------------------------------------------

func TestNormalizeResponsesUsage(t *testing.T) {
	u := normalizeResponsesUsage(map[string]any{
		"input_tokens": 100, "output_tokens": 20, "total_tokens": 120,
		"input_tokens_details":  map[string]any{"cached_tokens": 80, "cache_write_tokens": 3},
		"output_tokens_details": map[string]any{"reasoning_tokens": 10},
	})
	wantToken(t, u.PromptTokens, 100, "prompt")
	wantToken(t, u.CompletionTokens, 20, "completion")
	wantToken(t, u.TotalTokens, 120, "total")
	wantToken(t, u.CacheReadInputTokens, 80, "cache read")
	wantToken(t, u.CacheCreationInputTokens, 3, "cache write")
	wantToken(t, u.ThinkingTokens, 10, "thinking")

	// Zero details still normalize (the AG2 fixture sends explicit zeros).
	zeros := normalizeResponsesUsage(map[string]any{
		"input_tokens": 1, "output_tokens": 1, "total_tokens": 2,
		"input_tokens_details":  map[string]any{"cached_tokens": 0, "cache_write_tokens": 0},
		"output_tokens_details": map[string]any{"reasoning_tokens": 0},
	})
	wantToken(t, zeros.CacheReadInputTokens, 0, "zero cache read")
	wantToken(t, zeros.ThinkingTokens, 0, "zero thinking")

	// A usage object without detail objects normalizes the base tokens only.
	plain := normalizeResponsesUsage(map[string]any{"input_tokens": 5, "output_tokens": 2, "total_tokens": 7})
	wantToken(t, plain.PromptTokens, 5, "plain prompt")
	if plain.CacheReadInputTokens != nil {
		t.Fatalf("missing details must stay nil: %+v", plain)
	}
}

// --- reasoning and hosted stream events ----------------------------------------------

func TestResponsesStreamReasoningAndHostedEvents(t *testing.T) {
	completed := completedResponse()
	body := sseBody(
		map[string]any{"type": "response.output_item.done", "item": map[string]any{
			"type":    "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": "thinking"}},
		}},
		map[string]any{"type": "response.output_item.done", "item": map[string]any{
			"id": "fc_1", "type": "code_interpreter_call", "status": "completed",
			"code": "print(1)", "container_id": "c-9",
			"outputs": []any{map[string]any{"type": "logs", "logs": "1"}},
		}},
		map[string]any{"type": "response.completed", "response": completed},
	)
	srv, _ := captureServer(t, "text/event-stream", http.StatusOK, body)
	cfg := NewOpenAIResponses("gpt-test")
	cfg.APIKey = "test-key"
	cfg.BaseURL = srv.URL + "/v1"
	cfg.Streaming = true

	req := events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "go"}}}
	_, sent := responsesCall(t, cfg, []events.Event{&req}, config.CallOptions{})

	var reasoning int
	var logs string
	var container any
	for _, ev := range *sent {
		switch e := events.AsValue(ev).(type) {
		case events.ModelReasoning:
			reasoning++
			if e.Content != "thinking" {
				t.Fatalf("reasoning content: %+v", e)
			}
		case events.BuiltinToolCallEvent:
			if e.Name != tools.CodeExecutionToolName || e.Arguments != `{"code":"print(1)"}` {
				t.Fatalf("code call: %+v", e)
			}
		case events.BuiltinToolResultEvent:
			container = e.Result.Metadata["container_id"]
			if len(e.Result.Parts) == 1 {
				logs = events.AsValue(e.Result.Parts[0]).(events.TextInput).Content
			}
		}
	}
	if reasoning != 1 {
		t.Fatalf("reasoning events: %d", reasoning)
	}
	if logs != "1" || container != "c-9" {
		t.Fatalf("code result: logs=%q container=%v", logs, container)
	}
}

// --- small test helpers ---------------------------------------------------------------

func strPtr(s string) *string { return &s }
