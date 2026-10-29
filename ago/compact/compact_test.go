package compact

import (
	"errors"
	"strings"
	"testing"

	agocontext "ago/context"
	"ago/events"
	"ago/stream"
	agotest "ago/testing"
)

// strategyFunc is a closure standing in for an LLM summarizer.
type strategyFunc func(ctx *agocontext.Context, evs []events.Event) (string, error)

func (f strategyFunc) Summarize(ctx *agocontext.Context, evs []events.Event) (string, error) {
	return f(ctx, evs)
}

// compactBoom is a named error so ErrorType assertions are stable.
type compactBoom struct{}

func (compactBoom) Error() string { return "compact boom" }

// collector records the type of every event that passes the stream, plus custom
// markers appended by tests (e.g. "summarize" from inside a strategy closure).
type collector struct {
	seen *[]string
}

func (c collector) record(ev events.Event) error {
	*c.seen = append(*c.seen, ev.EventType())
	return nil
}

func newTestCtx(t *testing.T) (*agocontext.Context, *[]string) {
	t.Helper()
	s := stream.NewMemoryStream()
	log := []string{}
	s.Subscribe(collector{seen: &log}.record, stream.SubscribeOptions{})
	return agocontext.New(s), &log
}

// send persists one event on the conversation stream.
func send(t *testing.T, ctx *agocontext.Context, ev events.Event) {
	t.Helper()
	if err := ctx.Send(ev); err != nil {
		t.Fatalf("send %s: %v", ev.EventType(), err)
	}
}

// userReq builds a conversational ModelRequest carrying one text part.
func userReq(content string) *events.ModelRequest {
	return &events.ModelRequest{Parts: []events.Input{events.TextInput{Content: content}}}
}

// fingerprint renders a history snapshot for before/after comparison.
func fingerprint(evs []events.Event) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = ev.EventType() + ":" + events.RenderForPrompt(ev)
	}
	return out
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestEventCountTrigger(t *testing.T) {
	conv := func(n int) []events.Event {
		evs := make([]events.Event, n)
		for i := range evs {
			evs[i] = userReq("u")
		}
		return evs
	}
	telemetry := []events.Event{&events.UsageEvent{}, &events.UsageEvent{}}

	tests := []struct {
		name string
		n    int
		evs  []events.Event
		want bool
	}{
		{"empty history", 2, nil, false},
		{"at threshold does not fire", 2, conv(2), false},
		{"above threshold fires", 2, conv(3), true},
		{"telemetry never counts", 2, append(conv(2), telemetry...), false},
		{"zero disables", 0, conv(10), false},
		{"negative disables", -1, conv(10), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EventCountTrigger(tt.n).ShouldTrigger(tt.evs, events.Usage{}); got != tt.want {
				t.Fatalf("EventCountTrigger(%d) = %v, want %v", tt.n, got, tt.want)
			}
		})
	}
}

func TestTokenTrigger(t *testing.T) {
	big := userReq(strings.Repeat("x", 400)) // 400 chars / 4 = 100 est. tokens
	small := userReq("hi")                   // clamps to 1 est. token
	telemetry := &events.UsageEvent{}        // not conversational, never estimated

	tests := []struct {
		name   string
		budget int
		evs    []events.Event
		want   bool
	}{
		{"below budget", 50, []events.Event{small}, false},
		{"over budget fires", 50, []events.Event{big, small}, true},
		{"telemetry not estimated", 50, []events.Event{small, telemetry}, false},
		{"budget boundary excluded", 101, []events.Event{big, small}, false},
		{"zero disables", 0, []events.Event{big}, false},
		{"negative disables", -5, []events.Event{big}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TokenTrigger(tt.budget).ShouldTrigger(tt.evs, events.Usage{}); got != tt.want {
				t.Fatalf("TokenTrigger(%d) = %v, want %v", tt.budget, got, tt.want)
			}
		})
	}
}

