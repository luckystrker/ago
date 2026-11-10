package gemini

// Tests pinning the boundary and branch behavior the mutation run of wave
// v0.8.0 found unpinned in client.go and mappers.go: empty optional blocks on
// the wire, status-band boundaries, empty-turn skipping, typed-nil hosted
// schemas, and inlineData/functionCall part folding.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/tools"
)

// --- client.go -----------------------------------------------------------------

func TestGeminiRequestOmitsEmptyOptionalBlocks(t *testing.T) {
	// With no tools and no sampling fields the request must not carry "tools"
	// or "generationConfig" at all: empty placeholders are not the wire shape.
	srv, cap := captureServer(t, 200, `{"candidates":[]}`)
	cl := startClient(t, newTestConfig(srv.URL))
	req := &events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "hi"}}}
	if _, err := cl.Call(context.Background(), []events.Event{req}, nil, config.CallOptions{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	body := cap.JSON(t)
	if _, ok := body["tools"]; ok {
		t.Fatalf(`no tools configured but request carries "tools": %v`, body["tools"])
	}
	if _, ok := body["generationConfig"]; ok {
		t.Fatalf(`no sampling fields but request carries "generationConfig": %v`, body["generationConfig"])
	}

	// An empty JSONSchema() on the response proto must not turn on structured
	// output either.
	if _, err := cl.Call(context.Background(), []events.Event{req}, nil, config.CallOptions{
		ResponseSchema: fakeProto{name: "Out"},
	}); err != nil {
		t.Fatalf("call with empty schema: %v", err)
	}
	body = cap.JSON(t)
	if _, ok := body["generationConfig"]; ok {
		t.Fatalf(`empty schema must not add "generationConfig": %v`, body["generationConfig"])
	}
}

func TestGeminiHTTPStatus300Rejected(t *testing.T) {
	// 300 sits inside the rejected band: status < 200 || status >= 300.
	srv, _ := captureServer(t, 300, `{"error":"nope"}`)
	cl := startClient(t, newTestConfig(srv.URL))
	req := &events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "hi"}}}
	_, err := cl.Call(context.Background(), []events.Event{req}, nil, config.CallOptions{})
	if err == nil || !strings.Contains(err.Error(), "returned 300") {
		t.Fatalf("err = %v, want the returned-300 error", err)
	}
}

func TestGeminiStreamEmptyResponseLeavesMessageNil(t *testing.T) {
	// A streamed answer with no text parts must not produce a ModelMessage.
	srv, _ := sseServer(t, `{"candidates":[]}`)
	cl := startClient(t, newTestConfig(srv.URL, WithStreaming(true)))
	req := &events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "hi"}}}
	resp, err := cl.Call(context.Background(), []events.Event{req}, nil, config.CallOptions{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if resp.Message != nil {
		t.Fatalf("empty stream must not carry a ModelMessage, got %+v", resp.Message)
	}
}

func TestContinuationOfFinishReasonRules(t *testing.T) {
	// A token with no finish reason is resumable; STOP is not; CONTINUATION is.
	var raw genResponse
	if err := json.Unmarshal([]byte(`{"candidates":[{"continuationToken":"tok"}]}`), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	token, ok := continuationOf(&raw)
	if !ok || token != "tok" {
		t.Fatalf("token with no finish reason must be resumable, got %q ok=%v", token, ok)
	}
	raw.Candidates[0].FinishReason = "STOP"
	if _, ok := continuationOf(&raw); ok {
		t.Fatal("STOP must not be resumable")
	}
	raw.Candidates[0].FinishReason = "CONTINUATION"
	if _, ok := continuationOf(&raw); !ok {
		t.Fatal("CONTINUATION must be resumable")
	}
}

// --- mappers.go ------------------------------------------------------------------

func TestBuildContentsSkipsEmptyTurns(t *testing.T) {
	// An empty turn never reaches the wire; a textless model turn with a tool
	// call keeps only the functionCall part; a drained request maps like a
	// plain one.
	got, err := buildContents([]events.Event{
		events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{Name: "f", Arguments: `{}`},
		}}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: ""}},
		events.ModelRequest{},
		&events.DrainedModelRequest{ModelRequest: events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "go"}}}},
		&events.DrainedModelRequest{},
	})
	if err != nil {
		t.Fatalf("buildContents: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 contents (model functionCall + drained user turn), got %d: %+v", len(got), got)
	}
	if got[0].Role != "model" || got[0].Parts[0]["functionCall"] == nil {
		t.Fatalf("want the model functionCall turn, got %+v", got[0])
	}
	if got[1].Role != "user" || got[1].Parts[0]["text"] != "go" {
		t.Fatalf("want the drained user turn with its text, got %+v", got[1])
	}
}

func TestBuildContentsToolResultShape(t *testing.T) {
	// Zero text parts map to an empty response object (no "result" key); a
	// single text part collapses to a bare string.
	got, err := buildContents([]events.Event{events.ToolResultsEvent{Results: []events.ToolResultEvent{
		{Name: "f"},
		{Name: "g", Result: events.ToolResult{Parts: []events.Input{&events.TextInput{Content: "ok"}}}},
	}}})
	if err != nil {
		t.Fatalf("buildContents: %v", err)
	}
	if len(got) != 1 || len(got[0].Parts) != 2 {
		t.Fatalf("want 1 content with 2 parts, got %+v", got)
	}
	resp0, ok := got[0].Parts[0]["functionResponse"].(map[string]any)["response"].(map[string]any)
	if !ok || len(resp0) != 0 {
		t.Fatalf("an empty tool result must map to {}, got %v", got[0].Parts[0])
	}
	resp1, ok := got[0].Parts[1]["functionResponse"].(map[string]any)["response"].(map[string]any)
	if !ok || resp1["result"] != "ok" {
		t.Fatalf("a single text result must collapse to a string, got %v", got[0].Parts[1])
	}
}

