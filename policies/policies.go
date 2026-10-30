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
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
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

// requiredCallIDs returns the call ids ev needs answered inside the span it
// opens; nil when the event stands on its own. ToolErrorEvent and
// ToolNotFoundEvent embed ToolResultEvent, and BuiltinToolResultEvent is the
// provider-hosted variant — all four carry ParentID. ToolResultsEvent is the
// batch container the executor actually sends (tools: executor → cx.Send),
// so every result it carries requires its own parent. A standalone
// ToolCallEvent or ClientToolCallEvent maps to nothing on the wire — provider
// mappers replay calls only from a ModelResponse's tool_calls — so it requires
// the response announcing it. A builtin call carries its own provider item and
// needs nothing.
func requiredCallIDs(ev events.Event) []string {
	switch e := events.AsValue(ev).(type) {
	case events.ToolResultEvent:
		return callIDList(e.ParentID)
	case events.ToolErrorEvent:
		return callIDList(e.ParentID)
	case events.ToolNotFoundEvent:
		return callIDList(e.ParentID)
	case events.BuiltinToolResultEvent:
		return callIDList(e.ParentID)
	case events.ToolResultsEvent:
		var out []string
		for _, r := range e.Results {
			if r.ParentID != "" {
				out = append(out, r.ParentID)
			}
		}
		return out
	case events.ToolCallEvent:
		return callIDList(e.ID)
	case events.ClientToolCallEvent:
		return callIDList(e.ID)
	}
	return nil
}

func callIDList(id string) []string {
	if id == "" {
		return nil
	}
	return []string{id}
}

// answerableCallIDs indexes the call ids a retained span can answer: ids its
// ModelResponses announce in their tool_calls (what providers replay) and ids
// of builtin calls, which replay as their own provider item. Standalone call
// events do not qualify: they carry no replayable content, so their presence
// cannot satisfy a requirement (Python parity: _answerable_ids).
func answerableCallIDs(evs []events.Event) map[string]bool {
	out := make(map[string]bool, len(evs))
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.ModelResponse:
			for _, c := range e.ToolCalls.Calls {
				out[c.ID] = true
			}
		case events.BuiltinToolCallEvent:
			out[e.ToolCallEvent.ID] = true
		}
	}
	return out
}

// widenCut moves cut back until the retained span convs[cut:] opens with an
// event that stands on its own: an opening event whose required call ids are
// unanswered pulls its whole round back in — a leading tool result widens to
// the ModelResponse announcing its call (and the call events between), so the
// wire never opens with a tool answer without its assistant tool_calls. The
// span never widens past the start of the transcript, so it can only grow,
// never empty.
//
// AGO-DEVIATION: Python's replayable_span drops orphaned events from the tail
// instead of widening (_replay._prune); AGo keeps the whole round on the wire
// (D-030). Python's ProviderReplay anchor/turn items do not exist in the Go
// event vocabulary.
func widenCut(convs []events.Event, cut int) int {
	for cut > 0 && cut < len(convs) {
		need := requiredCallIDs(convs[cut])
		if len(need) == 0 {
			break // the opening event stands on its own
		}
		answerable := answerableCallIDs(convs[cut:])
		satisfied := true
		for _, id := range need {
			if !answerable[id] {
				satisfied = false
				break
			}
		}
		if satisfied {
			break
		}
		cut--
	}
	return cut
}
