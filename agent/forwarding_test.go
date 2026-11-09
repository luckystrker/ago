package agent

import (
	stdcontext "context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luckystrker/ago/config"
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

func TestNewForwarderExtendsAncestorPathAndIgnoresForeignDependency(t *testing.T) {
	parent := agocontext.New(stream.NewMemoryStream())
	// A dependency under the forwarding key that is not a Forwarder must be
	// ignored, not dereferenced.
	parent.SetDependency(forwarderDependencyKey, "not-a-forwarder")
	f := newForwarder(parent, stream.NewMemoryStream(), "task-b", "b")
	if len(f.path) != 1 || f.path[0] != "task-b" {
		t.Fatalf("local path = %v, want just the task id", f.path)
	}

	ancestor := &forwarder{path: []string{"root", "a"}}
	parent.SetDependency(forwarderDependencyKey, ancestor)
	nested := newForwarder(parent, stream.NewMemoryStream(), "task-b", "b")
	if len(nested.path) != 3 || nested.path[0] != "root" || nested.path[1] != "a" || nested.path[2] != "task-b" {
		t.Fatalf("nested path = %v, want root/a/task-b", nested.path)
	}
	nested.path[0] = "mutated"
	if ancestor.path[0] != "root" {
		t.Fatal("newForwarder aliased the ancestor path slice")
	}
}

func TestForwarderNilAndEmptyStateIsSafe(t *testing.T) {
	var nilF *forwarder
	nilF.start()
	nilF.stop()
	if err := nilF.forward(nil); err != nil {
		t.Fatalf("nil forward: %v", err)
	}

	(&forwarder{}).start()
	(&forwarder{}).stop()
	if err := (&forwarder{}).forward(&events.ModelResponse{}); err != nil {
		t.Fatalf("nil parent forward: %v", err)
	}

	parent := agocontext.New(stream.NewMemoryStream())
	if err := (&forwarder{parent: &agocontext.Context{}}).forward(&events.ModelResponse{}); err != nil {
		t.Fatalf("nil parent stream forward: %v", err)
	}
	if err := (&forwarder{parent: parent}).forward(nil); err != nil {
		t.Fatalf("nil event forward: %v", err)
	}

	// A nil child stream is not the parent stream, so the event is relayed; the
	// relay must not panic on the missing child handle.
	f := &forwarder{parent: parent, taskID: "t", agentName: "a", path: []string{"t"}}
	if err := f.forward(&events.ModelResponse{Message: &events.ModelMessage{Content: "x"}}); err != nil {
		t.Fatalf("nil child stream forward: %v", err)
	}
	evs, err := parent.Stream.History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("forwarded events = %d, want 1", len(evs))
	}
	if _, ok := events.AsValue(evs[0]).(events.ChildEvent); !ok {
		t.Fatalf("forwarded event = %T, want ChildEvent", evs[0])
	}

	(&forwarder{parent: parent}).start()
	(&forwarder{parent: parent}).stop()
	(&forwarder{stream: stream.NewMemoryStream()}).start()
}

func TestForwarderDoesNotEchoOnTheSameStream(t *testing.T) {
	s := stream.NewMemoryStream()
	parent := agocontext.New(s)
	f := newForwarder(parent, s, "t", "a")
	f.start()
	defer f.stop()
	if f.sub != 0 {
		t.Fatal("forwarder subscribed to its own parent stream")
	}
	if err := s.Send(&events.ModelResponse{Message: &events.ModelMessage{Content: "once"}}); err != nil {
		t.Fatalf("send: %v", err)
	}
	evs, err := s.History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("history = %d events, want 1 (no echoed ChildEvent)", len(evs))
	}
}