func TestGeminiParametersNormalization(t *testing.T) {
	// Parameterless, type-less and null/none-typed schemas normalize to the
	// empty object schema; a typed schema survives with additionalProperties
	// stripped.
	empty := func() map[string]any { return map[string]any{"type": "object", "properties": map[string]any{}} }
	for _, tc := range []struct {
		name   string
		params map[string]any
		want   map[string]any
	}{
		{"empty", map[string]any{}, empty()},
		{"type-less", map[string]any{"properties": map[string]any{"q": map[string]any{"type": "string"}}}, empty()},
		{"null type", map[string]any{"type": "null"}, empty()},
		{"none type", map[string]any{"type": "None"}, empty()},
		{"object type keeps shape", map[string]any{
			"type":                 "OBJECT",
			"properties":           map[string]any{"q": map[string]any{"type": "string"}},
			"additionalProperties": false,
		}, map[string]any{
			// The type travels with its original spelling; only the lowercased
			// copy decides normalization.
			"type":       "OBJECT",
			"properties": map[string]any{"q": map[string]any{"type": "string"}},
		}},
	} {
		got := geminiParameters(tc.params)
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBuildToolsPointerSchemas(t *testing.T) {
	// Pointer forms of every schema type work; typed nils fail fast.
	sch := tools.Schema{Name: "f", Parameters: map[string]any{}}
	decls, err := buildTools([]any{&sch})
	if err != nil {
		t.Fatalf("pointer function schema: %v", err)
	}
	if len(decls) != 1 || decls[0]["functionDeclarations"] == nil {
		t.Fatalf("want one functionDeclarations block, got %v", decls)
	}
	if _, err := buildTools([]any{(*tools.Schema)(nil)}); err == nil {
		t.Fatal("nil *Schema must fail")
	}

	wf := tools.WebFetchToolSchema{}
	got, err := buildTools([]any{&wf})
	if err != nil {
		t.Fatalf("pointer web_fetch: %v", err)
	}
	if len(got) != 1 || got[0]["urlContext"] == nil {
		t.Fatalf("want one urlContext entry, got %v", got)
	}
	if _, err := buildTools([]any{(*tools.WebFetchToolSchema)(nil)}); err == nil {
		t.Fatal("nil *WebFetchToolSchema must fail")
	}

	fs := tools.FileSearchToolSchema{StoreNames: []string{"stores/x"}}
	got, err = buildTools([]any{&fs})
	if err != nil {
		t.Fatalf("pointer file_search: %v", err)
	}
	if len(got) != 1 || got[0]["fileSearch"] == nil {
		t.Fatalf("want one fileSearch entry, got %v", got)
	}
	if _, err := buildTools([]any{(*tools.FileSearchToolSchema)(nil)}); err == nil {
		t.Fatal("nil *FileSearchToolSchema must fail")
	}
}

func TestAddPartInlineDataAndFunctionCall(t *testing.T) {
	var acc genAccumulator
	var p genPart
	payload := base64.StdEncoding.EncodeToString([]byte("bin"))
	if err := json.Unmarshal([]byte(`{"inlineData":{"mimeType":"image/png","data":"`+payload+`"}}`), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := acc.addPart(p, nil, false); err != nil {
		t.Fatalf("valid inlineData must decode, got %v", err)
	}
	if len(acc.files) != 1 || string(acc.files[0].Data) != "bin" {
		t.Fatalf("files: %+v", acc.files)
	}
	if acc.files[0].Metadata["media_type"] != "image/png" {
		t.Fatalf("media_type: %v", acc.files[0].Metadata)
	}

	// A part without a MIME type carries no media_type key.
	var q genPart
	if err := json.Unmarshal([]byte(`{"inlineData":{"data":"aGk="}}`), &q); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := acc.addPart(q, nil, false); err != nil {
		t.Fatalf("addPart: %v", err)
	}
	if _, ok := acc.files[1].Metadata["media_type"]; ok {
		t.Fatal("an empty MIME type must not set media_type")
	}

	// A server-side call id travels verbatim; a missing one is generated and
	// nil args marshal to {}.
	var fc genPart
	if err := json.Unmarshal([]byte(`{"functionCall":{"id":"call-9","name":"f","args":{"a":1}}}`), &fc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := acc.addPart(fc, nil, false); err != nil {
		t.Fatalf("addPart: %v", err)
	}
	if acc.calls[0].ID != "call-9" {
		t.Fatalf("call id: %q", acc.calls[0].ID)
	}
	var fc2 genPart
	if err := json.Unmarshal([]byte(`{"functionCall":{"name":"g"}}`), &fc2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := acc.addPart(fc2, nil, false); err != nil {
		t.Fatalf("addPart: %v", err)
	}
	if acc.calls[1].ID == "" {
		t.Fatal("a missing call id must be generated")
	}
	if acc.calls[1].Arguments != "{}" {
		t.Fatalf("nil args must marshal to {}, got %s", acc.calls[1].Arguments)
	}
}
