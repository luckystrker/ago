package events

import (
	"encoding/json"
	"time"
)

// Direction values for observation envelopes. An envelope travels "up" from a
// descendant; DownEvent audits an ancestor's intervention travelling "down".
const (
	DirectionUp   = "up"
	DirectionDown = "down"
)

// ChildEvent is an observation of an event authored by a descendant. It is
// never a local trigger: observers on an ancestor can inspect it, but the
// ancestor's tools, model loop and HITL subscribers must not react to it.
//
// Zero values of Direction and Path are normalized on the wire ("up", []).
// The struct is immutable after construction and safe for concurrent reads.
//
// AGO-DEVIATION: Python dataclass defaults fill direction/path at construction;
// Go normalizes them in the constructors and on the wire (D-091).
type ChildEvent struct {
	Base
	Inner         Event    `json:"inner"`
	TaskID        string   `json:"task_id"`
	Path          []string `json:"path"`
	AgentName     string   `json:"agent_name"`
	OriginEventID string   `json:"origin_event_id"`
	Direction     string   `json:"direction"`
}

// NewChildEvent builds an uplink observation with the default direction.
func NewChildEvent(inner Event, taskID string, path []string, agentName, originEventID string) *ChildEvent {
	return &ChildEvent{
		Inner:         inner,
		TaskID:        taskID,
		Path:          normalizePath(path),
		AgentName:     agentName,
		OriginEventID: originEventID,
		Direction:     DirectionUp,
	}
}

// normalizePath keeps an empty path representable as an empty JSON array rather
// than null; callers may pass nil.
func normalizePath(path []string) []string {
	if path == nil {
		return []string{}
	}
	return path
}

func (ChildEvent) EventType() string    { return "ChildEvent" }
func (ChildEvent) Transient() bool      { return false }
func (ChildEvent) Conversational() bool { return false }

// sourceEvent lets SourceEvent resolve the authored event through any nesting.
func (c ChildEvent) sourceEvent() Event { return c.Inner }

// childEventWire is the JSON shape; Inner keeps its own "@type" discriminator.
type childEventWire struct {
	Base
	Inner         json.RawMessage `json:"inner"`
	TaskID        string          `json:"task_id"`
	Path          []string        `json:"path"`
	AgentName     string          `json:"agent_name"`
	OriginEventID string          `json:"origin_event_id"`
	Direction     string          `json:"direction"`
}

// MarshalJSON encodes the nested event with MarshalEvent so it stays
// deserializable on its own; the delivery marker is never part of the payload.
func (c ChildEvent) MarshalJSON() ([]byte, error) {
	inner, err := marshalNestedEvent(c.Inner)
	if err != nil {
		return nil, err
	}
	direction := c.Direction
	if direction == "" {
		direction = DirectionUp
	}
	path := c.Path
	if path == nil {
		path = []string{}
	}
	return json.Marshal(childEventWire{
		Base:          c.Base,
		Inner:         inner,
		TaskID:        c.TaskID,
		Path:          path,
		AgentName:     c.AgentName,
		OriginEventID: c.OriginEventID,
		Direction:     direction,
	})
}

// UnmarshalJSON rebuilds the nested event through UnmarshalEvent.
func (c *ChildEvent) UnmarshalJSON(data []byte) error {
	var wire childEventWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	inner, err := unmarshalNestedEvent(wire.Inner)
	if err != nil {
		return err
	}
	*c = ChildEvent{
		Base:          wire.Base,
		Inner:         inner,
		TaskID:        wire.TaskID,
		Path:          wire.Path,
		AgentName:     wire.AgentName,
		OriginEventID: wire.OriginEventID,
		Direction:     wire.Direction,
	}
	if c.Direction == "" {
		c.Direction = DirectionUp
	}
	return nil
}

// TransientChildEvent preserves the descendant's storage policy for a
// transient inner event.
type TransientChildEvent struct {
	ChildEvent
}

