package acp

import (
	"encoding/json"
	"strings"
	"testing"
)

// The wire-shape tests pin the dialect this package puts on the wire: the
// Python SDK's spelling (mappers.py block_text(update.content), bridge.py's
// nested request-permission outcome), with the earlier revision's dialect
// still understood on input.

func TestAgentMessageChunkEmitsContent(t *testing.T) {
	data, err := json.Marshal(updateParams{SessionID: "s1", Update: agentMessageChunk("hi")})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, `"content":{"type":"text","text":"hi"}`) {
		t.Fatalf("update = %s, want the block under \"content\"", got)
	}
	if strings.Contains(got, "contentBlock") {
		t.Fatalf("update = %s: \"contentBlock\" must never be emitted", got)
	}
	var p updateParams
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if p.SessionID != "s1" || p.Update.text() != "hi" {
		t.Fatalf("round trip = %+v", p)
	}
}

// TestUpdateAcceptsBothBlockFields: a peer speaking the earlier revision's
// "contentBlock" dialect is understood alongside the SDK's "content".
func TestUpdateAcceptsBothBlockFields(t *testing.T) {
	tests := []struct {
		name string
		wire string
		want string
	}{
		{"sdk content", `{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"sdk"}}}`, "sdk"},
		{"legacy contentBlock", `{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","contentBlock":{"type":"text","text":"legacy"}}}`, "legacy"},
		{"neither", `{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk"}}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p updateParams
			if err := json.Unmarshal([]byte(tt.wire), &p); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got := p.Update.text(); got != tt.want {
				t.Fatalf("text = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDecisionEmitsNestedOutcome(t *testing.T) {
	tests := []struct {
		name string
		in   Decision
		want string
	}{
		{"selected", Decision{Outcome: OutcomeSelected, OptionID: "ok-1"},
			`{"outcome":{"outcome":"selected","optionId":"ok-1"}}`},
		{"cancelled", Decision{Outcome: OutcomeCancelled},
			`{"outcome":{"outcome":"cancelled"}}`},
		// A zero Decision denies: an absent selection must never read as
		// an allow on the wire.
		{"zero", Decision{}, `{"outcome":{"outcome":"cancelled"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if got := string(data); got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestDecisionAcceptsNestedAndFlat(t *testing.T) {
	tests := []struct {
		name string
		wire string
		want Decision
	}{
		{"sdk nested", `{"outcome":{"outcome":"selected","optionId":"a"}}`, Decision{Outcome: OutcomeSelected, OptionID: "a"}},
		{"nested cancelled", `{"outcome":{"outcome":"cancelled"}}`, Decision{Outcome: OutcomeCancelled}},
		{"flat legacy", `{"outcome":"selected","optionId":"a"}`, Decision{Outcome: OutcomeSelected, OptionID: "a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var d Decision
			if err := json.Unmarshal([]byte(tt.wire), &d); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if d != tt.want {
				t.Fatalf("got %+v, want %+v", d, tt.want)
			}
		})
	}
}

func TestDecisionAllowed(t *testing.T) {
	allow := Decision{Outcome: OutcomeSelected, OptionID: "allow"}
	if !allow.Allowed() {
		t.Error("selected allow must be allowed")
	}
	denied := []Decision{
		{Outcome: OutcomeSelected, OptionID: "reject"}, // the synthesized deny
		{Outcome: OutcomeSelected},                     // selected nothing
		{Outcome: OutcomeCancelled, OptionID: "allow"},
	}
	for _, d := range denied {
		if d.Allowed() {
			t.Errorf("%+v must not be allowed", d)
		}
	}
}
