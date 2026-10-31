package compact

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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

// DefaultTimeout bounds one LLMStrategy summarization call when Timeout is
// unset. Compaction runs inside the post-turn hook, so an unbounded
// summarizer call would block an already-successful turn — and with it the
// agent — indefinitely on a hung provider.
//
// AGO-DEVIATION: Python compaction applies no deadline of its own (its LLM
// clients carry theirs); the Go HTTP stack defaults to no timeout, so AGo
// bounds the call explicitly.
const DefaultTimeout = 120 * time.Second

// LLMStrategy summarizes via one model call over the rendered events. Client is
// the summarizer; Prompt precedes the rendered history (empty → DefaultPrompt).
// Safe for concurrent use (the struct is read-only at call time).
type LLMStrategy struct {
	Client config.LLMClient
	Prompt string
	// Timeout bounds the summarization call; zero selects DefaultTimeout.
	// There is deliberately no "no timeout" setting: compaction runs after a
	// successful turn and an unbounded call would hang that turn forever on a
	// stuck provider. Raise the value for a legitimately slow summarizer;
	// negative values are rejected by Summarize.
	Timeout time.Duration
}

var _ Strategy = LLMStrategy{}

// Summarize renders every event it receives with events.RenderForPrompt and asks
// the client once. The middleware passes only conversational events, so
// telemetry never leaks into the prompt (Python parity: the summarizer filters
// is_conversational when rendering).
//
// AGO-DEVIATION: the call is headless — a nil conversation context, so provider
// progress events (streaming chunks) are dropped. Python summarizes over a
// throwaway MemoryStream to the same effect. The middleware hooks carry no
// stdlib context, so cancellation rides on context.Background() bounded by
// Timeout (DefaultTimeout when unset): a hung provider surfaces as a
// context.DeadlineExceeded error instead of blocking the turn forever (same
// limitation as middleware.Retry).
func (s LLMStrategy) Summarize(ctx *agocontext.Context, evs []events.Event) (string, error) {
	if s.Client == nil {
		return "", errors.New("compact: LLMStrategy needs a Client")
	}
	if s.Timeout < 0 {
		return "", errors.New("compact: LLMStrategy.Timeout must not be negative")
	}
	timeout := s.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
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
	callCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	resp, err := s.Client.Call(callCtx, []events.Event{req}, nil, config.CallOptions{})
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
