// Package events is the event vocabulary of AGo: every observation and action in a
// conversation is an Event flowing on a Stream.
//
// Parity with Python AG2 ag2/events/: one file here per module there. The Python
// condition-DSL and descriptor fields are replaced by plain structs plus predicate
// combinators (Condition). See docs/specs/core.md and docs/DECISIONS.md D-005.
package events

import (
	"reflect"
	"time"
)

// Event is implemented by every event struct. Type identity travels with the value:
// EventType returns the stable type id (the Python class name) used for serialization,
// registry lookup and wire compatibility.
type Event interface {
	EventType() string
	At() time.Time
	Transient() bool      // ephemeral streaming artifacts: not persisted by default
	Conversational() bool // part of the durable transcript replayed to the model
}

// Base is embedded in every event struct and carries the creation timestamp.
// A zero CreatedAt is stamped with the current time when the event is first sent.
// Events are passed as pointers so the stream can stamp them in place.
type Base struct {
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// At implements the timestamp accessor promoted to every event struct.
func (b Base) At() time.Time { return b.CreatedAt }

// StampNow fills CreatedAt when it is zero. Called by stream.Send.
func (b *Base) StampNow(now time.Time) {
	if b.CreatedAt.IsZero() {
		b.CreatedAt = now
	}
}

// sourceEventer is implemented by events that represent another authored event:
// observation envelopes and routed deliveries. It is unexported so only this
// package can define the indirection; callers use SourceEvent.
type sourceEventer interface {
	sourceEvent() Event
}

// sourceEventDepth bounds a malformed self-referential envelope chain. Valid
// delegation trees are shallow; the limit keeps a cycle from hanging a reader.
const sourceEventDepth = 64

// SourceEvent returns the event that authored ev. For an ordinary event it is ev
// itself; for a ChildEvent or a routed delivery it is the wrapped authored
// event, including through nested observations. A DownEvent is its own source:
// it audits an intervention and its payload must not be counted twice.
//
// AGO-DEVIATION: Python exposes this as a BaseEvent property; Go uses a
// function so ordinary events need no extra method (D-091).
func SourceEvent(ev Event) Event {
	for i := 0; i < sourceEventDepth; i++ {
		if ev == nil {
			return nil
		}
		s, ok := ev.(sourceEventer)
		if !ok {
			return ev
		}
		ev = s.sourceEvent()
	}
	return ev
}

// AsValue dereferences pointer events so type switches can match one form,
// unwrapping delivery markers so type-based conditions see the authored type.
// Value-receiver methods keep the result a valid Event.
func AsValue(ev Event) Event {
	for i := 0; i < sourceEventDepth; i++ {
		switch e := ev.(type) {
		case *RoutedEvent:
			if e == nil {
				return nil
			}
			ev = e.Inner
			continue
		}
		rv := reflect.ValueOf(ev)
		if rv.Kind() == reflect.Pointer && !rv.IsNil() {
			if v, ok := rv.Elem().Interface().(Event); ok {
				return v
			}
		}
		return ev
	}
	return ev
}
