package tools

import (
	stdcontext "context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/hitl"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
)

// collector records every event the stream dispatches.
type collector struct {
	mu  sync.Mutex
	evs []events.Event
}

func newCollector(s stream.Stream) *collector {
	c := &collector{}
	s.Subscribe(func(ev events.Event) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.evs = append(c.evs, ev)
		return nil
	}, stream.SubscribeOptions{})
	return c
}

func (c *collector) snapshot() []events.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]events.Event, len(c.evs))
	copy(out, c.evs)
	return out
}

func (c *collector) ofType(t *testing.T, want string) []events.Event {
	t.Helper()
	var out []events.Event
	for _, ev := range c.snapshot() {
		if events.AsValue(ev).EventType() == want {
			out = append(out, events.AsValue(ev))
		}
	}
	return out
}

func (c *collector) last(t *testing.T, want string) events.Event {
	t.Helper()
	evs := c.snapshot()
	if len(evs) == 0 {
		t.Fatalf("no events recorded")
	}
	last := events.AsValue(evs[len(evs)-1])
	if last.EventType() != want {
		t.Fatalf("last event = %s, want %s (all: %v)", last.EventType(), want, eventTypes(evs))
	}
	return last
}

func eventTypes(evs []events.Event) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = events.AsValue(ev).EventType()
	}
	return out
}

// partText renders the first result part as text, accepting value and pointer forms
// (events.FromErrorCall builds value TextInput parts, EnsureInput pointer ones).
func partText(p events.Input) string {
	switch v := events.AsValue(p).(type) {
	case events.TextInput:
		return v.Content
	case events.DataInput:
		return fmt.Sprintf("%v", v.Data)
	}
	return ""
}

type addArgs struct {
	A int `json:"a"`
	B int `json:"b"`
}

func TestExecutorExecutesCallsAndAnswersBatch(t *testing.T) {
	s, cx := newTestContext()
	rec := newCollector(s)
	ft := mustFunctionTool(t, "add", "", func(a addArgs) (string, error) {
		return fmt.Sprintf("%d", a.A+a.B), nil
	})
	ex := NewExecutor()
	if _, err := ex.Register(cx, []Tool{ft}, map[string]bool{"add": true}, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Send(events.ToolCallsEvent{Calls: []events.ToolCallEvent{
		{ID: "t1", Name: "add", Arguments: `{"a":2,"b":3}`},
	}}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// The call event was forwarded to the stream, then the batch settled.
	sawCall := false
	for _, ev := range rec.snapshot() {
		if call, ok := events.AsValue(ev).(events.ToolCallEvent); ok && call.ID == "t1" {
			sawCall = true
		}
	}
	if !sawCall {
		t.Fatalf("ToolCallEvent was not sent; events: %v", eventTypes(rec.snapshot()))
	}
	results := rec.last(t, "ToolResultsEvent").(events.ToolResultsEvent)
	if len(results.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(results.Results))
	}
	r := results.Results[0]
	if r.ParentID != "t1" || r.Name != "add" {
		t.Fatalf("result identity = %s/%s, want t1/add", r.ParentID, r.Name)
	}
	if got := partText(r.Result.Parts[0]); got != "5" {
		t.Fatalf("content = %q, want 5", got)
	}
}

func TestExecutorRunsCallsInParallel(t *testing.T) {
	s, cx := newTestContext()
	rec := newCollector(s)
	releaseA := make(chan struct{})
	bStarted := make(chan struct{})
	ftA := mustFunctionTool(t, "a", "", func() string {
		<-releaseA // blocks until b has started: proves parallel execution
		return "a"
	})
	ftB := mustFunctionTool(t, "b", "", func() string {
		close(bStarted)
		return "b"
	})
	ex := NewExecutor()
	if _, err := ex.Register(cx, []Tool{ftA, ftB}, map[string]bool{"a": true, "b": true}, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- s.Send(events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "1", Name: "a", Arguments: "{}"},
			{ID: "2", Name: "b", Arguments: "{}"},
		}})
	}()
	select {
	case <-bStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("call b never started while call a was blocked: execution is not parallel")
	}
	close(releaseA)
	if err := <-done; err != nil {
		t.Fatalf("Send: %v", err)
	}
	results := rec.last(t, "ToolResultsEvent").(events.ToolResultsEvent)
	if len(results.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(results.Results))
	}
	// Outcomes are gathered in call order regardless of completion order.
	if results.Results[0].ParentID != "1" || results.Results[1].ParentID != "2" {
		t.Fatalf("result order = %s,%s; want 1,2", results.Results[0].ParentID, results.Results[1].ParentID)
	}
}

