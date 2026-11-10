package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/response"
)

// wireCall is one request a scripted server received.
type wireCall struct {
	body   map[string]any
	header http.Header
}

// scriptedServer answers request i with responses[i] (the last repeats) and
// records every request body and header set.
func scriptedServer(t *testing.T, contentType string, responses ...string) (*httptest.Server, *[]wireCall) {
	t.Helper()
	var calls []wireCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		i := len(calls)
		calls = append(calls, wireCall{body: m, header: r.Header.Clone()})
		if i >= len(responses) {
			i = len(responses) - 1
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write([]byte(responses[i]))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// --- Files API references (D-046) -------------------------------------------------------

func TestFileIdInputBlocks(t *testing.T) {
	cases := []struct {
		filename string
		kind     string
	}{
		{"photo.PNG", "image"}, // extension match is case-insensitive
		{"a.jpeg", "image"},
		{"report.pdf", "document"},
		{"", "document"}, // no filename: a document
		{"archive.png.zip", "document"},
	}
	for _, tc := range cases {
		t.Run(tc.filename, func(t *testing.T) {
			turns, err := ConvertMessages([]events.Event{&events.ModelRequest{Parts: []events.Input{
				&events.TextInput{Content: "see"},
				&events.FileIdInput{FileID: "file_01", Filename: tc.filename},
			}}})
			if err != nil {
				t.Fatalf("ConvertMessages: %v", err)
			}
			blocks := turns[0]["content"].([]map[string]any)
			want := map[string]any{"type": tc.kind, "source": map[string]any{"type": "file", "file_id": "file_01"}}
			if !reflect.DeepEqual(blocks[1], want) {
				t.Fatalf("block = %#v, want %#v", blocks[1], want)
			}
		})
	}
}

func TestFileReferenceRequestCarriesFilesBeta(t *testing.T) {
	srv, calls := scriptedServer(t, "application/json", textResponse)
	cl := newTestClient(srv, func(c *AnthropicConfig) {
		c.DefaultHeaders = map[string]string{"anthropic-beta": "my-flag"}
	})
	history := []events.Event{
		&events.ModelRequest{Parts: []events.Input{&events.FileIdInput{FileID: "file_01", Filename: "x.png"}}},
	}
	if _, err := cl.Call(context.Background(), history, nil, config.CallOptions{}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	// With caching on the lone user turn also carries the prefix breakpoint.
	msg := (*calls)[0].body["messages"].([]any)[0].(map[string]any)
	block := msg["content"].([]any)[0].(map[string]any)
	if block["type"] != "image" || !reflect.DeepEqual(block["source"], map[string]any{"type": "file", "file_id": "file_01"}) {
		t.Fatalf("wire block = %v", block)
	}
	// Sorted union of the framework flag and the caller's own default.
	if got := (*calls)[0].header.Get("anthropic-beta"); got != "files-api-2025-04-14,my-flag" {
		t.Fatalf("anthropic-beta = %q", got)
	}

	// A file reference inside a tool result needs the flag as well; a history
	// without one sends none.
	srv2, calls2 := scriptedServer(t, "application/json", textResponse)
	cl2 := newTestClient(srv2, nil)
	call := events.ToolCallEvent{ID: "toolu_1", Name: "mk", Arguments: "{}"}
	withTool := []events.Event{
		&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "go"}}},
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{call}}},
		&events.ToolResultsEvent{Results: []events.ToolResultEvent{
			events.FromResultCall(call, events.ToolResult{Parts: []events.Input{&events.FileIdInput{FileID: "file_02"}}}),
		}},
	}
	if _, err := cl2.Call(context.Background(), withTool, nil, config.CallOptions{}); err != nil {
		t.Fatalf("Call (tool result): %v", err)
	}
	if got := (*calls2)[0].header.Get("anthropic-beta"); got != "files-api-2025-04-14" {
		t.Fatalf("tool-result reference: anthropic-beta = %q", got)
	}
	if _, err := cl2.Call(context.Background(), withTool[:1], nil, config.CallOptions{}); err != nil {
		t.Fatalf("Call (plain): %v", err)
	}
	if got := (*calls2)[1].header.Get("anthropic-beta"); got != "" {
		t.Fatalf("no file reference: anthropic-beta = %q, want none", got)
	}
}

// --- conversation-prefix cache breakpoint (D-020) ---------------------------------------

func TestPrefixCacheBreakpointOnLastUserTurn(t *testing.T) {
	srv, calls := scriptedServer(t, "application/json", textResponse)
	cl := newTestClient(srv, nil)
	call := events.ToolCallEvent{ID: "toolu_1", Name: "lookup", Arguments: `{"q":"x"}`}
	history := []events.Event{
		&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "first"}}},
		&events.ModelResponse{
			Message:   &events.ModelMessage{Content: "checking"},
			ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{call}},
		},
		&events.ToolResultsEvent{Results: []events.ToolResultEvent{
			events.FromResultCall(call, events.NewToolResult(false, "found it")),
		}},
	}
	if _, err := cl.Call(context.Background(), history, nil, config.CallOptions{}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	msgs := (*calls)[0].body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %v", msgs)
	}
	// The first user turn is left a plain string: only the last user turn is marked.
	if msgs[0].(map[string]any)["content"] != "first" {
		t.Fatalf("an earlier turn was touched: %v", msgs[0])
	}
	asst := msgs[1].(map[string]any)["content"].([]any)
	for _, b := range asst {
		if _, has := b.(map[string]any)["cache_control"]; has {
			t.Fatalf("assistant block carries a breakpoint: %v", b)
		}
	}
	// The last user turn here is the tool-result turn: its last block is marked.
	results := msgs[2].(map[string]any)["content"].([]any)
	last := results[len(results)-1].(map[string]any)
	if last["type"] != "tool_result" || !reflect.DeepEqual(last["cache_control"], map[string]any{"type": "ephemeral"}) {
		t.Fatalf("last tool_result block = %v", last)
	}
}

