package agent

import (
	stdcontext "context"
	"testing"

	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/middleware"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
)

// TestTurnPublishesAgentDependency pins the fix for the always-"unknown" agent
// label: during a turn the agent publishes itself under the ago:agent dependency
// (Python parity: AGENT_CONTEXT_DEPENDENCY_KEY), so MetricsMiddleware labels its
// series with the real name without WithMetricsAgentName.
func TestTurnPublishesAgentDependency(t *testing.T) {
	reg := middleware.NewMetricsRegistry()
	cfg := agotest.NewTestConfig(&events.ModelResponse{Message: &events.ModelMessage{Content: "done"}})
	a := New("bot-7", "", WithConfig(cfg), WithMiddleware(middleware.Metrics(reg)))

	if _, err := a.Ask(stdcontext.Background(), "go"); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	sample, ok := reg.Sample(middleware.MetricAgentTurnsTotal, map[string]string{
		"agent": "bot-7", "outcome": "success", "error_type": "",
	})
	if !ok {
		t.Fatalf("no %s series labelled agent=bot-7; series: %v", middleware.MetricAgentTurnsTotal, reg.Samples())
	}
	if sample.Value != 1 {
		t.Fatalf("agent turns = %v, want 1", sample.Value)
	}
	if _, ok := reg.Sample(middleware.MetricAgentTurnsTotal, map[string]string{
		"agent": "unknown", "outcome": "success", "error_type": "",
	}); ok {
		t.Fatal("the turn was labelled unknown instead of the real agent name")
	}
	// The same label reaches the LLM series published under the turn.
	if _, ok := reg.Sample(middleware.MetricLLMCallsTotal, map[string]string{
		"agent": "bot-7", "provider": "test", "model": "test",
		"outcome": "success", "finish_reason": "unknown", "error_type": "",
	}); !ok {
		t.Fatalf("LLM call series missing the agent label: %v", reg.Samples())
	}
}

// TestAgentDependencyValueIsTheAgent pins the value's shape: a tool sees the
// agent itself (a value with Name()), not just a label string.
func TestAgentDependencyValueIsTheAgent(t *testing.T) {
	var names []string
	probe, err := tools.NewFunctionTool("probe", "report which agent is running",
		func(args struct {
			Agent any `aginject:"ago:agent"`
		}) (string, error) {
			if named, ok := args.Agent.(interface{ Name() string }); ok {
				names = append(names, named.Name())
			}
			return "ok", nil
		})
	if err != nil {
		t.Fatalf("probe tool: %v", err)
	}
	cfg := agotest.NewTestConfig(
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "c1", Name: "probe", Arguments: "{}"},
		}}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: "done"}},
	)
	a := New("probe-agent", "", WithConfig(cfg), WithTools(probe))
	if _, err := a.Ask(stdcontext.Background(), "go"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(names) != 1 || names[0] != "probe-agent" {
		t.Fatalf("tool saw agent names %v, want [probe-agent]", names)
	}
}
