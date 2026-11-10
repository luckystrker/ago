package compact

import (
	"strings"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/usage"
)

// TestTailWindowCompactKeepsLastN pins the pure strategy: it retains the last
// Target conversational events and drops the rest, with no LLM call.
func TestTailWindowCompactKeepsLastN(t *testing.T) {
	evs := []events.Event{
		userReq("one"),
		userReq("two"),
		userReq("three"),
		userReq("four"),
	}
	got := NewTailWindowCompact(2).Compact(evs)
	if len(got) != 2 {
		t.Fatalf("kept %d events, want 2", len(got))
	}
	if got[0] != evs[2] || got[1] != evs[3] {
		t.Fatalf("kept the wrong suffix")
	}
}

// TestTailWindowCompactNoOp covers the boundaries: a window that already covers
// the history (or a non-positive target) leaves it untouched.
func TestTailWindowCompactNoOp(t *testing.T) {
	evs := []events.Event{userReq("one"), userReq("two")}
	for _, target := range []int{2, 3, 0, -1} {
		got := NewTailWindowCompact(target).Compact(evs)
		if len(got) != len(evs) {
			t.Fatalf("target %d: kept %d events, want the history unchanged", target, len(got))
		}
	}

	// The cut lands exactly on index 0 (the window covers every conversational
	// event): the history is returned as-is, not as a re-slice that could drift.
	exact := []events.Event{userReq("one"), userReq("two"), userReq("three")}
	got := NewTailWindowCompact(3).Compact(exact)
	if len(got) != len(exact) {
		t.Fatalf("exact window kept %d events, want %d", len(got), len(exact))
	}
	for i := range exact {
		if got[i] != exact[i] {
			t.Fatalf("exact window changed event %d", i)
		}
	}
}

// TestTailWindowCompactKeepsAllWhenTailIsUnreplayable pins the retained-range
// guard: when aligning the cut to a replayable boundary would reach the end of
// the history, the window keeps everything instead of returning an empty slice.
func TestTailWindowCompactKeepsAllWhenTailIsUnreplayable(t *testing.T) {
	call := events.ToolCallEvent{ID: "c1", Name: "t"}
	evs := []events.Event{
		userReq("u"),
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{call}}},
		&events.ToolResultsEvent{Results: []events.ToolResultEvent{{ParentID: "c1"}}},
	}
	// target 1 lands on the result, whose call would be dropped: the cut advances
	// to the end and there is nothing left to keep.
	got := NewTailWindowCompact(1).Compact(evs)
	if len(got) != len(evs) {
		t.Fatalf("kept %d events, want the history unchanged (%d)", len(got), len(evs))
	}
	for i := range evs {
		if got[i] != evs[i] {
			t.Fatalf("the no-op path changed event %d", i)
		}
	}
}

// TestTailWindowMiddlewareRewritesHistory pins the middleware path: the old
// events are dropped, no CompactionSummary is inserted, and the lifecycle events
// name the strategy with zero LLM calls.
func TestTailWindowMiddlewareRewritesHistory(t *testing.T) {
	ctx, log := newTestCtx(t)
	for _, text := range []string{"one", "two", "three", "four"} {
		send(t, ctx, userReq(text))
	}
	send(t, ctx, events.NewUsageEvent(events.Usage{PromptTokens: events.I64(7)}, events.UsageKindModelCall))

	m := NewTailWindow(EventCountTrigger(3), 2)()
	_, err := m.OnTurn(func(*events.ModelRequest, *agocontext.Context) (*events.ModelResponse, error) {
		return &events.ModelResponse{}, nil
	}, &events.ModelRequest{}, ctx)
	if err != nil {
		t.Fatalf("OnTurn: %v", err)
	}

	evs, err := ctx.Stream.History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	for _, ev := range evs {
		if _, ok := ev.(*events.CompactionSummary); ok {
			t.Fatal("TailWindowCompact must not insert a summary event")
		}
	}
	// The two most recent requests survive; the earlier ones are gone. Source
	// usage records are preserved across the rewrite so reporting stays whole.
	var texts []string
	usageKept := false
	for _, ev := range evs {
		if req, ok := ev.(*events.ModelRequest); ok {
			texts = append(texts, events.RenderForPrompt(req))
		}
		if u, ok := usage.EventFrom(ev); ok && u.Usage.PromptTokens != nil && *u.Usage.PromptTokens == 7 {
			usageKept = true
		}
	}
	if len(texts) != 2 || !strings.Contains(texts[0], "three") || !strings.Contains(texts[1], "four") {
		t.Fatalf("retained %v, want the last two requests", texts)
	}
	if !usageKept {
		t.Fatal("a source usage record before the cut must survive the rewrite")
	}

	want := []string{"CompactionStarted", "CompactionCompleted"}
	if got := (*log)[len(*log)-2:]; !equalStrs(got, want) {
		t.Fatalf("lifecycle events = %v, want %v", *log, want)
	}
}

// TestTailWindowSkipsBelowTrigger pins that the trigger still gates the window.
func TestTailWindowSkipsBelowTrigger(t *testing.T) {
	ctx, log := newTestCtx(t)
	for _, text := range []string{"one", "two"} {
		send(t, ctx, userReq(text))
	}
	before := len(*log)

	m := NewTailWindow(EventCountTrigger(10), 1)()
	if _, err := m.OnTurn(func(*events.ModelRequest, *agocontext.Context) (*events.ModelResponse, error) {
		return &events.ModelResponse{}, nil
	}, &events.ModelRequest{}, ctx); err != nil {
		t.Fatalf("OnTurn: %v", err)
	}
	if len(*log) != before {
		t.Fatalf("below the trigger history must be untouched, log grew: %v", (*log)[before:])
	}
}
