// Command multi_turn demonstrates a five-turn conversation chained through
// AgentReply.Ask: every follow-up continues the same stream, so constraints
// stated once — the budget on turn 2, trains over flights on turn 3 — stay in
// the transcript and ride forward into later turns. The caller never re-supplies
// them, and the final summary turn still honors all of them.
//
// The model is a scripted test config, so the example runs offline. The script
// imitates what a history-aware planner would answer; the mechanism itself —
// that each turn's model call receives the whole earlier transcript — is real,
// and the run proves it two ways at the end: it lists what the turn-5 call saw
// (recorded by the test config), and it checks that the early constraints are
// present in that input.
//
// Run: go run ./examples/multi_turn
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/events"
	agotest "github.com/luckystrker/ago/testing"
)

// turns is the user's side of the dialogue, one entry per turn (Python parity
// with the travel-planner example).
var turns = []string{
	"I want to plan a 5-day trip to Japan in late April. Just cherry-blossom season.",
	"Budget is around $2500 per person, two travellers. Optimise for sightseeing, not luxury.",
	"We prefer trains to flights once we're in Japan. Draft a day-by-day itinerary.",
	"Looks great. For day 3, swap the shopping stop for something outdoorsy in or near Kyoto.",
	"Summarize the final itinerary in a single bullet list, one line per day.",
}

