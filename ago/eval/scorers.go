package eval

import (
	"context"
	"strings"
)

// MatchMode selects how Correctness compares the answer with the expectation
// (Python parity: final_answer_matches matchers exact / casefold / contains).
type MatchMode string

const (
	MatchExact    MatchMode = "exact"    // strict equality
	MatchCasefold MatchMode = "casefold" // case-insensitive equality (the Python default)
	MatchContains MatchMode = "contains" // expected appears as a substring of the answer
)

// Correctness passes iff the trace's FinalResponse matches Expected. The zero
// Mode is casefold (the Python default). A trace with no model call has no
// answer and fails (Python parity: no final response → False).
type Correctness struct {
	Expected string
	Mode     MatchMode
}

func (c Correctness) Name() string { return "correctness" }

func (c Correctness) Score(_ context.Context, t *Trace) (Score, error) {
	details := map[string]any{"expected": c.Expected}
	if len(t.LLMCalls) == 0 {
		details["reason"] = "no model response"
		return Score{Name: c.Name(), Details: details}, nil
	}
	var ok bool
	switch c.Mode {
	case MatchExact:
		ok = t.FinalResponse == c.Expected
	case MatchContains:
		ok = strings.Contains(t.FinalResponse, c.Expected)
	default: // MatchCasefold
		ok = strings.EqualFold(t.FinalResponse, c.Expected)
	}
	details["actual"] = t.FinalResponse
	return boolScore(c.Name(), ok, details), nil
}

// TokenBudget passes iff the trace's total input+output tokens (Trace.Tokens)
// stay at or under Max, per trace (Python parity: scorers/cost.py token_budget —
// the Python cost scorer is this token budget; cache tokens are excluded).
type TokenBudget struct {
	Max int64
}

func (b TokenBudget) Name() string { return "token_budget" }

func (b TokenBudget) Score(_ context.Context, t *Trace) (Score, error) {
	u := t.Tokens()
	total := deref(u.PromptTokens) + deref(u.CompletionTokens)
	details := map[string]any{"tokens": total, "max": b.Max}
	return boolScore(b.Name(), total <= b.Max, details), nil
}

// CallMode selects whether ToolsCalled requires every name or at least one.
type CallMode string

const (
	CallAll CallMode = "all" // every name must have been called (the default)
	CallAny CallMode = "any" // at least one name must have been called
)

// ToolsCalled passes iff all (CallAll, the zero Mode) or at least one (CallAny)
// of Names was called. Python parity: scorers/tools.py tool_called — a single
// name with "at least one call"; the multi-name any/all shape follows the AGo
// contract and the per-name counts land in Details.
type ToolsCalled struct {
	Names []string
	Mode  CallMode
}

func (s ToolsCalled) Name() string { return "tools_called" }

func (s ToolsCalled) Score(_ context.Context, t *Trace) (Score, error) {
	counts := map[string]int{}
	for _, c := range t.ToolCalls {
		counts[c.Name]++
	}
	called := map[string]any{}
	for _, name := range s.Names {
		called[name] = counts[name]
	}
	details := map[string]any{"called": called, "mode": string(s.Mode)}
	var ok bool
	switch s.Mode {
	case CallAny:
		for _, name := range s.Names {
			if counts[name] >= 1 {
				ok = true
			}
		}
	default: // CallAll
		ok = len(s.Names) > 0
		for _, name := range s.Names {
			if counts[name] == 0 {
				ok = false
			}
		}
	}
	return boolScore(s.Name(), ok, details), nil
}

// Threshold gates an inner scorer's numeric value at Min: pass iff
// Value >= Min (Python parity: scorers/threshold.py with at_least; the at_most
// upper bound is outside the AGo contract and deferred). The gate keeps the
// inner scorer's name (Python: "the column simply becomes pass/fail"). An
// ungradeable inner — an error return — is a fail, never an abort (Python:
// score=None → fail).
type Threshold struct {
	Inner Scorer
	Min   float64
}

func (t Threshold) Name() string { return t.Inner.Name() }

func (t Threshold) Score(ctx context.Context, tr *Trace) (Score, error) {
	inner, err := t.Inner.Score(ctx, tr)
	if err != nil {
		return Score{
			Name:    t.Name(),
			Details: map[string]any{"error": err.Error(), "min": t.Min},
		}, nil
	}
	details := map[string]any{"score": inner.Value, "min": t.Min}
	for k, v := range inner.Details {
		if _, dup := details[k]; !dup {
			details[k] = v
		}
	}
	return Score{
		Name:    t.Name(),
		Value:   inner.Value,
		Passed:  inner.Value >= t.Min,
		Details: details,
	}, nil
}

// boolScore renders a boolean verdict as 1.0/0.0 so every pass/fail scorer
// feeds Threshold and the run aggregates the same way.
func boolScore(name string, passed bool, details map[string]any) Score {
	v := 0.0
	if passed {
		v = 1.0
	}
	return Score{Name: name, Value: v, Passed: passed, Details: details}
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
