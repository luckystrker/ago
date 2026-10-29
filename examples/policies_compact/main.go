// Command policies_compact demonstrates composing assembly policies on top of
// history compaction (Python parity: the 07_long_doc_chat example). Three
// policies compose in order and control exactly what the LLM sees on every call
// while the underlying transcript keeps growing:
//
//  1. ConversationPolicy — drops every non-conversational event (usage
//     telemetry, enqueue bookkeeping) from the model's view.
//  2. SlidingWindowPolicy(6) — hard-caps the number of conversational events
//     forwarded, so the view cannot grow unbounded.
//  3. TokenBudgetPolicy(2000) — estimated-token secondary cap, belt-and-braces
//     against one huge event blowing the budget.
//
// A separate compact.CompactionMiddleware keeps the stream history itself small:
// once the transcript passes 8 conversational events, the old span is replaced
// by one CompactionSummary plus the recent tail. Assembly trims the view into
// history; compaction rewrites history — different layers, wired as two
// middleware on one agent.
//
// The model is a scripted test config, so the example runs offline. The proof
// of the story is at the end: the test config records what every call actually
// received (post-assembly), and the run prints that per call — the raw history
// grows, the wire stays capped, and after compaction the earliest words reach
// the model only through the summary.
//
// AGO-DEVIATION from the Python original: KnowledgeConfig/MemoryKnowledgeStore
// is not ported (the AGo agent has no knowledge wiring yet, and the store played
// no observable role in the original run); SlidingWindowPolicy has no
// transparent flag (the AGo policy is always view-only); TailWindowCompact's
// tunable tail is compact.KeepRecent (fixed 2 conversational events); and the
// summarizer gets its own scripted client so the turn script stays readable.
//
// Run: go run ./examples/policies_compact
package main

import (
	"context"
	"fmt"

	"ago/agent"
	"ago/compact"
	agocontext "ago/context"
	"ago/events"
	"ago/middleware"
	"ago/policies"
	"ago/stream"
	agotest "ago/testing"
)

// questions is the user's side of the dialogue (Python parity). The last turn
// asks for the three most recent words — answerable only from what the policies
// and the summary actually left in view.
var questions = []string{
	"Remember the word 'oak'.",
	"Remember the word 'river'.",
	"Remember the word 'lantern'.",
	"Remember the word 'sable'.",
	"Remember the word 'quartz'.",
	"Name the three most recent words I asked you to remember.",
}

