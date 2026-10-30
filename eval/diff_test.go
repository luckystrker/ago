package eval

import (
	"strings"
	"testing"
)

// diffRun builds a RunResult with one case per map entry and its pass state;
// every case carries a single score ("s") matching the case's verdict.
func diffRun(passed map[string]bool) *RunResult {
	res := &RunResult{Cases: []CaseResult{}}
	for name, p := range passed {
		res.Cases = append(res.Cases, CaseResult{Name: name, Passed: p, Scores: []Score{{Name: "s", Passed: p}}})
	}
	return res
}

func TestDiffMovements(t *testing.T) {
	base := diffRun(map[string]bool{"Alpha": true, "Beta": false, "Gamma": true, "delta": false, "Echo": true})
	cur := diffRun(map[string]bool{"Alpha": true, "Beta": true, "Gamma": false, "delta": false, "Echo": true})

	d := Diff(cur, base)
	if d.ComparableCases() != 5 {
		t.Fatalf("comparable: %d", d.ComparableCases())
	}
	for _, c := range d.Cases {
		wantBase, wantCur := baseCase(base, c.Name), curPassed(cur, c.Name)
		if c.Baseline != wantBase || c.Current != wantCur {
			t.Errorf("case %s: %+v (want baseline=%v current=%v)", c.Name, c, wantBase, wantCur)
		}
	}
	if got := strings.Join(d.Regressions, ","); got != "Gamma" {
		t.Errorf("regressions: %q", got)
	}
	if got := strings.Join(d.Improvements, ","); got != "Beta" {
		t.Errorf("improvements: %q", got)
	}
	if got := strings.Join(d.Unchanged, ","); got != "Alpha,Echo,delta" {
		t.Errorf("unchanged: %q", got)
	}
	if len(d.OnlyInCurrent) != 0 || len(d.OnlyInBaseline) != 0 {
		t.Errorf("only-in lists: %v %v", d.OnlyInCurrent, d.OnlyInBaseline)
	}
}

func baseCase(base *RunResult, name string) bool {
	for _, c := range base.Cases {
		if c.Name == name {
			return c.Passed
		}
	}
	return false
}

func curPassed(cur *RunResult, name string) bool {
	for _, c := range cur.Cases {
		if c.Name == name {
			return c.Passed
		}
	}
	return false
}

func TestDiffOnlyIn(t *testing.T) {
	base := diffRun(map[string]bool{"Alpha": true, "bravo": false, "Zeta": true})
	cur := diffRun(map[string]bool{"Alpha": true, "Bravo": true, "Yankee": true})
	d := Diff(cur, base)
	if got := strings.Join(d.OnlyInBaseline, ","); got != "Zeta,bravo" {
		t.Errorf("only in baseline: %q", got)
	}
	if got := strings.Join(d.OnlyInCurrent, ","); got != "Bravo,Yankee" {
		t.Errorf("only in current: %q", got)
	}
	for _, list := range [][]string{d.Regressions, d.Improvements, d.Unchanged} {
		for _, name := range list {
			if name == "Zeta" || name == "bravo" || name == "Yankee" || name == "Bravo" {
				t.Errorf("excluded case %s leaked into %v", name, list)
			}
		}
	}
	if d.ComparableCases() != 1 {
		t.Errorf("comparable: %d", d.ComparableCases())
	}

	if _, err := DiffStrict(cur, base); err == nil {
		t.Error("strict diff must reject mismatched suites")
	} else if !strings.Contains(err.Error(), "didn't grade the same suite") {
		t.Errorf("strict error text: %v", err)
	}
	if _, err := DiffStrict(diffRun(map[string]bool{"Alpha": true}), diffRun(map[string]bool{"Alpha": false})); err != nil {
		t.Errorf("aligned suites must pass the strict gate: %v", err)
	}
}

func TestDiffScorerRates(t *testing.T) {
	base := &RunResult{Cases: []CaseResult{
		{Name: "x", Passed: true, Scores: []Score{{Name: "correctness", Passed: true}, {Name: "llm_judge", Passed: false}}},
		{Name: "y", Passed: true, Scores: []Score{{Name: "correctness", Passed: true}, {Name: "llm_judge", Passed: true}}},
	}}
	cur := &RunResult{Cases: []CaseResult{
		{Name: "x", Passed: true, Scores: []Score{{Name: "correctness", Passed: true}, {Name: "llm_judge", Passed: true}}},
		{Name: "y", Passed: true, Scores: []Score{{Name: "correctness", Passed: true}, {Name: "llm_judge", Passed: true}}},
	}}
	d := Diff(cur, base)
	if d.ScorerPassRates["correctness"] != (ScorerDelta{Baseline: 1, Current: 1}) {
		t.Errorf("correctness delta: %+v", d.ScorerPassRates["correctness"])
	}
	if d.ScorerPassRates["llm_judge"] != (ScorerDelta{Baseline: 0.5, Current: 1}) {
		t.Errorf("llm_judge delta: %+v", d.ScorerPassRates["llm_judge"])
	}
}

func TestDiffNilAndSummary(t *testing.T) {
	if d := Diff(nil, diffRun(map[string]bool{"Alpha": true})); d.ComparableCases() != 0 {
		t.Errorf("nil current must give an empty diff: %+v", d)
	}

	base := diffRun(map[string]bool{"Alpha": true, "Gamma": false, "Zeta": true})
	cur := diffRun(map[string]bool{"Alpha": true, "Gamma": true, "Beta": true})
	s := Diff(cur, base).Summary()
	for _, want := range []string{
		"0 regression(s), 1 improvement(s), 1 unchanged",
		// The scorer rates over the two shared cases: Alpha passes on both
		// sides, Gamma flips — 50% -> 100%.
		"s", "50.0% -> 100.0%",
		"flipped fail->pass: [Gamma]",
		"only in baseline: [Zeta]",
		"only in current: [Beta]",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("summary misses %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "flipped pass->fail") {
		t.Errorf("summary must not list regressions when there are none:\n%s", s)
	}

	// A drop marks the scorer line and lists the flip.
	s = Diff(diffRun(map[string]bool{"Gamma": false}), diffRun(map[string]bool{"Gamma": true})).Summary()
	for _, want := range []string{"1 regression(s)", "REGRESSION", "100.0% ->   0.0%", "flipped pass->fail: [Gamma]"} {
		if !strings.Contains(s, want) {
			t.Errorf("regression summary misses %q:\n%s", want, s)
		}
	}
}
