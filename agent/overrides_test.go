package agent

import (
	stdcontext "context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/plugin"
	"github.com/luckystrker/ago/stream"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
)

// rejectSchema is a response schema whose validation always fails: it makes a
// free-text answer observable through AgentReply.Content.
type rejectSchema struct{}

func (rejectSchema) Name() string               { return "reject" }
func (rejectSchema) Description() string        { return "always rejects" }
func (rejectSchema) JSONSchema() map[string]any { return map[string]any{"type": "object"} }
func (rejectSchema) SystemPrompt() string       { return "" }
func (s rejectSchema) Validate(string, *agocontext.Context) (any, error) {
	return nil, errors.New("schema: rejected")
}

// TestAskWithPerInvocationOverrides pins the ask/run keyword arguments that Go
// spells as RunOptions fields (Python parity: agent.ask(config=, tools=,
// dependencies=, variables=, prompt=, response_schema=)). The agent is
// configured one way and the call another; every assertion reads the call's
// values, so none of them can pass without the override being applied.
func TestAskWithPerInvocationOverrides(t *testing.T) {
	base := stream.NewMemoryStream()
	agentCfg := agotest.NewTestConfig(textResp("from agent config"))
	callCfg := agotest.NewTestConfig(
		toolResp("call_tool", `{}`),
		textResp("free text answer"),
	)
	callTool, err := tools.NewFunctionTool("call_tool", "report the call's overrides",
		func(_ struct{}, cx *agocontext.Context) (string, error) {
			dep, _ := cx.Dependency("d")
			v, _ := cx.Variable("v")
			return "d=" + dep.(string) + " v=" + v.(string), nil
		})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	a := New("overridden", "AGENT PROMPT", WithConfig(agentCfg), WithStreamBase(base),
		WithResponseSchema(rejectSchema{}),
		WithDependencies(map[string]any{"d": "agent"}),
		WithVariables(map[string]any{"v": "agent"}))

	reply, err := a.AskWith(stdcontext.Background(), RunOptions{
		Config:       callCfg,
		Tools:        []tools.Tool{callTool},
		Dependencies: map[string]any{"d": "call"},
		Variables:    map[string]any{"v": "call"},
		Prompt:       []string{"CALL PROMPT"},
		FreeText:     true,
	}, "go")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if reply.Body() != "free text answer" {
		t.Fatalf("body = %q, want the call config's answer", reply.Body())
	}
	if calls := agentCfg.Calls(); len(calls) != 0 {
		t.Fatalf("agent config was used for %d calls, want 0", len(calls))
	}
	if got := runToolResultText(t, base, "call_tool"); got != "d=call v=call" {
		t.Fatalf("tool result = %q, want the call's dependencies and variables", got)
	}
	prompt := reply.Context().PromptLines()
	if len(prompt) != 1 || prompt[0] != "CALL PROMPT" {
		t.Fatalf("prompt = %q, want only the call's fragment", prompt)
	}
	// The agent has a schema that rejects everything; only FreeText makes this
	// free-text answer readable.
	if _, err := reply.Content(); err != nil {
		t.Fatalf("Content with FreeText: %v", err)
	}
}

// TestAskWithPerInvocationHitlHook pins hitl_hook=: a hook supplied for one call
// answers the tool's question instead of the agent's hook, which is never asked.
func TestAskWithPerInvocationHitlHook(t *testing.T) {
	askHuman, err := tools.NewFunctionTool("ask_human", "ask",
		func(_ struct{}, cx *agocontext.Context) (string, error) {
			return cx.Input(stdcontext.Background(), "which?", 2*time.Second)
		})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	base := stream.NewMemoryStream()
	cfg := agotest.NewTestConfig(toolResp("ask_human", `{}`), textResp("done"))
	agentHookAsked := false
	a := New("hitl", "", WithConfig(cfg), WithStreamBase(base), WithTools(askHuman),
		WithHitlHook(func(stdcontext.Context, *events.HumanInputRequest) (*events.HumanMessage, error) {
			agentHookAsked = true
			return &events.HumanMessage{Content: "agent answer"}, nil
		}))
	callHookAsked := false
	_, err = a.AskWith(stdcontext.Background(), RunOptions{
		HitlHook: func(stdcontext.Context, *events.HumanInputRequest) (*events.HumanMessage, error) {
			callHookAsked = true
			return &events.HumanMessage{Content: "call answer"}, nil
		},
	}, "go")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if !callHookAsked || agentHookAsked {
		t.Fatalf("hooks asked: call=%v agent=%v, want only the call's", callHookAsked, agentHookAsked)
	}
	if got := runToolResultText(t, base, "ask_human"); got != "call answer" {
		t.Fatalf("tool result = %q, want the call hook's answer", got)
	}
}

