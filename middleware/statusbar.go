package middleware

import (
	"fmt"
	"reflect"
	"runtime"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// StatusCallback computes the current status snapshot as input parts, once per
// model call (Python parity: the DI callback of statusbar_middleware). It reads
// its inputs from ctx.Dependencies / ctx.Variables — AGO-DEVIATION: Python
// resolves fast_depends annotations (Context, Inject(), Variable(), Depends())
// when building the callback; AGo's DI is a plain string-keyed map, so the
// callback reads the context itself (docs/DECISIONS.md D-080). Long-running
// work should honor ctx.TurnContext() for cancellation. Returning nil or an
// empty slice skips the call: nothing is computed, published or attached.
type StatusCallback func(ctx *agocontext.Context) ([]events.Input, error)

// StatusOption configures Statusbar.
type StatusOption func(*statusOptions)

type statusOptions struct {
	persist bool
}

// WithPersist publishes every non-empty snapshot as a durable
// events.StatusObservation before the model call, so later calls keep seeing
// the status even after the callback starts skipping. Without it the status
// lives only inside the single call it was computed for.
func WithPersist(p bool) StatusOption { return func(o *statusOptions) { o.persist = p } }

// Statusbar keeps a dynamic status in front of the model: on every LLM call the
// callback computes input parts, which are deep-copied and appended to the call
// history as a detached service view (events.StatusRequest). The parts never
// enter the transcript — the model sees them, the conversation does not.
//
// With WithPersist(true) each non-empty snapshot is additionally sent to the
// stream as a durable StatusObservation BEFORE the rest of the pipeline runs;
// the agent projects persisted observations into service views on every later
// call (events.ModelInputs), so the last committed status keeps riding the
// model input across calls, tool rounds and continuations. A persistence
// failure — like a callback error — stops the pipeline before the model call:
// no response is produced and nothing else is transcribed; a provider failure
// after a committed observation leaves that observation recorded.
//
// Register the factory OUTSIDE (before) Retry in the middleware list, so the
// callback runs once per logical call. Inside Retry it runs once per attempt,
// and WithPersist(true) then publishes one observation per attempt — AGo's
// Retry has no published-output guard (see retry.go), so duplicates are
// possible.
//
// A nil callback yields a pass-through middleware.
func Statusbar(cb StatusCallback, opts ...StatusOption) Factory {
	var o statusOptions
	for _, opt := range opts {
		opt(&o)
	}
	return func() Middleware {
		s := &statusbar{cb: cb, persist: o.persist}
		if cb != nil {
			s.source = runtime.FuncForPC(reflect.ValueOf(cb).Pointer()).Name()
		}
		return s
	}
}

type statusbar struct {
	Base
	cb      StatusCallback
	persist bool
	// source names the callback (StatusObservation.source parity); empty only
	// for a nil callback, which computes nothing.
	source string
}

func (s *statusbar) OnLLMCall(next LLMCallFunc, evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
	if s.cb == nil {
		return next(evs, ctx)
	}
	parts, err := s.cb(ctx)
	if err != nil {
		return nil, fmt.Errorf("middleware: statusbar: %w", err)
	}
	if len(parts) == 0 {
		return next(evs, ctx)
	}
	copied, err := events.CopyInputs(parts)
	if err != nil {
		return nil, fmt.Errorf("middleware: statusbar: %w", err)
	}
	var view *events.ModelRequest
	if s.persist {
		obs := &events.StatusObservation{Parts: copied, Source: s.source}
		// Commit the observation before the pipeline continues: a provider
		// failure below leaves the status recorded, and a storage failure
		// stops the call before it can produce any output.
		if err := ctx.Send(obs); err != nil {
			return nil, fmt.Errorf("middleware: statusbar: persist status: %w", err)
		}
		// The view is projected from the published observation — exactly what
		// the agent projects from history on every later call — so this call
		// and future calls see byte-identical projections.
		view, err = events.ObservationRequest(obs)
		if err != nil {
			return nil, fmt.Errorf("middleware: statusbar: %w", err)
		}
	} else {
		view = events.StatusRequest(copied)
	}
	out := make([]events.Event, 0, len(evs)+1)
	out = append(out, evs...)
	out = append(out, view)
	return next(out, ctx)
}

var _ Middleware = (*statusbar)(nil)
