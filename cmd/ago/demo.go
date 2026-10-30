package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/events"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
)

// demo runs a fully offline agent scenario: the model is a scripted test
// double (agotest.NewTestConfig — no network), and the script exercises two
// function-tool round trips before the final answer. Modeled on
// examples/tools but self-contained, so `ago demo` works on a machine with
// no API keys and no connectivity.
func demo(out io.Writer) error {
	add, err := tools.NewFunctionTool("add",
		"Add two integers.",
		func(args struct {
			A int `json:"a" agschema:"description=First addend"`
			B int `json:"b" agschema:"description=Second addend"`
		}) (string, error) {
			fmt.Fprintf(out, "  [tool] add(%d, %d) = %d\n", args.A, args.B, args.A+args.B)
			return fmt.Sprintf("%d", args.A+args.B), nil
		})
	if err != nil {
		return fmt.Errorf("build add tool: %w", err)
	}

	now, err := tools.NewFunctionTool("current_time",
		"Return the current local time.",
		func() (string, error) {
			t := time.Now().Format(time.RFC3339)
			fmt.Fprintf(out, "  [tool] current_time() = %s\n", t)
			return t, nil
		})
	if err != nil {
		return fmt.Errorf("build current_time tool: %w", err)
	}

	// Scripted model: round 1 calls add, round 2 calls current_time, round 3
	// answers without further tool calls.
	cfg := agotest.NewTestConfig(
		&events.ModelResponse{
			ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
				{ID: "call-1", Name: "add", Arguments: `{"a":19,"b":23}`},
			}},
		},
		&events.ModelResponse{
			ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
				{ID: "call-2", Name: "current_time", Arguments: `{}`},
			}},
		},
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: "19 + 23 = 42. The current local time is in the tool result above."},
		},
	)

	fmt.Fprintln(out, "ago demo — offline scripted run (the model is a test double; no network)")

	a := agent.New("demo", "You compute things with the tools you have.",
		agent.WithConfig(cfg), agent.WithTools(add, now))

	reply, err := a.Ask(context.Background(), "What is 19 + 23, and what time is it?")
	if err != nil {
		return fmt.Errorf("ask: %w", err)
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "final answer:", reply.Body())

	evs, err := reply.History().GetEvents()
	if err != nil {
		return fmt.Errorf("history: %w", err)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "== transcript (request, tool rounds, answer) ==")
	writeTranscript(out, evs)
	return nil
}

// writeTranscript renders the event kinds that make up a tool round trip.
func writeTranscript(out io.Writer, evs []events.Event) {
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.ModelRequest:
			fmt.Fprintf(out, "[user]      %s\n", events.RenderForPrompt(e))
		case events.ModelResponse:
			if e.Content() != "" {
				fmt.Fprintf(out, "[assistant] %s\n", e.Content())
			}
		case events.ToolCallsEvent:
			for _, c := range e.Calls {
				fmt.Fprintf(out, "[model]     tool call %s(%s)\n", c.Name, c.Arguments)
			}
		case events.ToolResultsEvent:
			for _, r := range e.Results {
				fmt.Fprintf(out, "[tool]      %s -> %s\n", r.Name, renderParts(r.Result))
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
