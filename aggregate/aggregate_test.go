package aggregate

import (
	"errors"
	"strings"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
	agotest "github.com/luckystrker/ago/testing"
)

// --- harness -----------------------------------------------------------------

// newTestCtx builds a context over a fresh stream and records every event
// dispatched on it.
func newTestCtx(t *testing.T) (*agocontext.Context, *[]events.Event) {
	t.Helper()
	ms := stream.NewMemoryStream()
	var seen []events.Event
	ms.Subscribe(func(ev events.Event) error {
		seen = append(seen, ev)
		return nil
	}, stream.SubscribeOptions{})
	return agocontext.New(ms), &seen
}

// seedHistory sends the events through the stream so they land in the transcript.
func seedHistory(t *testing.T, ctx *agocontext.Context, evs ...events.Event) {
	t.Helper()
	for _, ev := range evs {
		if err := ctx.Send(ev); err != nil {
			t.Fatalf("seed: Send(%s): %v", ev.EventType(), err)
		}
	}
}

// fixedTurn returns a TurnFunc answering with resp and recording that it ran.
func fixedTurn(resp *events.ModelResponse, ran *bool) middleware.TurnFunc {
	return func(_ *events.ModelRequest, _ *agocontext.Context) (*events.ModelResponse, error) {
		if ran != nil {
			*ran = true
		}
		return resp, nil
	}
}

func cannedResponse(body string) *events.ModelResponse {
	return &events.ModelResponse{Message: &events.ModelMessage{Content: body}}
}

func historyOf(t *testing.T, ctx *agocontext.Context) []events.Event {
	t.Helper()
	evs, err := ctx.Stream.History().GetEvents()
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	return evs
}

// lifecycleEvents filters the dispatched events down to the Aggregation* kinds.
func lifecycleEvents(seen *[]events.Event) []events.Event {
	var out []events.Event
	for _, ev := range *seen {
		switch events.AsValue(ev).(type) {
		case events.AggregationStarted, events.AggregationCompleted, events.AggregationFailed:
			out = append(out, ev)
		}
	}
	return out
}

func textInputs(t *testing.T, evs []events.Event) []string {
	t.Helper()
	var out []string
	for _, ev := range evs {
		if ti, ok := events.AsValue(ev).(events.TextInput); ok {
			out = append(out, ti.Content)
		}
	}
	return out
}

// --- triggers ----------------------------------------------------------------

func TestSummaryCountTrigger(t *testing.T) {
	trig := SummaryCountTrigger(2)
	usage := events.Usage{TotalTokens: events.I64(5000)} // must not matter

	none := []events.Event{&events.TextInput{Content: "a"}}
	if trig.ShouldTrigger(none, usage) {
		t.Error("no summaries: trigger fired, want it to hold")
	}
	one := []events.Event{&events.CompactionSummary{Content: "s1"}, &events.TextInput{Content: "a"}}
	if trig.ShouldTrigger(one, usage) {
		t.Error("one summary: trigger fired, want it to hold")
	}
	two := []events.Event{
		&events.CompactionSummary{Content: "s1"},
		&events.CompactionSummary{Content: "s2"},
	}
	if !trig.ShouldTrigger(two, events.Usage{}) {
		t.Error("two summaries: trigger held, want it to fire")
	}
	three := append(append([]events.Event{}, two...), &events.CompactionSummary{Content: "s3"})
	if !trig.ShouldTrigger(three, usage) {
		t.Error("three summaries (>= n): trigger held, want it to fire")
	}
}

func TestUsageTrigger(t *testing.T) {
	trig := UsageTrigger(1000)

	if trig.ShouldTrigger(nil, events.Usage{}) {
		t.Error("no usage reported: trigger fired, want it to hold")
	}
	if trig.ShouldTrigger(nil, events.Usage{TotalTokens: events.I64(999)}) {
		t.Error("below threshold: trigger fired, want it to hold")
	}
	if !trig.ShouldTrigger(nil, events.Usage{TotalTokens: events.I64(1000)}) {
		t.Error("at threshold: trigger held, want it to fire")
	}
	if !trig.ShouldTrigger(nil, events.Usage{TotalTokens: events.I64(1500)}) {
		t.Error("above threshold: trigger held, want it to fire")
	}
	// History is irrelevant to this trigger; usage alone decides.
	hist := []events.Event{&events.CompactionSummary{Content: "s"}}
	if trig.ShouldTrigger(hist, events.Usage{}) {
		t.Error("summaries without usage: trigger fired, want it to hold")
	}
}

// --- middleware: successful merge --------------------------------------------

