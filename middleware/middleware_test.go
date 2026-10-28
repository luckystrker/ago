package middleware

import (
	"testing"

	agocontext "ago/context"
	"ago/events"
	"ago/stream"
)

func newTestCtx(t *testing.T) *agocontext.Context {
	t.Helper()
	return agocontext.New(stream.NewMemoryStream())
}

// recorder records the enter/exit of every hook it wraps.
type recorder struct {
	Base
	name string
	log  *[]string
}

func (r *recorder) enter(hook string) func() {
	*r.log = append(*r.log, hook+">:"+r.name)
	return func() { *r.log = append(*r.log, hook+"<:"+r.name) }
}

func (r *recorder) OnTurn(next TurnFunc, ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
	defer r.enter("turn")()
	return next(ev, ctx)
}

func (r *recorder) OnLLMCall(next LLMCallFunc, evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
	defer r.enter("llm")()
	return next(evs, ctx)
}

func (r *recorder) OnToolExecution(next ToolExecFunc, ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
	defer r.enter("tool")()
	return next(ev, ctx)
}

func (r *recorder) OnHumanInput(next HumanInputFunc, ev *events.HumanInputRequest, ctx *agocontext.Context) (*events.HumanMessage, error) {
	defer r.enter("human")()
	return next(ev, ctx)
}

func recorderFactory(name string, log *[]string) Factory {
	return func() Middleware { return &recorder{name: name, log: log} }
}

func TestBasePassesThrough(t *testing.T) {
	var m Middleware = Base{}
	ctx := newTestCtx(t)

	turnCalled := false
	resp, err := m.OnTurn(func(ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
		turnCalled = true
		return &events.ModelResponse{ResponseID: "r1"}, nil
	}, &events.ModelRequest{}, ctx)
	if !turnCalled || err != nil || resp.ResponseID != "r1" {
		t.Fatalf("OnTurn passthrough: called=%v resp=%v err=%v", turnCalled, resp, err)
	}

	llmCalled := false
	resp, err = m.OnLLMCall(func(evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
		llmCalled = true
		return &events.ModelResponse{ResponseID: "r2"}, nil
	}, []events.Event{}, ctx)
	if !llmCalled || err != nil || resp.ResponseID != "r2" {
		t.Fatalf("OnLLMCall passthrough: called=%v resp=%v err=%v", llmCalled, resp, err)
	}

	toolCalled := false
	call := events.NewToolCallEvent("t")
	res, err := m.OnToolExecution(func(ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
		toolCalled = true
		return events.FromResultCall(*ev, events.NewToolResult(false, "ok")), nil
	}, &call, ctx)
	if !toolCalled || err != nil || res == nil {
		t.Fatalf("OnToolExecution passthrough: called=%v res=%v err=%v", toolCalled, res, err)
	}

	humanCalled := false
	msg, err := m.OnHumanInput(func(ev *events.HumanInputRequest, ctx *agocontext.Context) (*events.HumanMessage, error) {
		humanCalled = true
		return &events.HumanMessage{Content: "ans"}, nil
	}, &events.HumanInputRequest{}, ctx)
	if !humanCalled || err != nil || msg.Content != "ans" {
		t.Fatalf("OnHumanInput passthrough: called=%v msg=%v err=%v", humanCalled, msg, err)
	}
}

