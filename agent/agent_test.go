package agent

import (
	stdcontext "context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	agocontext "ago/context"
	"ago/events"
	"ago/middleware"
	"ago/response"
	"ago/stream"
	agotest "ago/testing"
	"ago/tools"
)

func textResp(content string) *events.ModelResponse {
	return &events.ModelResponse{Message: &events.ModelMessage{Content: content},
		Usage: events.Usage{PromptTokens: events.I64(1), CompletionTokens: events.I64(2), TotalTokens: events.I64(3)}}
}

func toolResp(name, argsJSON string) *events.ModelResponse {
	call := events.ToolCallEvent{ID: "call-" + name, Name: name, Arguments: argsJSON}
	return &events.ModelResponse{
		ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{call}},
		Usage:     events.Usage{PromptTokens: events.I64(1), TotalTokens: events.I64(1)},
	}
}

func TestAskBasic(t *testing.T) {
	cfg := agotest.NewTestConfig(textResp("hello there"))
	a := New("helper", "You are helpful.", WithConfig(cfg))
	reply, err := a.Ask(stdcontext.Background(), "hi")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if reply.Body() != "hello there" {
		t.Fatalf("body: %q", reply.Body())
	}
	evs, _ := reply.History().GetEvents()
	var gotReq, gotResp bool
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.ModelRequest:
			gotReq = true
			if len(e.Parts) != 1 || e.Parts[0].(*events.TextInput).Content != "hi" {
				t.Fatalf("request parts wrong: %+v", e.Parts)
			}
		case events.ModelResponse:
			gotResp = true
		}
	}
	if !gotReq || !gotResp {
		t.Fatalf("transcript missing request/response: %d events", len(evs))
	}
	// Usage: one model call recorded through the event log.
	if got := reply.Usage().Total.TotalTokens; got == nil || *got != 3 {
		t.Fatalf("usage total: %v", got)
	}
}

func TestAskCallsScriptedInOrder(t *testing.T) {
	cfg := agotest.NewTestConfig(textResp("one"), textResp("two"), textResp("three"))
	a := New("a", "", WithConfig(cfg))
	r1, err := a.Ask(stdcontext.Background(), "1")
	if err != nil || r1.Body() != "one" {
		t.Fatalf("first: %q %v", r1.Body(), err)
	}
	// Continuation on the same stream sees the full history.
	r2, err := r1.Ask(stdcontext.Background(), "2")
	if err != nil || r2.Body() != "two" {
		t.Fatalf("second: %q %v", r2.Body(), err)
	}
	evs, _ := r2.History().GetEvents()
	requests := 0
	for _, ev := range evs {
		if _, ok := events.AsValue(ev).(events.ModelRequest); ok {
			requests++
		}
	}
	if requests != 2 {
		t.Fatalf("continuation should add one request, got %d", requests)
	}
	if calls := cfg.Calls(); len(calls) != 2 {
		t.Fatalf("client saw %d calls, want 2", len(calls))
	} else {
		// The second call must replay the first turn's response.
		var sawPrior bool
		for _, ev := range calls[1] {
			if mr, ok := events.AsValue(ev).(events.ModelResponse); ok && mr.Content() == "one" {
				sawPrior = true
			}
		}
		if !sawPrior {
			t.Fatal("second call did not replay the first turn's response")
		}
	}
}

func TestAskMissingConfig(t *testing.T) {
	a := New("a", "p")
	if _, err := a.Ask(stdcontext.Background(), "hi"); !errors.Is(err, ErrConfigNotProvided) {
		t.Fatalf("want ErrConfigNotProvided, got %v", err)
	}
}

func TestToolRoundTrip(t *testing.T) {
	executed := ""
	weather := func(args struct {
		City string `json:"city" agschema:"description=City name,required"`
	}) (string, error) {
		executed = args.City
		return "sunny in " + args.City, nil
	}
	tool, err := tools.NewFunctionTool("get_weather", "Get the weather", weather)
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	cfg := agotest.NewTestConfig(
		toolResp("get_weather", `{"city":"Moscow"}`),
		textResp("The weather is sunny."),
	)
	a := New("assistant", "", WithConfig(cfg), WithTools(tool))
	reply, err := a.Ask(stdcontext.Background(), "what is the weather in Moscow?")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if executed != "Moscow" {
		t.Fatalf("tool executed with %q", executed)
	}
	if reply.Body() != "The weather is sunny." {
		t.Fatalf("body: %q", reply.Body())
	}
	evs, _ := reply.History().GetEvents()
	var sawCalls, sawResults bool
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.ToolCallsEvent:
			sawCalls = true
			if e.Calls[0].Name != "get_weather" {
				t.Fatalf("call name: %s", e.Calls[0].Name)
			}
		case events.ToolResultsEvent:
			sawResults = true
			if txt := events.RenderForPrompt(e.Results[0].Result.Parts[0]); !strings.Contains(txt, "sunny") {
				t.Fatalf("result text: %q", txt)
			}
		}
	}
	if !sawCalls || !sawResults {
		t.Fatalf("transcript missing tool events (calls=%v results=%v)", sawCalls, sawResults)
	}
}

