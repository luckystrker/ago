// Command knowledge_memory demonstrates persistent agent memory across runs
// (port of the AG2 example 06_journal_companion): a daily-journal agent that
// remembers by writing a rollup of each session into /memory/working.md in a
// knowledge.DiskStore and re-injecting that file as a system prompt before
// every later LLM call. Two sessions run on the same store with different
// freshly built agents; the second one (a brand-new object whose stream holds
// only the new question) "recalls" what the user said in the first session,
// because the state lives in the store — not in conversation history.
//
// Mapping to the Python example's primitives:
//
//	DiskKnowledgeStore            → knowledge.NewDiskStore(tempdir)
//	WorkingMemoryAggregate +      → one LLM rollup call over the finished
//	AggregateTrigger(on_end=True)   session transcript, response written to
//	                                policies.WorkingMemoryPath (AGO-DEVIATION:
//	                                AGo has no aggregate-to-knowledge primitive
//	                                yet, so the example performs the same
//	                                end-of-conversation rollup itself)
//	WorkingMemoryPolicy           → policies.NewWorkingMemoryPolicy(store)
//	ConversationPolicy            → policies.NewConversationPolicy()
//	AssemblerMiddleware           → the small "assembler" middleware below,
//	                                which runs the policies before each model
//	                                call and hands the fragments to the model
//	                                as system-prompt lines
//
// The LLM is a scripted test config (agotest.NewTestConfig), so the example
// runs offline without API keys. The fourth scripted response is written as
// the recall answer; the "what session 2's model call actually saw" section
// prints the assembled system prompt and wire transcript of that call, so the
// injection itself — not just the scripted answer — is visible in the output.
//
// Run: go run ./examples/knowledge_memory
package main

import (
	"context"
	"fmt"
	"os"

	"ago/agent"
	"ago/config"
	agocontext "ago/context"
	"ago/events"
	"ago/knowledge"
	"ago/middleware"
	"ago/policies"
	agotest "ago/testing"
)

// assembler is the AGo stand-in for Python's AssemblerMiddleware: before each
// model call it runs the assembly policies left to right over (prompts,
// events) and appends the resulting prompt fragments to the context's system
// prompt, which is what providers send as the system message. A fragment
// already present on the context (a continuation clones its prompt lines) is
// not added twice.
type assembler struct {
	middleware.Base
	policies []policies.AssemblyPolicy

	// recording, for the example's printout: which fragments this assembler
	// injected, and the system prompt + wire events of the most recent call.
	injected []string
	sysLines []string
	wire     []events.Event
}

// OnLLMCall assembles the call context, then delegates to the rest of the
// pipeline with the filtered event list.
func (a *assembler) OnLLMCall(next middleware.LLMCallFunc, evs []events.Event, cx *agocontext.Context) (*events.ModelResponse, error) {
	var prompts []string
	var err error
	for _, p := range a.policies {
		prompts, evs, err = p.Apply(prompts, evs, cx)
		if err != nil {
			return nil, err
		}
	}
	have := map[string]bool{}
	for _, line := range cx.PromptLines() {
		have[line] = true
	}
	for _, fr := range prompts {
		if !have[fr] {
			cx.AddPrompt(fr)
			a.injected = append(a.injected, fr)
		}
	}
	a.sysLines = cx.PromptLines()
	a.wire = append([]events.Event(nil), evs...)
	return next(evs, cx)
}

// journalAgent pairs an agent with its assembler so main can print what the
// model call saw. Two buildAgent calls share nothing but the config script
// and the store — exactly the "new Agent instance, same store" setup.
type journalAgent struct {
	agent *agent.Agent
	asm   *assembler
}

func buildAgent(cfg config.ModelConfig, store knowledge.Store) journalAgent {
	asm := &assembler{policies: []policies.AssemblyPolicy{
		policies.NewWorkingMemoryPolicy(store), // inject /memory/working.md on every LLM call
		policies.NewConversationPolicy(),       // then filter to conversation events
	}}
	a := agent.New("journal",
		"You are a supportive daily journal companion. Keep a running understanding of "+
			"what the user is working on. Be brief and reference their past entries when relevant.",
		agent.WithConfig(cfg),
		agent.WithMiddleware(func() middleware.Middleware { return asm }),
	)
	return journalAgent{agent: a, asm: asm}
}

func section(title string) {
	fmt.Printf("\n── %s ───\n", title)
}