func TestExecutorToolNotFoundFallback(t *testing.T) {
	s, cx := newTestContext()
	rec := newCollector(s)
	ex := NewExecutor()
	if _, err := ex.Register(cx, nil, map[string]bool{"known_func": true}, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Send(events.ToolCallEvent{ID: "tc_1", Name: "unknown_func", Arguments: "{}"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	notFound := rec.ofType(t, "ToolNotFoundEvent")
	if len(notFound) != 1 {
		t.Fatalf("not-found events = %d, want 1", len(notFound))
	}
	nf := notFound[0].(events.ToolNotFoundEvent)
	if nf.ParentID != "tc_1" || nf.Name != "unknown_func" {
		t.Fatalf("not-found identity = %s/%s", nf.ParentID, nf.Name)
	}
	if !strings.Contains(nf.Error, "Tool `unknown_func` not found") {
		t.Fatalf("error text = %q", nf.Error)
	}

	// A known name (registered or hosted) never triggers the fallback.
	before := len(rec.snapshot())
	if err := s.Send(events.ToolCallEvent{ID: "tc_2", Name: "known_func", Arguments: "{}"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	for _, ev := range rec.snapshot()[before:] {
		if events.AsValue(ev).EventType() == "ToolNotFoundEvent" {
			t.Fatalf("known name produced ToolNotFoundEvent")
		}
	}
}

func TestExecutorHitlFailureAbandonsBatchAndPropagates(t *testing.T) {
	s, cx := newTestContext() // no hitl hook: every request fails
	rec := newCollector(s)
	ftAsk := mustFunctionTool(t, "ask", "", func(a struct{}, cx *agocontext.Context) (string, error) {
		return cx.Input(stdcontext.Background(), "question?", 0)
	})
	ftSibling := mustFunctionTool(t, "sibling", "", func() string {
		time.Sleep(30 * time.Millisecond)
		return "ok"
	})
	ex := NewExecutor()
	if _, err := ex.Register(cx, []Tool{ftAsk, ftSibling}, map[string]bool{"ask": true, "sibling": true}, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	err := s.Send(events.ToolCallsEvent{Calls: []events.ToolCallEvent{
		{ID: "1", Name: "ask", Arguments: "{}"},
		{ID: "2", Name: "sibling", Arguments: "{}"},
	}})
	if !errors.Is(err, hitl.ErrNotProvided) {
		t.Fatalf("err = %v, want hitl.ErrNotProvided", err)
	}
	// The batch never settles: the model is not handed a tool answer for a
	// question that was never put.
	for _, ev := range rec.snapshot() {
		if events.AsValue(ev).EventType() == "ToolResultsEvent" {
			t.Fatalf("ToolResultsEvent sent after a hitl failure")
		}
	}
	sawCalls := 0
	for _, ev := range rec.snapshot() {
		if _, ok := events.AsValue(ev).(events.ToolCallEvent); ok {
			sawCalls++
		}
	}
	if sawCalls != 2 {
		t.Fatalf("call events sent = %d, want 2", sawCalls)
	}
}

func TestExecutorFinalTrueForcedModelResponse(t *testing.T) {
	t.Run("text part", func(t *testing.T) {
		s, cx := newTestContext()
		rec := newCollector(s)
		ft := mustFunctionTool(t, "finisher", "", func() events.ToolResult {
			return events.ToolResult{
				Parts:    []events.Input{events.TextInput{Content: "the end"}},
				Final:    true,
				Metadata: map[string]any{"why": "final"},
			}
		})
		ex := NewExecutor()
		if _, err := ex.Register(cx, []Tool{ft}, map[string]bool{"finisher": true}, nil); err != nil {
			t.Fatalf("Register: %v", err)
		}
		if err := s.Send(events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "1", Name: "finisher", Arguments: "{}"},
		}}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		mr := rec.last(t, "ModelResponse").(events.ModelResponse)
		if !mr.ResponseForce || mr.Message == nil || mr.Message.Content != "the end" {
			t.Fatalf("forced response = %+v", mr)
		}
		if mr.Message.Metadata["why"] != "final" {
			t.Fatalf("metadata lost: %+v", mr.Message.Metadata)
		}
		for _, ev := range rec.snapshot() {
			if events.AsValue(ev).EventType() == "ToolResultsEvent" {
				t.Fatalf("final=true must short-circuit the batch")
			}
		}
	})

	t.Run("data part is JSON encoded", func(t *testing.T) {
		s, cx := newTestContext()
		rec := newCollector(s)
		ft := mustFunctionTool(t, "finisher2", "", func() events.ToolResult {
			return events.ToolResult{
				Parts: []events.Input{events.DataInput{Data: map[string]any{"k": 1.0}}},
				Final: true,
			}
		})
		ex := NewExecutor()
		if _, err := ex.Register(cx, []Tool{ft}, map[string]bool{"finisher2": true}, nil); err != nil {
			t.Fatalf("Register: %v", err)
		}
		if err := s.Send(events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "1", Name: "finisher2", Arguments: "{}"},
		}}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		mr := rec.last(t, "ModelResponse").(events.ModelResponse)
		if mr.Message == nil || mr.Message.Content != `{"k":1}` {
			t.Fatalf("content = %v, want JSON of the data part", mr.Message)
		}
	})

	t.Run("multi-part final result is an error", func(t *testing.T) {
		s, cx := newTestContext()
		rec := newCollector(s)
		ft := mustFunctionTool(t, "greedy", "", func() events.ToolResult {
			return events.ToolResult{Parts: []events.Input{
				events.TextInput{Content: "a"}, events.TextInput{Content: "b"},
			}, Final: true}
		})
		ex := NewExecutor()
		if _, err := ex.Register(cx, []Tool{ft}, map[string]bool{"greedy": true}, nil); err != nil {
			t.Fatalf("Register: %v", err)
		}
		err := s.Send(events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "1", Name: "greedy", Arguments: "{}"},
		}})
		if err == nil || !strings.Contains(err.Error(), "exactly one part") {
			t.Fatalf("err = %v, want exactly-one-part error", err)
		}
		if len(rec.ofType(t, "ModelResponse")) != 0 {
			t.Fatalf("no forced response expected")
		}
	})

	t.Run("unsupported part type is an error", func(t *testing.T) {
		s, cx := newTestContext()
		ft := mustFunctionTool(t, "weird", "", func() events.ToolResult {
			return events.ToolResult{Parts: []events.Input{events.BinaryInput{Data: []byte{1}}}, Final: true}
		})
		ex := NewExecutor()
		if _, err := ex.Register(cx, []Tool{ft}, map[string]bool{"weird": true}, nil); err != nil {
			t.Fatalf("Register: %v", err)
		}
		err := s.Send(events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "1", Name: "weird", Arguments: "{}"},
		}})
		if err == nil || !strings.Contains(err.Error(), "unsupported final part") {
			t.Fatalf("err = %v, want unsupported-part error", err)
		}
	})
}