func TestMiddlewareMergesTwoSummariesIntoOne(t *testing.T) {
	ctx, seen := newTestCtx(t)
	seedHistory(t, ctx,
		&events.TextInput{Content: "old-1"},
		&events.TextInput{Content: "old-2"},
		&events.CompactionSummary{Content: "summary-A"},
		&events.CompactionSummary{Content: "summary-B"},
		&events.TextInput{Content: "recent-1"},
		&events.TextInput{Content: "recent-2"},
	)

	scripted := cannedResponse("merged-summary")
	scripted.Usage = events.Usage{TotalTokens: events.I64(42), PromptTokens: events.I64(30), CompletionTokens: events.I64(12)}
	cfg := agotest.NewTestConfig(scripted)
	mw := New(SummaryCountTrigger(2), &LLMStrategy{Client: cfg})()

	turnRan := false
	resp, err := mw.OnTurn(fixedTurn(cannedResponse("turn answer"), &turnRan), &events.ModelRequest{}, ctx)
	if err != nil {
		t.Fatalf("OnTurn: %v", err)
	}
	if !turnRan {
		t.Error("turn body never ran")
	}
	if got := resp.Content(); got != "turn answer" {
		t.Errorf("OnTurn response = %q, want the turn's own answer %q", got, "turn answer")
	}

	// History replaced: one new summary, then the keepRecent tail verbatim.
	hist := historyOf(t, ctx)
	if len(hist) != 3 {
		t.Fatalf("history length = %d, want 3", len(hist))
	}
	merged, ok := events.AsValue(hist[0]).(events.CompactionSummary)
	if !ok {
		t.Fatalf("history[0] = %T, want CompactionSummary", hist[0])
	}
	if merged.Content != "merged-summary" {
		t.Errorf("merged summary = %q, want %q", merged.Content, "merged-summary")
	}
	if texts := strings.Join(textInputs(t, hist[1:]), ","); texts != "recent-1,recent-2" {
		t.Errorf("tail = [%s], want [recent-1,recent-2]", texts)
	}

	// The LLM call saw one prompt containing both old summaries and the old
	// events, but not the recent tail.
	calls := cfg.Calls()
	if len(calls) != 1 {
		t.Fatalf("LLM calls = %d, want 1", len(calls))
	}
	if len(calls[0]) != 1 {
		t.Fatalf("call history = %d events, want the single prompt request", len(calls[0]))
	}
	req, ok := events.AsValue(calls[0][0]).(events.ModelRequest)
	if !ok {
		t.Fatalf("call event = %T, want ModelRequest", calls[0][0])
	}
	prompt := ""
	for _, p := range req.Parts {
		if ti, ok := events.AsValue(p).(events.TextInput); ok {
			prompt += ti.Content
		}
	}
	for _, want := range []string{"summary-A", "summary-B", "old-1", "old-2"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if strings.Contains(prompt, "recent-1") {
		t.Error("prompt contains recent tail event recent-1, want it excluded")
	}

	// Lifecycle bracket around the replacement; the strategy's usage is reported
	// as kind="aggregation" and echoed in AggregationCompleted.
	lc := lifecycleEvents(seen)
	if len(lc) != 2 {
		t.Fatalf("lifecycle events = %d (%v), want Started then Completed", len(lc), lc)
	}
	if lc[0].EventType() != "AggregationStarted" || lc[1].EventType() != "AggregationCompleted" {
		t.Errorf("lifecycle order = %s, %s; want AggregationStarted, AggregationCompleted",
			lc[0].EventType(), lc[1].EventType())
	}
	completed, ok := events.AsValue(lc[1]).(events.AggregationCompleted)
	if !ok {
		t.Fatalf("second lifecycle event = %T, want AggregationCompleted", lc[1])
	}
	if completed.EventsBefore != 6 || completed.EventsAfter != 3 {
		t.Errorf("completed = before %d / after %d, want 6 / 3", completed.EventsBefore, completed.EventsAfter)
	}
	if completed.LLMCalls != 1 {
		t.Errorf("completed.LLMCalls = %d, want 1", completed.LLMCalls)
	}
	if completed.Usage.TotalTokens == nil || *completed.Usage.TotalTokens != 42 {
		t.Errorf("completed.Usage.TotalTokens = %v, want 42", completed.Usage.TotalTokens)
	}

	// The aggregation call's tokens are on the stream as UsageEvent(kind=aggregation),
	// so usage reports attribute them.
	var aggUsage *events.UsageEvent
	for _, ev := range *seen {
		if ue, ok := events.AsValue(ev).(events.UsageEvent); ok && ue.Kind == events.UsageKindAggregation {
			aggUsage = &ue
		}
	}
	if aggUsage == nil {
		t.Fatal("no UsageEvent(kind=aggregation) sent, want one")
	}
	if aggUsage.Model != "test" && aggUsage.Model != "" {
		t.Errorf("usage model = %q, want the scripted response's model", aggUsage.Model)
	}
}

func TestMiddlewareTriggerNotFiredLeavesHistoryAlone(t *testing.T) {
	ctx, seen := newTestCtx(t)
	seedHistory(t, ctx,
		&events.TextInput{Content: "a"},
		&events.CompactionSummary{Content: "s1"},
		&events.TextInput{Content: "b"},
	)

	cfg := agotest.NewTestConfig(cannedResponse("unused"))
	mw := New(SummaryCountTrigger(5), &LLMStrategy{Client: cfg})()

	before := historyOf(t, ctx)
	if _, err := mw.OnTurn(fixedTurn(cannedResponse("ok"), nil), &events.ModelRequest{}, ctx); err != nil {
		t.Fatalf("OnTurn: %v", err)
	}

	if after := historyOf(t, ctx); len(after) != len(before) {
		t.Errorf("history length = %d, want unchanged (%d)", len(after), len(before))
	}
	if calls := cfg.Calls(); len(calls) != 0 {
		t.Errorf("LLM calls = %d, want 0 (trigger did not fire)", len(calls))
	}
	if lc := lifecycleEvents(seen); len(lc) != 0 {
		t.Errorf("lifecycle events = %v, want none", lc)
	}
}

func TestMiddlewareNothingToAggregateSkipsSilently(t *testing.T) {
	ctx, seen := newTestCtx(t)
	seedHistory(t, ctx,
		&events.TextInput{Content: "a"},
		&events.TextInput{Content: "b"},
	)

	// SummaryCountTrigger(0) always fires; with no summaries and no old
	// conversational events there is nothing to fold.
	cfg := agotest.NewTestConfig(cannedResponse("unused"))
	mw := New(SummaryCountTrigger(0), &LLMStrategy{Client: cfg})()

	before := historyOf(t, ctx)
	if _, err := mw.OnTurn(fixedTurn(cannedResponse("ok"), nil), &events.ModelRequest{}, ctx); err != nil {
		t.Fatalf("OnTurn: %v", err)
	}

	if after := historyOf(t, ctx); len(after) != len(before) {
		t.Errorf("history length = %d, want unchanged (%d)", len(after), len(before))
	}
	if calls := cfg.Calls(); len(calls) != 0 {
		t.Errorf("LLM calls = %d, want 0 (nothing to aggregate)", len(calls))
	}
	if lc := lifecycleEvents(seen); len(lc) != 0 {
		t.Errorf("lifecycle events = %v, want none", lc)
	}
}

func TestMiddlewareKeepsUsageBookkeepingEvents(t *testing.T) {
	ctx, _ := newTestCtx(t)
	seedHistory(t, ctx,
		&events.UsageEvent{Kind: events.UsageKindModelCall, Usage: events.Usage{TotalTokens: events.I64(10)}},
		&events.TextInput{Content: "old-1"},
		&events.CompactionSummary{Content: "summary-A"},
		&events.TextInput{Content: "recent-1"},
		&events.TextInput{Content: "recent-2"},
	)

	cfg := agotest.NewTestConfig(cannedResponse("merged"))
	mw := New(UsageTrigger(5), &LLMStrategy{Client: cfg})()
	if _, err := mw.OnTurn(fixedTurn(cannedResponse("ok"), nil), &events.ModelRequest{}, ctx); err != nil {
		t.Fatalf("OnTurn: %v", err)
	}

	hist := historyOf(t, ctx)
	// UsageEvent preserved + merged summary + tail of two.
	if len(hist) != 4 {
		t.Fatalf("history length = %d, want 4", len(hist))
	}
	if _, ok := events.AsValue(hist[0]).(events.UsageEvent); !ok {
		t.Errorf("history[0] = %T, want the preserved UsageEvent", hist[0])
	}
	if _, ok := events.AsValue(hist[1]).(events.CompactionSummary); !ok {
		t.Errorf("history[1] = %T, want the merged CompactionSummary", hist[1])
	}
}

// --- middleware: strategy failure --------------------------------------------

type failingStrategy struct{ err error }

func (f failingStrategy) Aggregate(_ *agocontext.Context, _ []string, _ []events.Event) (string, error) {
	return "", f.err
}

func TestMiddlewareStrategyFailureKeepsHistory(t *testing.T) {
	ctx, seen := newTestCtx(t)
	seedHistory(t, ctx,
		&events.TextInput{Content: "old-1"},
		&events.CompactionSummary{Content: "summary-A"},
		&events.CompactionSummary{Content: "summary-B"},
		&events.TextInput{Content: "recent-1"},
		&events.TextInput{Content: "recent-2"},
	)

	stratErr := errors.New("boom: summarizer down")
	mw := New(SummaryCountTrigger(2), failingStrategy{err: stratErr})()

	turnRan := false
	resp, err := mw.OnTurn(fixedTurn(cannedResponse("turn answer"), &turnRan), &events.ModelRequest{}, ctx)
	if err != nil {
		t.Fatalf("OnTurn error = %v, want nil (aggregation failure must not fail the turn)", err)
	}
	if !turnRan || resp.Content() != "turn answer" {
		t.Errorf("turn result = ran:%v resp:%q, want the turn to complete normally", turnRan, resp.Content())
	}

	// History untouched: both summaries and every event still in place.
	hist := historyOf(t, ctx)
	if len(hist) != 5 {
		t.Fatalf("history length = %d, want unchanged (5)", len(hist))
	}
	summaries := 0
	for _, ev := range hist {
		if _, ok := events.AsValue(ev).(events.CompactionSummary); ok {
			summaries++
		}
	}
	if summaries != 2 {
		t.Errorf("summaries in history = %d, want both original ones (2)", summaries)
	}

	// Failure reported as AggregationStarted + AggregationFailed.
	lc := lifecycleEvents(seen)
	if len(lc) != 2 {
		t.Fatalf("lifecycle events = %d (%v), want Started then Failed", len(lc), lc)
	}
	if lc[0].EventType() != "AggregationStarted" || lc[1].EventType() != "AggregationFailed" {
		t.Errorf("lifecycle order = %s, %s; want AggregationStarted, AggregationFailed",
			lc[0].EventType(), lc[1].EventType())
	}
	failed, ok := events.AsValue(lc[1]).(events.AggregationFailed)
	if !ok {
		t.Fatalf("second lifecycle event = %T, want AggregationFailed", lc[1])
	}
	if !strings.Contains(failed.Error, "boom") {
		t.Errorf("failed.Error = %q, want the strategy error text", failed.Error)
	}
	if failed.ErrorType == "" {
		t.Error("failed.ErrorType empty, want a classification")
	}
}

// --- strategy ----------------------------------------------------------------

func TestLLMStrategyWithoutClient(t *testing.T) {
	s := &LLMStrategy{}
	if _, err := s.Aggregate(agocontext.New(stream.NewMemoryStream()), nil, nil); !errors.Is(err, ErrNoClient) {
		t.Errorf("err = %v, want ErrNoClient", err)
	}
}

func TestLLMStrategyMergesInputs(t *testing.T) {
	cfg := agotest.NewTestConfig(cannedResponse("the merge"))
	s := &LLMStrategy{Client: cfg}

	out, err := s.Aggregate(
		agocontext.New(stream.NewMemoryStream()),
		[]string{"summary-A", "summary-B"},
		[]events.Event{&events.TextInput{Content: "old-1"}},
	)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if out != "the merge" {
		t.Errorf("Aggregate = %q, want the scripted response content", out)
	}
	calls := cfg.Calls()
	if len(calls) != 1 {
		t.Fatalf("LLM calls = %d, want 1", len(calls))
	}
	req, ok := events.AsValue(calls[0][0]).(events.ModelRequest)
	if !ok {
		t.Fatalf("call event = %T, want ModelRequest", calls[0][0])
	}
	if len(req.Parts) != 1 {
		t.Fatalf("prompt parts = %d, want 1", len(req.Parts))
	}
	ti, ok := events.AsValue(req.Parts[0]).(events.TextInput)
	if !ok {
		t.Fatalf("part = %T, want TextInput", req.Parts[0])
	}
	for _, want := range []string{"summary-A", "summary-B", "old-1"} {
		if !strings.Contains(ti.Content, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestLLMStrategyClientErrorPropagates(t *testing.T) {
	cfg := agotest.NewTestConfig() // empty script: every call fails
	s := &LLMStrategy{Client: cfg}
	_, err := s.Aggregate(agocontext.New(stream.NewMemoryStream()), []string{"s"}, nil)
	if err == nil {
		t.Fatal("Aggregate succeeded, want the scripted-exhaustion error")
	}
	if !errors.Is(err, agotest.ErrScriptedExhausted) {
		t.Errorf("err = %v, want it to wrap agotest.ErrScriptedExhausted", err)
	}
}
