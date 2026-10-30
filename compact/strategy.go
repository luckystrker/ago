package compact

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// Strategy summarizes a span of old events into one text — the content of the
// CompactionSummary event that replaces them.
type Strategy interface {
	Summarize(ctx *agocontext.Context, evs []events.Event) (string, error)
}

// DefaultPrompt precedes the rendered history when LLMStrategy.Prompt is empty
// (Python parity: SummarizeCompact's summarization instruction).
const DefaultPrompt = "Summarize the following conversation history concisely, " +
	"preserving key decisions, findings, and context:"

// LLMStrategy summarizes via one model call over the rendered events. Client is
// the summarizer; Prompt precedes the rendered history (empty → DefaultPrompt).
// Safe for concurrent use (the struct is read-only at call time).
type LLMStrategy struct {
	Client config.LLMClient
	Prompt string
}

var _ Strategy = LLMStrategy{}

// Summarize renders every event it receives with events.RenderForPrompt and asks
// the client once. The middleware passes only conversational events, so
// telemetry never leaks into the prompt (Python parity: the summarizer filters
// is_conversational when rendering).
//
// AGO-DEVIATION: the call is headless — a nil conversation context, so provider
// progress events (streaming chunks) are dropped. Python summarizes over a
// throwaway MemoryStream to the same effect. Cancellation rides on
// context.Background(): the middleware hooks carry no stdlib context (same
// limitation as middleware.Retry).
func (s LLMStrategy) Summarize(ctx *agocontext.Context, evs []events.Event) (string, error) {
	if s.Client == nil {
		return "", errors.New("compact: LLMStrategy needs a Client")
	}
	rendered := make([]string, 0, len(evs))
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		rendered = append(rendered, events.RenderForPrompt(ev))
	}
	prompt := s.Prompt
	if prompt == "" {
		prompt = DefaultPrompt
	}
	req := &events.ModelRequest{
		Parts: []events.Input{events.TextInput{Content: prompt + "\n\n" + strings.Join(rendered, "\n")}},
	}
	resp, err := s.Client.Call(context.Background(), []events.Event{req}, nil, config.CallOptions{})
	if err != nil {
		return "", fmt.Errorf("compact: summarizer call failed: %w", err)
	}
	if resp == nil {
		return "", errors.New("compact: summarizer returned no response")
	}
	// Surface the call's own spend onto the conversation stream (Python parity:
	// the summarization record is sent live so monitoring sees it; the middleware
	// carries it through the history rewrite).
	zero := events.Usage{}
	if ctx != nil && resp.Usage != zero {
		ue := &events.UsageEvent{
			Usage:    resp.Usage,
			Kind:     events.UsageKindCompaction,
			Model:    resp.Model,
			Provider: resp.Provider,
		}
		if err := ctx.Send(ue); err != nil {
			return "", fmt.Errorf("compact: send compaction usage: %w", err)
		}
	}
	return resp.Content(), nil
}
