package plugin_test

// Tests live in an external package on purpose: they drive the agent
// integration (agent.WithPlugin) end to end, which the library package itself
// must not import (plugin never imports agent — no import cycle).

import (
	stdcontext "context"
	"strings"
	"sync"
	"testing"

	"ago/agent"
	"ago/config"
	agocontext "ago/context"
	"ago/events"
	"ago/middleware"
	"ago/plugin"
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

// promptSpy is a model double recording the system prompt each call was handed
// (conv.PromptLines becomes the system message).
type promptSpy struct {
	mu      sync.Mutex
	prompts [][]string
}

func (p *promptSpy) Provider() config.Provider                        { return "test" }
func (p *promptSpy) Model() string                                    { return "test" }
func (p *promptSpy) Create() (config.LLMClient, error)                { return p, nil }
func (p *promptSpy) Clone(map[string]any) (config.ModelConfig, error) { return p, nil }
func (p *promptSpy) Call(_ stdcontext.Context, _ []events.Event, conv *agocontext.Context, _ config.CallOptions) (*events.ModelResponse, error) {
	p.mu.Lock()
	p.prompts = append(p.prompts, conv.PromptLines())
	p.mu.Unlock()
	return textResp("ok"), nil
}

func (p *promptSpy) recorded() [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]string, len(p.prompts))
	copy(out, p.prompts)
	return out
}