func (TransientChildEvent) EventType() string { return "TransientChildEvent" }
func (TransientChildEvent) Transient() bool   { return true }

// DeliveryFailed reports that a downlink could not reach an executing
// delegation. It is an audit record, not a trigger.
type DeliveryFailed struct {
	Base
	// AGO-DEVIATION: upstream serializes task_id as null when absent; AGo's
	// optional string fields omit empty values across the event vocabulary (D-091).
	TaskID string `json:"task_id,omitempty"`
	Reason string `json:"reason"`
}

func (DeliveryFailed) EventType() string    { return "DeliveryFailed" }
func (DeliveryFailed) Transient() bool      { return false }
func (DeliveryFailed) Conversational() bool { return false }

// DownEvent audits an ancestor's intervention. The wrapped event itself is
// delivered normally to the descendant; the audit is not conversational and is
// its own source (the payload must not be counted again).
type DownEvent struct {
	Base
	Inner     Event    `json:"inner"`
	TaskID    string   `json:"task_id"`
	Path      []string `json:"path"`
	AgentName string   `json:"agent_name"`
	Direction string   `json:"direction"`
	// AGO-DEVIATION: upstream serializes origin_task_id as null when absent;
	// AGo omits empty optional strings (D-091).
	OriginTaskID   string   `json:"origin_task_id,omitempty"`
	OriginPath     []string `json:"origin_path"`
	OriginStreamID string   `json:"origin_stream_id"`
}

// NewDownEvent builds an intervention audit with the default direction.
func NewDownEvent(inner Event, taskID string, path []string, agentName string) *DownEvent {
	return &DownEvent{
		Inner:      inner,
		TaskID:     taskID,
		Path:       normalizePath(path),
		AgentName:  agentName,
		Direction:  DirectionDown,
		OriginPath: []string{},
	}
}

func (DownEvent) EventType() string    { return "DownEvent" }
func (DownEvent) Transient() bool      { return false }
func (DownEvent) Conversational() bool { return false }

type downEventWire struct {
	Base
	Inner          json.RawMessage `json:"inner"`
	TaskID         string          `json:"task_id"`
	Path           []string        `json:"path"`
	AgentName      string          `json:"agent_name"`
	Direction      string          `json:"direction"`
	OriginTaskID   string          `json:"origin_task_id,omitempty"`
	OriginPath     []string        `json:"origin_path"`
	OriginStreamID string          `json:"origin_stream_id"`
}

func (d DownEvent) MarshalJSON() ([]byte, error) {
	inner, err := marshalNestedEvent(d.Inner)
	if err != nil {
		return nil, err
	}
	direction := d.Direction
	if direction == "" {
		direction = DirectionDown
	}
	path := d.Path
	if path == nil {
		path = []string{}
	}
	originPath := d.OriginPath
	if originPath == nil {
		originPath = []string{}
	}
	return json.Marshal(downEventWire{
		Base:           d.Base,
		Inner:          inner,
		TaskID:         d.TaskID,
		Path:           path,
		AgentName:      d.AgentName,
		Direction:      direction,
		OriginTaskID:   d.OriginTaskID,
		OriginPath:     originPath,
		OriginStreamID: d.OriginStreamID,
	})
}

func (d *DownEvent) UnmarshalJSON(data []byte) error {
	var wire downEventWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	inner, err := unmarshalNestedEvent(wire.Inner)
	if err != nil {
		return err
	}
	*d = DownEvent{
		Base:           wire.Base,
		Inner:          inner,
		TaskID:         wire.TaskID,
		Path:           wire.Path,
		AgentName:      wire.AgentName,
		Direction:      wire.Direction,
		OriginTaskID:   wire.OriginTaskID,
		OriginPath:     wire.OriginPath,
		OriginStreamID: wire.OriginStreamID,
	}
	if d.Direction == "" {
		d.Direction = DirectionDown
	}
	return nil
}

