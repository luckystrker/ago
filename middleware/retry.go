package middleware

import (
	"context"
	"errors"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// retryBaseDelay is the wait before the first retry; each further retry waits one
// more multiple (linear backoff). A var so tests and hosts can shorten it.
var retryBaseDelay = 100 * time.Millisecond

// Retry repeats a failing LLM call, making at most attempts calls in total, with
// linear backoff (retryBaseDelay × attempt number). Only OnLLMCall is wrapped
// (Python parity: RetryMiddleware); other hooks pass through. attempts below 1
// performs a single attempt.
//
// Context cancellation is never retried: an error wrapping context.Canceled or
// context.DeadlineExceeded returns immediately.
//
// AGO-DEVIATION: middleware hooks carry no stdlib context, so cancellation is
// detected through the error value only, and the backoff sleep is not
// interruptible by a canceled turn. Python additionally refuses to retry an
// attempt that already published stream events (duplicate-output guard); AGo omits
// that guard — the agent turn loop owns stream consistency.
func Retry(attempts int) Factory {
	if attempts < 1 {
		attempts = 1
	}
	return func() Middleware {
		return &retry{attempts: attempts}
	}
}

type retry struct {
	Base
	attempts int
}

func (r *retry) OnLLMCall(next LLMCallFunc, evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
	var resp *events.ModelResponse
	var err error
	for attempt := 0; attempt < r.attempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * retryBaseDelay)
		}
		resp, err = next(evs, ctx)
		if err == nil {
			return resp, nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
	}
	return nil, err
}

var _ Middleware = (*retry)(nil)
