package acp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/tools"
)

// --- the LLM client ----------------------------------------------------------

// llmClient is the config.LLMClient of a Config; the sessions live in the
// config, so a fresh client per model turn (Create is called per turn) still
// reaches the conversation's live session.
type llmClient struct{ cfg *Config }

var _ config.LLMClient = (*llmClient)(nil)

// Call performs one model turn: the requests the agent has not yet seen are
// sent as one session/prompt, the agent's updates stream onto conv, and the
// accumulated text becomes the ModelResponse.
func (l *llmClient) Call(ctx context.Context, messages []events.Event, conv *agocontext.Context, opts config.CallOptions) (*events.ModelResponse, error) {
	c := l.cfg
	var (
		functions []tools.Schema
		external  []MCPServer
	)
	if !c.DisableTools {
		var err error
		if functions, external, err = partitionTools(opts.Tools); err != nil {
			return nil, err
		}
	}
	if conv == nil {
		return nil, fmt.Errorf("%w: acp needs a conversation context", config.ErrNotProvided)
	}
	// Refuse input the agent cannot take before any subprocess is started.
	if _, _, err := newPromptText(messages, 0); err != nil {
		return nil, err
	}

	sess, err := c.sessionFor(ctx, conv, functions, external)
	if err != nil {
		return nil, err
	}
	text, total, err := newPromptText(messages, sess.sent)
	if err != nil {
		return nil, err
	}

	turn := newTurnState(ctx, conv)
	sess.setTurn(turn)
	if sess.host != nil {
		sess.host.setTurn(ctx, conv)
	}
	defer sess.setTurn(nil)

	out := sess.drive(ctx, turn, text)

	switch {
	case out.ctxErr != nil:
		c.dropSession(conv, sess)
		return nil, out.ctxErr
	case out.err != nil:
		// The connection is gone, so the session it carried is gone too: drop
		// it rather than leave a started session with a dead pipe that the
		// next turn would reuse. Nothing is resumed or replayed.
		c.dropSession(conv, sess)
		_ = sess.close()
		if isTransportErr(out.err) {
			return nil, &TransportError{Transport: "stdio", Err: out.err}
		}
		return nil, fmt.Errorf("acp: prompt: %w", out.err)
	}
	if out.hardStopped {
		c.dropSession(conv, sess)
	}
	if cerr := turn.failure(); cerr != nil {
		return nil, fmt.Errorf("%w: %w", ErrChannelFailed, cerr)
	}
	if !out.hardStopped {
		sess.sent = total
	}

	partial, files, worked := turn.snapshot()
	reply := out.res.Text
	if out.hardStopped {
		// No prompt response arrived; what streamed is all there is.
		reply = partial
	}
	finish := out.res.StopReason
	if out.timedOut {
		finish = "timeout"
	}
	model := sess.model
	if model == "" {
		model = c.ModelName
	}
	if finish == StopEndTurn && reply == "" && !worked && len(files) == 0 {
		// Some CLI agents end a turn this way when the provider call failed on
		// their side; nothing reaches the ACP wire, so the empty reply would
		// otherwise be the only clue.
		slog.Warn("ACP agent ended the turn with stop_reason=end_turn but produced no output; "+
			"the agent may have failed the provider call silently — check its own logs, and that the model is "+
			"spelled right and authorized", "agent", c.agentLabel(), "model", model)
	}
	return &events.ModelResponse{
		Message:      &events.ModelMessage{Content: reply},
		Files:        files,
		Usage:        mapUsage(out.res.Usage),
		FinishReason: finish,
		Provider:     string(ProviderACP),
		Model:        model,
	}, nil
}

