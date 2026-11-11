package usage

import (
	"testing"

	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
)

// TestCollectorGathersLiveRecords pins collect_usage_events parity: a normal
// stream subscription collects source UsageEvents as they are published and
// ignores everything else.
func TestCollectorGathersLiveRecords(t *testing.T) {
	s := stream.NewMemoryStream()
	c := NewCollector()
	s.Subscribe(c.Collect, stream.SubscribeOptions{})

	first := modelCall(10, 4, "claude", "anthropic")
	if err := s.Send(first); err != nil {
		t.Fatalf("send first: %v", err)
	}
	if err := s.Send(&events.TextInput{Content: "not usage"}); err != nil {
		t.Fatalf("send text: %v", err)
	}
	if err := s.Send(modelCall(6, 2, "claude", "anthropic")); err != nil {
		t.Fatalf("send second: %v", err)
	}

	records := c.Records()
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	if records[0].Kind != events.UsageKindModelCall {
		t.Fatalf("first record = %+v", records[0])
	}

	rep := c.Report()
	wantTotal := events.Usage{PromptTokens: events.I64(16), CompletionTokens: events.I64(6)}
	if !usageEqual(rep.Total, wantTotal) {
		t.Fatalf("total = %+v, want %+v", rep.Total, wantTotal)
	}
	if len(rep.Records) != 2 {
		t.Fatalf("report records = %d, want 2", len(rep.Records))
	}
}

// TestCollectorReadsArchivedEnvelope pins that an archived observation envelope
// resolves to the source record it wraps (usage_event_from parity).
func TestCollectorReadsArchivedEnvelope(t *testing.T) {
	s := stream.NewMemoryStream()
	c := NewCollector()
	s.Subscribe(c.Collect, stream.SubscribeOptions{})

	inner := modelCall(3, 1, "gpt", "openai")
	envelope := events.NewChildEvent(inner, "task-1", nil, "child", inner.EventID)
	if err := s.Send(envelope); err != nil {
		t.Fatalf("send envelope: %v", err)
	}

	records := c.Records()
	if len(records) != 1 || records[0].EventID != inner.EventID {
		t.Fatalf("records = %+v, want the wrapped source record", records)
	}
	if rep := c.Report(); !usageEqual(rep.Total, inner.Usage) {
		t.Fatalf("total = %+v, want %+v", rep.Total, inner.Usage)
	}
}

// TestCollectorReportDeduplicates pins the report rules: the collector appends
// every record, but Report counts a source event id once and skips records that
// report no tokens.
func TestCollectorReportDeduplicates(t *testing.T) {
	s := stream.NewMemoryStream()
	c := NewCollector()
	s.Subscribe(c.Collect, stream.SubscribeOptions{})

	dup := modelCall(5, 5, "m", "p")
	dup.EventID = "usage-1"
	again := modelCall(5, 5, "m", "p")
	again.EventID = "usage-1"
	empty := &events.UsageEvent{EventID: "usage-2", Kind: events.UsageKindModelCall}

	for _, ev := range []events.Event{dup, again, empty} {
		if err := s.Send(ev); err != nil {
			t.Fatalf("send: %v", err)
		}
	}

	if got := len(c.Records()); got != 3 {
		t.Fatalf("records = %d, want 3 appended as they arrived", got)
	}
	rep := c.Report()
	if len(rep.Records) != 1 {
		t.Fatalf("report records = %d, want 1 (duplicate and empty dropped)", len(rep.Records))
	}
	wantTotal := events.Usage{PromptTokens: events.I64(5), CompletionTokens: events.I64(5)}
	if !usageEqual(rep.Total, wantTotal) {
		t.Fatalf("total = %+v, want %+v", rep.Total, wantTotal)
	}
}
