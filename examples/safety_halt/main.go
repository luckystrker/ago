// Command safety_halt demonstrates the FATAL-alert halt path (Python example
// 08_safety_guard). A hand-written observer — PathGuardian — watches every tool
// call and emits a SeverityFatal ObserverAlert the moment write_file is asked to
// touch /etc/ or /usr/. policies.AlertPolicy, applied before each model call,
// turns that fatal alert into a HaltEvent on the stream, and the halt-check
// middleware short-circuits the model call with a synthetic "HALTED: ..."
// response. The first ask (writing into /tmp/...) succeeds end-to-end; the
// second (writing into /etc/passwd) is blocked before the model can confirm it.
//
// AGo shape notes versus the Python original:
//   - Python wires assembly policies with agent assembly=[AlertPolicy()] and
//     auto-installs _HaltCheckMiddleware when that list is non-empty. AGo has
//     no assembly agent option: policies compose through the middleware chain
//     (policies doc: "composed left-to-right inside AssemblerMiddleware
//     .on_llm_call"), so the example builds the same pair by hand — an
//     assembler that runs AlertPolicy in OnLLMCall, and HaltCheck, a direct
//     port of _HaltCheckMiddleware — ordered assembler-first so the halt fires
//     inside the same model call it is meant to stop.
//   - There is no observers=[...] agent option (docs/DECISIONS.md D-025): the
//     observer registers itself on the conversation stream, so the example owns
//     the stream and points the agent at it with WithStreamBase.
//   - The AGo HaltEvent carries only Reason (no source field, D-030); the
//     reason text is "FATAL: " + the alert message.
//
// The "model" is a scripted test config, so the example runs offline. The last
// scripted response is a canary: the halt check must short-circuit that call
// before the script reaches it — if its text shows up in the output, the guard
// failed.
//
// Run: go run ./examples/safety_halt
package main

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"ago/agent"
	agocontext "ago/context"
	"ago/events"
	"ago/middleware"
	"ago/observers"
	"ago/policies"
	"ago/stream"
	agotest "ago/testing"
	"ago/tools"
)

