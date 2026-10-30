package eval

import (
	"context"
	"errors"
	"testing"

	"ago/events"
)

func answeredTrace(final string, evs ...events.Event) *Trace {
	if len(evs) == 0 {
		evs = []events.Event{&events.ModelResponse{}}
	}
	tr := BuildTrace(evs)
	tr.FinalResponse = final
	return tr
}

func TestCorrectnessModes(t *testing.T) {
	tr := answeredTrace("The capital is Paris.", &events.ModelResponse{})
	for _, tc := range []struct {
		name string
		c    Correctness
		want bool
	}{
		{"contains", Correctness{Expected: "Paris", Mode: MatchContains}, true},
		{"contains miss", Correctness{Expected: "Lyon", Mode: MatchContains}, false},
		{"exact", Correctness{Expected: "The capital is Paris.", Mode: MatchExact}, true},
		{"exact case miss", Correctness{Expected: "the capital is paris.", Mode: MatchExact}, false},
		{"zero mode is casefold", Correctness{Expected: "THE CAPITAL IS PARIS."}, true},
	} {
		got, err := tc.c.Score(context.Background(), tr)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got.Passed != tc.want {
			t.Errorf("%s: passed=%v value=%v", tc.name, got.Passed, got.Value)
		}
		if got.Name != "correctness" {
			t.Errorf("%s: name %q", tc.name, got.Name)
		}
		if got.Value != 0 && got.Value != 1 {
			t.Errorf("%s: boolean scorers carry 1.0/0.0, got %v", tc.name, got.Value)
		}
	}
}

func TestCorrectnessNoAnswer(t *testing.T) {
	tr := BuildTrace(nil) // no model call at all
	got, err := Correctness{Expected: "", Mode: MatchExact}.Score(context.Background(), tr)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if got.Passed {
		t.Error("no answer must not pass even against an empty expectation")
	}
	if got.Details["reason"] != "no model response" {
		t.Errorf("details: %+v", got.Details)
	}
}

func TestTokenBudget(t *testing.T) {
	evs := []events.Event{
		&events.UsageEvent{Usage: usageOf(30, 10), Kind: events.UsageKindModelCall},
		&events.UsageEvent{Usage: usageOf(2, 3), Kind: events.UsageKindSubtask}, // rollups count
	}
	tr := BuildTrace(evs)
	got, err := TokenBudget{Max: 45}.Score(context.Background(), tr)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if !got.Passed || got.Value != 1 {
		t.Errorf("45 budget over 45 tokens should pass: %+v", got)
	}
	got, err = TokenBudget{Max: 44}.Score(context.Background(), tr)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if got.Passed || got.Value != 0 {
		t.Errorf("44 budget over 45 tokens should fail: %+v", got)
	}
	if got.Details["tokens"] != int64(45) {
		t.Errorf("details: %+v", got.Details)
	}
}

func TestToolsCalled(t *testing.T) {
	c1 := events.ToolCallEvent{ID: "1", Name: "get_weather", Arguments: "{}"}
	c2 := events.ToolCallEvent{ID: "2", Name: "search", Arguments: "{}"}
	tr := BuildTrace([]events.Event{&c1, &c2})
	for _, tc := range []struct {
		name string
		s    ToolsCalled
		want bool
	}{
		{"all present", ToolsCalled{Names: []string{"get_weather", "search"}}, true},
		{"all missing one", ToolsCalled{Names: []string{"get_weather", "nope"}}, false},
		{"any hit", ToolsCalled{Names: []string{"nope", "search"}, Mode: CallAny}, true},
		{"any miss", ToolsCalled{Names: []string{"nope", "nada"}, Mode: CallAny}, false},
		{"no names", ToolsCalled{}, false},
	} {
		got, err := tc.s.Score(context.Background(), tr)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got.Passed != tc.want {
			t.Errorf("%s: passed=%v", tc.name, got.Passed)
		}
	}
}

func TestThreshold(t *testing.T) {
	quality := staticScorer{sc: Score{Name: "quality", Value: 0.8, Details: map[string]any{"source": "test"}}}
	got, err := Threshold{Inner: quality, Min: 0.7}.Score(context.Background(), answeredTrace("x"))
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if !got.Passed || got.Value != 0.8 {
		t.Errorf("gate 0.7 over 0.8 should pass: %+v", got)
	}
	if got.Name != "quality" {
		t.Errorf("gate keeps the inner name (Python parity): %q", got.Name)
	}
	if got.Details["min"] != 0.7 || got.Details["source"] != "test" {
		t.Errorf("details: %+v", got.Details)
	}
	low, err := Threshold{Inner: quality, Min: 0.9}.Score(context.Background(), answeredTrace("x"))
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if low.Passed {
		t.Errorf("gate 0.9 over 0.8 should fail: %+v", low)
	}
}

func TestThresholdUngradeableFails(t *testing.T) {
	boom := staticScorer{err: errors.New("judge offline")}
	got, err := Threshold{Inner: boom, Min: 0.5}.Score(context.Background(), answeredTrace("x"))
	if err != nil {
		t.Fatalf("an inner error must not abort: %v", err)
	}
	if got.Passed {
		t.Error("ungradeable inner is a fail (Python: score=None → fail)")
	}
	if got.Details["error"] != "judge offline" {
		t.Errorf("details: %+v", got.Details)
	}
}

// staticScorer returns a canned score or error.
type staticScorer struct {
	sc  Score
	err error
}

func (s staticScorer) Name() string { return s.sc.Name }

func (s staticScorer) Score(context.Context, *Trace) (Score, error) { return s.sc, s.err }
