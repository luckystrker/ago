// Package usage aggregates token-usage reports from a stream's event log.
//
// The event log is the source of truth: every place that spends tokens emits a
// UsageEvent as the tokens are spent — a direct LLM call (kind "model_call"),
// history compaction ("compaction"), memory aggregation ("aggregation"), or a
// sub-agent rollup ("subtask"). A subtask rollup already contains the child
// total, so summing the top-level UsageEvents yields the correct grand total.
package usage

import (
	"ago/events"
)

// Record attributes the usage of a single stage of a run.
type Record struct {
	Usage        events.Usage
	Kind         string // "model_call" | "subtask" | "compaction" | "aggregation"
	Model        string // "" when the stage has no single-model attribution
	Provider     string // "" likewise
	FinishReason string
	Label        string // sub-agent name for "subtask" records
}

// Report is the aggregated token usage for a run, broken down by stage.
// The maps are always non-nil (empty when nothing matched) so callers can
// range over them without a nil check.
type Report struct {
	Total      events.Usage
	Records    []Record
	ByModel    map[string]events.Usage
	ByProvider map[string]events.Usage
	ByKind     map[string]events.Usage
}

// FromEvents aggregates the UsageEvents in evs. Non-usage events (including
// ModelResponse, whose Usage field exists for local inspection only) are
// ignored. Parts of a subtask rollup are deliberately not visited: the rollup's
// own Usage already sums its children, so recursing into Parts would double
// count. Events that report no tokens at all are skipped (parity with Python's
// Usage.__bool__, where zero-valued fields count as unset).
func FromEvents(evs []events.Event) *Report {
	rep := &Report{
		Records:    make([]Record, 0, len(evs)),
		ByModel:    map[string]events.Usage{},
		ByProvider: map[string]events.Usage{},
		ByKind:     map[string]events.Usage{},
	}
	for _, ev := range evs {
		u, ok := events.AsValue(ev).(events.UsageEvent)
		if !ok || !usageReported(u.Usage) {
			continue
		}
		rec := Record{
			Usage:        u.Usage,
			Kind:         u.Kind,
			Model:        u.Model,
			Provider:     u.Provider,
			FinishReason: u.FinishReason,
			Label:        u.Label,
		}
		rep.Records = append(rep.Records, rec)
		rep.Total = rep.Total.Add(rec.Usage)
		if rec.Model != "" {
			rep.ByModel[rec.Model] = rep.ByModel[rec.Model].Add(rec.Usage)
		}
		if rec.Provider != "" {
			rep.ByProvider[rec.Provider] = rep.ByProvider[rec.Provider].Add(rec.Usage)
		}
		rep.ByKind[rec.Kind] = rep.ByKind[rec.Kind].Add(rec.Usage)
	}
	return rep
}

// usageReported reports whether any usage field carries a non-zero value.
func usageReported(u events.Usage) bool {
	for _, p := range []*int64{
		u.PromptTokens, u.CompletionTokens, u.TotalTokens,
		u.CacheReadInputTokens, u.CacheCreationInputTokens, u.ThinkingTokens,
	} {
		if p != nil && *p != 0 {
			return true
		}
	}
	return false
}
