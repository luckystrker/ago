package compact

import (
	"errors"
	"strings"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
)

// toolRoundHistory is the shape a tool round leaves in the transcript: the
// request, the response that announces the call, the standalone call event an
// observer gets, and the result answering it. The final response is appended by
// the turn.
func toolRoundHistory() []events.Event {
	call := events.ToolCallEvent{ID: "c1", Name: "write", Arguments: `{"p":"/tmp/x"}`}
	return []events.Event{
		userReq("write it"),
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{call}}},
		&events.ToolCallsEvent{Calls: []events.ToolCallEvent{call}},
		&events.ToolResultsEvent{Results: []events.ToolResultEvent{
			{ParentID: "c1", Name: "write", Result: events.NewToolResult(false, "ok")},
		}},
	}
}

// TestSnapCutMovesPastAnOrphanedToolResult pins the core of _replay.snap: a cut
// that would retain the result without the call it answers advances to the next
// replayable index.
func TestSnapCutMovesPastAnOrphanedToolResult(t *testing.T) {
	final := &events.ModelResponse{Message: &events.ModelMessage{Content: "done"}}
	evs := append(toolRoundHistory(), final)

	// keep=2 lands on the result (the final response is 1, the result is 2).
	cut := cutIndex(evs, 2)
	if cut != 3 {
		t.Fatalf("cutIndex = %d, want 3 (the tool result)", cut)
	}
	if isReplayable(evs, cut) {
		t.Fatal("a span starting at the tool result is not replayable")
	}
	if got := snapCut(evs, cut); got != 4 {
		t.Fatalf("snapCut = %d, want 4 (the final response)", got)
	}
	if !isReplayable(evs, 4) {
		t.Fatal("the final response alone is replayable")
	}

	// A cut of zero stays zero: nothing decided to drop must be turned into a drop.
	if got := snapCut(evs, 0); got != 0 {
		t.Fatalf("snapCut(0) = %d, want 0", got)
	}
	// A cut past the end stays past the end.
	if got := snapCut(evs, len(evs)+3); got != len(evs)+3 {
		t.Fatalf("snapCut past the end = %d", got)
	}
}

// TestSnapCutMovesPastAnUnansweredCall pins the announced side of the invariant:
// a standalone call event retained without the response that announced it maps
// to nothing on the wire, so the cut moves past it.
func TestSnapCutMovesPastAnUnansweredCall(t *testing.T) {
	call := events.ToolCallEvent{ID: "c1", Name: "write"}
	evs := []events.Event{
		userReq("write it"),
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{call}}},
		&events.ToolCallsEvent{Calls: []events.ToolCallEvent{call}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: "done"}},
	}
	// Cut lands on the standalone call: the response announcing it is dropped.
	if isReplayable(evs, 2) {
		t.Fatal("a standalone call without its response is not replayable")
	}
	if got := snapCut(evs, 2); got != 3 {
		t.Fatalf("snapCut = %d, want 3", got)
	}
}

// TestIsReplayableWholeHistory pins the no-op cases: an empty span and a span
// that carries its own calls are both replayable.
func TestIsReplayableWholeHistory(t *testing.T) {
	evs := append(toolRoundHistory(), &events.ModelResponse{})
	if !isReplayable(evs, 0) {
		t.Fatal("the whole history is replayable")
	}
	if !isReplayable(evs, len(evs)) {
		t.Fatal("an empty span is replayable")
	}
	if isReplayable(evs, 3) {
		t.Fatal("the result without its call is not replayable")
	}
}

// TestCompactionNeverSplitsAToolRound pins the finish line for the summarizing
// path: after compaction the history must not carry a tool result whose call was
// summarized away.
func TestCompactionNeverSplitsAToolRound(t *testing.T) {
	ctx, _ := newTestCtx(t)
	for _, ev := range toolRoundHistory() {
		send(t, ctx, ev)
	}

	var summarized []string
	strategy := strategyFunc(func(_ *agocontext.Context, evs []events.Event) (string, error) {
		for _, ev := range evs {
			summarized = append(summarized, events.RenderForPrompt(ev))
		}
		return "summarized", nil
	})
	mw := New(EventCountTrigger(1), strategy)()
	if _, err := mw.OnTurn(func(*events.ModelRequest, *agocontext.Context) (*events.ModelResponse, error) {
		r := &events.ModelResponse{Message: &events.ModelMessage{Content: "done"}}
		send(t, ctx, r)
		return r, nil
	}, userReq("write it"), ctx); err != nil {
		t.Fatalf("OnTurn: %v", err)
	}

	evs, err := ctx.Stream.History().GetEvents()
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	var kinds []string
	for _, ev := range evs {
		kinds = append(kinds, ev.EventType())
		if strings.HasPrefix(ev.EventType(), "ToolResult") {
			t.Fatalf("history retained an orphan tool result: %v", kinds)
		}
	}
	// The whole round moved into the summary; only the final answer is retained.
	want := []string{"CompactionSummary", "ModelResponse"}
	if !equalStrs(kinds, want) {
		t.Fatalf("history kinds = %v, want %v", kinds, want)
	}
	// The summarized span covers the request, the announcing response, the
	// standalone call and the result — the round is whole, not split.
	if len(summarized) != 4 {
		t.Fatalf("summarized %d events, want the whole round (4)", len(summarized))
	}
}

