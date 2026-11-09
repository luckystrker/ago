package agent

import (
	stdcontext "context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
	agotask "github.com/luckystrker/ago/task"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
	"github.com/luckystrker/ago/usage"
)

// taskLifecycleTypes lists the Task lifecycle event types on a stream in order.
func taskLifecycleTypes(t *testing.T, s stream.Stream) []string {
	t.Helper()
	evs, err := s.History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var out []string
	for _, ev := range evs {
		switch events.AsValue(ev).(type) {
		case events.TaskStarted:
			out = append(out, "started")
		case events.TaskProgress:
			out = append(out, "progress")
		case events.TaskCompleted:
			out = append(out, "completed")
		case events.TaskFailed:
			out = append(out, "failed")
		case events.TaskCancelled:
			out = append(out, "cancelled")
		case events.TaskExpired:
			out = append(out, "expired")
		}
	}
	return out
}

// TestUntaggedCallsPublishNoTaskEvents pins ADR 0035: an ordinary invocation
// without capability/ttl stays byte-identical to the pre-lifecycle behavior.
func TestUntaggedCallsPublishNoTaskEvents(t *testing.T) {
	t.Run("ask", func(t *testing.T) {
		s := stream.NewMemoryStream()
		a := New("analyst", "", WithConfig(agotest.NewTestConfig(textResp("done"))), WithStreamBase(s))
		if _, err := a.Ask(stdcontext.Background(), "analyse"); err != nil {
			t.Fatalf("ask: %v", err)
		}
		if got := taskLifecycleTypes(t, s); len(got) != 0 {
			t.Fatalf("task events = %v, want none", got)
		}
	})
	t.Run("run", func(t *testing.T) {
		s := stream.NewMemoryStream()
		a := New("analyst", "", WithConfig(agotest.NewTestConfig(textResp("done"))), WithStreamBase(s))
		run, err := a.Run(stdcontext.Background(), "analyse")
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if _, err := run.Result(stdcontext.Background()); err != nil {
			t.Fatalf("result: %v", err)
		}
		run.Close()
		if got := taskLifecycleTypes(t, s); len(got) != 0 {
			t.Fatalf("task events = %v, want none", got)
		}
	})
}

// TestTaggedAskReportsItsTaskWithoutThePrompt pins upstream
// test_tagged_ask_reports_its_task_without_leaking_the_prompt: the title is the
// agent name and the prompt never reaches a Task event (and therefore never the
// network mirror).
func TestTaggedAskReportsItsTaskWithoutLeakingThePrompt(t *testing.T) {
	s := stream.NewMemoryStream()
	a := New("analyst", "", WithConfig(agotest.NewTestConfig(textResp("done"))), WithStreamBase(s))

	reply, err := a.AskWith(stdcontext.Background(), RunOptions{Capability: "analysis"}, "confidential: customer list")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if reply.Body() != "done" {
		t.Fatalf("body = %q", reply.Body())
	}
	te := collectTaskEvents(t, s)
	if len(te.started) != 1 || len(te.completed) != 1 || len(te.failed) != 0 {
		t.Fatalf("lifecycle = started %d completed %d failed %d", len(te.started), len(te.completed), len(te.failed))
	}
	started, completed := te.started[0], te.completed[0]
	if started.AgentName != "analyst" || started.Objective != "analyst" {
		t.Fatalf("started identity = %q/%q, want the agent name", started.AgentName, started.Objective)
	}
	if started.Spec == nil || started.Spec.Title != "analyst" || started.Spec.Capability != "analysis" {
		t.Fatalf("started spec = %+v", started.Spec)
	}
	if started.ExpiresAt != "" || started.ParentToolCallID != "" {
		t.Fatalf("unexpected ttl/parent: %q/%q", started.ExpiresAt, started.ParentToolCallID)
	}
	if completed.TaskID != started.TaskID || completed.Result != "done" {
		t.Fatalf("completed = %+v", completed)
	}
	if completed.TaskStream != s.ID() {
		t.Fatalf("task stream = %q, want %q", completed.TaskStream, s.ID())
	}
	evs, _ := s.History().GetEvents()
	for _, ev := range evs {
		switch ev.(type) {
		case *events.TaskStarted, *events.TaskCompleted, events.TaskStarted, events.TaskCompleted:
			data, merr := events.MarshalEvent(ev)
			if merr != nil {
				t.Fatalf("marshal task event: %v", merr)
			}
			if strings.Contains(string(data), "confidential") {
				t.Fatalf("prompt leaked into a task event: %s", data)
			}
		}
	}
}

