// Package network ports the functional single-host subset of AG2's
// multi-agent messaging layer (docs/specs/events-network.md §4, D-010):
// envelopes, channels, passports and rules, an in-process hub with an
// optional write-ahead log, protocol adapters (consulting, conversation,
// discussion, workflow), and client handles for agents and humans.
//
// WebSocket links and federation are out of the subset; the frame/link wire
// vocabulary lives in the transport subpackage.
package network

import (
	"encoding/json"
	"fmt"
	"time"
)

// Envelope event types. The ag2.* wire vocabulary is preserved verbatim for
// wire compatibility (D-010).
const (
	EnvText                = "ag2.msg.text"
	EnvPacket              = "ag2.packet"
	EnvChannelInvite       = "ag2.channel.invite"
	EnvChannelAck          = "ag2.channel.ack" // Python: ag2.channel.invite.ack (brief pins the short form)
	EnvChannelReject       = "ag2.channel.invite.reject"
	EnvChannelOpened       = "ag2.channel.opened"
	EnvChannelClosed       = "ag2.channel.closed"
	EnvChannelExpired      = "ag2.channel.expired"
	EnvTaskCancelRequest   = "ag2.task.cancel_request"
	EnvTaskCancelled       = "ag2.task.cancelled"
	EnvContextSet          = "ag2.context.set"
	EnvExpectationViolated = "ag2.expectation.violated"
)

// Envelope delivery priorities (Python is a string literal; AGo uses small
// ints — the value is advisory metadata this subset never schedules on).
const (
	PriorityBackground = -1
	PriorityNormal     = 0
	PriorityUrgent     = 1
)

