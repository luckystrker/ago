package policies

import (
	agocontext "ago/context"
	"ago/events"
)

// SlidingWindowPolicy keeps the last n conversational events and drops older
// ones (Python parity: sliding_window.py over _replay.replayable_span).
//
// The window is a target, not a guarantee: a cut that would orphan a tool
// result — its announcing response retained nowhere — widens back until the
// retained span contains the whole round, so the model never sees an answer
// without the assistant tool_calls that requested it.
type SlidingWindowPolicy struct {
	n int
}

// NewSlidingWindowPolicy keeps at most n conversational events. n <= 0 disables
// trimming: a window of zero events is never a useful request.
func NewSlidingWindowPolicy(n int) SlidingWindowPolicy { return SlidingWindowPolicy{n: n} }

// Apply returns the last n conversational events, widened as needed to keep
// tool call/result pairs together. When no trimming is needed the input passes
// through unchanged.
func (p SlidingWindowPolicy) Apply(prompts []string, evs []events.Event, _ *agocontext.Context) ([]string, []events.Event, error) {
	convs := conversational(evs)
	if p.n <= 0 || len(convs) <= p.n {
		return prompts, evs, nil
	}
	return prompts, convs[widenCut(convs, len(convs)-p.n):], nil
}
