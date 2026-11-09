package agent

import (
	stdcontext "context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
	agotest "github.com/luckystrker/ago/testing"
)

// TestTwoAgentsOneSharedStreamDoNotCrossForward pins the process-wide turn lock:
// two distinct child agents reusing one persistent stream serialize there, so
// their relays cannot capture each other's events.
func TestTwoAgentsOneSharedStreamDoNotCrossForward(t *testing.T) {
	shared := stream.NewMemoryStream()
	alpha := New("alpha", "", WithConfig(sharedSlowConfig{name: "alpha"}), WithStreamBase(shared))
	beta := New("beta", "", WithConfig(sharedSlowConfig{name: "beta"}), WithStreamBase(shared))
	base := stream.NewMemoryStream()
	root := New("root", "", WithConfig(agotest.NewTestConfig(
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "a", Name: "task_alpha", Arguments: `{"objective":"one"}`},
			{ID: "b", Name: "task_beta", Arguments: `{"objective":"two"}`},
		}}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: "done"}},
	)), WithStreamBase(base), WithTools(
		alpha.AsTool(WithToolName("task_alpha")),
		beta.AsTool(WithToolName("task_beta")),
	))
	if _, err := root.Ask(stdcontext.Background(), "go"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	evs, err := base.History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var started, completed int
	usageIDs := map[string]int{}
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.TaskStarted:
			started++
			if len(e.Path) != 1 || e.Path[0] != e.TaskID {
				t.Errorf("started %s/%s path=%v", e.AgentName, e.TaskID, e.Path)
			}
		case events.TaskCompleted:
			completed++
			if len(e.Path) != 1 || e.Path[0] != e.TaskID {
				t.Errorf("completed %s/%s path=%v", e.AgentName, e.TaskID, e.Path)
			}
		case events.UsageEvent:
			if e.AgentName != "" {
				usageIDs[e.EventID]++
			}
		}
	}
	if started != 2 || completed != 2 {
		t.Fatalf("lifecycle=%d/%d, want 2/2", started, completed)
	}
	for id, n := range usageIDs {
		if n != 1 {
			t.Fatalf("usage %s seen %d times", id, n)
		}
	}
}

// TestLateTurnEventsDoNotLeakIntoTheNextDelegation pins Close waiting for the
// turn goroutine: a cancelled turn that ignores ctx must publish its late event
// under its own relay (or not at all), never under the next invocation's.
func TestLateTurnEventsDoNotLeakIntoTheNextDelegation(t *testing.T) {
	var n int64
	childStream := stream.NewMemoryStream()
	child := New("child", "", WithConfig(slowLateConfig{n: &n}), WithStreamBase(childStream))
	parent := agocontext.New(stream.NewMemoryStream())
	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if r1, err := runTaskOn(ctx, parent, child, "first", ""); err != nil || !r1.cancelled {
		t.Fatalf("first delegation should be cancelled: %+v %v", r1, err)
	}
	// The first Close must have awaited the late model call before the second
	// delegation subscribed to the same stream.
	parent2 := agocontext.New(stream.NewMemoryStream())
	res, err := runTaskOn(stdcontext.Background(), parent2, child, "second", "")
	if err != nil || !res.completed {
		t.Fatalf("second: %+v %v", res, err)
	}
	evs, _ := parent2.Stream.History().GetEvents()
	for _, ev := range evs {
		ce, ok := events.AsValue(ev).(events.ChildEvent)
		if !ok {
			continue
		}
		if mr, ok := events.AsValue(ce.Inner).(events.ModelResponse); ok && mr.Content() == "late-1" {
			t.Fatalf("late event of the cancelled turn forwarded as %s's event", ce.TaskID)
		}
	}
}

// TestRejectBeforeTurnScopePublishesStartedOnce pins upstream reject: a
// delegation that fails before its turn scope opens (here a scoped-variable
// conflict in newRunWith) still publishes TaskStarted once, then TaskFailed,
// without duplicating the opening event.
func TestRejectBeforeTurnScopePublishesStartedOnce(t *testing.T) {
	parent := agocontext.New(stream.NewMemoryStream())
	child := New("child", "", WithConfig(agotest.NewTestConfig(textResp("never"))),
		WithVariables(map[string]any{"shared-new": agocontext.ScopedValue{Value: 1, Scope: agocontext.ScopeShared}}))
	res, err := runTaskOn(stdcontext.Background(), parent, child, "work", "")
	if err != nil {
		t.Fatalf("reject returned a turn-ending error: %v", err)
	}
	if res.completed || res.err == nil {
		t.Fatalf("result = %+v, want a failed delegation", res)
	}
	evs, _ := parent.Stream.History().GetEvents()
	var started, failed int
	for _, ev := range evs {
		switch events.AsValue(ev).(type) {
		case events.TaskStarted:
			started++
		case events.TaskFailed:
			failed++
		}
	}
	if started != 1 || failed != 1 {
		t.Fatalf("reject lifecycle = started %d failed %d, want 1/1", started, failed)
	}
}

