package tools_test

// Regression for the confirmed review finding on tools/executor.go: Register
// discarded the stream subscription ids it created, so a turn continuing on a
// shared stream (AgentReply.ask) kept the previous turn's executor and tool
// handlers alive — the second turn executed every call again (duplicate side
// effects, duplicate ToolResultsEvent, extra model calls until the script ran
// dry). Register now returns the ids and the agent's turn scope unsubscribes
// them in Close (docs/DECISIONS.md D-032; Python keeps the lifetime with a
// per-turn ExitStack sub_scope, and agent.py's docstring warns precisely about
// "duplicate tool execution" when it leaks).

import (
	stdcontext "context"
	"sync/atomic"
	"testing"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/events"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
)

func scriptText(content string) *events.ModelResponse {
	return &events.ModelResponse{Message: &events.ModelMessage{Content: content}}
}

func scriptToolCall(id, name, argsJSON string) *events.ModelResponse {
	return &events.ModelResponse{
		ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: id, Name: name, Arguments: argsJSON},
		}},
		Usage: events.Usage{PromptTokens: events.I64(1), TotalTokens: events.I64(1)},
	}
}

// TestTwoTurnsOnOneStreamExecuteToolOncePerTurn replays the finding's
// reproduction: two AgentReply.Ask turns on the same stream with one tool must
// execute the tool exactly once per turn.
func TestTwoTurnsOnOneStreamExecuteToolOncePerTurn(t *testing.T) {
	var executions int64
	echo, err := tools.NewFunctionTool("echo", "Echo the message",
		func(args struct {
			Msg string `json:"msg"`
		}) (string, error) {
			atomic.AddInt64(&executions, 1)
			return args.Msg, nil
		})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	cfg := agotest.NewTestConfig(
		scriptToolCall("c1", "echo", `{"msg":"one"}`),
		scriptText("first done"),
		scriptToolCall("c2", "echo", `{"msg":"two"}`),
		scriptText("second done"),
	)
	a := agent.New("worker", "", agent.WithConfig(cfg), agent.WithTools(echo))

	r1, err := a.Ask(stdcontext.Background(), "start")
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if r1.Body() != "first done" {
		t.Fatalf("turn 1 body = %q, want %q", r1.Body(), "first done")
	}
	if got := atomic.LoadInt64(&executions); got != 1 {
		t.Fatalf("after turn 1: tool executions = %d, want 1", got)
	}

	// Turn 2 continues on turn 1's stream — the leak's trigger.
	r2, err := r1.Ask(stdcontext.Background(), "again")
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if r2.Body() != "second done" {
		t.Fatalf("turn 2 body = %q, want %q", r2.Body(), "second done")
	}
	if got := atomic.LoadInt64(&executions); got != 2 {
		t.Fatalf("after turn 2: tool executions = %d, want 2 (stale turn-1 handlers fired)", got)
	}
	// Exactly one scripted response per model call: a leaked handler would have
	// exhausted the script and failed turn 2 above.
	if calls := len(cfg.Calls()); calls != 4 {
		t.Fatalf("model calls = %d, want 4", calls)
	}
}
