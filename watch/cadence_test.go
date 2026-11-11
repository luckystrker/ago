package watch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
)

// cadenceBatch starts one WaitBatch and returns its result channel plus the
// subscription signal, so a test can send events only once the subscription is
// live.
func cadenceBatch(t *testing.T, w BatchWatch) <-chan []events.Event {
	t.Helper()
	out := make(chan []events.Event, 1)
	go func() {
		batch, err := w.WaitBatch(waitCtx(t))
		if err != nil {
			t.Errorf("WaitBatch: %v", err)
			return
		}
		out <- batch
	}()
	return out
}

func newCadence(t *testing.T, n int, maxWait time.Duration, opts ...CadenceOption) *cadenceWatch {
	t.Helper()
	w, err := CadenceWatch(stream.NewMemoryStream(), n, maxWait, opts...)
	if err != nil {
		t.Fatalf("CadenceWatch: %v", err)
	}
	cw := w.(*cadenceWatch)
	cw.subscribed = make(chan struct{})
	return cw
}

func sendTexts(t *testing.T, s stream.Stream, contents ...string) {
	t.Helper()
	for _, c := range contents {
		if err := s.Send(events.TextInput{Content: c}); err != nil {
			t.Fatalf("send %q: %v", c, err)
		}
	}
}

// TestCadenceCountTrigger pins the count side: the batch fires as soon as n
// matching events have arrived and carries them in arrival order.
func TestCadenceCountTrigger(t *testing.T) {
	w := newCadence(t, 3, 0)
	got := cadenceBatch(t, w)
	<-w.subscribed

	sendTexts(t, w.s, "a", "b", "c", "d")
	batch := <-got
	if len(batch) != 3 {
		t.Fatalf("batch = %v, want 3 events", batch)
	}
	for i, want := range []string{"a", "b", "c"} {
		if batch[i].(events.TextInput).Content != want {
			t.Fatalf("batch[%d] = %v, want %q", i, batch[i], want)
		}
	}
}

// TestCadenceWaitReturnsLastEvent pins the Watch view of a batch.
func TestCadenceWaitReturnsLastEvent(t *testing.T) {
	w := newCadence(t, 2, 0)
	done := make(chan events.Event, 1)
	go func() {
		ev, err := w.Wait(waitCtx(t))
		if err != nil {
			t.Errorf("Wait: %v", err)
			return
		}
		done <- ev
	}()
	<-w.subscribed
	sendTexts(t, w.s, "first", "last")

	ev := <-done
	if ev == nil {
		t.Fatal("Wait returned nil event")
	}
	if got := ev.(events.TextInput).Content; got != "last" {
		t.Fatalf("Wait event = %q, want the last batch event %q", got, "last")
	}
}

// TestCadenceTimeTrigger pins the time side: the window opens with the first
// event and fires when it expires, even below the count.
func TestCadenceTimeTrigger(t *testing.T) {
	w := newCadence(t, 5, 40*time.Millisecond)
	got := cadenceBatch(t, w)
	<-w.subscribed
	start := time.Now()
	sendTexts(t, w.s, "only")

	batch := <-got
	if len(batch) != 1 {
		t.Fatalf("batch = %v, want the single buffered event", batch)
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Fatalf("fired after %v, want the 40ms window", elapsed)
	}
}

// TestCadenceCountWinsOverTime pins "whichever comes first": the count fires the
// batch without waiting out the window.
func TestCadenceCountWinsOverTime(t *testing.T) {
	w := newCadence(t, 2, time.Hour)
	got := cadenceBatch(t, w)
	<-w.subscribed
	start := time.Now()
	sendTexts(t, w.s, "a", "b")

	select {
	case batch := <-got:
		if len(batch) != 2 {
			t.Fatalf("batch = %v, want 2 events", batch)
		}
	case <-time.After(time.Second):
		t.Fatal("count trigger did not fire before the long window")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("count trigger took %v", elapsed)
	}
}

// TestCadenceConditionFilters pins the condition option.
func TestCadenceConditionFilters(t *testing.T) {
	w := newCadence(t, 1, 0, WithCadenceCondition(events.TypeOf[events.TextInput]()))
	got := cadenceBatch(t, w)
	<-w.subscribed

	if err := w.s.Send(events.ModelMessageChunk{Content: "noise"}); err != nil {
		t.Fatalf("send noise: %v", err)
	}
	sendTexts(t, w.s, "wanted")

	batch := <-got
	if len(batch) != 1 {
		t.Fatalf("batch = %v, want only the matching event", batch)
	}
	if got := batch[0].(events.TextInput).Content; got != "wanted" {
		t.Fatalf("batch event = %q", got)
	}
}

// TestCadenceRejectsInvalidTriggers pins the constructor validation.
func TestCadenceRejectsInvalidTriggers(t *testing.T) {
	tests := []struct {
		name    string
		n       int
		maxWait time.Duration
	}{
		{name: "neither trigger", n: 0, maxWait: 0},
		{name: "negative count", n: -1, maxWait: time.Second},
		{name: "negative window", n: 1, maxWait: -time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := CadenceWatch(stream.NewMemoryStream(), tt.n, tt.maxWait); !errors.Is(err, ErrInvalidCadence) {
				t.Fatalf("error = %v, want ErrInvalidCadence", err)
			}
		})
	}
}

// TestCadenceHonorsCancellation pins cancellation while waiting for the first
// event.
func TestCadenceHonorsCancellation(t *testing.T) {
	w := newCadence(t, 3, 0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := w.WaitBatch(ctx)
		done <- err
	}()
	<-w.subscribed
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}