// TestTTLPublishesADeadline pins upstream test_ttl_publishes_a_deadline.
func TestTTLPublishesADeadline(t *testing.T) {
	s := stream.NewMemoryStream()
	a := New("analyst", "", WithConfig(agotest.NewTestConfig(textResp("done"))), WithStreamBase(s))
	if _, err := a.AskWith(stdcontext.Background(), RunOptions{TTL: time.Minute}, "analyse"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	te := collectTaskEvents(t, s)
	if len(te.started) != 1 {
		t.Fatalf("started = %d", len(te.started))
	}
	expires, err := time.Parse(time.RFC3339, te.started[0].ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at %q: %v", te.started[0].ExpiresAt, err)
	}
	if !expires.After(time.Now()) {
		t.Fatalf("expires_at %v is not in the future", expires)
	}
}

// TestRunPublishesTheTerminalOnClose pins upstream
// test_run_publishes_the_terminal_after_the_block_exits: result() leaves the
// scope open; the terminal follows on Close, and it is the last event.
func TestRunPublishesTheTerminalOnClose(t *testing.T) {
	s := stream.NewMemoryStream()
	a := New("analyst", "", WithConfig(agotest.NewTestConfig(textResp("done"))), WithStreamBase(s))
	run, err := a.RunWith(stdcontext.Background(), RunOptions{Capability: "analysis"}, "analyse")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := run.Start(stdcontext.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := run.Result(stdcontext.Background()); err != nil {
		t.Fatalf("result: %v", err)
	}
	if got := taskLifecycleTypes(t, s); len(got) != 1 || got[0] != "started" {
		t.Fatalf("before Close task events = %v", got)
	}
	run.Close()
	if got := taskLifecycleTypes(t, s); len(got) != 2 || got[0] != "started" || got[1] != "completed" {
		t.Fatalf("after Close task events = %v", got)
	}
}

// TestRunLeftWithoutDrivingPublishesNothing pins upstream
// test_run_left_without_driving_publishes_nothing.
func TestRunLeftWithoutDrivingPublishesNothing(t *testing.T) {
	s := stream.NewMemoryStream()
	a := New("analyst", "", WithConfig(agotest.NewTestConfig(textResp("unused"))), WithStreamBase(s))
	run, err := a.RunWith(stdcontext.Background(), RunOptions{Capability: "analysis"}, "analyse")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	run.Close()
	if got := taskLifecycleTypes(t, s); len(got) != 0 {
		t.Fatalf("task events = %v, want none", got)
	}
}

// TestTaggedRunFailurePublishesTaskFailed pins the always-terminal guarantee on
// a setup failure, including a missing model config.
func TestTaggedRunFailurePublishesTaskFailed(t *testing.T) {
	cases := []struct {
		name   string
		agent  func(*stream.MemoryStream) *Agent
		errSub string
	}{
		{
			name: "no config",
			agent: func(s *stream.MemoryStream) *Agent {
				return New("analyst", "", WithStreamBase(s))
			},
			errSub: "config",
		},
		{
			name: "create fails",
			agent: func(s *stream.MemoryStream) *Agent {
				return New("analyst", "", WithStreamBase(s), WithConfig(failingConfig{err: errors.New("no model")}))
			},
			errSub: "no model",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := stream.NewMemoryStream()
			_, err := tc.agent(s).AskWith(stdcontext.Background(), RunOptions{Capability: "analysis"}, "analyse")
			if err == nil || !strings.Contains(err.Error(), tc.errSub) {
				t.Fatalf("ask error = %v, want %q", err, tc.errSub)
			}
			if got := taskLifecycleTypes(t, s); len(got) != 2 || got[0] != "started" || got[1] != "failed" {
				t.Fatalf("task events = %v, want started+failed", got)
			}
		})
	}
}

// TestTaggedRunAdoptsAnObserverExpiry pins upstream
// test_expiry_reported_by_an_observer_is_final_and_does_not_stop_the_run: an
// external terminal for the task id is adopted, the run still returns its
// reply, and no second terminal is published.
func TestTaggedRunAdoptsAnObserverExpiry(t *testing.T) {
	s := stream.NewMemoryStream()
	s.Subscribe(func(ev events.Event) error {
		started, ok := events.AsValue(ev).(events.TaskStarted)
		if !ok {
			return nil
		}
		return s.Send(&events.TaskExpired{
			TaskID:    started.TaskID,
			AgentName: started.AgentName,
			Objective: started.Objective,
		})
	}, stream.SubscribeOptions{Condition: events.TypeOf[events.TaskStarted]()})
	a := New("analyst", "", WithConfig(agotest.NewTestConfig(textResp("done"))), WithStreamBase(s))
	reply, err := a.AskWith(stdcontext.Background(), RunOptions{TTL: time.Minute}, "analyse")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if reply.Body() != "done" {
		t.Fatalf("body = %q", reply.Body())
	}
	if got := taskLifecycleTypes(t, s); len(got) != 2 || got[0] != "started" || got[1] != "expired" {
		t.Fatalf("task events = %v, want started+expired", got)
	}
}

// ctxBlockingConfig holds the model call until the turn context is cancelled.
type ctxBlockingConfig struct{ entered chan struct{} }

func (c *ctxBlockingConfig) Provider() config.Provider { return "test" }
func (c *ctxBlockingConfig) Model() string             { return "blocking" }
func (c *ctxBlockingConfig) Create() (config.LLMClient, error) {
	return c, nil
}
func (c *ctxBlockingConfig) Clone(map[string]any) (config.ModelConfig, error) { return c, nil }
func (c *ctxBlockingConfig) Call(ctx stdcontext.Context, _ []events.Event, _ *agocontext.Context, _ config.CallOptions) (*events.ModelResponse, error) {
	select {
	case c.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestTaggedRunCancellationPublishesTaskCancelled pins the cancellation terminal
// and cleanup ordering: the client observes ctx.Done before Close returns.
func TestTaggedRunCancellationPublishesTaskCancelled(t *testing.T) {
	s := stream.NewMemoryStream()
	cfg := &ctxBlockingConfig{entered: make(chan struct{}, 1)}
	a := New("analyst", "", WithConfig(cfg), WithStreamBase(s))
	run, err := a.RunWith(stdcontext.Background(), RunOptions{Capability: "analysis"}, "analyse")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := run.Start(stdcontext.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-cfg.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("model call never started")
	}
	run.Close()
	evs, _ := s.History().GetEvents()
	var cancelled []events.TaskCancelled
	var types []string
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.TaskStarted:
			types = append(types, "started")
		case events.TaskCancelled:
			types = append(types, "cancelled")
			cancelled = append(cancelled, e)
		}
	}
	if len(types) != 2 || types[0] != "started" || types[1] != "cancelled" {
		t.Fatalf("task events = %v, want started+cancelled", types)
	}
	if cancelled[0].Reason != "cancelled" || cancelled[0].Objective != "analyst" {
		t.Fatalf("cancelled = %+v", cancelled[0])
	}
}

// panicTurnMiddleware panics inside the turn, exercising the recover in
// executeTurn: the lifecycle must still publish a failed terminal and no panic
// may escape the caller.
type panicTurnMiddleware struct{ middleware.Base }

func (panicTurnMiddleware) OnTurn(middleware.TurnFunc, *events.ModelRequest, *agocontext.Context) (*events.ModelResponse, error) {
	panic("middleware exploded")
}

// TestTaggedRunMiddlewarePanicFailsTaskAndDoesNotEscape pins the panic path.
func TestTaggedRunMiddlewarePanicFailsTaskAndDoesNotEscape(t *testing.T) {
	s := stream.NewMemoryStream()
	a := New("analyst", "", WithConfig(agotest.NewTestConfig(textResp("done"))), WithStreamBase(s),
		WithMiddleware(func() middleware.Middleware { return panicTurnMiddleware{} }))
	_, err := a.AskWith(stdcontext.Background(), RunOptions{Capability: "analysis"}, "analyse")
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("ask error = %v, want the recovered panic", err)
	}
	if got := taskLifecycleTypes(t, s); len(got) != 2 || got[0] != "started" || got[1] != "failed" {
		t.Fatalf("task events = %v, want started+failed", got)
	}
	// The failed scope must release the per-stream lock: a later turn on the
	// same stream starts immediately.
	next := New("next", "", WithConfig(agotest.NewTestConfig(textResp("ok"))), WithStreamBase(s))
	ctx, cancel := stdcontext.WithTimeout(stdcontext.Background(), 2*time.Second)
	defer cancel()
	if reply, nerr := next.Ask(ctx, "next"); nerr != nil || reply.Body() != "ok" {
		t.Fatalf("stream not released after the panic: reply=%v err=%v", reply, nerr)
	}
}

