package eval

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/luckystrker/ago/events"
)

// score is the interface method call shorthand used throughout these tests.
func score(ctx context.Context, s Scorer, tr *Trace) (Score, error) { return s.Score(ctx, tr) }

func TestRegistryBuiltins(t *testing.T) {
	r := NewScorerRegistry()

	// correctness without an expected param falls back to the case scope.
	sc, err := r.Build("correctness", nil, CaseScope{Expected: "the capital is Paris."})
	if err != nil {
		t.Fatalf("build correctness: %v", err)
	}
	got, err := score(context.Background(), sc, answeredTrace("The capital is Paris."))
	if err != nil || !got.Passed {
		t.Errorf("scope expected: err=%v score=%+v", err, got)
	}
	if sc.Name() != "correctness" {
		t.Errorf("name: %q", sc.Name())
	}

	// An explicit expected overrides the scope; a miss fails.
	sc, err = r.Build("correctness", map[string]any{"expected": "Lyon"}, CaseScope{Expected: "the capital is Paris."})
	if err != nil {
		t.Fatalf("build correctness: %v", err)
	}
	if got, _ := score(context.Background(), sc, answeredTrace("The capital is Paris.")); got.Passed {
		t.Errorf("explicit expected must win over the scope: %+v", got)
	}

	// The mode param is validated.
	if _, err = r.Build("correctness", map[string]any{"mode": "regex"}, CaseScope{}); err == nil {
		t.Error("unknown mode must error")
	}
	sc, _ = r.Build("correctness", map[string]any{"expected": "PARIS", "mode": "casefold"}, CaseScope{})
	if got, _ := score(context.Background(), sc, answeredTrace("paris")); !got.Passed {
		t.Errorf("casefold mode: %+v", got)
	}

	// token_budget takes an integral max; a fraction is rejected.
	sc, err = r.Build("token_budget", map[string]any{"max": 3.0}, CaseScope{})
	if err != nil {
		t.Fatalf("build token_budget: %v", err)
	}
	tr := BuildTrace([]events.Event{
		&events.UsageEvent{Usage: events.Usage{PromptTokens: events.I64(1), CompletionTokens: events.I64(2)}, Kind: events.UsageKindModelCall},
		&events.ModelResponse{Message: &events.ModelMessage{Content: "hi"}},
	})
	if got, _ := score(context.Background(), sc, tr); !got.Passed {
		t.Errorf("3 tokens under max 3: %+v", got)
	}
	if _, err = r.Build("token_budget", map[string]any{"max": 2.5}, CaseScope{}); err == nil {
		t.Error("fractional max must error")
	}
	if _, err = r.Build("token_budget", map[string]any{"max": "three"}, CaseScope{}); err == nil {
		t.Error("string max must error")
	}

	// tools_called takes names and an optional mode.
	sc, err = r.Build("tools_called", map[string]any{"names": []any{"get_weather"}}, CaseScope{})
	if err != nil {
		t.Fatalf("build tools_called: %v", err)
	}
	if got, _ = score(context.Background(), sc, failedToolTrace("")); !got.Passed {
		t.Errorf("called tool: %+v", got)
	}
	if _, err = r.Build("tools_called", map[string]any{"names": "get_weather"}, CaseScope{}); err == nil {
		t.Error("scalar names must error")
	}

	// Unknown names, params, and duplicates are errors.
	if _, err = r.Build("nonsense", nil, CaseScope{}); err == nil {
		t.Error("unknown scorer must error")
	} else if want := "registered"; !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "correctness") {
		t.Errorf("unknown-scorer error should list the registry: %v", err)
	}
	if _, err = r.Build("correctness", map[string]any{"expexted": "typo"}, CaseScope{}); err == nil {
		t.Error("unknown param must error")
	}
	if err = r.Register("correctness", func(map[string]any, CaseScope) (Scorer, error) { return nil, nil }); err == nil {
		t.Error("duplicate register must error")
	}
	if err = r.Register("", func(map[string]any, CaseScope) (Scorer, error) { return nil, nil }); err == nil {
		t.Error("empty name must error")
	}
	for _, name := range []string{"correctness", "token_budget", "tools_called", "threshold", "llm_judge", "failure"} {
		if !strings.Contains(strings.Join(r.Names(), ","), name) {
			t.Errorf("registry is missing built-in %q: %v", name, r.Names())
		}
	}
}

