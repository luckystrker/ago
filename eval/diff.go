package eval

import (
	"fmt"
	"sort"
	"strings"
)

// CaseStatus classifies a shared case's pass/fail movement from the baseline
// run to the current one.
type CaseStatus string

const (
	StatusRegression  CaseStatus = "regression"  // passed before, fails now — the CI gate
	StatusImprovement CaseStatus = "improvement" // failed before, passes now
	StatusUnchanged   CaseStatus = "unchanged"
)

// CaseDiff is one shared case's movement between the two runs.
type CaseDiff struct {
	Name     string     `json:"name"`
	Status   CaseStatus `json:"status"`
	Baseline bool       `json:"baseline_passed"`
	Current  bool       `json:"current_passed"`
}

// ScorerDelta is one scorer's pass rate over the shared cases: baseline vs
// current (Python parity: diff.pass_rate_deltas, computed independently per
// side over the cases where the scorer appears).
type ScorerDelta struct {
	Baseline float64 `json:"baseline"`
	Current  float64 `json:"current"`
}

// RunDiff answers "did my change help or hurt?" by lining two runs up by case
// name (Python parity: results/diff.py RunDiff, reduced to the case-level
// pass/fail movement the AGo RunResult carries — AGO-DEVIATION: Python keys
// flips by (scorer, task) and detects content drift from the tasks'
// inputs/reference_outputs; the AGo RunResult persists neither per-case inputs
// nor per-scorer flips, so the diff is per case and content drift is
// undetectable — D-061).
type RunDiff struct {
	// Cases lines the shared cases up, sorted by name.
	Cases []CaseDiff `json:"cases"`
	// Regressions / Improvements / Unchanged name the shared cases by movement;
	// Regressions is the CI gate (Python: assert not diff.regressions).
	Regressions  []string `json:"regressions"`
	Improvements []string `json:"improvements"`
	Unchanged    []string `json:"unchanged"`
	// OnlyInCurrent / OnlyInBaseline list the cases that did not line up —
	// never part of the comparison.
	OnlyInCurrent  []string `json:"only_in_current,omitempty"`
	OnlyInBaseline []string `json:"only_in_baseline,omitempty"`
	// ScorerPassRates maps a scorer name to its pass rates over the shared
	// cases, baseline then current. Scorers appearing in only one run are
	// rated over the side they appear on (the empty side is 0 cases → 0.0).
	ScorerPassRates map[string]ScorerDelta `json:"scorer_pass_rates,omitempty"`
}

// ComparableCases counts the cases both runs graded.
func (d RunDiff) ComparableCases() int { return len(d.Cases) }

// Diff compares current against baseline over the cases they share. The runs
// pair by Case.Name; a case present in only one run lands in OnlyInCurrent /
// OnlyInBaseline and is never compared.
func Diff(current, baseline *RunResult) RunDiff {
	var d RunDiff
	if current == nil || baseline == nil {
		return d
	}
	baseByName := make(map[string]*CaseResult, len(baseline.Cases))
	for i := range baseline.Cases {
		baseByName[baseline.Cases[i].Name] = &baseline.Cases[i]
	}
	// Per-scorer pass tallies over the shared cases, per side.
	curRates := map[string]*scorerTally{}
	baseRates := map[string]*scorerTally{}

	d.OnlyInCurrent = []string{}
	for i := range current.Cases {
		cc := &current.Cases[i]
		bc, ok := baseByName[cc.Name]
		if !ok {
			d.OnlyInCurrent = append(d.OnlyInCurrent, cc.Name)
			continue
		}
		status := StatusUnchanged
		switch {
		case cc.Passed && !bc.Passed:
			status = StatusImprovement
		case bc.Passed && !cc.Passed:
			status = StatusRegression
		}
		d.Cases = append(d.Cases, CaseDiff{
			Name: cc.Name, Status: status, Baseline: bc.Passed, Current: cc.Passed,
		})
		tallySide(baseRates, bc)
		tallySide(curRates, cc)
	}
	d.OnlyInBaseline = []string{}
	shared := make(map[string]bool, len(d.Cases))
	for _, c := range d.Cases {
		shared[c.Name] = true
	}
	for _, c := range baseline.Cases {
		if !shared[c.Name] {
			d.OnlyInBaseline = append(d.OnlyInBaseline, c.Name)
		}
	}

	d.Regressions = []string{}
	d.Improvements = []string{}
	d.Unchanged = []string{}
	for _, c := range d.Cases {
		switch c.Status {
		case StatusRegression:
			d.Regressions = append(d.Regressions, c.Name)
		case StatusImprovement:
			d.Improvements = append(d.Improvements, c.Name)
		default:
			d.Unchanged = append(d.Unchanged, c.Name)
		}
	}
	sort.Strings(d.Regressions)
	sort.Strings(d.Improvements)
	sort.Strings(d.Unchanged)
	sort.Strings(d.OnlyInCurrent)
	sort.Strings(d.OnlyInBaseline)

	if len(d.Cases) > 0 {
		d.ScorerPassRates = map[string]ScorerDelta{}
		for name, t := range baseRates {
			d.ScorerPassRates[name] = ScorerDelta{Baseline: rate(t), Current: rate(curRates[name])}
		}
		for name, t := range curRates {
			if _, dup := d.ScorerPassRates[name]; !dup {
				d.ScorerPassRates[name] = ScorerDelta{Baseline: 0, Current: rate(t)}
			}
		}
	}
	return d
}

