package aggregate

import (
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/usage"
)

// keepRecent is the number of most recent history events aggregation never
// touches: the tail of the transcript stays verbatim so the model keeps the
// latest turns in full (porting brief: keepRecent = 2).
const keepRecent = 2

// New returns a middleware.Factory for the aggregation pass. The pass runs
// post-turn: the turn completes normally first, then the history is folded.
func New(trigger Trigger, strategy Strategy) middleware.Factory {
	return func() middleware.Middleware {
		return &aggregationMiddleware{trigger: trigger, strategy: strategy}
	}
}

type aggregationMiddleware struct {
	middleware.Base
	trigger  Trigger
	strategy Strategy
}

// OnTurn runs the turn, then aggregates when the trigger fires. An aggregation
// failure is reported as AggregationFailed and leaves the history untouched —
// the turn's own result is returned unchanged, because a summarization failure
// must not fail the conversation.
func (m *aggregationMiddleware) OnTurn(next middleware.TurnFunc, ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
	resp, err := next(ev, ctx)
	if err != nil {
		return resp, err
	}
	m.maybeAggregate(ctx)
	return resp, nil
}

var _ middleware.Middleware = (*aggregationMiddleware)(nil)

// maybeAggregate checks the trigger and, when it fires, replaces the history:
// every CompactionSummary plus the conversational events older than the
// keepRecent tail collapse into one new CompactionSummary produced by the
// strategy. Non-conversational bookkeeping events (UsageEvent and the like) are
// preserved verbatim — usage reports read them from the history.
func (m *aggregationMiddleware) maybeAggregate(ctx *agocontext.Context) {
	hist, err := ctx.Stream.History().GetEvents()
	if err != nil {
		m.fail(ctx, "history_error", err)
		return
	}
	u := usage.FromEvents(hist).Total
	if !m.trigger.ShouldTrigger(hist, u) {
		return
	}
	summaries, superseded, bookkeeping := collect(hist)
	if len(summaries) == 0 && len(superseded) == 0 {
		// The trigger fired but there is nothing to fold (e.g. a usage threshold
		// reached on a short transcript); an empty summary would be noise.
		return
	}
	sendLifecycle(ctx, &events.AggregationStarted{})

	merged, err := m.strategy.Aggregate(ctx, summaries, superseded)
	if err != nil {
		m.fail(ctx, "strategy_error", err)
		return
	}

	// Replace only after a successful strategy call: a failure leaves the
	// history untouched.
	start := len(hist) - keepRecent
	if start < 0 {
		start = 0
	}
	newHist := make([]events.Event, 0, len(bookkeeping)+1+keepRecent)
	newHist = append(newHist, bookkeeping...)
	newHist = append(newHist, &events.CompactionSummary{Content: merged})
	newHist = append(newHist, hist[start:]...)
	if err := ctx.Stream.History().Replace(newHist); err != nil {
		m.fail(ctx, "history_error", err)
		return
	}

	compUsage, llmCalls := usageOf(m.strategy)
	sendLifecycle(ctx, &events.AggregationCompleted{
		EventsBefore: len(hist),
		EventsAfter:  len(newHist),
		LLMCalls:     llmCalls,
		Usage:        compUsage,
	})
}

// collect splits the history into summaries to merge, superseded conversational
// events to summarize, and bookkeeping events to keep. Summaries are taken from
// the whole history (the trigger counts them there too); conversational events
// only from before the keepRecent tail, which stays in the history verbatim.
func collect(hist []events.Event) (summaries []string, superseded, bookkeeping []events.Event) {
	cut := len(hist) - keepRecent
	if cut < 0 {
		cut = 0
	}
	for i, ev := range hist {
		if e, ok := events.AsValue(ev).(events.CompactionSummary); ok {
			summaries = append(summaries, e.Content)
			continue
		}
		if i >= cut {
			continue // the recent tail is kept verbatim
		}
		switch {
		case ev.Transient():
			// Streaming artifacts (persisted only under PersistAll) are dropped.
		case ev.Conversational():
			superseded = append(superseded, ev)
		default:
			bookkeeping = append(bookkeeping, ev)
		}
	}
	return summaries, superseded, bookkeeping
}

// usageOf reads the optional usage seam; strategies that do not implement it
// report zero usage and zero LLM calls.
func usageOf(s Strategy) (events.Usage, int) {
	if us, ok := s.(usageSource); ok {
		return us.usageOfLast()
	}
	return events.Usage{}, 0
}

func (m *aggregationMiddleware) fail(ctx *agocontext.Context, errorType string, err error) {
	sendLifecycle(ctx, &events.AggregationFailed{ErrorType: errorType, Error: err.Error()})
}

// sendLifecycle emits a transient lifecycle event. Failures are swallowed:
// lifecycle events are bookkeeping, and a broken subscriber must not break the
// aggregation or the turn.
func sendLifecycle(ctx *agocontext.Context, ev events.Event) {
	_ = ctx.Send(ev)
}
