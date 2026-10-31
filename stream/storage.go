// Package stream provides conversation streams: an in-memory pub/sub bus with a
// durable transcript (History) and a pending-message inbox.
//
// Parity with Python AG2 ag2/stream.py + history.py. AGO-DEVIATION (D-013):
// subscribers receive only the event; Python's per-send context argument is
// unnecessary here because AGo registers per-turn closures that capture the
// conversation context. See docs/DECISIONS.md.
package stream

import (
	"fmt"
	"sync"

	"github.com/luckystrker/ago/events"
)

// Storage persists the transcript of one or more streams.
type Storage interface {
	SaveEvent(streamID string, ev events.Event) error
	GetHistory(streamID string) ([]events.Event, error)
	SetHistory(streamID string, evs []events.Event) error
	DropHistory(streamID string) error
}

// MemoryStorage keeps transcripts in process memory.
type MemoryStorage struct {
	mu      sync.Mutex
	streams map[string][]events.Event
}

func NewMemoryStorage() *MemoryStorage {
	return &MemoryStorage{streams: map[string][]events.Event{}}
}

func (m *MemoryStorage) SaveEvent(id string, ev events.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.streams[id] = append(m.streams[id], ev)
	return nil
}

func (m *MemoryStorage) GetHistory(id string) ([]events.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	evs := m.streams[id]
	out := make([]events.Event, len(evs))
	copy(out, evs)
	return out, nil
}

func (m *MemoryStorage) SetHistory(id string, evs []events.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]events.Event, len(evs))
	copy(cp, evs)
	m.streams[id] = cp
	return nil
}

func (m *MemoryStorage) DropHistory(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.streams, id)
	return nil
}

// History is the durable transcript of one stream, backed by a Storage.
type History struct {
	mu       sync.Mutex
	streamID string
	storage  Storage
	// cap is the maximum number of retained events; 0 = unlimited (Python
	// parity). When set, appends evict the oldest events so the transcript
	// cannot grow without bound (audit M-28).
	cap int
}

// NewHistory creates a transcript over storage.
func NewHistory(streamID string, storage Storage) *History {
	if storage == nil {
		storage = NewMemoryStorage()
	}
	return &History{streamID: streamID, storage: storage}
}

// SetCap limits how many events the transcript retains (n <= 0 = unlimited).
// Excess events are dropped from the head on the next write.
func (h *History) SetCap(n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cap = n
}

// Cap returns the retention limit (0 = unlimited).
func (h *History) Cap() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cap
}

// evict trims next to the newest h.cap events (no-op when unlimited).
func (h *History) evict(next []events.Event) []events.Event {
	if h.cap > 0 && len(next) > h.cap {
		return next[len(next)-h.cap:]
	}
	return next
}

// GetEvents returns a snapshot of the transcript.
func (h *History) GetEvents() ([]events.Event, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.storage.GetHistory(h.streamID)
}

// Replace overwrites the transcript (used by compaction and resume).
func (h *History) Replace(evs []events.Event) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.storage.SetHistory(h.streamID, h.evict(evs))
}

// Append adds already-persisted events without re-dispatching (resume, repair).
func (h *History) Append(evs ...events.Event) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	existing, err := h.storage.GetHistory(h.streamID)
	if err != nil {
		return err
	}
	return h.storage.SetHistory(h.streamID, h.evict(append(existing, evs...)))
}

// Drop clears the transcript.
func (h *History) Drop() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.storage.DropHistory(h.streamID)
}

// StreamID returns the owning stream id.
func (h *History) StreamID() string { return h.streamID }

// Storage returns the backing transcript storage (delegation parity: a sub-task
// stream is built over the parent's storage, so both transcripts land in one
// place under their own stream ids).
func (h *History) Storage() Storage { return h.storage }

// CloseUnanswered appends sentinel results for tool calls left unanswered by a
// killed turn (parity: history.close_unanswered_tool_calls).
func (h *History) CloseUnanswered() (int, error) {
	evs, err := h.GetEvents()
	if err != nil {
		return 0, err
	}
	fixes := events.CloseUnansweredToolCalls(evs)
	if len(fixes) == 0 {
		return 0, nil
	}
	asEvents := make([]events.Event, len(fixes))
	for i, f := range fixes {
		fe := f
		asEvents[i] = &fe
	}
	if err := h.Append(asEvents...); err != nil {
		return 0, err
	}
	return len(fixes), nil
}

// RenderTranscript renders events as plain text (debug helper).
func RenderTranscript(evs []events.Event) string {
	out := ""
	for _, ev := range evs {
		if txt := events.RenderForPrompt(ev); txt != "" {
			out += fmt.Sprintf("%s\n", txt)
		}
	}
	return out
}
