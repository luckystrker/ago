package tools

import (
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
)

// countingMW counts the tool executions that passed through it.
type countingMW struct {
	middleware.Base
	n int
}

func (m *countingMW) OnToolExecution(next middleware.ToolExecFunc, ev *events.ToolCallEvent, cx *agocontext.Context) (events.Event, error) {
	m.n++
	return next(ev, cx)
}

// TestClientToolScopedMiddleware pins the tool-scoped middleware option on the
// client-tool factory: the middleware wraps the conversion to
// ClientToolCallEvent, and the call still produces that event.
func TestClientToolScopedMiddleware(t *testing.T) {
	mw := &countingMW{}
	ct, err := NewClientTool(map[string]any{"name": "deploy"}, WithClientToolMiddleware(mw))
	if err != nil {
		t.Fatalf("NewClientTool: %v", err)
	}
	s := stream.NewMemoryStream()
	cx := agocontext.New(s)
	if _, err := ct.Register(cx, nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := cx.Send(events.ToolCallEvent{ID: "c1", Name: "deploy", Arguments: `{"x":1}`}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if mw.n != 1 {
		t.Fatalf("client tool middleware ran %d times, want 1", mw.n)
	}
	evs, err := s.History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var saw bool
	for _, ev := range evs {
		if call, ok := events.AsValue(ev).(events.ClientToolCallEvent); ok && call.Name == "deploy" {
			saw = true
		}
	}
	if !saw {
		t.Fatal("no ClientToolCallEvent on the stream")
	}
}
