package hitl

import (
	"context"
	"errors"
	"testing"

	"ago/events"
)

func TestNotProvidedError(t *testing.T) {
	if got := (&NotProvidedError{}).Error(); got != "hitl: no human input hook provided" {
		t.Errorf("bare error = %q", got)
	}
	if got := (&NotProvidedError{Detail: "ask"}).Error(); got != "hitl: no human input hook provided: ask" {
		t.Errorf("detailed error = %q", got)
	}
	if !errors.Is(&NotProvidedError{Detail: "x"}, ErrNotProvided) {
		t.Error("NotProvidedError must unwrap to ErrNotProvided")
	}
}

func TestFailedError(t *testing.T) {
	if got := (&FailedError{}).Error(); got != "hitl: human input failed" {
		t.Errorf("bare error = %q", got)
	}
	cause := errors.New("websocket closed")
	fe := &FailedError{Cause: cause}
	if got := fe.Error(); got != "hitl: human input failed: websocket closed" {
		t.Errorf("caused error = %q", got)
	}
	if fe.Unwrap() != cause {
		t.Errorf("Unwrap = %v, want the cause", fe.Unwrap())
	}
	if !errors.Is(fe, cause) {
		t.Error("errors.Is must reach the cause")
	}
	// A sentinel buried in the cause chain stays reachable.
	wrapped := &FailedError{Cause: fmtWrapped{ErrNotProvided}}
	if !errors.Is(wrapped, ErrNotProvided) {
		t.Error("errors.Is must traverse the wrapped cause")
	}
}

// fmtWrapped stands in for fmt.Errorf("%w", ...) output in the cause chain.
type fmtWrapped struct{ error }

func (w fmtWrapped) Unwrap() error { return w.error }
func (w fmtWrapped) Error() string { return "wrapped: " + w.error.Error() }

func TestTimeoutError(t *testing.T) {
	tests := []struct {
		timeout float64
		want    string
	}{
		{1.5, "hitl: human input timed out after 1.5s"},
		{0.5, "hitl: human input timed out after 0.5s"},
	}
	for _, tt := range tests {
		if got := (&TimeoutError{Timeout: tt.timeout}).Error(); got != tt.want {
			t.Errorf("TimeoutError(%v) = %q, want %q", tt.timeout, got, tt.want)
		}
	}
	// No Unwrap: a timeout is not a missing hook.
	if errors.Is(&TimeoutError{Timeout: 1}, ErrNotProvided) {
		t.Error("TimeoutError must not match ErrNotProvided")
	}
}

func TestDefaultHook(t *testing.T) {
	msg, err := DefaultHook(context.Background(), &events.HumanInputRequest{Content: "approve?"})
	if msg != nil {
		t.Errorf("message = %v, want nil", msg)
	}
	if !errors.Is(err, ErrNotProvided) {
		t.Errorf("err = %v, want ErrNotProvided", err)
	}
	if want := `hitl: no human input hook provided: request "approve?"`; err.Error() != want {
		t.Errorf("err = %q, want %q", err.Error(), want)
	}
}
