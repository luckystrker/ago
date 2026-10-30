package tui

import (
	"bytes"
	stdcontext "context"
	"errors"
	"strings"
	"testing"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	agotest "github.com/luckystrker/ago/testing"
)

func textResp(content string) *events.ModelResponse {
	return &events.ModelResponse{Message: &events.ModelMessage{Content: content}}
}

// textInputs collects the TextInput contents of every scripted call, so tests
// can assert what history each model call saw.
func textInputs(calls [][]events.Event) [][]string {
	out := make([][]string, len(calls))
	for i, call := range calls {
		for _, ev := range call {
			switch v := events.AsValue(ev).(type) {
			case events.ModelRequest:
				for _, part := range v.Parts {
					if t, ok := events.AsValue(part).(events.TextInput); ok {
						out[i] = append(out[i], t.Content)
					}
				}
			}
		}
	}
	return out
}

// TestREPLDialogContinuesHistory: two user lines produce two answers through
// one conversation — the second model call must already see the first user
// turn — and /exit ends the loop cleanly.
func TestREPLDialogContinuesHistory(t *testing.T) {
	cfg := agotest.NewTestConfig(textResp("hello there"), textResp("doing fine"))
	a := agent.New("helper", "You are helpful.", agent.WithConfig(cfg))

	in := strings.NewReader("hi\nhow are you\n/exit\n")
	var out bytes.Buffer
	if err := REPL(a, in, &out); err != nil {
		t.Fatalf("repl: %v", err)
	}

	got := out.String()
	for _, want := range []string{"hello there", "doing fine"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q misses reply %q", got, want)
		}
	}

	calls := cfg.Calls()
	if len(calls) != 2 {
		t.Fatalf("model calls = %d, want 2", len(calls))
	}
	// The continuation reuses one conversation, so the second call sees the
	// first user turn (a fresh ask per line would not).
	second := textInputs(calls)[1]
	if len(second) == 0 || second[0] != "hi" {
		t.Errorf("second call saw text inputs %v, want it to open with the first turn %q", second, "hi")
	}
}

