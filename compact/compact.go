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

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/usage"
)

// CompactionMiddleware compacts the stream history after a turn when the
// trigger fires. Instances are built per turn by New's Factory; trigger and
// strategy are shared across turns. Safe for concurrent use: all state is
// read-only after construction.
type CompactionMiddleware struct {
	middleware.Base
	trigger  Trigger
	strategy Strategy
	// tail, when set, replaces the summarizing strategy with the zero-cost
	// TailWindowCompact rewrite (NewTailWindow).
	tail  *TailWindowCompact
	agent string
	// keep overrides KeepRecent: how many trailing conversational events a
	// summarizing compaction retains verbatim (0 = KeepRecent).
	keep int
}

var _ middleware.Middleware = (*CompactionMiddleware)(nil)

// Option configures a CompactionMiddleware.
type Option func(*CompactionMiddleware)

// WithAgent stamps the lifecycle events with the owning agent's name (Python
// parity: CompactionStarted.agent / CompactionCompleted.agent).
func WithAgent(name string) Option {
	return func(m *CompactionMiddleware) { m.agent = name }
}

// WithKeepRecent sets how many trailing conversational events a summarizing
// compaction retains verbatim (Python parity: SummarizeCompact(target)). The
// window is aligned to a tool-round boundary afterwards — see snapCut — so the
// retained range may be shorter than n. n <= 0 keeps KeepRecent.
func WithKeepRecent(n int) Option {
	return func(m *CompactionMiddleware) { m.keep = n }
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

// NewTailWindow builds the Factory for the zero-LLM-cost rewrite: when the
// trigger fires, the history keeps only the last target conversational events
// and everything after them (Python parity: CompactTrigger + TailWindowCompact).
// No CompactionSummary is produced, so the turn pays nothing for compaction.
// A nil trigger yields a pass-through middleware.
func NewTailWindow(trigger Trigger, target int, opts ...Option) middleware.Factory {
	return func() middleware.Middleware {
		window := NewTailWindowCompact(target)
		m := &CompactionMiddleware{trigger: trigger, tail: &window}
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
	if m.trigger == nil || (m.strategy == nil && m.tail == nil) {
		return nil
	}
	evs, err := ctx.Stream.History().GetEvents()
	if err != nil {
		return fmt.Errorf("compact: read history: %w", err)
	}
	if !m.trigger.ShouldTrigger(evs, historyUsage(evs)) {
		return nil
	}
	if m.tail != nil {
		return m.tailCompact(ctx, evs)
	}
	keep := m.keep
	if keep <= 0 {
		keep = KeepRecent
	}
	cut := snapCut(evs, cutIndex(evs, keep))
	if cut <= 0 || cut >= len(evs) {
		// Fewer than keep conversational events, nothing before the fresh range,
		// or no replayable boundary left to cut at: there is no old span worth a
		// summary (Python parity: _cut_for_target returning 0, or an empty
		// retained range, is a no-op).
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
		if u, ok := usage.EventFrom(ev); ok {
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
	// Retain every source accounting record from before the rewrite, not just
	// the records inside the summarized window: spend that happened earlier must
	// stay reportable after compaction (upstream _with_usage_events). The
	// retained tail contributes its non-usage events below, so a usage record is
	// never present twice; the ids make the report idempotent even if it is.
	seenUsage := map[string]struct{}{}
	replacement = retainUsage(replacement, evs, seenUsage)
	replacement = append(replacement, &events.CompactionSummary{Content: summary})
	for _, ev := range evs[cut:] {
		if _, isUsage := usage.EventFrom(ev); isUsage {
			continue
		}
		replacement = append(replacement, ev)
	}
	for _, ue := range spent {
		if ue.EventID != "" {
			if _, dup := seenUsage[ue.EventID]; dup {
				continue
			}
			seenUsage[ue.EventID] = struct{}{}
		}
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

// tailCompact applies the zero-cost window: the old span disappears, the last
// Target conversational events and everything after them stay. Source usage
// records from before the cut are preserved so reporting stays whole (the same
// rule as the summarizing path). No summary event is produced.
//
// AGO-DEVIATION: Python's TailWindowCompact returns the retained tail alone, so
// source accounting records before the cut disappear with it. AGo keeps them
// because the event log is the usage report's source of truth (see usage.FromEvents).
func (m *CompactionMiddleware) tailCompact(ctx *agocontext.Context, evs []events.Event) error {
	retained := m.tail.Compact(evs)
	if len(retained) == len(evs) {
		// The window already covers the history: nothing to drop.
		return nil
	}
	name := typeLabel(*m.tail)
	if err := ctx.Send(&events.CompactionStarted{Agent: m.agent, Strategy: name, EventCount: len(evs)}); err != nil {
		return fmt.Errorf("compact: send CompactionStarted: %w", err)
	}
	seenUsage := map[string]struct{}{}
	replacement := retainUsage(make([]events.Event, 0, len(evs)), evs, seenUsage)
	for _, ev := range retained {
		if _, isUsage := usage.EventFrom(ev); isUsage {
			continue
		}
		replacement = append(replacement, ev)
	}
	if err := ctx.Stream.History().Replace(replacement); err != nil {
		return fmt.Errorf("compact: replace history: %w", err)
	}
	done := &events.CompactionCompleted{
		Agent:        m.agent,
		Strategy:     name,
		EventsBefore: len(evs),
		EventsAfter:  len(replacement),
		LLMCalls:     0,
	}
	if err := ctx.Send(done); err != nil {
		return fmt.Errorf("compact: send CompactionCompleted: %w", err)
	}
	return nil
}

// retainUsage appends every source accounting record in evs to dst, skipping
// duplicates by source event id, and records the seen ids so a later tail append
// cannot repeat one.
func retainUsage(dst []events.Event, evs []events.Event, seen map[string]struct{}) []events.Event {
	for _, ev := range evs {
		u, ok := usage.EventFrom(ev)
		if !ok {
			continue
		}
		if u.EventID != "" {
			if _, dup := seen[u.EventID]; dup {
				continue
			}
			seen[u.EventID] = struct{}{}
		}
		dst = append(dst, ev)
	}
	return dst
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