// TransientDownEvent preserves the storage policy for a transient audit.
type TransientDownEvent struct {
	DownEvent
}

func (TransientDownEvent) EventType() string { return "TransientDownEvent" }
func (TransientDownEvent) Transient() bool   { return true }

// Delivery describes how a copy entered a stream it was not published on.
// A routed copy is never forwarded back to the stream it came from. Stream.Send
// calls Validate before every handler, so a delivery cannot reach a later turn.
// Validate must be safe for concurrent use, and an error means the addressed
// invocation has ended.
//
// AGO-DEVIATION: Python raises from validate and carries the marker in a hidden
// attribute; Go returns an error and uses a wrapper type (D-091).
type Delivery struct {
	Validate func() error
}

// RoutedEvent marks an event delivered from another stream. It is a transport
// wrapper, not a wire type: EventType, timestamps and storage flags delegate to
// the inner event, and MarshalEvent writes only the inner payload.
//
// The wrapper does not clone Inner. Events are pointers (or value structs) and
// the wrapper is immutable; callers stamp provenance before wrapping.
//
// AGO-DEVIATION: Python's routed() returns a shallow copy; Go wraps the
// original value and mutates nothing (D-091).
type RoutedEvent struct {
	Inner    Event
	Delivery Delivery
}

// Routed wraps ev with a delivery marker.
func Routed(ev Event, d Delivery) *RoutedEvent {
	return &RoutedEvent{Inner: ev, Delivery: d}
}

func (r *RoutedEvent) EventType() string {
	if r == nil || r.Inner == nil {
		return "RoutedEvent"
	}
	return r.Inner.EventType()
}

func (r *RoutedEvent) At() time.Time {
	if r == nil || r.Inner == nil {
		return time.Time{}
	}
	return r.Inner.At()
}

// StampNow forwards the stream's timestamp stamp to the authored event, so a
// routed copy persisted without its inner having been sent directly still gets
// a creation time.
func (r *RoutedEvent) StampNow(now time.Time) {
	if r == nil || r.Inner == nil {
		return
	}
	if s, ok := r.Inner.(interface{ StampNow(time.Time) }); ok {
		s.StampNow(now)
	}
}

func (r *RoutedEvent) Transient() bool {
	return r != nil && r.Inner != nil && r.Inner.Transient()
}

func (r *RoutedEvent) Conversational() bool {
	return r != nil && r.Inner != nil && r.Inner.Conversational()
}

func (r *RoutedEvent) sourceEvent() Event {
	if r == nil {
		return nil
	}
	return r.Inner
}

// MarshalJSON unwraps the marker: delivery is process-local and never serialized.
func (r *RoutedEvent) MarshalJSON() ([]byte, error) {
	if r == nil || r.Inner == nil {
		return []byte("null"), nil
	}
	return json.Marshal(r.Inner)
}

// DeliveryOf reports the delivery marker of a routed event.
func DeliveryOf(ev Event) (Delivery, bool) {
	switch e := ev.(type) {
	case *RoutedEvent:
		if e != nil {
			return e.Delivery, true
		}
	}
	return Delivery{}, false
}

// IsRouted reports whether ev is a delivery wrapper.
func IsRouted(ev Event) bool {
	_, ok := DeliveryOf(ev)
	return ok
}

func marshalNestedEvent(ev Event) (json.RawMessage, error) {
	if ev == nil {
		return json.RawMessage("null"), nil
	}
	return MarshalEvent(ev)
}

func unmarshalNestedEvent(raw json.RawMessage) (Event, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	return UnmarshalEvent(raw)
}

func init() {
	RegisterEvent(ChildEvent{})
	RegisterEvent(TransientChildEvent{})
	RegisterEvent(DeliveryFailed{})
	RegisterEvent(DownEvent{})
	RegisterEvent(TransientDownEvent{})
}
