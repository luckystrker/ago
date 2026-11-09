package eval

import (
	stdcontext "context"
	"testing"

	"github.com/luckystrker/ago/agent"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
)

func evalToolCall(name, args string) *events.ModelResponse {
	return &events.ModelResponse{
		ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{{
			ID: "call-" + name, Name: name, Arguments: args,
		}}},
	}
}

// TestBuildTraceIncludesDelegatedCallsButKeepsRootFinal pins the eval consumer
// migration: descendant model/tool events arrive as ChildEvent observations and
// contribute LLMCalls/ToolCalls, while FinalResponse remains the root agent's
// answer. Token totals stay source-based and agree with the calls.
func TestBuildTraceIncludesDelegatedCallsButKeepsRootFinal(t *testing.T) {
	probe, err := tools.NewFunctionTool("probe", "probe the child",
		func(_ struct{}, _ *agocontext.Context) (string, error) { return "probed", nil })
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	child := agent.New("child", "", agent.WithConfig(agotest.NewTestConfig(
		evalToolCall("probe", `{}`),
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: "child done"},
			Usage:   events.Usage{TotalTokens: events.I64(20)},
			Model:   "child-model",
		},
	)), agent.WithTools(probe))
	root := agent.New("root", "", agent.WithConfig(agotest.NewTestConfig(
		evalToolCall("task_child", `{"objective":"dig"}`),
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: "root done"},
			Usage:   events.Usage{TotalTokens: events.I64(1)},
			Model:   "root-model",
		},
	)), agent.WithTools(child.AsTool(agent.WithToolDescription("Dig"))))

	reply, err := root.Ask(stdcontext.Background(), "go")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	history, err := reply.History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	trace := BuildTrace(history)
	if trace.FinalResponse != "root done" {
		t.Fatalf("FinalResponse = %q, want the root answer", trace.FinalResponse)
	}
	if len(trace.LLMCalls) != 4 {
		t.Fatalf("LLMCalls = %d, want 4 (two per agent)", len(trace.LLMCalls))
	}
	models := map[string]bool{}
	for _, call := range trace.LLMCalls {
		if call.Model != "" {
			models[call.Model] = true
		}
	}
	if !models["child-model"] || !models["root-model"] {
		t.Fatalf("LLMCall models = %v", models)
	}
	if len(trace.ToolCalls) != 2 {
		t.Fatalf("ToolCalls = %d, want root task_child and child probe", len(trace.ToolCalls))
	}
	byName := map[string]ToolCall{}
	for _, call := range trace.ToolCalls {
		byName[call.Name] = call
	}
	if byName["task_child"].Result != "child done" {
		t.Fatalf("task_child result = %q", byName["task_child"].Result)
	}
	if byName["probe"].Result != "probed" {
		t.Fatalf("probe result = %q", byName["probe"].Result)
	}
	if got := trace.Tokens().TotalTokens; got == nil || *got != 21 {
		t.Fatalf("Tokens = %v, want 21", got)
	}
}
