// Package hitl carries the human-in-the-loop contract: a HumanHook answers
// HumanInputRequests (questions, tool approvals) on behalf of a human operator.
//
// Parity with Python AG2 ag2/hitl.py. AGO-DEVIATION: the hook is stored on the
// conversation context and invoked directly (Python routes it through a stream
// subscription); the observable behavior — one answer per request, normalized
// errors — is identical. See docs/DECISIONS.md.
package hitl

import (
	"context"
	"errors"
	"fmt"

	"ago/events"
)

// HumanHook answers a human input request. Return the answer message or an error.
type HumanHook func(ctx context.Context, req *events.HumanInputRequest) (*events.HumanMessage, error)

// DefaultHook is used when no hook is configured: it refuses every request.
func DefaultHook(ctx context.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
	return nil, fmt.Errorf("%w: request %q", ErrNotProvided, req.Content)
}

var ErrNotProvided = errors.New("hitl: no human input hook provided")

// NotProvidedError reports that no human input is possible for this conversation.
type NotProvidedError struct{ Detail string }

func (e *NotProvidedError) Error() string {
	if e.Detail == "" {
		return ErrNotProvided.Error()
	}
	return ErrNotProvided.Error() + ": " + e.Detail
}
func (e *NotProvidedError) Unwrap() error { return ErrNotProvided }

// FailedError reports that the human input channel itself failed.
type FailedError struct{ Cause error }

func (e *FailedError) Error() string {
	if e.Cause == nil {
		return "hitl: human input failed"
	}
	return "hitl: human input failed: " + e.Cause.Error()
}
func (e *FailedError) Unwrap() error { return e.Cause }

// TimeoutError reports that the human did not answer in time.
type TimeoutError struct{ Timeout float64 }

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("hitl: human input timed out after %gs", e.Timeout)
}
