package watch

import (
	"context"
	"errors"
	"testing"
	"time"

	"ago/events"
	"ago/stream"
)

var errBoom = errors.New("boom")

var never = make(chan struct{}) // closed by nobody: a gate that never opens

func textEvent(content string) events.Event { return events.TextInput{Content: content} }

// fixed fires immediately with the given outcome.
func fixed(ev events.Event, err error) Watch {
	return WatchFunc(func(context.Context) (events.Event, error) { return ev, err })
}

// gated fires with ev once done closes; it honors ctx cancellation.
func gated(done <-chan struct{}, ev events.Event) Watch {
	return WatchFunc(func(ctx context.Context) (events.Event, error) {
		select {
		case <-done:
			return ev, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
}

// waitCtx is a watchdog: a regression that hangs fails in 2s instead of stalling the suite.
func waitCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestEventWatchFiresOnFirstMatch(t *testing.T) {
	s := stream.NewMemoryStream()
	w := EventWatch(s, events.TypeOf[events.TextInput]())

	// Noise sent before and (scheduled) after arming must not satisfy the watch.
	if err := s.Send(events.ModelMessageChunk{Content: "noise"}); err != nil {
		t.Fatalf("send noise: %v", err)
	}
	time.AfterFunc(5*time.Millisecond, func() {
		_ = s.Send(events.ModelMessageChunk{Content: "noise"})
		_ = s.Send(textEvent("wanted"))
	})

	ev, err := w.Wait(waitCtx(t))
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	got, ok := ev.(events.TextInput)
	if !ok || got.Content != "wanted" {
		t.Fatalf("got %#v, want TextInput \"wanted\"", ev)
	}
}

func TestEventWatchNilConditionMatchesEverything(t *testing.T) {
	s := stream.NewMemoryStream()
	w := EventWatch(s, nil)

	time.AfterFunc(5*time.Millisecond, func() { _ = s.Send(events.ModelMessageChunk{Content: "any"}) })

	ev, err := w.Wait(waitCtx(t))
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if ev.EventType() != "ModelMessageChunk" {
		t.Fatalf("got %s, want ModelMessageChunk", ev.EventType())
	}
}

func TestEventWatchRespectsCancellation(t *testing.T) {
	s := stream.NewMemoryStream()
	w := EventWatch(s, events.TypeOf[events.TextInput]())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := w.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled ctx: got %v, want context.Canceled", err)
	}

	// Cancellation while waiting: no matching event is ever sent.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel2()
	if _, err := w.Wait(ctx2); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("mid-wait cancel: got %v, want context.DeadlineExceeded", err)
	}
}

func TestIntervalWatch(t *testing.T) {
	w := IntervalWatch(5 * time.Millisecond)
	start := time.Now()
	ev, err := w.Wait(waitCtx(t))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if ev != nil {
		t.Fatalf("tick event = %#v, want nil (signal semantics)", ev)
	}
	if elapsed < 5*time.Millisecond {
		t.Fatalf("returned after %v, want >= 5ms", elapsed)
	}

	// Zero interval fires immediately.
	if ev, err := IntervalWatch(0).Wait(waitCtx(t)); err != nil || ev != nil {
		t.Fatalf("zero interval: got (%#v, %v), want (nil, nil)", ev, err)
	}

	// Cancellation while waiting for the next tick.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if _, err := IntervalWatch(time.Hour).Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled tick: got %v, want context.DeadlineExceeded", err)
	}
}

