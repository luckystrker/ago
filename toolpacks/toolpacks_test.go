package toolpacks

import (
	"encoding/json"
	"testing"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
)

// newTestContext builds a fresh stream and its conversation context.
func newTestContext() (*stream.MemoryStream, *agocontext.Context) {
	s := stream.NewMemoryStream()
	return s, agocontext.New(s)
}

// callTool drives one tool call through the real machinery: Register on the
// stream, the call event, and the settlement (tools test parity: callDirectly).
func callTool(t *testing.T, tool tools.Tool, cx *agocontext.Context, name, args string) events.Event {
	t.Helper()
	subs, err := tool.Register(cx, nil)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, id := range subs {
		defer cx.Stream.Unsubscribe(id)
	}
	s := cx.Stream
	ch := make(chan events.Event, 8)
	sub := s.Subscribe(func(ev events.Event) error { ch <- ev; return nil }, stream.SubscribeOptions{})
	defer s.Unsubscribe(sub)
	call := events.NewToolCallEvent(name)
	call.Arguments = args
	if err := s.Send(call); err != nil {
		t.Fatalf("send call: %v", err)
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-ch:
			switch events.AsValue(ev).(type) {
			case events.ToolResultEvent, events.ToolErrorEvent:
				return ev
			}
		case <-deadline:
			t.Fatal("timed out waiting for the tool result")
		}
	}
}

// resultPartText extracts the first TextInput content of a result event.
func resultPartText(t *testing.T, ev events.Event) string {
	t.Helper()
	switch res := events.AsValue(ev).(type) {
	case events.ToolResultEvent:
		if len(res.Result.Parts) == 0 {
			t.Fatalf("empty result parts: %+v", res.Result)
		}
		if txt, ok := events.AsValue(res.Result.Parts[0]).(events.TextInput); ok {
			return txt.Content
		}
		t.Fatalf("first part is %T, want TextInput", res.Result.Parts[0])
	case events.ToolErrorEvent:
		return "TOOL_ERROR: " + res.Error
	}
	t.Fatalf("unexpected event %T", ev)
	return ""
}

// firstDataJSON marshals the result's first DataInput part payload ("data") to
// JSON (the wire shape a struct or slice return becomes).
func firstDataJSON(t *testing.T, ev events.Event) string {
	t.Helper()
	res, ok := events.AsValue(ev).(events.ToolResultEvent)
	if !ok {
		t.Fatalf("event %T is not a result", ev)
	}
	if len(res.Result.Parts) == 0 {
		t.Fatal("no parts")
	}
	b, err := json.Marshal(events.AsValue(res.Result.Parts[0]))
	if err != nil {
		t.Fatalf("marshal part: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal part: %v", err)
	}
	data, err := json.Marshal(m["data"])
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	return string(data)
}