func TestToolNotFoundContinues(t *testing.T) {
	cfg := agotest.NewTestConfig(
		toolResp("no_such_tool", "{}"),
		textResp("handled"),
	)
	a := New("a", "", WithConfig(cfg))
	reply, err := a.Ask(stdcontext.Background(), "go")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if reply.Body() != "handled" {
		t.Fatalf("body: %q", reply.Body())
	}
}

type askRecorder struct {
	mu   sync.Mutex
	asks int
}

func (r *askRecorder) hook(ctx stdcontext.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
	r.mu.Lock()
	r.asks++
	r.mu.Unlock()
	return &events.HumanMessage{Content: "yes, approved"}, nil
}

func TestHitlToolApproval(t *testing.T) {
	var rec askRecorder
	sensitive := func(args struct{}, ctx *agocontext.Context) (string, error) {
		answer, err := ctx.Input(stdcontext.Background(), "delete everything?", 0)
		if err != nil {
			return "", err
		}
		return "did it: " + answer, nil
	}
	tool, err := tools.NewFunctionTool("purge", "Purge data", sensitive)
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	cfg := agotest.NewTestConfig(
		toolResp("purge", "{}"),
		textResp("purge complete"),
	)
	a := New("operator", "", WithConfig(cfg), WithTools(tool), WithHitlHook(rec.hook))
	reply, err := a.Ask(stdcontext.Background(), "clean up")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if rec.asks != 1 {
		t.Fatalf("hook asked %d times, want 1", rec.asks)
	}
	if reply.Body() != "purge complete" {
		t.Fatalf("body: %q", reply.Body())
	}
	// Only the answer is on the transcript: the question is delivered to the
	// hook through the stream and swallowed there (Python parity: dispatch stops
	// at the interrupt subscriber before the persistence subscriber).
	evs, _ := reply.History().GetEvents()
	var sawReq, sawAnswer bool
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.HumanInputRequest:
			sawReq = e.Content == "delete everything?"
		case events.HumanMessage:
			sawAnswer = e.Content == "yes, approved"
		}
	}
	if sawReq {
		t.Fatal("question must not be transcribed (dispatch stops at the interrupt subscriber)")
	}
	if !sawAnswer {
		t.Fatal("answer missing from the transcript")
	}
}

