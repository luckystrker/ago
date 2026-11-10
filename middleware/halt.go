package middleware

import (
	"sync"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
)

// HaltCheck returns the middleware that short-circuits model calls after a
// HaltEvent arrives during the turn (Python parity: HaltCheckMiddleware). It
// answers "HALTED: <reason>" instead of reaching the model, so a FATAL
// ObserverAlert turned into a HaltEvent by policies.AlertPolicy stops the very
// model call during which it was sent.
//
// Register it after the layer that can emit the halt (an assembly/alert
// middleware), so the halt fires inside the same call it is meant to stop. The
// agent's own halt guard only covers calls that start after the halt.
//
// The subscription is scoped to one turn and removed even when the turn fails or
// is cancelled, so a later turn on the same stream is not halted by an older
// HaltEvent. A Factory builds a fresh instance per turn; the instance is safe
// for concurrent use.
func HaltCheck() Factory {
	return func() Middleware { return &haltCheck{} }
}

type haltCheck struct {
	Base
	mu     sync.Mutex
	halted bool
	reason string
}

var _ Middleware = (*haltCheck)(nil)

// OnTurn brackets the turn with the halt subscription (Python parity: subscribe
// in on_turn, unsubscribe in the finally block). Without a stream there is
// nothing to observe, so the turn runs unwrapped.
func (h *haltCheck) OnTurn(next TurnFunc, ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
	if ctx == nil || ctx.Stream == nil {
		return next(ev, ctx)
	}
	sub := ctx.Stream.Subscribe(func(e events.Event) error {
		halt, ok := events.AsValue(e).(events.HaltEvent)
		if !ok {
			return nil
		}
		h.mu.Lock()
		h.halted, h.reason = true, halt.Reason
		h.mu.Unlock()
		return nil
	}, stream.SubscribeOptions{Condition: events.TypeOf[events.HaltEvent]()})
	defer ctx.Stream.Unsubscribe(sub)
	return next(ev, ctx)
}

// OnLLMCall short-circuits every model call after the halt (Python parity: the
// synthetic "HALTED: <reason>" ModelResponse).
func (h *haltCheck) OnLLMCall(next LLMCallFunc, evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
	h.mu.Lock()
	halted, reason := h.halted, h.reason
	h.mu.Unlock()
	if halted {
		return &events.ModelResponse{Message: &events.ModelMessage{Content: "HALTED: " + reason}}, nil
	}
	return next(evs, ctx)
}