// panickingTool panics in its body; the tool layer recovers it into a tool
// error, so the run still terminates normally and the panic never escapes.
func panickingTool(t *testing.T) tools.Tool {
	t.Helper()
	ft, err := tools.NewFunctionTool("boom", "Panic while executing", func(struct{}) (string, error) {
		panic("tool exploded")
	})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	return ft
}

// TestTaggedRunToolPanicStillTerminates pins the tool panic guarantee: no panic
// escapes, the error is a tool result, and the task reaches a terminal.
func TestTaggedRunToolPanicStillTerminates(t *testing.T) {
	s := stream.NewMemoryStream()
	a := New("analyst", "", WithStreamBase(s),
		WithConfig(agotest.NewTestConfig(toolResp("boom", `{}`), textResp("recovered"))),
		WithTools(panickingTool(t)))
	reply, err := a.AskWith(stdcontext.Background(), RunOptions{Capability: "analysis"}, "analyse")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if reply.Body() != "recovered" {
		t.Fatalf("body = %q", reply.Body())
	}
	if got := taskLifecycleTypes(t, s); len(got) != 2 || got[0] != "started" || got[1] != "completed" {
		t.Fatalf("task events = %v, want started+completed", got)
	}
}

// TestTaggedContinuationIsANewTask pins ADR 0035: capability/ttl are per
// invocation, a continuation gets a fresh identity, and an untagged call after
// a tagged one emits nothing.
func TestTaggedContinuationIsANewTask(t *testing.T) {
	s := stream.NewMemoryStream()
	a := New("analyst", "", WithConfig(agotest.NewTestConfig(textResp("one"), textResp("two"), textResp("three"))), WithStreamBase(s))
	first, err := a.AskWith(stdcontext.Background(), RunOptions{Capability: "analysis"}, "first")
	if err != nil {
		t.Fatalf("first ask: %v", err)
	}
	second, err := first.AskWith(stdcontext.Background(), RunOptions{Capability: "followup"}, "second")
	if err != nil {
		t.Fatalf("second ask: %v", err)
	}
	if _, err := second.Ask(stdcontext.Background(), "third"); err != nil {
		t.Fatalf("third ask: %v", err)
	}
	te := collectTaskEvents(t, s)
	if len(te.started) != 2 || len(te.completed) != 2 {
		t.Fatalf("lifecycle = started %d completed %d, want 2/2", len(te.started), len(te.completed))
	}
	var capabilities []string
	for _, started := range te.started {
		if started.Spec == nil {
			t.Fatalf("started without spec: %+v", started)
		}
		capabilities = append(capabilities, started.Spec.Capability)
	}
	if capabilities[0] != "analysis" || capabilities[1] != "followup" {
		t.Fatalf("capabilities = %v", capabilities)
	}
	if te.started[0].TaskID == te.started[1].TaskID {
		t.Fatal("continuation reused the previous task id")
	}
}