// countingTurnMW counts the turns it wrapped.
type countingTurnMW struct {
	middleware.Base
	mu sync.Mutex
	n  int
}

func (m *countingTurnMW) OnTurn(next middleware.TurnFunc, ev *events.ModelRequest, cx *agocontext.Context) (*events.ModelResponse, error) {
	m.mu.Lock()
	m.n++
	m.mu.Unlock()
	return next(ev, cx)
}

func (m *countingTurnMW) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.n
}

// TestAskWithPerInvocationMiddlewareAndPlugin pins middleware= and plugins= on
// one call (Python parity): the call's middleware wraps the turn, and a plugin
// attached to the invocation contributes its tool and prompt fragment without
// being attached to the agent.
func TestAskWithPerInvocationMiddlewareAndPlugin(t *testing.T) {
	pluginTool, err := tools.NewFunctionTool("plugin_tool", "plugin tool",
		func(struct{}) (string, error) { return "plugin result", nil })
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	bundle := plugin.NewBundle("invocation").
		WithTool(pluginTool).
		WithPrompt("PLUGIN PROMPT")
	mw := &countingTurnMW{}
	base := stream.NewMemoryStream()
	a := New("plugged", "AGENT PROMPT", WithConfig(agotest.NewTestConfig(
		toolResp("plugin_tool", `{}`),
		textResp("done"),
	)), WithStreamBase(base))

	reply, err := a.AskWith(stdcontext.Background(), RunOptions{
		Middleware: []middleware.Factory{func() middleware.Middleware { return mw }},
		Plugins:    []plugin.Plugin{bundle},
	}, "go")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if mw.count() != 1 {
		t.Fatalf("invocation middleware wrapped %d turns, want 1", mw.count())
	}
	if got := runToolResultText(t, base, "plugin_tool"); got != "plugin result" {
		t.Fatalf("plugin tool result = %q, want the invocation plugin's tool", got)
	}
	prompt := reply.Context().PromptLines()
	var sawAgent, sawPlugin bool
	for _, p := range prompt {
		sawAgent = sawAgent || p == "AGENT PROMPT"
		sawPlugin = sawPlugin || p == "PLUGIN PROMPT"
	}
	if !sawAgent || !sawPlugin {
		t.Fatalf("prompt = %q, want the agent's and the plugin's fragments", prompt)
	}

	// The plugin is attached to the invocation only: the next call has neither
	// its tool nor its prompt.
	next, err := a.AskWith(stdcontext.Background(), RunOptions{Config: agotest.NewTestConfig(textResp("second"))}, "again")
	if err != nil {
		t.Fatalf("second ask: %v", err)
	}
	for _, p := range next.Context().PromptLines() {
		if p == "PLUGIN PROMPT" {
			t.Fatal("invocation plugin prompt survived the call")
		}
	}
}

// TestResumeWithStreamReplaysOnExistingStream pins resume(stream=): the seeded
// trajectory replaces that stream's history and the turn continues on it.
func TestResumeWithStreamReplaysOnExistingStream(t *testing.T) {
	cfg := agotest.NewTestConfig(textResp("resumed answer"))
	a := New("resumer", "", WithConfig(cfg))
	s := stream.NewMemoryStream()
	seed := []events.Event{
		&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "old"}}},
		&events.ModelMessage{Content: "old answer"},
		&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "new"}}},
	}
	reply, err := a.ResumeWith(stdcontext.Background(), RunOptions{Stream: s}, seed...)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if reply.Body() != "resumed answer" {
		t.Fatalf("body = %q", reply.Body())
	}
	evs, err := s.History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(evs) < len(seed) {
		t.Fatalf("stream history has %d events, want at least the %d seeded ones", len(evs), len(seed))
	}
	if st := events.AsValue(evs[0]).(events.ModelRequest).Parts[0].(*events.TextInput).Content; st != "old" {
		t.Fatalf("first history event = %q, want the seeded prefix", st)
	}
	var sawNew bool
	for _, ev := range evs {
		if req, ok := events.AsValue(ev).(events.ModelRequest); ok {
			for _, p := range req.Parts {
				if ti, ok := p.(*events.TextInput); ok && strings.Contains(ti.Content, "new") {
					sawNew = true
				}
			}
		}
	}
	if !sawNew {
		t.Fatal("resumed trigger is not in the supplied stream's history")
	}
}