func main() {
	// One scripted ModelResponse per model call — one call per turn, five turns.
	// Later answers quote earlier constraints (budget, trains, the day-3 swap),
	// the way a model with the full transcript in its prompt would. Usage numbers
	// are invented; prompt tokens grow turn over turn because the history does.
	cfg := agotest.NewTestConfig(
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: strings.Join([]string{
				"Late April is peak cherry-blossom season on the Tokyo-Kyoto corridor. A working skeleton:",
				"Day 1 arrive Tokyo; Day 2 Tokyo old town (Asakusa, Ueno Park); Day 3 shinkansen to Kyoto;",
				"Day 4 Kyoto + Nara; Day 5 Osaka, fly home. Tell me budget and travel style, and I'll firm it up.",
			}, " ")},
			Usage: events.Usage{PromptTokens: events.I64(40), CompletionTokens: events.I64(55), TotalTokens: events.I64(95)},
		},
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: strings.Join([]string{
				"Noted: ~$2500 per person for two travellers, sightseeing over luxury. That comfortably covers",
				"a regional rail pass, business hotels (~$120/night) and ~$60/day for food. I'll favour free",
				"sights — parks, shrines, markets — and cap paid entries at one per day.",
			}, " ")},
			Usage: events.Usage{PromptTokens: events.I64(150), CompletionTokens: events.I64(65), TotalTokens: events.I64(215)},
		},
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: strings.Join([]string{
				"Trains it is — no domestic flights. Draft day-by-day:",
				"Day 1 - land at Narita, Narita Express to Tokyo, evening in Shinjuku.",
				"Day 2 - Tokyo: Asakusa, Ueno Park blossoms, river walk to Asakusa.",
				"Day 3 - shinkansen to Kyoto, Teramachi shopping arcade, Gion at dusk.",
				"Day 4 - Nara day trip by train: deer park and Todai-ji.",
				"Day 5 - Kyoto to Osaka by special rapid, Osaka Castle, fly home from KIX.",
			}, "\n")},
			Usage: events.Usage{PromptTokens: events.I64(280), CompletionTokens: events.I64(105), TotalTokens: events.I64(385)},
		},
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: strings.Join([]string{
				"Day 3 revised: instead of the Teramachi shopping arcade, spend the morning in Arashiyama —",
				"bamboo grove, garden and riverbank walking, outdoors and free; keep the Gion stroll in the",
				"evening. The rest of the itinerary and the $2500-per-person budget are untouched.",
			}, " ")},
			Usage: events.Usage{PromptTokens: events.I64(430), CompletionTokens: events.I64(60), TotalTokens: events.I64(490)},
		},
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: strings.Join([]string{
				"Final itinerary, one line per day:",
				"- Day 1 - Narita Express into Tokyo, evening in Shinjuku.",
				"- Day 2 - Tokyo old town: Asakusa and the Ueno Park blossoms.",
				"- Day 3 - shinkansen to Kyoto; Arashiyama bamboo grove (the outdoors swap), Gion at dusk.",
				"- Day 4 - Nara day trip by train: deer park and Todai-ji.",
				"- Day 5 - train to Osaka, Osaka Castle, fly home from KIX.",
				"Two travellers, ~$2500 each, trains throughout.",
			}, "\n")},
			Usage: events.Usage{PromptTokens: events.I64(540), CompletionTokens: events.I64(95), TotalTokens: events.I64(635)},
		},
	)

	planner := agent.New("travel-planner",
		"You are a detail-oriented travel planner. When the user adds constraints, "+
			"update the plan rather than starting over. Be concrete and concise.",
		agent.WithConfig(cfg))

	// Turn 1 starts the conversation; the reply handle owns the stream from here on.
	section("Turn 1 - kick off")
	reply, err := planner.Ask(context.Background(), turns[0])
	if err != nil {
		fmt.Println("turn 1 failed:", err)
		return
	}
	fmt.Println(reply.Body())

	// Turns 2..5 chain reply.Ask: same stream, full history, no re-supplied context.
	for i, question := range turns[1:] {
		section(fmt.Sprintf("Turn %d - %s", i+2, question))
		reply, err = reply.Ask(context.Background(), question)
		if err != nil {
			fmt.Printf("turn %d failed: %v\n", i+2, err)
			return
		}
		fmt.Println(reply.Body())
	}

	// Proof of the carry-forward claim, straight from the recorded model calls:
	// the last call's input contains every earlier user message, constraints included.
	section("What the turn-5 model call saw (recorded by the test config)")
	calls := cfg.Calls()
	last := calls[len(calls)-1]
	fmt.Printf("call transcripts, oldest first (events, est. tokens): ")
	for i, c := range calls {
		fmt.Printf("[call %d: %d ev, ~%d tok] ", i+1, len(c), estimateTokens(c))
	}
	fmt.Println()
	for _, ev := range last {
		if req, ok := events.AsValue(ev).(events.ModelRequest); ok {
			fmt.Printf("  user message in input: %q\n", oneLine(events.RenderForPrompt(req)))
		}
	}
	checkCarried(last, "budget is around $2500", "turn-2 budget constraint")
	checkCarried(last, "prefer trains to flights", "turn-3 trains preference")
	checkCarried(last, "day 3, swap the shopping", "turn-4 day-3 revision")

	// The durable transcript: all five turns in one stream, callers never re-sent anything.
	section("Full transcript - one stream across all five turns")
	evs, err := reply.History().GetEvents()
	if err != nil {
		fmt.Println("history failed:", err)
		return
	}
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.ModelRequest:
			fmt.Printf("[user]      %s\n", events.RenderForPrompt(e))
		case events.ModelResponse:
			if e.Content() != "" {
				fmt.Printf("[assistant] %s\n", oneLine(e.Content()))
			}
		}
	}
}

// section prints a turn separator (Python parity with the original's section()).
func section(title string) {
	fmt.Printf("\n-- %s --\n", title)
}

// checkCarried reports whether a fragment of an early turn is present in the
// input of a later model call — the constraints travel via history alone.
func checkCarried(input []events.Event, fragment, label string) {
	for _, ev := range input {
		if strings.Contains(strings.ToLower(events.RenderForPrompt(ev)), fragment) {
			fmt.Printf("  carried forward: %s is in the input (never re-supplied by the caller)\n", label)
			return
		}
	}
	fmt.Printf("  MISSING: %s (%q) not found in the input\n", label, fragment)
}

// estimateTokens sums the crude per-event token estimate over one call snapshot.
func estimateTokens(evs []events.Event) int {
	total := 0
	for _, ev := range evs {
		total += events.EstimatedTokens(ev, 4)
	}
	return total
}

// oneLine flattens multi-line text so the transcript stays scannable.
func oneLine(s string) string {
	return strings.ReplaceAll(s, "\n", " ")
}
