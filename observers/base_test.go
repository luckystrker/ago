package observers

import (
	"errors"
	"sync"
	"testing"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// batchRecorder records every batch a ProcessFunc receives.
type batchRecorder struct {
	mu      sync.Mutex
	batches [][]events.Event
}

func (r *batchRecorder) process(_ *agocontext.Context, evs []events.Event) (*events.ObserverAlert, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batches = append(r.batches, append([]events.Event(nil), evs...))
	return nil, nil
}

func (r *batchRecorder) snapshot() [][]events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]events.Event(nil), r.batches...)
}

// TestBaseObserverProcessesEachEventByDefault pins the zero value: no batching.
func TestBaseObserverProcessesEachEventByDefault(t *testing.T) {
	s, cx := newConversation()
	rec := &batchRecorder{}
	obs := NewBaseObserver("plain", rec.process, WithObserverCondition(events.TypeOf[events.TextInput]()))
	if err := obs.Register(cx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, text := range []string{"a", "b", "c"} {
		if err := s.Send(events.TextInput{Content: text}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	batches := rec.snapshot()
	if len(batches) != 3 {
		t.Fatalf("batches = %d, want 3", len(batches))
	}
	for i, b := range batches {
		if len(b) != 1 {
			t.Fatalf("batch %d = %v, want one event", i, b)
		}
	}
}

// TestBaseObserverBatchSize pins the count trigger.
func TestBaseObserverBatchSize(t *testing.T) {
	s, cx := newConversation()
	rec := &batchRecorder{}
	obs := NewBaseObserver("counted", rec.process, WithBatchSize(3), WithObserverCondition(events.TypeOf[events.TextInput]()))
	if err := obs.Register(cx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, text := range []string{"a", "b", "c", "d", "e", "f"} {
		if err := s.Send(events.TextInput{Content: text}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	batches := rec.snapshot()
	if len(batches) != 2 {
		t.Fatalf("batches = %d, want 2", len(batches))
	}
	for i, want := range []int{3, 3} {
		if len(batches[i]) != want {
			t.Fatalf("batch %d has %d events, want %d", i, len(batches[i]), want)
		}
	}
}

// TestBaseObserverBatchWindow pins the time trigger: events arriving inside the
// window are processed together once it expires.
func TestBaseObserverBatchWindow(t *testing.T) {
	s, cx := newConversation()
	rec := &batchRecorder{}
	obs := NewBaseObserver("windowed", rec.process, WithBatchWindow(40*time.Millisecond), WithObserverCondition(events.TypeOf[events.TextInput]()))
	if err := obs.Register(cx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Send(events.TextInput{Content: "a"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := s.Send(events.TextInput{Content: "b"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("processed %d batches before the window expired", len(got))
	}
	deadline := time.Now().Add(time.Second)
	for {
		if batches := rec.snapshot(); len(batches) == 1 {
			if len(batches[0]) != 2 {
				t.Fatalf("batch = %v, want both buffered events", batches[0])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("window batch was not processed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestBaseObserverSendsAlert pins the alert path and its source.
func TestBaseObserverSendsAlert(t *testing.T) {
	s, cx := newConversation()
	alerts := collectAlerts(s)
	obs := NewBaseObserver("alerter", func(*agocontext.Context, []events.Event) (*events.ObserverAlert, error) {
		return &events.ObserverAlert{Source: "alerter", Severity: events.SeverityWarning, Message: "heads up"}, nil
	}, WithObserverCondition(events.TypeOf[events.TextInput]()))
	if err := obs.Register(cx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Send(events.TextInput{Content: "trigger"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := alerts()
	if len(got) != 1 || got[0].Message != "heads up" {
		t.Fatalf("alerts = %v, want one 'heads up'", got)
	}
}

// TestBaseObserverProcessErrorIsSwallowed pins Python parity: a process error is
// logged, never turned into a Send failure.
func TestBaseObserverProcessErrorIsSwallowed(t *testing.T) {
	s, cx := newConversation()
	obs := NewBaseObserver("broken", func(*agocontext.Context, []events.Event) (*events.ObserverAlert, error) {
		return nil, errors.New("process failed")
	}, WithObserverCondition(events.TypeOf[events.TextInput]()))
	if err := obs.Register(cx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Send(events.TextInput{Content: "x"}); err != nil {
		t.Fatalf("Send must not fail on a process error: %v", err)
	}
}

// TestBaseObserverProcessTimeout pins the per-call deadline: the dispatching
// Send is released and no alert is emitted.
func TestBaseObserverProcessTimeout(t *testing.T) {
	s, cx := newConversation()
	alerts := collectAlerts(s)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	obs := NewBaseObserver("slow", func(*agocontext.Context, []events.Event) (*events.ObserverAlert, error) {
		<-release
		return &events.ObserverAlert{Source: "slow", Severity: events.SeverityWarning, Message: "late"}, nil
	}, WithBatchSize(1), WithProcessTimeout(20*time.Millisecond))
	if err := obs.Register(cx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	start := time.Now()
	if err := s.Send(events.TextInput{Content: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 15*time.Millisecond {
		t.Fatalf("Send returned after %v, want the 20ms deadline", elapsed)
	}
	if got := alerts(); len(got) != 0 {
		t.Fatalf("alerts = %v, want none after a timeout", got)
	}
}

// TestBaseObserverRegisterErrors pins the validation paths.
func TestBaseObserverRegisterErrors(t *testing.T) {
	if err := NewBaseObserver("nil", nil).Register(observerCtx(t)); !errors.Is(err, ErrNilProcess) {
		t.Fatalf("Register(nil process) = %v, want ErrNilProcess", err)
	}
	if err := NewBaseObserver("ctx", (&batchRecorder{}).process).Register(nil); !errors.Is(err, ErrNilContext) {
		t.Fatalf("Register(nil ctx) = %v, want ErrNilContext", err)
	}
}