// TestREPLCommandsAndBlankLines: /help prints the command list, blank lines
// never reach the model, and /exit stops before reading further input.
func TestREPLCommandsAndBlankLines(t *testing.T) {
	cfg := agotest.NewTestConfig(textResp("only answer"))
	a := agent.New("helper", "", agent.WithConfig(cfg))

	in := strings.NewReader("\n   \n/help\nhello\n/exit\nthis must not be read\n")
	var out bytes.Buffer
	if err := REPL(a, in, &out); err != nil {
		t.Fatalf("repl: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "/exit") {
		t.Errorf("help output %q misses the command list", got)
	}
	if calls := cfg.Calls(); len(calls) != 1 {
		t.Fatalf("model calls = %d, want 1 (blank lines and /help must not ask)", len(calls))
	}
}

// TestREPLEndOfInputEndsLoop: input without /exit ends the loop with a nil
// error once the reader is exhausted.
func TestREPLEndOfInputEndsLoop(t *testing.T) {
	cfg := agotest.NewTestConfig(textResp("bye"))
	a := agent.New("helper", "", agent.WithConfig(cfg))

	var out bytes.Buffer
	if err := REPL(a, strings.NewReader("hello\n"), &out); err != nil {
		t.Fatalf("repl: %v", err)
	}
	if !strings.Contains(out.String(), "bye") {
		t.Errorf("output %q misses the reply", out.String())
	}
}

// errOnceConfig fails its first call, then delegates to the scripted config.
type errOnceConfig struct {
	inner config.ModelConfig
	calls int
}

func (e *errOnceConfig) Provider() config.Provider { return "test" }
func (e *errOnceConfig) Model() string             { return "err-once" }
func (e *errOnceConfig) Clone(m map[string]any) (config.ModelConfig, error) {
	return e.inner.Clone(m)
}
func (e *errOnceConfig) Create() (config.LLMClient, error) {
	inner, err := e.inner.Create()
	if err != nil {
		return nil, err
	}
	e.calls++
	fail := e.calls == 1
	return errOnceClient{inner: inner, fail: fail}, nil
}

type errOnceClient struct {
	inner config.LLMClient
	fail  bool
}

func (c errOnceClient) Call(ctx stdcontext.Context, messages []events.Event, conv *agocontext.Context, opts config.CallOptions) (*events.ModelResponse, error) {
	if c.fail {
		return nil, errors.New("model down")
	}
	return c.inner.Call(ctx, messages, conv, opts)
}

// TestREPLTurnErrorKeepsLoopAliveReal: the first turn fails, the error lands
// on out, and the second turn succeeds.
func TestREPLTurnErrorKeepsLoopAliveReal(t *testing.T) {
	cfg := &errOnceConfig{inner: agotest.NewTestConfig(textResp("recovered"))}
	a := agent.New("helper", "", agent.WithConfig(cfg))

	var out bytes.Buffer
	if err := REPL(a, strings.NewReader("first\nsecond\n/exit\n"), &out); err != nil {
		t.Fatalf("repl: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "error: model down") {
		t.Errorf("output %q misses the reported turn error", got)
	}
	if !strings.Contains(got, "recovered") {
		t.Errorf("output %q misses the second turn's reply", got)
	}
}

// streamingDouble publishes two ModelMessageChunk deltas before its response
// (a real streaming provider client streams the same way — conv.Send of
// ModelMessageChunk).
type streamingDouble struct{}

func (d *streamingDouble) Provider() config.Provider { return "test" }
func (d *streamingDouble) Model() string             { return "double-model" }
func (d *streamingDouble) Create() (config.LLMClient, error) {
	return d, nil
}
func (d *streamingDouble) Clone(m map[string]any) (config.ModelConfig, error) {
	return d, nil
}
func (d *streamingDouble) Call(ctx stdcontext.Context, messages []events.Event, conv *agocontext.Context, opts config.CallOptions) (*events.ModelResponse, error) {
	for _, part := range []string{"He", "y"} {
		if err := conv.Send(&events.ModelMessageChunk{Content: part}); err != nil {
			return nil, err
		}
	}
	return &events.ModelResponse{Message: &events.ModelMessage{Content: "Hey"}}, nil
}

// TestREPLStreamingEchoesChunksOnce: with WithStreaming the deltas stream out
// as produced and the final body — identical to the joined deltas — is not
// printed a second time.
func TestREPLStreamingEchoesChunksOnce(t *testing.T) {
	a := agent.New("helper", "", agent.WithConfig(&streamingDouble{}))

	var out bytes.Buffer
	if err := REPL(a, strings.NewReader("hi\n/exit\n"), &out, WithStreaming()); err != nil {
		t.Fatalf("repl: %v", err)
	}

	got := out.String()
	if strings.Count(got, "Hey") != 1 {
		t.Errorf("output %q shows the answer %d times, want exactly once (streamed, not repeated)", got, strings.Count(got, "Hey"))
	}
	if !strings.Contains(got, "He") {
		t.Errorf("output %q misses the streamed deltas", got)
	}
}

// TestREPLStreamingPrintsBodyWithoutChunks: a non-streaming model under
// WithStreaming still gets its final answer printed.
func TestREPLStreamingPrintsBodyWithoutChunks(t *testing.T) {
	cfg := agotest.NewTestConfig(textResp("plain answer"))
	a := agent.New("helper", "", agent.WithConfig(cfg))

	var out bytes.Buffer
	if err := REPL(a, strings.NewReader("hi\n/exit\n"), &out, WithStreaming()); err != nil {
		t.Fatalf("repl: %v", err)
	}
	if !strings.Contains(out.String(), "plain answer") {
		t.Errorf("output %q misses the final body", out.String())
	}
}
