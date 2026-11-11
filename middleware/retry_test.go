package middleware

import (
	stdcontext "context"
	"errors"
	"testing"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// transient and permanent stand in for provider-side errors.
var (
	errTransient = errors.New("503 service unavailable")
	errPermanent = errors.New("400 bad request")
)

// scriptedLLM returns the scripted results in order and counts calls.
func scriptedLLM(calls *int, results ...error) LLMCallFunc {
	return func([]events.Event, *agocontext.Context) (*events.ModelResponse, error) {
		i := *calls
		*calls++
		if i < len(results) && results[i] != nil {
			return nil, results[i]
		}
		return &events.ModelResponse{Message: &events.ModelMessage{Content: "ok"}}, nil
	}
}

func isTransient(err error) bool { return errors.Is(err, errTransient) }

// TestRetryOnFiltersPermanentErrors pins retry_on: an error the filter rejects
// is returned after a single paid call instead of being retried.
func TestRetryOnFiltersPermanentErrors(t *testing.T) {
	calls := 0
	m := Retry(3, WithRetryOn(isTransient))()
	_, err := m.OnLLMCall(scriptedLLM(&calls, errPermanent), nil, newTestCtx(t))
	if !errors.Is(err, errPermanent) {
		t.Fatalf("err = %v, want the permanent error", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (a rejected error is not retried)", calls)
	}
}

// TestRetryOnRetriesMatchingErrors pins the other side: a transient error is
// retried up to the attempt budget and a later success wins.
func TestRetryOnRetriesMatchingErrors(t *testing.T) {
	calls := 0
	m := Retry(3, WithRetryOn(isTransient))()
	resp, err := m.OnLLMCall(scriptedLLM(&calls, errTransient, errTransient), nil, newTestCtx(t))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if resp == nil || resp.Content() != "ok" {
		t.Fatalf("resp = %+v", resp)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

// TestRetryKeepsTotalAttempts pins the pre-existing constructor: its argument
// stays the total number of calls, so old call sites keep their meaning.
func TestRetryKeepsTotalAttempts(t *testing.T) {
	calls := 0
	m := Retry(2)()
	if _, err := m.OnLLMCall(scriptedLLM(&calls, errPermanent, errPermanent), nil, newTestCtx(t)); !errors.Is(err, errPermanent) {
		t.Fatalf("err = %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

// TestRetryMaxRetriesMatchesPython pins AG2 parity: RetryMiddleware(max_retries=N)
// makes N retried calls plus one final call, so N=2 gives three calls.
func TestRetryMaxRetriesMatchesPython(t *testing.T) {
	calls := 0
	m := RetryMaxRetries(2)()
	if _, err := m.OnLLMCall(scriptedLLM(&calls, errPermanent, errPermanent, errPermanent), nil, newTestCtx(t)); !errors.Is(err, errPermanent) {
		t.Fatalf("err = %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3 (max_retries=2 means 3 calls)", calls)
	}
}

// TestRetryMaxRetriesZeroIsOneCall pins the boundary: no retries means exactly
// one call, matching Python's max_retries=0.
func TestRetryMaxRetriesZeroIsOneCall(t *testing.T) {
	calls := 0
	m := RetryMaxRetries(0)()
	_, _ = m.OnLLMCall(scriptedLLM(&calls, errPermanent), nil, newTestCtx(t))
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

// TestRetryMaxRetriesDoesNotSleep pins Python parity for pacing: the retry
// constructor waits nothing between attempts, unlike Retry's linear backoff.
// Three failing calls with the default 100 ms base would take >= 100 ms if any
// sleep ran.
func TestRetryMaxRetriesDoesNotSleep(t *testing.T) {
	calls := 0
	m := RetryMaxRetries(2)()
	start := time.Now()
	if _, err := m.OnLLMCall(scriptedLLM(&calls, errPermanent, errPermanent, errPermanent), nil, newTestCtx(t)); !errors.Is(err, errPermanent) {
		t.Fatalf("err = %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("RetryMaxRetries slept %s between attempts, want none", elapsed)
	}
}

// TestRetryNeverRetriesCancellation pins the pre-existing guard: a cancelled
// call is not retried even when the filter accepts everything.
func TestRetryNeverRetriesCancellation(t *testing.T) {
	calls := 0
	m := RetryMaxRetries(3, WithRetryOn(func(error) bool { return true }))()
	_, err := m.OnLLMCall(scriptedLLM(&calls, stdcontext.Canceled), nil, newTestCtx(t))
	if !errors.Is(err, stdcontext.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}
