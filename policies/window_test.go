package policies

import (
	"testing"

	"github.com/luckystrker/ago/events"
)

// toolPairTranscript builds a conversational transcript whose cut between the
// call and its result would orphan the answer:
//
//	TextInput | ModelResponse(tool_calls=[call]) | ToolCallEvent | ToolResultEvent
//	| UsageEvent (not conversational) | TextInput | ModelResponse
func toolPairTranscript(t *testing.T) ([]events.Event, events.ToolCallEvent) {
	t.Helper()
	call := events.NewToolCallEvent("search")
	return []events.Event{
		&events.TextInput{Content: "find it"},
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{call}}},
		&call,
		&events.ToolResultEvent{ParentID: call.ID, Name: call.Name, Result: events.NewToolResult(false, "found")},
		&events.UsageEvent{Kind: events.UsageKindModelCall},
		&events.TextInput{Content: "thanks"},
		&events.ModelResponse{Message: &events.ModelMessage{Content: "done"}},
	}, call
}

// toolRoundTranscript builds the shape the turn loop actually produces:
// the executor sends results as one ToolResultsEvent container, never as
// top-level result events (tools/executor.go):
//
//	TextInput | ModelResponse(tool_calls=[call]) | ToolCallEvent
//	| ToolResultsEvent([result]) | TextInput | ModelResponse
func toolRoundTranscript(t *testing.T) ([]events.Event, events.ToolCallEvent) {
	t.Helper()
	call := events.NewToolCallEvent("search")
	return []events.Event{
		&events.TextInput{Content: "find it"},
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{call}}},
		&call,
		&events.ToolResultsEvent{Results: []events.ToolResultEvent{events.FromResultCall(call, events.NewToolResult(false, "found"))}},
		&events.TextInput{Content: "thanks"},
		&events.ModelResponse{Message: &events.ModelMessage{Content: "done"}},
	}, call
}

func TestSlidingWindowPolicy(t *testing.T) {
	t.Run("widens to keep the tool pair", func(t *testing.T) {
		in, call := toolPairTranscript(t)
		p := NewSlidingWindowPolicy(3) // cut lands on the ToolResultEvent
		_, got, err := p.Apply(nil, in, nil)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		// [ToolResult, TextInput, ModelResponse] would orphan the result, so the
		// window widens past the call event to the response announcing it: a
		// standalone call event maps to nothing on the wire, so only the
		// announcing ModelResponse closes the pair.
		if len(got) != 5 {
			t.Fatalf("got %d events (%v), want 5", len(got), eventTypesOf(got))
		}
		first, ok := events.AsValue(got[0]).(events.ModelResponse)
		if !ok || len(first.ToolCalls.Calls) != 1 || first.ToolCalls.Calls[0].ID != call.ID {
			t.Fatalf("window opens with %v, want the ModelResponse announcing call %s", got[0].EventType(), call.ID)
		}
		if got[1].EventType() != "ToolCallEvent" || got[2].EventType() != "ToolResultEvent" || got[3].EventType() != "TextInput" || got[4].EventType() != "ModelResponse" {
			t.Errorf("window tail = %v, want [ToolCallEvent ToolResultEvent TextInput ModelResponse]", eventTypesOf(got[1:]))
		}
		for _, ev := range got {
			if ev.EventType() == "UsageEvent" {
				t.Errorf("UsageEvent leaked into the window")
			}
		}
	})

	t.Run("widens the executor's ToolResultsEvent container", func(t *testing.T) {
		in, call := toolRoundTranscript(t)
		// Every cut that lands inside the round (n = 3, 4, 5) widens back to the
		// announcing response instead of stranding the container.
		for _, n := range []int{3, 4, 5} {
			_, got, err := NewSlidingWindowPolicy(n).Apply(nil, in, nil)
			if err != nil {
				t.Fatalf("Apply(n=%d): %v", n, err)
			}
			if len(got) != 5 {
				t.Fatalf("n=%d: got %d events (%v), want the whole round (5)", n, len(got), eventTypesOf(got))
			}
			first, ok := events.AsValue(got[0]).(events.ModelResponse)
			if !ok || len(first.ToolCalls.Calls) != 1 || first.ToolCalls.Calls[0].ID != call.ID {
				t.Fatalf("n=%d: window opens with %v, want the announcing ModelResponse", n, eventTypesOf(got))
			}
			results, ok := events.AsValue(got[2]).(events.ToolResultsEvent)
			if !ok || len(results.Results) != 1 || results.Results[0].ParentID != call.ID {
				t.Fatalf("n=%d: got[2] = %v, want the ToolResultsEvent answering %s", n, got[2].EventType(), call.ID)
			}
		}
	})

	t.Run("a window small enough drops the whole round", func(t *testing.T) {
		in, _ := toolRoundTranscript(t)
		// n = 2 cuts before the round starts: the cut lands on the fresh user
		// turn, which stands on its own — no widening, round gone entirely.
		_, got, err := NewSlidingWindowPolicy(2).Apply(nil, in, nil)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if len(got) != 2 || got[0].EventType() != "TextInput" || got[1].EventType() != "ModelResponse" {
			t.Fatalf("got %v, want [TextInput ModelResponse]", eventTypesOf(got))
		}
	})

	t.Run("window larger than transcript passes through", func(t *testing.T) {
		in, _ := toolPairTranscript(t)
		p := NewSlidingWindowPolicy(100)
		prompts, got, err := p.Apply([]string{"sys"}, in, nil)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if len(prompts) != 1 || prompts[0] != "sys" {
			t.Errorf("prompts = %v, want unchanged", prompts)
		}
		if len(got) != len(in) {
			t.Errorf("got %d events, want the full transcript (%d)", len(got), len(in))
		}
	})

	t.Run("non-positive window passes through", func(t *testing.T) {
		in, _ := toolPairTranscript(t)
		for _, n := range []int{0, -1} {
			_, got, err := NewSlidingWindowPolicy(n).Apply(nil, in, nil)
			if err != nil {
				t.Fatalf("Apply(n=%d): %v", n, err)
			}
			if len(got) != len(in) {
				t.Errorf("n=%d: got %d events, want %d", n, len(got), len(in))
			}
		}
	})

	t.Run("only conversational events count toward n", func(t *testing.T) {
		in, _ := toolPairTranscript(t) // 6 conversational + 1 UsageEvent
		p := NewSlidingWindowPolicy(6)
		_, got, err := p.Apply(nil, in, nil)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if len(got) != len(in) {
			t.Errorf("got %d events, want the full transcript", len(got))
		}
	})
}