func TestPrefixCacheBreakpointOffWithoutPromptCaching(t *testing.T) {
	srv, calls := scriptedServer(t, "application/json", textResponse)
	cl := newTestClient(srv, func(c *AnthropicConfig) { c.PromptCaching = false })
	history := []events.Event{&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "hi"}}}}
	if _, err := cl.Call(context.Background(), history, nil, config.CallOptions{}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got := (*calls)[0].body["messages"].([]any)[0].(map[string]any)["content"]; got != "hi" {
		t.Fatalf("PromptCaching off must leave the turn alone, got %v", got)
	}
}

func TestInjectCacheBreakpointSkipsThinkingAndKeepsCallerChoice(t *testing.T) {
	turns := []map[string]any{
		turn("user", "older"),
		turn("assistant", []map[string]any{{"type": "text", "text": "a"}}),
		turn("user", []map[string]any{
			{"type": "text", "text": "x"},
			{"type": "thinking", "thinking": "hmm"},
		}),
		turn("assistant", []map[string]any{{"type": "text", "text": "tail"}}), // a trailing assistant turn is ignored
	}
	injectCacheBreakpoint(turns)
	blocks := turns[2]["content"].([]map[string]any)
	if _, has := blocks[1]["cache_control"]; has {
		t.Fatal("a thinking block cannot carry a breakpoint")
	}
	if turns[0]["content"] != "older" {
		t.Fatal("only the last user turn is marked")
	}

	kept := []map[string]any{turn("user", []map[string]any{
		{"type": "text", "text": "x", "cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"}},
	})}
	injectCacheBreakpoint(kept)
	if cc := kept[0]["content"].([]map[string]any)[0]["cache_control"]; !reflect.DeepEqual(cc, map[string]any{"type": "ephemeral", "ttl": "1h"}) {
		t.Fatalf("caller's cache_control replaced: %v", cc)
	}
}

// --- native structured output (D-020) ---------------------------------------------------

func TestNativeStructuredOutputRequest(t *testing.T) {
	schema := response.RawSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"answer": map[string]any{"type": "string"},
			"nested": map[string]any{"type": "object", "properties": map[string]any{"n": map[string]any{"type": "integer"}}},
			"list":   map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{}}},
		},
		"$defs": map[string]any{"D": map[string]any{"type": "object", "additionalProperties": true}},
	})
	history := []events.Event{&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "go"}}}}

	call := func(t *testing.T, native bool, s response.Proto) map[string]any {
		t.Helper()
		srv, calls := scriptedServer(t, "application/json", textResponse)
		cl := newTestClient(srv, func(c *AnthropicConfig) {
			c.NativeStructuredOutput = native
			c.PromptCaching = false
		})
		if _, err := cl.Call(context.Background(), history, nil, config.CallOptions{ResponseSchema: s}); err != nil {
			t.Fatalf("Call: %v", err)
		}
		return (*calls)[0].body
	}

	body := call(t, true, schema)
	want := map[string]any{"format": map[string]any{"type": "json_schema", "schema": map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"answer": map[string]any{"type": "string"},
			"nested": map[string]any{"type": "object", "additionalProperties": false,
				"properties": map[string]any{"n": map[string]any{"type": "integer"}}},
			"list": map[string]any{"type": "array", "items": map[string]any{
				"type": "object", "additionalProperties": false, "properties": map[string]any{}}},
		},
		// An explicit additionalProperties is the caller's and is kept.
		"$defs": map[string]any{"D": map[string]any{"type": "object", "additionalProperties": true}},
	}}}
	if !reflect.DeepEqual(body["output_config"], want) {
		t.Fatalf("output_config =\n%v\nwant\n%v", body["output_config"], want)
	}
	// The schema's own system prompt still goes out next to the native block.
	if _, ok := body["system"]; !ok && schema.SystemPrompt() != "" {
		t.Fatal("the schema's system prompt must stay")
	}

	if _, has := call(t, false, schema)["output_config"]; has {
		t.Fatal("output_config sent without NativeStructuredOutput")
	}
	// A prompt-driven schema has no JSON schema to enforce.
	if _, has := call(t, true, response.NewPromptedSchema(schema))["output_config"]; has {
		t.Fatal("output_config sent for a schema without a JSON schema")
	}
}

// A bare call sends no system, tools or beta header at all: empty values would
// change the request (an empty tools array, a blank beta flag).
func TestBareRequestOmitsOptionalFields(t *testing.T) {
	srv, calls := scriptedServer(t, "application/json", textResponse)
	cl := newTestClient(srv, nil)
	history := []events.Event{&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "hi"}}}}
	if _, err := cl.Call(context.Background(), history, nil, config.CallOptions{}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	for _, key := range []string{"system", "tools", "mcp_servers", "output_config", "diagnostics", "stop_sequences"} {
		if v, has := (*calls)[0].body[key]; has {
			t.Fatalf("%s = %v on a bare request", key, v)
		}
	}
	if _, has := (*calls)[0].header["Anthropic-Beta"]; has {
		t.Fatalf("anthropic-beta header present: %v", (*calls)[0].header["Anthropic-Beta"])
	}
}
