// Package hitl carries the human-in-the-loop contract: a HumanHook answers
// HumanInputRequests (questions, tool approvals) on behalf of a human operator.
//
// Parity with Python AG2 ag2/hitl.py: the installed hook is handed each question
// through a stream subscription (Context.Ask routes HumanInputRequest events to
// it and waits for the answer on the stream), so the observable behavior — one
// answer per request, the answer on the transcript, normalized errors — matches.
// AGO-DEVIATION: Python cancels an in-flight hook at the request deadline via
// asyncio.wait_for; a Go hook cannot be cancelled, only abandoned — see
// docs/DECISIONS.md D-032.
package hitl

import (
	"context"
	"errors"
	"fmt"

	"github.com/luckystrker/ago/events"
)

// HumanHook answers a human input request. Return the answer message or an error.
type HumanHook func(ctx context.Context, req *events.HumanInputRequest) (*events.HumanMessage, error)

// DefaultHook is used when no hook is configured: it refuses every request.
func DefaultHook(ctx context.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
	return nil, fmt.Errorf("%w: request %q", ErrNotProvided, req.Content)
}

var ErrNotProvided = errors.New("hitl: no human input hook provided")

// IsError reports whether err is a human-in-the-loop failure (Python parity:
// HumanInputError and its subclasses). Such errors never become tool results:
// the tool layer lets them end the turn so the asking agent can bridge or
// re-raise them.
func IsError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNotProvided) {
		return true
	}
	var notProvided *NotProvidedError
	var failed *FailedError
	var timeout *TimeoutError
	return errors.As(err, &notProvided) || errors.As(err, &failed) || errors.As(err, &timeout)
}

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
