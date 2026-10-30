// Package aggregate folds an overgrown transcript into a single summary.
//
// A Trigger decides when the transcript has grown past what is worth keeping
// verbatim; a Strategy merges the existing CompactionSummary events and the
// superseded conversational events into one summary text; the aggregation
// middleware (New) applies the result to the history after a turn, bracketed
// by AggregationStarted/Completed/Failed lifecycle events.
//
// AGO-DEVIATION: Python AG2 (ag2/aggregate.py) treats aggregation as a
// knowledge-store writer — strategies extract structured knowledge from raw
// events into /memory/ paths and never touch the transcript. AGo (porting
// brief) reduces aggregation to transcript shaping instead: summaries plus old
// conversational events are merged into ONE CompactionSummary that replaces
// them. See docs/DECISIONS.md.
package aggregate

import (
	stdcontext "context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/usage"
)

// ErrNoClient reports an LLMStrategy constructed without an LLM client.
var ErrNoClient = errors.New("aggregate: LLMStrategy has no client")

// Trigger decides whether aggregation should run after a turn.
type Trigger interface {
	// ShouldTrigger receives the current transcript and the total token usage
	// reported on the stream so far.
	ShouldTrigger(evs []events.Event, u events.Usage) bool
}

// SummaryCountTrigger fires once the history holds at least n CompactionSummary
// events.
func SummaryCountTrigger(n int) Trigger { return summaryCountTrigger{min: n} }

type summaryCountTrigger struct{ min int }

func (t summaryCountTrigger) ShouldTrigger(evs []events.Event, _ events.Usage) bool {
	n := 0
	for _, ev := range evs {
		if _, ok := events.AsValue(ev).(events.CompactionSummary); ok {
			n++
		}
	}
	return n >= t.min
}

// UsageTrigger fires once the token usage reported on the stream reaches total
// (TotalTokens >= total). Turns that reported no token usage never fire it.
func UsageTrigger(total int64) Trigger { return usageTrigger{total: total} }

type usageTrigger struct{ total int64 }

func (t usageTrigger) ShouldTrigger(_ []events.Event, u events.Usage) bool {
	return u.TotalTokens != nil && *u.TotalTokens >= t.total
}

// Strategy merges existing summaries and superseded events into one summary
// text. Implementations must be safe for concurrent use: middleware factories
// may drive several turns at once.
type Strategy interface {
	Aggregate(ctx *agocontext.Context, summaries []string, evs []events.Event) (string, error)
}

// DefaultPrompt is the instruction LLMStrategy sends when no prompt is configured.
const DefaultPrompt = "Merge the existing summaries and the older conversation below " +
	"into a single compact summary. Preserve key decisions, findings, outcomes, and " +
	"any unfinished work. Drop duplication. Keep it concise and factual.\n\n"

// LLMStrategy summarizes with a single LLM call (Python parity: every builtin
// aggregation strategy "costs one LLM call per aggregation"). Construct and use
// it as *LLMStrategy: the struct records the token usage of its last call.
type LLMStrategy struct {
	Client config.LLMClient

	mu        sync.Mutex
	lastUsage events.Usage
	lastCalls int
}

// Aggregate renders summaries and events into one prompt, calls the client and
// returns the response text. A reported token usage is emitted onto the
// conversation stream as UsageEvent(kind="aggregation") so usage reports see
// the aggregation call (parity: ag2/aggregate.py _emit_aggregation_usage).
func (s *LLMStrategy) Aggregate(ctx *agocontext.Context, summaries []string, evs []events.Event) (string, error) {
	if s == nil || s.Client == nil {
		return "", ErrNoClient
	}
	req := events.EnsureRequest(renderPrompt(summaries, evs))
	// The Strategy contract carries no standard context; the summarization call
	// is a one-shot request made inline on the turn's goroutine.
	resp, err := s.Client.Call(stdcontext.Background(), []events.Event{req}, ctx, config.CallOptions{})
	if err != nil {
		return "", fmt.Errorf("aggregate: summarization call failed: %w", err)
	}
	if resp == nil {
		return "", errors.New("aggregate: summarization client returned no response")
	}
	s.mu.Lock()
	s.lastUsage = resp.Usage
	s.lastCalls++
	s.mu.Unlock()

	if usage.Reported(resp.Usage) {
		// Best-effort telemetry: a subscriber failure must not fail the aggregation.
		_ = ctx.Send(&events.UsageEvent{
			Usage:    resp.Usage,
			Kind:     events.UsageKindAggregation,
			Model:    resp.Model,
			Provider: resp.Provider,
		})
	}
	return resp.Content(), nil
}

// usageSource is the optional seam the middleware reads AggregationCompleted's
// usage and LLM-call count from. A hand-written strategy that does not
// implement it reports zero usage and zero LLM calls.
type usageSource interface {
	usageOfLast() (events.Usage, int)
}

func (s *LLMStrategy) usageOfLast() (events.Usage, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastUsage, s.lastCalls
}

// renderPrompt builds the single-user-message prompt: the instruction, then the
// existing summaries, then the superseded events rendered as plain text.
func renderPrompt(summaries []string, evs []events.Event) string {
	var b strings.Builder
	b.WriteString(DefaultPrompt)
	if len(summaries) > 0 {
		b.WriteString("## Existing summaries\n")
		for _, s := range summaries {
			b.WriteString("- ")
			b.WriteString(s)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	if len(evs) > 0 {
		b.WriteString("## Older conversation\n")
		for _, ev := range evs {
			if txt := events.RenderForPrompt(ev); txt != "" {
				b.WriteString(txt)
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}