// TestTailWindowNeverSplitsAToolRound pins the same invariant for the zero-cost
// window strategy.
func TestTailWindowNeverSplitsAToolRound(t *testing.T) {
	evs := append(toolRoundHistory(), &events.ModelResponse{Message: &events.ModelMessage{Content: "done"}})

	got := NewTailWindowCompact(2).Compact(evs)
	if len(got) != 1 {
		t.Fatalf("kept %d events, want only the final response", len(got))
	}
	last, ok := events.AsValue(got[0]).(events.ModelResponse)
	if !ok || last.Content() != "done" {
		t.Fatalf("kept %T, want the final answer", got[0])
	}

	// A window wide enough to keep the whole round keeps it, from the response
	// that announces the call onwards: the request is the only event dropped.
	wide := NewTailWindowCompact(4).Compact(evs)
	if len(wide) != 4 {
		t.Fatalf("wide window kept %d events, want the round from its announcing response (4)", len(wide))
	}
	if _, ok := events.AsValue(wide[0]).(events.ModelResponse); !ok {
		t.Fatalf("wide window starts with %T, want the announcing response", wide[0])
	}
}

// TestSummarizeCompactKeepRecentOption pins the tunable window: the number of
// trailing conversational events kept verbatim is an option, not a constant.
func TestSummarizeCompactKeepRecentOption(t *testing.T) {
	run := func(t *testing.T, opts ...Option) (summarized int, after int) {
		t.Helper()
		ctx, _ := newTestCtx(t)
		for _, text := range []string{"u1", "u2", "u3", "u4", "u5", "u6"} {
			send(t, ctx, userReq(text))
		}
		strategy := strategyFunc(func(_ *agocontext.Context, evs []events.Event) (string, error) {
			summarized = len(evs)
			return "s", nil
		})
		mw := New(EventCountTrigger(1), strategy, opts...)()
		var done *events.CompactionCompleted
		ctx.Stream.Subscribe(func(ev events.Event) error {
			if c, ok := events.AsValue(ev).(events.CompactionCompleted); ok {
				done = &c
			}
			return nil
		}, stream.SubscribeOptions{Condition: events.TypeOf[events.CompactionCompleted]()})
		if _, err := mw.OnTurn(func(*events.ModelRequest, *agocontext.Context) (*events.ModelResponse, error) {
			r := &events.ModelResponse{Message: &events.ModelMessage{Content: "done"}}
			send(t, ctx, r)
			return r, nil
		}, userReq("u7"), ctx); err != nil {
			t.Fatalf("OnTurn: %v", err)
		}
		if done == nil {
			t.Fatal("no CompactionCompleted")
		}
		return summarized, done.EventsAfter
	}

	t.Run("default keeps KeepRecent", func(t *testing.T) {
		summarized, after := run(t)
		if summarized != 5 || after != 3 {
			t.Fatalf("summarized %d, kept %d; want 5 and 3", summarized, after)
		}
	})
	t.Run("WithKeepRecent widens the window", func(t *testing.T) {
		summarized, after := run(t, WithKeepRecent(4))
		if summarized != 3 || after != 5 {
			t.Fatalf("summarized %d, kept %d; want 3 and 5", summarized, after)
		}
	})
	t.Run("non-positive keeps the default", func(t *testing.T) {
		summarized, after := run(t, WithKeepRecent(0))
		if summarized != 5 || after != 3 {
			t.Fatalf("summarized %d, kept %d; want the defaults 5 and 3", summarized, after)
		}
	})
}

// TestSnapCutKeepsACutOfZeroUnchanged pins the non-positive guard: a caller that
// decided to keep everything must not be turned into one that drops events, even
// when the whole history is unreplayable on its own.
func TestSnapCutKeepsACutOfZeroUnchanged(t *testing.T) {
	orphan := &events.ToolResultEvent{ParentID: "c1", Name: "t", Result: events.NewToolResult(false, "r")}
	evs := []events.Event{userReq("u"), orphan}
	if isReplayable(evs, 0) {
		t.Fatal("the test needs a history that is not replayable from index 0")
	}
	if got := snapCut(evs, 0); got != 0 {
		t.Fatalf("snapCut(0) = %d, want 0 (nothing may be dropped)", got)
	}
	if got := snapCut(evs, -3); got != 0 {
		t.Fatalf("snapCut(-3) = %d, want 0", got)
	}
}

