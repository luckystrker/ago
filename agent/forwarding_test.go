package agent

import (
	stdcontext "context"
	"sync"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
	"github.com/luckystrker/ago/usage"
)

// journal collects what the root stream observer sees.
type journal struct {
	mu  sync.Mutex
	evs []events.Event
}

func (j *journal) record(ev events.Event) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.evs = append(j.evs, ev)
}

func (j *journal) snapshot() []events.Event {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]events.Event, len(j.evs))
	copy(out, j.evs)
	return out
}

// TestRootObservesOriginalLeafAndCountsEachCallOnce pins the three-level event
// tree: a leaf's operational event reaches the root as a ChildEvent whose path
// names both hops; transient observations never enter the transcript; source
// usage arrives once per call with its spender, so the report totals 60.
func TestRootObservesOriginalLeafAndCountsEachCallOnce(t *testing.T) {
	emit, err := tools.NewFunctionTool("emit_chunk", "emit a transient observation",
		func(_ struct{}, cx *agocontext.Context) (string, error) {
			if err := cx.Send(&events.ModelMessageChunk{Content: "working"}); err != nil {
				return "", err
			}
			return "emitted", nil
		})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}

	toolCallResp := func(name, args string) *events.ModelResponse {
		return &events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{{
			ID: "call-" + name, Name: name, Arguments: args,
		}}}}
	}
	leaf := New("leaf", "", WithConfig(agotest.NewTestConfig(
		toolCallResp("emit_chunk", `{}`),
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: "leaf done"},
			Usage:   events.Usage{TotalTokens: events.I64(30)},
			Model:   "leaf-model",
		},
	)), WithTools(emit))
	worker := New("worker", "", WithConfig(agotest.NewTestConfig(
		toolCallResp("task_leaf", `{"objective":"dig"}`),
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: "worker done"},
			Usage:   events.Usage{TotalTokens: events.I64(20)},
			Model:   "worker-model",
		},
	)), WithTools(leaf.AsTool(WithToolDescription("Dig"))))
	base := stream.NewMemoryStream()
	root := New("root", "", WithConfig(agotest.NewTestConfig(
		toolCallResp("task_worker", `{"objective":"organize"}`),
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: "root done"},
			Usage:   events.Usage{TotalTokens: events.I64(10)},
			Model:   "root-model",
		},
	)), WithStreamBase(base), WithTools(worker.AsTool(WithToolDescription("Organize"))))

	j := &journal{}
	base.Subscribe(func(ev events.Event) error { j.record(ev); return nil }, stream.SubscribeOptions{})

	reply, err := root.Ask(stdcontext.Background(), "start")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if reply.Body() != "root done" {
		t.Fatalf("body = %q", reply.Body())
	}

	var chunks []events.ChildEvent
	for _, ev := range j.snapshot() {
		if ce, ok := childEnvelope(ev); ok {
			if _, isChunk := events.AsValue(ce.Inner).(events.ModelMessageChunk); isChunk {
				chunks = append(chunks, ce)
			}
		}
	}
	if len(chunks) != 1 {
		t.Fatalf("leaf chunk observations: %d, want 1", len(chunks))
	}
	if chunks[0].AgentName != "leaf" || len(chunks[0].Path) != 2 {
		t.Fatalf("observation = agent %q path %v, want leaf through two hops", chunks[0].AgentName, chunks[0].Path)
	}

	history, err := reply.History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	for _, ev := range history {
		if ce, ok := childEnvelope(ev); ok {
			if _, isChunk := events.AsValue(ce.Inner).(events.ModelMessageChunk); isChunk {
				t.Fatal("transient child observation was persisted")
			}
		}
	}

	spent := map[string]events.UsageEvent{}
	for _, ev := range history {
		if u, ok := events.AsValue(ev).(events.UsageEvent); ok && u.AgentName != "" {
			if _, dup := spent[u.AgentName]; dup {
				t.Fatalf("duplicate source usage for %q", u.AgentName)
			}
			spent[u.AgentName] = u
		}
	}
	if len(spent) != 2 {
		t.Fatalf("forwarded spenders = %v, want worker and leaf", spent)
	}
	if got := spent["worker"]; len(got.Path) != 1 || got.Path[0] != got.TaskID {
		t.Fatalf("worker path = %v, task = %q", got.Path, got.TaskID)
	}
	if got := spent["leaf"]; len(got.Path) != 2 {
		t.Fatalf("leaf path = %v, want two hops", got.Path)
	}
	report := usage.FromEvents(history)
	if report.Total.TotalTokens == nil || *report.Total.TotalTokens != 60 {
		t.Fatalf("total tokens = %v, want 60 (each call once)", report.Total.TotalTokens)
	}
	if got := report.ByKind[events.UsageKindSubtask]; usage.Reported(got) {
		t.Fatalf("subtask rollup present in report: %+v", got)
	}
}

// TestForwardedOperationsDoNotTriggerRootTools pins the inert boundary: a
// descendant's ToolCallEvent arrives as an observation only, so a root tool of
// the same name does not execute a second time.
func TestForwardedOperationsDoNotTriggerRootTools(t *testing.T) {
	var mu sync.Mutex
	rootCalls, leafCalls := 0, 0
	rootProbe, err := tools.NewFunctionTool("probe", "root probe",
		func(_ struct{}, _ *agocontext.Context) (string, error) {
			mu.Lock()
			rootCalls++
			mu.Unlock()
			return "probed", nil
		})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	leafProbe, err := tools.NewFunctionTool("probe", "leaf probe",
		func(_ struct{}, _ *agocontext.Context) (string, error) {
			mu.Lock()
			leafCalls++
			mu.Unlock()
			return "probed", nil
		})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}

	leaf := New("leaf", "", WithConfig(agotest.NewTestConfig(
		toolResp("probe", `{}`),
		textResp("leaf done"),
	)), WithTools(leafProbe))
	root := New("root", "", WithConfig(agotest.NewTestConfig(
		toolResp("task_leaf", `{"objective":"dig"}`),
		textResp("root done"),
	)), WithTools(leaf.AsTool(WithToolDescription("Dig")), rootProbe))

	if _, err := root.Ask(stdcontext.Background(), "start"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if leafCalls != 1 {
		t.Fatalf("leaf probe calls = %d, want 1", leafCalls)
	}
	if rootCalls != 0 {
		t.Fatalf("root probe calls = %d, want 0: forwarded ToolCallEvent triggered the root", rootCalls)
	}
}

// childEnvelope normalizes the two observation spellings, mirroring the
// Python base/transient subclass relation.
func childEnvelope(ev events.Event) (events.ChildEvent, bool) {
	switch e := events.AsValue(ev).(type) {
	case events.ChildEvent:
		return e, true
	case events.TransientChildEvent:
		return e.ChildEvent, true
	}
	return events.ChildEvent{}, false
}