type lenScorer struct{}

func (lenScorer) Name() string { return "answer_len" }
func (lenScorer) Score(_ context.Context, tr *Trace) (Score, error) {
	return boolScore("answer_len", len(tr.FinalResponse) > 0, nil), nil
}

func TestRegistryUserScorer(t *testing.T) {
	r := NewScorerRegistry()
	if err := r.Register("answer_len", func(_ map[string]any, _ CaseScope) (Scorer, error) {
		return lenScorer{}, nil
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	// User factories own their params — no built-in key check applies.
	sc, err := r.Build("answer_len", map[string]any{"anything": true}, CaseScope{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := ScoreWith(context.Background(), sc, answeredTrace("x")); !got.Passed {
		t.Errorf("score: %+v", got)
	}
}

func TestRegistryThresholdNested(t *testing.T) {
	r := NewScorerRegistry()
	r.SetClient(&scriptClient{bodies: []string{verdictJSON("0.9", "solid")}})
	sc, err := r.Build("threshold", map[string]any{
		"inner": map[string]any{"name": "llm_judge", "params": map[string]any{"rubric": "names the capital"}},
		"min":   0.5,
	}, CaseScope{})
	if err != nil {
		t.Fatalf("build threshold: %v", err)
	}
	got, err := score(context.Background(), sc, answeredTrace("The capital is Paris."))
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	// Threshold keeps the inner scorer's name and value.
	if got.Name != "llm_judge" || got.Value != 0.9 || !got.Passed {
		t.Errorf("threshold over judge: %+v", got)
	}
	// A malformed nested spec is an error, not a panic.
	for _, bad := range []map[string]any{
		{"inner": "llm_judge", "min": 0.5},
		{"inner": map[string]any{"params": map[string]any{}}, "min": 0.5},
		{"inner": map[string]any{"name": "llm_judge"}, "min": 0.5},
		{"inner": map[string]any{"name": "llm_judge"}, "min": "low"},
	} {
		if _, err = r.Build("threshold", bad, CaseScope{}); err == nil {
			t.Errorf("bad threshold spec must error: %v", bad)
		}
	}
}

func TestRegistryClientBacked(t *testing.T) {
	r := NewScorerRegistry()
	if _, err := r.Build("llm_judge", map[string]any{"rubric": "r"}, CaseScope{}); err == nil {
		t.Error("llm_judge without a client must error")
	}
	sc := &scriptClient{bodies: []string{verdictJSON("0.8", "ok"), `{"failed": true, "error_mode": "incorrect_answer", "decisive_step": -1, "reasoning": "wrong"}`}}
	r.SetClient(sc)

	judge, err := r.Build("llm_judge", map[string]any{"rubric": "names the capital"}, CaseScope{})
	if err != nil {
		t.Fatalf("build llm_judge: %v", err)
	}
	if got, _ := score(context.Background(), judge, answeredTrace("Paris.")); !got.Passed {
		t.Errorf("judge score: %+v", got)
	}

	// failure picks up the client too: a clean trace goes to the LLM attributor.
	attr, err := r.Build("failure", map[string]any{"agent_name": "a"}, CaseScope{})
	if err != nil {
		t.Fatalf("build failure: %v", err)
	}
	got, err := score(context.Background(), attr, answeredTrace("Paris."))
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if got.Passed || got.Details["error_mode"] != "incorrect_answer" {
		t.Errorf("attributed score: %+v", got)
	}
}

func TestRegistryConcurrentUse(t *testing.T) {
	r := NewScorerRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_ = r.Register(fmt.Sprintf("user-%d", i), func(map[string]any, CaseScope) (Scorer, error) {
				return lenScorer{}, nil
			})
		}(i)
		go func() {
			defer wg.Done()
			if sc, err := r.Build("correctness", map[string]any{"expected": "x"}, CaseScope{}); err == nil {
				_, _ = score(context.Background(), sc, answeredTrace("x"))
			}
		}()
	}
	wg.Wait()
	if got := len(r.Names()); got != 6+8 {
		t.Errorf("names: %d", got)
	}
}
