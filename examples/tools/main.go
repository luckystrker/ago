// Command tools demonstrates function tools: an agent with two tools — one with
// arguments (add) and one without (current_time). The scripted model asks for the
// tools in two consecutive rounds; the agent executes each call in-process and
// feeds the results back until the model produces the final answer. Tool
// invocations print their own lines, so the round trips are visible.
//
// Run: go run ./examples/tools
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/events"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
)

func main() {
	// add: argument struct fields become the JSON-schema parameters; untagged
	// non-pointer fields are required by default.
	add, err := tools.NewFunctionTool("add",
		"Add two integers.",
		func(args struct {
			A int `json:"a" agschema:"description=First addend"`
			B int `json:"b" agschema:"description=Second addend"`
		}) (string, error) {
			fmt.Printf("  [tool] add(%d, %d) = %d\n", args.A, args.B, args.A+args.B)
			return fmt.Sprintf("%d", args.A+args.B), nil
		})
	if err != nil {
		fmt.Println("build add:", err)
		return
	}

	// current_time: no struct parameter, so the tool takes no model arguments.
	now, err := tools.NewFunctionTool("current_time",
		"Return the current local time.",
		func() (string, error) {
			t := time.Now().Format(time.RFC3339)
			fmt.Printf("  [tool] current_time() = %s\n", t)
			return t, nil
		})
	if err != nil {
		fmt.Println("build current_time:", err)
		return
	}

	// Scripted model behavior: round 1 calls add, round 2 calls current_time,
	// round 3 stops requesting tools and answers.
	cfg := agotest.NewTestConfig(
		&events.ModelResponse{
			ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
				{ID: "call-1", Name: "add", Arguments: `{"a":19,"b":23}`},
			}},
			Usage: events.Usage{PromptTokens: events.I64(30), CompletionTokens: events.I64(10), TotalTokens: events.I64(40)},
		},
		&events.ModelResponse{
			ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
				{ID: "call-2", Name: "current_time", Arguments: `{}`},
			}},
			Usage: events.Usage{PromptTokens: events.I64(60), CompletionTokens: events.I64(8), TotalTokens: events.I64(68)},
		},
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: "19 + 23 = 42. The current local time is in the tool result above."},
			Usage:   events.Usage{PromptTokens: events.I64(90), CompletionTokens: events.I64(16), TotalTokens: events.I64(106)},
		},
	)

	a := agent.New("calculator", "You compute things with the tools you have.",
		agent.WithConfig(cfg), agent.WithTools(add, now))

	reply, err := a.Ask(context.Background(), "What is 19 + 23, and what time is it?")
	if err != nil {
		fmt.Println("ask failed:", err)
		return
	}

	fmt.Println("\nfinal answer:", reply.Body())

	fmt.Println("\n== transcript (request, tool rounds, answer) ==")
	evs, err := reply.History().GetEvents()
	if err != nil {
		fmt.Println("history failed:", err)
		return
	}
	printTranscript(evs)
}

// printTranscript renders the event kinds that make up a tool round trip.
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
