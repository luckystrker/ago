package network

import (
	"testing"
	"time"
)

// warn addresses only the violator; with no participant among the violators it
// falls back to the whole channel, like notify_channel.
func TestWarnHandlerAudience(t *testing.T) {
	r := newTurnRig(t, 3)
	id := r.discussion(nil)
	WarnViolationHandler{}.Handle(r.h, id, Violation{Expectation: turnWithin("warn", 1), ViolatorIDs: []string{r.ids[2]}})
	WarnViolationHandler{}.Handle(r.h, id, Violation{Expectation: turnWithin("warn", 1), ViolatorIDs: []string{"ghost"}})
	WarnViolationHandler{}.Handle(r.h, id, Violation{Expectation: turnWithin("warn", 1)})
	envs := r.violationEnvelopes(id)
	if len(envs) != 3 || envs[0].Audience != r.ids[2] || envs[1].Audience != "" || envs[2].Audience != "" {
		t.Fatalf("audiences %+v", envs)
	}
}

// Unit coverage of the evaluator's guard branches.
func TestTurnWithinEvaluatorGuards(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	exp := turnWithin("warn", 10)
	ev := TurnWithinEvaluator{}
	active := &ChannelHandle{State: ChannelActive, CreatedAt: now.Add(-time.Hour)}
	if ev.Name() != "turn_within" {
		t.Fatalf("name %q", ev.Name())
	}
	if v := ev.Evaluate(exp, ExpectationContext{Channel: active, Now: now}); v != nil {
		t.Fatalf("no expected turn must not fire: %+v", v)
	}
	pending := &ChannelHandle{State: ChannelPending, CreatedAt: now.Add(-time.Hour)}
	turn := &ExpectedTurn{AgentID: "b"}
	if v := ev.Evaluate(exp, ExpectationContext{Channel: pending, Now: now, ExpectedTurn: turn}); v != nil {
		t.Fatalf("non-active channel must not fire: %+v", v)
	}
	// Opening turn without an EnvChannelOpened in the log: the channel's
	// creation time anchors it.
	if v := ev.Evaluate(exp, ExpectationContext{Channel: active, Now: now, ExpectedTurn: turn}); v == nil || v.TurnKey != "" {
		t.Fatalf("opening turn violation %+v", v)
	}
	// A handing-over envelope missing from the log: nothing sound to measure.
	missing := &ExpectedTurn{AgentID: "b", TriggeringEnvelopeID: "env-x"}
	if v := ev.Evaluate(exp, ExpectationContext{Channel: active, Now: now, ExpectedTurn: missing}); v != nil {
		t.Fatalf("missing trigger must not fire: %+v", v)
	}
	wal := []Envelope{{ID: "env-x", CreatedAt: now.Add(-11 * time.Second)}}
	v := ev.Evaluate(exp, ExpectationContext{Channel: active, WAL: wal, Now: now, ExpectedTurn: missing})
	if v == nil || v.TurnKey != "env-x" || len(v.ViolatorIDs) != 1 || v.ViolatorIDs[0] != "b" {
		t.Fatalf("violation %+v", v)
	}
}