func TestExecutorClientToolForcedResponsePreservesCalls(t *testing.T) {
	s, cx := newTestContext()
	rec := newCollector(s)
	ct, err := NewClientTool(map[string]any{"name": "client_action"})
	if err != nil {
		t.Fatalf("NewClientTool: %v", err)
	}
	ex := NewExecutor()
	if _, err := ex.Register(cx, []Tool{ct}, map[string]bool{"client_action": true}, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	vendor := map[string]any{"provider": "test"}
	if err := s.Send(events.ToolCallsEvent{Calls: []events.ToolCallEvent{
		{ID: "cc1", Name: "client_action", Arguments: `{"x":1}`, VendorMetadata: vendor},
	}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	mr := rec.last(t, "ModelResponse").(events.ModelResponse)
	if !mr.ResponseForce {
		t.Fatalf("client hand-off must force the response")
	}
	if len(mr.ToolCalls.Calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(mr.ToolCalls.Calls))
	}
	c := mr.ToolCalls.Calls[0]
	if c.ID != "cc1" || c.Name != "client_action" || c.Arguments != `{"x":1}` {
		t.Fatalf("preserved call = %+v", c)
	}
	if c.VendorMetadata["provider"] != "test" {
		t.Fatalf("vendor metadata lost: %+v", c.VendorMetadata)
	}
}

func TestExecutorTimeout(t *testing.T) {
	s, cx := newTestContext()
	rec := newCollector(s)
	ft := mustFunctionTool(t, "sleepy", "", func() string {
		time.Sleep(200 * time.Millisecond)
		return "late"
	}, WithTimeout(50*time.Millisecond))
	ex := NewExecutor()
	if _, err := ex.Register(cx, []Tool{ft}, map[string]bool{"sleepy": true}, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Send(events.ToolCallsEvent{Calls: []events.ToolCallEvent{
		{ID: "1", Name: "sleepy", Arguments: "{}"},
	}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	results := rec.last(t, "ToolResultsEvent").(events.ToolResultsEvent)
	if len(results.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(results.Results))
	}
	tev := rec.ofType(t, "ToolErrorEvent")
	if len(tev) != 1 {
		t.Fatalf("want one ToolErrorEvent, got %v", eventTypes(rec.snapshot()))
	}
	timeoutEv := tev[0].(events.ToolErrorEvent)
	if timeoutEv.Error != "Tool 'sleepy' timed out after 0.05 seconds" {
		t.Fatalf("timeout text = %q", timeoutEv.Error)
	}
	if timeoutEv.ParentID != "1" || timeoutEv.Name != "sleepy" {
		t.Fatalf("timeout identity = %s/%s", timeoutEv.ParentID, timeoutEv.Name)
	}
}

func TestExecutorMiddlewareCanShortCircuit(t *testing.T) {
	s, cx := newTestContext()
	rec := newCollector(s)
	denied := &denyingMW{}
	ft := mustFunctionTool(t, "secret", "", func() string { return "granted" })
	ex := NewExecutor()
	if _, err := ex.Register(cx, []Tool{ft}, map[string]bool{"secret": true}, []middleware.Middleware{denied}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Send(events.ToolCallsEvent{Calls: []events.ToolCallEvent{
		{ID: "1", Name: "secret", Arguments: "{}"},
	}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	results := rec.last(t, "ToolResultsEvent").(events.ToolResultsEvent)
	if got := partText(results.Results[0].Result.Parts[0]); got != "denied by human" {
		t.Fatalf("middleware result not honored: %q", got)
	}
}

// denyingMW answers every call without invoking the tool.
type denyingMW struct{ middleware.Base }

func (d *denyingMW) OnToolExecution(next middleware.ToolExecFunc, ev *events.ToolCallEvent, _ *agocontext.Context) (events.Event, error) {
	return events.FromErrorCall(*ev, errors.New("denied by human")), nil
}

func TestExecutorSourceStampedOnHandledCall(t *testing.T) {
	s, cx := newTestContext()
	var gotSource string
	var mu sync.Mutex
	mw := &sourceMW{capture: &gotSource, mu: &mu}
	ft := mustFunctionTool(t, "stamped", "", func() string { return "" }, WithSource("function:app.ops:deploy:deploy"))
	ex := NewExecutor()
	if _, err := ex.Register(cx, []Tool{ft}, map[string]bool{"stamped": true}, []middleware.Middleware{mw}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Send(events.ToolCallsEvent{Calls: []events.ToolCallEvent{
		{ID: "1", Name: "stamped", Arguments: "{}"},
	}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotSource != "function:app.ops:deploy:deploy" {
		t.Fatalf("source seen by middleware = %q", gotSource)
	}
}

type sourceMW struct {
	middleware.Base
	capture *string
	mu      *sync.Mutex
}

func (m *sourceMW) OnToolExecution(next middleware.ToolExecFunc, ev *events.ToolCallEvent, _ *agocontext.Context) (events.Event, error) {
	m.mu.Lock()
	*m.capture = ev.Source
	m.mu.Unlock()
	return next(ev, nil)
}

func TestExecutorToolErrorForMiddlewareFailure(t *testing.T) {
	s, cx := newTestContext()
	rec := newCollector(s)
	ft := mustFunctionTool(t, "wrapped", "", func() string { return "" })
	ex := NewExecutor()
	if _, err := ex.Register(cx, []Tool{ft}, map[string]bool{"wrapped": true},
		[]middleware.Middleware{&failingMW{}}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Send(events.ToolCallsEvent{Calls: []events.ToolCallEvent{
		{ID: "1", Name: "wrapped", Arguments: "{}"},
	}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	results := rec.last(t, "ToolResultsEvent").(events.ToolResultsEvent)
	if got := partText(results.Results[0].Result.Parts[0]); got != "mw failed" {
		t.Fatalf("middleware error not converted: %q", got)
	}
}

type failingMW struct{ middleware.Base }

func (f *failingMW) OnToolExecution(_ middleware.ToolExecFunc, _ *events.ToolCallEvent, _ *agocontext.Context) (events.Event, error) {
	return nil, errors.New("mw failed")
}

// regFailTool fails its own registration; used to pin the unwind contract.
type regFailTool struct{}

func (regFailTool) Name() string         { return "reg_fail" }
func (regFailTool) DeclaredInCode() bool { return true }
func (regFailTool) Schemas(*agocontext.Context) ([]any, error) {
	return []any{Schema{Name: "reg_fail"}}, nil
}
func (regFailTool) Register(*agocontext.Context, []middleware.Middleware) ([]stream.SubID, error) {
	return nil, errors.New("register failed")
}

// TestExecutorRegisterReturnsSubscriptionsForTurnTeardown pins the turn-scope
// contract (docs/DECISIONS.md D-032): Register reports the ids of everything it
// subscribed so the caller can tear the turn down before the next registration on
// the same stream; without the teardown a second registration would execute every
// call twice (stale handlers from the first registration are still live).
func TestExecutorRegisterReturnsSubscriptionsForTurnTeardown(t *testing.T) {
	s, cx := newTestContext()
	rec := newCollector(s)
	var executions int
	ft := mustFunctionTool(t, "count", "", func() string {
		executions++
		return "ok"
	})
	ex := NewExecutor()

	// Turn 1.
	subs, err := ex.Register(cx, []Tool{ft}, map[string]bool{"count": true}, nil)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	// The batch handler, the tool's own subscription and the not-found fallback.
	if len(subs) != 3 {
		t.Fatalf("subscriptions = %d, want 3", len(subs))
	}
	if err := s.Send(events.ToolCallsEvent{Calls: []events.ToolCallEvent{
		{ID: "1", Name: "count", Arguments: "{}"},
	}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if executions != 1 {
		t.Fatalf("turn 1 executions = %d, want 1", executions)
	}

	// Turn scope teardown, then the next turn re-registers on the same stream.
	for _, id := range subs {
		s.Unsubscribe(id)
	}
	executions = 0
	if _, err := ex.Register(cx, []Tool{ft}, map[string]bool{"count": true}, nil); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if err := s.Send(events.ToolCallsEvent{Calls: []events.ToolCallEvent{
		{ID: "2", Name: "count", Arguments: "{}"},
	}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if executions != 1 {
		t.Fatalf("turn 2 executions = %d, want 1 (stale handler from turn 1 fired)", executions)
	}
	// One settled batch per turn: the stale batch handler from turn 1 must not
	// produce a second ToolResultsEvent for turn 2's calls.
	results := rec.ofType(t, "ToolResultsEvent")
	if len(results) != 2 {
		t.Fatalf("ToolResultsEvent total = %d, want 2 (one per turn)", len(results))
	}
	if last := results[1].(events.ToolResultsEvent); len(last.Results) != 1 || last.Results[0].ParentID != "2" {
		t.Fatalf("turn 2 batch = %+v, want the single result for call 2", last)
	}
}

// TestExecutorRegisterErrorLeavesNothingSubscribed pins the unwind contract: a
// failing tool registration removes the batch handler and any tools already
// registered, so the stream stays silent (Python ExitStack parity).
func TestExecutorRegisterErrorLeavesNothingSubscribed(t *testing.T) {
	s, cx := newTestContext()
	rec := newCollector(s)
	ft := mustFunctionTool(t, "fine", "", func() string { return "ok" })
	ex := NewExecutor()
	if _, err := ex.Register(cx, []Tool{ft, regFailTool{}},
		map[string]bool{"fine": true, "reg_fail": true}, nil); err == nil {
		t.Fatal("Register succeeded, want the reg_fail tool's error")
	}
	before := len(rec.snapshot())
	if err := s.Send(events.ToolCallsEvent{Calls: []events.ToolCallEvent{
		{ID: "1", Name: "fine", Arguments: "{}"},
	}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// Only the sent event itself may appear: no forwarded call, no settlement —
	// the batch handler and the tool's subscription are gone.
	for _, ev := range rec.snapshot()[before:] {
		switch events.AsValue(ev).EventType() {
		case "ToolCallEvent", "ToolResultEvent", "ToolErrorEvent", "ToolNotFoundEvent", "ToolResultsEvent":
			t.Fatalf("failed registration left handlers live: %s flowed", events.AsValue(ev).EventType())
		}
	}
}
