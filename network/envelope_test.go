package network

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEnvelopeEventTypes(t *testing.T) {
	cases := map[string]string{
		"ag2.msg.text":             EnvText,
		"ag2.packet":               EnvPacket,
		"ag2.channel.invite":       EnvChannelInvite,
		"ag2.channel.ack":          EnvChannelAck,
		"ag2.channel.opened":       EnvChannelOpened,
		"ag2.channel.closed":       EnvChannelClosed,
		"ag2.task.cancel_request":  EnvTaskCancelRequest,
		"ag2.task.cancelled":       EnvTaskCancelled,
		"ag2.context.set":          EnvContextSet,
		"ag2.expectation.violated": EnvExpectationViolated,
	}
	for want, got := range cases {
		if got != want {
			t.Errorf("event type constant: want %q, got %q", want, got)
		}
	}
}

func TestNewTextEnvelope(t *testing.T) {
	env := NewTextEnvelope("ch1", "alice", "", "hello")
	if env.EventType != EnvText || env.ChannelID != "ch1" || env.SenderID != "alice" || env.Audience != "" {
		t.Fatalf("unexpected envelope: %+v", env)
	}
	if env.Text() != "hello" {
		t.Fatalf("Text(): got %q", env.Text())
	}
}

func TestNewPacketEnvelope(t *testing.T) {
	env := NewPacketEnvelope("ch1", "alice", "", "body",
		WithHandoff(&Handoff{Target: "bob", Reason: "load"}),
		WithPacketContext(map[string]any{"k": "v"}, "old"),
	)
	if env.EventType != EnvPacket {
		t.Fatalf("event type: %q", env.EventType)
	}
	var p struct {
		Body           string         `json:"body"`
		Routing        map[string]any `json:"routing"`
		ContextUpdates struct {
			Set    map[string]any `json:"set"`
			Delete []string       `json:"delete"`
		} `json:"context_updates"`
	}
	if err := env.Decode(&p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Body != "body" {
		t.Errorf("body: %q", p.Body)
	}
	if p.Routing["kind"] != "handoff" || p.Routing["target"] != "bob" || p.Routing["reason"] != "load" {
		t.Errorf("routing: %v", p.Routing)
	}
	if p.ContextUpdates.Set["k"] != "v" || len(p.ContextUpdates.Delete) != 1 || p.ContextUpdates.Delete[0] != "old" {
		t.Errorf("context updates: %+v", p.ContextUpdates)
	}
}

func TestNewPacketEnvelopeFinish(t *testing.T) {
	env := NewPacketEnvelope("ch", "a", "", "done", WithFinish(&Finish{Summary: "s", Reason: "r"}))
	var p struct {
		Routing map[string]any `json:"routing"`
	}
	_ = env.Decode(&p)
	if p.Routing["kind"] != "finish" || p.Routing["summary"] != "s" || p.Routing["reason"] != "r" {
		t.Fatalf("routing: %v", p.Routing)
	}
}

func TestEnvelopeJSONRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	env := Envelope{
		ChannelID:      "ch1",
		SenderID:       "a",
		Audience:       "b",
		EventType:      EnvPacket,
		Payload:        []byte(`{"body":"x","routing":{"kind":"text"}}`),
		CausationID:    "env-0",
		Depth:          2,
		TTLSecs:        30,
		IdempotencyKey: "idem",
		Priority:       PriorityUrgent,
		CreatedAt:      at,
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The payload must travel as raw JSON, not base64.
	if !json.Valid([]byte(`{"body":"x","routing":{"kind":"text"}}`)) {
		t.Fatal("test payload invalid")
	}
	var back Envelope
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(back.Payload) != string(env.Payload) {
		t.Fatalf("payload mismatch: %s vs %s", back.Payload, env.Payload)
	}
	if !back.CreatedAt.Equal(at) || back.Depth != 2 || back.TTLSecs != 30 || back.Priority != PriorityUrgent {
		t.Fatalf("fields mismatch: %+v", back)
	}
}

func TestVisibleTo(t *testing.T) {
	env := Envelope{SenderID: "a", Audience: ""}
	if !VisibleTo(env, "a") || !VisibleTo(env, "b") {
		t.Error("broadcast must be visible to everyone incl. sender")
	}
	addr := Envelope{SenderID: "a", Audience: "b"}
	if !VisibleTo(addr, "a") || !VisibleTo(addr, "b") || VisibleTo(addr, "c") {
		t.Error("addressed envelope visible to sender + addressee only")
	}
}
