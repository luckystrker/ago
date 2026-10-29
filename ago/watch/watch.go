// Package watch provides composable "wait until something happens" triggers over
// conversation events and time: an event match, a periodic tick, a one-shot delay,
// and the and/or/sequence combinations of watches.
//
// A Watch is a pull primitive: Wait blocks until the watch fires and reports the
// event that fired it. One Wait call is one armed watch — there is no separate
// arm/disarm lifecycle to manage, and every Wait on the same watch is independent,
// so all implementations here are safe for concurrent use.
//
// Signal semantics: a fired watch reports success with a nil event when the trigger
// carries no payload. IntervalWatch and DelayWatch always return (nil, nil) — the
// tick itself is the payload — and the composites propagate whatever their
// sub-watches produced. A nil event with a nil error therefore means "the watch
// fired", never "nothing happened"; callers that only need the signal ignore the
// event, callers that need a payload assert on it.
//
// Every watch honors context cancellation: a cancelled ctx makes Wait return
// ctx.Err() (wrapped where a composite forwards a sub-watch error).
package watch

import (
	"context"
	"errors"
	"sync"
	"time"

	"ago/events"
	"ago/stream"
)

// Watch is a waitable trigger.
type Watch interface {
	// Wait blocks until the watch fires or ctx is done. On success it returns the
	// event that fired the watch — nil for time-based triggers (signal semantics,
	// see the package comment). On cancellation it returns ctx.Err().
	Wait(ctx context.Context) (events.Event, error)
}

// WatchFunc adapts a plain function to the Watch interface.
type WatchFunc func(ctx context.Context) (events.Event, error)

// Wait implements Watch.
func (f WatchFunc) Wait(ctx context.Context) (events.Event, error) { return f(ctx) }

// ErrNilWatch is reported by the composites when a sub-watch is nil. A nil watch
// would otherwise panic inside a spawned goroutine and take the process down.
var ErrNilWatch = errors.New("watch: nil sub-watch")

// EventWatch waits for the first event matching cond. The subscription lives only
// for the duration of one Wait: the watch subscribes, waits on a channel and
// unsubscribes on every return path (match, error, cancellation), so repeated or
// abandoned waits never leak subscriptions. A nil cond matches every event.
// Only events sent after Wait subscribed are seen (no history replay), matching
// the push model's "fires for events after arming".
type eventWatch struct {
	s    stream.Stream
	cond events.Condition
}

// EventWatch returns a Watch that fires on the first stream event matching cond.
func EventWatch(s stream.Stream, cond events.Condition) Watch {
	return eventWatch{s: s, cond: cond}
}

