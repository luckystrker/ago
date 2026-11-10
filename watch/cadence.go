package watch

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
)

// ErrInvalidCadence reports a CadenceWatch with no trigger: a cadence needs at
// least one of a positive count or a positive time window, and neither may be
// negative (Python parity: CadenceWatch raises ValueError).
var ErrInvalidCadence = errors.New("watch: cadence requires a positive n or max_wait")

// BatchWatch is a Watch whose firing carries the whole batch that fired it.
// CadenceWatch returns a BatchWatch; the plain Wait result is the last event of
// that batch, so a BatchWatch can be used wherever a Watch is expected.
type BatchWatch interface {
	Watch
	// WaitBatch blocks until the batch fires and returns its events in arrival
	// order. The batch is never empty on success.
	WaitBatch(ctx context.Context) ([]events.Event, error)
}

// CadenceOption configures CadenceWatch.
type CadenceOption func(*cadenceWatch)

// WithCadenceCondition narrows which events count toward the cadence (Python
// parity: condition=). A nil condition counts every event.
func WithCadenceCondition(cond events.Condition) CadenceOption {
	return func(w *cadenceWatch) { w.cond = cond }
}

// CadenceWatch fires when n matching events have arrived or maxWait has elapsed
// since the first of them, whichever comes first. At least one trigger must be
// positive; n and maxWait must not be negative (Python parity: CadenceWatch).
//
// In the pull model one WaitBatch call is one armed window: the subscription
// lives for that call only, so events sent between two calls are not carried
// over. The timer starts with the first buffered event, so a time-only cadence
// waits for an event and then bounds how long the batch may grow — it does not
// fire on an empty stream (Python parity).
//
// AGO-DEVIATION: Python arms the watch once and keeps buffering across
// callbacks; AGo's Watch contract is one Wait = one armed watch (D-024), so the
// batch is returned to the caller instead of pushed into a callback.
func CadenceWatch(s stream.Stream, n int, maxWait time.Duration, opts ...CadenceOption) (BatchWatch, error) {
	if n < 0 || maxWait < 0 || (n == 0 && maxWait == 0) {
		return nil, ErrInvalidCadence
	}
	w := &cadenceWatch{s: s, n: n, maxWait: maxWait}
	for _, opt := range opts {
		opt(w)
	}
	return w, nil
}

type cadenceWatch struct {
	s       stream.Stream
	n       int
	maxWait time.Duration
	cond    events.Condition

	// subscribed, when non-nil, is closed as soon as a Wait has installed its
	// subscription; a test seam so a test can send events without racing it.
	subscribed chan struct{}
}

// Wait implements Watch: it returns the last event of the batch that fired.
func (w *cadenceWatch) Wait(ctx context.Context) (events.Event, error) {
	batch, err := w.WaitBatch(ctx)
	if err != nil {
		return nil, err
	}
	if len(batch) == 0 {
		return nil, nil
	}
	return batch[len(batch)-1], nil
}

// WaitBatch implements BatchWatch.
func (w *cadenceWatch) WaitBatch(ctx context.Context) ([]events.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var (
		mu      sync.Mutex // guards buf and firstAt
		buf     []events.Event
		firstAt time.Time
	)
	signal := make(chan struct{}, 1)
	id := w.s.Subscribe(func(ev events.Event) error {
		mu.Lock()
		if len(buf) == 0 {
			firstAt = time.Now()
		}
		buf = append(buf, ev)
		mu.Unlock()
		// Non-blocking: the subscriber runs inside Stream.Send and must never
		// wait for a Wait that may already have returned.
		select {
		case signal <- struct{}{}:
		default:
		}
		return nil
	}, stream.SubscribeOptions{Condition: w.cond})
	defer w.s.Unsubscribe(id)
	if w.subscribed != nil {
		close(w.subscribed)
	}

	take := func(n int) []events.Event {
		n = min(n, len(buf))
		batch := append([]events.Event(nil), buf[:n]...)
		buf = nil
		firstAt = time.Time{}
		return batch
	}
	for {
		mu.Lock()
		if w.n > 0 && len(buf) >= w.n {
			batch := take(w.n)
			mu.Unlock()
			return batch, nil
		}
		var wait time.Duration
		if w.maxWait > 0 && len(buf) > 0 {
			wait = w.maxWait - time.Since(firstAt)
			if wait <= 0 {
				batch := take(len(buf))
				mu.Unlock()
				return batch, nil
			}
		}
		mu.Unlock()

		if wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-signal:
				t.Stop()
			case <-t.C:
				// The buffer check above returns the expired batch.
			case <-ctx.Done():
				t.Stop()
				return nil, ctx.Err()
			}
			continue
		}
		select {
		case <-signal:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
