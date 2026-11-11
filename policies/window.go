package policies

import (
	"fmt"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// SlidingWindowOption configures SlidingWindowPolicy.
type SlidingWindowOption func(*SlidingWindowPolicy)

// WithWindowTransparency injects a note telling the model that history was cut
// (Python parity: transparent=True). The note is appended as a prompt fragment:
// "[sliding_window] Showing last N of M events."
//
// AGO-DEVIATION: Python counts the raw event list; AGo counts the conversational
// subset it reduces (D-030), so telemetry and alerts never inflate M.
func WithWindowTransparency(on bool) SlidingWindowOption {
	return func(p *SlidingWindowPolicy) { p.transparent = on }
}

// SlidingWindowPolicy keeps the last n conversational events and drops older
// ones (Python parity: sliding_window.py over _replay.replayable_span).
//
// The window is a target, not a guarantee: a cut that would orphan a tool
// result — its announcing response retained nowhere — widens back until the
// retained span contains the whole round, so the model never sees an answer
// without the assistant tool_calls that requested it.
type SlidingWindowPolicy struct {
	n           int
	transparent bool
}

// NewSlidingWindowPolicy keeps at most n conversational events. n <= 0 disables
// trimming: a window of zero events is never a useful request.
func NewSlidingWindowPolicy(n int, opts ...SlidingWindowOption) SlidingWindowPolicy {
	p := SlidingWindowPolicy{n: n}
	for _, opt := range opts {
		opt(&p)
	}
	return p
}

// Apply returns the last n conversational events, widened as needed to keep
// tool call/result pairs together. When no trimming is needed the input passes
// through unchanged.
func (p SlidingWindowPolicy) Apply(prompts []string, evs []events.Event, _ *agocontext.Context) ([]string, []events.Event, error) {
	convs := conversational(evs)
	if p.n <= 0 || len(convs) <= p.n {
		return prompts, evs, nil
	}
	retained := convs[widenCut(convs, len(convs)-p.n):]
	if p.transparent {
		prompts = append(prompts, fmt.Sprintf("[sliding_window] Showing last %d of %d events.", len(retained), len(convs)))
	}
	return prompts, retained, nil
}
