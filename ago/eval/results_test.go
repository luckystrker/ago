package eval

import (
	stdcontext "context"
	"path/filepath"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	a := scriptedAgent(t, textResp("Paris!"))
	res, err := Run(stdcontext.Background(), a, Suite{
		{Name: "cap", Input: "capital?", Expected: "Paris", Scorers: []Scorer{
			Correctness{Expected: "Paris", Mode: MatchContains},
			TokenBudget{Max: 100},
		}},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	path := filepath.Join(t.TempDir(), "nested", "run.json")
	if err := res.SaveJSON(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := LoadJSON(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if loaded.PassRate != res.PassRate || len(loaded.Cases) != 1 {
		t.Fatalf("loaded: %+v", loaded)
	}
	src, dst := res.Cases[0], loaded.Cases[0]
	if src.Name != dst.Name || src.Output != dst.Output || src.Passed != dst.Passed || src.Err != dst.Err {
		t.Errorf("case drift: %+v vs %+v", src, dst)
	}
	if len(src.Scores) != len(dst.Scores) {
		t.Fatalf("scores: %d vs %d", len(src.Scores), len(dst.Scores))
	}
	for i := range src.Scores {
		if src.Scores[i].Name != dst.Scores[i].Name || src.Scores[i].Passed != dst.Scores[i].Passed {
			t.Errorf("score %d drift: %+v vs %+v", i, src.Scores[i], dst.Scores[i])
		}
	}
	if dst.Trace == nil || dst.Trace.AgentName != "evaluated" || dst.Trace.FinalResponse != "Paris!" {
		t.Errorf("trace drift: %+v", dst.Trace)
	}
	// Events round-trip through the "@type" registry, so usage still sums.
	if len(dst.Trace.Events) != len(src.Trace.Events) {
		t.Fatalf("events: %d vs %d", len(dst.Trace.Events), len(src.Trace.Events))
	}
	if got := *dst.Trace.Tokens().TotalTokens; got != *src.Trace.Tokens().TotalTokens {
		t.Errorf("tokens after load: %d", got)
	}
	if len(dst.Trace.LLMCalls) != 1 || dst.Trace.LLMCalls[0].OutputText != "Paris!" {
		t.Errorf("llm calls after load: %+v", dst.Trace.LLMCalls)
	}
}

func TestCompareThreeCases(t *testing.T) {
	pass := func(name string) CaseResult {
		return CaseResult{Name: name, Passed: true, Scores: []Score{{Name: "s", Passed: true}}}
	}
	fail := func(name string) CaseResult {
		return CaseResult{Name: name, Passed: false, Scores: []Score{{Name: "s"}}}
	}
	a := &RunResult{Cases: []CaseResult{pass("shared-pass"), fail("shared-fail"), pass("shared-tie"), pass("only-a")}}
	b := &RunResult{Cases: []CaseResult{pass("shared-pass"), pass("shared-fail"), fail("shared-tie"), fail("only-b")}}

	cmp := Compare(a, b)
	if cmp.AWins != 1 || cmp.BWins != 1 || cmp.Ties != 1 {
		t.Errorf("tally: %+v", cmp)
	}
	// Python parity: B's win-rate, ties count 0.5 → (1 + 0.5) / 3.
	if cmp.WinRate != 0.5 {
		t.Errorf("win rate: %v", cmp.WinRate)
	}
	if cmp := Compare(nil, b); cmp != (Comparison{}) {
		t.Errorf("nil run: %+v", cmp)
	}
	if cmp := Compare(a, &RunResult{}); cmp != (Comparison{}) {
		t.Errorf("no shared cases: %+v", cmp)
	}
}
