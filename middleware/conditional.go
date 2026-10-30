package middleware

import (
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// If wraps a factory with a condition: the inner middleware is applied to a hook
// only when the condition matches the hook's event; otherwise the hook goes
// straight to next (Python parity: ConditionalMiddleware evaluates the condition
// per hook).
//
// AGO-DEVIATION: Python evaluates the OnLLMCall condition against the turn's
// initial event captured when the factory runs. A Go Factory() takes no event, so
// the condition is evaluated on the last event of the history the hook receives —
// in the agent turn loop that is the event that triggered the call (the
// ModelRequest for the first call of a turn, ToolResultsEvent afterwards).
func If(cond events.Condition, f Factory) Factory {
	return func() Middleware {
		return &conditional{cond: cond, inner: f()}
	}
}

type conditional struct {
	Base
	cond  events.Condition
	inner Middleware
}

func (c *conditional) OnTurn(next TurnFunc, ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
	if c.matches(ev) {
		return c.inner.OnTurn(next, ev, ctx)
	}
	return next(ev, ctx)
}

func (c *conditional) OnLLMCall(next LLMCallFunc, evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
	if len(evs) > 0 && c.matches(evs[len(evs)-1]) {
		return c.inner.OnLLMCall(next, evs, ctx)
	}
	return next(evs, ctx)
}

func (c *conditional) OnToolExecution(next ToolExecFunc, ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
	if c.matches(ev) {
		return c.inner.OnToolExecution(next, ev, ctx)
	}
	return next(ev, ctx)
}

func (c *conditional) OnHumanInput(next HumanInputFunc, ev *events.HumanInputRequest, ctx *agocontext.Context) (*events.HumanMessage, error) {
	if c.matches(ev) {
		return c.inner.OnHumanInput(next, ev, ctx)
	}
	return next(ev, ctx)
}

// matches treats a nil condition as "always", so If(nil, f) degenerates to f.
func (c *conditional) matches(ev events.Event) bool {
	return c.cond == nil || c.cond(ev)
}

var _ Middleware = (*conditional)(nil)
