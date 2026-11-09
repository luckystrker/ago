// Package usage aggregates token-usage reports from a stream's event log.
//
// The event log is the source of truth: every place that spends tokens emits a
// single source UsageEvent as the tokens are spent — a direct LLM call (kind
// "model_call"), history compaction ("compaction"), memory aggregation
// ("aggregation"), or a descendant's own call forwarded by a delegation, which
// keeps its original kind, provider, model and event_id. Reports deduplicate by
// source event id, so summing the log yields each expenditure exactly once.
package usage

import (
	"github.com/luckystrker/ago/events"
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
// ignored. An archival ChildEvent envelope resolves to the source record it
// wraps, and a non-empty source event_id is counted once: a forwarded record,
// a republished copy or a replayed transcript cannot inflate the total.
// Parts of a legacy subtask rollup are deliberately not visited: the rollup's
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
	seen := map[string]struct{}{}
	for _, ev := range evs {
		u, ok := EventFrom(ev)
		if !ok || !usageReported(u.Usage) {
			continue
		}
		if u.EventID != "" {
			if _, dup := seen[u.EventID]; dup {
				continue
			}
			seen[u.EventID] = struct{}{}
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

// EventFrom reads the source accounting record behind ev, accepting an event
// that is itself a UsageEvent as well as an archived observation envelope that
// wraps one (upstream parity: usage_event_from).
func EventFrom(ev events.Event) (events.UsageEvent, bool) {
	u, ok := events.AsValue(events.SourceEvent(ev)).(events.UsageEvent)
	return u, ok
}

// usageReported reports whether any usage field carries a non-zero value.
func usageReported(u events.Usage) bool {
	for _, p := range []*int64{
		u.PromptTokens, u.CompletionTokens, u.TotalTokens,
		u.CacheReadInputTokens, u.CacheCreationInputTokens,
		u.CacheCreationExtendedInputTokens, u.ThinkingTokens,
	} {
		if p != nil && *p != 0 {
			return true
		}
	}
	return false
}

// Reported reports whether the usage carries any non-zero token count (Python
// parity: Usage.__bool__ — an all-zero usage counts as "not reported", so no
// UsageEvent is emitted for it). Callers that emit UsageEvents use this as the gate.
func Reported(u events.Usage) bool { return usageReported(u) }
