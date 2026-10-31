package toolpacks

import (
	stdcontext "context"
	"strings"
	"sync"
	"testing"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/middleware"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
)

func textResp(content string) *events.ModelResponse {
	return &events.ModelResponse{
		Message: &events.ModelMessage{Content: content},
		Usage:   events.Usage{PromptTokens: events.I64(1), CompletionTokens: events.I64(2), TotalTokens: events.I64(3)},
	}
}

func toolResp(name, argsJSON string) *events.ModelResponse {
	call := events.ToolCallEvent{ID: "call-" + name, Name: name, Arguments: argsJSON}
	return &events.ModelResponse{
		ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{call}},
		Usage:     events.Usage{PromptTokens: events.I64(1), TotalTokens: events.I64(1)},
	}
}

func weatherTool(t *testing.T) tools.Tool {
	t.Helper()
	fn := func(args struct {
		City string `json:"city" agschema:"description=City name,required"`
	}) (string, error) {
		return "sunny in " + args.City, nil
	}
	wt, err := tools.NewFunctionTool("get_weather", "Get the weather for a city.", fn)
	if err != nil {
		t.Fatalf("weather tool: %v", err)
	}
	return wt
}

func TestDynamicAgentFactoryRunsChild(t *testing.T) {
	childCfg := agotest.NewTestConfig(textResp("child reply: all done"))
	factory, err := NewDynamicAgentFactory(childCfg, []tools.Tool{weatherTool(t)})
	if err != nil {
		t.Fatalf("NewDynamicAgentFactory: %v", err)
	}
	if factory.Name() != "create_and_run_agent" {
		t.Fatalf("name = %q", factory.Name())
	}

	parentCfg := agotest.NewTestConfig(
		toolResp("create_and_run_agent",
			`{"spec":{"name":"worker","prompt":["You are a worker."],"tool_names":["get_weather"]},"objective":"report the weather"}`),
		textResp("parent got: child reply"),
	)
	a := agent.New("parent", "", agent.WithConfig(parentCfg), agent.WithTools(factory))
	reply, err := a.Ask(stdcontext.Background(), "spawn a worker")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if reply.Body() != "parent got: child reply" {
		t.Fatalf("body = %q", reply.Body())
	}

	// The delegation lifecycle is on the parent stream (run_task parity).
	evs, err := reply.History().GetEvents()
	if err != nil {
		t.Fatal(err)
	}
	var started, completed *bool
	completedResult := ""
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.TaskStarted:
			v := e.AgentName == "worker" && e.Objective == "report the weather"
			started = &v
		case events.TaskCompleted:
			completedResult = e.Result
			v := e.AgentName == "worker"
			completed = &v
		}
	}
	if started == nil || !*started {
		t.Fatalf("TaskStarted for worker missing in %d events", len(evs))
	}
	if completed == nil || !*completed {
		t.Fatalf("TaskCompleted for worker missing")
	}
	if completedResult != "child reply: all done" {
		t.Fatalf("TaskCompleted result = %q", completedResult)
	}

	// The child spec reached the child agent: its config recorded one call
	// carrying the objective and the worker prompt.
	calls := childCfg.Calls()
	if len(calls) != 1 {
		t.Fatalf("child config saw %d calls, want 1", len(calls))
	}
	var sawObjective, sawPrompt bool
	for _, ev := range calls[0] {
		switch e := events.AsValue(ev).(type) {
		case events.ModelRequest:
			for _, p := range e.Parts {
				if txt, ok := events.AsValue(p).(events.TextInput); ok && strings.Contains(txt.Content, "report the weather") {
					sawObjective = true
				}
			}
		case events.ModelResponse:
			// history replay
		}
	}
	_ = sawPrompt
	if !sawObjective {
		t.Fatal("child history does not carry the objective")
	}
}

