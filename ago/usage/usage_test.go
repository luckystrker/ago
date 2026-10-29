package usage

import (
	"testing"

	"ago/events"
)

func modelCall(prompt, completion int64, model, provider string) *events.UsageEvent {
	return &events.UsageEvent{
		Usage:    events.Usage{PromptTokens: events.I64(prompt), CompletionTokens: events.I64(completion)},
		Kind:     events.UsageKindModelCall,
		Model:    model,
		Provider: provider,
	}
}

func TestGroupsByModelAndProvider(t *testing.T) {
	report := FromEvents([]events.Event{
		modelCall(10, 4, "claude", "anthropic"),
		modelCall(6, 2, "claude", "anthropic"),
		modelCall(5, 1, "gpt", "openai"),
	})

	wantTotal := events.Usage{PromptTokens: events.I64(21), CompletionTokens: events.I64(7)}
	if got := report.Total; !usageEqual(got, wantTotal) {
		t.Fatalf("total = %+v, want %+v", got, wantTotal)
	}
	if len(report.Records) != 3 {
		t.Fatalf("records = %d, want 3", len(report.Records))
	}
	wantByModel := map[string]events.Usage{
		"claude": {PromptTokens: events.I64(16), CompletionTokens: events.I64(6)},
		"gpt":    {PromptTokens: events.I64(5), CompletionTokens: events.I64(1)},
	}
	if len(report.ByModel) != len(wantByModel) ||
		!usageEqual(report.ByModel["claude"], wantByModel["claude"]) ||
		!usageEqual(report.ByModel["gpt"], wantByModel["gpt"]) {
		t.Fatalf("by_model = %+v", report.ByModel)
	}
	if !usageEqual(report.ByProvider["anthropic"], wantByModel["claude"]) ||
		!usageEqual(report.ByProvider["openai"], wantByModel["gpt"]) {
		t.Fatalf("by_provider = %+v", report.ByProvider)
	}
	if !usageEqual(report.ByKind["model_call"], wantTotal) {
		t.Fatalf("by_kind = %+v", report.ByKind)
	}
}

// TestSubtaskRollupHasNoDoubleCount mirrors ag2/test/test_usage.py: the parent
// made one model call, a subtask rolled its own spend into a single subtask
// UsageEvent, and the report sums both without double counting.
func TestSubtaskRollupHasNoDoubleCount(t *testing.T) {
	report := FromEvents([]events.Event{
		modelCall(10, 4, "claude", "anthropic"),
		&events.UsageEvent{
			Usage: events.Usage{PromptTokens: events.I64(100), CompletionTokens: events.I64(50)},
			Kind:  events.UsageKindSubtask,
			Label: "worker",
		},
	})

	if want := (events.Usage{PromptTokens: events.I64(110), CompletionTokens: events.I64(54)}); !usageEqual(report.Total, want) {
		t.Fatalf("total = %+v, want %+v", report.Total, want)
	}
	if !usageEqual(report.ByKind[events.UsageKindModelCall], events.Usage{PromptTokens: events.I64(10), CompletionTokens: events.I64(4)}) ||
		!usageEqual(report.ByKind[events.UsageKindSubtask], events.Usage{PromptTokens: events.I64(100), CompletionTokens: events.I64(50)}) {
		t.Fatalf("by_kind = %+v", report.ByKind)
	}
	// Subtask usage carries no single-model attribution.
	if !usageEqual(report.ByModel["claude"], events.Usage{PromptTokens: events.I64(10), CompletionTokens: events.I64(4)}) || len(report.ByModel) != 1 {
		t.Fatalf("by_model = %+v", report.ByModel)
	}
	// The subtask record is attributed by label.
	found := false
	for _, rec := range report.Records {
		if rec.Kind == events.UsageKindSubtask && rec.Label == "worker" && rec.Model == "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("subtask record missing: %+v", report.Records)
	}
}

// TestPartsAreNeverReCounted pins the Parts rule: the parent subtask rollup
// already contains the child total, so FromEvents must not visit Parts.
func TestPartsAreNeverReCounted(t *testing.T) {
	rollup := events.UsageEvent{
		Usage: events.Usage{PromptTokens: events.I64(10), CompletionTokens: events.I64(20), TotalTokens: events.I64(30)},
		Kind:  events.UsageKindSubtask,
		Label: "worker",
		Parts: []events.UsageEvent{
			{Usage: events.Usage{PromptTokens: events.I64(4), CompletionTokens: events.I64(6), TotalTokens: events.I64(10)}, Kind: events.UsageKindModelCall, Model: "gpt", Provider: "openai"},
			{Usage: events.Usage{PromptTokens: events.I64(6), CompletionTokens: events.I64(14), TotalTokens: events.I64(20)}, Kind: events.UsageKindModelCall, Model: "gpt", Provider: "openai"},
		},
	}
	report := FromEvents([]events.Event{&rollup})

	want := events.Usage{PromptTokens: events.I64(10), CompletionTokens: events.I64(20), TotalTokens: events.I64(30)}
	if !usageEqual(report.Total, want) {
		t.Fatalf("total = %+v, want %+v (parts double counted)", report.Total, want)
	}
	if len(report.Records) != 1 {
		t.Fatalf("records = %d, want 1", len(report.Records))
	}
	if _, ok := report.ByModel["gpt"]; ok {
		t.Fatalf("parts leaked into by_model: %+v", report.ByModel)
	}
}

