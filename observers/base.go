package observers

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
)

// ErrNilProcess reports a BaseObserver built without a process function.
var ErrNilProcess = errors.New("observers: nil process function")

// ErrProcessTimeout reports that one process call exceeded the observer's
// per-call deadline. It is logged, never propagated into the dispatching Send
// (Python parity: BaseObserver logs the expiry and drops the result).
var ErrProcessTimeout = errors.New("observers: process timed out")

// ProcessFunc analyzes one batch of events and may return an alert, which the
// BaseObserver sends on the conversation stream.
type ProcessFunc func(ctx *agocontext.Context, evs []events.Event) (*events.ObserverAlert, error)

// BaseObserverOption configures a BaseObserver.
type BaseObserverOption func(*BaseObserver)

// WithObserverCondition narrows which events reach the batch (nil = every
// event).
func WithObserverCondition(cond events.Condition) BaseObserverOption {
	return func(b *BaseObserver) { b.cond = cond }
}

// WithBatchSize processes a batch as soon as n events have arrived (Python
// parity: a cadence count trigger). n <= 0 keeps per-event processing.
func WithBatchSize(n int) BaseObserverOption {
	return func(b *BaseObserver) {
		if n > 0 {
			b.batchSize = n
		}
	}
}

// WithBatchWindow bounds how long a batch may grow: process it when the window
// since its first event expires (Python parity: a cadence time trigger). d <= 0
// disables the window.
func WithBatchWindow(d time.Duration) BaseObserverOption {
	return func(b *BaseObserver) {
		if d > 0 {
			b.batchWindow = d
		}
	}
}

// WithProcessTimeout bounds one process call (Python parity: BaseObserver's
// timeout=). Expiry is logged and the result dropped; the process function is
// not cancelled and may keep running on its goroutine. d <= 0 waits
// indefinitely.
func WithProcessTimeout(d time.Duration) BaseObserverOption {
	return func(b *BaseObserver) {
		if d > 0 {
			b.timeout = d
		}
	}
}

// BaseObserver is the trigger-driven observer skeleton: it subscribes with one
// condition, collects matching events into batches (one event by default, n
// events, or a time window) and hands each batch to a ProcessFunc under an
// optional deadline. A non-nil alert returned by ProcessFunc is sent on the
// stream as an ObserverAlert; errors are logged and swallowed, like Python's
// BaseObserver.
//
// Safe for concurrent use: the stream may dispatch from several goroutines and
// a batch window fires on its own timer.
//
// AGO-DEVIATION: Python arms a Watch to collect batches; AGo subscribes directly
// and batches inline because the Watch contract is pull-model (D-024), so
// batching cannot be composed with the push path without a goroutine per
// observer.
type BaseObserver struct {
	name        string
	process     ProcessFunc
	cond        events.Condition
	batchSize   int
	batchWindow time.Duration
	timeout     time.Duration

	mu    sync.Mutex // guards buf and timer
	buf   []events.Event
	timer *time.Timer
}

// NewBaseObserver builds an observer named name that processes batches with
// process. Without options every matching event is one batch.
func NewBaseObserver(name string, process ProcessFunc, opts ...BaseObserverOption) *BaseObserver {
	b := &BaseObserver{name: name, process: process}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Name returns the observer's display name.
func (b *BaseObserver) Name() string { return b.name }

// Register subscribes the observer on the conversation stream.
func (b *BaseObserver) Register(ctx *agocontext.Context) error {
	if err := validateTarget(ctx); err != nil {
		return err
	}
	if b.process == nil {
		return ErrNilProcess
	}
	ctx.Stream.Subscribe(func(ev events.Event) error {
		b.ingest(ctx, ev)
		return nil
	}, stream.SubscribeOptions{Condition: b.cond})
	return nil
}

// ingest adds one event to the current batch and processes it when a trigger
// fires. Count-triggered batches run inline in the dispatching Send, like the
// pre-existing observers; window-triggered batches run on the timer.
func (b *BaseObserver) ingest(ctx *agocontext.Context, ev events.Event) {
	b.mu.Lock()
	b.buf = append(b.buf, ev)
	var batch []events.Event
	switch {
	case b.batchSize == 0 && b.batchWindow == 0:
		batch = b.takeLocked()
	case b.batchSize > 0 && len(b.buf) >= b.batchSize:
		batch = b.takeLocked()
	case b.batchWindow > 0 && b.timer == nil:
		b.timer = time.AfterFunc(b.batchWindow, func() { b.flushWindow(ctx) })
	}
	b.mu.Unlock()
	if len(batch) > 0 {
		b.deliver(ctx, batch)
	}
}

// takeLocked removes and returns the current batch, stopping its window timer.
func (b *BaseObserver) takeLocked() []events.Event {
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	batch := b.buf
	b.buf = nil
	return batch
}

// flushWindow processes a window-triggered batch.
func (b *BaseObserver) flushWindow(ctx *agocontext.Context) {
	b.mu.Lock()
	batch := b.takeLocked()
	b.mu.Unlock()
	if len(batch) > 0 {
		b.deliver(ctx, batch)
	}
}

// deliver runs process under the optional deadline and sends a non-nil alert.
func (b *BaseObserver) deliver(ctx *agocontext.Context, evs []events.Event) {
	alert, err := b.processBatch(ctx, evs)
	if err != nil {
		slog.Warn("observers: process failed", "observer", b.name, "err", err)
		return
	}
	if alert == nil {
		return
	}
	if err := ctx.Send(alert); err != nil {
		slog.Warn("observers: send alert failed", "observer", b.name, "err", err)
	}
}

// processBatch bounds one process call by the configured timeout.
func (b *BaseObserver) processBatch(ctx *agocontext.Context, evs []events.Event) (*events.ObserverAlert, error) {
	if b.timeout <= 0 {
		return b.process(ctx, evs)
	}
	type outcome struct {
		alert *events.ObserverAlert
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		alert, err := b.process(ctx, evs)
		done <- outcome{alert: alert, err: err}
	}()
	t := time.NewTimer(b.timeout)
	defer t.Stop()
	select {
	case o := <-done:
		return o.alert, o.err
	case <-t.C:
		return nil, ErrProcessTimeout
	}
}
