package eval

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/luckystrker/ago/events"
)

func failedToolTrace(final string) *Trace {
	call := events.ToolCallEvent{ID: "c1", Name: "get_weather", Arguments: `{"city":"Paris"}`}
	errEv := &events.ToolErrorEvent{
		ToolResultEvent: events.ToolResultEvent{ParentID: "c1", Name: "get_weather"},
		Error:           "api down",
	}
	evs := []events.Event{
		&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "weather in Paris?"}}},
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{call}}},
		&call,
		errEv,
	}
	tr := BuildTrace(evs)
	tr.FinalResponse = final
	return tr
}

func TestDetectMechanical(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tr         *Trace
		wantNil    bool
		wantMode   string
		wantFailed bool
		wantStep   *int
	}{
		{
			name:       "crash",
			tr:         &Trace{Err: "boom", Events: []events.Event{&events.ModelResponse{}, &events.ModelResponse{}}},
			wantMode:   "crash",
			wantFailed: true,
			wantStep:   intPtr(1), // the last event, Python: events[-1]
		},
		{
			name:       "crash with no events",
			tr:         &Trace{Err: "boom"},
			wantMode:   "crash",
			wantFailed: true,
			wantStep:   nil,
		},
		{
			name:       "tool failure, no answer",
			tr:         failedToolTrace(""),
			wantMode:   "tool_failure",
			wantFailed: true,
			wantStep:   intPtr(3), // the ToolErrorEvent's index in the history
		},
		{
			name:    "tool failure but answered is semantic",
			tr:      failedToolTrace("It is sunny."),
			wantNil: true,
		},
		{
			name: "tool not found counts as tool failure",
			tr: BuildTrace([]events.Event{&events.ToolNotFoundEvent{ToolErrorEvent: events.ToolErrorEvent{
				ToolResultEvent: events.ToolResultEvent{ParentID: "x", Name: "nope"},
				Error:           "not registered",
			}}}),
			wantMode:   "tool_failure",
			wantFailed: true,
			wantStep:   intPtr(0),
		},
		{
			name:       "no answer",
			tr:         BuildTrace(nil),
			wantMode:   "no_answer",
			wantFailed: true,
			wantStep:   nil,
		},
		{
			name:    "answered cleanly is not mechanical",
			tr:      answeredTrace("The capital is Paris."),
			wantNil: true,
		},
	} {
		got := DetectMechanical(tc.tr)
		if tc.wantNil {
			if got != nil {
				t.Errorf("%s: want nil, got %+v", tc.name, got)
			}
			continue
		}
		if got == nil {
			t.Errorf("%s: want %s, got nil", tc.name, tc.wantMode)
			continue
		}
		if got.ErrorMode != tc.wantMode || got.Failed != tc.wantFailed {
			t.Errorf("%s: mode=%s failed=%v, want %s/%v", tc.name, got.ErrorMode, got.Failed, tc.wantMode, tc.wantFailed)
		}
		if (got.DecisiveStep == nil) != (tc.wantStep == nil) ||
			(got.DecisiveStep != nil && *got.DecisiveStep != *tc.wantStep) {
			t.Errorf("%s: decisive step %v, want %v", tc.name, got.DecisiveStep, tc.wantStep)
		}
	}
}

func intPtr(i int) *int { return &i }

func TestFailureAttributionDeterministic(t *testing.T) {
	f := FailureAttribution{AgentName: "evaluated"}
	got, err := f.Score(context.Background(), answeredTrace("The capital is Paris."))
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if got.Name != "failure" || !got.Passed || got.Value != 1.0 {
		t.Errorf("clean trace: %+v", got)
	}
	if got.Details["error_mode"] != "none" || got.Details["responsible_agent"] != "evaluated" {
		t.Errorf("clean trace details: %+v", got.Details)
	}
	if got.Details["reasoning"] != "no mechanical failure detected" {
		t.Errorf("clean trace reasoning: %+v", got.Details)
	}

	got, err = f.Score(context.Background(), BuildTrace(nil))
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if got.Passed || got.Value != 0.0 || got.Details["error_mode"] != "no_answer" {
		t.Errorf("no-answer trace: %+v", got)
	}

	// Key override: the score is reported under the custom key.
	named := FailureAttribution{Key: "who_failed"}
	if got, _ := named.Score(context.Background(), answeredTrace("ok")); got.Name != "who_failed" {
		t.Errorf("custom key: %q", got.Name)
	}
}

