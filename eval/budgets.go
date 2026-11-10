package eval

// BudgetThresholds are the per-task ceilings a run measures itself against
// (Python parity: results/budgets.py BudgetThresholds). They are
// observational: a case over budget is counted in RunResult.BudgetViolations
// and never aborted, so the count is a CI regression signal ("no task may go
// over budget") rather than a kill switch.
//
// A zero field means "no limit", which is why the zero struct disables both
// checks (Python spells the same idea as None).
type BudgetThresholds struct {
	// MaxTokensPerTask caps the sum of input and output tokens across every
	// model call in one task.
	MaxTokensPerTask int
	// MaxSecondsPerTask caps the wall-clock duration of one task.
	MaxSecondsPerTask float64
}

// Enabled reports whether any threshold is set.
func (b BudgetThresholds) Enabled() bool {
	return b.MaxTokensPerTask > 0 || b.MaxSecondsPerTask > 0
}

// ExceededBy reports whether the trace is over any set threshold.
func (b BudgetThresholds) ExceededBy(t *Trace) bool {
	if t == nil {
		return false
	}
	if b.MaxTokensPerTask > 0 && t.TotalTokens() > b.MaxTokensPerTask {
		return true
	}
	return b.MaxSecondsPerTask > 0 && t.Duration().Seconds() > b.MaxSecondsPerTask
}
