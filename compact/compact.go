// Package compact reduces stream history when measurable limits are approached
// (Python parity: ag2/compact.py plus the post-turn _CompactionMiddleware in
// ag2/agent.py). Compaction protects runtime stability: it is the
// constraint-respecting operation — triggered when event count or token
// estimate exceed a threshold, it replaces the old transcript span with one
// CompactionSummary event and keeps the recent tail verbatim. Compaction
// removes; aggregation creates. They are separate concerns.
//
// Wire into an agent as middleware: agent middleware lists hold
// middleware.Factory values, so compact.New(trigger, strategy) drops in
// directly. The hook is post-processing: the turn runs first, compaction fires
// only after a successful turn, and a summarization failure never fails the
// turn — the history is left untouched.
package compact

import (
	"fmt"
	"log/slog"
	"reflect"

	agocontext "ago/context"
	"ago/events"
	"ago/middleware"
	"ago/stream"
)

// CompactionMiddleware compacts the stream history after a turn when the
// trigger fires. Instances are built per turn by New's Factory; trigger and
// strategy are shared across turns. Safe for concurrent use: all state is
// read-only after construction.
type CompactionMiddleware struct {
	middleware.Base
	trigger  Trigger
	strategy Strategy
	agent    string
}

var _ middleware.Middleware = (*CompactionMiddleware)(nil)

// Option configures a CompactionMiddleware.
type Option func(*CompactionMiddleware)

// WithAgent stamps the lifecycle events with the owning agent's name (Python
// parity: CompactionStarted.agent / CompactionCompleted.agent).
func WithAgent(name string) Option {
	return func(m *CompactionMiddleware) { m.agent = name }
}

// New builds the middleware Factory: one CompactionMiddleware per turn.
// A nil trigger or nil strategy yields a pass-through middleware.
func New(trigger Trigger, strategy Strategy, opts ...Option) middleware.Factory {
	return func() middleware.Middleware {
		m := &CompactionMiddleware{trigger: trigger, strategy: strategy}
		for _, o := range opts {
			o(m)
		}
		return m
	}
}

// OnTurn runs the rest of the pipeline, then compacts. A failed turn is
// returned untouched — compaction fires only after a successful turn (Python
// parity: the strategy runs after call_next returns).
func (m *CompactionMiddleware) OnTurn(next middleware.TurnFunc, ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
	result, err := next(ev, ctx)
	if err != nil {
		return result, err
	}
	if err := m.maybeCompact(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// maybeCompact snapshots the history, consults the trigger, and on fire replaces
// the old conversational span with one CompactionSummary plus the fresh range.
func (m *CompactionMiddleware) maybeCompact(ctx *agocontext.Context) error {
	if m.trigger == nil || m.strategy == nil {
		return nil
	}
	evs, err := ctx.Stream.History().GetEvents()
	if err != nil {
		return fmt.Errorf("compact: read history: %w", err)
	}
	if !m.trigger.ShouldTrigger(evs, historyUsage(evs)) {
		return nil
	}
	cut := cutIndex(evs, KeepRecent)
	if cut <= 0 {
		// Fewer than KeepRecent+1 conversational events, or nothing before the
		// fresh range: there is no old span worth a summary (Python parity:
		// _cut_for_target returning 0 is a no-op).
		return nil
	}
	old := conversational(evs[:cut])
	if len(old) == 0 {
		return nil
	}

	name := typeLabel(m.strategy)
	if err := ctx.Send(&events.CompactionStarted{Agent: m.agent, Strategy: name, EventCount: len(evs)}); err != nil {
		return fmt.Errorf("compact: send CompactionStarted: %w", err)
	}

	// Collect the token records sent while the strategy runs: they persist into
	// the transcript during the call and would otherwise be wiped by the Replace
	// below (Python parity: spent_during, collected live rather than re-read,
	// so no assumption about how history grew in the meantime is needed).
	var spent []*events.UsageEvent
	collect := func(ev events.Event) error {
		if u, ok := events.AsValue(ev).(events.UsageEvent); ok {
			ue := u
			spent = append(spent, &ue)
		}
		return nil
	}
	sub := ctx.Stream.Subscribe(collect, stream.SubscribeOptions{})
	summary, err := m.strategy.Summarize(ctx, old)
	ctx.Stream.Unsubscribe(sub)
	if err != nil {
		// A summarization failure never fails the turn: report it and keep the
		// history exactly as it was — the records collected above are already
		// persisted in it (Python parity: CompactionFailed under
		// `with suppress(Exception)`).
		slog.Error("compact: summarization failed", "agent", m.agent, "strategy", name, "err", err)
		failed := &events.CompactionFailed{Agent: m.agent, Strategy: name, ErrorType: typeLabel(err), Error: err.Error()}
		if serr := ctx.Send(failed); serr != nil {
			slog.Error("compact: send CompactionFailed failed", "err", serr)
		}
		return nil
	}

	replacement := make([]events.Event, 0, 1+len(evs)-cut+len(spent))
	replacement = append(replacement, &events.CompactionSummary{Content: summary})
	replacement = append(replacement, evs[cut:]...)
	// AGO-DEVIATION: Python's _with_usage_events additionally re-attaches every
	// UsageEvent from before the compaction so spend outside the retained window
	// keeps being reportable; the AGo contract replaces history with the summary
	// plus the fresh range, so only the records of the summarization window ride
	// along (appended below).
	for _, ue := range spent {
		replacement = append(replacement, ue)
	}
	if err := ctx.Stream.History().Replace(replacement); err != nil {
		return fmt.Errorf("compact: replace history: %w", err)
	}

	done := &events.CompactionCompleted{
		Agent:        m.agent,
		Strategy:     name,
		EventsBefore: len(evs),
		EventsAfter:  len(replacement),
		LLMCalls:     len(spent),
	}
	for _, ue := range spent {
		done.Usage = done.Usage.Add(ue.Usage)
	}
	if err := ctx.Send(done); err != nil {
		return fmt.Errorf("compact: send CompactionCompleted: %w", err)
	}
	return nil
}

// cutIndex returns the index of the first of the last keep conversational
// events, sweeping from the tail so non-conversational events after the cut
// ride along with the fresh range (Python parity: _cut_for_target). -1 when
// there are fewer than keep conversational events.
func cutIndex(evs []events.Event, keep int) int {
	if keep <= 0 {
		return -1
	}
	seen := 0
	for i := len(evs) - 1; i >= 0; i-- {
		if ev := evs[i]; ev != nil && ev.Conversational() {
			seen++
			if seen == keep {
				return i
			}
		}
	}
	return -1
}

// conversational filters to the events compaction summarizes and counts.
func conversational(evs []events.Event) []events.Event {
	out := make([]events.Event, 0, len(evs))
	for _, ev := range evs {
		if ev != nil && ev.Conversational() {
			out = append(out, ev)
		}
	}
	return out
}

// historyUsage sums the usage reported by the ModelResponse events of a history
// snapshot — the spend the trigger's second argument sees.
func historyUsage(evs []events.Event) events.Usage {
	var u events.Usage
	for _, ev := range evs {
		if r, ok := events.AsValue(ev).(events.ModelResponse); ok {
			u = u.Add(r.Usage)
		}
	}
	return u
}

// typeLabel names a value's dynamic type (Python parity: type(x).__name__ for
// Compaction*.strategy and .error_type); anonymous types fall back to the full
// reflect string.
func typeLabel(v any) string {
	t := reflect.TypeOf(v)
	if t == nil {
		return ""
	}
	if n := t.Name(); n != "" {
		return n
	}
	return t.String()
}