// recorder middleware logs its walk through the turn onion.
type recMW struct {
	middleware.Base
	name  string
	mu    *sync.Mutex
	order *[]string
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

func recordingMW(name string, mu *sync.Mutex, order *[]string) middleware.Factory {
	return func() middleware.Middleware { return &recMW{name: name, mu: mu, order: order} }
}

func TestBundleToPlugin(t *testing.T) {
	tool, err := tools.NewFunctionTool("p_tool", "Plugin tool", func() (string, error) { return "ok", nil })
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	hook := func(*agocontext.Context) ([]string, error) { return []string{"dyn"}, nil }

	b := plugin.NewBundle("myplug").
		WithTool(tool).
		WithMiddleware(func() middleware.Middleware { return middleware.Base{} }).
		WithPrompt("static-1").
		WithPrompt("static-2").
		WithPromptHook(hook)
	p := b.Plugin()

	if p.Name() != "myplug" {
		t.Fatalf("name: %q", p.Name())
	}
	gotTools := p.Tools()
	if len(gotTools) != 1 || gotTools[0].Name() != "p_tool" {
		t.Fatalf("tools: %d entries", len(gotTools))
	}
	if len(p.Middleware()) != 1 {
		t.Fatalf("middleware: %d entries", len(p.Middleware()))
	}
	if got := strings.Join(p.PromptFragments(), ","); got != "static-1,static-2" {
		t.Fatalf("fragments: %q", got)
	}
	gotHooks := p.PromptHooks()
	if len(gotHooks) != 1 {
		t.Fatalf("hooks: %d entries", len(gotHooks))
	}
	fr, err := gotHooks[0](nil)
	if err != nil || strings.Join(fr, ",") != "dyn" {
		t.Fatalf("hook output: %q, err %v", fr, err)
	}

	// Accessors hand out copies: mutating a returned slice must not reach the
	// bundle (documented Plugin contract).
	frags := p.PromptFragments()
	frags[0] = "mutated"
	if p.PromptFragments()[0] != "static-1" {
		t.Fatal("PromptFragments leaked the internal slice")
	}

	// Repeated Plugin() calls return the same Plugin.
	if b.Plugin() != p {
		t.Fatal("Plugin() is not identity-preserving")
	}
}

func TestEmptyBundleIsHarmless(t *testing.T) {
	empty := plugin.NewBundle("empty").Plugin()
	if empty.Name() != "empty" {
		t.Fatalf("name: %q", empty.Name())
	}
	if len(empty.Tools()) != 0 || len(empty.Middleware()) != 0 ||
		len(empty.PromptFragments()) != 0 || len(empty.PromptHooks()) != 0 {
		t.Fatal("empty bundle contributed something")
	}

	// A typed-nil bundle is a valid zero Plugin: every accessor is nil-safe.
	var nilBundle *plugin.Bundle
	zero := nilBundle.Plugin()
	if zero.Name() != "" {
		t.Fatalf("zero name: %q", zero.Name())
	}
	if len(zero.Tools()) != 0 || len(zero.Middleware()) != 0 ||
		len(zero.PromptFragments()) != 0 || len(zero.PromptHooks()) != 0 {
		t.Fatal("zero Plugin contributed something")
	}
}

// TestAgentSeesPluginTools pins the tools leg of the plugin contract: the
// plugin's tool joins the agent's tool pool and answers a real tool_calls turn.
func TestAgentSeesPluginTools(t *testing.T) {
	executed := ""
	weather := func(args struct {
		City string `json:"city" agschema:"description=City name,required"`
	}) (string, error) {
		executed = args.City
		return "sunny in " + args.City, nil
	}
	tool, err := tools.NewFunctionTool("plugin_weather", "Get the weather", weather)
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	bundle := plugin.NewBundle("weather").WithTool(tool)

	cfg := agotest.NewTestConfig(
		toolResp("plugin_weather", `{"city":"Moscow"}`),
		textResp("The weather is sunny."),
	)
	a := agent.New("assistant", "", agent.WithConfig(cfg), agent.WithPlugin(bundle.Plugin()))

	// The tool is visible on the agent's static pool (spec parity surface).
	pool := a.Tools()
	if len(pool) != 1 || pool[0].Name() != "plugin_weather" {
		t.Fatalf("agent tools: %d entries", len(pool))
	}

	reply, err := a.Ask(stdcontext.Background(), "what is the weather in Moscow?")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if executed != "Moscow" {
		t.Fatalf("plugin tool executed with %q", executed)
	}
	if reply.Body() != "The weather is sunny." {
		t.Fatalf("body: %q", reply.Body())
	}
}

// TestAgentPluginMiddlewareOrder pins the middleware leg: plugin factories are
// appended after the agent's own, which places them innermost (Python parity:
// _apply_plugin routes through add_middleware — appended runs closest to the
// model).
func TestAgentPluginMiddlewareOrder(t *testing.T) {
	var mu sync.Mutex
	var order []string

	bundle := plugin.NewBundle("mw").
		WithMiddleware(recordingMW("plugA", &mu, &order)).
		WithMiddleware(recordingMW("plugB", &mu, &order))
	cfg := agotest.NewTestConfig(textResp("done"))
	a := agent.New("a", "", agent.WithConfig(cfg),
		agent.WithMiddleware(recordingMW("own", &mu, &order)),
		agent.WithPlugin(bundle.Plugin()))

	reply, err := a.Ask(stdcontext.Background(), "go")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if reply.Body() != "done" {
		t.Fatalf("body: %q", reply.Body())
	}
	// OnTurn: own outermost, then plugA, plugB innermost; OnLLMCall nests the
	// same way around the client call (first wraps second wraps client). Within
	// the plugin, bundle order is preserved.
	want := []string{
		"own:on_turn:in", "plugA:on_turn:in", "plugB:on_turn:in",
		"own:on_llm:in", "plugA:on_llm:in", "plugB:on_llm:in",
		"plugB:on_llm:out", "plugA:on_llm:out", "own:on_llm:out",
		"plugB:on_turn:out", "plugA:on_turn:out", "own:on_turn:out",
	}
	if got := strings.Join(order, ","); got != strings.Join(want, ",") {
		t.Fatalf("order:\n got %s\nwant %s", got, strings.Join(want, ","))
	}
}

// TestAgentPluginPromptFragments pins the prompt legs: static fragments extend
// the agent's, and a plugin hook runs exactly like an agent.WithPromptHook hook
// — evaluated when the turn's context is seeded, captured on the wire by the
// client (conv.PromptLines).
func TestAgentPluginPromptFragments(t *testing.T) {
	var hookMu sync.Mutex
	var hookRuns int
	spy := &promptSpy{}

	bundle := plugin.NewBundle("prompted").
		WithPrompt("PLUGIN-STATIC").
		WithPromptHook(func(*agocontext.Context) ([]string, error) {
			hookMu.Lock()
			hookRuns++
			hookMu.Unlock()
			return []string{"PLUGIN-DYN"}, nil
		})
	a := agent.New("bot", "SYS", agent.WithConfig(spy), agent.WithPlugin(bundle.Plugin()))

	for i := 0; i < 2; i++ {
		if _, err := a.Ask(stdcontext.Background(), "hi"); err != nil {
			t.Fatalf("ask %d: %v", i+1, err)
		}
	}
	got := spy.recorded()
	if len(got) != 2 {
		t.Fatalf("client saw %d calls, want 2", len(got))
	}
	want := "SYS,PLUGIN-STATIC,PLUGIN-DYN"
	for i, p := range got {
		if have := strings.Join(p, ","); have != want {
			t.Fatalf("call %d prompt = [%s], want [%s]", i+1, have, want)
		}
	}
	// A fresh conversation re-seeds, so the plugin hook ran once per Ask — the
	// same lifecycle as a regular prompt hook.
	if hookRuns != 2 {
		t.Fatalf("plugin hook ran %d times, want 2", hookRuns)
	}
}

// TestAgentEmptyPluginHarmless pins that an empty plugin — and a nil one —
// leave the agent untouched.
func TestAgentEmptyPluginHarmless(t *testing.T) {
	var nilBundle *plugin.Bundle
	cfg := agotest.NewTestConfig(textResp("ok"))
	a := agent.New("bot", "sys", agent.WithConfig(cfg),
		agent.WithPlugin(plugin.NewBundle("empty").Plugin()),
		agent.WithPlugin(nil),
		agent.WithPlugin(nilBundle.Plugin()))
	if ts := a.Tools(); len(ts) != 0 {
		t.Fatalf("tools: %d entries, want 0", len(ts))
	}
	if fr := a.PromptFragments(); strings.Join(fr, ",") != "sys" {
		t.Fatalf("fragments: %v", fr)
	}
	reply, err := a.Ask(stdcontext.Background(), "hi")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if reply.Body() != "ok" {
		t.Fatalf("body: %q", reply.Body())
	}
}
