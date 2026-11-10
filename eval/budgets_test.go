package eval

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/luckystrker/ago/events"
)

// A run counts the cases over its budget and still grades every one of them:
// the count is a regression signal, not a kill switch.
func TestRunBudgetViolations(t *testing.T) {
	a := scriptedAgent(t,
		textResp("one"),
		textResp("two"),
		textResp("three"),
	)
	suite := Suite{
		{Name: "cheap", Input: "a", Scorers: []Scorer{TokenBudget{Max: 100}}},
		{Name: "pricey", Input: "b", Scorers: []Scorer{TokenBudget{Max: 100}}},
		{Name: "also-cheap", Input: "c", Scorers: []Scorer{TokenBudget{Max: 100}}},
	}
	res, err := Run(context.Background(), a, suite,
		WithConcurrency(1),
		WithBudgets(BudgetThresholds{MaxTokensPerTask: 1}))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.BudgetViolations != 3 {
		t.Fatalf("budget violations = %d, want 3 (every case spends 3 tokens)", res.BudgetViolations)
	}
	if res.PassRate != 1 {
		t.Fatalf("pass rate = %v: budgets must not fail a case", res.PassRate)
	}

	// A budget nothing exceeds counts nothing; no budget at all counts
	// nothing either (the old behavior).
	res, err = Run(context.Background(), a, suite,
		WithConcurrency(1),
		WithBudgets(BudgetThresholds{MaxTokensPerTask: 1000}))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.BudgetViolations != 0 {
		t.Fatalf("budget violations = %d, want 0", res.BudgetViolations)
	}

	// A seconds-only budget is measured too: a run that takes any measurable
	// time exceeds a near-zero ceiling, so the threshold must be enabled on
	// its own (a token-only budget would leave the count at zero). A fresh
	// agent, because a scripted model's answers are consumed by a run.
	fresh := scriptedAgent(t, textResp("one"), textResp("two"), textResp("three"))
	res, err = Run(context.Background(), fresh, suite,
		WithConcurrency(1),
		WithBudgets(BudgetThresholds{MaxSecondsPerTask: 1e-9}))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.BudgetViolations != 3 {
		t.Fatalf("seconds-only budget violations = %d, want 3", res.BudgetViolations)
	}
}

func TestBudgetThresholdsExceededBy(t *testing.T) {
	start := time.Now()
	tr := &Trace{StartedAt: start, FinishedAt: start.Add(2 * time.Second), Events: []events.Event{
		&events.UsageEvent{Usage: usageOf(30, 20), Kind: events.UsageKindModelCall},
	}}
	cases := []struct {
		name string
		b    BudgetThresholds
		want bool
	}{
		{"no thresholds", BudgetThresholds{}, false},
		{"tokens under", BudgetThresholds{MaxTokensPerTask: 60}, false},
		{"tokens over", BudgetThresholds{MaxTokensPerTask: 40}, true},
		{"seconds under", BudgetThresholds{MaxSecondsPerTask: 3}, false},
		{"seconds over", BudgetThresholds{MaxSecondsPerTask: 1}, true},
	}
	for _, tc := range cases {
		if got := tc.b.ExceededBy(tr); got != tc.want {
			t.Errorf("%s: ExceededBy = %v, want %v", tc.name, got, tc.want)
		}
	}
	if (BudgetThresholds{MaxTokensPerTask: 1}).ExceededBy(nil) {
		t.Error("a nil trace exceeds nothing")
	}
}

// The suite's name and source land on the result (Python parity: Suite.name /
// Suite.source); without them the fields stay empty.
func TestRunRecordsSuiteNameAndSource(t *testing.T) {
	a := scriptedAgent(t, textResp("ok"))
	suite := Suite{{Name: "only", Input: "a"}}
	res, err := Run(context.Background(), a, suite,
		WithConcurrency(1),
		WithSuiteName("smoke"),
		WithSuiteSource("/data/smoke.jsonl"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.SuiteName != "smoke" || res.SuiteSource != "/data/smoke.jsonl" {
		t.Fatalf("suite metadata: %q %q", res.SuiteName, res.SuiteSource)
	}

	res, err = Run(context.Background(), a, suite, WithConcurrency(1))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.SuiteName != "" || res.SuiteSource != "" {
		t.Fatalf("suite metadata without options: %q %q", res.SuiteName, res.SuiteSource)
	}
}

// Tags and metadata travel from the dataset line through the case to the case
// result.
func TestCaseTagsAndMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "suite.jsonl")
	line := `{"name":"tagged","input":"hi","tags":["happy-path","adversarial"],` +
		`"metadata":{"owner":"team-a","priority":2}}`
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	suite, err := SuiteFromJSONL(path, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(suite) != 1 {
		t.Fatalf("suite: %+v", suite)
	}
	if got := suite[0].Tags; len(got) != 2 || got[0] != "happy-path" || got[1] != "adversarial" {
		t.Fatalf("tags = %v", got)
	}
	if suite[0].Metadata["owner"] != "team-a" {
		t.Fatalf("metadata = %+v", suite[0].Metadata)
	}

	a := scriptedAgent(t, textResp("ok"))
	res, err := Run(context.Background(), a, suite, WithConcurrency(1))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := res.Cases[0].Tags; len(got) != 2 || got[1] != "adversarial" {
		t.Fatalf("case result tags = %v", got)
	}

	// A malformed tags field is a load error naming the field.
	bad := filepath.Join(t.TempDir(), "bad.jsonl")
	if err := os.WriteFile(bad, []byte(`{"input":"hi","tags":"nope"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SuiteFromJSONL(bad, nil); err == nil {
		t.Fatal("a non-list tags field must be a load error")
	}
}

// A value exactly at the limit is not a violation — the ceiling is inclusive —
// and anything above it is. Both thresholds are checked at their boundary.
func TestBudgetThresholdsExactLimit(t *testing.T) {
	start := time.Now()
	trace := func(tokens int64, d time.Duration) *Trace {
		return &Trace{StartedAt: start, FinishedAt: start.Add(d), Events: []events.Event{
			&events.UsageEvent{Usage: usageOf(tokens, 0), Kind: events.UsageKindModelCall},
		}}
	}

	cases := []struct {
		name  string
		b     BudgetThresholds
		trace *Trace
		want  bool
	}{
		{"tokens exactly at the limit", BudgetThresholds{MaxTokensPerTask: 50}, trace(50, 0), false},
		{"tokens one over the limit", BudgetThresholds{MaxTokensPerTask: 50}, trace(51, 0), true},
		{"tokens one under the limit", BudgetThresholds{MaxTokensPerTask: 50}, trace(49, 0), false},
		{"seconds exactly at the limit", BudgetThresholds{MaxSecondsPerTask: 2}, trace(1, 2*time.Second), false},
		{"seconds just over the limit", BudgetThresholds{MaxSecondsPerTask: 2}, trace(1, 2*time.Second+time.Millisecond), true},
		{"seconds just under the limit", BudgetThresholds{MaxSecondsPerTask: 2}, trace(1, 2*time.Second-time.Millisecond), false},
	}
	for _, tc := range cases {
		if got := tc.b.ExceededBy(tc.trace); got != tc.want {
			t.Errorf("%s: ExceededBy = %v, want %v (tokens %d, duration %v)",
				tc.name, got, tc.want, tc.trace.TotalTokens(), tc.trace.Duration())
		}
	}
}