func TestForwardRetransmitsEnvelopeAndStampsFreshOrigin(t *testing.T) {
	parent := agocontext.New(stream.NewMemoryStream())
	child := stream.NewMemoryStream()
	f := newForwarder(parent, child, "worker", "worker")
	f.start()
	defer f.stop()

	envelope := events.NewChildEvent(&events.ModelResponse{Message: &events.ModelMessage{Content: "leaf"}},
		"leaf", []string{"worker", "leaf"}, "leaf", "leaf-source")
	if err := child.Send(envelope); err != nil {
		t.Fatalf("send envelope: %v", err)
	}
	if err := child.Send(&events.ModelResponse{Message: &events.ModelMessage{Content: "worker"}}); err != nil {
		t.Fatalf("send response: %v", err)
	}

	evs, err := parent.Stream.History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("forwarded events = %d, want 2", len(evs))
	}
	relayed, ok := events.AsValue(evs[0]).(events.ChildEvent)
	if !ok {
		t.Fatalf("first forwarded event = %T, want ChildEvent", evs[0])
	}
	if relayed.TaskID != "leaf" || relayed.AgentName != "leaf" || relayed.OriginEventID != "leaf-source" ||
		len(relayed.Path) != 2 || relayed.Path[1] != "leaf" {
		t.Fatalf("relayed envelope lost provenance: %+v", relayed)
	}
	if _, nested := events.AsValue(relayed.Inner).(events.ChildEvent); nested {
		t.Fatal("a deeper observation was wrapped a second time")
	}

	wrapped, ok := events.AsValue(evs[1]).(events.ChildEvent)
	if !ok {
		t.Fatalf("second forwarded event = %T, want ChildEvent", evs[1])
	}
	if wrapped.TaskID != "worker" || len(wrapped.Path) != 1 || wrapped.Path[0] != "worker" {
		t.Fatalf("wrapped envelope = %+v", wrapped)
	}
	if wrapped.OriginEventID == "" {
		t.Fatal("a locally authored forwarded event must get a fresh origin id")
	}
}

func spendResp(content string, tokens int64) *events.ModelResponse {
	return &events.ModelResponse{
		Message: &events.ModelMessage{Content: content},
		Usage:   events.Usage{TotalTokens: events.I64(tokens)},
		Model:   content + "-model",
	}
}

// delegationChain builds a root that delegates depth levels down to a leaf, each
// level spending twice as much as the one below it.
func delegationChain(depth int) *Agent {
	child := New("leaf", "", WithConfig(agotest.NewTestConfig(spendResp("leaf done", 1))))
	for level := 1; level <= depth; level++ {
		name := fmt.Sprintf("level%d", level)
		cfg := agotest.NewTestConfig(
			toolResp("task_"+child.Name(), `{"objective":"go"}`),
			spendResp(name+" done", int64(1)<<level),
		)
		child = New(name, "", WithConfig(cfg), WithTools(child.AsTool(WithToolDescription("delegate"))))
	}
	return child
}

// TestDelegationUsageMatrix pins the accounting contract at every nesting depth
// a delegation tree can reach: each leaf call reaches the root once, with a path
// naming every hop, and TaskCompleted carries a cumulative snapshot that a report
// must not add in.
func TestDelegationUsageMatrix(t *testing.T) {
	tests := []struct {
		depth         int
		wantTotal     int64
		wantSnapshots map[string]int64
	}{
		// Each delegating agent also spends one token on its tool-call round.
		{depth: 1, wantTotal: 4, wantSnapshots: map[string]int64{"level1": 4}},
		{depth: 2, wantTotal: 9, wantSnapshots: map[string]int64{"level1": 4, "level2": 9}},
		{depth: 3, wantTotal: 18, wantSnapshots: map[string]int64{"level1": 4, "level2": 9, "level3": 18}},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("depth-%d", tt.depth), func(t *testing.T) {
			root := delegationChain(tt.depth)
			reply, err := root.Ask(stdcontext.Background(), "start")
			if err != nil {
				t.Fatalf("ask: %v", err)
			}
			evs, err := reply.History().GetEvents()
			if err != nil {
				t.Fatalf("history: %v", err)
			}
			report := usage.FromEvents(evs)
			if report.Total.TotalTokens == nil || *report.Total.TotalTokens != tt.wantTotal {
				t.Fatalf("total = %d, want %d counted once", derefTokens(report.Total.TotalTokens), tt.wantTotal)
			}
			if got := report.ByKind[events.UsageKindSubtask]; usage.Reported(got) {
				t.Fatalf("legacy rollup in report: %+v", got)
			}
			// Every delegating agent contributes its tool-call and final rounds;
			// the leaf contributes only its final round.
			if len(report.Records) != tt.depth*2+1 {
				t.Fatalf("source records = %d, want %d", len(report.Records), tt.depth*2+1)
			}
			byAgent := map[string]int{}
			for _, ev := range evs {
				ue, ok := events.AsValue(ev).(events.UsageEvent)
				if !ok {
					continue
				}
				byAgent[ue.AgentName]++
				if ue.AgentName == "" {
					continue
				}
				wantPath := tt.depth
				if ue.AgentName != "leaf" {
					level, err := strconv.Atoi(strings.TrimPrefix(ue.AgentName, "level"))
					if err != nil {
						t.Fatalf("unexpected delegated agent %q", ue.AgentName)
					}
					wantPath = tt.depth - level
				}
				if len(ue.Path) != wantPath {
					t.Fatalf("%s path = %v, want %d hops", ue.AgentName, ue.Path, wantPath)
				}
				if ue.TaskID == "" || ue.Path[len(ue.Path)-1] != ue.TaskID {
					t.Fatalf("%s task/path mismatch: task=%q path=%v", ue.AgentName, ue.TaskID, ue.Path)
				}
			}
			if byAgent[""] != 2 {
				t.Fatalf("root source records = %d, want 2", byAgent[""])
			}
			// The top level is the root; levels below it are delegated agents.
			for name := 1; name < tt.depth; name++ {
				agent := fmt.Sprintf("level%d", name)
				if byAgent[agent] != 2 {
					t.Fatalf("%s source records = %d, want 2", agent, byAgent[agent])
				}
			}
			if byAgent["leaf"] != 1 {
				t.Fatalf("leaf source records = %d, want 1", byAgent["leaf"])
			}
			wantSnapshots := tt.wantSnapshots
			for _, ev := range evs {
				tc, ok := events.AsValue(ev).(events.TaskCompleted)
				if !ok {
					continue
				}
				want, tracked := wantSnapshots[tc.AgentName]
				if !tracked {
					continue
				}
				if tc.Usage.TotalTokens == nil || *tc.Usage.TotalTokens != want {
					t.Fatalf("%s TaskCompleted.Usage = %v, want cumulative %d", tc.AgentName, tc.Usage.TotalTokens, want)
				}
			}
		})
	}
}

