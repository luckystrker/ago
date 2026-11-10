package policies

import (
	"fmt"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// charsPerToken is the sizing default of events.EstimatedTokens (Python parity:
// events/sizing.py).
const charsPerToken = 4

// TokenBudgetOption configures NewTokenBudgetPolicy.
type TokenBudgetOption func(*TokenBudgetPolicy)

// WithCharsPerToken sets the chars-per-token estimate used to size events
// (Python parity: TokenBudgetPolicy(max_tokens, chars_per_token=4)). Lower
// values estimate more tokens for the same text; n <= 0 keeps the default.
//
// AGO-DEVIATION: Python raises ValueError for chars_per_token < 1; the
// constructor has no error return, so a non-positive value is ignored.
func WithCharsPerToken(n int) TokenBudgetOption {
	return func(p *TokenBudgetPolicy) {
		if n > 0 {
			p.charsPerToken = n
		}
	}
}

// WithBudgetTransparency injects a note telling the model that history was cut
// (Python parity: transparent=True): "[token_budget] Showing N of M events
// (token budget)."
//
// AGO-DEVIATION: Python counts the raw event list; AGo counts the conversational
// subset it reduces (D-030), so telemetry and alerts never inflate M.
func WithBudgetTransparency(on bool) TokenBudgetOption {
	return func(p *TokenBudgetPolicy) { p.transparent = on }
}

// TokenBudgetPolicy keeps the most recent conversational events whose estimated
// token cost fits the budget (Python parity: token_budget.py, which sizes by
// character count with the same 4-chars-per-token default, tunable with
// WithCharsPerToken).
//
// The budget is a target, not a guarantee: the newest event is always retained
// even when it alone overshoots, and a cut that would orphan a tool result
// widens back until the retained span contains the whole tool round.
type TokenBudgetPolicy struct {
	budget        int
	charsPerToken int
	transparent   bool
}

// NewTokenBudgetPolicy keeps a suffix of conversational events worth at most
// budget estimated tokens.
func NewTokenBudgetPolicy(budget int, opts ...TokenBudgetOption) TokenBudgetPolicy {
	p := TokenBudgetPolicy{budget: budget, charsPerToken: charsPerToken}
	for _, opt := range opts {
		opt(&p)
	}
	return p
}

// Apply returns the conversational suffix that fits the budget. When the whole
// transcript fits, the input passes through unchanged.
func (p TokenBudgetPolicy) Apply(prompts []string, evs []events.Event, _ *agocontext.Context) ([]string, []events.Event, error) {
	convs := conversational(evs)
	if len(convs) == 0 {
		return prompts, evs, nil
	}
	total := 0
	for _, ev := range convs {
		total += events.EstimatedTokens(ev, p.charsPerToken)
	}
	if total <= p.budget {
		return prompts, evs, nil
	}
	cost := events.EstimatedTokens(convs[len(convs)-1], p.charsPerToken)
	kept := 1 // the newest event is retained even when it alone breaks the budget
	for i := len(convs) - 2; i >= 0; i-- {
		c := events.EstimatedTokens(convs[i], p.charsPerToken)
		if cost+c > p.budget {
			break
		}
		cost += c
		kept++
	}
	retained := convs[widenCut(convs, len(convs)-kept):]
	if p.transparent {
		prompts = append(prompts, fmt.Sprintf("[token_budget] Showing %d of %d events (token budget).", len(retained), len(convs)))
	}
	return prompts, retained, nil
}