// TestHitlHookReceivesRequestDeadline pins the per-request context: the user
// hook receives the context carrying the request deadline, not the bare turn
// context (Python parity: the timeout covers the hook — wait_for cancels it).
func TestHitlHookReceivesRequestDeadline(t *testing.T) {
	var (
		mu       sync.Mutex
		hadDdl   bool
		ddlCalls int
	)
	hook := func(ctx stdcontext.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
		mu.Lock()
		defer mu.Unlock()
		ddlCalls++
		_, ok := ctx.Deadline()
		hadDdl = hadDdl || ok
		return &events.HumanMessage{Content: "y"}, nil
	}
	sensitive := func(args struct{}, ctx *agocontext.Context) (string, error) {
		return ctx.Input(stdcontext.Background(), "delete everything?", 30*time.Second)
	}
	tool, err := tools.NewFunctionTool("purge", "Purge data", sensitive)
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	cfg := agotest.NewTestConfig(toolResp("purge", "{}"), textResp("purge complete"))
	a := New("operator", "", WithConfig(cfg), WithTools(tool), WithHitlHook(hook))
	if _, err := a.Ask(stdcontext.Background(), "clean up"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if ddlCalls == 0 || !hadDdl {
		t.Fatalf("hook asked %d times, saw a deadline: %v", ddlCalls, hadDdl)
	}
}

func TestHitlMissingHookRepairsHistory(t *testing.T) {
	dangerous := func(args struct{}, ctx *agocontext.Context) (string, error) {
		return ctx.Input(stdcontext.Background(), "sure?", 0)
	}
	tool, _ := tools.NewFunctionTool("risky", "Risky", dangerous)
	base := stream.NewMemoryStream()
	cfg2 := agotest.NewTestConfig(toolResp("risky", "{}"), textResp("never reached"))
	a2 := New("a2", "", WithConfig(cfg2), WithTools(tool), WithStreamBase(base))
	if _, err := a2.Ask(stdcontext.Background(), "go"); err == nil {
		t.Fatal("expected HITL failure")
	}
	fixed, _ := base.History().CloseUnanswered()
	if fixed != 0 {
		t.Fatalf("history should already be repaired, found %d unanswered", fixed)
	}
	evs, _ := base.History().GetEvents()
	closed := false
	for _, ev := range evs {
		if e, ok := events.AsValue(ev).(events.ToolResultEvent); ok && len(e.Result.Parts) > 0 &&
			events.AsValue(e.Result.Parts[0]).(events.TextInput).Content == events.SentinelToolResult {
			closed = true
		}
	}
	if !closed {
		t.Fatal("sentinel repair result missing from history")
	}
}

func TestConcurrentAsksOnOneAgent(t *testing.T) {
	// Concurrent asks on one agent (separate streams — the default) must not
	// interfere. A shared stream base merges concurrent input by design (the
	// inbox drain), which is timing-dependent and covered indirectly by the
	// continuation tests.
	cfg := agotest.NewTestConfig(textResp("r1"), textResp("r2"))
	a := New("a", "", WithConfig(cfg))
	var wg sync.WaitGroup
	var mu sync.Mutex
	bodies := map[string]bool{}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			reply, err := a.Ask(stdcontext.Background(), "q")
			if err != nil {
				t.Errorf("concurrent ask: %v", err)
				return
			}
			mu.Lock()
			bodies[reply.Body()] = true
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if len(bodies) != 2 || !bodies["r1"] || !bodies["r2"] {
		t.Fatalf("expected both scripted replies, got %v", bodies)
	}
}

func TestMiddlewareOrdering(t *testing.T) {
	var mu sync.Mutex
	var order []string
	rec := func(name string) middleware.Factory {
		return func() middleware.Middleware {
			return &recMW{name: name, order: &order, mu: &mu}
		}
	}
	cfg := agotest.NewTestConfig(textResp("done"))
	a := New("a", "", WithConfig(cfg), WithMiddleware(rec("first"), rec("second")))
	reply, err := a.Ask(stdcontext.Background(), "go")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if reply.Body() != "done" {
		t.Fatalf("body: %q", reply.Body())
	}
	// OnTurn: first is outermost; OnLLMCall: first wraps second wraps client.
	want := []string{"first:on_turn:in", "second:on_turn:in", "first:on_llm:in", "second:on_llm:in",
		"second:on_llm:out", "first:on_llm:out", "second:on_turn:out", "first:on_turn:out"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("order:\n got %v\nwant %v", order, want)
	}
}

type recMW struct {
	middleware.Base
	name  string
	order *[]string
	mu    *sync.Mutex
}

func (m *recMW) log(s string) {
	m.mu.Lock()
	*m.order = append(*m.order, s)
	m.mu.Unlock()
}

func (m *recMW) OnTurn(next middleware.TurnFunc, ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
	m.log(m.name + ":on_turn:in")
	resp, err := next(ev, ctx)
	m.log(m.name + ":on_turn:out")
	return resp, err
}

func (m *recMW) OnLLMCall(next middleware.LLMCallFunc, evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
	m.log(m.name + ":on_llm:in")
	resp, err := next(evs, ctx)
	m.log(m.name + ":on_llm:out")
	return resp, err
}

func TestResumeFromToolResults(t *testing.T) {
	cfg := agotest.NewTestConfig(textResp("resumed answer"))
	a := New("a", "", WithConfig(cfg))
	seed := []events.Event{
		&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "go"}}},
		&events.ModelResponse{
			ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{{
				ID: "c1", Name: "t", Arguments: "{}",
			}}},
		},
	}
	trigger := &events.ToolResultsEvent{Results: []events.ToolResultEvent{
		{ParentID: "c1", Name: "t", Result: events.NewToolResult(false, "tool output")},
	}}
	reply, err := a.Resume(stdcontext.Background(), append(seed, trigger)...)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if reply.Body() != "resumed answer" {
		t.Fatalf("body: %q", reply.Body())
	}
	evs, _ := reply.History().GetEvents()
	// seed 2 + trigger 1 + UsageEvent 1 + response 1
	if len(evs) != 5 {
		t.Fatalf("history: %d events (want 5)", len(evs))
	}
}

