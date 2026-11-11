package compact

import (
	"github.com/luckystrker/ago/events"
)

// Replay invariants for a trimmed span of history (Python parity:
// ag2/_replay.py). Every history reducer cuts the event list at an index and
// replays evs[cut:] to the provider, and that tail has to stand on its own:
//
//	retained event            required companion              if missing
//	tool result               the call it answers             orphan tool message
//	local call event          the response announcing it       maps to nothing
//
// Compaction persists everything it drops, so it moves the cut instead of
// filtering the tail: filtering would leave an event neither retained nor
// persisted.
//
// AGO-DEVIATION: Python's _replay also pairs provider-native items — a builtin
// call needs the reasoning item that anchors it, and some providers hand back a
// turn item that stands in for the whole assistant turn. AGo has no
// ProviderReplay events (D-030), so the invariant reduces to calls and results.

// snapCut advances cut to the next index whose retained span is replayable
// (Python parity: _replay.snap). Advancing only ever drops more events, so a
// caller's budget still holds afterwards, and the events skipped over are the
// caller's to persist. A cut of 0 trims nothing and is returned untouched: a
// caller that decided to keep everything must not be turned into one that drops
// events.
func snapCut(evs []events.Event, cut int) int {
	if cut <= 0 {
		return 0
	}
	for cut < len(evs) && !isReplayable(evs, cut) {
		cut++
	}
	return cut
}

// isReplayable reports whether evs[cut:] needs nothing the cut dropped.
func isReplayable(evs []events.Event, cut int) bool {
	span := evs[cut:]
	answerable := answerableIDs(span)
	for _, ev := range span {
		for _, id := range requiredIDs(ev) {
			if _, ok := answerable[id]; !ok {
				return false
			}
		}
		for _, id := range announcedIDs(ev) {
			if _, ok := answerable[id]; !ok {
				return false
			}
		}
	}
	return true
}

// answerableIDs returns the ids of calls a span can answer: calls inside a
// ModelResponse, plus builtin calls, which arrive as their own event and never
// appear in ModelResponse.ToolCalls.
func answerableIDs(span []events.Event) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, ev := range span {
		switch e := events.AsValue(ev).(type) {
		case events.ModelResponse:
			for _, call := range e.ToolCalls.Calls {
				if call.ID != "" {
					ids[call.ID] = struct{}{}
				}
			}
		case events.BuiltinToolCallEvent:
			if e.ID != "" {
				ids[e.ID] = struct{}{}
			}
		}
	}
	return ids
}

// requiredIDs returns the call ids an event is a result for; nil for everything
// else. Every ToolResultEvent subtype (error, not-found, builtin) counts: a
// result without its call is an orphan on every wire that replays it.
func requiredIDs(ev events.Event) []string {
	switch e := events.AsValue(ev).(type) {
	case events.ToolResultsEvent:
		ids := make([]string, 0, len(e.Results))
		for _, r := range e.Results {
			if r.ParentID != "" {
				ids = append(ids, r.ParentID)
			}
		}
		return ids
	case events.ToolResultEvent:
		return parentIDs(e.ParentID)
	case events.ToolErrorEvent:
		return parentIDs(e.ParentID)
	case events.ToolNotFoundEvent:
		return parentIDs(e.ParentID)
	case events.BuiltinToolResultEvent:
		return parentIDs(e.ParentID)
	}
	return nil
}

// parentIDs wraps one parent id, dropping the empty one.
func parentIDs(id string) []string {
	if id == "" {
		return nil
	}
	return []string{id}
}

// announcedIDs returns the call ids an event merely announces, carrying no
// replayable content: a local call is announced twice, inside the ModelResponse
// that requested it and as a standalone event for observers. Without the
// response the standalone event maps to no input item at all. Builtin calls are
// excluded: they carry their own provider item.
func announcedIDs(ev events.Event) []string {
	switch e := events.AsValue(ev).(type) {
	case events.ToolCallsEvent:
		ids := make([]string, 0, len(e.Calls))
		for _, call := range e.Calls {
			if call.ID != "" {
				ids = append(ids, call.ID)
			}
		}
		return ids
	case events.ToolCallEvent:
		return parentIDs(e.ID)
	}
	return nil
}