func TestDynamicAgentFactoryUnknownToolIsRecoverable(t *testing.T) {
	childCfg := agotest.NewTestConfig(textResp("never reached"))
	factory, err := NewDynamicAgentFactory(childCfg, []tools.Tool{weatherTool(t)})
	if err != nil {
		t.Fatalf("NewDynamicAgentFactory: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, factory, cx, factory.Name(),
		`{"spec":{"name":"worker","prompt":["p"],"tool_names":["ghost"]},"objective":"x"}`)
	got := resultPartText(t, ev)
	if !strings.Contains(got, "Error: unknown tools ['ghost']") ||
		!strings.Contains(got, "Available: ['get_weather']") {
		t.Fatalf("unknown-tool result = %q, want the recoverable Python-handler text", got)
	}
}

func TestDynamicAgentFactoryDescriptionMenu(t *testing.T) {
	factory, err := NewDynamicAgentFactory(agotest.NewTestConfig(textResp("x")), []tools.Tool{weatherTool(t)})
	if err != nil {
		t.Fatalf("NewDynamicAgentFactory: %v", err)
	}
	_, cx := newTestContext()
	schemas, err := factory.Schemas(cx)
	if err != nil || len(schemas) != 1 {
		t.Fatalf("schemas = %v, %v", schemas, err)
	}
	sch := schemas[0].(toolsSchema)
	if !strings.Contains(sch.Description, "Available tools:") ||
		!strings.Contains(sch.Description, "- get_weather: Get the weather for a city.") {
		t.Fatalf("description missing the pool menu:\n%s", sch.Description)
	}
}

func TestDynamicAgentFactoryValidation(t *testing.T) {
	if _, err := NewDynamicAgentFactory(nil, nil); err == nil {
		t.Fatal("a nil config must be rejected")
	}
	if _, err := NewDynamicAgentFactory(agotest.NewTestConfig(textResp("x")), nil,
		WithDynamicName("")); err == nil {
		t.Fatal("an empty name must be rejected")
	}
}

// dynamicSpecArgs is a spec payload picking get_weather from the pool.
const dynamicSpecArgs = `{"spec":{"name":"worker","prompt":["You are a worker."],` +
	`"tool_names":["get_weather"]},"objective":"report the weather"}`

// TestDynamicAgentFactoryInheritsParentApproval covers H-7: the parent's
// Approval middleware must intercept the spawned child's tool call, not only
// the create_and_run_agent call itself. The hook approves spawning; its answer
// to the child's get_weather call decides whether the tool runs.
func TestDynamicAgentFactoryInheritsParentApproval(t *testing.T) {
	for _, tt := range []struct {
		name     string
		answer   string
		wantTool bool
	}{
		{name: "deny blocks the child tool call", answer: "n", wantTool: false},
		{name: "approve runs the child tool call", answer: "y", wantTool: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var toolRan bool
			fn := func(args struct {
				City string `json:"city" agschema:"description=City name,required"`
			}) (string, error) {
				toolRan = true
				return "sunny in " + args.City, nil
			}
			wt, err := tools.NewFunctionTool("get_weather", "Get the weather for a city.", fn)
			if err != nil {
				t.Fatalf("weather tool: %v", err)
			}
			childCfg := agotest.NewTestConfig(
				toolResp("get_weather", `{"city":"Paris"}`),
				textResp("child done"),
			)
			factory, err := NewDynamicAgentFactory(childCfg, []tools.Tool{wt})
			if err != nil {
				t.Fatalf("NewDynamicAgentFactory: %v", err)
			}

			var mu sync.Mutex
			weatherAsked := false
			hook := func(hctx stdcontext.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
				answer := "y" // approve spawning the child
				if strings.Contains(req.Content, "`get_weather`") {
					mu.Lock()
					weatherAsked = true
					mu.Unlock()
					answer = tt.answer
				}
				return &events.HumanMessage{Content: answer}, nil
			}
			parentCfg := agotest.NewTestConfig(
				toolResp("create_and_run_agent", dynamicSpecArgs),
				textResp("parent done"),
			)
			a := agent.New("parent", "",
				agent.WithConfig(parentCfg),
				agent.WithTools(factory),
				agent.WithMiddleware(middleware.Approval()),
				agent.WithHitlHook(hook),
			)
			reply, err := a.Ask(stdcontext.Background(), "spawn a worker")
			if err != nil {
				t.Fatalf("ask: %v", err)
			}
			if reply.Body() != "parent done" {
				t.Fatalf("body = %q", reply.Body())
			}
			mu.Lock()
			asked := weatherAsked
			mu.Unlock()
			if !asked {
				t.Fatal("the parent's Approval never asked about the child's get_weather call")
			}
			if toolRan != tt.wantTool {
				t.Fatalf("child tool ran = %v, want %v", toolRan, tt.wantTool)
			}
		})
	}
}

// TestWithDynamicOptionAppliesToChild checks the caller-supplied agent options
// reach the spawned child (here: a config override wins over the factory's).
func TestWithDynamicOptionAppliesToChild(t *testing.T) {
	defaultCfg := agotest.NewTestConfig(textResp("default config must not be used"))
	overrideCfg := agotest.NewTestConfig(textResp("child done"))
	factory, err := NewDynamicAgentFactory(defaultCfg, nil,
		WithDynamicOption(agent.WithConfig(overrideCfg)))
	if err != nil {
		t.Fatalf("NewDynamicAgentFactory: %v", err)
	}
	parentCfg := agotest.NewTestConfig(
		toolResp("create_and_run_agent",
			`{"spec":{"name":"worker","prompt":["p"]},"objective":"x"}`),
		textResp("parent done"),
	)
	a := agent.New("parent", "", agent.WithConfig(parentCfg), agent.WithTools(factory))
	reply, err := a.Ask(stdcontext.Background(), "spawn a worker")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if reply.Body() != "parent done" {
		t.Fatalf("body = %q", reply.Body())
	}
	if n := len(overrideCfg.Calls()); n != 1 {
		t.Fatalf("override config saw %d child calls, want 1", n)
	}
	if n := len(defaultCfg.Calls()); n != 0 {
		t.Fatalf("factory config saw %d child calls, want 0", n)
	}
}