func TestContentAsWithRetry(t *testing.T) {
	type qa struct {
		Answer string `json:"answer"`
	}
	schema := response.NewSchema[qa]("qa", "Answer questions.")
	cfg := agotest.NewTestConfig(
		&events.ModelResponse{Message: &events.ModelMessage{Content: `{"answer": 42}`}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: `{"answer": "forty two"}`}},
	)
	a := New("a", "", WithConfig(cfg), WithResponseSchema(schema))
	reply, err := a.Ask(stdcontext.Background(), "meaning of life?")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	got, err := ContentAs[qa](stdcontext.Background(), reply, 1)
	if err != nil {
		t.Fatalf("content: %v", err)
	}
	if got.Answer != "forty two" {
		t.Fatalf("answer: %q", got.Answer)
	}
}

func TestSubtaskRollsUp(t *testing.T) {
	// Parent's client sees: 1) tool call run_subtask, 3) final wrap.
	parentCfg := agotest.NewTestConfig(
		toolResp("run_subtask", `{"objective":"count to two"}`),
		textResp("parent done"),
	)
	// Child's client sees: 2) plain answer.
	childCfg := agotest.NewTestConfig(
		&events.ModelResponse{Message: &events.ModelMessage{Content: "child result"},
			Usage: events.Usage{PromptTokens: events.I64(5), CompletionTokens: events.I64(5), TotalTokens: events.I64(10)}},
	)
	a := New("parent", "", WithConfig(parentCfg), WithTasks(TaskConfig{Config: childCfg}))
	reply, err := a.Ask(stdcontext.Background(), "delegate please")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if reply.Body() != "parent done" {
		t.Fatalf("body: %q", reply.Body())
	}
	// The parent stream carries the rolled-up subtask usage.
	evs, _ := reply.History().GetEvents()
	var sawRollup bool
	for _, ev := range evs {
		if ue, ok := events.AsValue(ev).(events.UsageEvent); ok && ue.Kind == events.UsageKindSubtask {
			sawRollup = true
			if ue.Usage.TotalTokens == nil || *ue.Usage.TotalTokens != 10 {
				t.Fatalf("rollup total: %v", ue.Usage.TotalTokens)
			}
		}
	}
	if !sawRollup {
		t.Fatal("subtask usage rollup missing")
	}
}

func TestAsToolDelegation(t *testing.T) {
	child := New("specialist", "You are a specialist.", WithConfig(agotest.NewTestConfig(textResp("specialist answer"))))
	parentTool := child.AsTool(WithToolName("ask_specialist"))
	parent := New("lead", "", WithConfig(agotest.NewTestConfig(
		toolResp("ask_specialist", `{"message":"what do you know?"}`),
		textResp("lead done"),
	)), WithTools(parentTool))
	reply, err := parent.Ask(stdcontext.Background(), "consult the specialist")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if reply.Body() != "lead done" {
		t.Fatalf("body: %q", reply.Body())
	}
}

func TestAskCancel(t *testing.T) {
	cfg := agotest.NewTestConfig(textResp("too late"))
	a := New("a", "", WithConfig(cfg))
	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	cancel()
	if _, err := a.Ask(ctx, "hi"); err == nil {
		t.Fatal("expected cancellation error")
	}
}

func TestRunObservableAPI(t *testing.T) {
	cfg := agotest.NewTestConfig(textResp("streamed"))
	a := New("a", "", WithConfig(cfg))
	run, err := a.Run(stdcontext.Background(), "hi")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var chunks []string
	sub := run.Stream().Subscribe(func(ev events.Event) error {
		if mc, ok := events.AsValue(ev).(events.ModelResponse); ok {
			chunks = append(chunks, mc.Content())
		}
		return nil
	}, stream.SubscribeOptions{Condition: events.TypeOf[events.ModelResponse]()})
	defer run.Stream().Unsubscribe(sub)

	if err := run.Start(stdcontext.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := run.Result(stdcontext.Background()); err != nil {
			t.Errorf("result: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not finish in time")
	}
	run.Close()
	if len(chunks) != 1 || chunks[0] != "streamed" {
		t.Fatalf("stream observer saw %v", chunks)
	}
}
