package a2a

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	agoevents "github.com/luckystrker/ago/events"
)

// TestEventsToPayloadWireShape pins the ag2.history+json text to the shape
// Python's mappers/history.py produces: same kind discriminators, same field
// names, same nesting.
func TestEventsToPayloadWireShape(t *testing.T) {
	history := []agoevents.Event{
		&agoevents.ModelRequest{Parts: []agoevents.Input{agoevents.TextInput{Content: "hi"}}},
		&agoevents.ToolCallEvent{ID: "call-1", Name: "clock", Arguments: "{}"},
		&agoevents.ToolCallsEvent{Calls: []agoevents.ToolCallEvent{
			{ID: "call-1", Name: "clock", Arguments: "{}"},
		}},
		&agoevents.ToolResultEvent{
			ParentID: "call-1",
			Name:     "clock",
			Result:   agoevents.ToolResult{Parts: []agoevents.Input{agoevents.TextInput{Content: "12:00"}}},
		},
		&agoevents.ToolResultsEvent{Results: []agoevents.ToolResultEvent{
			{ParentID: "call-1", Name: "clock", Result: agoevents.ToolResult{Parts: []agoevents.Input{agoevents.TextInput{Content: "12:00"}}}},
		}},
		&agoevents.ModelResponse{
			Message:   &agoevents.ModelMessage{Content: "the time is 12:00"},
			ToolCalls: agoevents.ToolCallsEvent{Calls: []agoevents.ToolCallEvent{{ID: "call-1", Name: "clock", Arguments: "{}"}}},
		},
		&agoevents.ModelMessage{Content: "done"},
	}

	data, err := json.Marshal(EventsToPayload(history))
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	want := `{"events":[` +
		`{"kind":"user_input","parts":[{"type":"text","text":"hi"}]},` +
		`{"kind":"tool_call","id":"call-1","name":"clock","arguments":"{}"},` +
		`{"kind":"tool_calls","calls":[{"id":"call-1","name":"clock","arguments":"{}"}]},` +
		`{"kind":"tool_result","parent_id":"call-1","name":"clock","content":"12:00"},` +
		`{"kind":"tool_results","results":[{"parent_id":"call-1","name":"clock","content":"12:00"}]},` +
		`{"kind":"model_response","tool_calls":[{"id":"call-1","name":"clock","arguments":"{}"}],"content":"the time is 12:00"},` +
		`{"kind":"agent_message","content":"done"}` +
		`]}`
	if string(data) != want {
		t.Fatalf("payload shape\n got: %s\nwant: %s", data, want)
	}
}

func TestHistoryPayloadRoundTrip(t *testing.T) {
	binary := agoevents.BinaryInput{
		Data:           []byte{1, 2, 3},
		MediaType:      "image/png",
		Kind:           agoevents.BinaryImage,
		VendorMetadata: map[string]any{"filename": "a.png"},
	}
	cases := map[string]agoevents.Event{
		"user text": &agoevents.ModelRequest{Parts: []agoevents.Input{agoevents.TextInput{Content: "plain"}}},
		"user data": &agoevents.ModelRequest{Parts: []agoevents.Input{
			agoevents.DataInput{Data: map[string]any{"k": "v"}},
		}},
		"user binary": &agoevents.ModelRequest{Parts: []agoevents.Input{binary}},
		"user url": &agoevents.ModelRequest{Parts: []agoevents.Input{
			agoevents.UrlInput{URL: "https://example.test/a.png", Kind: agoevents.BinaryImage},
		}},
		"user file": &agoevents.ModelRequest{Parts: []agoevents.Input{
			agoevents.FileIdInput{FileID: "file-1", Filename: "a.png"},
		}},
		"tool call":  &agoevents.ToolCallEvent{ID: "c1", Name: "t", Arguments: `{"x":1}`},
		"tool calls": &agoevents.ToolCallsEvent{Calls: []agoevents.ToolCallEvent{{ID: "c1", Name: "t", Arguments: "{}"}}},
		"tool result": &agoevents.ToolResultEvent{
			ParentID: "c1", Name: "t",
			Result: agoevents.ToolResult{Parts: []agoevents.Input{agoevents.TextInput{Content: "ok"}}},
		},
		"tool results": &agoevents.ToolResultsEvent{Results: []agoevents.ToolResultEvent{{
			ParentID: "c1", Name: "t",
			Result: agoevents.ToolResult{Parts: []agoevents.Input{agoevents.TextInput{Content: "ok"}}},
		}}},
		"model response": &agoevents.ModelResponse{
			Message: &agoevents.ModelMessage{Content: "hello"},
		},
		"model response with calls": &agoevents.ModelResponse{
			ToolCalls: agoevents.ToolCallsEvent{Calls: []agoevents.ToolCallEvent{{ID: "c1", Name: "t", Arguments: "{}"}}},
		},
		"agent message": &agoevents.ModelMessage{Content: "note"},
	}
	for name, event := range cases {
		t.Run(name, func(t *testing.T) {
			restored := PayloadToEvents(EventsToPayload([]agoevents.Event{event}))
			if len(restored) != 1 {
				t.Fatalf("restored %d events, want 1", len(restored))
			}
			if !reflect.DeepEqual(restored[0], event) {
				t.Fatalf("round trip\n got: %#v\nwant: %#v", restored[0], event)
			}
		})
	}
}

