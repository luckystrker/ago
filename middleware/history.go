package middleware

import (
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// HistoryLimiter truncates the history passed to each LLM call to the last n
// non-transient events (Python parity: middleware/builtin/history_limiter.py).
// Transient events (streaming chunks, reasoning deltas, bookkeeping) are dropped
// and do not count toward n.
//
// AGO-DEVIATION: Python re-attaches a leading ModelRequest anchor and widens the
// cut through replayable_span so tool-call/result pairs are never orphaned; the
// AGo contract keeps a plain suffix — history shaping and pair repair stay with
// the agent. A limit below 1 behaves as 1 (Python raises ValueError; a Factory
// has no error return).
func HistoryLimiter(n int) Factory {
	if n < 1 {
		n = 1
	}
	return func() Middleware {
		return &historyLimiter{max: n}
	}
}

type historyLimiter struct {
	Base
	max int
}

func (h *historyLimiter) OnLLMCall(next LLMCallFunc, evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
	kept := make([]events.Event, 0, len(evs))
	for _, ev := range evs {
		if ev == nil || ev.Transient() {
			continue
		}
		kept = append(kept, ev)
	}
	if len(kept) > h.max {
		kept = kept[len(kept)-h.max:]
	}
	return next(kept, ctx)
}

var _ Middleware = (*historyLimiter)(nil)

// TokenLimiter truncates the history to fit a token budget, always retaining a
// suffix of the most recent events (Python parity: token_limiter.py). Events are
// sized with events.EstimatedTokens (default chars-per-token estimate).
//
// The most recent event is always kept even when it alone exceeds the budget, so a
// call is never made with empty history. AGO-DEVIATION: as with HistoryLimiter,
// Python re-attaches a leading ModelRequest anchor and widens via replayable_span;
// AGo keeps a plain suffix. A budget below 1 behaves as 1.
func TokenLimiter(budget int) Factory {
	if budget < 1 {
		budget = 1
	}
	return func() Middleware {
		return &tokenLimiter{budget: budget}
	}
}

type tokenLimiter struct {
	Base
	budget int
}

func (t *tokenLimiter) OnLLMCall(next LLMCallFunc, evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
	if len(evs) == 0 {
		return next(evs, ctx)
	}
	tokens := make([]int, len(evs))
	total := 0
	for i, ev := range evs {
		tokens[i] = events.EstimatedTokens(ev, 0)
		total += tokens[i]
	}
	if total <= t.budget {
		return next(evs, ctx)
	}
	// Walk backwards accumulating events while they fit; the newest event is kept
	// unconditionally (Python parity: "always preserve the most recent event").
	start := len(evs) - 1
	used := 0
	for i := len(evs) - 1; i >= 0; i-- {
		if i < len(evs)-1 && used+tokens[i] > t.budget {
			break
		}
		used += tokens[i]
		start = i
	}
	return next(evs[start:], ctx)
}

var _ Middleware = (*tokenLimiter)(nil)