// TestDelegationTaskStartedCarriesSpecAndParentCall pins the delegation side of
// the unified lifecycle: the same Task primitive, objective title, real parent
// tool-call id, and a populated TaskSpec.
func TestDelegationTaskStartedCarriesSpecAndParentCall(t *testing.T) {
	base := stream.NewMemoryStream()
	child := New("worker", "", WithConfig(agotest.NewTestConfig(textResp("summary"))))
	root := New("lead", "", WithStreamBase(base), WithConfig(agotest.NewTestConfig(
		toolResp("task_worker", `{"objective":"summarise the report"}`), textResp("done"))),
		WithTools(child.AsTool(WithToolName("task_worker"))))
	if _, err := root.Ask(stdcontext.Background(), "go"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	te := collectTaskEvents(t, base)
	if len(te.started) != 1 || len(te.completed) != 1 {
		t.Fatalf("lifecycle = started %d completed %d", len(te.started), len(te.completed))
	}
	started := te.started[0]
	if started.Spec == nil || started.Spec.Title != "summarise the report" {
		t.Fatalf("delegation spec = %+v", started.Spec)
	}
	if started.Objective != "summarise the report" || started.AgentName != "worker" {
		t.Fatalf("delegation identity = %q/%q", started.AgentName, started.Objective)
	}
	if started.ParentToolCallID != "call-task_worker" {
		t.Fatalf("parent tool call = %q", started.ParentToolCallID)
	}
}

// TestDelegationParentCancellationCancelsChild pins the closed D-092 gap:
// AsTool starts the child on the parent turn's cancellable context, so a parent
// cancellation reaches the child and is reported as PARENT_CANCELLED.
func TestDelegationParentCancellationCancelsChild(t *testing.T) {
	parentStream := stream.NewMemoryStream()
	childCfg := &ctxBlockingConfig{entered: make(chan struct{}, 1)}
	child := New("worker", "", WithConfig(childCfg))
	root := New("lead", "", WithStreamBase(parentStream), WithConfig(agotest.NewTestConfig(
		toolResp("task_worker", `{"objective":"work"}`), textResp("done"))),
		WithTools(child.AsTool(WithToolName("task_worker"))))

	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	done := make(chan error, 1)
	go func() {
		_, err := root.AskWith(ctx, RunOptions{Capability: "triage"}, "go")
		done <- err
	}()
	select {
	case <-childCfg.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("child model call never started")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, stdcontext.Canceled) {
			t.Fatalf("root ask error = %v, want cancellation", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled parent turn did not finish")
	}

	evs, _ := parentStream.History().GetEvents()
	var childCancelled, parentCancelled bool
	for _, ev := range evs {
		e, ok := events.AsValue(ev).(events.TaskCancelled)
		if !ok {
			continue
		}
		switch e.AgentName {
		case "worker":
			childCancelled = true
			if e.Reason != ParentCancelledReason {
				t.Fatalf("child cancel reason = %q, want %q", e.Reason, ParentCancelledReason)
			}
		case "lead":
			parentCancelled = true
			if e.Reason != "cancelled" {
				t.Fatalf("parent cancel reason = %q, want cancelled", e.Reason)
			}
		}
	}
	if !childCancelled || !parentCancelled {
		t.Fatalf("cancelled terminals = child %v parent %v", childCancelled, parentCancelled)
	}
}

// TestDelegationUsageReadFailureStillPublishesTerminal pins the always-terminal
// guarantee when the child transcript cannot be read: the delegation fails, the
// parent turn continues, and exactly one terminal reaches the parent.
func TestDelegationUsageReadFailureStillPublishesTerminal(t *testing.T) {
	parentStream := stream.NewMemoryStream()
	childStream := stream.NewMemoryStream(stream.WithStorage(brokenReadStorage{stream.NewMemoryStorage()}))
	child := New("worker", "", WithConfig(agotest.NewTestConfig(textResp("never"))), WithStreamBase(childStream))
	root := New("lead", "", WithStreamBase(parentStream), WithConfig(agotest.NewTestConfig(
		toolResp("task_worker", `{"objective":"work"}`), textResp("recovered"))),
		WithTools(child.AsTool(WithToolName("task_worker"))))
	if _, err := root.Ask(stdcontext.Background(), "go"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	te := collectTaskEvents(t, parentStream)
	if len(te.started) != 1 || len(te.failed) != 1 || len(te.completed) != 0 {
		t.Fatalf("lifecycle = started %d failed %d completed %d, want 1/1/0",
			len(te.started), len(te.failed), len(te.completed))
	}
}

// TestDelegatedTurnPanicDoesNotEscape pins the panic guarantee across the
// delegation boundary: a panicking child middleware becomes TaskFailed, the
// parent recovers, and nothing panics the caller.
func TestDelegatedTurnPanicDoesNotEscape(t *testing.T) {
	base := stream.NewMemoryStream()
	child := New("worker", "", WithConfig(agotest.NewTestConfig(textResp("never"))),
		WithMiddleware(func() middleware.Middleware { return panicTurnMiddleware{} }))
	root := New("lead", "", WithStreamBase(base), WithConfig(agotest.NewTestConfig(
		toolResp("task_worker", `{"objective":"work"}`), textResp("recovered"))),
		WithTools(child.AsTool(WithToolName("task_worker"))))
	reply, err := root.Ask(stdcontext.Background(), "go")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if reply.Body() != "recovered" {
		t.Fatalf("body = %q", reply.Body())
	}
	te := collectTaskEvents(t, base)
	if len(te.started) != 1 || len(te.failed) != 1 {
		t.Fatalf("lifecycle = started %d failed %d, want 1/1", len(te.started), len(te.failed))
	}
}

// TestRunLifecyclePersistsRetainedAccounting pins upstream persist_accounting:
// source records captured by the relay but missing from the lifecycle stream are
// appended before the terminal, and the snapshot deduplicates them.
func TestRunLifecyclePersistsRetainedAccounting(t *testing.T) {
	s := stream.NewMemoryStream()
	cx := agocontext.New(s)
	lc := newRunLifecycle("worker", agotask.Spec{Title: "work"}, cx)
	if err := lc.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	src := events.NewUsageEvent(events.Usage{TotalTokens: events.I64(7)}, events.UsageKindModelCall)
	lc.accounting = func() []events.Event { return []events.Event{src} }
	if err := lc.finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
	evs, _ := s.History().GetEvents()
	var usageIDs []string
	var completed *events.TaskCompleted
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.UsageEvent:
			usageIDs = append(usageIDs, e.EventID)
		case events.TaskCompleted:
			completed = &e
		}
	}
	if len(usageIDs) != 1 || usageIDs[0] != src.EventID {
		t.Fatalf("retained usage ids = %v, want [%s]", usageIDs, src.EventID)
	}
	if completed == nil {
		t.Fatal("TaskCompleted not published")
	}
	if completed.Usage.TotalTokens == nil || *completed.Usage.TotalTokens != 7 {
		t.Fatalf("completed usage = %v, want 7 (no double count)", completed.Usage.TotalTokens)
	}
}

// White-box guard tests below: each pins a lifecycle branch that the
// end-to-end runs above never take (nil receivers, idempotent starts,
// pre-existing failures, storage faults, dedup edges). Without them an
// operator-level mutation of the branch survives the suite while silently
// changing the terminal contract.

// TestRunLifecycleStartedReportsDriveState pins started()/lifecycleStarted():
// nil and undriven lifecycles report false, a driven one true.
func TestRunLifecycleStartedReportsDriveState(t *testing.T) {
	var nilLC *runLifecycle
	if nilLC.started() {
		t.Fatal("nil lifecycle reports started")
	}
	s := stream.NewMemoryStream()
	lc := newRunLifecycle("worker", agotask.Spec{Title: "work"}, agocontext.New(s))
	if lc.started() {
		t.Fatal("fresh lifecycle reports started")
	}
	if err := lc.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !lc.started() {
		t.Fatal("driven lifecycle does not report started")
	}
	if err := lc.finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}

	var nilRun *AgentRun
	if nilRun.lifecycleStarted() {
		t.Fatal("nil run reports a started lifecycle")
	}
	if (&AgentRun{}).lifecycleStarted() {
		t.Fatal("run without a lifecycle reports started")
	}
	fresh := newRunLifecycle("worker", agotask.Spec{Title: "work"}, agocontext.New(stream.NewMemoryStream()))
	if (&AgentRun{lifecycle: fresh}).lifecycleStarted() {
		t.Fatal("run with an undriven lifecycle reports started")
	}
	if !(&AgentRun{lifecycle: lc}).lifecycleStarted() {
		t.Fatal("run with a driven lifecycle does not report started")
	}
}

