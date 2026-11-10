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
// conversational events and everything after them. When the window already
// covers the history (Target <= 0, or fewer than Target conversational events),
// the result is evs itself.
func (t TailWindowCompact) Compact(evs []events.Event) []events.Event {
	cut := cutIndex(evs, t.Target)
	if cut <= 0 {
		return evs
	}
	return evs[cut:]
}
