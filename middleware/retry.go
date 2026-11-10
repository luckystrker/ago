package middleware

import (
	"context"
	"errors"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// retryBaseDelay is the wait before the first retry; each further retry waits one
// more multiple (linear backoff). A var so tests and hosts can shorten it. Only
// Retry applies it; RetryMaxRetries follows Python and waits nothing.
var retryBaseDelay = 100 * time.Millisecond

// RetryOption configures Retry and RetryMaxRetries.
type RetryOption func(*retryOptions)

type retryOptions struct {
	// retryOn decides whether an error is transient; nil retries every error
	// except context cancellation (Python default: retry_on=(Exception,)).
	retryOn func(error) bool
}

// WithRetryOn restricts what is retried (Python parity: retry_on). An error the
// filter rejects propagates after the current call, so a permanent 4xx, an auth
// failure or a validation error no longer becomes a paid retry. Context
// cancellation is never retried, whatever the filter says.
//
// A nil filter has no effect (every non-cancellation error is retried).
func WithRetryOn(filter func(error) bool) RetryOption {
	return func(o *retryOptions) { o.retryOn = filter }
}

// Retry repeats a failing LLM call, making at most attempts calls in total, with
// linear backoff (retryBaseDelay × attempt number). Only OnLLMCall is wrapped
// (Python parity: RetryMiddleware); other hooks pass through. attempts below 1
// performs a single attempt.
//
// Context cancellation is never retried: an error wrapping context.Canceled or
// context.DeadlineExceeded returns immediately. WithRetryOn narrows what counts
// as transient.
//
// AGO-DEVIATION: middleware hooks carry no stdlib context, so cancellation is
// detected through the error value only, and the backoff sleep is not
// interruptible by a canceled turn. Python additionally refuses to retry an
// attempt that already published stream events (duplicate-output guard); AGo omits
// that guard — the agent turn loop owns stream consistency. Python's max_retries
// counts retries, not calls: use RetryMaxRetries for that arithmetic.
func Retry(attempts int, opts ...RetryOption) Factory {
	if attempts < 1 {
		attempts = 1
	}
	return newRetry(attempts, false, opts)
}

// RetryMaxRetries is the AG2-parity constructor: maxRetries retried attempts
// followed by one final call, so maxRetries=3 makes up to 4 model calls
// (Python parity: RetryMiddleware(max_retries=N)). Values below zero mean no
// retry. There is no backoff between attempts, matching Python; use Retry when a
// caller wants pacing.
func RetryMaxRetries(maxRetries int, opts ...RetryOption) Factory {
	if maxRetries < 0 {
		maxRetries = 0
	}
	return newRetry(maxRetries, true, opts)
}

func newRetry(n int, retriesOnly bool, opts []RetryOption) Factory {
	var o retryOptions
	for _, opt := range opts {
		opt(&o)
	}
	return func() Middleware {
		return &retry{attempts: n, retriesOnly: retriesOnly, filter: o.retryOn}
	}
}

type retry struct {
	Base
	// attempts is the number of calls for Retry and the number of retries for
	// RetryMaxRetries; retriesOnly selects which.
	attempts    int
	retriesOnly bool
	filter      func(error) bool
}

func (r *retry) OnLLMCall(next LLMCallFunc, evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
	calls := r.attempts
	if r.retriesOnly {
		calls++ // the final attempt sits outside Python's retry loop
	}
	var err error
	for attempt := 0; attempt < calls; attempt++ {
		if attempt > 0 && !r.retriesOnly {
			time.Sleep(time.Duration(attempt) * retryBaseDelay)
		}
		var resp *events.ModelResponse
		resp, err = next(evs, ctx)
		if err == nil {
			return resp, nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		if r.filter != nil && !r.filter(err) {
			return nil, err
		}
	}
	return nil, err
}

var _ Middleware = (*retry)(nil)
