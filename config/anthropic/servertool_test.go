package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
)

// A response where the model searched the web inside one turn: text, the
// server-side call, its result, then the answer.
const serverToolResponse = `{
  "id": "msg_srv", "model": "claude-sonnet-4", "type": "message", "role": "assistant",
  "content": [
    {"type": "text", "text": "Let me look that up."},
    {"type": "server_tool_use", "id": "srvtoolu_1", "name": "web_search", "input": {"query": "ago go port"}},
    {"type": "web_search_tool_result", "tool_use_id": "srvtoolu_1", "content": [
      {"type": "web_search_result", "url": "https://example.com/a", "title": "A", "page_age": "1 day", "encrypted_content": "enc-a"},
      {"type": "web_search_result", "url": "https://example.com/b", "title": "B", "page_age": null, "encrypted_content": "enc-b"}
    ]},
    {"type": "text", "text": "It is a Go port."}
  ],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 12, "output_tokens": 34}
}`

// responseBlocks decodes the content of a scripted JSON response.
func responseBlocks(t *testing.T, raw string) []any {
	t.Helper()
	var m struct {
		Content []any `json:"content"`
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return m.Content
}

// replayBlocks decodes the scripted response and removes the one null the
// fixture carries (page_age of the second search result): a replayed block
// never carries a null field, the way Python's exclude_none dump has it.
func replayBlocks(t *testing.T, raw string) []any {
	t.Helper()
	blocks := responseBlocks(t, raw)
	result := blocks[2].(map[string]any)["content"].([]any)[1].(map[string]any)
	if v, ok := result["page_age"]; !ok || v != nil {
		t.Fatalf("fixture lost its null page_age: %v", result)
	}
	delete(result, "page_age")
	return blocks
}

// builtinEvents picks the server-tool carriers a call put on the stream, in order.
func builtinEvents(sent []events.Event) []events.Event {
	var out []events.Event
	for _, ev := range sent {
		switch events.AsValue(ev).(type) {
		case events.BuiltinToolCallEvent, events.BuiltinToolResultEvent:
			out = append(out, ev)
		}
	}
	return out
}

// replayTurn runs a second call over the first call's history and returns the
// assistant turn of its request, as decoded JSON blocks.
func replayTurn(t *testing.T, cl config.LLMClient, calls *[]wireCall, sent []events.Event, resp *events.ModelResponse) []any {
	t.Helper()
	history := append(userHistory(), builtinEvents(sent)...)
	history = append(history, resp)
	history = append(history, &events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "and then?"}}})
	if _, err := cl.Call(context.Background(), history, nil, config.CallOptions{}); err != nil {
		t.Fatalf("replay call: %v", err)
	}
	msgs := (*calls)[len(*calls)-1].body["messages"].([]any)
	if len(msgs) != 3 || msgs[1].(map[string]any)["role"] != "assistant" {
		t.Fatalf("replay messages = %v", msgs)
	}
	return msgs[1].(map[string]any)["content"].([]any)
}

func checkServerToolEvents(t *testing.T, sent []events.Event) {
	t.Helper()
	got := builtinEvents(sent)
	if len(got) != 2 {
		t.Fatalf("builtin events = %d, want a call and a result", len(got))
	}
	call, ok := events.AsValue(got[0]).(events.BuiltinToolCallEvent)
	if !ok || call.ID != "srvtoolu_1" || call.Name != tools.WebSearchToolName || call.Arguments != `{"query":"ago go port"}` {
		t.Fatalf("call event = %+v", got[0])
	}
	result, ok := events.AsValue(got[1]).(events.BuiltinToolResultEvent)
	if !ok || result.ParentID != "srvtoolu_1" || result.Name != tools.WebSearchToolName {
		t.Fatalf("result event = %+v", got[1])
	}
	if len(result.Result.Parts) != 2 {
		t.Fatalf("result parts = %v", result.Result.Parts)
	}
	first, _ := events.AsValue(result.Result.Parts[0]).(events.UrlInput)
	if first.URL != "https://example.com/a" {
		t.Fatalf("first source = %+v", result.Result.Parts[0])
	}
	if result.Result.Metadata["count"] != 2 {
		t.Fatalf("metadata = %v", result.Result.Metadata)
	}
}

