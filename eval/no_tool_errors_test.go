package eval

import (
	"context"
	"testing"

	"github.com/luckystrker/ago/events"
)

func TestNoToolErrors(t *testing.T) {
	okCall := events.ToolCallEvent{ID: "c1", Name: "get_weather", Arguments: "{}"}
	okResult := &events.ToolResultEvent{ParentID: "c1", Name: "get_weather"}
	errResult := &events.ToolErrorEvent{ToolResultEvent: events.ToolResultEvent{ParentID: "c2", Name: "boom"}, Error: "exploded"}
	missing := events.NewToolCallEvent("no_such_tool")
	notFound := &events.ToolNotFoundEvent{ToolErrorEvent: events.FromErrorCall(missing, context.DeadlineExceeded)}

	tests := []struct {
		name      string
		evs       []events.Event
		wantPass  bool
		wantTool  string
		wantIndex int
	}{
		{name: "no events", evs: nil, wantPass: true},
		{name: "tool calls all succeeded", evs: []events.Event{&okCall, okResult}, wantPass: true},
		{name: "tool error", evs: []events.Event{&okCall, okResult, errResult}, wantTool: "boom", wantIndex: 2},
		{name: "tool not found counts", evs: []events.Event{&missing, notFound}, wantTool: "no_such_tool", wantIndex: 1},
		{name: "first error reported", evs: []events.Event{errResult, notFound}, wantTool: "boom", wantIndex: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NoToolErrors{}.Score(context.Background(), BuildTrace(tc.evs))
			if err != nil {
				t.Fatalf("score: %v", err)
			}
			if got.Name != "no_tool_errors" {
				t.Errorf("name = %q", got.Name)
			}
			if got.Passed != tc.wantPass {
				t.Fatalf("passed = %v, want %v (details %v)", got.Passed, tc.wantPass, got.Details)
			}
			wantValue := 0.0
			if tc.wantPass {
				wantValue = 1.0
			}
			if got.Value != wantValue {
				t.Errorf("value = %v, want %v", got.Value, wantValue)
			}
			if tc.wantPass {
				if len(got.Details) != 0 {
					t.Errorf("passing details = %v, want empty", got.Details)
				}
				return
			}
			if got.Details["tool"] != tc.wantTool || got.Details["event_index"] != tc.wantIndex {
				t.Errorf("details = %v, want tool %q index %d", got.Details, tc.wantTool, tc.wantIndex)
			}
		})
	}
}

// TestNoToolErrorsOnRealRun: the scorer reads a trace Run built from an agent
// whose tool call hits an unregistered name.
func TestNoToolErrorsOnRealRun(t *testing.T) {
	a := scriptedAgent(t,
		toolResp("get_weather", `{"city":"Paris"}`),
		textResp("sunny"),
		toolResp("missing_tool", `{}`),
		textResp("gave up"),
	)
	res, err := Run(context.Background(), a, Suite{
		{Name: "clean", Input: "weather?", Scorers: []Scorer{NoToolErrors{}}},
		{Name: "broken", Input: "other?", Scorers: []Scorer{NoToolErrors{}}},
	}, WithConcurrency(1))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Cases[0].Passed {
		t.Errorf("clean case failed: %+v", res.Cases[0].Scores)
	}
	if res.Cases[1].Passed {
		t.Errorf("tool-not-found case passed: %+v", res.Cases[1].Scores)
	}
}
