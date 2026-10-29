package policies

import (
	"testing"

	"ago/events"
)

// textIn builds a TextInput and its estimated token cost (len/4, at least 1).
func textIn(content string) (events.Event, int) {
	ev := &events.TextInput{Content: content}
	return ev, events.EstimatedTokens(ev, charsPerToken)
}

func TestTokenBudgetPolicy(t *testing.T) {
	t.Run("keeps the fitting suffix", func(t *testing.T) {
		e1, c1 := textIn("aaaaaaaa") // 2 tokens
		e2, c2 := textIn("bb")       // 1 token
		e3, c3 := textIn("cccccccc") // 2 tokens
		if c1 != 2 || c2 != 1 || c3 != 2 {
			t.Fatalf("fixture costs = %d/%d/%d, want 2/1/2", c1, c2, c3)
		}
		in := []events.Event{e1, e2, e3}
		// Total 5 > budget 3: the suffix [e2, e3] costs exactly 3.
		_, got, err := NewTokenBudgetPolicy(3).Apply(nil, in, nil)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d events (%v), want 2", len(got), eventTypesOf(got))
		}
		if got[0].EventType() != "TextInput" || got[0].(*events.TextInput).Content != "bb" {
			t.Errorf("suffix starts with %+v, want the \"bb\" input", got[0])
		}
	})

	t.Run("newest event is always retained", func(t *testing.T) {
		e1, _ := textIn("aaaaaaaa")
		e2, _ := textIn("aaaaaaaa")
		e3, _ := textIn("cccccccc") // 2 tokens, alone over budget 1
		_, got, err := NewTokenBudgetPolicy(1).Apply(nil, []events.Event{e1, e2, e3}, nil)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if len(got) != 1 || got[0].(*events.TextInput).Content != "cccccccc" {
			t.Fatalf("got %v, want only the newest event", eventTypesOf(got))
		}
	})

	t.Run("budget covering the transcript passes through", func(t *testing.T) {
		e1, _ := textIn("aaaaaaaa")
		e2, _ := textIn("bb")
		in := []events.Event{e1, e2}
		prompts, got, err := NewTokenBudgetPolicy(1000).Apply([]string{"sys"}, in, nil)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if len(prompts) != 1 || prompts[0] != "sys" {
			t.Errorf("prompts = %v, want unchanged", prompts)
		}
		if len(got) != 2 {
			t.Errorf("got %d events, want 2", len(got))
		}
	})

	t.Run("widens for a tool pair at the cut", func(t *testing.T) {
		e1, _ := textIn("aaaaaaaa") // 2 tokens
		call := events.NewToolCallEvent("search")
		result := &events.ToolResultEvent{ParentID: call.ID, Name: call.Name}
		e4, _ := textIn("cccccccc") // 2 tokens
		in := []events.Event{e1, &call, result, e4}
		// Costs 2/1/1/2, total 6 > 3: the fitting suffix is [result, e4] (cost 3),
		// but it orphans the result — the span widens to include the call.
		_, got, err := NewTokenBudgetPolicy(3).Apply(nil, in, nil)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("got %d events (%v), want 3", len(got), eventTypesOf(got))
		}
		first, ok := events.AsValue(got[0]).(events.ToolCallEvent)
		if !ok || first.ID != call.ID {
			t.Fatalf("suffix opens with %v, want the tool call", eventTypesOf(got))
		}
	})
}
