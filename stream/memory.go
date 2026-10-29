package stream

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"ago/events"
	"ago/internal/uuid"
)

// Subscriber handles one event. An interrupt subscriber returning ErrHandled
// swallows the event (delivery stops, the transcript does not see it); any other
// non-nil error stops delivery of the event to everyone and surfaces from Send
// (Python parity: an interrupter returning None stops send before the persistence
// subscriber, a raised exception does the same).
type Subscriber func(ev events.Event) error

// ErrHandled is returned by an interrupt subscriber that consumed the event:
// delivery stops silently and the event is not persisted.
var ErrHandled = errors.New("stream: event handled by interrupt subscriber")

// SubID identifies a subscription for Unsubscribe.
type SubID uint64

// SubscribeOptions configures one subscription.
type SubscribeOptions struct {
	Interrupt bool             // runs before normal subscribers; may swallow the event
	Condition events.Condition // nil = match everything
}

// Stream is the conversation event bus: dispatch, transcript and inbox.
type Stream interface {
	ID() string
	History() *History
	Pending() []*events.ModelRequest
	Drain() []*events.ModelRequest
	Send(ev events.Event) error
	Enqueue(parts ...events.Input) error
	Subscribe(fn Subscriber, opts SubscribeOptions) SubID
	Unsubscribe(id SubID)
	Where(c events.Condition) Stream
	Get(ctx context.Context, c events.Condition) (events.Event, error)
	SpawnBackground(ctx context.Context, fn func(context.Context))
	WaitBackground()
}

type subEntry struct {
	id  SubID
	fn  Subscriber
	opt SubscribeOptions
}

// MemoryStream is the default in-process Stream.
type MemoryStream struct {
	id         string
	persistAll bool
	logger     *slog.Logger

	mu      sync.Mutex
	subs    []subEntry
	nextSub SubID
	pending []*events.ModelRequest
	history *History

	bgWG sync.WaitGroup
}

type MemoryOption func(*MemoryStream)

// WithStorage replaces the transcript storage.
func WithStorage(s Storage) MemoryOption {
	return func(m *MemoryStream) { m.history = NewHistory(m.id, s) }
}

// WithStreamID sets the stream id (otherwise a fresh uuid).
func WithStreamID(id string) MemoryOption { return func(m *MemoryStream) { m.id = id } }

// WithPersistAll persists transient events too (debugging).
func WithPersistAll() MemoryOption { return func(m *MemoryStream) { m.persistAll = true } }

// WithLogger sets the logger for subscriber errors (default: slog default).
func WithLogger(l *slog.Logger) MemoryOption { return func(m *MemoryStream) { m.logger = l } }

func NewMemoryStream(opts ...MemoryOption) *MemoryStream {
	m := &MemoryStream{logger: slog.Default()}
	m.history = NewHistory(m.id, NewMemoryStorage())
	for _, o := range opts {
		o(m)
	}
	if m.id == "" {
		m.id = newStreamID()
	}
	return m
}

func newStreamID() string { return "s-" + uuid.New() }

// Send stamps and dispatches the event, then persists it. Dispatch happens without
// holding the lock so subscribers may send further events (reentrant, depth-first
// like the Python await chain). Interrupt subscribers run first: ErrHandled from
// one swallows the event — delivery stops and nothing is persisted (Python parity:
// the persistence subscriber is a normal subscriber, so a swallowed event never
// reaches it); another error stops delivery and is returned. Errors from normal
// subscribers are logged and do not stop delivery.
func (m *MemoryStream) Send(ev events.Event) error {
	if s, ok := ev.(interface{ StampNow(time.Time) }); ok {
		s.StampNow(time.Now())
	}
	m.mu.Lock()
	subs := make([]subEntry, len(m.subs))
	copy(subs, m.subs)
	m.mu.Unlock()

	// Interrupt subscribers first: ErrHandled swallows, another error surfaces.
	for _, se := range subs {
		if !se.opt.Interrupt || !matches(se.opt.Condition, ev) {
			continue
		}
		if err := se.fn(ev); err != nil {
			if errors.Is(err, ErrHandled) {
				return nil
			}
			return err
		}
	}
	// A swallowed event never reaches the transcript; everything else persists
	// before the normal subscribers see it (Python parity: persistence is the
	// first-registered normal subscriber, after the interrupters).
	if err := m.persist(ev); err != nil {
		return err
	}
	var firstErr error
	for _, se := range subs {
		if se.opt.Interrupt || !matches(se.opt.Condition, ev) {
			continue
		}
		if err := se.fn(ev); err != nil && firstErr == nil {
			m.logger.Error("stream: subscriber failed", "event", ev.EventType(), "err", err)
			firstErr = err
		}
	}
	return firstErr
}

func matches(c events.Condition, ev events.Event) bool {
	return c == nil || c(ev)
}

func (m *MemoryStream) persist(ev events.Event) error {
	if ev.Transient() && !m.persistAll {
		return nil
	}
	return m.historyAppend(ev)
}

func (m *MemoryStream) historyAppend(ev events.Event) error {
	return m.history.Append(ev)
}