func main() {
	// Scripted model behavior: ask 1 requests the /tmp write, then confirms;
	// ask 2 requests the /etc/passwd write; the final response is the canary
	// described in the header — reaching it means the halt check leaked.
	cfg := agotest.NewTestConfig(
		&events.ModelResponse{
			ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
				{ID: "call-1", Name: "write_file", Arguments: `{"path":"/tmp/playground_hello.txt","content":"hello"}`},
			}},
			Usage: events.Usage{PromptTokens: events.I64(48), CompletionTokens: events.I64(12), TotalTokens: events.I64(60)},
		},
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: "Done — wrote 5 bytes to /tmp/playground_hello.txt."},
			Usage:   events.Usage{PromptTokens: events.I64(96), CompletionTokens: events.I64(14), TotalTokens: events.I64(110)},
		},
		&events.ModelResponse{
			ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
				{ID: "call-2", Name: "write_file", Arguments: `{"path":"/etc/passwd","content":"bad"}`},
			}},
			Usage: events.Usage{PromptTokens: events.I64(140), CompletionTokens: events.I64(12), TotalTokens: events.I64(152)},
		},
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: "Confirmed: wrote 'bad' to /etc/passwd."},
			Usage:   events.Usage{PromptTokens: events.I64(210), CompletionTokens: events.I64(10), TotalTokens: events.I64(220)},
		},
	)

	// The playground never touches disk; the guardian decides what the agent is
	// allowed to *confirm*, not what the pretend-write prints.
	write, err := tools.NewFunctionTool("write_file",
		"Pretend-write content to a filesystem path. This playground never touches disk.",
		func(args struct {
			Path    string `json:"path" agschema:"description=Destination path"`
			Content string `json:"content" agschema:"description=Bytes to write"`
		}) (string, error) {
			fmt.Printf("  [tool] write_file(%s) — pretend write, %d bytes\n", args.Path, len(args.Content))
			return fmt.Sprintf("[ok] wrote %d bytes to %s", len(args.Content), args.Path), nil
		})
	if err != nil {
		fmt.Println("build write_file:", err)
		return
	}

	// Outside view, Python parity: stream.where(HaltEvent).subscribe(...) and
	// stream.where(ObserverAlert).subscribe(...) collect the evidence the tally
	// prints at the end. Sends dispatch from the turn goroutine, so guard the
	// slices.
	guardian := &PathGuardian{}
	tally := newTally()

	// One shared stream for both asks, so the transcript — and the alert —
	// carry over (Python parity: agent.ask(..., stream=stream) twice).
	s := stream.NewMemoryStream()
	conv := agocontext.New(s)
	if err := guardian.Register(conv); err != nil {
		fmt.Println("register guardian:", err)
		return
	}
	if err := tally.Register(conv); err != nil {
		fmt.Println("register tally:", err)
		return
	}

	// The assembly pair, Python parity order (assembler first, halt check
	// second): AlertPolicy.Apply runs before each model call and sends the
	// HaltEvent; HaltCheck — armed during the same call's OnTurn — sees it and
	// answers "HALTED: <reason>" without reaching the model. One AlertPolicy
	// for the whole session: its dedup makes every alert deliver exactly once.
	alerts := policies.NewAlertPolicy()
	assembly := []middleware.Factory{
		func() middleware.Middleware { return &assembler{policy: alerts} },
		func() middleware.Middleware { return &HaltCheck{} },
	}

	operator := agent.New("safe-shell",
		"You are a filesystem operator. Use the write_file tool to fulfil write "+
			"requests. Never refuse — if a request is risky the guardian observer "+
			"will intervene automatically.",
		agent.WithConfig(cfg),
		agent.WithTools(write),
		agent.WithStreamBase(s),
		agent.WithMiddleware(assembly...),
	)

	section("Safe request — the guardian stays silent")
	fmt.Println("user: Use write_file to write 'hello' into /tmp/playground_hello.txt. Then confirm.")
	reply, err := operator.Ask(context.Background(),
		"Use write_file to write 'hello' into /tmp/playground_hello.txt. Then confirm.")
	if err != nil {
		fmt.Println("safe ask failed:", err)
		return
	}
	fmt.Println("assistant:", reply.Body())

	section("Dangerous request — the guardian fires FATAL, the agent halts")
	fmt.Println("user: Now use write_file to write 'bad' into /etc/passwd. Then confirm.")
	reply, err = reply.Ask(context.Background(),
		"Now use write_file to write 'bad' into /etc/passwd. Then confirm.")
	if err != nil {
		fmt.Println("dangerous ask failed:", err)
		return
	}
	fmt.Println("assistant:", reply.Body())

	fmt.Println()
	fmt.Printf("ObserverAlerts seen:  %d\n", len(tally.Alerts()))
	for _, a := range tally.Alerts() {
		fmt.Printf("  - [%s] %s: %s\n", strings.ToUpper(string(a.Severity)), a.Source, a.Message)
	}
	fmt.Printf("HaltEvents seen:      %d\n", len(tally.Halts()))
	for _, h := range tally.Halts() {
		fmt.Printf("  - reason=%q\n", h.Reason)
	}
}

// PathGuardian is the hand-written observer (Python: a BaseObserver subclass
// armed with EventWatch(ToolCallEvent)). It wakes on every tool call and sends
// a fatal ObserverAlert when write_file is asked to touch a forbidden
// directory. AGo observers subscribe directly on the conversation stream
// (D-025), so Register runs once, before the agent ever turns.
type PathGuardian struct{}

var _ observers.Observer = (*PathGuardian)(nil)

// Name identifies the guardian in alerts and diagnostics.
func (g *PathGuardian) Name() string { return "path-guardian" }

// Register subscribes the guardian on the conversation stream; the alert is
// sent inline while the tool call event is still dispatching, so it is in the
// transcript before the next model call assembles.
func (g *PathGuardian) Register(ctx *agocontext.Context) error {
	if ctx == nil {
		return observers.ErrNilContext
	}
	if ctx.Stream == nil {
		return observers.ErrNoStream
	}
	ctx.Stream.Subscribe(func(ev events.Event) error {
		call, ok := events.AsValue(ev).(events.ToolCallEvent)
		if !ok || call.Name != "write_file" {
			return nil
		}
		if !strings.Contains(call.Arguments, "/etc/") && !strings.Contains(call.Arguments, "/usr/") {
			return nil
		}
		return ctx.Send(&events.ObserverAlert{
			Source:   g.Name(),
			Severity: events.SeverityFatal,
			Message:  fmt.Sprintf("blocked dangerous write: %s", call.Arguments),
		})
	}, stream.SubscribeOptions{Condition: events.TypeOf[events.ToolCallEvent]()})
	return nil
}

