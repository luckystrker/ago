package middleware

import (
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// Chain composes factories into one onion: the first factory is the outermost
// layer, the last the innermost (Python parity: the agent wires Assembler →
// compaction → aggregation → user middleware → LLM client, outermost first).
// Invoking the chained factory instantiates every layer once and routes each hook
// through them in order.
func Chain(fs ...Factory) Factory {
	return func() Middleware {
		mws := make([]Middleware, len(fs))
		for i, f := range fs {
			mws[i] = f()
		}
		return &chain{mws: mws}
	}
}

type chain struct{ mws []Middleware }

func (c *chain) OnTurn(next TurnFunc, ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
	call := next
	for i := len(c.mws) - 1; i >= 0; i-- {
		mw, inner := c.mws[i], call
		call = func(ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
			return mw.OnTurn(inner, ev, ctx)
		}
	}
	return call(ev, ctx)
}

func (c *chain) OnLLMCall(next LLMCallFunc, evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
	call := next
	for i := len(c.mws) - 1; i >= 0; i-- {
		mw, inner := c.mws[i], call
		call = func(evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
			return mw.OnLLMCall(inner, evs, ctx)
		}
	}
	return call(evs, ctx)
}

func (c *chain) OnToolExecution(next ToolExecFunc, ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
	call := next
	for i := len(c.mws) - 1; i >= 0; i-- {
		mw, inner := c.mws[i], call
		call = func(ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
			return mw.OnToolExecution(inner, ev, ctx)
		}
	}
	return call(ev, ctx)
}

func (c *chain) OnHumanInput(next HumanInputFunc, ev *events.HumanInputRequest, ctx *agocontext.Context) (*events.HumanMessage, error) {
	call := next
	for i := len(c.mws) - 1; i >= 0; i-- {
		mw, inner := c.mws[i], call
		call = func(ev *events.HumanInputRequest, ctx *agocontext.Context) (*events.HumanMessage, error) {
			return mw.OnHumanInput(inner, ev, ctx)
		}
	}
	return call(ev, ctx)
}

var _ Middleware = (*chain)(nil)
