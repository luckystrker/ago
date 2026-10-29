package policies

import (
	"testing"

	"ago/events"
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

func TestSlidingWindowPolicy(t *testing.T) {
	t.Run("widens to keep the tool pair", func(t *testing.T) {
		in, call := toolPairTranscript(t)
		p := NewSlidingWindowPolicy(3) // cut lands on the ToolResultEvent
		_, got, err := p.Apply(nil, in, nil)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		// [ToolResult, TextInput, ModelResponse] would orphan the result, so the
		// window widens to [ToolCallEvent, ToolResult, TextInput, ModelResponse].
		if len(got) != 4 {
			t.Fatalf("got %d events (%v), want 4", len(got), eventTypesOf(got))
		}
		first, ok := events.AsValue(got[0]).(events.ToolCallEvent)
		if !ok {
			t.Fatalf("window opens with %s, want ToolCallEvent", got[0].EventType())
		}
		if first.ID != call.ID {
			t.Errorf("window call id = %s, want %s", first.ID, call.ID)
		}
		if got[1].EventType() != "ToolResultEvent" || got[2].EventType() != "TextInput" || got[3].EventType() != "ModelResponse" {
			t.Errorf("window tail = %v, want [ToolResultEvent TextInput ModelResponse]", eventTypesOf(got[1:]))
		}
		for _, ev := range got {
			if ev.EventType() == "UsageEvent" {
				t.Errorf("UsageEvent leaked into the window")
			}
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
	// A result orphaned at the very start of the transcript cannot be repaired by
	// widening: the span grows to the whole list instead of emptying.
	orphan := &events.ToolResultEvent{ParentID: "gone", Name: "search"}
	tailCall := events.NewToolCallEvent("calc")
	convs := []events.Event{
		orphan,
		&tailCall,
		&events.ToolResultEvent{ParentID: tailCall.ID},
		&events.TextInput{Content: "ok"},
	}
	if got := widenCut(convs, 2); got != 1 {
		t.Errorf("widenCut = %d, want 1 (pair [call, result] kept, orphan result dropped)", got)
	}
	if got := widenCut(convs, 0); got != 0 {
		t.Errorf("widenCut = %d, want 0 (nothing trimmed, nothing to widen)", got)
	}
	if got := widenCut(convs, 3); got != 3 {
		t.Errorf("widenCut = %d, want 3 (leading TextInput stands on its own)", got)
	}
}