// TestChainOrder checks onion ordering for all four hooks: Chain(a, b, c) enters
// a → b → c and exits c → b → a.
func TestChainOrder(t *testing.T) {
	log := []string{}
	c := Chain(recorderFactory("a", &log), recorderFactory("b", &log), recorderFactory("c", &log))()
	ctx := newTestCtx(t)

	t.Run("OnTurn", func(t *testing.T) {
		log = nil
		_, err := c.OnTurn(func(ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
			return &events.ModelResponse{}, nil
		}, &events.ModelRequest{}, ctx)
		if err != nil {
			t.Fatalf("OnTurn: %v", err)
		}
		want := []string{"turn>:a", "turn>:b", "turn>:c", "turn<:c", "turn<:b", "turn<:a"}
		if !equalStrs(log, want) {
			t.Fatalf("order = %v, want %v", log, want)
		}
	})

	t.Run("OnLLMCall", func(t *testing.T) {
		log = nil
		_, err := c.OnLLMCall(func(evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
			return &events.ModelResponse{}, nil
		}, []events.Event{events.TextInput{Content: "hi"}}, ctx)
		if err != nil {
			t.Fatalf("OnLLMCall: %v", err)
		}
		want := []string{"llm>:a", "llm>:b", "llm>:c", "llm<:c", "llm<:b", "llm<:a"}
		if !equalStrs(log, want) {
			t.Fatalf("order = %v, want %v", log, want)
		}
	})

	t.Run("OnToolExecution", func(t *testing.T) {
		log = nil
		call := events.NewToolCallEvent("t")
		_, err := c.OnToolExecution(func(ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
			return events.FromResultCall(*ev, events.NewToolResult(false, "ok")), nil
		}, &call, ctx)
		if err != nil {
			t.Fatalf("OnToolExecution: %v", err)
		}
		want := []string{"tool>:a", "tool>:b", "tool>:c", "tool<:c", "tool<:b", "tool<:a"}
		if !equalStrs(log, want) {
			t.Fatalf("order = %v, want %v", log, want)
		}
	})

	t.Run("OnHumanInput", func(t *testing.T) {
		log = nil
		_, err := c.OnHumanInput(func(ev *events.HumanInputRequest, ctx *agocontext.Context) (*events.HumanMessage, error) {
			return &events.HumanMessage{Content: "ans"}, nil
		}, &events.HumanInputRequest{}, ctx)
		if err != nil {
			t.Fatalf("OnHumanInput: %v", err)
		}
		want := []string{"human>:a", "human>:b", "human>:c", "human<:c", "human<:b", "human<:a"}
		if !equalStrs(log, want) {
			t.Fatalf("order = %v, want %v", log, want)
		}
	})
}

// turnOnly overrides a single hook; every other hook must stay a Base passthrough
// (no log entries).
type turnOnly struct {
	Base
	log *[]string
}

func (m *turnOnly) OnTurn(next TurnFunc, ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
	*m.log = append(*m.log, "turn")
	return next(ev, ctx)
}

func TestChainPartialOverride(t *testing.T) {
	log := []string{}
	c := Chain(func() Middleware { return &turnOnly{log: &log} }, recorderFactory("r", &log))()
	ctx := newTestCtx(t)

	_, err := c.OnLLMCall(func(evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
		return &events.ModelResponse{}, nil
	}, nil, ctx)
	if err != nil {
		t.Fatalf("OnLLMCall: %v", err)
	}
	// turnOnly has no OnLLMCall override: Base passes through to the recorder
	// unchanged, and turnOnly records nothing.
	if !equalStrs(log, []string{"llm>:r", "llm<:r"}) {
		t.Fatalf("unoverridden hook must be a plain passthrough, got %v", log)
	}
	log = nil
	_, err = c.OnTurn(func(ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
		return &events.ModelResponse{}, nil
	}, &events.ModelRequest{}, ctx)
	if err != nil {
		t.Fatalf("OnTurn: %v", err)
	}
	if !equalStrs(log, []string{"turn", "turn>:r", "turn<:r"}) {
		t.Fatalf("order = %v", log)
	}
}

func TestChainEmpty(t *testing.T) {
	called := false
	c := Chain()()
	ctx := newTestCtx(t)
	_, err := c.OnTurn(func(ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
		called = true
		return &events.ModelResponse{}, nil
	}, &events.ModelRequest{}, ctx)
	if err != nil || !called {
		t.Fatalf("empty chain must call next: called=%v err=%v", called, err)
	}
}

// TestChainInstantiatesPerTurn checks that every Factory() call produces fresh
// instances (per-turn state does not leak across turns).
func TestChainInstantiatesPerTurn(t *testing.T) {
	f := Chain(recorderFactory("a", &[]string{}))
	if f() == f() {
		t.Fatal("each Factory() call must build a new middleware instance")
	}
}

func TestIfAppliesInnerOnMatch(t *testing.T) {
	log := []string{}
	// Match only requests carrying at least one part.
	cond := func(ev events.Event) bool {
		req, ok := ev.(*events.ModelRequest)
		return ok && len(req.Parts) > 0
	}
	c := If(cond, recorderFactory("inner", &log))()
	ctx := newTestCtx(t)
	next := func(ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
		return &events.ModelResponse{}, nil
	}

	if _, err := c.OnTurn(next, events.EnsureRequest("hello"), ctx); err != nil {
		t.Fatalf("OnTurn: %v", err)
	}
	if !equalStrs(log, []string{"turn>:inner", "turn<:inner"}) {
		t.Fatalf("matching event must run the inner middleware, got %v", log)
	}

	log = nil
	if _, err := c.OnTurn(next, &events.ModelRequest{}, ctx); err != nil {
		t.Fatalf("OnTurn: %v", err)
	}
	if len(log) != 0 {
		t.Fatalf("non-matching event must skip the inner middleware, got %v", log)
	}
}