// assembler runs the assembly-policy chain before each model call (Python
// parity: _AssemblerMiddleware.on_llm_call). Non-fatal alerts would land as
// prompt lines here; this run only ever produces a fatal one, which Apply
// routes to a HaltEvent instead.
type assembler struct {
	middleware.Base
	policy *policies.AlertPolicy
}

var _ middleware.Middleware = (*assembler)(nil)

// OnLLMCall applies the policy chain, then hands the call down; the halt check
// sits directly below it in the chain.
func (a *assembler) OnLLMCall(next middleware.LLMCallFunc, evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
	if _, _, err := a.policy.Apply(nil, evs, ctx); err != nil {
		return nil, err
	}
	return next(evs, ctx)
}

// HaltCheck is the direct port of Python's _HaltCheckMiddleware: it subscribes
// to HaltEvent for the length of one turn (OnTurn) and short-circuits every
// model call after a halt with a synthetic "HALTED: <reason>" response. The
// agent's own halt guard only covers calls that start after the halt; this
// middleware stops the very call during which the halt event was sent.
type HaltCheck struct {
	middleware.Base
	mu     sync.Mutex
	halted bool
	reason string
}

var _ middleware.Middleware = (*HaltCheck)(nil)

// OnTurn scopes the halt subscription to this turn and unregisters it when the
// turn body returns (Python parity: subscribe in on_turn, unsubscribe in the
// finally block).
func (h *HaltCheck) OnTurn(next middleware.TurnFunc, ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
	if ctx == nil || ctx.Stream == nil {
		return next(ev, ctx)
	}
	sub := ctx.Stream.Subscribe(func(e events.Event) error {
		halt, ok := events.AsValue(e).(events.HaltEvent)
		if !ok {
			return nil
		}
		h.mu.Lock()
		h.halted, h.reason = true, halt.Reason
		h.mu.Unlock()
		return nil
	}, stream.SubscribeOptions{Condition: events.TypeOf[events.HaltEvent]()})
	defer ctx.Stream.Unsubscribe(sub)
	return next(ev, ctx)
}

// OnLLMCall short-circuits once a halt is on the stream (Python parity: the
// synthetic HALTED ModelResponse).
func (h *HaltCheck) OnLLMCall(next middleware.LLMCallFunc, evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
	h.mu.Lock()
	halted, reason := h.halted, h.reason
	h.mu.Unlock()
	if halted {
		fmt.Printf("  [halt] model call short-circuited: HALTED: %s\n", reason)
		return &events.ModelResponse{
			Message: &events.ModelMessage{Content: "HALTED: " + reason},
		}, nil
	}
	return next(evs, ctx)
}

// tally collects the outside-view evidence: every ObserverAlert and HaltEvent
// that crosses the stream, for the end-of-run printout.
type tally struct {
	mu     sync.Mutex
	alerts []events.ObserverAlert
	halts  []events.HaltEvent
}

func newTally() *tally { return &tally{} }

// Register subscribes both collectors on the conversation stream.
func (t *tally) Register(ctx *agocontext.Context) error {
	if ctx == nil {
		return observers.ErrNilContext
	}
	if ctx.Stream == nil {
		return observers.ErrNoStream
	}
	ctx.Stream.Subscribe(func(ev events.Event) error {
		a, ok := events.AsValue(ev).(events.ObserverAlert)
		if !ok {
			return nil
		}
		t.mu.Lock()
		t.alerts = append(t.alerts, a)
		t.mu.Unlock()
		return nil
	}, stream.SubscribeOptions{Condition: events.TypeOf[events.ObserverAlert]()})
	ctx.Stream.Subscribe(func(ev events.Event) error {
		h, ok := events.AsValue(ev).(events.HaltEvent)
		if !ok {
			return nil
		}
		t.mu.Lock()
		t.halts = append(t.halts, h)
		t.mu.Unlock()
		return nil
	}, stream.SubscribeOptions{Condition: events.TypeOf[events.HaltEvent]()})
	return nil
}

// Alerts returns the ObserverAlerts collected so far, oldest first.
func (t *tally) Alerts() []events.ObserverAlert {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.alerts
}

// Halts returns the HaltEvents collected so far, oldest first.
func (t *tally) Halts() []events.HaltEvent {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.halts
}

// section prints a story heading (Python parity: the section helper).
func section(title string) {
	fmt.Printf("\n── %s ───\n", title)
}