// TestForwarderRetainsAccountingBesideForeignUsage pins the accounting dedup
// predicate: an unrelated UsageEvent already in the parent history must not
// make the failed delivery of a different source record disappear.
func TestForwarderRetainsAccountingBesideForeignUsage(t *testing.T) {
	parent := agocontext.New(stream.NewMemoryStream())
	foreign := events.NewUsageEvent(events.Usage{TotalTokens: events.I64(5)}, events.UsageKindModelCall)
	if err := parent.Stream.History().Append(foreign); err != nil {
		t.Fatalf("seed foreign usage: %v", err)
	}
	parent.Stream.Subscribe(func(ev events.Event) error {
		if events.IsInert(ev) {
			return errors.New("parent unavailable")
		}
		return nil
	}, stream.SubscribeOptions{Interrupt: true})
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
	ids := map[string]bool{}
	for _, ev := range evs {
		if u, ok := events.AsValue(ev).(events.UsageEvent); ok {
			ids[u.EventID] = true
		}
	}
	if len(ids) != 2 || !ids[foreign.EventID] || !ids[src.EventID] {
		t.Fatalf("retained usage ids = %v, want both foreign and failed record", ids)
	}
}

// TestForwarderRejectOnSameStreamPersistsLocally pins the reject fast path: a
// pre-turn terminal for a delegation configured with the caller's stream is
// persisted there (the relay has no echo guard for a rejected attempt).
func TestForwarderRejectOnSameStreamPersistsLocally(t *testing.T) {
	s := stream.NewMemoryStream()
	f := newForwarder(agocontext.New(s), s, "t", "a")
	if err := f.reject(&events.ModelResponse{Message: &events.ModelMessage{Content: "once"}}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	evs, _ := s.History().GetEvents()
	if len(evs) != 1 {
		t.Fatalf("same-stream history = %d events, want 1", len(evs))
	}
}

// TestForwarderUsageIncludesRetainedAccounting pins TaskCompleted.Usage parity:
// spend retained only on the parent side after a delivery failure still counts
// in the delegation's cumulative snapshot.
func TestForwarderUsageIncludesRetainedAccounting(t *testing.T) {
	parent := agocontext.New(stream.NewMemoryStream())
	parent.Stream.Subscribe(func(ev events.Event) error {
		if events.IsInert(ev) {
			return errors.New("parent unavailable")
		}
		return nil
	}, stream.SubscribeOptions{Interrupt: true})
	child := stream.NewMemoryStream()
	f := newForwarder(parent, child, "t", "child")
	f.start()
	defer f.stop()
	src := events.NewUsageEvent(events.Usage{TotalTokens: events.I64(10)}, events.UsageKindModelCall)
	if err := child.Send(src); err == nil {
		t.Fatal("child send succeeded")
	}
	if got := snapshotUsage(child, f.accountingEvents()).TotalTokens; got == nil || *got != 10 {
		t.Fatalf("forwarder usage = %v, want 10 from retained accounting", got)
	}
}

type slowLateConfig struct{ n *int64 }

func (c slowLateConfig) Provider() config.Provider { return "test" }
func (c slowLateConfig) Model() string             { return "late" }
func (c slowLateConfig) Create() (config.LLMClient, error) {
	return slowLateClient{n: c.n}, nil
}
func (c slowLateConfig) Clone(map[string]any) (config.ModelConfig, error) { return c, nil }

type slowLateClient struct{ n *int64 }

func (c slowLateClient) Call(stdcontext.Context, []events.Event, *agocontext.Context, config.CallOptions) (*events.ModelResponse, error) {
	time.Sleep(200 * time.Millisecond)
	n := atomic.AddInt64(c.n, 1)
	return &events.ModelResponse{
		Message: &events.ModelMessage{Content: "late-" + strconv.FormatInt(n, 10)},
		Usage:   events.Usage{TotalTokens: events.I64(1)},
	}, nil
}

// TestForwarderNilGuards pins the defensive paths used by rejected/partial
// delegations: a forwarder without a parent or stream must not panic while
// rejecting or retaining accounting.
func TestForwarderNilGuards(t *testing.T) {
	var f forwarder
	if err := f.reject(&events.ModelResponse{Message: &events.ModelMessage{Content: "once"}}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	f.persistAccounting(events.NewUsageEvent(events.Usage{TotalTokens: events.I64(1)}, events.UsageKindModelCall), "id")
	f.persistAccounting(nil, "")
}
