// Command observers demonstrates the watchdog layer: three observers attached to
// one conversation — the built-ins TokenMonitor (token budget over UsageEvent
// telemetry) and LoopDetector (repeated model responses), plus a hand-written
// AlertConsole that subscribes to ObserverAlert events and prints each one as a
// dashboard line. Thresholds are deliberately low and the "model" is a scripted
// test config whose writer repeats itself and overspends, so both built-ins trip
// offline, without API keys.
//
// AGo shape notes versus the Python 04_token_watchdog example:
//   - There is no observers=[...] agent option (docs/DECISIONS.md D-025): an
//     observer registers itself on the conversation stream, so the example owns
//     the stream, registers the observers and points the agent at the same
//     stream with WithStreamBase.
//   - TokenMonitor carries a single budget threshold (Python has warn + alert);
//     it fires once, above the budget, with severity "error".
//   - LoopDetector pins repeated ModelResponse text (Python watched repeated
//     tool-call pairs), and a tool-free turn is one model call — so the script
//     replays the same paragraph on a second turn to reproduce the looping
//     writer it is meant to catch.
//
// Run: go run ./examples/observers
package main

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/luckystrker/ago/agent"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/observers"
	"github.com/luckystrker/ago/stream"
	agotest "github.com/luckystrker/ago/testing"
)

func main() {
	section("Watchdog — low thresholds so observers trip on a short scripted run")

	// The scripted "writer" answers every prompt with the same paragraph and
	// bills 120 tokens per call — exactly the pathology the watchdog exists for.
	paragraph := "Cherry blossoms drift across the Kamo river, petals settling on dark water; " +
		"temple bells soften the evening air, and lanterns bloom along stone lanes as Kyoto " +
		"shrugs off the last winter chill."
	cfg := agotest.NewTestConfig(scriptedReply(paragraph), scriptedReply(paragraph))

	// Budget 100 tokens: the first call (120) is already overspend. Repeats 2:
	// the second identical response is a loop.
	monitor := observers.NewTokenMonitor(100)
	detector := observers.NewLoopDetector(2)
	console := &AlertConsole{}

	// AGo registers observers on the conversation stream, not on the agent: own
	// the stream, subscribe the watchdog, then run the agent on that stream.
	s := stream.NewMemoryStream()
	conv := agocontext.New(s)
	for _, o := range []observers.Observer{monitor, detector, console} {
		if err := o.Register(conv); err != nil {
			fmt.Println("register observer:", err)
			return
		}
	}

	writer := agent.New("writer",
		"Write prose the user asks for. Favour variety — never repeat the same sentence twice.",
		agent.WithConfig(cfg),
		agent.WithStreamBase(s),
	)

	fmt.Println("user: Write three distinct 30-word paragraphs about springtime in Kyoto.")
	reply, err := writer.Ask(context.Background(), "Write three distinct 30-word paragraphs about springtime in Kyoto.")
	if err != nil {
		fmt.Println("turn 1 failed:", err)
		return
	}

	section("Turn 2 — the scripted writer repeats itself anyway")
	fmt.Println("user: Again — and make it different this time.")
	reply, err = reply.Ask(context.Background(), "Again — and make it different this time.")
	if err != nil {
		fmt.Println("turn 2 failed:", err)
		return
	}
	fmt.Println("assistant:", reply.Body())

	fmt.Println()
	fmt.Println("Final reply (truncated):")
	fmt.Printf("    %s\n", truncate(reply.Body(), 240))
	fmt.Println()
	fmt.Println("Total tokens tracked by TokenMonitor:", monitor.Total())
	fmt.Println("Alerts emitted this run:             ", len(console.Seen()))
}

// scriptedReply builds one model response: the paragraph plus invented usage,
// exactly like a real provider would report it.
func scriptedReply(body string) *events.ModelResponse {
	return &events.ModelResponse{
		Message: &events.ModelMessage{Content: body},
		Usage:   events.Usage{PromptTokens: events.I64(40), CompletionTokens: events.I64(80), TotalTokens: events.I64(120)},
	}
}

// AlertConsole is the hand-written observer: it implements observers.Observer,
// wakes on every ObserverAlert on the stream, prints a formatted line and keeps
// the alerts for the end-of-run tally. (Python: a BaseObserver subclass armed
// with EventWatch(ObserverAlert).) Register runs once per conversation and
// Seens reads race with the stream, so the slice is guarded.
type AlertConsole struct {
	mu   sync.Mutex
	seen []events.ObserverAlert
}

// Name identifies the console in diagnostics.
func (c *AlertConsole) Name() string { return "alert_console" }

// Register subscribes the console on the conversation stream; alerts dispatched
// later (including alerts sent inline by other observers) reach the callback.
func (c *AlertConsole) Register(ctx *agocontext.Context) error {
	if ctx == nil {
		return observers.ErrNilContext
	}
	if ctx.Stream == nil {
		return observers.ErrNoStream
	}
	ctx.Stream.Subscribe(func(ev events.Event) error {
		alert, ok := events.AsValue(ev).(events.ObserverAlert)
		if !ok {
			return nil
		}
		c.mu.Lock()
		c.seen = append(c.seen, alert)
		c.mu.Unlock()
		fmt.Printf("    [%-8s] %s: %s\n",
			strings.ToUpper(string(alert.Severity)), alert.Source, alert.Message)
		return nil
	}, stream.SubscribeOptions{Condition: events.TypeOf[events.ObserverAlert]()})
	return nil
}

// Seen returns the alerts collected so far, oldest first.
func (c *AlertConsole) Seen() []events.ObserverAlert {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]events.ObserverAlert, len(c.seen))
	copy(out, c.seen)
	return out
}

// section prints a story heading (Python parity: the section helper).
func section(title string) {
	fmt.Printf("\n── %s ───\n", title)
}

// truncate cuts s to at most n runes without splitting one.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + " ..."
	}
	return s
}
