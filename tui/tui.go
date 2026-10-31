// Package tui is a terminal chat loop over an agent (Python parity:
// ag2/textual.py — TUIAgent on the textual App, rebuilt as a plain stdlib line
// REPL because the core module carries no TUI framework, D-003/D-056).
package tui

import (
	"bufio"
	stdcontext "context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
)

// prompt is written to out before every read, mirroring the textual Input
// widget's "type here" affordance.
const prompt = "> "

const helpText = `Commands:
  /help  show this help
  /exit  leave the chat (Ctrl-D works too)`

// REPL runs the read-eval-print chat loop against a: it reads lines from in,
// sends each one as the next user turn, and writes the answer to out. The
// first turn goes through Agent.Ask, every later turn continues the same
// conversation through AgentReply.Ask, so history accumulates across the
// session (Python parity: `self.conversation.ask(text) if self.conversation
// else self.agent.ask(text)`).
//
// Commands: /help prints the command list, /exit (or end of input) ends the
// loop with a nil error. A failed turn is reported on out and the loop
// continues, so one bad answer does not kill the session; the error is
// returned only when writing to out itself fails.
func REPL(a *agent.Agent, in io.Reader, out io.Writer, opts ...Option) error {
	var o options
	for _, f := range opts {
		f(&o)
	}

	sc := bufio.NewScanner(in)
	// Long pastes are normal chat input; the default 64 KiB cap is not.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var convo *agent.AgentReply
	for {
		if _, err := fmt.Fprint(out, prompt); err != nil {
			return fmt.Errorf("tui: write prompt: %w", err)
		}
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return fmt.Errorf("tui: read input: %w", err)
			}
			return nil
		}
		text := strings.TrimSpace(sc.Text())
		switch strings.ToLower(text) {
		case "":
			continue
		case "/exit":
			return nil
		case "/help":
			if _, err := fmt.Fprintln(out, helpText); err != nil {
				return fmt.Errorf("tui: write help: %w", err)
			}
			continue
		}

		var err error
		if o.streaming {
			convo, err = streamingTurn(a, convo, text, out)
		} else {
			convo, err = blockingTurn(a, convo, text, out)
		}
		if err != nil {
			// The turn failed; keep the previous conversation so the user can
			// retry, and stay in the loop.
			// Error texts from providers can echo untrusted response bodies,
			// so they go through the same terminal sanitizer.
			if _, werr := fmt.Fprintf(out, "error: %s\n", sanitizeTerminal(err.Error())); werr != nil {
				return fmt.Errorf("tui: write error: %w", werr)
			}
		}
	}
}

// blockingTurn runs one turn through Ask and prints the final answer.
func blockingTurn(a *agent.Agent, prev *agent.AgentReply, text string, out io.Writer) (*agent.AgentReply, error) {
	ctx := stdcontext.Background() // the REPL signature carries no context
	reply, err := ask(a, prev, ctx, text)
	if err != nil {
		return prev, err
	}
	if body := reply.Body(); body != "" {
		// Model text is untrusted; strip terminal control data before it
		// reaches out (audit H-10, ANSI/OSC injection).
		if _, err := fmt.Fprintln(out, sanitizeTerminal(body)); err != nil {
			return reply, fmt.Errorf("tui: write reply: %w", err)
		}
	}
	return reply, nil
}

// streamingTurn runs one turn through Run with a subscription on the turn's
// stream: every ModelMessageChunk delta is written to out as it arrives
// (Python parity: TUIAgent's ModelMessageChunk sub_scope appending into the
// assistant block). The final body is printed only when it differs from the
// joined deltas — the same rule acp's ServeAgent uses to avoid re-sending.
func streamingTurn(a *agent.Agent, prev *agent.AgentReply, text string, out io.Writer) (*agent.AgentReply, error) {
	ctx := stdcontext.Background() // the REPL signature carries no context
	run, err := runTurn(a, prev, ctx, text)
	if err != nil {
		return prev, err
	}
	defer run.Close()

	var mu sync.Mutex
	var streamed strings.Builder
	// san carries an incomplete escape sequence across deltas, so a sequence
	// split by the provider's chunking is swallowed whole instead of leaking
	// half of it (audit H-10).
	var san sanitizer
	sub := run.Stream().Subscribe(func(ev events.Event) error {
		chunk, ok := events.AsValue(ev).(events.ModelMessageChunk)
		if !ok {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		streamed.WriteString(chunk.Content)
		if _, err := io.WriteString(out, san.push(chunk.Content)); err != nil {
			return fmt.Errorf("tui: write chunk: %w", err)
		}
		return nil
	}, stream.SubscribeOptions{Condition: events.TypeOf[events.ModelMessageChunk]()})
	defer run.Stream().Unsubscribe(sub)

	reply, err := run.Result(ctx)
	if err != nil {
		return prev, err
	}
	mu.Lock()
	joined := streamed.String()
	mu.Unlock()
	if joined != "" {
		// Terminate the streamed line before anything else is printed.
		if _, err := fmt.Fprintln(out); err != nil {
			return reply, fmt.Errorf("tui: end streamed line: %w", err)
		}
	}
	if body := reply.Body(); body != "" && body != joined {
		// Untrusted model text; sanitized like every other write (H-10).
		if _, err := fmt.Fprintln(out, sanitizeTerminal(body)); err != nil {
			return reply, fmt.Errorf("tui: write reply: %w", err)
		}
	}
	return reply, nil
}

func ask(a *agent.Agent, prev *agent.AgentReply, ctx stdcontext.Context, text string) (*agent.AgentReply, error) {
	if prev == nil {
		return a.Ask(ctx, text)
	}
	return prev.Ask(ctx, text)
}

func runTurn(a *agent.Agent, prev *agent.AgentReply, ctx stdcontext.Context, text string) (*agent.AgentRun, error) {
	if prev == nil {
		return a.Run(ctx, text)
	}
	return prev.Run(ctx, text)
}

type options struct {
	streaming bool
}

// Option configures REPL.
type Option func(*options)

// WithStreaming turns on live echo of streaming deltas: the turn runs through
// Agent.Run / AgentReply.Run with a ModelMessageChunk subscription and each
// delta lands on out as produced, instead of only the final answer. A model
// client that does not stream simply produces no deltas and the final body is
// printed as usual.
func WithStreaming() Option { return func(o *options) { o.streaming = true } }
