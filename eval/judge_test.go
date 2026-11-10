package eval

import (
	"context"
	"strings"
	"testing"
)

func verdictJSON(score string, reason string) string {
	return `{"score": ` + score + `, "reason": "` + reason + `"}`
}

func TestLLMJudge(t *testing.T) {
	sc := &scriptClient{bodies: []string{verdictJSON("0.8", "matches the rubric")}}
	j := LLMJudge{Client: sc, Rubric: "The answer names the capital."}
	got, err := j.Score(context.Background(), answeredTrace("The capital is Paris."))
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if got.Value != 0.8 || !got.Passed {
		t.Errorf("score: %+v", got)
	}
	if got.Name != "llm_judge" {
		t.Errorf("name: %q", got.Name)
	}
	if got.Details["reason"] != "matches the rubric" {
		t.Errorf("details: %+v", got.Details)
	}
	if len(sc.prompts) != 1 {
		t.Fatalf("judge must be exactly one client call, got %d", len(sc.prompts))
	}
	prompt := sc.prompts[0]
	if !strings.Contains(prompt, "The answer names the capital.") ||
		!strings.Contains(prompt, "The capital is Paris.") {
		t.Errorf("prompt misses rubric/answer: %q", prompt)
	}
}

func TestLLMJudgeLowScoreAndFences(t *testing.T) {
	sc := &scriptClient{bodies: []string{
		"```json\n" + verdictJSON("0.2", "wrong city") + "\n```",
	}}
	got, err := (LLMJudge{Client: sc, Rubric: "r"}).Score(context.Background(), answeredTrace("Lyon."))
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if got.Passed || got.Value != 0.2 {
		t.Errorf("score below the 0.5 midpoint fails: %+v", got)
	}
}

func TestLLMJudgeClamps(t *testing.T) {
	sc := &scriptClient{bodies: []string{verdictJSON("1.7", "overenthusiastic")}}
	got, err := (LLMJudge{Client: sc, Rubric: "r"}).Score(context.Background(), answeredTrace("great"))
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if got.Value != 1.0 || got.Details["clamped"] != true || got.Details["raw_score"] != 1.7 {
		t.Errorf("clamp not recorded: %+v", got)
	}
}

func TestLLMJudgeErrors(t *testing.T) {
	if _, err := (LLMJudge{Rubric: "r"}).Score(context.Background(), answeredTrace("x")); err == nil {
		t.Error("nil client must error")
	}
	sc := &scriptClient{bodies: []string{"I like it, good answer!"}}
	if _, err := (LLMJudge{Client: sc, Rubric: "r"}).Score(context.Background(), answeredTrace("x")); err == nil {
		t.Error("non-JSON verdict must error")
	}
	sc2 := &scriptClient{bodies: []string{"{\"reason\": \"no score here\"}"}}
	if _, err := (LLMJudge{Client: sc2, Rubric: "r"}).Score(context.Background(), answeredTrace("x")); err == nil {
		t.Error("verdict without score must error")
	}
	// Runner parity: the error becomes a failing score, the run moves on.
	got := ScoreWith(context.Background(), LLMJudge{Client: sc, Rubric: "r"}, answeredTrace("x"))
	if got.Passed || got.Details["error"] == nil {
		t.Errorf("ScoreWith should record the failure: %+v", got)
	}
}

func TestLLMJudgeNoAnswer(t *testing.T) {
	sc := &scriptClient{bodies: []string{verdictJSON("0", "empty")}}
	got, err := (LLMJudge{Client: sc, Rubric: "r"}).Score(context.Background(), BuildTrace(nil))
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if strings.Contains(sc.prompts[0], "no answer") == false {
		t.Errorf("judge should see the no-answer placeholder: %q", sc.prompts[0])
	}
	if got.Passed {
		t.Errorf("score 0 must fail: %+v", got)
	}
}