func main() {
	// One scripted ModelResponse per model call, handed out in order across
	// both agents and the rollup (TestConfig shares one script cursor):
	// session 1 ask 1, session 1 ask 2, end-of-session rollup, session 2 ask.
	cfg := agotest.NewTestConfig(
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: "Nice start — a home espresso setup is a great project. " +
				"I'll keep track: you're choosing between a Silvia Pro and a Linea Mini."},
			Usage: events.Usage{PromptTokens: events.I64(41), CompletionTokens: events.I64(24), TotalTokens: events.I64(65)},
		},
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: "Noted. Your running picture so far: the espresso rig decision " +
				"(Silvia Pro vs Linea Mini) and The Pragmatic Programmer, chapter 2 on orthogonality."},
			Usage: events.Usage{PromptTokens: events.I64(97), CompletionTokens: events.I64(28), TotalTokens: events.I64(125)},
		},
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: "# Working memory\n\n" +
				"- Home espresso setup in progress: deciding between a Silvia Pro and a Linea Mini.\n" +
				"- Reading The Pragmatic Programmer; on chapter 2 (orthogonality)."},
			Usage: events.Usage{PromptTokens: events.I64(150), CompletionTokens: events.I64(38), TotalTokens: events.I64(188)},
		},
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: "You were setting up a home espresso rig — still choosing between " +
				"a Silvia Pro and a Linea Mini — and reading The Pragmatic Programmer, chapter 2 on orthogonality."},
			Usage: events.Usage{PromptTokens: events.I64(203), CompletionTokens: events.I64(27), TotalTokens: events.I64(230)},
		},
	)

	// A fresh tempdir on real disk, so "between runs" is literal and the
	// example is reproducible.
	workdir, err := os.MkdirTemp("", "journal-companion-")
	if err != nil {
		fmt.Println("tempdir:", err)
		return
	}
	defer os.RemoveAll(workdir)
	store := knowledge.NewDiskStore(workdir)

	section("Session 1 — tell the journal what you're doing")

	a1 := buildAgent(cfg, store)
	r1, err := a1.agent.Ask(context.Background(),
		"Today I started learning to build a home espresso setup. Still choosing between a Silvia Pro and a Linea Mini.")
	if err != nil {
		fmt.Println("session 1, ask 1 failed:", err)
		return
	}
	fmt.Println("journal:", r1.Body())

	r2, err := r1.Ask(context.Background(),
		"Also started reading The Pragmatic Programmer. On chapter 2 about orthogonality. That's the whole update.")
	if err != nil {
		fmt.Println("session 1, ask 2 failed:", err)
		return
	}
	fmt.Println("journal:", r2.Body())

	// The conversation is over; roll it up into working memory. Python does
	// this inside WorkingMemoryAggregate (fired by AggregateTrigger(on_end=True)):
	// one LLM call over the finished transcript, response written to
	// /memory/working.md. AGo has no aggregate-to-knowledge primitive yet, so
	// the example drives the same two steps by hand.
	section("End of session 1 — roll up into /memory/working.md (aggregate, on_end)")

	client, err := cfg.Create()
	if err != nil {
		fmt.Println("create rollup client:", err)
		return
	}
	transcript, err := r2.History().GetEvents()
	if err != nil {
		fmt.Println("read session transcript:", err)
		return
	}
	rollupAsk := events.EnsureRequest(
		"Roll up this journal session into working-memory notes for future sessions: 2-3 bullet lines, facts only.")
	rollup, err := client.Call(context.Background(),
		append([]events.Event{rollupAsk}, transcript...), nil, config.CallOptions{})
	if err != nil {
		fmt.Println("rollup call failed:", err)
		return
	}
	if err := store.Write(policies.WorkingMemoryPath, []byte(rollup.Content())); err != nil {
		fmt.Println("write working.md:", err)
		return
	}
	fmt.Println("wrote", policies.WorkingMemoryPath)

	working, err := store.Read(policies.WorkingMemoryPath)
	if err != nil {
		fmt.Println("read working.md back:", err)
		return
	}
	fmt.Printf("\n## %s after session 1 (store root: %s)\n%s\n", policies.WorkingMemoryPath, workdir, working)

	section("Session 2 — new Agent instance, same store: memory persists")

	// agent2 is built from scratch: new object, new empty stream. The only
	// thing it shares with agent1 is the store directory.
	a2 := buildAgent(cfg, store)
	r3, err := a2.agent.Ask(context.Background(), "Quick check-in: what was I working on? Answer in one line.")
	if err != nil {
		fmt.Println("session 2 failed:", err)
		return
	}
	fmt.Println("journal:", r3.Body())
	fmt.Println("(the answer mentions espresso and The Pragmatic Programmer even though this agent's " +
		"stream holds only the question — WorkingMemoryPolicy injected " + policies.WorkingMemoryPath + ")")

	// Proof of the injection: the assembled context of session 2's model call.
	// With a real provider this is exactly what would go over the wire.
	fmt.Println("\n## what session 2's model call actually saw")
	injected := map[string]bool{}
	for _, fr := range a2.asm.injected {
		injected[fr] = true
	}
	for _, line := range a2.asm.sysLines {
		if injected[line] {
			fmt.Printf("[system] %s\n      ^ injected by WorkingMemoryPolicy from %s\n", line, policies.WorkingMemoryPath)
		} else {
			fmt.Println("[system]", line)
		}
	}
	for _, ev := range a2.asm.wire {
		switch e := events.AsValue(ev).(type) {
		case events.ModelRequest:
			fmt.Println("[user]  ", events.RenderForPrompt(e))
		case events.ModelResponse:
			if e.Content() != "" {
				fmt.Println("[assistant]", e.Content())
			}
		}
	}
}
