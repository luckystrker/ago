// Provider-native events the Messages API needs back on later requests.
// Parity with ag2/config/anthropic/events.py: Anthropic requires its thinking
// blocks echoed back verbatim on a tool-use turn, so they travel as durable
// replay events rather than as transient stream progress.

package anthropic

import (
	"github.com/luckystrker/ago/events"
)

// ThinkingBlockEvent is one signed `thinking` block of a response, kept
// verbatim for replay.
//
// AGO-DEVIATION: Python maps a thinking block to a transient ModelReasoning
// only, so its signature is lost and the block is never replayed; Anthropic
// rejects a tool-use turn whose thinking blocks are missing or unsigned, so
// AGo records the block as well (docs/DECISIONS.md, the W1-A entry).
type ThinkingBlockEvent struct {
	events.Base
	// Thinking is the block's text (possibly empty with max_tokens cut).
	Thinking string `json:"thinking"`
	// Signature is the opaque signature the API stamps on the block.
	Signature string `json:"signature,omitempty"`
}

func (ThinkingBlockEvent) EventType() string    { return "AnthropicThinkingBlockEvent" }
func (ThinkingBlockEvent) Transient() bool      { return false }
func (ThinkingBlockEvent) Conversational() bool { return true }

// Block renders the wire block replayed inside the assistant turn.
func (e ThinkingBlockEvent) Block() map[string]any {
	b := map[string]any{"type": "thinking", "thinking": e.Thinking}
	if e.Signature != "" {
		b["signature"] = e.Signature
	}
	return b
}

// RedactedThinkingEvent is safety-redacted thinking: opaque and encrypted.
// The API requires the block echoed back unchanged, so it is a replay anchor,
// not transient reasoning (Python parity: AnthropicRedactedThinkingEvent).
type RedactedThinkingEvent struct {
	events.Base
	// Data is the opaque encrypted payload the API supplied.
	Data string `json:"data"`
}

func (RedactedThinkingEvent) EventType() string    { return "AnthropicRedactedThinkingEvent" }
func (RedactedThinkingEvent) Transient() bool      { return false }
func (RedactedThinkingEvent) Conversational() bool { return true }

// Block renders the wire block replayed inside the assistant turn.
func (e RedactedThinkingEvent) Block() map[string]any {
	return map[string]any{"type": "redacted_thinking", "data": e.Data}
}

// CacheDiagnostics reports why the API could not reuse the cached prefix of the
// previous request. Transient: a fact about one request, never part of the
// reply (Python parity: AnthropicCacheDiagnostics).
type CacheDiagnostics struct {
	events.Base
	// Reason is the API's cache-miss type: model_changed, system_changed,
	// tools_changed, messages_changed, previous_message_not_found or
	// unavailable. Kept a string so a new reason is reported, not dropped.
	Reason string `json:"reason"`
	// CacheMissedInputTokens estimates the input tokens a cache hit would have
	// saved; nil where the API reported no estimate.
	CacheMissedInputTokens *int64 `json:"cache_missed_input_tokens,omitempty"`
	// PreviousMessageID names the message this one was compared against.
	PreviousMessageID string `json:"previous_message_id,omitempty"`
}

func (CacheDiagnostics) EventType() string    { return "AnthropicCacheDiagnostics" }
func (CacheDiagnostics) Transient() bool      { return true }
func (CacheDiagnostics) Conversational() bool { return false }

func init() {
	events.RegisterEvent(ThinkingBlockEvent{})
	events.RegisterEvent(RedactedThinkingEvent{})
	events.RegisterEvent(CacheDiagnostics{})
}