func TestPairwiseConsistentWinner(t *testing.T) {
	// Both orders prefer the first-position answer, which is A in order 1 and B
	// in order 2 — consistent, so A wins.
	sc := &scriptClient{bodies: []string{
		verdictPref("first", "sharper"),
		verdictPref("second", "sharper"),
	}}
	a := answeredTrace("Paris is the capital.")
	b := answeredTrace("Lyon maybe.")
	out, err := (Pairwise{Client: sc, Rubric: "names the capital"}).Compare(context.Background(), a, b)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if out.Winner != WinsA {
		t.Errorf("winner: %q", out.Winner)
	}
	if out.Detail["order1"] != "first" || out.Detail["order2"] != "second" {
		t.Errorf("detail: %+v", out.Detail)
	}
	if len(sc.prompts) != 2 {
		t.Fatalf("dual-order swap needs two calls, got %d", len(sc.prompts))
	}
	// Order 1 shows A first; order 2 shows B first.
	first := sc.prompts[0][strings.Index(sc.prompts[0], "## Response 1"):]
	second := sc.prompts[1][strings.Index(sc.prompts[1], "## Response 1"):]
	if !strings.Contains(first, "Paris") || !strings.Contains(second, "Lyon") {
		t.Errorf("swap wrong: %q then %q", first, second)
	}
}

func TestPairwiseFlipIsTie(t *testing.T) {
	// Both orders prefer "first" positionally: A wins order 1, B wins order 2 —
	// a position flip resolves to a tie.
	sc := &scriptClient{bodies: []string{
		verdictPref("first", "looks better"),
		verdictPref("first", "looks better"),
	}}
	out, err := (Pairwise{Client: sc, Rubric: "r"}).Compare(
		context.Background(), answeredTrace("A answer"), answeredTrace("B answer"))
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if out.Winner != WinsTie {
		t.Errorf("flip must be a tie, got %q", out.Winner)
	}
	if out.Reasoning != "looks better" {
		t.Errorf("reasoning comes from order 1: %q", out.Reasoning)
	}
}

func TestPairwiseBWins(t *testing.T) {
	sc := &scriptClient{bodies: []string{
		verdictPref("second", "B is right"),
		verdictPref("first", "B is right"),
	}}
	out, err := (Pairwise{Client: sc, Rubric: "r"}).Compare(
		context.Background(), answeredTrace("A"), answeredTrace("B"))
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if out.Winner != WinsB {
		t.Errorf("winner: %q", out.Winner)
	}
}

func TestPairwiseErrors(t *testing.T) {
	if _, err := (Pairwise{Rubric: "r"}).Compare(context.Background(), answeredTrace("a"), answeredTrace("b")); err == nil {
		t.Error("nil client must error")
	}
	sc := &scriptClient{bodies: []string{`{"preferred": "left", "reason": "x"}`, verdictPref("first", "y")}}
	if _, err := (Pairwise{Client: sc, Rubric: "r"}).Compare(
		context.Background(), answeredTrace("a"), answeredTrace("b")); err == nil {
		t.Error("unknown preference must error")
	}
	// An empty preferred (missing verdict field) degrades to a tie, not an error.
	sc2 := &scriptClient{bodies: []string{`{"reason": "silent"}`, `{"reason": "silent"}`}}
	out, err := (Pairwise{Client: sc2, Rubric: "r"}).Compare(
		context.Background(), answeredTrace("a"), answeredTrace("b"))
	if err != nil {
		t.Fatalf("empty verdict should be a tie: %v", err)
	}
	if out.Winner != WinsTie {
		t.Errorf("winner: %q", out.Winner)
	}
}

func verdictPref(pref, reason string) string {
	return `{"preferred": "` + pref + `", "reason": "` + reason + `"}`
}

// StructuredOutput asks the provider to constrain the answer to the verdict
// schema; off, the call carries no schema (the old wire shape).
func TestLLMJudgeStructuredOutput(t *testing.T) {
	sc := &scriptClient{bodies: []string{verdictJSON("0.9", "good")}}
	if _, err := (LLMJudge{Client: sc, Rubric: "r", StructuredOutput: true}).Score(context.Background(), answeredTrace("x")); err != nil {
		t.Fatalf("score: %v", err)
	}
	if len(sc.opts) != 1 || sc.opts[0].ResponseSchema == nil {
		t.Fatalf("structured output must send a response schema: %+v", sc.opts)
	}
	schema := sc.opts[0].ResponseSchema.JSONSchema()
	if _, ok := schema["properties"].(map[string]any)["score"]; !ok {
		t.Fatalf("schema does not describe the verdict: %#v", schema)
	}

	plain := &scriptClient{bodies: []string{verdictJSON("0.9", "good")}}
	if _, err := (LLMJudge{Client: plain, Rubric: "r"}).Score(context.Background(), answeredTrace("x")); err != nil {
		t.Fatalf("score: %v", err)
	}
	if plain.opts[0].ResponseSchema != nil {
		t.Fatal("without StructuredOutput the call must carry no schema")
	}
}