// A failed tool result must keep its error branch through the wire: Python
// rebuilds a ToolErrorEvent from the text, not a plain result.
func TestToolErrorRoundTripsAsError(t *testing.T) {
	failed := &agoevents.ToolErrorEvent{
		ToolResultEvent: agoevents.ToolResultEvent{
			ParentID: "c1", Name: "t",
			Result: agoevents.ToolResult{Parts: []agoevents.Input{agoevents.TextInput{Content: "boom"}}},
		},
		Error: "boom",
	}
	restored := PayloadToEvents(EventsToPayload([]agoevents.Event{failed}))
	if len(restored) != 1 {
		t.Fatalf("restored %d events, want 1", len(restored))
	}
	errEvent, ok := restored[0].(*agoevents.ToolErrorEvent)
	if !ok {
		t.Fatalf("restored %T, want *events.ToolErrorEvent", restored[0])
	}
	if errEvent.Error != "boom" || errEvent.ParentID != "c1" {
		t.Fatalf("restored error event = %#v", errEvent)
	}
}

// Transient and unknown events are conversation deltas or future additions: the
// payload drops them and both sides stay compatible.
func TestHistoryPayloadSkipsTransientAndUnknown(t *testing.T) {
	history := []agoevents.Event{
		&agoevents.ModelMessageChunk{Content: "chunk"},
		&agoevents.ModelReasoning{Content: "thinking"},
		&agoevents.TaskStarted{},
		&agoevents.ModelMessage{Content: "kept"},
	}
	payload := EventsToPayload(history)
	if len(payload.Events) != 1 || payload.Events[0].Kind != HistoryKindAgentMessage {
		t.Fatalf("payload events = %#v, want only the agent message", payload.Events)
	}

	unknown := HistoryPayload{Events: []HistoryEvent{
		{Kind: "future_kind"},
		{Kind: HistoryKindAgentMessage, Content: "known"},
	}}
	restored := PayloadToEvents(unknown)
	if len(restored) != 1 {
		t.Fatalf("restored %d events, want the known one only", len(restored))
	}
}

func TestToolResultMessageCarriesHistory(t *testing.T) {
	history := []agoevents.Event{
		&agoevents.ModelRequest{Parts: []agoevents.Input{agoevents.TextInput{Content: "hi"}}},
		&agoevents.ModelResponse{Message: &agoevents.ModelMessage{Content: "hello"}},
	}
	msg := BuildToolResultMessage("task-1", "ctx-1",
		[]ToolResultEntry{{ID: "c1", Name: "t", Content: "ok"}},
		[]ToolSchema{{Name: "t"}},
		WithHistory(history),
	)

	var kinds []string
	for _, p := range msg.Parts {
		if dp, ok := p.(*DataPart); ok {
			kinds = append(kinds, dp.MimeType)
		}
	}
	want := []string{MIMEToolResult, MIMEToolSchemas, MIMEHistory}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("part order = %v, want %v (parity: results, schemas, history)", kinds, want)
	}

	parsed, err := ParseHistory(msg)
	if err != nil {
		t.Fatalf("parse history: %v", err)
	}
	if !reflect.DeepEqual(parsed, history) {
		t.Fatalf("parsed history = %#v, want %#v", parsed, history)
	}
}

