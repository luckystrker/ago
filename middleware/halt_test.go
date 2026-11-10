package middleware_test

import (
	stdcontext "context"
	"testing"

	"github.com/luckystrker/ago/agent"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
	agotest "github.com/luckystrker/ago/testing"
)

// halter is the assembly-side layer that emits a HaltEvent before the model call
// it is meant to stop (the role policies.AlertPolicy plays in a real chain).
type halter struct {
	middleware.Base
	reason string
}

func (h halter) OnLLMCall(next middleware.LLMCallFunc, evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
	if err := ctx.Send(&events.HaltEvent{Reason: h.reason}); err != nil {
		return nil, err
	}
	return next(evs, ctx)
}

// TestHaltCheckStopsTheSameModelCall pins the point of the middleware: a halt
// emitted inside one model call stops that call, before the model double is
// reached. The canary response would only appear if the halt leaked.
func TestHaltCheckStopsTheSameModelCall(t *testing.T) {
	cfg := agotest.NewTestConfig(&events.ModelResponse{
		Message: &events.ModelMessage{Content: "CANARY: the model was called"},
	})
	a := agent.New("guarded", "", agent.WithConfig(cfg),
		agent.WithMiddleware(
			func() middleware.Middleware { return halter{reason: "FATAL: blocked"} },
			middleware.HaltCheck(),
		))

	reply, err := a.Ask(stdcontext.Background(), "do the dangerous thing")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if got, want := reply.Body(), "HALTED: FATAL: blocked"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if calls := cfg.Calls(); len(calls) != 0 {
		t.Fatalf("the model was called %d time(s); the halt must short-circuit it", len(calls))
	}
}

// TestHaltCheckWithoutHaltPassesThrough pins the no-halt path: with nothing
// emitting a HaltEvent the model answers normally.
func TestHaltCheckWithoutHaltPassesThrough(t *testing.T) {
	cfg := agotest.NewTestConfig(&events.ModelResponse{
		Message: &events.ModelMessage{Content: "answered"},
	})
	a := agent.New("open", "", agent.WithConfig(cfg), agent.WithMiddleware(middleware.HaltCheck()))
	reply, err := a.Ask(stdcontext.Background(), "hello")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if reply.Body() != "answered" {
		t.Fatalf("body = %q, want answered", reply.Body())
	}
}

// TestHaltCheckSubscriptionIsTurnScoped pins the finally block: a HaltEvent that
// crosses the stream after the turn returned must not arm the middleware, so a
// later model call is not short-circuited.
func TestHaltCheckSubscriptionIsTurnScoped(t *testing.T) {
	conv := agocontext.New(stream.NewMemoryStream())
	mw := middleware.HaltCheck()()

	if _, err := mw.OnTurn(func(*events.ModelRequest, *agocontext.Context) (*events.ModelResponse, error) {
		return &events.ModelResponse{}, nil
	}, &events.ModelRequest{}, conv); err != nil {
		t.Fatalf("OnTurn: %v", err)
	}

	// The turn is over: a halt arriving now belongs to whoever runs next.
	if err := conv.Send(&events.HaltEvent{Reason: "late"}); err != nil {
		t.Fatalf("send halt: %v", err)
	}

	called := false
	resp, err := mw.OnLLMCall(func([]events.Event, *agocontext.Context) (*events.ModelResponse, error) {
		called = true
		return &events.ModelResponse{Message: &events.ModelMessage{Content: "model"}}, nil
	}, nil, conv)
	if err != nil {
		t.Fatalf("OnLLMCall: %v", err)
	}
	if !called {
		t.Fatal("the stale HaltEvent was still observed after the turn closed")
	}
	if resp.Message.Content != "model" {
		t.Fatalf("body = %q, want the model's answer", resp.Message.Content)
	}
}

// TestHaltCheckWithoutContext pins the defensive path: a nil conversation (no
// stream to observe) still runs the turn.
func TestHaltCheckWithoutContext(t *testing.T) {
	mw := middleware.HaltCheck()()
	resp, err := mw.OnTurn(func(*events.ModelRequest, *agocontext.Context) (*events.ModelResponse, error) {
		return &events.ModelResponse{Message: &events.ModelMessage{Content: "ok"}}, nil
	}, &events.ModelRequest{}, nil)
	if err != nil {
		t.Fatalf("OnTurn: %v", err)
	}
	if resp.Message.Content != "ok" {
		t.Fatalf("body = %q, want ok", resp.Message.Content)
	}
}