// Envelope is the wire shape for every message between participants. Every
// exchange happens inside a channel and the carrier is an envelope.
//
// AGO-DEVIATION vs the Python Envelope: Audience is a single id ("" =
// broadcast) instead of a list, the payload dict is raw JSON bytes
// (Payload), and hub-stamped fields keep Go names (ID, TTLSecs, Priority
// int). Field-level wire compatibility with ag2 is not preserved — the
// event-type vocabulary is (D-010).
type Envelope struct {
	ID             string    `json:"id"` // hub-stamped on accept; empty on the sender side
	ChannelID      string    `json:"channel_id"`
	SenderID       string    `json:"sender_id"`
	Audience       string    `json:"audience,omitempty"` // "" = broadcast within the channel
	EventType      string    `json:"event_type"`
	Payload        []byte    `json:"payload,omitempty"` // JSON event data, shaped per event type
	CausationID    string    `json:"causation_id,omitempty"`
	Depth          int       `json:"depth,omitempty"` // delegation hop count
	TTLSecs        int       `json:"ttl_secs,omitempty"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	Priority       int       `json:"priority,omitempty"`
	CreatedAt      time.Time `json:"created_at,omitempty"` // hub-stamped on accept
}

// envelopeWire mirrors Envelope with the payload embedded as raw JSON
// instead of base64 ([]byte's default JSON encoding).
type envelopeWire struct {
	ID             string          `json:"id"`
	ChannelID      string          `json:"channel_id"`
	SenderID       string          `json:"sender_id"`
	Audience       string          `json:"audience,omitempty"`
	EventType      string          `json:"event_type"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	CausationID    string          `json:"causation_id,omitempty"`
	Depth          int             `json:"depth,omitempty"`
	TTLSecs        int             `json:"ttl_secs,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	Priority       int             `json:"priority,omitempty"`
	CreatedAt      time.Time       `json:"created_at,omitempty"`
}

// MarshalJSON embeds the payload as raw JSON so envelopes stay readable on
// the wire and in the WAL.
func (e Envelope) MarshalJSON() ([]byte, error) {
	return json.Marshal(envelopeWire{
		ID: e.ID, ChannelID: e.ChannelID, SenderID: e.SenderID,
		Audience: e.Audience, EventType: e.EventType, Payload: e.Payload,
		CausationID: e.CausationID, Depth: e.Depth, TTLSecs: e.TTLSecs,
		IdempotencyKey: e.IdempotencyKey, Priority: e.Priority, CreatedAt: e.CreatedAt,
	})
}

// UnmarshalJSON reverses MarshalJSON.
func (e *Envelope) UnmarshalJSON(data []byte) error {
	var w envelopeWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	e.ID, e.ChannelID, e.SenderID = w.ID, w.ChannelID, w.SenderID
	e.Audience, e.EventType = w.Audience, w.EventType
	e.Payload = []byte(w.Payload)
	e.CausationID, e.Depth, e.TTLSecs = w.CausationID, w.Depth, w.TTLSecs
	e.IdempotencyKey, e.Priority, e.CreatedAt = w.IdempotencyKey, w.Priority, w.CreatedAt
	return nil
}

// Decode unmarshals the payload into v. An empty payload decodes into the
// zero value of v without error.
func (e Envelope) Decode(v any) error {
	if len(e.Payload) == 0 {
		return nil
	}
	return json.Unmarshal(e.Payload, v)
}

// Text returns the text body of an EnvText envelope (payload {"text": ...}),
// or "" for any other shape.
func (e Envelope) Text() string {
	var p struct {
		Text string `json:"text"`
	}
	_ = e.Decode(&p)
	return p.Text
}

// VisibleTo is the pure delivery / view-filtering predicate: the sender
// always sees their own envelope, broadcasts are visible to everyone,
// addressed envelopes only to the addressee.
func VisibleTo(env Envelope, participantID string) bool {
	if env.SenderID == participantID {
		return true
	}
	if env.Audience == "" {
		return true
	}
	return env.Audience == participantID
}

// --- envelope constructors ---------------------------------------------------

// NewTextEnvelope builds an EnvText envelope. audience "" broadcasts.
func NewTextEnvelope(channelID, senderID, audience, text string) Envelope {
	payload, _ := json.Marshal(map[string]string{"text": text})
	return Envelope{
		ChannelID: channelID,
		SenderID:  senderID,
		Audience:  audience,
		EventType: EnvText,
		Payload:   payload,
	}
}

// PacketOption decorates NewPacketEnvelope envelopes.
type PacketOption func(*packetOpts)

type packetOpts struct {
	routing map[string]any
	set     map[string]any
	del     []string
}

// WithPacketRouting sets the packet's routing field verbatim (kind/tool/
// target/reason/summary keys). Prefer WithHandoff / WithFinish which build
// it.
func WithPacketRouting(routing map[string]any) PacketOption {
	return func(o *packetOpts) { o.routing = routing }
}

// WithHandoff encodes a dynamic handoff: the next speaker is target
// (a Passport name or agent id), resolved by the workflow adapter at fold
// time.
func WithHandoff(h *Handoff) PacketOption {
	return func(o *packetOpts) {
		r := map[string]any{"kind": "handoff", "tool": "handoff"}
		if h != nil {
			r["target"] = h.Target
			r["reason"] = h.Reason
		}
		o.routing = r
	}
}

// WithFinish encodes a clean channel termination driven by a tool decision.
func WithFinish(f *Finish) PacketOption {
	return func(o *packetOpts) {
		r := map[string]any{"kind": "finish", "reason": "finished", "summary": ""}
		if f != nil {
			if f.Reason != "" {
				r["reason"] = f.Reason
			}
			r["summary"] = f.Summary
		}
		o.routing = r
	}
}

// WithPacketContext carries channel-scoped context variable mutations on the
// packet (applied before the next speaker is selected).
func WithPacketContext(set map[string]any, del ...string) PacketOption {
	return func(o *packetOpts) { o.set, o.del = set, del }
}

// NewPacketEnvelope builds an EnvPacket envelope — one agent round captured
// atomically (body + optional routing + context mutations).
func NewPacketEnvelope(channelID, senderID, audience, body string, opts ...PacketOption) Envelope {
	var o packetOpts
	for _, opt := range opts {
		opt(&o)
	}
	payload := map[string]any{"body": body}
	if o.routing != nil {
		payload["routing"] = o.routing
	}
	if o.set != nil || len(o.del) > 0 {
		payload["context_updates"] = map[string]any{"set": o.set, "delete": o.del}
	}
	data, _ := json.Marshal(payload)
	return Envelope{
		ChannelID: channelID,
		SenderID:  senderID,
		Audience:  audience,
		EventType: EnvPacket,
		Payload:   data,
	}
}

// NewContextSetEnvelope builds an EnvContextSet envelope mutating
// channel-scoped context variables outside a packet round.
func NewContextSetEnvelope(channelID, senderID string, set map[string]any, del ...string) Envelope {
	payload, _ := json.Marshal(map[string]any{"set": set, "delete": del})
	return Envelope{
		ChannelID: channelID,
		SenderID:  senderID,
		EventType: EnvContextSet,
		Payload:   payload,
	}
}

// NewChannelAck answers a channel invite (accept when reject is false,
// decline otherwise). The ack causally references the invite.
func NewChannelAck(invite Envelope, senderID string, reject bool, reason string) Envelope {
	payload, _ := json.Marshal(map[string]any{"channel_id": invite.ChannelID, "reject": reject, "reason": reason})
	kind := EnvChannelAck
	if reject {
		kind = EnvChannelReject
	}
	return Envelope{
		ChannelID:   invite.ChannelID,
		SenderID:    senderID,
		EventType:   kind,
		Payload:     payload,
		CausationID: invite.ID,
	}
}

// ReplyDepth is the delegation depth of a reply to trigger: one hop deeper.
func ReplyDepth(trigger Envelope) int {
	return trigger.Depth + 1
}

// packetPayload is the decoded shape of an EnvPacket payload.
type packetPayload struct {
	Body           string         `json:"body"`
	Routing        map[string]any `json:"routing,omitempty"`
	ContextUpdates struct {
		Set    map[string]any `json:"set"`
		Delete []string       `json:"delete"`
	} `json:"context_updates,omitempty"`
}

// decodePacket decodes an EnvPacket payload; non-packet envelopes decode to
// an empty body with no routing.
func decodePacket(env Envelope) packetPayload {
	var p packetPayload
	if env.EventType != EnvPacket {
		return p
	}
	_ = env.Decode(&p)
	return p
}

// String renders a short diagnostic form.
func (e Envelope) String() string {
	aud := e.Audience
	if aud == "" {
		aud = "*"
	}
	return fmt.Sprintf("Envelope{id=%s ch=%s from=%s to=%s type=%s}", e.ID, e.ChannelID, e.SenderID, aud, e.EventType)
}
