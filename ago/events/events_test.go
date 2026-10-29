package events

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMarshalRoundTripAllRegisteredTypes(t *testing.T) {
	inputs := []Input{
		TextInput{Content: "hello"},
		DataInput{Data: map[string]any{"k": 1.0}},
		BinaryInput{Data: []byte{1, 2}, MediaType: "image/png", Kind: BinaryImage},
		UrlInput{URL: "https://example.com/a.png", Kind: BinaryImage},
		FileIdInput{FileID: "f1", Filename: "a.png"},
	}
	eventsList := []Event{
		&ModelRequest{Parts: inputs, Metadata: map[string]any{"src": "test"}},
		&DrainedModelRequest{ModelRequest{Parts: inputs}},
		&ModelMessage{Content: "assistant text"},
		&ModelMessageChunk{Content: "chunk"},
		&ModelReasoning{Content: "thinking"},
		&ModelResponse{
			Message:   &ModelMessage{Content: "final"},
			ToolCalls: ToolCallsEvent{Calls: []ToolCallEvent{NewToolCallEvent("get_weather")}},
			Usage:     Usage{PromptTokens: I64(3), TotalTokens: I64(5)},
		},
		&UsageEvent{Usage: Usage{PromptTokens: I64(1)}, Kind: UsageKindModelCall,
			Parts: []UsageEvent{{Usage: Usage{PromptTokens: I64(1)}, Kind: UsageKindModelCall}}},
		&HumanInputRequest{ID: "r1", Content: "approve?"},
		&ToolApprovalRequest{HumanInputRequest: HumanInputRequest{ID: "r2", Content: "ok?"}, ToolCallID: "c1"},
		&HumanMessage{ParentID: "r1", Content: "yes"},
		&ToolCallsEvent{Calls: []ToolCallEvent{NewToolCallEvent("t1")}},
		&ToolResultsEvent{Results: []ToolResultEvent{FromResultCall(NewToolCallEvent("t1"), ToolResult{Parts: []Input{TextInput{Content: "done"}}})}},
		&ToolResultEvent{ParentID: "c1", Name: "t1", Result: NewToolResult(false, "ok", map[string]any{"x": 2.0})},
		&ToolErrorEvent{ToolResultEvent: ToolResultEvent{ParentID: "c1", Result: ToolResult{Parts: []Input{TextInput{Content: "boom"}}}}, Error: "boom"},
		&ToolNotFoundEvent{ToolErrorEvent: ToolErrorEvent{ToolResultEvent: ToolResultEvent{ParentID: "c1"}, Error: "nope"}},
		&ClientToolCallEvent{ID: "c2", Name: "remote", Arguments: "{}"},
		&BuiltinToolCallEvent{ToolCallEvent: NewToolCallEvent("web_search")},
		&BuiltinToolResultEvent{ToolResultEvent: ToolResultEvent{ParentID: "c3", Result: NewToolResult(false, "r")}},
		&CompactionSummary{Content: "summary"},
		&TaskStarted{TaskID: "t", AgentName: "a", ParentToolCallID: "call-1",
			Spec: &TaskSpec{Title: "research", Payload: map[string]any{"k": 1.0}, Capability: "search"}},
		&TaskProgress{TaskID: "t", AgentName: "a", Payload: map[string]any{"p": 1.0}},
		&TaskCompleted{TaskID: "t", AgentName: "a", Usage: Usage{TotalTokens: I64(7)}},
		&TaskFailed{TaskID: "t", AgentName: "a", Error: "e"},
		&TaskExpired{TaskID: "t", AgentName: "a"},
		&TaskCancelled{TaskID: "t", AgentName: "a", Reason: "r"},
		&ObserverAlert{Source: "loop", Severity: SeverityWarning, Message: "m"},
		&HaltEvent{Reason: "halt"},
		&ObserverStarted{Name: "n"},
		&ObserverCompleted{Name: "n"},
		&CompactionStarted{Agent: "a", Strategy: "s", EventCount: 3},
		&CompactionCompleted{Agent: "a", Strategy: "s"},
		&CompactionFailed{Agent: "a", Strategy: "s", Error: "e"},
		&AggregationStarted{Agent: "a"},
		&AggregationCompleted{Agent: "a"},
		&AggregationFailed{Agent: "a", Error: "e"},
		&EventLogFailed{Agent: "a", Error: "e"},
	}

	for _, ev := range eventsList {
		data, err := MarshalEvent(ev)
		if err != nil {
			t.Fatalf("%T: marshal: %v", ev, err)
		}
		back, err := UnmarshalEvent(data)
		if err != nil {
			t.Fatalf("%T: unmarshal: %v (%s)", ev, err, data)
		}
		if back.EventType() != ev.EventType() {
			t.Errorf("%T: type mismatch: got %s", ev, back.EventType())
		}
		// Deep equality via a second marshal of the decoded value.
		again, err := MarshalEvent(back)
		if err != nil {
			t.Fatalf("%T: remarshal: %v", ev, err)
		}
		var a, b map[string]any
		_ = json.Unmarshal(data, &a)
		_ = json.Unmarshal(again, &b)
		if len(a) != len(b) {
			t.Errorf("%T: field count changed across round trip: %d != %d", ev, len(a), len(b))
		}
	}
}