// Enqueue appends a ModelRequest to the inbox and announces MessageEnqueued.
func (m *MemoryStream) Enqueue(parts ...events.Input) error {
	req := &events.ModelRequest{Parts: parts}
	m.mu.Lock()
	m.pending = append(m.pending, req)
	m.mu.Unlock()
	return m.Send(&events.MessageEnqueued{})
}

// Pending returns a snapshot of the inbox.
func (m *MemoryStream) Pending() []*events.ModelRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*events.ModelRequest, len(m.pending))
	copy(out, m.pending)
	return out
}

// Drain empties the inbox and returns its contents.
func (m *MemoryStream) Drain() []*events.ModelRequest {
	m.mu.Lock()
	out := m.pending
	m.pending = nil
	m.mu.Unlock()
	return out
}

// Subscribe registers a handler; the returned id cancels it via Unsubscribe.
func (m *MemoryStream) Subscribe(fn Subscriber, opts SubscribeOptions) SubID {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextSub++
	id := m.nextSub
	m.subs = append(m.subs, subEntry{id: id, fn: fn, opt: opts})
	return id
}

// Unsubscribe removes a subscription (unknown ids are ignored).
func (m *MemoryStream) Unsubscribe(id SubID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, se := range m.subs {
		if se.id == id {
			m.subs = append(m.subs[:i], m.subs[i+1:]...)
			return
		}
	}
}

// Get waits for the first incoming event matching c, honoring ctx cancellation.
func (m *MemoryStream) Get(ctx context.Context, c events.Condition) (events.Event, error) {
	ch := make(chan events.Event, 4)
	id := m.Subscribe(func(ev events.Event) error {
		select {
		case ch <- ev:
		default:
		}
		return nil
	}, SubscribeOptions{Condition: c})
	defer m.Unsubscribe(id)
	select {
	case ev := <-ch:
		return ev, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// SpawnBackground runs fn on a goroutine tracked by the stream.
func (m *MemoryStream) SpawnBackground(ctx context.Context, fn func(context.Context)) {
	m.bgWG.Add(1)
	go func() {
		defer m.bgWG.Done()
		defer func() {
			if r := recover(); r != nil {
				m.logger.Error("stream: background task panicked", "panic", r)
			}
		}()
		fn(ctx)
	}()
}

// WaitBackground blocks until all spawned background tasks finished.
func (m *MemoryStream) WaitBackground() { m.bgWG.Wait() }

// ID returns the stream id.
func (m *MemoryStream) ID() string { return m.id }

// History returns the transcript handle.
func (m *MemoryStream) History() *History { return m.history }

// Where returns a filtered view sharing the parent's id, history and inbox.
func (m *MemoryStream) Where(c events.Condition) Stream {
	return &SubStream{parent: m, cond: c}
}

// SubStream is a condition-filtered view of a Stream.
type SubStream struct {
	parent Stream
	cond   events.Condition
}

func (s *SubStream) ID() string                      { return s.parent.ID() }
func (s *SubStream) History() *History               { return s.parent.History() }
func (s *SubStream) Pending() []*events.ModelRequest { return s.parent.Pending() }
func (s *SubStream) Drain() []*events.ModelRequest   { return s.parent.Drain() }
func (s *SubStream) Get(ctx context.Context, c events.Condition) (events.Event, error) {
	if c == nil {
		c = s.cond
	} else if s.cond != nil {
		both := c
		c = func(ev events.Event) bool { return s.cond(ev) && both(ev) }
	}
	return s.parent.Get(ctx, c)
}
func (s *SubStream) SpawnBackground(ctx context.Context, fn func(context.Context)) {
	s.parent.SpawnBackground(ctx, fn)
}
func (s *SubStream) WaitBackground() { s.parent.WaitBackground() }

func (s *SubStream) Send(ev events.Event) error {
	if !matches(s.cond, ev) {
		return nil
	}
	return s.parent.Send(ev)
}

func (s *SubStream) Enqueue(parts ...events.Input) error {
	if len(parts) == 0 {
		return nil
	}
	// The inbox is shared with the parent; a filtered view does not enqueue.
	return s.parent.Enqueue(parts...)
}

func (s *SubStream) Subscribe(fn Subscriber, opts SubscribeOptions) SubID {
	if s.cond != nil {
		outer := opts.Condition
		opts.Condition = func(ev events.Event) bool {
			return s.cond(ev) && (outer == nil || outer(ev))
		}
	}
	return s.parent.Subscribe(fn, opts)
}

func (s *SubStream) Unsubscribe(id SubID) { s.parent.Unsubscribe(id) }

// Where narrows the view further.
func (s *SubStream) Where(c events.Condition) Stream {
	if s.cond == nil {
		return &SubStream{parent: s.parent, cond: c}
	}
	outer := c
	return &SubStream{parent: s.parent, cond: func(ev events.Event) bool { return s.cond(ev) && outer(ev) }}
}

// LoadHistory builds a stream over existing transcript events (resume parity:
// Agent.resume seeds history, all but the last event; the last is the re-entry trigger).
func LoadHistory(id string, storage Storage, evs []events.Event) (*MemoryStream, error) {
	m := NewMemoryStream(WithStreamID(id), WithStorage(storage))
	if err := m.history.Replace(evs); err != nil {
		return nil, err
	}
	return m, nil
}