func TestIfPerHookConditions(t *testing.T) {
	ctx := newTestCtx(t)

	t.Run("OnLLMCall last event", func(t *testing.T) {
		log := []string{}
		c := If(events.TypeOf[events.ToolResultsEvent](), recorderFactory("inner", &log))()
		next := func(evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
			return &events.ModelResponse{}, nil
		}
		if _, err := c.OnLLMCall(next, []events.Event{events.TextInput{Content: "q"}, events.ToolResultsEvent{}}, ctx); err != nil {
			t.Fatalf("OnLLMCall: %v", err)
		}
		if !equalStrs(log, []string{"llm>:inner", "llm<:inner"}) {
			t.Fatalf("last event matches: got %v", log)
		}
		log = nil
		if _, err := c.OnLLMCall(next, []events.Event{events.TextInput{Content: "q"}}, ctx); err != nil {
			t.Fatalf("OnLLMCall: %v", err)
		}
		if len(log) != 0 {
			t.Fatalf("last event does not match: got %v", log)
		}
	})

	t.Run("OnToolExecution", func(t *testing.T) {
		log := []string{}
		c := If(events.Equal(toolCallName, "risky"), recorderFactory("inner", &log))()
		next := func(ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
			return events.FromResultCall(*ev, events.NewToolResult(false, "ok")), nil
		}
		risky := events.NewToolCallEvent("risky")
		safe := events.NewToolCallEvent("safe")
		if _, err := c.OnToolExecution(next, &risky, ctx); err != nil {
			t.Fatalf("OnToolExecution: %v", err)
		}
		if !equalStrs(log, []string{"tool>:inner", "tool<:inner"}) {
			t.Fatalf("matching call: got %v", log)
		}
		log = nil
		if _, err := c.OnToolExecution(next, &safe, ctx); err != nil {
			t.Fatalf("OnToolExecution: %v", err)
		}
		if len(log) != 0 {
			t.Fatalf("non-matching call: got %v", log)
		}
	})

	t.Run("OnHumanInput", func(t *testing.T) {
		log := []string{}
		c := If(events.Equal(humanContent, "secret"), recorderFactory("inner", &log))()
		next := func(ev *events.HumanInputRequest, ctx *agocontext.Context) (*events.HumanMessage, error) {
			return &events.HumanMessage{Content: "ans"}, nil
		}
		if _, err := c.OnHumanInput(next, &events.HumanInputRequest{Content: "secret"}, ctx); err != nil {
			t.Fatalf("OnHumanInput: %v", err)
		}
		if !equalStrs(log, []string{"human>:inner", "human<:inner"}) {
			t.Fatalf("matching request: got %v", log)
		}
		log = nil
		if _, err := c.OnHumanInput(next, &events.HumanInputRequest{Content: "hello"}, ctx); err != nil {
			t.Fatalf("OnHumanInput: %v", err)
		}
		if len(log) != 0 {
			t.Fatalf("non-matching request: got %v", log)
		}
	})
}

func TestIfNilConditionAlwaysApplies(t *testing.T) {
	log := []string{}
	c := If(nil, recorderFactory("inner", &log))()
	ctx := newTestCtx(t)
	_, err := c.OnTurn(func(ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
		return &events.ModelResponse{}, nil
	}, &events.ModelRequest{}, ctx)
	if err != nil {
		t.Fatalf("OnTurn: %v", err)
	}
	if !equalStrs(log, []string{"turn>:inner", "turn<:inner"}) {
		t.Fatalf("nil condition must always apply the inner middleware, got %v", log)
	}
}

func TestIfInstantiatesInnerPerTurn(t *testing.T) {
	f := If(func(events.Event) bool { return true }, recorderFactory("a", &[]string{}))
	if f() == f() {
		t.Fatal("each Factory() call must build a new conditional instance")
	}
}

func toolCallName(ev events.Event) string {
	switch c := ev.(type) {
	case *events.ToolCallEvent:
		return c.Name
	case events.ToolCallEvent:
		return c.Name
	}
	return ""
}

func humanContent(ev events.Event) string {
	switch r := ev.(type) {
	case *events.HumanInputRequest:
		return r.Content
	case events.HumanInputRequest:
		return r.Content
	}
	return ""
}

func equalStrs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
