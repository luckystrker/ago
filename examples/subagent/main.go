// Command subagent demonstrates agent delegation: a specialist agent is exposed
// to a lead agent as an ordinary tool via AsTool. When the lead's scripted model
// calls that tool, the framework runs a full specialist turn in-process and
// returns its answer as the tool result; the lead's second scripted response
// wraps it into the final answer. Both answers are printed.
//
// Run: go run ./examples/subagent
package main

import (
	"context"
	"fmt"

	"ago/agent"
	"ago/events"
	agotest "ago/testing"
)

func main() {
	// The child agent answers on its own scripted config; each delegation is a
	// separate conversation on the child's own stream.
	specialist := agent.New("specialist", "You are a database specialist.",
		agent.WithConfig(agotest.NewTestConfig(&events.ModelResponse{
			Message: &events.ModelMessage{Content: "Postgres uses MVCC: writers do not block readers."},
			Usage:   events.Usage{PromptTokens: events.I64(14), CompletionTokens: events.I64(12), TotalTokens: events.I64(26)},
		})))

	// The same agent, wrapped as a callable tool for other agents.
	askSpecialist := specialist.AsTool(
		agent.WithToolName("ask_specialist"),
		agent.WithToolDescription("Delegate a database question to the specialist."))

	// The lead's script: first request the delegation, then answer from the
	// specialist's tool result.
	cfg := agotest.NewTestConfig(
		&events.ModelResponse{
			ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
				{ID: "call-1", Name: "ask_specialist",
					Arguments: `{"objective":"How does Postgres handle concurrent writes?"}`},
			}},
			Usage: events.Usage{PromptTokens: events.I64(20), CompletionTokens: events.I64(8), TotalTokens: events.I64(28)},
		},
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: "Per the specialist: Postgres uses MVCC, so writers do not block readers."},
			Usage:   events.Usage{PromptTokens: events.I64(50), CompletionTokens: events.I64(15), TotalTokens: events.I64(65)},
		},
	)

	lead := agent.New("lead", "You coordinate and delegate.",
		agent.WithConfig(cfg), agent.WithTools(askSpecialist))

	reply, err := lead.Ask(context.Background(), "We need the database question answered.")
	if err != nil {
		fmt.Println("ask failed:", err)
		return
	}

	fmt.Println("specialist (child) answer:", specialistAnswer(reply))
	fmt.Println("lead (parent) answer:     ", reply.Body())
}

// specialistAnswer recovers the child's reply from the parent's transcript: the
// delegation tool result carries the specialist's answer text.
func specialistAnswer(reply *agent.AgentReply) string {
	evs, err := reply.History().GetEvents()
	if err != nil {
		return "<history error: " + err.Error() + ">"
	}
	for _, ev := range evs {
		if results, ok := events.AsValue(ev).(events.ToolResultsEvent); ok {
			for _, r := range results.Results {
				if r.Name == "ask_specialist" && len(r.Result.Parts) > 0 {
					return events.RenderForPrompt(r.Result.Parts[0])
				}
			}
		}
	}
	return "<no delegation found>"
}