// newPromptText renders the ModelRequests beyond sent as the prompt text and
// returns the total request count (the new high-water mark). Each part keeps
// its own line.
//
// AGO-DEVIATION: Python sends one content block per part and carries images,
// audio and documents when the agent's promptCapabilities allow; the Bridge
// sends a single text block, so a non-text part fails fast with
// config.ErrUnsupportedInput instead of being dropped.
func newPromptText(messages []events.Event, sent int) (string, int, error) {
	var requests []events.ModelRequest
	for _, m := range messages {
		switch r := events.AsValue(m).(type) {
		case events.ModelRequest:
			requests = append(requests, r)
		case events.DrainedModelRequest:
			requests = append(requests, r.ModelRequest)
		}
	}
	var lines []string
	for i := sent; i < len(requests); i++ {
		for _, part := range requests[i].Parts {
			t, ok := events.AsValue(part).(events.TextInput)
			if !ok {
				return "", 0, fmt.Errorf("%w: ACP prompts carry text only, got %T "+
					"(images, audio and documents are not forwarded to the agent)", config.ErrUnsupportedInput, part)
			}
			lines = append(lines, t.Content)
		}
	}
	return strings.Join(lines, "\n"), len(requests), nil
}

// mapUsage maps the prompt response's token accounting onto AGo's Usage
// (Python parity: mappers.map_usage).
func mapUsage(u *PromptUsage) events.Usage {
	if u == nil {
		return events.Usage{}
	}
	return events.Usage{
		PromptTokens:             u.InputTokens,
		CompletionTokens:         u.OutputTokens,
		TotalTokens:              u.TotalTokens,
		CacheReadInputTokens:     u.CachedReadTokens,
		CacheCreationInputTokens: u.CachedWriteTokens,
		ThinkingTokens:           u.ThoughtTokens,
	}
}

// isTransportErr reports whether err says the connection to the agent broke
// (the process exited or its pipes closed) rather than the agent answering
// with a protocol error of its own.
func isTransportErr(err error) bool {
	return errors.Is(err, ErrAgentExited) || errors.Is(err, ErrClosed) || errors.Is(err, ErrTimeout) ||
		errors.Is(err, io.ErrClosedPipe) || errors.Is(err, os.ErrClosed) || errors.Is(err, syscall.EPIPE)
}

// --- driving one turn --------------------------------------------------------

// turnOutcome is how one prompt turn ended.
type turnOutcome struct {
	res         TurnResult
	err         error // the exchange failed (transport or protocol error)
	ctxErr      error // the caller's context ended the turn
	timedOut    bool  // the turn deadline stopped it
	hardStopped bool  // the agent ignored the cancel; the process was killed
}

// drive runs one session/prompt and waits for it, ending it early when the
// deadline passes or a tool's human channel dies. Prefer cooperative
// cancellation: signal session/cancel and let the agent return the in-flight
// prompt with the cancelled stop reason; cancelling outright would corrupt the
// JSON-RPC stream, so past the grace the process is killed instead — the next
// turn then opens a new session.
func (s *providerSession) drive(ctx context.Context, turn *turnState, text string) turnOutcome {
	type result struct {
		res TurnResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := s.b.PromptTurn(s.id, text)
		done <- result{res, err}
	}()

	var deadline <-chan time.Time
	if s.cfg.TurnTimeout > 0 {
		t := time.NewTimer(s.cfg.TurnTimeout)
		defer t.Stop()
		deadline = t.C
	}

	timedOut := false
	select {
	case r := <-done:
		return turnOutcome{res: r.res, err: r.err}
	case <-ctx.Done():
		// The caller is gone and nobody will read a stop reason: no
		// cooperative attempt, only cleanup.
		s.kill()
		<-done
		return turnOutcome{ctxErr: ctx.Err()}
	case <-deadline:
		timedOut = true
	case <-turn.failed:
	}

	// Ask the agent to stop, then give it a bounded grace to return the prompt.
	_ = s.b.Cancel(s.id)
	grace := s.cfg.CancelTimeout
	if grace <= 0 {
		grace = defaultCancelTimeout
	}
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case r := <-done:
		return turnOutcome{res: r.res, err: r.err, timedOut: timedOut}
	case <-ctx.Done():
		s.kill()
		<-done
		return turnOutcome{ctxErr: ctx.Err()}
	case <-t.C:
		s.kill()
		<-done
		return turnOutcome{timedOut: timedOut, hardStopped: true}
	}
}