// TestSnapCutIgnoresEmptyCallIDs pins that an event with no call id carries no
// replay requirement: an empty id can never be answered, so treating it as one
// would make every such event unreplayable forever.
func TestSnapCutIgnoresEmptyCallIDs(t *testing.T) {
	singles := map[string]events.Event{
		"result without parent": &events.ToolResultEvent{Result: events.NewToolResult(false, "r")},
		"announced call without id": &events.ToolCallsEvent{
			Calls: []events.ToolCallEvent{{Name: "t"}},
		},
	}
	for name, ev := range singles {
		t.Run(name, func(t *testing.T) {
			evs := []events.Event{userReq("u"), ev}
			if !isReplayable(evs, 1) {
				t.Fatalf("%s was treated as unreplayable", name)
			}
			if got := snapCut(evs, 1); got != 1 {
				t.Fatalf("snapCut = %d, want the cut left alone at 1", got)
			}
		})
	}

	t.Run("turn with empty-id call and result", func(t *testing.T) {
		evs := []events.Event{
			&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{{Name: "t"}}}},
			&events.ToolResultsEvent{Results: []events.ToolResultEvent{{Name: "t"}}},
		}
		if !isReplayable(evs, 0) {
			t.Fatal("an empty id must not create a replay requirement")
		}
	})
}

// TestSnapCutTreatsEveryResultSubtypeAsRequired pins requiredIDs against every
// ToolResultEvent shape the agent can emit: each one is an orphan without its
// call, so the cut must advance past it.
func TestSnapCutTreatsEveryResultSubtypeAsRequired(t *testing.T) {
	call := events.ToolCallEvent{ID: "c1", Name: "t"}
	results := map[string]events.Event{
		"batch":          &events.ToolResultsEvent{Results: []events.ToolResultEvent{{ParentID: "c1"}}},
		"single":         &events.ToolResultEvent{ParentID: "c1"},
		"error":          events.FromErrorCall(call, errors.New("boom")),
		"not found":      events.ToolNotFoundEvent{ToolErrorEvent: events.FromErrorCall(call, errors.New("missing"))},
		"builtin result": &events.BuiltinToolResultEvent{ToolResultEvent: events.FromResultCall(call, events.NewToolResult(false, "r"))},
	}
	for name, ev := range results {
		t.Run(name, func(t *testing.T) {
			evs := []events.Event{userReq("u"), ev}
			if isReplayable(evs, 1) {
				t.Fatalf("%s alone was treated as replayable", name)
			}
			if got := snapCut(evs, 1); got != 2 {
				t.Fatalf("snapCut = %d, want 2 (past the orphan)", got)
			}
		})
	}
}

// TestCompactionSkipsWhenSnapLeavesNoTail pins the retained-range guard: when
// every event would have to be summarized (the tail is not replayable), the
// history must be left alone instead of being replaced by a summary alone.
func TestCompactionSkipsWhenSnapLeavesNoTail(t *testing.T) {
	ctx, log := newTestCtx(t)
	// A tool round with no final answer: the last events are the result and the
	// call it answers, and no cut inside the round is replayable.
	for _, ev := range toolRoundHistory() {
		send(t, ctx, ev)
	}
	before, err := ctx.Stream.History().GetEvents()
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}

	strategy := strategyFunc(func(*agocontext.Context, []events.Event) (string, error) {
		t.Error("the strategy must not run when nothing can be retained")
		return "s", nil
	})
	mw := New(EventCountTrigger(1), strategy)()
	if _, err := mw.OnTurn(func(*events.ModelRequest, *agocontext.Context) (*events.ModelResponse, error) {
		return &events.ModelResponse{}, nil
	}, &events.ModelRequest{}, ctx); err != nil {
		t.Fatalf("OnTurn: %v", err)
	}

	after, err := ctx.Stream.History().GetEvents()
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if !equalStrs(fingerprint(before), fingerprint(after)) {
		t.Fatalf("history changed:\n before %v\n after  %v", fingerprint(before), fingerprint(after))
	}
	for _, e := range *log {
		if strings.HasPrefix(e, "Compaction") {
			t.Fatalf("compaction events were published: %v", *log)
		}
	}
}

// TestSnapCutPairsBuiltinCallsWithTheirResults pins that a hosted (builtin) call
// answers its own result: builtin calls arrive as their own event and never
// appear in ModelResponse.ToolCalls, so reading only the responses would call
// their results orphaned.
func TestSnapCutPairsBuiltinCallsWithTheirResults(t *testing.T) {
	evs := []events.Event{
		userReq("search"),
		&events.BuiltinToolCallEvent{ToolCallEvent: events.ToolCallEvent{ID: "b1", Name: "web_search"}},
		&events.BuiltinToolResultEvent{ToolResultEvent: events.ToolResultEvent{
			ParentID: "b1", Name: "web_search", Result: events.NewToolResult(false, "hits"),
		}},
	}
	if !isReplayable(evs, 1) {
		t.Fatal("a builtin call must answer its own result")
	}
	if got := snapCut(evs, 1); got != 1 {
		t.Fatalf("snapCut = %d, want the cut left at 1", got)
	}
	// Without the call the result is an orphan.
	if isReplayable(evs[2:], 0) {
		t.Fatal("a builtin result without its call is not replayable")
	}
}