func TestLLMStrategySummarize(t *testing.T) {
	ctx, _ := newTestCtx(t)
	evs := []events.Event{
		userReq("hello"),
		&events.ModelResponse{Message: &events.ModelMessage{Content: "hi there"}},
	}

	t.Run("one call, prompt + rendered history, returns summary text", func(t *testing.T) {
		cfg := agotest.NewTestConfig(&events.ModelResponse{Message: &events.ModelMessage{Content: "the summary"}})
		out, err := LLMStrategy{Client: cfg, Prompt: "PROMPT"}.Summarize(ctx, evs)
		if err != nil {
			t.Fatalf("Summarize: %v", err)
		}
		if out != "the summary" {
			t.Fatalf("summary = %q", out)
		}
		calls := cfg.Calls()
		if len(calls) != 1 {
			t.Fatalf("calls = %d, want 1", len(calls))
		}
		req, ok := calls[0][0].(*events.ModelRequest)
		if !ok {
			t.Fatalf("call saw %T, want *events.ModelRequest", calls[0][0])
		}
		if len(req.Parts) != 1 {
			t.Fatalf("parts = %d, want 1", len(req.Parts))
		}
		// Parts survive the recorder's JSON round trip as pointer inputs; render
		// instead of asserting a concrete part type.
		text := events.RenderForPrompt(req.Parts[0])
		want := "PROMPT\n\nUser: hello\nAssistant: hi there"
		if text != want {
			t.Fatalf("prompt text = %q, want %q", text, want)
		}
	})

	t.Run("empty prompt falls back to DefaultPrompt", func(t *testing.T) {
		cfg := agotest.NewTestConfig(&events.ModelResponse{Message: &events.ModelMessage{Content: "s"}})
		s := LLMStrategy{Client: cfg}
		if _, err := s.Summarize(ctx, evs[:1]); err != nil {
			t.Fatalf("Summarize: %v", err)
		}
		text := events.RenderForPrompt(cfg.Calls()[0][0].(*events.ModelRequest).Parts[0])
		if !strings.HasPrefix(text, DefaultPrompt) {
			t.Fatalf("prompt text = %q, want DefaultPrompt prefix", text)
		}
	})

	t.Run("nil client is an error", func(t *testing.T) {
		s := LLMStrategy{}
		if _, err := s.Summarize(ctx, evs); err == nil {
			t.Fatal("want error for nil client")
		}
	})

	t.Run("client failure is wrapped", func(t *testing.T) {
		cfg := agotest.NewTestConfig() // no scripted responses → ErrScriptedExhausted
		_, err := LLMStrategy{Client: cfg}.Summarize(ctx, evs)
		if !errors.Is(err, agotest.ErrScriptedExhausted) {
			t.Fatalf("err = %v, want ErrScriptedExhausted", err)
		}
	})
}

// seedTurnHistory writes 7 conversational events with two interleaved telemetry
// records: ue1 inside the would-be old span, ue2 inside the fresh range.
func seedTurnHistory(t *testing.T, ctx *agocontext.Context) {
	t.Helper()
	for _, u := range []string{"u0", "u1", "u2", "u3", "u4"} {
		send(t, ctx, userReq(u))
	}
	send(t, ctx, &events.UsageEvent{Kind: events.UsageKindModelCall}) // ue1
	send(t, ctx, userReq("u5"))
	send(t, ctx, userReq("u6"))
	send(t, ctx, &events.UsageEvent{Kind: events.UsageKindModelCall}) // ue2
}

