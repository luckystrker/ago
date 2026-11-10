package observers

import (
	"errors"
	"testing"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
)

func observerCtx(t *testing.T) *agocontext.Context {
	t.Helper()
	return agocontext.New(stream.NewMemoryStream())
}

// TestStreamObserverInterruptRunsFirst pins interrupt=: the observer sees the
// event before a normal subscriber, even though it registered later.
func TestStreamObserverInterruptRunsFirst(t *testing.T) {
	ctx := observerCtx(t)
	order := []string{}
	ctx.Stream.Subscribe(func(events.Event) error {
		order = append(order, "normal")
		return nil
	}, stream.SubscribeOptions{})
	obs := NewStreamObserver("first", nil, func(events.Event) {
		order = append(order, "observer")
	}, WithInterrupt())
	if err := obs.Register(ctx); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := ctx.Send(&events.ModelResponse{Message: &events.ModelMessage{Content: "x"}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(order) != 2 || order[0] != "observer" || order[1] != "normal" {
		t.Fatalf("order = %v, want observer before normal", order)
	}
}

// TestStreamObserverTimeoutIsEnforced pins timeout=: a hung callback surfaces as
// ErrSubscriberTimeout from the dispatching Send. The callback is released at
// the end so its goroutine does not outlive the test.
func TestStreamObserverTimeoutIsEnforced(t *testing.T) {
	ctx := observerCtx(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	obs := NewStreamObserver("slow", nil, func(events.Event) {
		<-release
	}, WithTimeout(20*time.Millisecond))
	if err := obs.Register(ctx); err != nil {
		t.Fatalf("Register: %v", err)
	}

	err := ctx.Send(&events.ModelResponse{Message: &events.ModelMessage{Content: "x"}})
	if !errors.Is(err, stream.ErrSubscriberTimeout) {
		t.Fatalf("Send error = %v, want ErrSubscriberTimeout", err)
	}
}

// TestStreamObserverWithoutOptionsIsUnbounded pins the zero value: no deadline,
// no interrupt priority.
func TestStreamObserverWithoutOptionsIsUnbounded(t *testing.T) {
	ctx := observerCtx(t)
	order := []string{}
	ctx.Stream.Subscribe(func(events.Event) error { order = append(order, "normal"); return nil }, stream.SubscribeOptions{})
	obs := NewStreamObserver("plain", nil, func(events.Event) { order = append(order, "observer") })
	if err := obs.Register(ctx); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := ctx.Send(&events.ModelResponse{Message: &events.ModelMessage{Content: "x"}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(order) != 2 || order[0] != "normal" {
		t.Fatalf("order = %v, want the observer after the normal subscriber", order)
	}
}
