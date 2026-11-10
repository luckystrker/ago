package compact

import "github.com/luckystrker/ago/events"

// TailWindowCompact keeps the last Target conversational events and drops the
// rest, with zero LLM cost (Python parity: compact.TailWindowCompact). It is the
// strategy for histories whose old context has diminishing value: no summary is
// produced, the dropped events simply disappear.
//
// Non-conversational events after the cut ride along with the fresh tail, the
// same rule the summarizing path uses.
type TailWindowCompact struct {
	// Target is the number of trailing conversational events to keep.
	Target int
}

// NewTailWindowCompact builds a window that keeps the last target conversational
// events.
func NewTailWindowCompact(target int) TailWindowCompact {
	return TailWindowCompact{Target: target}
}

// Compact returns the part of evs the window keeps: the last Target
// conversational events and everything after them, moved forward to a replayable
// boundary (snapCut) so a retained tool result never loses the call it answers.
// When the window already covers the history (Target <= 0, or fewer than Target
// conversational events), or no boundary is left to cut at, the result is evs
// itself.
func (t TailWindowCompact) Compact(evs []events.Event) []events.Event {
	cut := cutIndex(evs, t.Target)
	if cut <= 0 {
		return evs
	}
	cut = snapCut(evs, cut)
	if cut >= len(evs) {
		// The whole history would have to go to keep the tail replayable: keep it
		// instead (Python parity: `if not retained: return events`).
		return evs
	}
	return evs[cut:]
}