func TestServerToolEventsAndReplay(t *testing.T) {
	srv, calls := scriptedServer(t, "application/json", serverToolResponse)
	cl := newTestClient(srv, nil)
	conv, sent := newCaptureConv()

	resp, err := cl.Call(context.Background(), userHistory(), conv, config.CallOptions{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Message == nil || resp.Message.Content != "It is a Go port." {
		t.Fatalf("message = %+v", resp.Message)
	}
	checkServerToolEvents(t, *sent)

	// Call 2 replays both server blocks with null fields stripped (like
	// Python's exclude_none: the null page_age is gone), ahead of the answer
	// text, inside one assistant turn.
	got := replayTurn(t, cl, calls, *sent, resp)
	blocks := replayBlocks(t, serverToolResponse)
	want := []any{blocks[1], blocks[2], map[string]any{"type": "text", "text": "It is a Go port."}}
	if !reflect.DeepEqual(got, want) {
		gb, _ := json.Marshal(got)
		wb, _ := json.Marshal(want)
		t.Fatalf("replayed turn =\n%s\nwant\n%s", gb, wb)
	}
}

const serverToolSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_srv","model":"claude-sonnet-4","role":"assistant","content":[],"usage":{"input_tokens":12,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Let me look that up."}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"query\": \"ago"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":" go port\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_result","url":"https://example.com/a","title":"A","page_age":"1 day","encrypted_content":"enc-a"},{"type":"web_search_result","url":"https://example.com/b","title":"B","page_age":null,"encrypted_content":"enc-b"}]}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: content_block_start
data: {"type":"content_block_start","index":3,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":3,"delta":{"type":"text_delta","text":"It is a "}}

event: content_block_delta
data: {"type":"content_block_delta","index":3,"delta":{"type":"text_delta","text":"Go port."}}

event: content_block_stop
data: {"type":"content_block_stop","index":3}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":34}}

event: message_stop
data: {"type":"message_stop"}
`

func TestServerToolEventsAndReplayStreaming(t *testing.T) {
	srv, calls := scriptedServer(t, "text/event-stream", serverToolSSE)
	cl := newTestClient(srv, func(c *AnthropicConfig) { c.Streaming = true })
	conv, sent := newCaptureConv()

	resp, err := cl.Call(context.Background(), userHistory(), conv, config.CallOptions{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	checkServerToolEvents(t, *sent) // the input was assembled from its deltas

	got := replayTurn(t, cl, calls, *sent, resp)
	blocks := replayBlocks(t, serverToolResponse)
	// A stream's answer text is every text block joined (the streaming contract),
	// unlike the one last block of a non-streaming answer.
	want := []any{blocks[1], blocks[2], map[string]any{"type": "text", "text": "Let me look that up.It is a Go port."}}
	if !reflect.DeepEqual(got, want) {
		gb, _ := json.Marshal(got)
		wb, _ := json.Marshal(want)
		t.Fatalf("replayed turn =\n%s\nwant\n%s", gb, wb)
	}
}

// Each server-tool kind becomes a call/result pair under its neutral tool name,
// with the verbatim block kept; an unknown server tool is dropped, not replayed.
func TestServerToolBlockKinds(t *testing.T) {
	tests := []struct {
		name      string
		call      string
		result    string
		tool      string
		wantParts []string // text parts, in order
		wantFiles []string // file references, in order
		wantPDF   string   // the inline PDF part's bytes, when expected
		wantMeta  map[string]any
	}{
		{
			name:      "web_fetch text",
			call:      `{"type":"server_tool_use","id":"s","name":"web_fetch","input":{"url":"https://e.com"}}`,
			result:    `{"type":"web_fetch_tool_result","tool_use_id":"s","content":{"type":"web_fetch_result","url":"https://e.com","retrieved_at":"t","content":{"type":"document","title":"T","source":{"type":"text","media_type":"text/plain","data":"page body"}}}}`,
			tool:      tools.WebFetchToolName,
			wantParts: []string{"page body"},
			wantMeta:  map[string]any{"title": "T", "retrieved_at": "t"},
		},
		{
			name:      "web_fetch error",
			call:      `{"type":"server_tool_use","id":"s","name":"web_fetch","input":{}}`,
			result:    `{"type":"web_fetch_tool_result","tool_use_id":"s","content":{"type":"web_fetch_tool_result_error","error_code":"url_not_accessible"}}`,
			tool:      tools.WebFetchToolName,
			wantParts: []string{"web_fetch_tool_result_error: url_not_accessible"},
			wantMeta:  map[string]any{"error": true, "error_code": "url_not_accessible"},
		},
		{
			name:      "code execution",
			call:      `{"type":"server_tool_use","id":"s","name":"code_execution","input":{"code":"print(1)"}}`,
			result:    `{"type":"code_execution_tool_result","tool_use_id":"s","content":{"type":"code_execution_result","stdout":"1\n","stderr":"","return_code":0,"content":[{"type":"code_execution_output","file_id":"file_9"}]}}`,
			tool:      tools.CodeExecutionToolName,
			wantParts: []string{"1\n"},
			wantFiles: []string{"file_9"},
			wantMeta:  map[string]any{"return_code": float64(0)},
		},
		{
			name:      "code execution encrypted",
			call:      `{"type":"server_tool_use","id":"s","name":"code_execution","input":{}}`,
			result:    `{"type":"code_execution_tool_result","tool_use_id":"s","content":{"type":"encrypted_code_execution_result","encrypted_stdout":"zz","stderr":"","return_code":0,"content":[{"type":"code_execution_output","file_id":"file_e"}]}}`,
			tool:      tools.CodeExecutionToolName,
			wantFiles: []string{"file_e"},
			wantMeta:  map[string]any{"encrypted": true, "return_code": float64(0)},
		},
		{
			name:     "web_fetch pdf",
			call:     `{"type":"server_tool_use","id":"s","name":"web_fetch","input":{"url":"https://e.com/a.pdf"}}`,
			result:   `{"type":"web_fetch_tool_result","tool_use_id":"s","content":{"type":"web_fetch_result","url":"https://e.com/a.pdf","retrieved_at":"t","content":{"type":"document","title":"P","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0x"}}}}`,
			tool:     tools.WebFetchToolName,
			wantPDF:  "%PDF-1",
			wantMeta: map[string]any{"title": "P"},
		},
		{
			name:      "text editor error with message",
			call:      `{"type":"server_tool_use","id":"s","name":"text_editor_code_execution","input":{}}`,
			result:    `{"type":"text_editor_code_execution_tool_result","tool_use_id":"s","content":{"type":"text_editor_code_execution_tool_result_error","error_code":"file_not_found","error_message":"no such file"}}`,
			tool:      tools.CodeExecutionToolName,
			wantParts: []string{"text_editor_code_execution_tool_result_error: file_not_found: no such file"},
			wantMeta:  map[string]any{"error": true, "error_code": "file_not_found", "error_message": "no such file"},
		},
		{
			name:     "text editor create",
			call:     `{"type":"server_tool_use","id":"s","name":"text_editor_code_execution","input":{"command":"create"}}`,
			result:   `{"type":"text_editor_code_execution_tool_result","tool_use_id":"s","content":{"type":"text_editor_code_execution_create_result","is_file_update":true}}`,
			tool:     tools.CodeExecutionToolName,
			wantMeta: map[string]any{"is_file_update": true},
		},
		{
			name:      "text editor str_replace",
			call:      `{"type":"server_tool_use","id":"s","name":"text_editor_code_execution","input":{"command":"str_replace"}}`,
			result:    `{"type":"text_editor_code_execution_tool_result","tool_use_id":"s","content":{"type":"text_editor_code_execution_str_replace_result","lines":["l1","l2"],"new_lines":2,"new_start":4,"old_lines":1,"old_start":4}}`,
			tool:      tools.CodeExecutionToolName,
			wantParts: []string{"l1\nl2"},
			wantMeta:  map[string]any{"new_lines": float64(2), "new_start": float64(4), "old_lines": float64(1), "old_start": float64(4)},
		},
		{
			name:     "tool search without matches",
			call:     `{"type":"server_tool_use","id":"s","name":"tool_search_tool_bm25","input":{"query":"zz"}}`,
			result:   `{"type":"tool_search_tool_result","tool_use_id":"s","content":{"type":"tool_search_tool_search_result","tool_references":[]}}`,
			tool:     tools.ToolSearchToolName,
			wantMeta: map[string]any{"tool_references": []string{}},
		},
		{
			name:      "bash execution",
			call:      `{"type":"server_tool_use","id":"s","name":"bash_code_execution","input":{"command":"ls"}}`,
			result:    `{"type":"bash_code_execution_tool_result","tool_use_id":"s","content":{"type":"bash_code_execution_result","stdout":"a\n","stderr":"w\n","return_code":1,"content":[]}}`,
			tool:      tools.CodeExecutionToolName,
			wantParts: []string{"a\n", "w\n"},
			wantMeta:  map[string]any{"return_code": float64(1)},
		},
		{
			name:      "text editor view",
			call:      `{"type":"server_tool_use","id":"s","name":"text_editor_code_execution","input":{"command":"view"}}`,
			result:    `{"type":"text_editor_code_execution_tool_result","tool_use_id":"s","content":{"type":"text_editor_code_execution_view_result","content":"line","file_type":"text","num_lines":1,"start_line":1,"total_lines":1}}`,
			tool:      tools.CodeExecutionToolName,
			wantParts: []string{"line"},
			wantMeta:  map[string]any{"file_type": "text"},
		},
		{
			name:      "tool search",
			call:      `{"type":"server_tool_use","id":"s","name":"tool_search_tool_regex","input":{"pattern":"w"}}`,
			result:    `{"type":"tool_search_tool_result","tool_use_id":"s","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"weather"},{"type":"tool_reference","tool_name":"time"}]}}`,
			tool:      tools.ToolSearchToolName,
			wantParts: []string{"weather, time"},
			wantMeta:  map[string]any{"tool_references": []string{"weather", "time"}},
		},
		{
			name:      "mcp connector with non-text content",
			call:      `{"type":"mcp_tool_use","id":"m","name":"lookup","server_name":"docs","input":{}}`,
			result:    `{"type":"mcp_tool_result","tool_use_id":"m","is_error":true,"content":[{"type":"image"},{"type":"text","text":"found"}]}`,
			tool:      tools.MCPServerToolName,
			wantParts: []string{"found"},
			wantMeta:  map[string]any{"error": true},
		},
		{
			name:      "mcp connector",
			call:      `{"type":"mcp_tool_use","id":"m","name":"lookup","server_name":"docs","input":{"q":"x"}}`,
			result:    `{"type":"mcp_tool_result","tool_use_id":"m","is_error":false,"content":[{"type":"text","text":"found"}]}`,
			tool:      tools.MCPServerToolName,
			wantParts: []string{"found"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := fmt.Sprintf(`{"id":"m1","model":"x","content":[%s,%s,{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, tc.call, tc.result)
			srv, calls := scriptedServer(t, "application/json", resp)
			cl := newTestClient(srv, nil)
			conv, sent := newCaptureConv()
			out, err := cl.Call(context.Background(), userHistory(), conv, config.CallOptions{})
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
			evs := builtinEvents(*sent)
			if len(evs) != 2 {
				t.Fatalf("builtin events = %d", len(evs))
			}
			call := events.AsValue(evs[0]).(events.BuiltinToolCallEvent)
			res := events.AsValue(evs[1]).(events.BuiltinToolResultEvent)
			if call.Name != tc.tool || res.Name != tc.tool || res.ParentID != call.ID {
				t.Fatalf("call %+v / result %+v", call, res)
			}
			var texts, files []string
			var pdf string
			for _, p := range res.Result.Parts {
				switch e := events.AsValue(p).(type) {
				case events.TextInput:
					texts = append(texts, e.Content)
				case events.FileIdInput:
					files = append(files, e.FileID)
				case events.BinaryInput:
					pdf = string(e.Data)
					if e.MediaType != "application/pdf" || e.Kind != events.BinaryDocument {
						t.Fatalf("inline part = %+v", e)
					}
				}
			}
			if !reflect.DeepEqual(texts, tc.wantParts) || !reflect.DeepEqual(files, tc.wantFiles) || pdf != tc.wantPDF {
				t.Fatalf("parts: text %q files %q pdf %q; want %q %q %q", texts, files, pdf, tc.wantParts, tc.wantFiles, tc.wantPDF)
			}
			for k, v := range tc.wantMeta {
				if !reflect.DeepEqual(res.Result.Metadata[k], v) {
					t.Fatalf("metadata[%q] = %#v, want %#v", k, res.Result.Metadata[k], v)
				}
			}
			// The pair survives a transcript round trip and replays verbatim.
			var stored []events.Event
			for _, ev := range evs {
				raw, err := events.MarshalEvent(ev)
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				back, err := events.UnmarshalEvent(raw)
				if err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				stored = append(stored, back)
			}
			got := replayTurn(t, cl, calls, stored, out)
			blocks := responseBlocks(t, resp)
			if want := []any{blocks[0], blocks[1], blocks[2]}; !reflect.DeepEqual(got, want) {
				gb, _ := json.Marshal(got)
				wb, _ := json.Marshal(want)
				t.Fatalf("replayed turn =\n%s\nwant\n%s", gb, wb)
			}
		})
	}
}

// A server tool AGo has no name for is neither announced nor replayed.
func TestUnknownServerToolIsDropped(t *testing.T) {
	resp := `{"id":"m1","model":"x","content":[{"type":"server_tool_use","id":"s","name":"mystery","input":{}},{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	srv, _ := scriptedServer(t, "application/json", resp)
	cl := newTestClient(srv, nil)
	conv, sent := newCaptureConv()
	if _, err := cl.Call(context.Background(), userHistory(), conv, config.CallOptions{}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if n := len(builtinEvents(*sent)); n != 0 {
		t.Fatalf("builtin events = %d, want none", n)
	}
}

// A builtin event of another provider (an OpenAI hosted item) has no Anthropic
// block and must not turn into one.
func TestForeignBuiltinEventsAreNotReplayed(t *testing.T) {
	turns, err := ConvertMessages([]events.Event{
		&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "hi"}}},
		&events.BuiltinToolCallEvent{ToolCallEvent: events.ToolCallEvent{
			ID: "ws_1", Name: tools.WebSearchToolName, VendorMetadata: map[string]any{"item": map[string]any{"type": "web_search_call"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("turns = %v", turns)
	}
}

// --- pause_turn ---------------------------------------------------------------------------

const pausedResponse = `{
  "id": "msg_p1", "model": "claude-sonnet-4", "type": "message", "role": "assistant",
  "content": [
    {"type": "text", "text": "Searching."},
    {"type": "server_tool_use", "id": "srvtoolu_1", "name": "web_search", "input": {"query": "q"}},
    {"type": "web_search_tool_result", "tool_use_id": "srvtoolu_1", "content": [
      {"type": "web_search_result", "url": "https://example.com/a", "title": "A", "page_age": null, "encrypted_content": "enc-a"}]}
  ],
  "stop_reason": "pause_turn",
  "usage": {"input_tokens": 10, "output_tokens": 5}
}`

const finalResponse = `{
  "id": "msg_p2", "model": "claude-sonnet-4", "type": "message", "role": "assistant",
  "content": [{"type": "text", "text": "All done."}],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 20, "output_tokens": 7}
}`

func TestPauseTurnContinuation(t *testing.T) {
	srv, calls := scriptedServer(t, "application/json", pausedResponse, finalResponse)
	cl := newTestClient(srv, func(c *AnthropicConfig) { c.CacheDiagnostics = true })
	conv, sent := newCaptureConv()

	resp, err := cl.Call(context.Background(), userHistory(), conv, config.CallOptions{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("requests = %d, want the call and one continuation", len(*calls))
	}
	// The continuation resends the paused content verbatim as an assistant turn.
	msgs := (*calls)[1].body["messages"].([]any)
	if len(msgs) != 2 || msgs[1].(map[string]any)["role"] != "assistant" {
		t.Fatalf("continuation messages = %v", msgs)
	}
	if got, want := msgs[1].(map[string]any)["content"].([]any), responseBlocks(t, pausedResponse); !reflect.DeepEqual(got, want) {
		t.Fatalf("continuation content = %v, want %v", got, want)
	}
	// The first request had no assistant turn, so the bodies differ only by it.
	if first := (*calls)[0].body["messages"].([]any); len(first) != 1 {
		t.Fatalf("first request messages = %v", first)
	}
	// The answer is the last hop's; the usage covers both.
	if resp.Message == nil || resp.Message.Content != "All done." || resp.FinishReason != "stop" || resp.ResponseID != "msg_p2" {
		t.Fatalf("response = %+v", resp)
	}
	if resp.Usage.PromptTokens == nil || *resp.Usage.PromptTokens != 30 ||
		resp.Usage.CompletionTokens == nil || *resp.Usage.CompletionTokens != 12 {
		t.Fatalf("usage = %+v, want both hops summed", resp.Usage)
	}
	// The first hop's server-tool blocks were announced.
	if n := len(builtinEvents(*sent)); n != 2 {
		t.Fatalf("builtin events = %d", n)
	}
	// Each hop re-chains its cache diagnostics to the message before it.
	d0 := (*calls)[0].body["diagnostics"].(map[string]any)["previous_message_id"]
	d1 := (*calls)[1].body["diagnostics"].(map[string]any)["previous_message_id"]
	if d0 != nil || d1 != "msg_p1" {
		t.Fatalf("diagnostics chain = %v then %v", d0, d1)
	}
}

func TestPauseTurnContinuationIsBounded(t *testing.T) {
	srv, calls := scriptedServer(t, "application/json", pausedResponse) // pauses forever
	cl := newTestClient(srv, nil)
	resp, err := cl.Call(context.Background(), userHistory(), nil, config.CallOptions{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(*calls) != 1+maxPauseContinuations {
		t.Fatalf("requests = %d, want %d", len(*calls), 1+maxPauseContinuations)
	}
	if resp.FinishReason != "pause_turn" {
		t.Fatalf("FinishReason = %q: the still-paused answer is returned as is", resp.FinishReason)
	}
	if resp.Usage.PromptTokens == nil || *resp.Usage.PromptTokens != int64(10*(1+maxPauseContinuations)) {
		t.Fatalf("usage = %+v, want every hop summed", resp.Usage)
	}
}

const pausedSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_p1","model":"claude-sonnet-4","role":"assistant","content":[],"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"plan "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"it"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-1"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"query\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"q\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"pause_turn"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}
`

const finalSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_p2","model":"claude-sonnet-4","role":"assistant","content":[],"usage":{"input_tokens":20,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"All done."}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}
`

func TestPauseTurnContinuationStreaming(t *testing.T) {
	srv, calls := scriptedServer(t, "text/event-stream", pausedSSE, finalSSE)
	cl := newTestClient(srv, func(c *AnthropicConfig) { c.Streaming = true })
	conv, _ := newCaptureConv()

	resp, err := cl.Call(context.Background(), userHistory(), conv, config.CallOptions{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(*calls) != 2 || (*calls)[1].body["stream"] != true {
		t.Fatalf("requests = %d", len(*calls))
	}
	// The paused stream is rebuilt block by block: deltas folded in, the tool
	// input assembled from its fragments.
	want := []any{
		map[string]any{"type": "thinking", "thinking": "plan it", "signature": "sig-1"},
		map[string]any{"type": "server_tool_use", "id": "srvtoolu_1", "name": "web_search", "input": map[string]any{"query": "q"}},
	}
	msgs := (*calls)[1].body["messages"].([]any)
	if got := msgs[len(msgs)-1].(map[string]any)["content"]; !reflect.DeepEqual(got, any(want)) {
		gb, _ := json.Marshal(got)
		wb, _ := json.Marshal(want)
		t.Fatalf("continuation content =\n%s\nwant\n%s", gb, wb)
	}
	if resp.Message == nil || resp.Message.Content != "All done." || resp.FinishReason != "stop" {
		t.Fatalf("response = %+v", resp)
	}
	if resp.Usage.PromptTokens == nil || *resp.Usage.PromptTokens != 30 ||
		resp.Usage.CompletionTokens == nil || *resp.Usage.CompletionTokens != 12 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

// A failure on the continuation fails the call (the first hop's answer is not
// returned as if it were complete).
func TestPauseTurnContinuationError(t *testing.T) {
	n := 0
	srv, _ := captureServer(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, pausedResponse)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`)
	})
	cl := newTestClient(srv, nil)
	if _, err := cl.Call(context.Background(), userHistory(), nil, config.CallOptions{}); err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("want the continuation's API error, got %v", err)
	}
}

// A paused answer with no content cannot be resumed (the API refuses an empty
// assistant turn): it is returned as is, after one request.
func TestPauseTurnWithoutContentIsReturned(t *testing.T) {
	empty := `{"id":"m","model":"x","content":[],"stop_reason":"pause_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	srv, calls := scriptedServer(t, "application/json", empty)
	cl := newTestClient(srv, nil)
	resp, err := cl.Call(context.Background(), userHistory(), nil, config.CallOptions{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(*calls) != 1 || resp.FinishReason != "pause_turn" {
		t.Fatalf("requests = %d, finish = %q", len(*calls), resp.FinishReason)
	}
}

// extra_body replaces derived fields on the first request, but it must not undo
// the continuation: the resumed request carries the paused content on top of
// whatever messages the first one sent.
func TestPauseTurnContinuationSurvivesExtraBodyMessages(t *testing.T) {
	srv, calls := scriptedServer(t, "application/json", pausedResponse, finalResponse)
	override := []any{map[string]any{"role": "user", "content": "from extra_body"}}
	cl := newTestClient(srv, func(c *AnthropicConfig) { c.ExtraBody = map[string]any{"messages": override} })
	resp, err := cl.Call(context.Background(), userHistory(), nil, config.CallOptions{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(*calls) != 2 || resp.Message == nil || resp.Message.Content != "All done." {
		t.Fatalf("requests = %d, response = %+v", len(*calls), resp)
	}
	first := (*calls)[0].body["messages"].([]any)
	if len(first) != 1 || first[0].(map[string]any)["content"] != "from extra_body" {
		t.Fatalf("first request messages = %v", first)
	}
	second := (*calls)[1].body["messages"].([]any)
	if len(second) != 2 || second[0].(map[string]any)["content"] != "from extra_body" ||
		second[1].(map[string]any)["role"] != "assistant" {
		t.Fatalf("continuation messages = %v, want the override plus the paused turn", second)
	}
	if !reflect.DeepEqual(second[1].(map[string]any)["content"], any(responseBlocks(t, pausedResponse))) {
		t.Fatalf("continuation content = %v", second[1])
	}
}

// An absent or null input is an empty argument object; foreign entries in a
// search result list are skipped, not turned into empty sources.
func TestServerToolEdgeShapes(t *testing.T) {
	for _, call := range []string{
		`{"type":"server_tool_use","id":"s","name":"web_search"}`,
		`{"type":"server_tool_use","id":"s","name":"web_search","input":null}`,
	} {
		var block map[string]any
		if err := json.Unmarshal([]byte(call), &block); err != nil {
			t.Fatal(err)
		}
		if ev := serverToolCallEvent(block); ev == nil || ev.Arguments != "{}" {
			t.Fatalf("%s: arguments = %+v", call, ev)
		}
	}

	var block map[string]any
	raw := `{"type":"web_search_tool_result","tool_use_id":"s","content":[
		{"type":"web_search_result","url":"https://e.com/a","title":"A"},
		{"type":"something_else","url":"https://e.com/skip"},
		"not an object"]}`
	if err := json.Unmarshal([]byte(raw), &block); err != nil {
		t.Fatal(err)
	}
	ev := serverToolResultEvent(block)
	if ev == nil || len(ev.Result.Parts) != 1 || ev.Result.Metadata["count"] != 3 {
		t.Fatalf("result = %+v", ev)
	}
}

const mcpSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_m","model":"claude-sonnet-4","role":"assistant","content":[],"usage":{"input_tokens":3,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"mcp_tool_use","id":"mcptoolu_1","name":"lookup","server_name":"docs","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"x\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"mcp_tool_result","tool_use_id":"mcptoolu_1","is_error":false,"content":[{"type":"text","text":"found"}]}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"text","text":"","citations":null}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"Cited."}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"citations_delta","citation":{"type":"web_search_result_location","url":"https://e.com","cited_text":"c"}}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"pause_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}
`

// The MCP connector's blocks stream like the other server tools, and a paused
// turn is resumed with its text block's citations folded back in.
func TestStreamMCPBlocksAndCitationsOnContinuation(t *testing.T) {
	srv, calls := scriptedServer(t, "text/event-stream", mcpSSE, finalSSE)
	cl := newTestClient(srv, func(c *AnthropicConfig) { c.Streaming = true })
	conv, sent := newCaptureConv()
	if _, err := cl.Call(context.Background(), userHistory(), conv, config.CallOptions{}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	evs := builtinEvents(*sent)
	if len(evs) != 2 {
		t.Fatalf("builtin events = %d, want the MCP call and result", len(evs))
	}
	call := events.AsValue(evs[0]).(events.BuiltinToolCallEvent)
	if call.Name != tools.MCPServerToolName || call.Arguments != `{"q":"x"}` {
		t.Fatalf("mcp call = %+v", call)
	}
	msgs := (*calls)[1].body["messages"].([]any)
	got := msgs[len(msgs)-1].(map[string]any)["content"].([]any)
	text := got[2].(map[string]any)
	wantCitations := []any{map[string]any{"type": "web_search_result_location", "url": "https://e.com", "cited_text": "c"}}
	if text["text"] != "Cited." || !reflect.DeepEqual(text["citations"], any(wantCitations)) {
		t.Fatalf("continuation text block = %v", text)
	}
}

// A server-tool event the stream refuses to persist fails the call, as with the
// thinking blocks, instead of vanishing from the transcript.
func TestStreamServerToolSendFailureFailsTheCall(t *testing.T) {
	for name, sse := range map[string]string{"server tool": serverToolSSE, "mcp": mcpSSE} {
		t.Run(name, func(t *testing.T) {
			srv, _ := scriptedServer(t, "text/event-stream", sse)
			ms := stream.NewMemoryStream(stream.WithStorage(failStorage{}))
			cl := newTestClient(srv, func(c *AnthropicConfig) { c.Streaming = true })
			if _, err := cl.Call(context.Background(), userHistory(), agocontext.New(ms), config.CallOptions{}); err == nil {
				t.Fatal("Call succeeded, want the failed write to surface")
			}
		})
	}
}
