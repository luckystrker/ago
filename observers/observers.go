// Package observers provides event-stream subscribers with lifecycle registration:
// an Observer attaches itself to a conversation stream and watches it, emitting
// ObserverAlert events when it detects a condition worth surfacing (looping model
// output, exhausted token budget, ...).
//
// Parity with Python AG2 ag2/observers/: the Observer protocol, StreamObserver,
// CompositeObserver and the built-ins LoopDetector and TokenMonitor. The alert flow
// is Observer → ObserverAlert on the stream; the assembly AlertPolicy (policies
// package) dedups alerts, injects non-fatal ones into the prompt and turns fatal
// ones into HaltEvent.
//
// AGO-DEVIATION: Python registers observers under a caller-owned ExitStack and
// buffers events through a Watch before processing; AGo subscribes directly on the
// stream (no cleanup stack — the subscription lives with the stream) and processes
// events inline in the dispatching Send. See docs/DECISIONS.md D-025.
package observers

import (
	"errors"
	"fmt"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
)

// Observer attaches its subscriptions to a conversation context. A Register call
// subscribes on ctx.Stream and must not block; implementations with mutable state
// must be safe for concurrent use, because the stream may dispatch from several
// goroutines.
type Observer interface {
	// Name identifies the observer in diagnostics and lifecycle events.
	Name() string
	// Register subscribes the observer on the conversation stream.
	Register(ctx *agocontext.Context) error
}

// Registration errors. Implementations wrap them with %w when adding context.
var (
	ErrNilContext  = errors.New("observers: nil conversation context")
	ErrNoStream    = errors.New("observers: conversation context has no stream")
	ErrNilCallback = errors.New("observers: nil event callback")
	ErrNilObserver = errors.New("observers: nil observer")
)

// validateTarget checks the context an observer is about to register on.
func validateTarget(ctx *agocontext.Context) error {
	if ctx == nil {
		return ErrNilContext
	}
	if ctx.Stream == nil {
		return ErrNoStream
	}
	return nil
}

// StreamObserver is the lightweight shape: a condition → callback subscription.
// fn runs for every stream event matching cond (nil cond = every event), inline
// in the dispatching Send. It replaces the Python @observer decorator factory.
//
// Without options it runs after every other non-interrupt subscriber and has no
// deadline, the pre-existing behaviour.
type StreamObserver struct {
	name      string
	cond      events.Condition
	fn        func(ev events.Event)
	interrupt bool
	timeout   time.Duration
}

// StreamObserverOption configures NewStreamObserver.
type StreamObserverOption func(*StreamObserver)

// WithInterrupt runs the observer before normal subscribers (Python parity:
// interrupt=True), so it sees an event first. The observer cannot swallow the
// event: its callback has no error result.
func WithInterrupt() StreamObserverOption {
	return func(o *StreamObserver) { o.interrupt = true }
}

// WithTimeout bounds one callback call (Python parity: timeout=). Zero or
// negative waits indefinitely. On expiry the dispatching Send reports an error
// wrapping stream.ErrSubscriberTimeout and stops delivering that event to later
// handlers; the callback is not cancelled and keeps running on its goroutine.
//
// AGO-DEVIATION: Python's observer also carries sync_to_thread=, which decides
// whether a synchronous callback is offloaded from the event loop. Go has no
// event loop and no GIL, so the knob has no Go meaning and is not ported; a
// timed callback runs on its own goroutine, an untimed one inline.
func WithTimeout(d time.Duration) StreamObserverOption {
	return func(o *StreamObserver) { o.timeout = d }
}

// NewStreamObserver watches the stream with cond and calls fn on each match.
func NewStreamObserver(name string, cond events.Condition, fn func(ev events.Event), opts ...StreamObserverOption) *StreamObserver {
	o := &StreamObserver{name: name, cond: cond, fn: fn}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// Name returns the observer's display name.
func (o *StreamObserver) Name() string { return o.name }

// Register subscribes the callback on the conversation stream.
func (o *StreamObserver) Register(ctx *agocontext.Context) error {
	if err := validateTarget(ctx); err != nil {
		return err
	}
	if o.fn == nil {
		return ErrNilCallback
	}
	// The callback has no error result (contract shape); subscriber errors are
	// only a stream-level concern for the built-ins that send alerts.
	ctx.Stream.Subscribe(func(ev events.Event) error {
		o.fn(ev)
		return nil
	}, stream.SubscribeOptions{Interrupt: o.interrupt, Condition: o.cond, Timeout: o.timeout})
	return nil
}

// CompositeObserver registers a group of observers with one Register call, in
// construction order. Immutable after construction, so it is safe for
// concurrent use.
type CompositeObserver struct {
	observers []Observer
}

// NewCompositeObserver groups observers; the slice is copied.
func NewCompositeObserver(observers ...Observer) *CompositeObserver {
	cs := make([]Observer, len(observers))
	copy(cs, observers)
	return &CompositeObserver{observers: cs}
}

// Name identifies the composite in diagnostics.
func (c *CompositeObserver) Name() string { return "composite" }

// Register registers every child in order, stopping at the first failure
// (the error keeps the child's error for errors.Is/As).
func (c *CompositeObserver) Register(ctx *agocontext.Context) error {
	if err := validateTarget(ctx); err != nil {
		return err
	}
	for i, o := range c.observers {
		if o == nil {
			return fmt.Errorf("observers: composite member %d: %w", i, ErrNilObserver)
		}
		if err := o.Register(ctx); err != nil {
			return fmt.Errorf("observers: %s: %w", o.Name(), err)
		}
	}
	return nil
}
