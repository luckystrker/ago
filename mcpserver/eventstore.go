package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// ErrUnknownEvent reports a replay request naming an event the store does not
// hold (never stored, or evicted by the store's own bound).
var ErrUnknownEvent = errors.New("mcpserver: unknown event id")

// EventStore makes streamable-HTTP streams resumable: every message the server
// sends on a stream is stored under an event id, and a client reconnecting with
// Last-Event-ID receives what it missed (Python parity: mcp.server.
// streamable_http.EventStore). Implementations must be safe for concurrent use.
type EventStore interface {
	// StoreEvent stores one message of a stream and returns its event id. A nil
	// message is the priming event that opens a resumable stream: a cursor with
	// no payload, so a client that disconnects before anything else arrived can
	// still resume. Ids must be unique across streams.
	StoreEvent(ctx context.Context, streamID string, message json.RawMessage) (string, error)
	// ReplayEventsAfter calls send for every event stored after lastEventID on
	// the same stream, in order, and returns that stream's id. ErrUnknownEvent
	// (wrapped) means lastEventID is not held.
	ReplayEventsAfter(ctx context.Context, lastEventID string, send func(eventID string, message json.RawMessage) error) (string, error)
}

// StreamForgetter is implemented by an EventStore that can drop the streams of a
// finished session. The server calls ForgetStreams with the session's id and a
// trailing "/" (every stream id of a session starts with it) when the session
// ends — DELETE, idle reaping, Server.Close — so a long-running server does not
// keep the events of sessions nobody can resume any more.
type StreamForgetter interface {
	ForgetStreams(ctx context.Context, prefix string)
}

// defaultStoreStreams bounds the streams a MemoryEventStore holds when no bound is
// set with WithMaxStreams.
const defaultStoreStreams = 10000

// MemoryEventStore is an in-process EventStore that keeps the most recent events
// of each stream, and the most recently written streams: past the stream bound
// the stream written longest ago is dropped whole (a client resuming it gets
// ErrUnknownEvent), and a session's streams go when the session ends
// (StreamForgetter). Memory is therefore bounded by streams × events per stream
// however long the server runs. It is what a single-replica deployment and the
// tests use; a shared store is the caller's to supply.
type MemoryEventStore struct {
	mu         sync.Mutex
	maxPerKey  int
	maxStreams int
	next       uint64
	streams    map[string][]storedEvent
	written    map[string]uint64 // stream id → counter value of its latest write
	owner      map[string]string // event id → stream id
}

type storedEvent struct {
	id  string
	msg json.RawMessage // nil: priming
}

// NewMemoryEventStore builds a store keeping up to maxPerStream events per
// stream (zero means 1000); older events are dropped, and a client resuming from
// a dropped id gets ErrUnknownEvent.
func NewMemoryEventStore(maxPerStream int) *MemoryEventStore {
	if maxPerStream <= 0 {
		maxPerStream = 1000
	}
	return &MemoryEventStore{maxPerKey: maxPerStream, maxStreams: defaultStoreStreams,
		streams: map[string][]storedEvent{}, written: map[string]uint64{}, owner: map[string]string{}}
}

// WithMaxStreams bounds how many streams the store keeps (zero or less means the
// default, 10,000) and returns the store.
func (m *MemoryEventStore) WithMaxStreams(n int) *MemoryEventStore {
	if n <= 0 {
		n = defaultStoreStreams
	}
	m.mu.Lock()
	m.maxStreams = n
	m.mu.Unlock()
	return m
}

func (m *MemoryEventStore) dropStreamLocked(id string) {
	for _, ev := range m.streams[id] {
		delete(m.owner, ev.id)
	}
	delete(m.streams, id)
	delete(m.written, id)
}

// ForgetStreams implements StreamForgetter.
func (m *MemoryEventStore) ForgetStreams(_ context.Context, prefix string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.streams {
		if strings.HasPrefix(id, prefix) {
			m.dropStreamLocked(id)
		}
	}
}

// streamCount reports how many streams are held.
func (m *MemoryEventStore) streamCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.streams)
}

// StoreEvent implements EventStore.
func (m *MemoryEventStore) StoreEvent(_ context.Context, streamID string, message json.RawMessage) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	id := strconv.FormatUint(m.next, 10) + ":" + streamID
	evs := append(m.streams[streamID], storedEvent{id: id, msg: message})
	if len(evs) > m.maxPerKey {
		for _, dropped := range evs[:len(evs)-m.maxPerKey] {
			delete(m.owner, dropped.id)
		}
		evs = append([]storedEvent(nil), evs[len(evs)-m.maxPerKey:]...)
	}
	m.streams[streamID] = evs
	m.owner[id] = streamID
	m.written[streamID] = m.next
	for len(m.streams) > m.maxStreams {
		oldest, at := "", uint64(0)
		for sid, w := range m.written {
			if sid != streamID && (oldest == "" || w < at) {
				oldest, at = sid, w
			}
		}
		if oldest == "" {
			break
		}
		m.dropStreamLocked(oldest)
	}
	return id, nil
}

// ReplayEventsAfter implements EventStore.
func (m *MemoryEventStore) ReplayEventsAfter(_ context.Context, lastEventID string, send func(string, json.RawMessage) error) (string, error) {
	m.mu.Lock()
	streamID, ok := m.owner[lastEventID]
	if !ok {
		m.mu.Unlock()
		return "", fmt.Errorf("%w: %q", ErrUnknownEvent, lastEventID)
	}
	var tail []storedEvent
	for i, ev := range m.streams[streamID] {
		if ev.id == lastEventID {
			tail = append([]storedEvent(nil), m.streams[streamID][i+1:]...)
			break
		}
	}
	m.mu.Unlock()
	for _, ev := range tail {
		if ev.msg == nil {
			continue // a priming event carries nothing to replay
		}
		if err := send(ev.id, ev.msg); err != nil {
			return streamID, err
		}
	}
	return streamID, nil
}
