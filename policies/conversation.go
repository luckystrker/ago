package policies

import (
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// ConversationPolicy keeps only conversational events (Python parity:
// conversation.py, which allows ModelRequest/ModelResponse/Tool*/ProviderReplay/
// CompactionSummary by type).
//
// AGO-DEVIATION: AGo filters on the Conversational() flag instead of a type
// list — the event vocabulary already encodes replay-worthiness, and
// CompactionSummary is conversational in AGo exactly as Python includes it
// explicitly here.
type ConversationPolicy struct{}

// NewConversationPolicy returns the conversation filter.
func NewConversationPolicy() ConversationPolicy { return ConversationPolicy{} }

// Apply drops every non-conversational event; prompts pass through untouched.
func (ConversationPolicy) Apply(prompts []string, evs []events.Event, _ *agocontext.Context) ([]string, []events.Event, error) {
	return prompts, conversational(evs), nil
}