func TestCompactionReplacesHistory(t *testing.T) {
	ctx, log := newTestCtx(t)
	seedTurnHistory(t, ctx)

	var summarized []string
	strategy := strategyFunc(func(ctx *agocontext.Context, evs []events.Event) (string, error) {
		*log = append(*log, "summarize")
		for _, ev := range evs {
			summarized = append(summarized, events.RenderForPrompt(ev))
		}
		return "old stuff summarized", nil
	})

	mw := New(EventCountTrigger(6), strategy, WithAgent("tester"))()
	var reply *events.ModelResponse
	next := func(ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
		reply = &events.ModelResponse{
			Message: &events.ModelMessage{Content: "done"},
			Usage:   events.Usage{TotalTokens: events.I64(42)},
		}
		send(t, ctx, reply)
		return reply, nil
	}
	got, err := mw.OnTurn(next, userReq("u7"), ctx)
	if err != nil {
		t.Fatalf("OnTurn: %v", err)
	}
	if got != reply || got.Content() != "done" {
		t.Fatalf("turn result = %v, want the model response", got)
	}

	// History is now the summary plus the fresh range: the last two
	// conversational events (u6 and the turn's response) and the telemetry
	// interleaved between them (ue2); ue1 went with the old span.
	evs, err := ctx.Stream.History().GetEvents()
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	want := []string{
		"CompactionSummary:[Summary of earlier conversation]\nold stuff summarized",
		"ModelRequest:User: u6",
		"UsageEvent:",
		"ModelResponse:Assistant: done",
	}
	if !equalStrs(fingerprint(evs), want) {
		t.Fatalf("history = %v, want %v", fingerprint(evs), want)
	}

	// The strategy saw exactly the old conversational span — telemetry (ue1)
	// excluded, the fresh range (u6, the response) excluded.
	if !equalStrs(summarized, []string{"User: u0", "User: u1", "User: u2", "User: u3", "User: u4", "User: u5"}) {
		t.Fatalf("summarized = %v", summarized)
	}

	// Lifecycle bracket: Started before the strategy, Completed after (the
	// collector's earlier entries are the seeded turn events).
	wantLog := []string{"CompactionStarted", "summarize", "CompactionCompleted"}
	tail := (*log)[len(*log)-3:]
	if !equalStrs(tail, wantLog) {
		t.Fatalf("event order tail = %v, want %v", tail, wantLog)
	}
}

func TestCompactionLifecycleFields(t *testing.T) {
	ctx, _ := newTestCtx(t)
	seedTurnHistory(t, ctx)

	var started *events.CompactionStarted
	var completed *events.CompactionCompleted
	sub := func(ev events.Event) error {
		switch e := events.AsValue(ev).(type) {
		case events.CompactionStarted:
			started = &e
		case events.CompactionCompleted:
			completed = &e
		}
		return nil
	}
	ctx.Stream.Subscribe(sub, stream.SubscribeOptions{})

	mw := New(EventCountTrigger(6), strategyFunc(func(*agocontext.Context, []events.Event) (string, error) {
		return "s", nil
	}), WithAgent("tester"))()

	if _, err := mw.OnTurn(func(*events.ModelRequest, *agocontext.Context) (*events.ModelResponse, error) {
		r := &events.ModelResponse{Message: &events.ModelMessage{Content: "done"}}
		send(t, ctx, r)
		return r, nil
	}, userReq("u7"), ctx); err != nil {
		t.Fatalf("OnTurn: %v", err)
	}

	if started == nil || completed == nil {
		t.Fatalf("lifecycle events missing: started=%v completed=%v", started, completed)
	}
	if started.Agent != "tester" || started.Strategy != "strategyFunc" {
		t.Fatalf("started = %+v", started)
	}
	// The snapshot is taken after the turn: 7 conversational seed events plus
	// the turn's response, plus 2 telemetry records = 10; 4 events after.
	if completed.EventsBefore != 10 || completed.EventsAfter != 4 {
		t.Fatalf("completed counts = before %d after %d, want 10 and 4", completed.EventsBefore, completed.EventsAfter)
	}
	if completed.Agent != "tester" || completed.Strategy != "strategyFunc" {
		t.Fatalf("completed = %+v", completed)
	}
}

func TestCompactionFailureKeepsHistory(t *testing.T) {
	ctx, log := newTestCtx(t)
	seedTurnHistory(t, ctx)
	before, err := ctx.Stream.History().GetEvents()
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}

	strategy := strategyFunc(func(*agocontext.Context, []events.Event) (string, error) {
		return "", compactBoom{}
	})
	mw := New(EventCountTrigger(3), strategy)()

	var failures []*events.CompactionFailed
	ctx.Stream.Subscribe(func(ev events.Event) error {
		if e, ok := events.AsValue(ev).(events.CompactionFailed); ok {
			f := e
			failures = append(failures, &f)
		}
		return nil
	}, stream.SubscribeOptions{})

	got, turnErr := mw.OnTurn(func(ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
		r := &events.ModelResponse{Message: &events.ModelMessage{Content: "done"}}
		send(t, ctx, r)
		return r, nil
	}, userReq("u7"), ctx)

	// The turn survives; only compaction failed.
	if turnErr != nil {
		t.Fatalf("OnTurn error = %v, want nil", turnErr)
	}
	if got == nil || got.Content() != "done" {
		t.Fatalf("turn result = %v, want the model response", got)
	}

	if len(failures) != 1 {
		t.Fatalf("CompactionFailed count = %d, want 1", len(failures))
	}
	f := failures[0]
	if f.Error != "compact boom" || f.ErrorType != "compactBoom" || f.Strategy != "strategyFunc" {
		t.Fatalf("failed = %+v", f)
	}
	for _, kind := range *log {
		if kind == "CompactionCompleted" {
			t.Fatal("CompactionCompleted must not fire on failure")
		}
	}

	// History is untouched except for the turn's own response.
	after, err := ctx.Stream.History().GetEvents()
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	wantBefore := fingerprint(before)
	wantBefore = append(wantBefore, "ModelResponse:Assistant: done")
	if !equalStrs(fingerprint(after), wantBefore) {
		t.Fatalf("history after failure = %v, want %v", fingerprint(after), wantBefore)
	}
}

