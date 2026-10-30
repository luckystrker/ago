package compact

import (
	"github.com/luckystrker/ago/events"
)

// KeepRecent is the number of trailing conversational events compaction always
// retains verbatim (the fresh range). Every conversational event before them is
// summarized into one CompactionSummary.
const KeepRecent = 2

// Trigger decides when compaction fires. It sees the full history snapshot and
// the usage reported by the ModelResponse events of that snapshot (a custom
// trigger can budget on reported spend; the built-ins estimate from content).
type Trigger interface {
	ShouldTrigger(evs []events.Event, u events.Usage) bool
}

// EventCountTrigger fires when the number of conversational events exceeds n
// (Python parity: CompactTrigger.max_events; n <= 0 disables the trigger).
// Transient artifacts and persisted telemetry (UsageEvent) are not
// conversational and never count.
func EventCountTrigger(n int) Trigger { return eventCountTrigger{max: n} }

type eventCountTrigger struct{ max int }

func (t eventCountTrigger) ShouldTrigger(evs []events.Event, _ events.Usage) bool {
	if t.max <= 0 {
		return false
	}
	n := 0
	for _, ev := range evs {
		if ev != nil && ev.Conversational() {
			n++
		}
	}
	return n > t.max
}

// TokenTrigger fires when the estimated token footprint of the conversational
// events exceeds budget (Python parity: CompactTrigger.max_tokens with
// chars_per_token=4 — events.EstimatedTokens' default). The reported usage
// argument is ignored on purpose: the estimate tracks what the transcript
// itself would cost, independent of how the provider bills it. budget <= 0
// disables the trigger.
func TokenTrigger(budget int) Trigger { return tokenTrigger{budget: budget} }

type tokenTrigger struct{ budget int }

func (t tokenTrigger) ShouldTrigger(evs []events.Event, _ events.Usage) bool {
	if t.budget <= 0 {
		return false
	}
	total := 0
	for _, ev := range evs {
		if ev != nil && ev.Conversational() {
			total += events.EstimatedTokens(ev, 0)
		}
	}
	return total > t.budget
}
