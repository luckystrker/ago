// Command ask demonstrates the smallest AGo program: one agent whose "LLM" is a
// scripted test config, so the example runs offline without API keys. It runs two
// conversational turns — the second continues the first turn's stream, so the
// model sees the earlier exchange — and prints the durable transcript (history)
// plus the token-usage report aggregated from UsageEvents.
//
// Run: go run ./examples/ask
package main

import (
	"context"
	"fmt"

	"ago/agent"
	"ago/events"
	agotest "ago/testing"
	"ago/usage"
)

func main() {
	// One scripted ModelResponse per model call, handed out in order. Usage
	// numbers are invented, exactly like a real provider would report them.
	cfg := agotest.NewTestConfig(
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: "AGo is a Go port of the AG2 agent framework."},
			Usage:   events.Usage{PromptTokens: events.I64(12), CompletionTokens: events.I64(20), TotalTokens: events.I64(32)},
		},
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: "Stdlib only keeps the dependency tree flat and audits trivial."},
			Usage:   events.Usage{PromptTokens: events.I64(48), CompletionTokens: events.I64(18), TotalTokens: events.I64(66)},
		},
	)

	a := agent.New("assistant", "You are a concise assistant.", agent.WithConfig(cfg))

	fmt.Println("== turn 1 ==")
	first, err := a.Ask(context.Background(), "What is AGo?")
	if err != nil {
		fmt.Println("turn 1 failed:", err)
		return
	}
	fmt.Println("assistant:", first.Body())

	fmt.Println("== turn 2: continuation on the same stream ==")
	second, err := first.Ask(context.Background(), "Why stdlib only?")
	if err != nil {
		fmt.Println("turn 2 failed:", err)
		return
	}
	fmt.Println("assistant:", second.Body())

	fmt.Println("\n== history (durable transcript, both turns) ==")
	evs, err := second.History().GetEvents()
	if err != nil {
		fmt.Println("history failed:", err)
		return
	}
	printTranscript(evs)

	fmt.Println("\n== usage (aggregated from UsageEvents on the stream) ==")
	printUsage(second.Usage())
}

// printTranscript renders the interesting event kinds of one conversation.
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

// printUsage shows the usage report: one record per model call plus the total.
func printUsage(rep *usage.Report) {
	for i, rec := range rep.Records {
		fmt.Printf("call %d: kind=%s model=%s provider=%s usage=%s\n",
			i+1, rec.Kind, rec.Model, rec.Provider, usageString(rec.Usage))
	}
	if t := rep.Total; t.TotalTokens != nil {
		fmt.Printf("total: %s\n", usageString(t))
	}
}

// usageString renders the non-nil token counters of a Usage record.
func usageString(u events.Usage) string {
	s := ""
	add := func(label string, p *int64) {
		if p == nil {
			return
		}
		if s != "" {
			s += " "
		}
		s += fmt.Sprintf("%s=%d", label, *p)
	}
	add("prompt", u.PromptTokens)
	add("completion", u.CompletionTokens)
	add("total", u.TotalTokens)
	if s == "" {
		return "none reported"
	}
	return s
}