func TestCompactionCarriesStrategySpend(t *testing.T) {
	ctx, _ := newTestCtx(t)
	seedTurnHistory(t, ctx)

	// A summarizer that reports its own spend (parity: LLMStrategy's usage send).
	strategy := strategyFunc(func(ctx *agocontext.Context, evs []events.Event) (string, error) {
		if err := ctx.Send(&events.UsageEvent{
			Usage:    events.Usage{TotalTokens: events.I64(525)},
			Kind:     events.UsageKindCompaction,
			Model:    "stub-summarizer",
			Provider: "stub",
		}); err != nil {
			return "", err
		}
		return "s", nil
	})
	mw := New(EventCountTrigger(6), strategy)()

	var completed *events.CompactionCompleted
	ctx.Stream.Subscribe(func(ev events.Event) error {
		if e, ok := events.AsValue(ev).(events.CompactionCompleted); ok {
			c := e
			completed = &c
		}
		return nil
	}, stream.SubscribeOptions{})

	if _, err := mw.OnTurn(func(ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
		r := &events.ModelResponse{Message: &events.ModelMessage{Content: "done"}}
		send(t, ctx, r)
		return r, nil
	}, userReq("u7"), ctx); err != nil {
		t.Fatalf("OnTurn: %v", err)
	}

	if completed == nil || completed.LLMCalls != 1 {
		t.Fatalf("completed = %+v, want LLMCalls 1", completed)
	}
	if completed.Usage.TotalTokens == nil || *completed.Usage.TotalTokens != 525 {
		t.Fatalf("completed usage = %+v, want total 525", completed.Usage)
	}

	// The summarization record rides through the history rewrite.
	evs, err := ctx.Stream.History().GetEvents()
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	last := evs[len(evs)-1]
	ue, ok := last.(*events.UsageEvent)
	if !ok {
		t.Fatalf("last history event = %T, want *events.UsageEvent", last)
	}
	if ue.Kind != events.UsageKindCompaction || ue.Usage.TotalTokens == nil || *ue.Usage.TotalTokens != 525 {
		t.Fatalf("carried usage = %+v", ue)
	}
}

func TestLLMStrategySendsCompactionUsage(t *testing.T) {
	ctx, _ := newTestCtx(t)
	var spent []*events.UsageEvent
	ctx.Stream.Subscribe(func(ev events.Event) error {
		if u, ok := events.AsValue(ev).(events.UsageEvent); ok {
			ue := u
			spent = append(spent, &ue)
		}
		return nil
	}, stream.SubscribeOptions{})

	cfg := agotest.NewTestConfig(&events.ModelResponse{
		Message:  &events.ModelMessage{Content: "s"},
		Usage:    events.Usage{PromptTokens: events.I64(500), CompletionTokens: events.I64(25), TotalTokens: events.I64(525)},
		Model:    "stub",
		Provider: "stub",
	})
	if _, err := (LLMStrategy{Client: cfg}).Summarize(ctx, []events.Event{userReq("hi")}); err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(spent) != 1 {
		t.Fatalf("usage events = %d, want 1", len(spent))
	}
	if spent[0].Kind != events.UsageKindCompaction || spent[0].Model != "stub" {
		t.Fatalf("usage event = %+v", spent[0])
	}
	if spent[0].Usage.TotalTokens == nil || *spent[0].Usage.TotalTokens != 525 {
		t.Fatalf("usage totals = %+v", spent[0].Usage)
	}
}