// An invalid verdict is re-asked, with the validation error in the re-ask;
// the retries are spent, it is an error.
func TestLLMJudgeRetriesInvalidVerdict(t *testing.T) {
	sc := &scriptClient{bodies: []string{"I think it is great!", verdictJSON("0.7", "after retry")}}
	got, err := (LLMJudge{Client: sc, Rubric: "r", Retries: 1}).Score(context.Background(), answeredTrace("x"))
	if err != nil {
		t.Fatalf("score after a retry: %v", err)
	}
	if got.Value != 0.7 || len(sc.prompts) != 2 {
		t.Fatalf("value=%v calls=%d", got.Value, len(sc.prompts))
	}
	if !strings.Contains(sc.prompts[1], "could not be validated") ||
		!strings.Contains(sc.prompts[1], "no JSON verdict") {
		t.Fatalf("re-ask must say what failed: %q", sc.prompts[1])
	}

	exhausted := &scriptClient{bodies: []string{"nope", "still nope"}}
	if _, err := (LLMJudge{Client: exhausted, Rubric: "r", Retries: 1}).Score(context.Background(), answeredTrace("x")); err == nil {
		t.Fatal("an invalid verdict past the retries must error")
	}
	if len(exhausted.prompts) != 2 {
		t.Fatalf("retries=1 must make exactly two calls, got %d", len(exhausted.prompts))
	}

	// A verdict without a score is a validation failure too, so it retries.
	noScore := &scriptClient{bodies: []string{`{"reason": "no score"}`, verdictJSON("0.4", "ok")}}
	if _, err := (LLMJudge{Client: noScore, Rubric: "r", Retries: 1}).Score(context.Background(), answeredTrace("x")); err != nil {
		t.Fatalf("score: %v", err)
	}
	if !strings.Contains(noScore.prompts[1], "no score") {
		t.Fatalf("re-ask must carry the validation error: %q", noScore.prompts[1])
	}
}

// SingleOrder makes one judge call and reports that order's preference: no
// swap, no flip-to-tie defense, and the detail carries one order only.
func TestPairwiseSingleOrder(t *testing.T) {
	sc := &scriptClient{bodies: []string{`{"preferred": "first", "reason": "clearer"}`}}
	out, err := (Pairwise{Client: sc, Rubric: "r", SingleOrder: true}).Compare(
		context.Background(), answeredTrace("answer a"), answeredTrace("answer b"))
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if out.Winner != WinsA {
		t.Fatalf("winner = %q, want a (response 1 was preferred)", out.Winner)
	}
	if out.Reasoning != "clearer" {
		t.Fatalf("reasoning = %q", out.Reasoning)
	}
	if len(sc.prompts) != 1 {
		t.Fatalf("single-order mode must make one call, got %d", len(sc.prompts))
	}
	if _, ok := out.Detail["order2"]; ok {
		t.Fatalf("single-order detail must not claim a second order: %+v", out.Detail)
	}
	if out.Detail["order1"] != "first" {
		t.Fatalf("detail = %+v", out.Detail)
	}

	// The same verdict in dual-order mode is a tie (the swapped call would
	// prefer the other side), which is what SingleOrder skips.
	dual := &scriptClient{bodies: []string{
		`{"preferred": "first", "reason": "clearer"}`,
		`{"preferred": "first", "reason": "clearer"}`,
	}}
	if out, err := (Pairwise{Client: dual, Rubric: "r"}).Compare(
		context.Background(), answeredTrace("answer a"), answeredTrace("answer b")); err != nil {
		t.Fatalf("compare: %v", err)
	} else if out.Winner != WinsTie || len(dual.prompts) != 2 {
		t.Fatalf("dual order: winner=%q calls=%d", out.Winner, len(dual.prompts))
	}
}