func (w eventWatch) Wait(ctx context.Context) (events.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Buffered channel + non-blocking send: subscribers run synchronously inside
	// Stream.Send and must never block on a Wait that may already have returned.
	ch := make(chan events.Event, 1)
	id := w.s.Subscribe(func(ev events.Event) error {
		select {
		case ch <- ev:
		default:
		}
		return nil
	}, stream.SubscribeOptions{Condition: w.cond})
	defer w.s.Unsubscribe(id)
	select {
	case ev := <-ch:
		return ev, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// IntervalWatch fires periodically: each Wait blocks for one interval and then
// returns the tick signal (nil, nil) — the event is always nil, the tick itself is
// the payload. An interval of zero or less fires immediately.
type intervalWatch struct{ interval time.Duration }

// IntervalWatch returns a Watch that fires after every interval elapse.
func IntervalWatch(interval time.Duration) Watch {
	return intervalWatch{interval: interval}
}

func (w intervalWatch) Wait(ctx context.Context) (events.Event, error) {
	t := time.NewTimer(w.interval)
	defer t.Stop()
	select {
	case <-t.C:
		return nil, nil // signal tick: fired with no event payload
		// AGO-DEVIATION: Python's IntervalWatch invokes the callback with an empty
		// event list; the Wait contract carries one event, so the tick is (nil, nil).
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// DelayWatch fires once after a delay: a Wait blocks until the delay elapses and
// returns (nil, nil). The watch is stateless — one Wait is one armed single-shot
// delay, so a sequence of Waits gets one delayed fire each; concurrent Waits each
// get their own. A delay of zero or less fires immediately.
//
// AGO-DEVIATION: Python's DelayWatch disarms itself forever after its single fire;
// the Watch contract has no arm/disarm surface, so the Wait call is the unit of
// arming and each Wait arms a fresh single-shot delay.
type delayWatch struct{ delay time.Duration }

// DelayWatch returns a Watch that fires once after delay.
func DelayWatch(delay time.Duration) Watch {
	return delayWatch{delay: delay}
}

func (w delayWatch) Wait(ctx context.Context) (events.Event, error) {
	return intervalWatch{interval: w.delay}.Wait(ctx)
}

// AllOf waits for every sub-watch and reports the last event any of them carried
// (nil when none did — e.g. when only time-based watches fired). Sub-waits run
// concurrently; the first failure cancels the remaining waits and is returned.
// An empty AllOf fires immediately with (nil, nil): all of nothing has fired.
//
// AGO-DEVIATION: Python's AllOf passes the combined batches of every sub-watch to
// the callback; one Wait carries one event, so AllOf reports the last non-nil one.
func AllOf(ws ...Watch) Watch {
	return WatchFunc(func(ctx context.Context) (events.Event, error) {
		for _, w := range ws {
			if w == nil {
				return nil, ErrNilWatch
			}
		}
		waitCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		var (
			mu   sync.Mutex // guards last and err below
			last events.Event
			err  error
			wg   sync.WaitGroup
		)
		for _, w := range ws {
			wg.Add(1)
			go func(w Watch) {
				defer wg.Done()
				ev, werr := w.Wait(waitCtx)
				mu.Lock()
				defer mu.Unlock()
				if werr != nil {
					if err == nil {
						err = werr
					}
					cancel()
					return
				}
				if ev != nil {
					last = ev
				}
			}(w)
		}
		wg.Wait()
		if err != nil {
			return nil, err
		}
		return last, nil
	})
}

// AnyOf waits for the first sub-watch to fire and reports its event; the remaining
// waits are cancelled and abandoned (a sub-watch that ignores cancellation keeps
// its goroutine until it returns on its own — its result is discarded). When every
// sub-watch fails, the error is ctx.Err() if the parent context is done, otherwise
// the first sub-watch failure. An empty AnyOf can never fire: Wait blocks until the
// context is cancelled.
func AnyOf(ws ...Watch) Watch {
	return WatchFunc(func(ctx context.Context) (events.Event, error) {
		for _, w := range ws {
			if w == nil {
				return nil, ErrNilWatch
			}
		}
		waitCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		type outcome struct {
			ev  events.Event
			err error
		}
		// Buffered: abandoned waits must be able to deliver and exit.
		results := make(chan outcome, len(ws))
		for _, w := range ws {
			go func(w Watch) {
				ev, err := w.Wait(waitCtx)
				results <- outcome{ev: ev, err: err}
			}(w)
		}
		var firstErr error
		for range ws {
			r := <-results
			if r.err == nil {
				return r.ev, nil
			}
			if firstErr == nil {
				firstErr = r.err
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(ws) == 0 {
			// Nothing can ever fire: block until the context is done.
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return nil, firstErr
	})
}

// Sequence waits for the sub-watches in order: each sub-watch's Wait starts only
// after the previous one fired, and the first failure stops the sequence. The
// result is the last event carried across the sub-watches (nil when none carried
// an event). An empty Sequence fires immediately with (nil, nil).
//
// AGO-DEVIATION: Python's Sequence collects every step's events and replays the
// batch to the callback; one Wait carries one event, so Sequence reports the last
// non-nil one (the same convention as AllOf).
func Sequence(ws ...Watch) Watch {
	return WatchFunc(func(ctx context.Context) (events.Event, error) {
		var last events.Event
		for _, w := range ws {
			if w == nil {
				return nil, ErrNilWatch
			}
			ev, err := w.Wait(ctx)
			if err != nil {
				return nil, err
			}
			if ev != nil {
				last = ev
			}
		}
		return last, nil
	})
}
