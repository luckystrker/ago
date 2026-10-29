package network

import (
	"encoding/json"
	"testing"

	"ago/events"
)

func TestHandoffFromTypedResult(t *testing.T) {
	res := events.NewToolResult(false, &Handoff{Target: "bob", Reason: "load"})
	h := HandoffFromResult(res)
	if h == nil || h.Target != "bob" || h.Reason != "load" {
		t.Fatalf("typed handoff: %+v", h)
	}
	// value form works too
	res2 := events.NewToolResult(false, Handoff{Target: "carol"})
	if h2 := HandoffFromResult(res2); h2 == nil || h2.Target != "carol" {
		t.Fatalf("value handoff: %+v", h2)
	}
	if h3 := HandoffFromResult(events.NewToolResult(false, "plain")); h3 != nil {
		t.Fatalf("plain text must not match: %+v", h3)
	}
}

func TestHandoffFromJSONResult(t *testing.T) {
	data, _ := json.Marshal(map[string]any{"handoff": map[string]any{"target": "bob", "reason": "r"}})
	res := events.NewToolResult(false, string(data))
	if h := HandoffFromResult(res); h == nil || h.Target != "bob" {
		t.Fatalf("json handoff: %+v", h)
	}
	// a plain string with no marker decodes to nothing
	if h := HandoffFromResult(events.NewToolResult(false, `{"answer": 2}`)); h != nil {
		t.Fatalf("unrelated json must not match: %+v", h)
	}
}

func TestFinishFromResult(t *testing.T) {
	res := events.NewToolResult(false, &Finish{Summary: "all done", Reason: "wrapped"})
	f := FinishFromResult(res)
	if f == nil || f.Summary != "all done" || f.Reason != "wrapped" {
		t.Fatalf("typed finish: %+v", f)
	}
	if FinishFromResult(events.NewToolResult(false, &Handoff{Target: "x"})) != nil {
		t.Fatal("handoff must not match as finish")
	}
}

func TestRoundFromEvents(t *testing.T) {
	call := events.NewToolCallEvent("finish_now")
	call.ID = "call-1"
	finishRes := events.FromResultCall(call, events.NewToolResult(false, &Finish{Reason: "done"}))
	handoffCall := events.NewToolCallEvent("route")
	handoffCall.ID = "call-2"
	handoffRes := events.FromResultCall(handoffCall, events.NewToolResult(false, &Handoff{Target: "bob"}))

	evs := []events.Event{
		events.ToolCallsEvent{Calls: []events.ToolCallEvent{call, handoffCall}},
		finishRes,
		handoffRes,
	}
	round := RoundFromEvents("the text", evs)
	if round.Text != "the text" {
		t.Errorf("text: %q", round.Text)
	}
	// Finish wins over Handoff (termination is the most explicit intent).
	if round.Finish == nil || round.Finish.Reason != "done" {
		t.Fatalf("finish: %+v", round.Finish)
	}
	if round.Handoff == nil || round.Handoff.Target != "bob" {
		t.Fatalf("handoff still captured: %+v", round.Handoff)
	}

	// No intents → plain text round.
	plain := RoundFromEvents("t", []events.Event{events.FromResultCall(call, events.NewToolResult(false, "ok"))})
	if plain.Finish != nil || plain.Handoff != nil {
		t.Fatalf("plain round: %+v", plain)
	}
}
