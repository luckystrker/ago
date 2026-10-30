// Package middleware carries the four-hook onion chain wrapped around an agent
// turn: OnTurn (the whole turn), OnLLMCall (each model invocation),
// OnToolExecution (each tool call) and OnHumanInput (each HITL request).
//
// Parity with Python AG2 ag2/middleware/base.py: a Factory builds one Middleware
// instance per turn; Base supplies pass-through defaults for every hook, so
// implementations embed it and override only the hooks they need. Builtins live in
// this package too (history/limiters, retry, logging, approval).
package middleware

import (
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// TurnFunc is the rest of the turn pipeline below an OnTurn hook.
type TurnFunc func(ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error)

// LLMCallFunc is the rest of the pipeline below an OnLLMCall hook: the model
// invocation over the given history.
type LLMCallFunc func(evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error)

// ToolExecFunc is the rest of the pipeline below an OnToolExecution hook. It
// returns the event answering the call: ToolResultEvent, ToolErrorEvent or
// ClientToolCallEvent.
type ToolExecFunc func(ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error)

// HumanInputFunc is the rest of the pipeline below an OnHumanInput hook.
type HumanInputFunc func(ev *events.HumanInputRequest, ctx *agocontext.Context) (*events.HumanMessage, error)

// Middleware is one onion layer around the agent turn. Every hook must call next
// (possibly with modified arguments) unless it deliberately short-circuits the
// pipeline. Implementations must be safe for concurrent use: the tool executor may
// run OnToolExecution hooks of one instance from several goroutines.
type Middleware interface {
	OnTurn(next TurnFunc, ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error)
	OnLLMCall(next LLMCallFunc, evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error)
	OnToolExecution(next ToolExecFunc, ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error)
	OnHumanInput(next HumanInputFunc, ev *events.HumanInputRequest, ctx *agocontext.Context) (*events.HumanMessage, error)
}

// Factory builds a Middleware instance per turn. Factories are shared across turns;
// instances may carry per-turn state (see Approval).
type Factory func() Middleware

// Base supplies the pass-through default for every hook: each returns next(...).
// Embed it and override only the hooks you implement.
type Base struct{}

func (Base) OnTurn(next TurnFunc, ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
	return next(ev, ctx)
}

func (Base) OnLLMCall(next LLMCallFunc, evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
	return next(evs, ctx)
}

func (Base) OnToolExecution(next ToolExecFunc, ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
	return next(ev, ctx)
}

func (Base) OnHumanInput(next HumanInputFunc, ev *events.HumanInputRequest, ctx *agocontext.Context) (*events.HumanMessage, error) {
	return next(ev, ctx)
}

var _ Middleware = Base{}