// TestParallelSiblingUsageCountedOnce pins concurrent siblings: two delegations
// spending in parallel add to the root once each, with distinct path roots.
func TestParallelSiblingUsageCountedOnce(t *testing.T) {
	left := New("left", "", WithConfig(agotest.NewTestConfig(spendResp("left done", 5))))
	right := New("right", "", WithConfig(agotest.NewTestConfig(spendResp("right done", 7))))
	root := New("root", "", WithConfig(agotest.NewTestConfig(
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "call-left", Name: "task_left", Arguments: `{"objective":"l"}`},
			{ID: "call-right", Name: "task_right", Arguments: `{"objective":"r"}`},
		}}},
		spendResp("root done", 11),
	)), WithTools(
		left.AsTool(WithToolName("task_left")),
		right.AsTool(WithToolName("task_right")),
	))
	reply, err := root.Ask(stdcontext.Background(), "start")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	evs, err := reply.History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	report := usage.FromEvents(evs)
	if report.Total.TotalTokens == nil || *report.Total.TotalTokens != 23 {
		t.Fatalf("total = %d, want 23 (11+5+7 once)", derefTokens(report.Total.TotalTokens))
	}
	if len(report.Records) != 3 {
		t.Fatalf("records = %d, want the root and both siblings", len(report.Records))
	}
	paths := map[string]string{}
	for _, ev := range evs {
		ue, ok := events.AsValue(ev).(events.UsageEvent)
		if !ok || ue.AgentName == "" {
			continue
		}
		if len(ue.Path) != 1 {
			t.Fatalf("%s path = %v, want one hop", ue.AgentName, ue.Path)
		}
		if _, dup := paths[ue.AgentName]; dup {
			t.Fatalf("%s counted twice", ue.AgentName)
		}
		paths[ue.AgentName] = ue.Path[0]
	}
	if paths["left"] == "" || paths["right"] == "" || paths["left"] == paths["right"] {
		t.Fatalf("sibling paths = %v, want distinct task ids", paths)
	}
}

// TestForwardedChildHaltDoesNotHaltRoot pins the inert boundary for control
// events: a descendant's HaltEvent is an observation, so the root turn keeps
// going instead of stopping with the child's reason.
func TestForwardedChildHaltDoesNotHaltRoot(t *testing.T) {
	haltNow, err := tools.NewFunctionTool("halt_now", "halt this agent",
		func(_ struct{}, cx *agocontext.Context) (string, error) {
			if err := cx.Send(&events.HaltEvent{Reason: "child stop"}); err != nil {
				return "", err
			}
			return "halted", nil
		})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	child := New("child", "", WithConfig(agotest.NewTestConfig(
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{{ID: "c1", Name: "halt_now", Arguments: `{}`}}}},
		textResp("child continues"),
	)), WithTools(haltNow))
	base := stream.NewMemoryStream()
	root := New("root", "", WithConfig(agotest.NewTestConfig(
		toolResp("task_child", `{"objective":"h"}`),
		textResp("root done"),
	)), WithStreamBase(base), WithTools(child.AsTool(WithToolDescription("delegate"))))

	j := &journal{}
	base.Subscribe(func(ev events.Event) error { j.record(ev); return nil }, stream.SubscribeOptions{})
	reply, err := root.Ask(stdcontext.Background(), "start")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if reply.Body() != "root done" {
		t.Fatalf("root body = %q, want root done (child halt leaked into the root)", reply.Body())
	}
	haltObservations, localHalts := 0, 0
	for _, ev := range j.snapshot() {
		switch e := events.AsValue(ev).(type) {
		case events.HaltEvent:
			_ = e
			localHalts++
		case events.ChildEvent:
			if _, ok := events.AsValue(e.Inner).(events.HaltEvent); ok {
				haltObservations++
			}
		}
	}
	if localHalts != 0 {
		t.Fatalf("root stream carries %d local HaltEvents", localHalts)
	}
	if haltObservations != 1 {
		t.Fatalf("child halt observations = %d, want 1", haltObservations)
	}
}

