package stream

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/ago/events"
)

// TestSubscriberTimeoutStopsDelivery pins Python parity for
// subscribe(timeout=): a handler that outlives its deadline surfaces as
// ErrSubscriberTimeout from Send, and later handlers do not see the event.
// The blocked handler is released at the end so its goroutine does not outlive
// the test.
func TestSubscriberTimeoutStopsDelivery(t *testing.T) {
	s := NewMemoryStream()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	s.Subscribe(func(events.Event) error {
		<-release
		return nil
	}, SubscribeOptions{Timeout: 20 * time.Millisecond})

	later := false
	s.Subscribe(func(events.Event) error {
		later = true
		return nil
	}, SubscribeOptions{})

	start := time.Now()
	err := s.Send(&events.ModelResponse{Message: &events.ModelMessage{Content: "x"}})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrSubscriberTimeout) {
		t.Fatalf("Send error = %v, want ErrSubscriberTimeout", err)
	}
	if elapsed > time.Second {
		t.Fatalf("Send took %s, want it bounded by the deadline", elapsed)
	}
	if later {
		t.Fatal("a timed-out handler must stop delivery to later handlers")
	}
}

// TestSubscriberTimeoutInterruptStopsDelivery pins the same rule for interrupt
// subscribers: the error aborts the send before normal delivery starts.
func TestSubscriberTimeoutInterruptStopsDelivery(t *testing.T) {
	s := NewMemoryStream()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	s.Subscribe(func(events.Event) error {
		<-release
		return nil
	}, SubscribeOptions{Interrupt: true, Timeout: 20 * time.Millisecond})

	persisted := false
	s.Subscribe(func(events.Event) error {
		persisted = true
		return nil
	}, SubscribeOptions{})

	err := s.Send(&events.ModelResponse{Message: &events.ModelMessage{Content: "x"}})
	if !errors.Is(err, ErrSubscriberTimeout) {
		t.Fatalf("Send error = %v, want ErrSubscriberTimeout", err)
	}
	if persisted {
		t.Fatal("a timed-out interrupter must stop the send")
	}
	if evs, _ := s.History().GetEvents(); len(evs) != 0 {
		t.Fatalf("a timed-out send persisted %d events", len(evs))
	}
}

// TestSubscriberPanicWithTimeoutBecomesError pins the recover inside
// runSubscriber: a panicking handler bounded by a timeout surfaces as an error
// from Send instead of crashing the process from another goroutine.
func TestSubscriberPanicWithTimeoutBecomesError(t *testing.T) {
	s := NewMemoryStream()
	s.Subscribe(func(events.Event) error { panic("boom") }, SubscribeOptions{Timeout: time.Second})

	err := s.Send(&events.ModelResponse{Message: &events.ModelMessage{Content: "x"}})
	if err == nil {
		t.Fatal("Send returned nil for a panicking handler, want an error")
	}
	if !strings.Contains(err.Error(), "panicked") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Send error = %q, want the panic reported", err)
	}
}

// TestSubscriberPanicWithoutTimeoutPropagates pins the other half of the
// documented contract: without a deadline the handler runs inline, so its panic
// reaches the caller of Send.
func TestSubscriberPanicWithoutTimeoutPropagates(t *testing.T) {
	s := NewMemoryStream()
	s.Subscribe(func(events.Event) error { panic("boom") }, SubscribeOptions{})

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Send swallowed a panic from an unbounded handler")
		}
	}()
	_ = s.Send(&events.ModelResponse{Message: &events.ModelMessage{Content: "x"}})
}

// TestSubscriberWithinTimeoutDelivers is the control: a handler that returns
// inside its deadline behaves exactly like an unbounded one.
func TestSubscriberWithinTimeoutDelivers(t *testing.T) {
	s := NewMemoryStream()
	calls := 0
	s.Subscribe(func(events.Event) error {
		calls++
		return nil
	}, SubscribeOptions{Timeout: time.Second})

	if err := s.Send(&events.ModelResponse{Message: &events.ModelMessage{Content: "x"}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if calls != 1 {
		t.Fatalf("handler ran %d times, want 1", calls)
	}
}

// TestSubscriberTimeoutZeroWaits pins the zero value: no deadline is applied,
// so the pre-existing unbounded behaviour is unchanged.
func TestSubscriberTimeoutZeroWaits(t *testing.T) {
	s := NewMemoryStream()
	got := make(chan struct{})
	s.Subscribe(func(events.Event) error {
		time.Sleep(20 * time.Millisecond)
		close(got)
		return nil
	}, SubscribeOptions{})

	if err := s.Send(&events.ModelResponse{Message: &events.ModelMessage{Content: "x"}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case <-got:
	default:
		t.Fatal("handler did not finish before Send returned")
	}
}
