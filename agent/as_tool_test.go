package agent

import (
	stdcontext "context"
	"sync"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
	agotest "github.com/luckystrker/ago/testing"
)

// countingToolMW counts the tool executions it wrapped.
type countingToolMW struct {
	middleware.Base
	mu sync.Mutex
	n  int
}

func (m *countingToolMW) OnToolExecution(next middleware.ToolExecFunc, ev *events.ToolCallEvent, cx *agocontext.Context) (events.Event, error) {
	m.mu.Lock()
	m.n++
	m.mu.Unlock()
	return next(ev, cx)
}

func (m *countingToolMW) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.n
}

// TestAsToolPinnedStreamAndMiddleware pins the two AsTool options (Python parity:
// subagent_tool's stream= and middleware=): a pinned stream carries every
// delegation, and the tool-scoped middleware wraps the delegation call.
func TestAsToolPinnedStreamAndMiddleware(t *testing.T) {
	child := New("child", "", WithConfig(agotest.NewTestConfig(textResp("child answer"))))
	delegStream := stream.NewMemoryStream()
	mw := &countingToolMW{}
	parent := New("parent", "", WithConfig(agotest.NewTestConfig(
		toolResp("delegate", `{"objective":"do it"}`),
		textResp("wrapped"),
	)), WithTools(child.AsTool(WithToolName("delegate"), WithToolStream(delegStream), WithToolMiddleware(mw))))

	if _, err := parent.Ask(stdcontext.Background(), "go"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if mw.count() != 1 {
		t.Fatalf("tool middleware ran %d times, want 1", mw.count())
	}
	evs, err := delegStream.History().GetEvents()
	if err != nil {
		t.Fatalf("delegation stream history: %v", err)
	}
	var started, completed bool
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.TaskStarted:
			started = started || e.AgentName == "child"
		case events.TaskCompleted:
			completed = completed || e.AgentName == "child"
		}
	}
	if !started || !completed {
		t.Fatalf("pinned stream lifecycle: started=%v completed=%v, want both (events: %d)", started, completed, len(evs))
	}
}

// TestPersistentStreamReusesOneStreamPerConversation pins persistent_stream():
// the factory returns the same stream id for the same agent and conversation,
// a different one per agent, and the second delegation reads back the first
// delegation's history.
func TestPersistentStreamReusesOneStreamPerConversation(t *testing.T) {
	parentStream := stream.NewMemoryStream()
	parentCtx := agocontext.New(parentStream)
	child := New("worker", "", WithConfig(agotest.NewTestConfig(
		textResp("first answer"),
		textResp("second answer"),
	)))
	other := New("other", "")

	factory := PersistentStream()
	first := factory(child, parentCtx)
	second := factory(child, parentCtx)
	if first.ID() != second.ID() {
		t.Fatalf("stream ids differ: %q vs %q", first.ID(), second.ID())
	}
	if factory(other, parentCtx).ID() == first.ID() {
		t.Fatal("two agents share one persistent stream")
	}

	res1, err := runTaskWith(stdcontext.Background(), parentCtx, child, "first", "", PersistentStream(), "")
	if err != nil {
		t.Fatalf("first delegation: %v", err)
	}
	res2, err := runTaskWith(stdcontext.Background(), parentCtx, child, "second", "", PersistentStream(), "")
	if err != nil {
		t.Fatalf("second delegation: %v", err)
	}
	if res1.childStream.ID() != res2.childStream.ID() {
		t.Fatalf("delegation streams differ: %q vs %q", res1.childStream.ID(), res2.childStream.ID())
	}
	// The child's second model call saw the first delegation's exchange: its
	// history carries the first call's response.
	calls := child.opts.cfg.(*agotest.TestConfig).Calls()
	if len(calls) < 2 {
		t.Fatalf("child model calls = %d, want 2", len(calls))
	}
	firstCall, secondCall := calls[0], calls[len(calls)-1]
	var sawFirst bool
	for _, ev := range secondCall {
		if _, ok := events.AsValue(ev).(events.ModelResponse); ok {
			sawFirst = true
		}
	}
	var firstCallHadResponse bool
	for _, ev := range firstCall {
		if _, ok := events.AsValue(ev).(events.ModelResponse); ok {
			firstCallHadResponse = true
		}
	}
	if firstCallHadResponse {
		t.Fatal("first delegation already saw a response; the fixture is wrong")
	}
	if !sawFirst {
		t.Fatal("second delegation did not read back the first delegation's history")
	}
}