func TestForwardedHitlAnswerCompletesWithoutHang(t *testing.T) {
	askHuman, err := tools.NewFunctionTool("ask_human", "ask",
		func(args struct {
			Q string `json:"q"`
		}, cx *agocontext.Context) (string, error) {
			return cx.Input(stdcontext.Background(), args.Q, 2*time.Second)
		})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	base := stream.NewMemoryStream()
	child := New("child", "", WithConfig(agotest.NewTestConfig(
		toolResp("ask_human", `{"q":"ping?"}`),
		textResp("child saw pong"),
	)), WithTools(askHuman))
	root := New("parent", "", WithConfig(agotest.NewTestConfig(
		toolResp("task_child", `{"objective":"ask"}`),
		textResp("root done"),
	)), WithStreamBase(base), WithTools(child.AsTool(WithToolDescription("delegate"))),
		WithHitlHook(func(_ stdcontext.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
			return &events.HumanMessage{Content: "pong"}, nil
		}))

	done := make(chan error, 1)
	go func() {
		_, err := root.Ask(stdcontext.Background(), "start")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ask: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delegated HITL ask hung")
	}
	te := collectTaskEvents(t, base)
	if len(te.completed) != 1 || te.completed[0].Result != "child saw pong" {
		t.Fatalf("child did not receive the forwarded answer: %+v", te.completed)
	}
}

func TestCancelledDelegationReportsTaskCancelled(t *testing.T) {
	parent := agocontext.New(stream.NewMemoryStream())
	child := New("child", "", WithConfig(agotest.NewTestConfig(textResp("never"))))
	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	cancel()
	res, err := runTaskOn(ctx, parent, child, "work", "")
	if err != nil {
		t.Fatalf("runTaskOn cancellation returned a turn-ending error: %v", err)
	}
	if !res.cancelled || !errors.Is(res.err, stdcontext.Canceled) {
		t.Fatalf("result = %+v, want cancelled with context.Canceled", res)
	}
	evs, err := parent.Stream.History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var cancelled, failed, completed int
	var cancelEvent events.TaskCancelled
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.TaskCancelled:
			cancelled++
			cancelEvent = e
		case events.TaskFailed:
			failed++
		case events.TaskCompleted:
			completed++
		}
	}
	if cancelled != 1 || failed != 0 || completed != 0 {
		t.Fatalf("terminal events: cancelled=%d failed=%d completed=%d", cancelled, failed, completed)
	}
	if cancelEvent.Objective != "work" {
		t.Fatalf("TaskCancelled objective = %q, want work", cancelEvent.Objective)
	}
}

func derefTokens(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

type sharedSlowConfig struct{ name string }

func (c sharedSlowConfig) Provider() config.Provider { return "test" }
func (c sharedSlowConfig) Model() string             { return c.name }
func (c sharedSlowConfig) Create() (config.LLMClient, error) {
	return sharedSlowClient{name: c.name}, nil
}
func (c sharedSlowConfig) Clone(map[string]any) (config.ModelConfig, error) { return c, nil }

type sharedSlowClient struct{ name string }

func (c sharedSlowClient) Call(ctx stdcontext.Context, _ []events.Event, _ *agocontext.Context, _ config.CallOptions) (*events.ModelResponse, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(40 * time.Millisecond):
	}
	return &events.ModelResponse{
		Message: &events.ModelMessage{Content: c.name + " done"},
		Usage:   events.Usage{TotalTokens: events.I64(10)},
		Model:   c.name,
	}, nil
}