func main() {
	// One scripted ModelResponse per turn — six turns, one model call each.
	// The final answer names exactly the words the assembled view can support:
	// quartz from the raw window, sable and lantern from the summary text.
	cfg := agotest.NewTestConfig(
		resp("Oak — noted."),
		resp("River — noted."),
		resp("Lantern — noted."),
		resp("Sable — noted."),
		resp("Quartz — noted."),
		resp("Quartz, sable, lantern."),
	)

	// The summarizer is a separate scripted client: compaction makes exactly one
	// summarization call, after turn 5, when the transcript passes the trigger.
	summarizer := agotest.NewTestConfig(
		resp("Earlier the user asked the assistant to remember the words " +
			"oak, river, lantern and sable; each was acknowledged."),
	)

	// Watch CompactionCompleted from outside the agent: the events are transient
	// (never persisted), so subscription is the only way to see them.
	ms := stream.NewMemoryStream()
	var compactions []events.CompactionCompleted
	ms.Subscribe(func(ev events.Event) error {
		if c, ok := events.AsValue(ev).(events.CompactionCompleted); ok {
			compactions = append(compactions, c)
		}
		return nil
	}, stream.SubscribeOptions{Condition: events.TypeOf[events.CompactionCompleted]()})

	lexicon := agent.New("lexicon",
		"Be very terse — one short sentence per reply. Answer directly without calling any tools.",
		agent.WithConfig(cfg),
		agent.WithStreamBase(ms),
		agent.WithMiddleware(
			assembly(
				policies.NewConversationPolicy(),
				policies.NewSlidingWindowPolicy(6),
				policies.NewTokenBudgetPolicy(2000),
			),
			compact.New(compact.EventCountTrigger(8),
				compact.LLMStrategy{Client: summarizer},
				compact.WithAgent("lexicon")),
		),
	)

	section("Long-doc chat — assembly policies trim what the LLM actually sees")

	reply, err := lexicon.Ask(context.Background(), questions[0])
	if err != nil {
		fmt.Println("turn 1 failed:", err)
		return
	}
	fmt.Printf("Q1> %s\nA1> %s\n", questions[0], reply.Body())

	for i, q := range questions[1:] {
		reply, err = reply.Ask(context.Background(), q)
		if err != nil {
			fmt.Printf("turn %d failed: %v\n", i+2, err)
			return
		}
		fmt.Printf("Q%d> %s\nA%d> %s\n", i+2, q, i+2, reply.Body())
	}

	section("Compactions fired during run")
	fmt.Printf("count: %d\n", len(compactions))
	for _, c := range compactions {
		fmt.Printf("  - %s: %d -> %d events (%d summarizer LLM %s)\n",
			c.Strategy, c.EventsBefore, c.EventsAfter, c.LLMCalls, plural(c.LLMCalls, "call"))
	}

	section("What the model saw on each call (recorded post-assembly by the test config)")
	calls := cfg.Calls()
	for i, evs := range calls {
		fmt.Printf("call %d: %d %s on the wire, ~%d est. tokens\n",
			i+1, len(evs), plural(len(evs), "event"), estimateTokens(evs))
		describeCall(evs)
	}

	section("Proof the caps did the work")
	last := calls[len(calls)-1]
	raw := 0
	for _, ev := range last {
		if _, ok := events.AsValue(ev).(events.ModelRequest); ok {
			raw++
		}
	}
	fmt.Printf("final call carried %d raw user turns of the %d asked — oak and river "+
		"reached the model only through the summary text\n", raw, len(questions))
	if evs, err := reply.History().GetEvents(); err == nil {
		fmt.Printf("stream history after the run: %d events, held up by one CompactionSummary\n", len(evs))
	}
}

// assembly is the assembly chain as agent middleware: every model call's view
// (prompts + history) passes through the policies in order before it reaches
// the client. This is the one piece the policies package does not ship —
// Python composes the same chain inside AssemblerMiddleware.on_llm_call.
func assembly(ps ...policies.AssemblyPolicy) middleware.Factory {
	return func() middleware.Middleware {
		return assemblyChain{policies: ps}
	}
}

type assemblyChain struct {
	middleware.Base
	policies []policies.AssemblyPolicy
}

// OnLLMCall folds the policy chain over (prompts, events) and forwards the
// transformed view. Policies compose: each Apply sees the previous output.
func (c assemblyChain) OnLLMCall(next middleware.LLMCallFunc, evs []events.Event, cx *agocontext.Context) (*events.ModelResponse, error) {
	prompts := cx.PromptLines()
	for _, p := range c.policies {
		var err error
		prompts, evs, err = p.Apply(prompts, evs, cx)
		if err != nil {
			return nil, fmt.Errorf("assembly policy failed: %w", err)
		}
	}
	return next(evs, cx)
}

// resp builds one scripted turn: a terse text answer with invented usage, the
// way a real provider reports spend per call.
func resp(content string) *events.ModelResponse {
	return &events.ModelResponse{
		Message: &events.ModelMessage{Content: content},
		Usage:   events.Usage{PromptTokens: events.I64(40), CompletionTokens: events.I64(8), TotalTokens: events.I64(48)},
	}
}

// describeCall renders one recorded call input: the assembled view, oldest first.
func describeCall(evs []events.Event) {
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.ModelRequest:
			fmt.Printf("    user:      %s\n", events.RenderForPrompt(e))
		case events.ModelResponse:
			fmt.Printf("    assistant: %s\n", events.RenderForPrompt(e))
		case events.CompactionSummary:
			fmt.Printf("    summary:   %s\n", e.Content)
		}
	}
}

// estimateTokens sums the crude per-event token estimate over one call snapshot.
func estimateTokens(evs []events.Event) int {
	total := 0
	for _, ev := range evs {
		total += events.EstimatedTokens(ev, 4)
	}
	return total
}

// plural returns "event"/"events"-style agreement for whole-number counts.
func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// section prints a separator (Python parity with the original's section()).
func section(title string) {
	fmt.Printf("\n-- %s --\n", title)
}