func TestUnmarshalUnknownType(t *testing.T) {
	ev, err := UnmarshalEvent([]byte(`{"@type":"FutureEvent","x":1}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ue, ok := ev.(UnknownEvent)
	if !ok || ue.TypeName != "FutureEvent" {
		t.Fatalf("want UnknownEvent FutureEvent, got %#v", ev)
	}
}

func TestUnmarshalInputRejectsNonInput(t *testing.T) {
	data, _ := MarshalEvent(ModelMessage{Content: "x"})
	if _, err := UnmarshalInput(data); err == nil {
		t.Fatal("expected error for non-Input event")
	}
}

func TestConditions(t *testing.T) {
	mr := &ModelRequest{Parts: []Input{TextInput{Content: "hi"}}}
	resp := &ModelResponse{Message: &ModelMessage{Content: "hey"}}
	dr := &DrainedModelRequest{}

	if !TypeOf[*ModelRequest]()(mr) || TypeOf[*ModelRequest]()(resp) {
		t.Error("TypeOf[*ModelRequest] misbehaves")
	}
	cond := AnyOf(TypeOf[ModelRequest](), TypeOf[DrainedModelRequest](), TypeOf[ToolResultsEvent]())
	if !cond(mr) || !cond(dr) || cond(resp) {
		t.Error("AnyOf type condition misbehaves")
	}
	if Not(TypeOf[ModelResponse]())(resp) || !Not(TypeOf[ModelResponse]())(mr) {
		t.Error("Not misbehaves")
	}
	nameEq := Equal(func(ev Event) string {
		if tc, ok := ev.(ToolCallEvent); ok {
			return tc.Name
		}
		return ""
	}, "get_weather")
	if !nameEq(ToolCallEvent{Name: "get_weather"}) || nameEq(ToolCallEvent{Name: "other"}) {
		t.Error("Equal misbehaves")
	}
	if !AllOf()(mr) || AnyOf()(mr) {
		t.Error("empty combinators misbehave")
	}
}

func TestUsageAdd(t *testing.T) {
	a := Usage{PromptTokens: I64(3)}
	b := Usage{PromptTokens: I64(4), TotalTokens: I64(7)}
	got := a.Add(b)
	if got.PromptTokens == nil || *got.PromptTokens != 7 {
		t.Errorf("PromptTokens: want 7, got %v", got.PromptTokens)
	}
	if got.TotalTokens == nil || *got.TotalTokens != 7 {
		t.Errorf("TotalTokens: want 7, got %v", got.TotalTokens)
	}
	if got.CompletionTokens != nil {
		t.Error("nil+nil should stay nil")
	}
}

func TestRepairUnansweredToolCalls(t *testing.T) {
	call := NewToolCallEvent("t")
	answered := NewToolCallEvent("ok")
	evs := []Event{
		&ModelRequest{Parts: []Input{TextInput{Content: "go"}}},
		call,
		answered,
		&ToolResultEvent{ParentID: answered.ID, Result: NewToolResult(false, "fine")},
	}
	got := UnansweredToolCalls(evs)
	if len(got) != 1 || got[0].ID != call.ID {
		t.Fatalf("want exactly the unanswered call, got %v", got)
	}
	fixes := CloseUnansweredToolCalls(evs)
	if len(fixes) != 1 || fixes[0].ParentID != call.ID {
		t.Fatalf("want one repair event, got %v", fixes)
	}
	if fixes[0].Result.Parts[0].(TextInput).Content != SentinelToolResult {
		t.Errorf("sentinel content wrong: %v", fixes[0].Result.Parts)
	}
	// Pin the phrase itself so a value drift cannot hide behind the constant.
	if got := fixes[0].Result.Parts[0].(TextInput).Content; got != "This tool call was not completed: the turn ended because a human-input request could not be answered." {
		t.Errorf("sentinel drifted from the Python phrase: %q", got)
	}
	if len(UnansweredToolCalls(append(append([]Event{}, evs...), fixes[0]))) != 0 {
		t.Error("repair did not answer the call")
	}
}

func TestEnsureInputAndRequest(t *testing.T) {
	if _, ok := EnsureInput("s").(*TextInput); !ok {
		t.Error("string should become *TextInput")
	}
	if _, ok := EnsureInput(map[string]any{"a": 1}).(*DataInput); !ok {
		t.Error("map should become *DataInput")
	}
	ti := &TextInput{Content: "kept"}
	if got := EnsureInput(ti); got != Input(ti) {
		t.Error("Input should pass through")
	}
	req := EnsureRequest("a", "b")
	if len(req.Parts) != 2 {
		t.Fatalf("want 2 parts, got %d", len(req.Parts))
	}
}

func TestMediaFactories(t *testing.T) {
	if _, err := ImageInput("https://x/y.png", "", nil, "", ""); err != nil {
		t.Errorf("url input: %v", err)
	}
	// A file_id is its own source (exactly one of url|file_id|path|data).
	in, err := ImageInput("", "", nil, "", "file-123")
	if err != nil {
		t.Fatalf("file_id input: %v", err)
	}
	fid, ok := in.(*FileIdInput)
	if !ok || fid.FileID != "file-123" {
		t.Fatalf("file_id input = %#v, want *FileIdInput", in)
	}
	// A path is read eagerly: bytes, media type and filename travel with the part.
	dir := t.TempDir()
	p := filepath.Join(dir, "pic.png")
	if err := os.WriteFile(p, []byte("pngbytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	in, err = ImageInput("", p, nil, "", "")
	if err != nil {
		t.Fatalf("path input: %v", err)
	}
	bin, ok := in.(*BinaryInput)
	if !ok {
		t.Fatalf("path input = %T, want *BinaryInput", in)
	}
	if string(bin.Data) != "pngbytes" || bin.MediaType != "image/png" || bin.Kind != BinaryImage {
		t.Errorf("path input = %+v", bin)
	}
	if bin.VendorMetadata["filename"] != "pic.png" {
		t.Errorf("vendor metadata = %v, want filename \"pic.png\"", bin.VendorMetadata)
	}
	// An explicit media type wins over the extension.
	in, err = ImageInput("", p, nil, "application/custom", "")
	if err != nil || in.(*BinaryInput).MediaType != "application/custom" {
		t.Errorf("explicit media type: %v, %+v", err, in)
	}
	// An unknown extension fails at the extension check, before any read.
	if _, err := ImageInput("", filepath.Join(dir, "blob.xyz"), nil, "", ""); err == nil {
		t.Error("unknown extension should fail")
	} else if !strings.Contains(err.Error(), "cannot infer image media type") {
		t.Errorf("error = %v, want an inference hint", err)
	}
	if _, err := ImageInput("", "", []byte{1}, "image/png", ""); err != nil {
		t.Errorf("data input: %v", err)
	}
	if _, err := ImageInput("", "", []byte{1}, "", ""); err == nil {
		t.Error("data without media type should fail")
	}
	if _, err := ImageInput("u", "p", nil, "", ""); err == nil {
		t.Error("two sources should fail")
	}
}

func TestEstimatedTokens(t *testing.T) {
	if got := EstimatedTokens(TextInput{Content: "abcdefgh"}, 4); got != 2 {
		t.Errorf("want 2, got %d", got)
	}
	req := EnsureRequest("01234567")
	if got := EstimatedTokens(*req, 4); got != 2 {
		t.Errorf("request: want 2, got %d", got)
	}
	if got := EstimatedTokens(BinaryInput{Data: []byte("x"), MediaType: "image/png", Kind: BinaryImage}, 4); got < 1 {
		t.Error("binary input should estimate at least 1")
	}
}

// TestEstimatedTokensContainersAndTelemetry pins sizing parity fixes: the
// ToolResultsEvent container (the executor's main result carrier) must count its
// contents instead of flooring to 1, telemetry must size to 0 (not the 1 floor),
// and DrainedModelRequest must size like ModelRequest despite the Go type switch
// not reaching the embedded struct (Python isinstance covers the subclass).
func TestEstimatedTokensContainersAndTelemetry(t *testing.T) {
	call := NewToolCallEvent("search")
	res := ToolResultsEvent{Results: []ToolResultEvent{
		FromResultCall(call, NewToolResult(false, strings.Repeat("x", 80))), // 20 tokens
	}}
	if got := EstimatedTokens(res, 4); got < 20 {
		t.Errorf("ToolResultsEvent = %d, want >= 20 (the result text alone)", got)
	}
	ue := &UsageEvent{Usage: Usage{TotalTokens: I64(500)}, Kind: UsageKindModelCall}
	if got := EstimatedTokens(ue, 4); got != 0 {
		t.Errorf("UsageEvent = %d, want 0 (telemetry occupies no budget)", got)
	}
	parts := []Input{TextInput{Content: "abcdefgh"}}
	base := EstimatedTokens(ModelRequest{Parts: parts}, 4)
	drained := EstimatedTokens(DrainedModelRequest{ModelRequest{Parts: parts}}, 4)
	if base == 0 || base != drained {
		t.Errorf("DrainedModelRequest = %d, ModelRequest = %d, want equal non-zero", drained, base)
	}
}

func TestEstimatedTokensModelResponseExtras(t *testing.T) {
	resp := ModelResponse{
		ToolCalls: ToolCallsEvent{Calls: []ToolCallEvent{{ID: "c1", Name: "get_weather", Arguments: `{"city":"Paris"}`}}},
		Files:     []BinaryResult{{Data: []byte{1}}},
	}
	// content 0 + usage 0 + "get_weather({...})" = 29 chars / 4 = 7 + one flat media budget.
	if got, want := EstimatedTokens(resp, 4), 7+binaryMediaTokens; got != want {
		t.Errorf("ModelResponse with call+file = %d, want %d", got, want)
	}
}

// TestRenderForPromptLabels pins the role labels and the telemetry gate (Python
// parity: sizing.py render_for_prompt).
func TestRenderForPromptLabels(t *testing.T) {
	if got := RenderForPrompt(&ModelRequest{Parts: []Input{TextInput{Content: "hi"}}}); got != "User: hi" {
		t.Errorf("request render = %q", got)
	}
	if got := RenderForPrompt(DrainedModelRequest{ModelRequest{Parts: []Input{TextInput{Content: "hi"}}}}); got != "User: hi" {
		t.Errorf("drained render = %q", got)
	}
	if got := RenderForPrompt(&ModelResponse{Message: &ModelMessage{Content: "hey"}}); got != "Assistant: hey" {
		t.Errorf("response render = %q", got)
	}
	call := NewToolCallEvent("search")
	res := ToolResultsEvent{Results: []ToolResultEvent{FromResultCall(call, NewToolResult(false, "found"))}}
	if got := RenderForPrompt(res); got != "Tool: found" {
		t.Errorf("results render = %q", got)
	}
	if got := RenderForPrompt(&UsageEvent{Kind: UsageKindModelCall}); got != "" {
		t.Errorf("telemetry render = %q, want empty", got)
	}
}

// TestConversationalParity pins the Conversational flag to the Python default
// (__conversational__ = True on durable events; ag2/events/base.py:255): the
// answer, the tool-call batch and the task lifecycle are part of what compaction
// counts and policies replay. Transient events are conversational-inert by
// is_conversational, and UsageEvent is the one durable False (types.py:172).
func TestConversationalParity(t *testing.T) {
	durable := []Event{
		&HumanMessage{ParentID: "r1", Content: "yes"},
		&HumanInputRequest{ID: "r1", Content: "approve?"},
		&ToolCallsEvent{Calls: []ToolCallEvent{NewToolCallEvent("search")}},
		&TaskStarted{TaskID: "t1"},
		&TaskCompleted{TaskID: "t1"},
		&TaskFailed{TaskID: "t1"},
		&TaskExpired{TaskID: "t1"},
		&TaskCancelled{TaskID: "t1"},
		&ModelResponse{Message: &ModelMessage{Content: "hi"}},
	}
	for _, ev := range durable {
		if !ev.Conversational() {
			t.Errorf("%s: durable event must be conversational", ev.EventType())
		}
	}
	transient := []Event{
		&ModelMessage{Content: "internal"},
		&TaskProgress{TaskID: "t1"},
	}
	for _, ev := range transient {
		if ev.Conversational() {
			t.Errorf("%s: transient event must not count as conversational", ev.EventType())
		}
	}
	if (&UsageEvent{Kind: UsageKindModelCall}).Conversational() {
		t.Error("UsageEvent must not be conversational")
	}
}
