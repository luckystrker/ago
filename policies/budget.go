package policies

import (
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// charsPerToken is the sizing default of events.EstimatedTokens (Python parity:
// events/sizing.py).
const charsPerToken = 4

// TokenBudgetPolicy keeps the most recent conversational events whose estimated
// token cost fits the budget (Python parity: token_budget.py, which sizes by
// character count with the same 4-chars-per-token default).
//
// The budget is a target, not a guarantee: the newest event is always retained
// even when it alone overshoots, and a cut that would orphan a tool result
// widens back until the retained span contains the whole tool round.
type TokenBudgetPolicy struct {
	budget int
}

// NewTokenBudgetPolicy keeps a suffix of conversational events worth at most
// budget estimated tokens.
func NewTokenBudgetPolicy(budget int) TokenBudgetPolicy { return TokenBudgetPolicy{budget: budget} }

// Apply returns the conversational suffix that fits the budget. When the whole
// transcript fits, the input passes through unchanged.
func (p TokenBudgetPolicy) Apply(prompts []string, evs []events.Event, _ *agocontext.Context) ([]string, []events.Event, error) {
	convs := conversational(evs)
	if len(convs) == 0 {
		return prompts, evs, nil
	}
	total := 0
	for _, ev := range convs {
		total += events.EstimatedTokens(ev, charsPerToken)
	}
	if total <= p.budget {
		return prompts, evs, nil
	}
	cost := events.EstimatedTokens(convs[len(convs)-1], charsPerToken)
	kept := 1 // the newest event is retained even when it alone breaks the budget
	for i := len(convs) - 2; i >= 0; i-- {
		c := events.EstimatedTokens(convs[i], charsPerToken)
		if cost+c > p.budget {
			break
		}
		cost += c
		kept++
	}
	return prompts, convs[widenCut(convs, len(convs)-kept):], nil
}
