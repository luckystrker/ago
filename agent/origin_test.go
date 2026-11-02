package agent

import (
	stdcontext "context"
	"sync"
	"testing"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
)

func userText(s string) []events.Input {
	return []events.Input{&events.TextInput{Content: s}}
}

// TestMergeRequestsOriginRule pins the drain rule: the merged origin is user
// when any merged request was user-origin (the "public request wins" rule —
// Python parity for _drain_pending); an empty origin counts as user (the
// Python field default).
func TestMergeRequestsOriginRule(t *testing.T) {
	mk := func(o events.RequestOrigin) *events.ModelRequest { return &events.ModelRequest{Origin: o} }
	tests := []struct {
		name string
		in   []*events.ModelRequest
		want events.RequestOrigin
	}{
		{"empty keeps zero origin", nil, ""},
		{"service stays service", []*events.ModelRequest{mk(events.OriginService)}, events.OriginService},
		{"user stays user", []*events.ModelRequest{mk(events.OriginUser)}, events.OriginUser},
		{"legacy empty counts user", []*events.ModelRequest{mk("")}, events.OriginUser},
		{"any user wins over service", []*events.ModelRequest{mk(events.OriginService), mk(events.OriginUser)}, events.OriginUser},
		{"legacy empty wins over service", []*events.ModelRequest{mk(events.OriginService), mk("")}, events.OriginUser},
		{"all service stays service", []*events.ModelRequest{mk(events.OriginService), mk(events.OriginService)}, events.OriginService},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := mergeRequests(tc.in).Origin; got != tc.want {
				t.Fatalf("merged origin = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAskStampsUserOriginAndOpensInterval pins the end-to-end interval
// semantics: every public ask enters as a user-origin request and the follow-up
// ask re-opens the interval — since-last-user-input sees only the latest turn.
func TestAskStampsUserOriginAndOpensInterval(t *testing.T) {
	cfg := agotest.NewTestConfig(textResp("one"), textResp("two"))
	a := New("a", "", WithConfig(cfg))
	r1, err := a.Ask(stdcontext.Background(), "1")
	if err != nil {
		t.Fatalf("first ask: %v", err)
	}
	r2, err := r1.Ask(stdcontext.Background(), "2")
	if err != nil {
		t.Fatalf("second ask: %v", err)
	}
	h := r2.History()
	reqs, err := h.Where(events.TypeOf[events.ModelRequest]()).All()
	if err != nil || len(reqs) != 2 {
		t.Fatalf("requests = %d %v, want 2", len(reqs), err)
	}
	for i, ev := range reqs {
		if mr := events.AsValue(ev).(events.ModelRequest); mr.Origin != events.OriginUser {
			t.Fatalf("request[%d].Origin = %q, want user (public input)", i, mr.Origin)
		}
	}
	// Without the scope both responses match.
	all, err := h.Where(events.TypeOf[events.ModelResponse]()).All()
	if err != nil || len(all) != 2 {
		t.Fatalf("all responses = %d %v, want 2", len(all), err)
	}
	// The second user request is the boundary: only its turn is in scope.
	scoped, err := h.Where(events.TypeOf[events.ModelResponse]()).SinceLastUserInput().All()
	if err != nil {
		t.Fatalf("scoped all: %v", err)
	}
	if len(scoped) != 1 || events.AsValue(scoped[0]).(events.ModelResponse).Content() != "two" {
		t.Fatalf("scoped responses = %#v, want only the second turn", scoped)
	}
	if n, err := h.Where(events.TypeOf[events.ModelResponse]()).SinceLastUserInput().Count(); n != 1 || err != nil {
		t.Fatalf("scoped count = %d %v, want 1", n, err)
	}
}

// TestMidTurnUserEnqueueOpensInterval pins the mid-turn merge: input enqueued
// through AgentRun.Enqueue carries the user origin into the DrainedModelRequest
// and re-opens the user interval.
func TestMidTurnUserEnqueueOpensInterval(t *testing.T) {
	var run *AgentRun
	emit := func(args struct{}, cx *agocontext.Context) (string, error) {
		if err := run.Enqueue(userText("mid-turn")...); err != nil {
			return "", err
		}
		return "ok", nil
	}
	tool, err := tools.NewFunctionTool("emit", "Emit mid-turn input", emit)
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	cfg := agotest.NewTestConfig(toolResp("emit", "{}"), textResp("done"))
	a := New("a", "", WithConfig(cfg), WithTools(tool))
	run, err = a.Run(stdcontext.Background(), "go")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	defer run.Close()
	if _, err := run.Result(stdcontext.Background()); err != nil {
		t.Fatalf("result: %v", err)
	}
	h := run.Stream().History()

	drained, ok, err := h.Where(events.TypeOf[events.DrainedModelRequest]()).First()
	if err != nil || !ok {
		t.Fatalf("drained merge missing: ok=%v err=%v", ok, err)
	}
	if dm := events.AsValue(drained).(events.DrainedModelRequest); dm.Origin != events.OriginUser {
		t.Fatalf("drained origin = %q, want user (AgentRun.Enqueue)", dm.Origin)
	}
	// The drained user request is the new boundary: only the second model
	// call's response is in scope, the initial "go" request is not.
	resps, err := h.Where(events.TypeOf[events.ModelResponse]()).SinceLastUserInput().All()
	if err != nil {
		t.Fatalf("scoped responses: %v", err)
	}
	if len(resps) != 1 || events.AsValue(resps[0]).(events.ModelResponse).Content() != "done" {
		t.Fatalf("scoped responses = %#v, want only %q", resps, "done")
	}
	reqs, err := h.Where(events.TypeOf[events.ModelRequest]()).SinceLastUserInput().All()
	if err != nil || len(reqs) != 0 {
		t.Fatalf("scoped requests = %d %v, want 0 (boundary excluded)", len(reqs), err)
	}
}

// TestMidTurnServiceEnqueueStaysInInterval pins the service side: a tool or
// background update enqueued through Context.Enqueue merges with the service
// origin and does not open a new user interval.
func TestMidTurnServiceEnqueueStaysInInterval(t *testing.T) {
	emit := func(args struct{}, cx *agocontext.Context) (string, error) {
		if err := cx.Enqueue(userText("tool-update")...); err != nil {
			return "", err
		}
		return "ok", nil
	}
	tool, err := tools.NewFunctionTool("emit", "Emit tool update", emit)
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	cfg := agotest.NewTestConfig(toolResp("emit", "{}"), textResp("done"))
	a := New("a", "", WithConfig(cfg), WithTools(tool))
	reply, err := a.Ask(stdcontext.Background(), "go")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	h := reply.History()

	drained, ok, err := h.Where(events.TypeOf[events.DrainedModelRequest]()).First()
	if err != nil || !ok {
		t.Fatalf("drained merge missing: ok=%v err=%v", ok, err)
	}
	if dm := events.AsValue(drained).(events.DrainedModelRequest); dm.Origin != events.OriginService {
		t.Fatalf("drained origin = %q, want service (Context.Enqueue)", dm.Origin)
	}
	// The boundary is still the initial user request: the drained service
	// merge and both responses stay inside the interval.
	inScope, err := h.Where(events.TypeOf[events.DrainedModelRequest]()).SinceLastUserInput().All()
	if err != nil || len(inScope) != 1 {
		t.Fatalf("scoped drained = %d %v, want 1 (service merge must not close the interval)", len(inScope), err)
	}
	if n, err := h.Where(events.TypeOf[events.ModelResponse]()).SinceLastUserInput().Count(); n != 2 || err != nil {
		t.Fatalf("scoped responses = %d %v, want 2", n, err)
	}
}

// finalFlushConfig enqueues a service-origin input while the first model call
// runs, so the turn picks it up in the final leftover flush (Python parity:
// the final drain in _execute_turn). It doubles as its own client.
type finalFlushConfig struct {
	mu    sync.Mutex
	calls int
}

func (c *finalFlushConfig) Provider() config.Provider { return "test" }
func (c *finalFlushConfig) Model() string             { return "test" }
func (c *finalFlushConfig) Create() (config.LLMClient, error) {
	return c, nil
}
func (c *finalFlushConfig) Clone(map[string]any) (config.ModelConfig, error) { return c, nil }

func (c *finalFlushConfig) Call(_ stdcontext.Context, _ []events.Event, cx *agocontext.Context, _ config.CallOptions) (*events.ModelResponse, error) {
	c.mu.Lock()
	n := c.calls
	c.calls++
	c.mu.Unlock()
	if n == 0 {
		// Enqueue strictly after the LLM trigger's own drain: only the final
		// flush can pick this up.
		if err := cx.Stream.Enqueue(userText("late")...); err != nil {
			return nil, err
		}
		return textResp("first"), nil
	}
	return textResp("second"), nil
}

// TestFinalServiceDrainDoesNotOpenInterval pins the turn end: leftover input
// flushed after the response carries the service origin and stays inside the
// interval the ask opened, and the flushed input gets its own model call whose
// answer is the reply body.
func TestFinalServiceDrainDoesNotOpenInterval(t *testing.T) {
	a := New("a", "", WithConfig(&finalFlushConfig{}))
	reply, err := a.Ask(stdcontext.Background(), "go")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	// The leftover flush is a real extra model call: the reply body is that
	// call's answer, not an empty turn.
	if body := reply.Body(); body != "second" {
		t.Fatalf("reply body = %q, want %q (the answer to the flushed input)", body, "second")
	}
	h := reply.History()
	// The flush re-emits a plain ModelRequest (not a DrainedModelRequest) with
	// the merged service origin.
	reqs, err := h.Where(events.TypeOf[events.ModelRequest]()).All()
	if err != nil || len(reqs) != 2 {
		t.Fatalf("requests = %d %v, want 2", len(reqs), err)
	}
	flushed := events.AsValue(reqs[1]).(events.ModelRequest)
	if flushed.Origin != events.OriginService {
		t.Fatalf("flushed origin = %q, want service (final drain default)", flushed.Origin)
	}
	// The final drain is not a boundary: it stays inside the user interval.
	inScope, err := h.Where(events.TypeOf[events.ModelRequest]()).SinceLastUserInput().All()
	if err != nil || len(inScope) != 1 {
		t.Fatalf("scoped requests = %d %v, want 1 (flush must not open an interval)", len(inScope), err)
	}
	if n, err := h.Where(events.TypeOf[events.ModelResponse]()).SinceLastUserInput().Count(); n != 2 || err != nil {
		t.Fatalf("scoped responses = %d %v, want 2", n, err)
	}
}

// TestResumePreservesRequestOrigin pins the resume re-entry: a ModelRequest
// trigger enters the inbox with its recorded origin — service stays service,
// and the service default of a plain enqueue would silently drop a user (or
// legacy) re-entry out of the user interval.
func TestResumePreservesRequestOrigin(t *testing.T) {
	tests := []struct {
		name string
		last events.ModelRequest
		want events.RequestOrigin
	}{
		{"user preserved", events.ModelRequest{Origin: events.OriginUser, Parts: userText("again")}, events.OriginUser},
		{"legacy counts user", events.ModelRequest{Parts: userText("again")}, events.OriginUser},
		{"service preserved", events.ModelRequest{Origin: events.OriginService, Parts: userText("again")}, events.OriginService},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			seed := []events.Event{
				&events.ModelRequest{Origin: events.OriginUser, Parts: userText("go")},
				&events.ModelResponse{Message: &events.ModelMessage{Content: "prior"}},
				&tc.last,
			}
			a := New("a", "", WithConfig(agotest.NewTestConfig(textResp("resumed"))))
			reply, err := a.Resume(stdcontext.Background(), seed...)
			if err != nil {
				t.Fatalf("resume: %v", err)
			}
			h := reply.History()
			reqs, err := h.Where(events.TypeOf[events.ModelRequest]()).All()
			if err != nil || len(reqs) != 2 {
				t.Fatalf("requests = %d %v, want seeded + re-entry", len(reqs), err)
			}
			got := events.AsValue(reqs[1]).(events.ModelRequest)
			if got.Origin != tc.want {
				t.Fatalf("re-entry origin = %q, want %q", got.Origin, tc.want)
			}
		})
	}
}
