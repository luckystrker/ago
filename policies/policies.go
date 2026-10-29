// Package policies assembles the LLM call context: a chain of AssemblyPolicy
// transforms reshapes (system-prompt fragments, transcript events) before every
// model call (Python parity: ag2/policies composed left-to-right inside
// AssemblerMiddleware.on_llm_call).
//
// Policies are pure transforms over their inputs, with two bounded exceptions:
// AlertPolicy sends a HaltEvent through the context, and the memory policies
// read a knowledge.Store. None of them mutate the caller's slices or write to
// the conversation stream.
//
// Order matters: filters and injections first, reductions (window, budget)
// last, so injected context is counted against the reduction budget.
package policies

import (
	agocontext "ago/context"
	"ago/events"
)

// AssemblyPolicy transforms the (prompts, events) pair before each LLM call.
// Policies compose: each Apply sees the output of the previous one.
type AssemblyPolicy interface {
	Apply(prompts []string, evs []events.Event, ctx *agocontext.Context) ([]string, []events.Event, error)
}

var (
	_ AssemblyPolicy = ConversationPolicy{}
	_ AssemblyPolicy = SlidingWindowPolicy{}
	_ AssemblyPolicy = TokenBudgetPolicy{}
	_ AssemblyPolicy = (*AlertPolicy)(nil)
	_ AssemblyPolicy = WorkingMemoryPolicy{}
	_ AssemblyPolicy = EpisodicMemoryPolicy{}
)

// conversational returns the events replayed to the model (Conversational() ==
// true). The agent hands the full stream history to every LLM call, so the
// reduction policies subset it themselves: usage telemetry, alerts and
// lifecycle bookkeeping never count toward a window or budget.
func conversational(evs []events.Event) []events.Event {
	out := make([]events.Event, 0, len(evs))
	for _, ev := range evs {
		if ev.Conversational() {
			out = append(out, ev)
		}
	}
	return out
}

// parentCallID returns the id of the tool call ev answers ("" for non-results).
// ToolErrorEvent and ToolNotFoundEvent embed ToolResultEvent, and
// BuiltinToolResultEvent is the provider-hosted variant — all four carry
// ParentID.
func parentCallID(ev events.Event) string {
	switch e := events.AsValue(ev).(type) {
	case events.ToolResultEvent:
		return e.ParentID
	case events.ToolErrorEvent:
		return e.ParentID
	case events.ToolNotFoundEvent:
		return e.ParentID
	case events.BuiltinToolResultEvent:
		return e.ParentID
	}
	return ""
}

// callIDs indexes every tool call declared in evs: model-requested calls,
// provider-hosted calls and client-executed calls alike.
func callIDs(evs []events.Event) map[string]bool {
	out := make(map[string]bool, len(evs))
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.ToolCallEvent:
			out[e.ID] = true
		case events.BuiltinToolCallEvent:
			out[e.ToolCallEvent.ID] = true
		case events.ClientToolCallEvent:
			out[e.ID] = true
		}
	}
	return out
}

// widenCut moves cut back until the retained span convs[cut:] opens with an
// event that stands on its own: a leading tool result whose call lies outside
// the span pulls the call back in. The span never widens past the start of the
// transcript, so it can only grow, never empty (Python parity: replayable_span
// widens rather than empties).
func widenCut(convs []events.Event, cut int) int {
	for cut > 0 && cut < len(convs) {
		parent := parentCallID(convs[cut])
		if parent == "" || callIDs(convs[cut:])[parent] {
			break
		}
		cut--
	}
	return cut
}