// TestRunLifecycleStartIsIdempotent pins start() re-entry: a second call is a
// no-op, not a second TaskStarted.
func TestRunLifecycleStartIsIdempotent(t *testing.T) {
	s := stream.NewMemoryStream()
	lc := newRunLifecycle("worker", agotask.Spec{Title: "work"}, agocontext.New(s))
	if err := lc.start(); err != nil {
		t.Fatalf("first start: %v", err)
	}
	if err := lc.start(); err != nil {
		t.Fatalf("second start: %v", err)
	}
	if got := taskLifecycleTypes(t, s); len(got) != 1 || got[0] != "started" {
		t.Fatalf("task events = %v, want a single started", got)
	}
	if err := lc.finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

// TestRunLifecycleStartPreservesAPriorFailure pins the start error path: when
// TaskStarted cannot be sent, the handle survives for the terminal and keeps
// the earlier failure as its cause.
func TestRunLifecycleStartPreservesAPriorFailure(t *testing.T) {
	s := stream.NewMemoryStream(stream.WithStorage(failTaskStartedStorage{stream.NewMemoryStorage()}))
	lc := newRunLifecycle("worker", agotask.Spec{Title: "work"}, agocontext.New(s))
	prior := errors.New("prior cause")
	lc.failure = prior
	if err := lc.start(); err == nil {
		t.Fatal("start succeeded on a storage that rejects TaskStarted")
	}
	if lc.failure != prior {
		t.Fatalf("failure = %v, want the prior cause", lc.failure)
	}
}

// TestRunLifecycleRejectPreservesAPriorFailure pins reject() with a failure
// already recorded: the terminal reports the first cause, not the late one.
func TestRunLifecycleRejectPreservesAPriorFailure(t *testing.T) {
	s := stream.NewMemoryStream()
	lc := newRunLifecycle("worker", agotask.Spec{Title: "work"}, agocontext.New(s))
	var published []events.Event
	lc.publisher = func(ev events.Event) error {
		published = append(published, ev)
		return s.Send(ev)
	}
	prior := errors.New("prior cause")
	lc.failure = prior
	if err := lc.reject(errors.New("late failure")); err != nil {
		t.Fatalf("reject: %v", err)
	}
	te := collectTaskEvents(t, s)
	if len(te.started) != 1 || len(te.failed) != 1 {
		t.Fatalf("lifecycle = started %d failed %d, want 1/1", len(te.started), len(te.failed))
	}
	if te.failed[0].Error != "prior cause" {
		t.Fatalf("terminal cause = %q, want the prior failure", te.failed[0].Error)
	}
}

// TestRunLifecycleNilReceiversAreNoOps pins the defensive nil guards: a nil
// lifecycle neither starts nor publishes.
func TestRunLifecycleNilReceiversAreNoOps(t *testing.T) {
	var nilLC *runLifecycle
	if err := nilLC.start(); err != nil {
		t.Fatalf("nil start: %v", err)
	}
	sentinel := errors.New("rejected")
	if err := nilLC.reject(sentinel); err != sentinel {
		t.Fatalf("nil reject = %v, want the input failure back", err)
	}
	if err := nilLC.finish(); err != nil {
		t.Fatalf("nil finish: %v", err)
	}
}

// failUsageAppendStorage rejects transcript writes carrying source usage
// records, exercising the retain-failure branch of finish().
type failUsageAppendStorage struct{ stream.Storage }

func (s failUsageAppendStorage) SetHistory(id string, evs []events.Event) error {
	for _, ev := range evs {
		// AsValue unwraps pointer and routed forms, so one case covers them.
		if _, ok := events.AsValue(ev).(events.UsageEvent); ok {
			return errors.New("storage down on usage append")
		}
	}
	return s.Storage.SetHistory(id, evs)
}

// TestRunLifecyclePersistFailureDecidesTerminal pins the retain-failure
// contract: when retained accounting cannot be persisted and nothing failed
// before, the terminal is failed with the retain error; a prior failure keeps
// its cause.
func TestRunLifecyclePersistFailureDecidesTerminal(t *testing.T) {
	t.Run("retain failure fails the task", func(t *testing.T) {
		s := stream.NewMemoryStream(stream.WithStorage(failUsageAppendStorage{stream.NewMemoryStorage()}))
		lc := newRunLifecycle("worker", agotask.Spec{Title: "work"}, agocontext.New(s))
		if err := lc.start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		src := events.NewUsageEvent(events.Usage{TotalTokens: events.I64(7)}, events.UsageKindModelCall)
		lc.accounting = func() []events.Event { return []events.Event{src} }
		if err := lc.finish(); err == nil {
			t.Fatal("finish succeeded despite the retain failure")
		}
		te := collectTaskEvents(t, s)
		if len(te.started) != 1 || len(te.failed) != 1 {
			t.Fatalf("lifecycle = started %d failed %d, want 1/1", len(te.started), len(te.failed))
		}
		if !strings.Contains(te.failed[0].Error, "retain") {
			t.Fatalf("terminal cause = %q, want the retain error", te.failed[0].Error)
		}
	})
	t.Run("prior failure keeps its cause", func(t *testing.T) {
		s := stream.NewMemoryStream(stream.WithStorage(failUsageAppendStorage{stream.NewMemoryStorage()}))
		lc := newRunLifecycle("worker", agotask.Spec{Title: "work"}, agocontext.New(s))
		if err := lc.start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		src := events.NewUsageEvent(events.Usage{TotalTokens: events.I64(7)}, events.UsageKindModelCall)
		lc.accounting = func() []events.Event { return []events.Event{src} }
		prior := errors.New("turn broke")
		lc.failure = prior
		if err := lc.finish(); err != nil {
			t.Fatalf("finish: %v", err)
		}
		te := collectTaskEvents(t, s)
		if len(te.failed) != 1 || te.failed[0].Error != "turn broke" {
			t.Fatalf("terminal = %+v, want the prior cause", te.failed)
		}
	})
}

// failTerminalStorage rejects transcript writes carrying a failed terminal,
// exercising the terminal-send-failure return of finish().
type failTerminalStorage struct{ stream.Storage }

func (s failTerminalStorage) SetHistory(id string, evs []events.Event) error {
	for _, ev := range evs {
		// AsValue unwraps pointer and routed forms, so one case covers them.
		if _, ok := events.AsValue(ev).(events.TaskFailed); ok {
			return errors.New("storage down on TaskFailed")
		}
	}
	return s.Storage.SetHistory(id, evs)
}

// TestRunLifecycleTerminalSendFailureSurfaces pins that a terminal the stream
// refuses is reported to the owner, not swallowed.
func TestRunLifecycleTerminalSendFailureSurfaces(t *testing.T) {
	s := stream.NewMemoryStream(stream.WithStorage(failTerminalStorage{stream.NewMemoryStorage()}))
	lc := newRunLifecycle("worker", agotask.Spec{Title: "work"}, agocontext.New(s))
	if err := lc.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	lc.failure = errors.New("turn broke")
	if err := lc.finish(); err == nil {
		t.Fatal("finish succeeded despite the refused terminal")
	}
}

// TestPersistUsageAccountingNilSafety pins the defensive guards: nil contexts,
// streamless contexts and empty batches persist nothing and report no error.
func TestPersistUsageAccountingNilSafety(t *testing.T) {
	src := events.NewUsageEvent(events.Usage{TotalTokens: events.I64(1)}, events.UsageKindModelCall)
	if err := persistUsageAccounting(nil, []events.Event{src}); err != nil {
		t.Fatalf("nil context: %v", err)
	}
	if err := persistUsageAccounting(agocontext.New(nil), []events.Event{src}); err != nil {
		t.Fatalf("streamless context: %v", err)
	}
	s := stream.NewMemoryStream()
	if err := persistUsageAccounting(agocontext.New(s), nil); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
	if evs, _ := s.History().GetEvents(); len(evs) != 0 {
		t.Fatalf("stream holds %d events, want none", len(evs))
	}
}

// TestPersistUsageAccountingDeduplicatesByEventID pins the append-not-replace
// contract: records already on the transcript (or repeated inside the batch)
// are not appended twice, and the completed snapshot counts each once.
func TestPersistUsageAccountingDeduplicatesByEventID(t *testing.T) {
	s := stream.NewMemoryStream()
	onStream := events.NewUsageEvent(events.Usage{TotalTokens: events.I64(5)}, events.UsageKindModelCall)
	if err := s.Send(onStream); err != nil {
		t.Fatalf("seed: %v", err)
	}
	fresh := events.NewUsageEvent(events.Usage{TotalTokens: events.I64(3)}, events.UsageKindModelCall)
	lc := newRunLifecycle("worker", agotask.Spec{Title: "work"}, agocontext.New(s))
	if err := lc.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	lc.accounting = func() []events.Event { return []events.Event{onStream, fresh, fresh} }
	if err := lc.finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
	counts := map[string]int{}
	evs, err := s.History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	for _, ev := range evs {
		if record, ok := usage.EventFrom(ev); ok {
			counts[record.EventID]++
		}
	}
	if len(counts) != 2 {
		t.Fatalf("usage records = %v, want exactly the two distinct ids", counts)
	}
	for id, n := range counts {
		if n != 1 {
			t.Fatalf("usage record %s appended %d times, want once", id, n)
		}
	}
	te := collectTaskEvents(t, s)
	if len(te.completed) != 1 {
		t.Fatalf("completed = %d, want 1", len(te.completed))
	}
	if te.completed[0].Usage.TotalTokens == nil || *te.completed[0].Usage.TotalTokens != 8 {
		t.Fatalf("completed usage = %v, want 8 (5+3, no double count)", te.completed[0].Usage.TotalTokens)
	}
}

// TestCloseDerivesTerminalFromRunState pins the Close fallbacks in white box:
// an unset lifecycle failure falls back to the run's start error, and a
// missing reply on a cancelled turn context reports cancellation.
func TestCloseDerivesTerminalFromRunState(t *testing.T) {
	t.Run("start error", func(t *testing.T) {
		s := stream.NewMemoryStream()
		lc := newRunLifecycle("worker", agotask.Spec{Title: "work"}, agocontext.New(s))
		if err := lc.start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		run := &AgentRun{stream: s, lifecycle: lc, startErr: errors.New("no model")}
		run.Close()
		te := collectTaskEvents(t, s)
		if len(te.started) != 1 || len(te.failed) != 1 {
			t.Fatalf("lifecycle = started %d failed %d, want 1/1", len(te.started), len(te.failed))
		}
		if te.failed[0].Error != "no model" {
			t.Fatalf("terminal cause = %q, want the start error", te.failed[0].Error)
		}
	})
	t.Run("cancelled turn without reply", func(t *testing.T) {
		s := stream.NewMemoryStream()
		lc := newRunLifecycle("worker", agotask.Spec{Title: "work"}, agocontext.New(s))
		if err := lc.start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
		cancel()
		run := &AgentRun{stream: s, lifecycle: lc, turnCtx: ctx}
		run.Close()
		if got := taskLifecycleTypes(t, s); len(got) != 2 || got[0] != "started" || got[1] != "cancelled" {
			t.Fatalf("task events = %v, want started+cancelled", got)
		}
	})
}

// TestTaggedCompletedIsTheLastStreamEvent pins the terminal-last guarantee
// for the success path: after Close, TaskCompleted is the final event on the
// stream, so an observer never sees operation events past the terminal.
func TestTaggedCompletedIsTheLastStreamEvent(t *testing.T) {
	s := stream.NewMemoryStream()
	a := New("analyst", "", WithConfig(agotest.NewTestConfig(textResp("done"))), WithStreamBase(s))
	if _, err := a.AskWith(stdcontext.Background(), RunOptions{Capability: "analysis"}, "analyse"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	evs, err := s.History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(evs) == 0 {
		t.Fatal("stream holds no events")
	}
	if _, ok := events.AsValue(evs[len(evs)-1]).(events.TaskCompleted); !ok {
		t.Fatalf("last event = %T, want TaskCompleted", events.AsValue(evs[len(evs)-1]))
	}
}

// TestTaggedStartFailureWithoutClosePublishesTerminal pins the failed-scope
// guarantee of open(): when Start fails after the lifecycle opened and the
// caller never Closes, the terminal is still published and the per-stream lock
// released.
func TestTaggedStartFailureWithoutClosePublishesTerminal(t *testing.T) {
	s := stream.NewMemoryStream()
	a := New("analyst", "", WithStreamBase(s))
	run, err := a.RunWith(stdcontext.Background(), RunOptions{Capability: "analysis"}, "analyse")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := run.Start(stdcontext.Background()); err == nil {
		t.Fatal("start succeeded without a model config")
	}
	defer run.Close()
	if got := taskLifecycleTypes(t, s); len(got) != 2 || got[0] != "started" || got[1] != "failed" {
		t.Fatalf("task events = %v, want started+failed without Close", got)
	}
	next := New("next", "", WithConfig(agotest.NewTestConfig(textResp("ok"))), WithStreamBase(s))
	ctx, cancel := stdcontext.WithTimeout(stdcontext.Background(), 2*time.Second)
	defer cancel()
	if reply, err := next.Ask(ctx, "next"); err != nil || reply.Body() != "ok" {
		t.Fatalf("stream not released after the failed start: reply=%v err=%v", reply, err)
	}
}
