package knowledge

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"ago/events"
)

// LogsDir is the virtual directory holding stream event logs (store path
// "logs/<streamID>.jsonl").
//
// AGO-DEVIATION: Python's LOG_PREFIX is "/log/"; the porting brief pins
// "logs/<streamID>.jsonl".
const LogsDir = "logs"

// LogPath is the store path of one stream's event log.
func LogPath(streamID string) string {
	return "/" + LogsDir + "/" + streamID + ".jsonl"
}

// EventLogWriter persists stream events to a Store as an append-only JSONL log
// (the hub WAL and the agent event log live here). Each event is one JSON line
// produced by events.MarshalEvent (the "@type" discriminator makes the line
// self-describing); Replay restores them with events.UnmarshalEvent.
//
// EventLogWriter holds no mutable state; concurrent use is as safe as the
// underlying store. Appends of one line go through Store.Append, so ordering is
// guaranteed only per store — wrap the store in LockedStore when several
// goroutines append to the same stream.
type EventLogWriter struct {
	store Store
}

// NewEventLogWriter returns a writer over store.
func NewEventLogWriter(store Store) *EventLogWriter {
	return &EventLogWriter{store: store}
}

// AppendEvent appends one event as a single JSON line to logs/<streamID>.jsonl.
func (w *EventLogWriter) AppendEvent(streamID string, ev events.Event) error {
	line, err := events.MarshalEvent(ev)
	if err != nil {
		return fmt.Errorf("knowledge: marshal event for stream %q: %w", streamID, err)
	}
	return w.store.Append(LogPath(streamID), append(line, '\n'))
}

// Replay loads every event logged for streamID, in append order. Broken lines —
// invalid JSON, a missing "@type", or a payload that fails to decode — are
// skipped. A stream with no log replays as empty.
func (w *EventLogWriter) Replay(streamID string) ([]events.Event, error) {
	raw, err := w.store.Read(LogPath(streamID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return []events.Event{}, nil
		}
		return nil, err
	}
	out := make([]events.Event, 0)
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		ev, err := events.UnmarshalEvent([]byte(line))
		if err != nil {
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}

// Streams returns the sorted ids of all streams that have a log file.
func (w *EventLogWriter) Streams() ([]string, error) {
	entries, err := w.store.List("/" + LogsDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !strings.HasSuffix(e, ".jsonl") {
			continue
		}
		out = append(out, strings.TrimSuffix(e, ".jsonl"))
	}
	sort.Strings(out)
	return out, nil
}
