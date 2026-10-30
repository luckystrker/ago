package eval

import (
	"context"
	"errors"
	"fmt"

	"github.com/luckystrker/ago/config"
)

// Winner labels of a pairwise comparison.
const (
	WinsA   = "a"
	WinsB   = "b"
	WinsTie = "tie"
)

// PairwiseOutcome is the result of comparing trace A against trace B on one
// criterion (Python parity: pairwise.PairwiseOutcome). Winner is "a", "b" or
// "tie"; Detail carries the per-order verdicts behind a swapped judgment.
type PairwiseOutcome struct {
	Winner    string         `json:"winner"`
	Reasoning string         `json:"reasoning,omitempty"`
	Detail    map[string]any `json:"detail,omitempty"`
}

// Pairwise is an LLM comparator of two traces against one rubric (Python
// parity: scorers/pairwise_judge.py — a PairwiseComparator). The judge runs
// twice with the responses swapped (dual-order position swap, the Python
// swap=True default): a winner is declared only when the same side wins in both
// orders — a flip resolves to a tie, defending against positional bias. The
// per-call verdict is position-based ("first"/"second", never A/B).
//
// AGO-DEVIATION: the Python judge is an Agent with a locked response_schema;
// AGo calls the client headless and parses a JSON verdict
// {"preferred": "first"|"second"|"tie", "reason": "..."}. The single-order mode
// (swap=False) and validation retries are not ported — D-051.
type Pairwise struct {
	Client config.LLMClient
	Rubric string
}

func (p Pairwise) Name() string { return "pairwise" }

// Compare judges trace A against trace B.
func (p Pairwise) Compare(ctx context.Context, a, b *Trace) (PairwiseOutcome, error) {
	if p.Client == nil {
		return PairwiseOutcome{}, errors.New("eval: Pairwise needs a client")
	}
	v1, err := p.verdict(ctx, answerText(a), answerText(b))
	if err != nil {
		return PairwiseOutcome{}, err
	}
	v2, err := p.verdict(ctx, answerText(b), answerText(a)) // order 2: Response 1 = B
	if err != nil {
		return PairwiseOutcome{}, err
	}
	w1 := prefToAB(v1.preferred, WinsA, WinsB) // order 1: Response 1 = A
	w2 := prefToAB(v2.preferred, WinsB, WinsA)
	winner := WinsTie // conservative: win only if consistent across both orders
	if w1 == w2 && w1 != WinsTie {
		winner = w1
	}
	return PairwiseOutcome{
		Winner:    winner,
		Reasoning: v1.reason,
		Detail:    map[string]any{"order1": v1.preferred, "order2": v2.preferred},
	}, nil
}

type pairwiseVerdict struct {
	preferred string
	reason    string
}

func (p Pairwise) verdict(ctx context.Context, first, second string) (pairwiseVerdict, error) {
	prompt := fmt.Sprintf(
		"You are comparing two AI responses against a single criterion.\n"+
			"Criterion: %s\n"+
			`Decide which response is better on THIS criterion only: answer "first", "second", or "tie". `+
			"Do not favor a response for its position or its length.\n"+
			`Return ONLY a JSON object: {"preferred": "first"|"second"|"tie", "reason": "<brief justification>"}.`+"\n\n"+
			"## Response 1\n%s\n\n## Response 2\n%s",
		p.Rubric, first, second)
	resp, err := callHeadless(ctx, p.Client, prompt)
	if err != nil {
		return pairwiseVerdict{}, fmt.Errorf("eval: pairwise judge call: %w", err)
	}
	var v struct {
		Preferred string `json:"preferred"`
		Reason    string `json:"reason"`
	}
	if err := unmarshalVerdict(resp.Content(), &v); err != nil {
		return pairwiseVerdict{}, fmt.Errorf("eval: pairwise %w", err)
	}
	// No verdict counts as a tie (Python parity: PairwiseVerdict(preferred="tie",
	// reasoning="judge returned no verdict")).
	if v.Preferred == "" {
		v.Preferred = WinsTie
	}
	switch v.Preferred {
	case "first", "second", "tie":
	default:
		return pairwiseVerdict{}, fmt.Errorf("eval: pairwise verdict preferred %q is not first/second/tie", v.Preferred)
	}
	return pairwiseVerdict{preferred: v.Preferred, reason: v.Reason}, nil
}

// prefToAB maps a position preference to a variant label: "first" → first,
// "second" → second, anything else → tie (Python parity: _pref_to_ab).
func prefToAB(preferred, first, second string) string {
	switch preferred {
	case "first":
		return first
	case "second":
		return second
	default:
		return WinsTie
	}
}
