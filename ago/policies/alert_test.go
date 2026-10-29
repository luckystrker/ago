package policies

import (
	"testing"

	agocontext "ago/context"
	"ago/events"
)

func TestAlertPolicyNonFatal(t *testing.T) {
	ctx := testContext(t)
	p := NewAlertPolicy()
	alert := &events.ObserverAlert{
		Source:   "loop-detector",
		Severity: events.SeverityWarning,
		Message:  "repetition detected",
	}
	in := []events.Event{&events.TextInput{Content: "hi"}, alert}

	prompts, evs, err := p.Apply([]string{"sys"}, in, ctx)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(prompts) != 2 || prompts[1] != "alert(warning) from loop-detector: repetition detected" {
		t.Fatalf("prompts = %q, want the alert line appended", prompts)
	}
	if len(evs) != 2 {
		t.Errorf("events = %d, want the input passed through", len(evs))
	}

	// The same alert replayed in the history is delivered once.
	prompts2, _, err := p.Apply(prompts, in, ctx)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if len(prompts2) != len(prompts) {
		t.Errorf("second Apply prompts = %q, want no duplicate injection", prompts2)
	}

	// A different alert still gets through.
	second := &events.ObserverAlert{Source: "token-monitor", Severity: events.SeverityError, Message: "high spend"}
	prompts3, _, err := p.Apply(prompts2, []events.Event{second}, ctx)
	if err != nil {
		t.Fatalf("third Apply: %v", err)
	}
	want := "alert(error) from token-monitor: high spend"
	if len(prompts3) != len(prompts2)+1 || prompts3[len(prompts3)-1] != want {
		t.Errorf("prompts = %q, want %q appended", prompts3, want)
	}
}

func TestAlertPolicyFatal(t *testing.T) {
	ctx := testContext(t)
	p := NewAlertPolicy()
	fatal := &events.ObserverAlert{
		Source:   "token-monitor",
		Severity: events.SeverityFatal,
		Message:  "budget exhausted",
	}

	// A fatal alert sends one HaltEvent and adds no prompt line.
	prompts, _, err := p.Apply(nil, []events.Event{fatal}, ctx)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(prompts) != 0 {
		t.Errorf("prompts = %q, want none for a fatal alert", prompts)
	}
	halts := haltEvents(t, ctx)
	if len(halts) != 1 {
		t.Fatalf("stream has %d HaltEvents, want 1", len(halts))
	}
	if halts[0].Reason != "FATAL: budget exhausted" {
		t.Errorf("halt reason = %q, want %q", halts[0].Reason, "FATAL: budget exhausted")
	}

	// Replaying the same alert does not halt twice.
	if _, _, err := p.Apply(nil, []events.Event{fatal}, ctx); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if halts = haltEvents(t, ctx); len(halts) != 1 {
		t.Errorf("stream has %d HaltEvents after replay, want 1", len(halts))
	}
}

func TestAlertPolicyMixedBatch(t *testing.T) {
	ctx := testContext(t)
	p := NewAlertPolicy()
	in := []events.Event{
		&events.ObserverAlert{Source: "loop-detector", Severity: events.SeverityWarning, Message: "repeat"},
		&events.ObserverAlert{Source: "watchdog", Severity: events.SeverityFatal, Message: "deadline"},
		&events.ObserverAlert{Source: "watchdog", Severity: events.SeverityFatal, Message: "second deadline"},
	}
	prompts, _, err := p.Apply(nil, in, ctx)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(prompts) != 1 || prompts[0] != "alert(warning) from loop-detector: repeat" {
		t.Errorf("prompts = %q, want only the non-fatal line", prompts)
	}
	halts := haltEvents(t, ctx)
	if len(halts) != 1 {
		t.Fatalf("stream has %d HaltEvents, want one for the first fatal alert", len(halts))
	}
	if halts[0].Reason != "FATAL: deadline" {
		t.Errorf("halt reason = %q, want %q", halts[0].Reason, "FATAL: deadline")
	}
}

// Regression: non-fatal alerts standing after the first fatal must still be
// injected. The delivery record marks them delivered in the same Apply, so a
// break on the fatal would lose their content permanently — they would never
// be injected on any later call either.
func TestAlertPolicyNonFatalAfterFatal(t *testing.T) {
	ctx := testContext(t)
	p := NewAlertPolicy()
	in := []events.Event{
		&events.ObserverAlert{Source: "loop-detector", Severity: events.SeverityInfo, Message: "one"},
		&events.ObserverAlert{Source: "watchdog", Severity: events.SeverityFatal, Message: "deadline"},
		&events.ObserverAlert{Source: "watchdog", Severity: events.SeverityInfo, Message: "two"},
	}

	prompts, _, err := p.Apply(nil, in, ctx)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := []string{
		"alert(info) from loop-detector: one",
		"alert(info) from watchdog: two",
	}
	if len(prompts) != len(want) {
		t.Fatalf("prompts = %q, want both non-fatal lines around the fatal", prompts)
	}
	for i, w := range want {
		if prompts[i] != w {
			t.Errorf("prompts[%d] = %q, want %q", i, prompts[i], w)
		}
	}
	halts := haltEvents(t, ctx)
	if len(halts) != 1 || halts[0].Reason != "FATAL: deadline" {
		t.Fatalf("halts = %v, want one halt naming the first fatal", halts)
	}

	// All three alerts were delivered in that single Apply: a replay injects
	// nothing new and does not halt again.
	prompts2, _, err := p.Apply(nil, in, ctx)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if len(prompts2) != 0 {
		t.Errorf("second Apply prompts = %q, want none", prompts2)
	}
	if halts = haltEvents(t, ctx); len(halts) != 1 {
		t.Errorf("stream has %d HaltEvents after replay, want 1", len(halts))
	}
}

func haltEvents(t *testing.T, ctx *agocontext.Context) []*events.HaltEvent {
	t.Helper()
	hist, err := ctx.History()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var out []*events.HaltEvent
	for _, ev := range hist {
		if h, ok := events.AsValue(ev).(events.HaltEvent); ok {
			out = append(out, &h)
		}
	}
	return out
}
