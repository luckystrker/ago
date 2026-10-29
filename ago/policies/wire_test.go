package policies_test

import (
	"testing"

	"ago/config"
	"ago/events"
	"ago/policies"
)

// Wire regression for orphan tool-result widening: a reduced span that strands
// a tool result maps to a role:"tool" message without a preceding assistant
// message announcing its tool_call_id — a 400 from OpenAI-compatible APIs
// (only config/anthropic repairs orphans on replay; openai-compat, gemini and
// ollama replay verbatim). The span the policies keep must therefore hold the
// whole round, and the wire built from it must satisfy that ordering for every
// tool answer.
func TestReducedSpanBuildsValidToolWire(t *testing.T) {
	// The shape the turn loop produces: the executor sends results as one
	// ToolResultsEvent container (tools/executor.go), announced by the
	// ModelResponse that requested the calls.
	call := events.NewToolCallEvent("search")
	convs := []events.Event{
		&events.TextInput{Content: "find it"},
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{call}}},
		&call,
		&events.ToolResultsEvent{Results: []events.ToolResultEvent{events.FromResultCall(call, events.NewToolResult(false, "found"))}},
		&events.TextInput{Content: "thanks"},
		&events.ModelResponse{Message: &events.ModelMessage{Content: "done"}},
	}

	cases := []struct {
		name string
		p    policies.AssemblyPolicy
	}{
		{"sliding window inside the round", policies.NewSlidingWindowPolicy(3)},
		{"token budget inside the round", policies.NewTokenBudgetPolicy(3)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, got, err := tc.p.Apply(nil, convs, nil)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			msgs, err := config.BuildMessages(nil, got)
			if err != nil {
				t.Fatalf("BuildMessages: %v", err)
			}
			sawToolAnswer := assertAnnouncedToolWire(t, msgs)
			if !sawToolAnswer {
				t.Fatalf("wire lost the tool answer entirely: %d messages, want the round widened in, not dropped", len(msgs))
			}
		})
	}
}

// assertAnnouncedToolWire checks that every role:"tool" message is preceded by
// an assistant message replaying its tool_call_id, and reports whether any
// tool answer is present at all.
func assertAnnouncedToolWire(t *testing.T, msgs []map[string]any) bool {
	t.Helper()
	announced := map[string]bool{}
	sawToolAnswer := false
	for _, m := range msgs {
		switch m["role"] {
		case "assistant":
			calls, ok := m["tool_calls"].([]map[string]any)
			if !ok {
				continue
			}
			for _, c := range calls {
				if id, ok := c["id"].(string); ok {
					announced[id] = true
				}
			}
		case "tool":
			sawToolAnswer = true
			id, _ := m["tool_call_id"].(string)
			if !announced[id] {
				t.Errorf("wire has a role:tool message for call %q with no preceding assistant tool_calls (invalid request)", id)
			}
		}
	}
	return sawToolAnswer
}