// scorerTally accumulates one scorer's pass count over the shared cases.
type scorerTally struct{ passed, graded int }

func tallySide(rates map[string]*scorerTally, c *CaseResult) {
	for _, s := range c.Scores {
		t, ok := rates[s.Name]
		if !ok {
			t = &scorerTally{}
			rates[s.Name] = t
		}
		t.graded++
		if s.Passed {
			t.passed++
		}
	}
}

func rate(t *scorerTally) float64 {
	if t == nil || t.graded == 0 {
		return 0
	}
	return float64(t.passed) / float64(t.graded)
}

// DiffStrict is Diff plus the strict-mode gate: it errors when the runs did
// not grade the same suite (Python parity: compute_diff strict=True raising
// RunsNotComparableError; the content-drift and scorer-set problems the Python
// gate also checks are outside what an AGo RunResult records — D-061).
func DiffStrict(current, baseline *RunResult) (RunDiff, error) {
	d := Diff(current, baseline)
	if len(d.OnlyInCurrent) > 0 || len(d.OnlyInBaseline) > 0 {
		return d, fmt.Errorf(
			"eval: these runs didn't grade the same suite: %d case(s) only in current %v, %d case(s) only in baseline %v",
			len(d.OnlyInCurrent), d.OnlyInCurrent, len(d.OnlyInBaseline), d.OnlyInBaseline)
	}
	return d, nil
}

// Summary renders a printable diff: per-case movement, per-scorer pass-rate
// deltas, and everything excluded (Python parity: RunDiff.summary).
func (d RunDiff) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Diff — %d comparable case(s): %d regression(s), %d improvement(s), %d unchanged",
		len(d.Cases), len(d.Regressions), len(d.Improvements), len(d.Unchanged))
	names := make([]string, 0, len(d.ScorerPassRates))
	for name := range d.ScorerPassRates {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		delta := d.ScorerPassRates[name]
		mark := ""
		if delta.Current < delta.Baseline {
			mark = "   REGRESSION"
		}
		fmt.Fprintf(&b, "\n  %-24s %5.1f%% -> %5.1f%%   %+5.1f%%%s",
			name, delta.Baseline*100, delta.Current*100, (delta.Current-delta.Baseline)*100, mark)
	}
	if len(d.Regressions) > 0 {
		fmt.Fprintf(&b, "\n  flipped pass->fail: %v", d.Regressions)
	}
	if len(d.Improvements) > 0 {
		fmt.Fprintf(&b, "\n  flipped fail->pass: %v", d.Improvements)
	}
	if len(d.OnlyInCurrent) > 0 || len(d.OnlyInBaseline) > 0 {
		b.WriteString("\n  — excluded (not comparable) —")
		if len(d.OnlyInCurrent) > 0 {
			fmt.Fprintf(&b, "\n  only in current: %v", d.OnlyInCurrent)
		}
		if len(d.OnlyInBaseline) > 0 {
			fmt.Fprintf(&b, "\n  only in baseline: %v", d.OnlyInBaseline)
		}
	}
	return b.String()
}