// TestMultiTurnContinuationRestoresFullHistory is the end-to-end check: each
// continuation ships the whole conversation so far, and the server sees every
// prior turn exactly once, in order.
func TestMultiTurnContinuationRestoresFullHistory(t *testing.T) {
	type restore struct {
		taskID  string
		history []agoevents.Event
	}
	var (
		mu       sync.Mutex
		restores []restore
	)
	srv := httptest.NewServer(NewServer(ServerConfig{
		Card:   testCard(false),
		Handle: echoHandler,
		RestoreHistory: func(_ context.Context, taskID string, history []agoevents.Event) error {
			mu.Lock()
			defer mu.Unlock()
			restores = append(restores, restore{taskID: taskID, history: history})
			return nil
		},
	}).Handler())
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL)

	// Turn 1: the opening message carries no history.
	reply, err := client.SendMessage(context.Background(), NewMessage(RoleUser, NewTextPart("one")))
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	taskID := reply.TaskID

	history := []agoevents.Event{
		&agoevents.ModelRequest{Parts: []agoevents.Input{agoevents.TextInput{Content: "one"}}},
		&agoevents.ModelResponse{Message: &agoevents.ModelMessage{Content: "echo: one"}},
	}
	// Turn 2 and 3: continuations on the same task, each shipping the history
	// it has seen so far.
	for _, turn := range []struct {
		input   string
		reply   string
		history []agoevents.Event
	}{
		{"two", "echo: two", history},
		{"three", "echo: three", append(history,
			&agoevents.ModelRequest{Parts: []agoevents.Input{agoevents.TextInput{Content: "two"}}},
			&agoevents.ModelResponse{Message: &agoevents.ModelMessage{Content: "echo: two"}},
		)},
	} {
		msg := NewMessage(RoleUser, NewTextPart(turn.input))
		msg.TaskID = taskID
		msg.ContextID = reply.ContextID
		AttachHistory(msg, turn.history)
		got, err := client.SendMessage(context.Background(), msg)
		if err != nil {
			t.Fatalf("turn %q: %v", turn.input, err)
		}
		if textOf(got) != turn.reply {
			t.Fatalf("turn %q reply = %q, want %q", turn.input, textOf(got), turn.reply)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(restores) != 2 {
		t.Fatalf("RestoreHistory ran %d times, want once per continuation (2)", len(restores))
	}
	if restores[0].taskID != taskID || restores[1].taskID != taskID {
		t.Fatalf("restored under task ids %q/%q, want %q", restores[0].taskID, restores[1].taskID, taskID)
	}
	if got := len(restores[0].history); got != 2 {
		t.Fatalf("first continuation restored %d events, want 2", got)
	}
	if got := len(restores[1].history); got != 4 {
		t.Fatalf("second continuation restored %d events, want 4", got)
	}
	// No gaps and no duplicates: the second restore is the first plus turn two,
	// and every entry appears exactly once, in order.
	for i, ev := range restores[0].history {
		if !reflect.DeepEqual(restores[1].history[i], ev) {
			t.Fatalf("restored history diverges at %d: %#v vs %#v", i, restores[1].history[i], ev)
		}
	}
	if !reflect.DeepEqual(restores[1].history[2:], []agoevents.Event{
		&agoevents.ModelRequest{Parts: []agoevents.Input{agoevents.TextInput{Content: "two"}}},
		&agoevents.ModelResponse{Message: &agoevents.ModelMessage{Content: "echo: two"}},
	}) {
		t.Fatalf("second continuation restored %#v, want the prior turns plus turn two", restores[1].history)
	}

	task, err := client.TasksGet(context.Background(), taskID)
	if err != nil {
		t.Fatalf("tasks/get: %v", err)
	}
	if len(task.History) != 3 {
		t.Fatalf("stored task history has %d messages, want 3 (one per turn, no duplicates)", len(task.History))
	}
}

// A history part that is not a payload object is a client bug and is refused
// before the handler runs.
func TestMalformedHistoryPartIsInvalidParams(t *testing.T) {
	srv := httptest.NewServer(NewServer(ServerConfig{
		Card:           testCard(false),
		Handle:         echoHandler,
		RestoreHistory: func(context.Context, string, []agoevents.Event) error { return nil },
	}).Handler())
	t.Cleanup(srv.Close)

	body := rpcBody(MethodMessageSend, `{"message":{"role":"user","parts":[{"kind":"text","text":"hi"},`+
		`{"kind":"data","mediaType":"`+MIMEHistory+`","data":"not-a-payload"}]}}`, "1")
	envelope := postRPC(t, srv.Config.Handler, body)
	if envelope.Error == nil || envelope.Error.Code != codeInvalidParams {
		t.Fatalf("history decode failure answered %#v, want %d invalid params", envelope.Error, codeInvalidParams)
	}
}

// textOf returns the concatenated text parts of a message.
func textOf(msg *Message) string {
	if msg == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range msg.Parts {
		if tp, ok := p.(*TextPart); ok {
			b.WriteString(tp.Text)
		}
	}
	return b.String()
}

var _ = http.MethodPost

// TestHistoryToolCallArgumentDefault pins the SDK's default for an absent
// `arguments`: payload_to_call reads `payload.get("arguments", "{}")`, so a
// call entry without the field decodes to "{}", not to "". Both the flattened
// tool_call entry and the tool_calls batch take that path.
func TestHistoryToolCallArgumentDefault(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"tool_call entry", `{"events":[{"kind":"tool_call","id":"c1","name":"clock"}]}`},
		{"tool_calls batch", `{"events":[{"kind":"tool_calls","calls":[{"id":"c1","name":"clock"}]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var payload HistoryPayload
			if err := json.Unmarshal([]byte(tc.payload), &payload); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			events := PayloadToEvents(payload)
			if len(events) != 1 {
				t.Fatalf("decoded %d events, want 1", len(events))
			}
			var call *agoevents.ToolCallEvent
			switch e := events[0].(type) {
			case *agoevents.ToolCallEvent:
				call = e
			case *agoevents.ToolCallsEvent:
				call = &e.Calls[0]
			default:
				t.Fatalf("decoded %T, want a tool call", events[0])
			}
			if call.Arguments != "{}" {
				t.Fatalf("arguments = %q, want %q (payload.get default)", call.Arguments, "{}")
			}
		})
	}

	// An explicit argument string survives untouched.
	var payload HistoryPayload
	if err := json.Unmarshal([]byte(`{"events":[{"kind":"tool_call","id":"c1","name":"t","arguments":"{\"x\":1}"}]}`), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	call, ok := PayloadToEvents(payload)[0].(*agoevents.ToolCallEvent)
	if !ok || call.Arguments != `{"x":1}` {
		t.Fatalf("explicit arguments = %#v, want the wire string", call)
	}
}

// TestToolResultTextFallback: a non-text part is rendered as JSON, and a part
// that cannot be serialised still yields text instead of silently disappearing
// from the transcript (the fallback branch is what keeps a result visible).
func TestToolResultTextFallback(t *testing.T) {
	text := toolResultText(agoevents.ToolResult{Parts: []agoevents.Input{
		agoevents.TextInput{Content: "ok "},
		agoevents.DataInput{Data: map[string]any{"k": "v"}},
	}})
	if !strings.HasPrefix(text, "ok ") || !strings.Contains(text, `"data":{"k":"v"}`) {
		t.Fatalf("toolResultText = %q, want the text plus the JSON encoding", text)
	}

	unmarshalable := toolResultText(agoevents.ToolResult{Parts: []agoevents.Input{
		agoevents.DataInput{Data: make(chan int)},
	}})
	if strings.TrimSpace(unmarshalable) == "" {
		t.Fatal("an unserialisable result part rendered as nothing")
	}
}