func TestFailureAttributionLLM(t *testing.T) {
	tr := answeredTrace("The capital is Lyon.")

	sc := &scriptClient{bodies: []string{
		`{"failed": true, "error_mode": "incorrect_answer", "decisive_step": 2, "reasoning": "wrong city"}`,
	}}
	f := FailureAttribution{Client: sc, AgentName: "evaluated"}
	got, err := f.Score(context.Background(), tr)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if got.Passed || got.Value != 0.0 || got.Details["error_mode"] != "incorrect_answer" {
		t.Errorf("verdict: %+v", got)
	}
	if got.Details["decisive_step"] != 2 || got.Details["responsible_agent"] != "evaluated" {
		t.Errorf("verdict details: %+v", got.Details)
	}
	if len(sc.prompts) != 1 {
		t.Fatalf("one attributor call, got %d", len(sc.prompts))
	}
	prompt := sc.prompts[0]
	for _, want := range []string{"## Task", "## Final answer", "## Trajectory (numbered steps)", "[0] model:", "incorrect_answer"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt misses %q: %q", want, prompt)
		}
	}

	// decisive_step -1 means "not applicable" and drops out of the details.
	sc = &scriptClient{bodies: []string{
		`{"failed": false, "error_mode": "none", "decisive_step": -1, "reasoning": "fine"}`,
	}}
	got, err = (FailureAttribution{Client: sc}).Score(context.Background(), tr)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if !got.Passed || got.Details["decisive_step"] != nil {
		t.Errorf("negative step: %+v", got)
	}

	// No verdict degrades to "other" without failing the case.
	sc = &scriptClient{bodies: []string{`{"reasoning": "silent"}`}}
	got, err = (FailureAttribution{Client: sc}).Score(context.Background(), tr)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if !got.Passed || got.Details["error_mode"] != "other" || got.Details["reasoning"] != "attributor returned no verdict" {
		t.Errorf("no verdict: %+v", got)
	}

	// A non-JSON verdict is an error (a recorded fail via ScoreWith).
	sc = &scriptClient{bodies: []string{"looks fine to me"}}
	if _, err := (FailureAttribution{Client: sc}).Score(context.Background(), tr); err == nil {
		t.Error("non-JSON verdict must error")
	}

	// The custom taxonomy replaces the default in the prompt.
	sc = &scriptClient{bodies: []string{`{"failed": false, "error_mode": "none", "decisive_step": -1, "reasoning": "r"}`}}
	if _, err := (FailureAttribution{Client: sc, Taxonomy: []string{"none", "wrong_city"}}).Score(context.Background(), tr); err != nil {
		t.Fatalf("score: %v", err)
	}
	if !strings.Contains(sc.prompts[0], "wrong_city") || strings.Contains(sc.prompts[0], "tool_failure") {
		t.Errorf("custom taxonomy not used: %q", sc.prompts[0])
	}
}

func TestTraceErrRoundTrip(t *testing.T) {
	tr := failedToolTrace("")
	tr.Err = "ask failed: boom"
	b, err := json.Marshal(tr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Trace
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Err != tr.Err {
		t.Errorf("err round trip: %q != %q", back.Err, tr.Err)
	}
	if got := DetectMechanical(&back); got == nil || got.ErrorMode != "crash" {
		t.Errorf("loaded trace must still attribute: %+v", got)
	}
}
