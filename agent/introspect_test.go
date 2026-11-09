package agent

import (
	stdcontext "context"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/hitl"
	"github.com/luckystrker/ago/middleware"
	agotest "github.com/luckystrker/ago/testing"
)

// passthroughMW is an agent-level middleware used only to make the composition
// view observable.
type passthroughMW struct{ middleware.Base }

func newPassthrough() middleware.Middleware { return &passthroughMW{} }

// TestAgentIntrospectionViews pins the read-only composition views (Python
// parity: Agent.system_prompt / dynamic_prompt / middleware / dependencies /
// variables / has_hitl_hook / response_schema / observers / tasks). Each view
// reports what was configured and hands out a copy.
func TestAgentIntrospectionViews(t *testing.T) {
	cfg := agotest.NewTestConfig(textResp("ok"))
	obs := &lifecycleObserver{name: "view"}
	hook := func(*agocontext.Context) ([]string, error) { return nil, nil }
	humanHook := func(stdcontext.Context, *events.HumanInputRequest) (*events.HumanMessage, error) {
		return nil, hitl.ErrNotProvided
	}
	a := New("viewed", "STATIC PROMPT",
		WithConfig(cfg),
		WithMiddleware(newPassthrough),
		WithObservers(obs),
		WithHitlHook(humanHook),
		WithDependencies(map[string]any{"dep": 1}),
		WithVariables(map[string]any{"var": 2}),
		WithResponseSchema(rejectSchema{}),
		WithPromptHook(hook),
		WithTasks(TaskConfig{MaxConcurrency: 3}),
	)

	if got := a.Config(); got != cfg {
		t.Fatalf("Config = %v, want the configured one", got)
	}
	if got := a.PromptFragments(); len(got) != 1 || got[0] != "STATIC PROMPT" {
		t.Fatalf("PromptFragments = %q", got)
	}
	if got := a.DynamicPrompt(); len(got) != 1 {
		t.Fatalf("DynamicPrompt = %d hooks, want 1", len(got))
	}
	if got := a.Middleware(); len(got) != 1 {
		t.Fatalf("Middleware = %d factories, want 1", len(got))
	}
	if got := a.Dependencies(); len(got) != 1 || got["dep"] != 1 {
		t.Fatalf("Dependencies = %v", got)
	}
	if got := a.Variables(); len(got) != 1 || got["var"] != 2 {
		t.Fatalf("Variables = %v", got)
	}
	if !a.HasHitlHook() {
		t.Fatal("HasHitlHook = false with a configured hook")
	}
	if a.ResponseSchema() == nil {
		t.Fatal("ResponseSchema = nil, want the configured schema")
	}
	if got := a.Observers(); len(got) != 1 || got[0].Name() != "view" {
		t.Fatalf("Observers = %v", got)
	}
	if got := a.Tasks(); got == nil || got.MaxConcurrency != 3 {
		t.Fatalf("Tasks = %+v, want MaxConcurrency 3", got)
	}
	if got := a.Name(); got != "viewed" {
		t.Fatalf("Name = %q", got)
	}

	// Copies: mutating a view must not reach the agent.
	a.Dependencies()["dep"] = 99
	a.Variables()["var"] = 99
	a.Observers()[0] = nil
	if a.Dependencies()["dep"] != 1 || a.Variables()["var"] != 2 || a.Observers()[0] == nil {
		t.Fatal("an introspection view handed out the agent's own state")
	}

	// A bare agent reports empty views.
	bare := New("bare", "")
	if bare.HasHitlHook() {
		t.Fatal("bare agent reports a hitl hook")
	}
	if bare.Tasks() != nil {
		t.Fatal("bare agent reports a task config")
	}
	if bare.ResponseSchema() != nil {
		t.Fatal("bare agent reports a response schema")
	}
	if bare.Config() != nil {
		t.Fatal("bare agent reports a model config")
	}
}
