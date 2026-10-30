package policies

import (
	"testing"

	"github.com/luckystrker/ago/events"
)

func TestConversationPolicy(t *testing.T) {
	p := NewConversationPolicy()
	in := []events.Event{
		&events.TextInput{Content: "find it"},
		&events.UsageEvent{Kind: events.UsageKindModelCall},
		&events.ModelResponse{Message: &events.ModelMessage{Content: "hello"}},
		&events.ObserverAlert{Source: "loop", Severity: events.SeverityWarning, Message: "repeat"},
		&events.CompactionSummary{Content: "earlier talk"},
		&events.MessageEnqueued{},
	}
	prompts, got, err := p.Apply([]string{"sys"}, in, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(prompts) != 1 || prompts[0] != "sys" {
		t.Errorf("prompts = %v, want unchanged", prompts)
	}
	want := []string{"TextInput", "ModelResponse", "CompactionSummary"}
	if len(got) != len(want) {
		t.Fatalf("got %d events (%v), want %d", len(got), eventTypesOf(got), len(want))
	}
	for i, ev := range got {
		if ev.EventType() != want[i] {
			t.Errorf("got[%d] = %s, want %s", i, ev.EventType(), want[i])
		}
	}
	if len(in) != 6 {
		t.Errorf("policy mutated its input: %d events left", len(in))
	}
}

func eventTypesOf(evs []events.Event) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = ev.EventType()
	}
	return out
}