func TestDelayWatch(t *testing.T) {
	w := DelayWatch(5 * time.Millisecond)
	start := time.Now()
	ev, err := w.Wait(waitCtx(t))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if ev != nil {
		t.Fatalf("delay event = %#v, want nil (signal semantics)", ev)
	}
	if elapsed < 5*time.Millisecond {
		t.Fatalf("returned after %v, want >= 5ms", elapsed)
	}

	// One Wait is one single-shot fire: a second Wait delays again.
	start = time.Now()
	if ev, err := w.Wait(waitCtx(t)); err != nil || ev != nil {
		t.Fatalf("second wait: got (%#v, %v), want (nil, nil)", ev, err)
	} else if time.Since(start) < 5*time.Millisecond {
		t.Fatalf("second wait returned early after %v", time.Since(start))
	}

	// Zero delay fires immediately; cancellation returns ctx.Err.
	if ev, err := DelayWatch(0).Wait(waitCtx(t)); err != nil || ev != nil {
		t.Fatalf("zero delay: got (%#v, %v), want (nil, nil)", ev, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if _, err := DelayWatch(time.Hour).Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled delay: got %v, want context.DeadlineExceeded", err)
	}
}

func TestAllOf(t *testing.T) {
	t.Run("last carried event wins", func(t *testing.T) {
		// The interval fires first with no payload; the gated watch carries the event.
		release := make(chan struct{})
		time.AfterFunc(5*time.Millisecond, func() { close(release) })
		ev, err := AllOf(IntervalWatch(0), gated(release, textEvent("b"))).Wait(waitCtx(t))
		if err != nil {
			t.Fatalf("wait: %v", err)
		}
		if ev == nil || ev.(events.TextInput).Content != "b" {
			t.Fatalf("got %#v, want TextInput \"b\"", ev)
		}
	})

	t.Run("empty fires immediately", func(t *testing.T) {
		if ev, err := AllOf().Wait(waitCtx(t)); err != nil || ev != nil {
			t.Fatalf("got (%#v, %v), want (nil, nil)", ev, err)
		}
	})

	t.Run("first failure wins", func(t *testing.T) {
		_, err := AllOf(fixed(nil, errBoom), gated(never, textEvent("b"))).Wait(waitCtx(t))
		if !errors.Is(err, errBoom) {
			t.Fatalf("got %v, want errBoom", err)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		defer cancel()
		_, err := AllOf(gated(never, nil), gated(never, nil)).Wait(ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v, want context.DeadlineExceeded", err)
		}
	})

	t.Run("nil sub-watch", func(t *testing.T) {
		if _, err := AllOf(fixed(nil, nil), nil).Wait(waitCtx(t)); !errors.Is(err, ErrNilWatch) {
			t.Fatalf("got %v, want ErrNilWatch", err)
		}
	})
}

func TestAnyOf(t *testing.T) {
	t.Run("first fire wins", func(t *testing.T) {
		ev, err := AnyOf(gated(never, textEvent("slow")), fixed(textEvent("fast"), nil)).Wait(waitCtx(t))
		if err != nil {
			t.Fatalf("wait: %v", err)
		}
		if ev == nil || ev.(events.TextInput).Content != "fast" {
			t.Fatalf("got %#v, want TextInput \"fast\"", ev)
		}
	})

	t.Run("all fail", func(t *testing.T) {
		_, err := AnyOf(fixed(nil, errBoom), fixed(nil, errBoom)).Wait(waitCtx(t))
		if !errors.Is(err, errBoom) {
			t.Fatalf("got %v, want errBoom", err)
		}
	})

	t.Run("parent ctx error wins over sub-watch errors", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		defer cancel()
		_, err := AnyOf(fixed(nil, errBoom), gated(never, nil)).Wait(ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v, want context.DeadlineExceeded", err)
		}
	})

	t.Run("empty never fires", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		defer cancel()
		if _, err := AnyOf().Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v, want context.DeadlineExceeded", err)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		defer cancel()
		_, err := AnyOf(gated(never, nil), gated(never, nil)).Wait(ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v, want context.DeadlineExceeded", err)
		}
	})

	t.Run("nil sub-watch", func(t *testing.T) {
		if _, err := AnyOf(fixed(nil, nil), nil).Wait(waitCtx(t)); !errors.Is(err, ErrNilWatch) {
			t.Fatalf("got %v, want ErrNilWatch", err)
		}
	})
}

func TestSequence(t *testing.T) {
	t.Run("waits in order and returns last carried event", func(t *testing.T) {
		var order []string
		first := WatchFunc(func(ctx context.Context) (events.Event, error) {
			order = append(order, "first")
			return textEvent("1"), nil
		})
		release := make(chan struct{})
		time.AfterFunc(5*time.Millisecond, func() { close(release) })
		second := WatchFunc(func(ctx context.Context) (events.Event, error) {
			// Only legal after the first step fired: sequencing is observable here.
			order = append(order, "second")
			return gated(release, textEvent("2")).Wait(ctx)
		})

		ev, err := Sequence(first, second).Wait(waitCtx(t))
		if err != nil {
			t.Fatalf("wait: %v", err)
		}
		if len(order) != 2 || order[0] != "first" || order[1] != "second" {
			t.Fatalf("order = %v, want [first second]", order)
		}
		if ev == nil || ev.(events.TextInput).Content != "2" {
			t.Fatalf("got %#v, want TextInput \"2\"", ev)
		}
	})

	t.Run("nil payload does not erase the carried event", func(t *testing.T) {
		ev, err := Sequence(fixed(textEvent("1"), nil), IntervalWatch(0)).Wait(waitCtx(t))
		if err != nil {
			t.Fatalf("wait: %v", err)
		}
		if ev == nil || ev.(events.TextInput).Content != "1" {
			t.Fatalf("got %#v, want TextInput \"1\"", ev)
		}
	})

	t.Run("failure stops the sequence", func(t *testing.T) {
		secondRan := false
		second := WatchFunc(func(context.Context) (events.Event, error) {
			secondRan = true
			return nil, nil
		})
		_, err := Sequence(fixed(nil, errBoom), second).Wait(waitCtx(t))
		if !errors.Is(err, errBoom) {
			t.Fatalf("got %v, want errBoom", err)
		}
		if secondRan {
			t.Fatal("second step ran after a failure")
		}
	})

	t.Run("empty fires immediately", func(t *testing.T) {
		if ev, err := Sequence().Wait(waitCtx(t)); err != nil || ev != nil {
			t.Fatalf("got (%#v, %v), want (nil, nil)", ev, err)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		defer cancel()
		_, err := Sequence(gated(never, nil), fixed(nil, nil)).Wait(ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v, want context.DeadlineExceeded", err)
		}
	})

	t.Run("nil sub-watch", func(t *testing.T) {
		if _, err := Sequence(fixed(nil, nil), nil).Wait(waitCtx(t)); !errors.Is(err, ErrNilWatch) {
			t.Fatalf("got %v, want ErrNilWatch", err)
		}
	})
}

func TestEventWatchConcurrentWaits(t *testing.T) {
	// Concurrent (and repeated) Waits are independent: one armed watch each.
	s := stream.NewMemoryStream()
	w := EventWatch(s, events.TypeOf[events.TextInput]())
	time.AfterFunc(5*time.Millisecond, func() {
		_ = s.Send(textEvent("a"))
		_ = s.Send(textEvent("b"))
	})

	ctx := waitCtx(t)
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := w.Wait(ctx)
			done <- err
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("concurrent wait %d: %v", i, err)
		}
	}
}