// TestConcurrentDelegationsOnOneSharedStreamDoNotCrossForward pins the turn-scope
// bracket: two AsTool calls of one child agent that reuse one WithStreamBase
// serialize on the child's per-stream lock, and each relay only sees its own
// invocation. A queued delegation must not capture the active turn's events,
// duplicate its TaskStarted/TaskCompleted or restamp its source usage.
func TestConcurrentDelegationsOnOneSharedStreamDoNotCrossForward(t *testing.T) {
	child := New("child", "", WithConfig(sharedSlowConfig{name: "child"}),
		WithStreamBase(stream.NewMemoryStream()))
	base := stream.NewMemoryStream()
	parent := New("root", "", WithConfig(agotest.NewTestConfig(
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "a", Name: "task_child", Arguments: `{"objective":"one"}`},
			{ID: "b", Name: "task_child", Arguments: `{"objective":"two"}`},
		}}},
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: "root done"},
			Usage:   events.Usage{TotalTokens: events.I64(1)},
		},
	)), WithStreamBase(base), WithTools(child.AsTool(WithToolName("task_child"))))

	if _, err := parent.Ask(stdcontext.Background(), "go"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	history, err := base.History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var started, completed []events.TaskCompleted
	var startEvents []events.TaskStarted
	for _, ev := range history {
		switch e := events.AsValue(ev).(type) {
		case events.TaskStarted:
			startEvents = append(startEvents, e)
			started = append(started, events.TaskCompleted{})
		case events.TaskCompleted:
			completed = append(completed, e)
		}
	}
	if len(startEvents) != 2 || len(completed) != 2 {
		t.Fatalf("lifecycle = started %d completed %d, want 2/2", len(startEvents), len(completed))
	}
	ids := map[string]bool{}
	for _, s := range startEvents {
		ids[s.TaskID] = true
		if len(s.Path) != 1 || s.Path[0] != s.TaskID {
			t.Fatalf("TaskStarted %s path = %v", s.TaskID, s.Path)
		}
	}
	for _, c := range completed {
		if !ids[c.TaskID] || len(c.Path) != 1 || c.Path[0] != c.TaskID {
			t.Fatalf("TaskCompleted %s path = %v (started ids %v)", c.TaskID, c.Path, ids)
		}
	}
	seenUsage := map[string]bool{}
	childSpend := int64(0)
	for _, ev := range history {
		u, ok := events.AsValue(ev).(events.UsageEvent)
		if !ok || u.AgentName != "child" {
			continue
		}
		if seenUsage[u.EventID] {
			t.Fatalf("source usage %s forwarded twice", u.EventID)
		}
		seenUsage[u.EventID] = true
		if len(u.Path) != 1 || u.Path[0] != u.TaskID {
			t.Fatalf("usage path = %v, task = %s", u.Path, u.TaskID)
		}
		if u.Usage.TotalTokens != nil {
			childSpend += *u.Usage.TotalTokens
		}
	}
	if len(seenUsage) != 2 || childSpend != 20 {
		t.Fatalf("child usage = %d records / %d tokens, want 2 / 20", len(seenUsage), childSpend)
	}
	if got := usage.FromEvents(history).Total.TotalTokens; got == nil || *got != 21 {
		t.Fatalf("report total = %v, want 21", got)
	}
}

// TestForwarderRetainsAccountingWhenUpwardDeliveryFails pins the
// _persist_accounting fallback: a failed parent publication must leave the
// source UsageEvent in the parent transcript exactly once, whether the failure
// happened before or after persistence.
func TestForwarderRetainsAccountingWhenUpwardDeliveryFails(t *testing.T) {
	for _, tc := range []struct {
		name      string
		interrupt bool
	}{
		{name: "failure before persistence", interrupt: true},
		{name: "subscriber failure after persistence"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := agocontext.New(stream.NewMemoryStream())
			parent.Stream.Subscribe(func(ev events.Event) error {
				if events.IsInert(ev) {
					return errors.New("parent unavailable")
				}
				return nil
			}, stream.SubscribeOptions{Interrupt: tc.interrupt})
			child := stream.NewMemoryStream()
			f := newForwarder(parent, child, "t", "child")
			f.start()
			defer f.stop()

			src := events.NewUsageEvent(events.Usage{TotalTokens: events.I64(10)}, events.UsageKindModelCall)
			if err := child.Send(src); err == nil {
				t.Fatal("child send succeeded despite the parent failure")
			}
			evs, err := parent.Stream.History().GetEvents()
			if err != nil {
				t.Fatalf("history: %v", err)
			}
			if len(evs) != 1 {
				t.Fatalf("parent accounting records = %d, want 1", len(evs))
			}
			u, ok := events.AsValue(evs[0]).(events.UsageEvent)
			if !ok || u.EventID != src.EventID || len(u.Path) != 1 || u.Path[0] != "t" {
				t.Fatalf("retained record = %#v", evs[0])
			}
		})
	}
}