func TestWidenCut(t *testing.T) {
	// Neither id can be answered anywhere in the transcript — no ModelResponse
	// announces either call — so widening cannot close either requirement: the
	// span grows to the whole list instead of emptying (the AGo widening
	// deviation; Python would prune the orphans instead).
	orphan := &events.ToolResultEvent{ParentID: "gone", Name: "search"}
	tailCall := events.NewToolCallEvent("calc")
	convs := []events.Event{
		orphan,
		&tailCall,
		&events.ToolResultEvent{ParentID: tailCall.ID},
		&events.TextInput{Content: "ok"},
	}
	if got := widenCut(convs, 2); got != 0 {
		t.Errorf("widenCut = %d, want 0 (nothing answers either call, whole list kept)", got)
	}
	if got := widenCut(convs, 0); got != 0 {
		t.Errorf("widenCut = %d, want 0 (nothing trimmed, nothing to widen)", got)
	}
	if got := widenCut(convs, 3); got != 3 {
		t.Errorf("widenCut = %d, want 3 (leading TextInput stands on its own)", got)
	}

	// A builtin result whose call stands inside the span does not widen: builtin
	// calls are answerable themselves (they replay as their own provider item),
	// so the pair stays together without the preceding turn.
	builtin := events.NewToolCallEvent("web.run")
	builtinConvs := []events.Event{
		&events.TextInput{Content: "go"},
		&events.BuiltinToolCallEvent{ToolCallEvent: builtin},
		&events.BuiltinToolResultEvent{ToolResultEvent: events.ToolResultEvent{ParentID: builtin.ID}},
	}
	if got := widenCut(builtinConvs, 2); got != 1 {
		t.Errorf("widenCut = %d, want 1 (the builtin call inside the span answers its result)", got)
	}
	if got := widenCut(builtinConvs, 1); got != 1 {
		t.Errorf("widenCut = %d, want 1 (a builtin call stands on its own)", got)
	}

	// A plain round widens past the standalone call event to the response
	// announcing it: a suffix that opens on the call or the result can never
	// contain that response (it precedes both in the transcript), so the whole
	// round comes back — a standalone call event alone cannot answer anything.
	response := &events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{tailCall}}}
	round := []events.Event{
		response,
		&tailCall,
		&events.ToolResultEvent{ParentID: tailCall.ID},
	}
	for _, cut := range []int{1, 2} {
		if got := widenCut(round, cut); got != 0 {
			t.Errorf("widenCut(round, %d) = %d, want 0 (the announcing response comes back in)", cut, got)
		}
	}
}