func TestCompactionSkipsBelowThreshold(t *testing.T) {
	ctx, log := newTestCtx(t)
	send(t, ctx, userReq("only"))

	called := false
	mw := New(EventCountTrigger(100), strategyFunc(func(*agocontext.Context, []events.Event) (string, error) {
		called = true
		return "s", nil
	}))()

	before, _ := ctx.Stream.History().GetEvents()
	if _, err := mw.OnTurn(func(ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
		return &events.ModelResponse{Message: &events.ModelMessage{Content: "ok"}}, nil
	}, userReq("x"), ctx); err != nil {
		t.Fatalf("OnTurn: %v", err)
	}

	if called {
		t.Fatal("strategy must not run below threshold")
	}
	for _, kind := range *log {
		if strings.HasPrefix(kind, "Compaction") {
			t.Fatalf("lifecycle event %s fired below threshold", kind)
		}
	}
	after, _ := ctx.Stream.History().GetEvents()
	if len(after) != len(before) {
		t.Fatalf("history changed: %d → %d events", len(before), len(after))
	}
}

func TestCompactionSkipsWhenNothingOld(t *testing.T) {
	ctx, _ := newTestCtx(t)
	send(t, ctx, userReq("u0"))
	send(t, ctx, userReq("u1"))

	called := false
	always := triggerFunc(func([]events.Event, events.Usage) bool { return true })
	mw := New(always, strategyFunc(func(*agocontext.Context, []events.Event) (string, error) {
		called = true
		return "s", nil
	}))()

	if _, err := mw.OnTurn(func(ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
		return &events.ModelResponse{}, nil
	}, userReq("x"), ctx); err != nil {
		t.Fatalf("OnTurn: %v", err)
	}
	if called {
		t.Fatal("strategy must not run when the old span is empty")
	}
}

// triggerFunc adapts a closure to Trigger.
type triggerFunc func(evs []events.Event, u events.Usage) bool

func (f triggerFunc) ShouldTrigger(evs []events.Event, u events.Usage) bool { return f(evs, u) }

func TestTriggerReceivesHistoryUsage(t *testing.T) {
	ctx, _ := newTestCtx(t)
	send(t, ctx, userReq("u0"))
	send(t, ctx, &events.ModelResponse{
		Message: &events.ModelMessage{Content: "a"},
		Usage:   events.Usage{TotalTokens: events.I64(7)},
	})
	send(t, ctx, &events.ModelResponse{
		Message: &events.ModelMessage{Content: "b"},
		Usage:   events.Usage{TotalTokens: events.I64(35)},
	})

	var sawEvs int
	var sawTotal int64
	trigger := triggerFunc(func(evs []events.Event, u events.Usage) bool {
		sawEvs = len(evs)
		if u.TotalTokens != nil {
			sawTotal = *u.TotalTokens
		}
		return false
	})
	mw := New(trigger, strategyFunc(func(*agocontext.Context, []events.Event) (string, error) {
		return "s", nil
	}))()
	if _, err := mw.OnTurn(func(ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
		return &events.ModelResponse{}, nil
	}, userReq("x"), ctx); err != nil {
		t.Fatalf("OnTurn: %v", err)
	}
	if sawEvs != 3 {
		t.Fatalf("trigger saw %d events, want 3", sawEvs)
	}
	if sawTotal != 42 {
		t.Fatalf("trigger saw total tokens %d, want 42", sawTotal)
	}
}

func TestCutIndex(t *testing.T) {
	conv := func() events.Event { return userReq("u") }
	idle := func() events.Event { return &events.UsageEvent{} }

	tests := []struct {
		name string
		evs  []events.Event
		keep int
		want int
	}{
		{"fewer than keep", []events.Event{conv()}, 2, -1},
		{"exactly keep", []events.Event{conv(), conv()}, 2, 0},
		{"skip telemetry from the tail", []events.Event{conv(), idle(), conv(), idle()}, 2, 0},
		{"middle cut", []events.Event{conv(), conv(), conv(), conv()}, 2, 2},
		{"keep zero is invalid", []events.Event{conv()}, 0, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cutIndex(tt.evs, tt.keep); got != tt.want {
				t.Fatalf("cutIndex = %d, want %d", got, tt.want)
			}
		})
	}
}
