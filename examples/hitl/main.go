// Command hitl demonstrates human-in-the-loop tool approval. The deploy tool is
// gated by the Approval middleware: before the tool runs, the framework asks the
// human operator through the agent's HITL hook. The hook here is a stub — it
// prints the question it receives and answers "y" — so the example runs offline.
// Only the answer lands on the transcript; the question is delivered to the hook
// without being transcribed (Python parity: dispatch stops at the interrupt
// subscriber).
//
// Run: go run ./examples/hitl
package main

import (
	"context"
	"fmt"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/middleware"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
)

func main() {
	// The sensitive tool: it only runs after the human approves the call.
	deploy, err := tools.NewFunctionTool("deploy",
		"Deploy a release version to production.",
		func(args struct {
			Version string `json:"version" agschema:"description=Release version to deploy"`
		}) (string, error) {
			fmt.Printf("  [tool] deploy: releasing %s ...\n", args.Version)
			return "version " + args.Version + " deployed", nil
		})
	if err != nil {
		fmt.Println("build deploy:", err)
		return
	}

	// The stub operator: every question is printed and approved. A real hook
	// would read stdin, poll a chat, or route to an on-call dashboard.
	hook := func(ctx context.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
		fmt.Printf("  [hitl] question: %s\n", req.Content)
		answer := "y"
		fmt.Printf("  [hitl] answer:   %s\n", answer)
		return &events.HumanMessage{ParentID: req.ID, Content: answer}, nil
	}

	// Scripted model behavior: round 1 requests the deploy call, round 2
	// summarizes the (approved) tool result.
	cfg := agotest.NewTestConfig(
		&events.ModelResponse{
			ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
				{ID: "call-1", Name: "deploy", Arguments: `{"version":"1.2.0"}`},
			}},
			Usage: events.Usage{PromptTokens: events.I64(25), CompletionTokens: events.I64(9), TotalTokens: events.I64(34)},
		},
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: "Version 1.2.0 is live in production."},
			Usage:   events.Usage{PromptTokens: events.I64(55), CompletionTokens: events.I64(10), TotalTokens: events.I64(65)},
		},
	)

	a := agent.New("operator", "You manage releases.",
		agent.WithConfig(cfg),
		agent.WithTools(deploy),
		agent.WithMiddleware(middleware.Approval()), // every tool call needs a human yes
		agent.WithHitlHook(hook))

	reply, err := a.Ask(context.Background(), "Ship release 1.2.0 please.")
	if err != nil {
		fmt.Println("ask failed:", err)
		return
	}

	fmt.Println("\nfinal answer:", reply.Body())

	fmt.Println("\n== transcript (the answer is part of history; the question went to the hook) ==")
	evs, err := reply.History().GetEvents()
	if err != nil {
		fmt.Println("history failed:", err)
		return
	}
	printTranscript(evs)
}

// printTranscript renders the event kinds involved in an approved tool call.
func printTranscript(evs []events.Event) {
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.ModelRequest:
			fmt.Printf("[user]      %s\n", events.RenderForPrompt(e))
		case events.ModelResponse:
			if e.Content() != "" {
				fmt.Printf("[assistant] %s\n", e.Content())
			}
		case events.ToolCallsEvent:
			for _, c := range e.Calls {
				fmt.Printf("[model]     tool call %s(%s)\n", c.Name, c.Arguments)
			}
		case events.ToolResultsEvent:
			for _, r := range e.Results {
				fmt.Printf("[tool]      %s -> %s\n", r.Name, renderParts(r.Result))
			}
		case events.HumanInputRequest:
			fmt.Printf("[question]  %s\n", e.Content)
		case events.HumanMessage:
			fmt.Printf("[human]     %s\n", e.Content)
		}
	}
}

func renderParts(r events.ToolResult) string {
	out := ""
	for i, p := range r.Parts {
		if i > 0 {
			out += "; "
		}
		out += events.RenderForPrompt(p)
	}
	return out
}
