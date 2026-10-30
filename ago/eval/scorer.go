package eval

import "context"

// Score is one scorer's verdict on one trace. Conventions:
//
//   - boolean scorers (Correctness, ToolsCalled, TokenBudget) set Value to 1.0
//     (pass) or 0.0 (fail), so Threshold and run-level math treat them uniformly;
//   - numeric scorers (LLMJudge) set Value to the raw grade and Passed by their
//     own gate;
//   - an error return is not fatal: ScoreWith records it as a failing score and
//     the run moves on (Python parity: a scorer that raises becomes
//     Feedback(score=None) — ungradeable — and never aborts the run).
type Score struct {
	Name    string         `json:"name"`
	Value   float64        `json:"value"`
	Passed  bool           `json:"passed"`
	Details map[string]any `json:"details,omitempty"`
}

// Scorer grades one Trace. Implementations must be safe for concurrent use when
// shared across cases; Name is the stable key the score is reported under.
type Scorer interface {
	Name() string
	Score(ctx context.Context, t *Trace) (Score, error)
}

// ScoreWith runs one scorer, converting an error into a failing score carrying
// the error text (Python parity: scorer.py wraps a raised exception into
// Feedback(score=None, comment="scorer raised: ...") — the run continues).
func ScoreWith(ctx context.Context, s Scorer, t *Trace) Score {
	sc, err := s.Score(ctx, t)
	if err != nil {
		return Score{Name: s.Name(), Details: map[string]any{"error": err.Error()}}
	}
	return sc
}
