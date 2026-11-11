package stream

import (
	"context"
	"errors"
	"sync"

	"github.com/luckystrker/ago/events"
)

// Join errors.
var (
	// ErrJoinDone ends a bounded iterator (JoinOptions.MaxEvents reached).
	ErrJoinDone = errors.New("stream: join window exhausted")
	// ErrJoinClosed ends an iterator whose Close was called.
	ErrJoinClosed = errors.New("stream: join closed")
)

// JoinOptions configures Join.
type JoinOptions struct {
	// Condition filters what the iterator yields; nil matches every event.
	Condition events.Condition
	// MaxEvents bounds the iterator: after this many events Next reports
	// ErrJoinDone. Zero or negative is unlimited (Python parity: join(max_events=None)).
	MaxEvents int
}

// Join starts a pull iterator over any Stream (Python parity: ABCStream.join).
// The iterator receives every event the stream dispatches after the join,
// including ones the scope sends before the first Next. The context cancels a
// blocked Next.
//
// The caller must Close the iterator; until then it holds a subscription. A
// bounded iterator releases its subscription as soon as the window is filled.
//
// Join is a package function rather than a method on the Stream interface so
// existing Stream implementations keep compiling; *MemoryStream and *SubStream
// also expose it as a method.
//
// AGO-DEVIATION: Python's join is a method on the stream ABC. The Go Stream
// interface is a published contract implemented outside this repository, so a
// new required method would break every third-party implementation.
func Join(ctx context.Context, s Stream, opts JoinOptions) *Joiner {
	if ctx == nil {
		ctx = context.Background()
	}
	j := &Joiner{ctx: ctx, max: opts.MaxEvents, wake: make(chan struct{}, 1)}
	j.unsubscribe = s.Unsubscribe
	j.id = s.Subscribe(j.deliver, SubscribeOptions{Condition: opts.Condition})
	return j
}

// Join starts a pull iterator over the stream (see the package-level Join).
func (m *MemoryStream) Join(ctx context.Context, opts JoinOptions) *Joiner {
	return Join(ctx, m, opts)
}

// Join starts a pull iterator over the filtered view (see the package-level Join).
func (s *SubStream) Join(ctx context.Context, opts JoinOptions) *Joiner {
	return Join(ctx, s, opts)
}

// Joiner is a live pull iterator over a stream. Events are buffered unboundedly,
// so a slow reader never blocks a Send. Safe for concurrent use.
type Joiner struct {
	ctx context.Context
	max int

	unsubscribe func(SubID)
	id          SubID

	mu        sync.Mutex
	buf       []events.Event
	delivered int
	closed    bool
	exhausted bool

	wake chan struct{}
}

// deliver buffers one event for Next. It never blocks the dispatching Send.
func (j *Joiner) deliver(ev events.Event) error {
	j.mu.Lock()
	if j.closed || j.exhausted {
		j.mu.Unlock()
		return nil
	}
	j.buf = append(j.buf, ev)
	j.mu.Unlock()
	select {
	case j.wake <- struct{}{}:
	default:
	}
	return nil
}

// Next returns the next buffered event, waiting for one if necessary. It returns
// ErrJoinDone once a bounded window is filled, ErrJoinClosed after Close, and the
// context error when the join context is cancelled.
func (j *Joiner) Next() (events.Event, error) {
	for {
		j.mu.Lock()
		switch {
		case j.exhausted:
			j.mu.Unlock()
			return nil, ErrJoinDone
		case j.closed:
			j.mu.Unlock()
			return nil, ErrJoinClosed
		case len(j.buf) > 0:
			ev := j.buf[0]
			j.buf = j.buf[1:]
			j.delivered++
			done := j.max > 0 && j.delivered >= j.max
			j.mu.Unlock()
			if done {
				j.finish()
			}
			return ev, nil
		}
		j.mu.Unlock()

		select {
		case <-j.wake:
		case <-j.ctx.Done():
			// Releasing the subscription here keeps a cancelled join from
			// outliving its consumer (the generator's close in Python).
			j.Close()
			return nil, j.ctx.Err()
		}
	}
}

// Close releases the subscription; later Next calls report ErrJoinClosed.
// Idempotent and safe to call from any goroutine.
func (j *Joiner) Close() {
	j.detach(func() { j.closed = true })
}

// finish releases the subscription after a bounded window is filled.
func (j *Joiner) finish() {
	j.detach(func() { j.exhausted = true })
}

// detach marks the iterator terminal under the lock, then unsubscribes outside
// it (Unsubscribe takes the stream's own lock).
func (j *Joiner) detach(mark func()) {
	j.mu.Lock()
	if j.closed || j.exhausted {
		j.mu.Unlock()
		return
	}
	mark()
	j.buf = nil
	unsub, id := j.unsubscribe, j.id
	j.unsubscribe = nil
	j.mu.Unlock()
	if unsub != nil {
		unsub(id)
	}
}