func TestInternalCallsCapturedByKind(t *testing.T) {
	report := FromEvents([]events.Event{
		modelCall(10, 4, "claude", "anthropic"),
		&events.UsageEvent{Usage: events.Usage{PromptTokens: events.I64(30), CompletionTokens: events.I64(5)}, Kind: events.UsageKindCompaction},
		&events.UsageEvent{Usage: events.Usage{PromptTokens: events.I64(20), CompletionTokens: events.I64(3)}, Kind: events.UsageKindAggregation},
	})

	want := events.Usage{PromptTokens: events.I64(60), CompletionTokens: events.I64(12)}
	if !usageEqual(report.Total, want) {
		t.Fatalf("total = %+v, want %+v", report.Total, want)
	}
	if len(report.ByKind) != 3 {
		t.Fatalf("by_kind = %+v, want 3 kinds", report.ByKind)
	}
}

// TestResponseUsageNotDoubleCounted: ModelResponse carries usage for local
// inspection, but the report aggregates UsageEvents alone.
func TestResponseUsageNotDoubleCounted(t *testing.T) {
	report := FromEvents([]events.Event{
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: "hi"},
			Usage:   events.Usage{PromptTokens: events.I64(999), CompletionTokens: events.I64(999)},
			Model:   "claude",
		},
		modelCall(3, 1, "claude", "anthropic"),
	})
	if len(report.Records) != 1 {
		t.Fatalf("records = %d, want 1", len(report.Records))
	}
	if want := (events.Usage{PromptTokens: events.I64(3), CompletionTokens: events.I64(1)}); !usageEqual(report.Total, want) {
		t.Fatalf("total = %+v, want %+v", report.Total, want)
	}
}

// TestSkipsEmptyAndZeroUsage: a UsageEvent reporting no tokens produces no
// record (Python Usage.__bool__ parity — zero-valued fields count as unset).
func TestSkipsEmptyAndZeroUsage(t *testing.T) {
	report := FromEvents([]events.Event{
		&events.UsageEvent{Kind: events.UsageKindModelCall},                                  // nothing reported
		&events.UsageEvent{Usage: events.Usage{PromptTokens: events.I64(0)}, Kind: "zeroed"}, // only zeros
		&events.UsageEvent{Usage: events.Usage{ThinkingTokens: events.I64(0), TotalTokens: events.I64(7)}, Kind: "mixed"},
	})
	if len(report.Records) != 1 {
		t.Fatalf("records = %d, want 1 (only the mixed event counts)", len(report.Records))
	}
	if report.Records[0].Kind != "mixed" {
		t.Fatalf("record kind = %q", report.Records[0].Kind)
	}
	// A reported zero survives the sum (Python _add_tokens(None, 0.0) parity).
	want := events.Usage{TotalTokens: events.I64(7), ThinkingTokens: events.I64(0)}
	if !usageEqual(report.Total, want) {
		t.Fatalf("total = %+v, want %+v", report.Total, want)
	}
}

// TestValueAndPointerForms: events reach a report both as pointers (the
// canonical stream form) and as values.
func TestValueAndPointerForms(t *testing.T) {
	val := modelCall(2, 3, "m", "p")
	report := FromEvents([]events.Event{val, *val})
	if want := (events.Usage{PromptTokens: events.I64(4), CompletionTokens: events.I64(6)}); !usageEqual(report.Total, want) {
		t.Fatalf("total = %+v, want %+v", report.Total, want)
	}
	if len(report.Records) != 2 {
		t.Fatalf("records = %d, want 2", len(report.Records))
	}
}

func TestEmptyInput(t *testing.T) {
	report := FromEvents(nil)
	if report == nil {
		t.Fatal("nil report")
	}
	if len(report.Records) != 0 || len(report.ByModel) != 0 || len(report.ByProvider) != 0 || len(report.ByKind) != 0 {
		t.Fatalf("unexpected entries: %+v", report)
	}
	if report.ByModel == nil || report.ByProvider == nil || report.ByKind == nil {
		t.Fatal("maps must be non-nil for safe ranging")
	}
	var empty events.Usage
	if !usageEqual(report.Total, empty) {
		t.Fatalf("total = %+v, want zero", report.Total)
	}
}

// TestRecordsPreserveOrder: records follow input order (stable for reporting).
func TestRecordsPreserveOrder(t *testing.T) {
	report := FromEvents([]events.Event{
		modelCall(1, 1, "a", "pa"),
		&events.UsageEvent{Usage: events.Usage{PromptTokens: events.I64(2)}, Kind: events.UsageKindCompaction},
		modelCall(3, 3, "b", "pb"),
	})
	wantKinds := []string{events.UsageKindModelCall, events.UsageKindCompaction, events.UsageKindModelCall}
	for i, kind := range wantKinds {
		if report.Records[i].Kind != kind {
			t.Fatalf("record %d kind = %q, want %q", i, report.Records[i].Kind, kind)
		}
	}
}

func usageEqual(a, b events.Usage) bool {
	same := func(x, y *int64) bool {
		switch {
		case x == nil && y == nil:
			return true
		case x == nil || y == nil:
			return false
		}
		return *x == *y
	}
	return same(a.PromptTokens, b.PromptTokens) &&
		same(a.CompletionTokens, b.CompletionTokens) &&
		same(a.TotalTokens, b.TotalTokens) &&
		same(a.CacheReadInputTokens, b.CacheReadInputTokens) &&
		same(a.CacheCreationInputTokens, b.CacheCreationInputTokens) &&
		same(a.ThinkingTokens, b.ThinkingTokens)
}
