package middleware

import (
	"bytes"
	stdcontext "context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	agocontext "ago/context"
	"ago/events"
	"ago/hitl"
	"ago/stream"
)

// captureLLM returns a terminal OnLLMCall next that records the history it saw.
func captureLLM(saw *[]events.Event) LLMCallFunc {
	return func(evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
		*saw = evs
		return &events.ModelResponse{}, nil
	}
}

func textEv(content string) events.Event { return events.TextInput{Content: content} }

func TestHistoryLimiterKeepsSuffix(t *testing.T) {
	tests := []struct {
		name string
		n    int
		in   []events.Event
		want []events.Event
	}{
		{
			name: "within limit unchanged",
			n:    10,
			in:   []events.Event{textEv("a"), textEv("b")},
			want: []events.Event{textEv("a"), textEv("b")},
		},
		{
			name: "keeps last n",
			n:    2,
			in:   []events.Event{textEv("a"), textEv("b"), textEv("c"), textEv("d")},
			want: []events.Event{textEv("c"), textEv("d")},
		},
		{
			name: "transient dropped and not counted",
			n:    5,
			in:   []events.Event{textEv("a"), events.ModelMessageChunk{Content: "chunk"}, textEv("b")},
			want: []events.Event{textEv("a"), textEv("b")},
		},
		{
			name: "limit counts only non-transient",
			n:    1,
			in:   []events.Event{events.ModelMessageChunk{Content: "chunk"}, textEv("a"), textEv("b")},
			want: []events.Event{textEv("b")},
		},
		{
			name: "limit below 1 behaves as 1",
			n:    0,
			in:   []events.Event{textEv("a"), textEv("b")},
			want: []events.Event{textEv("b")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var saw []events.Event
			m := HistoryLimiter(tt.n)()
			if _, err := m.OnLLMCall(captureLLM(&saw), tt.in, newTestCtx(t)); err != nil {
				t.Fatalf("OnLLMCall: %v", err)
			}
			if len(saw) != len(tt.want) {
				t.Fatalf("got %d events, want %d: %v", len(saw), len(tt.want), saw)
			}
			for i := range saw {
				got, ok1 := saw[i].(events.TextInput)
				wantEv, ok2 := tt.want[i].(events.TextInput)
				if !ok1 || !ok2 || got.Content != wantEv.Content {
					t.Fatalf("event %d = %v, want text %q", i, saw[i], wantEv.Content)
				}
			}
		})
	}
}

func TestHistoryLimiterOtherHooksPassThrough(t *testing.T) {
	m := HistoryLimiter(1)()
	turned := false
	_, err := m.OnTurn(func(ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
		turned = true
		return &events.ModelResponse{}, nil
	}, &events.ModelRequest{}, newTestCtx(t))
	if err != nil || !turned {
		t.Fatalf("OnTurn must pass through: turned=%v err=%v", turned, err)
	}
}

func TestTokenLimiterKeepsSuffix(t *testing.T) {
	tests := []struct {
		name   string
		budget int
		in     []events.Event
		want   []string
	}{
		{
			name:   "within budget unchanged",
			budget: 100,
			in:     []events.Event{textEv("aaaa"), textEv("bbbb")}, // 1 + 1 tokens
			want:   []string{"aaaa", "bbbb"},
		},
		{
			name:   "keeps newest events that fit",
			budget: 6,
			// tokens: 1, 1, 5, 1 → keep last (1), +5 fits (6), +1 breaks (7)
			in:   []events.Event{textEv("aaaa"), textEv("bbbb"), textEv(strings.Repeat("c", 20)), textEv("dddd")},
			want: []string{strings.Repeat("c", 20), "dddd"},
		},
		{
			name:   "newest event kept even over budget",
			budget: 2,
			in:     []events.Event{textEv(strings.Repeat("a", 20)), textEv("bbbb")}, // 5 + 1
			want:   []string{"bbbb"},
		},
		{
			// Transient events size to 0 (sizing parity: is_conversational → 0),
			// so they fit any budget and ride along in the suffix — the limiter
			// never filters them out, it only counts.
			name:   "transient events are sized, not filtered",
			budget: 1,
			in:     []events.Event{events.ModelMessageChunk{Content: "aaaa"}, textEv("bbbb")}, // 0 + 1 tokens
			want:   []string{"aaaa", "bbbb"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var saw []events.Event
			m := TokenLimiter(tt.budget)()
			if _, err := m.OnLLMCall(captureLLM(&saw), tt.in, newTestCtx(t)); err != nil {
				t.Fatalf("OnLLMCall: %v", err)
			}
			var got []string
			for _, ev := range saw {
				var txt string
				switch e := ev.(type) {
				case events.TextInput:
					txt = e.Content
				case events.ModelMessageChunk:
					txt = e.Content
				default:
					t.Fatalf("unexpected event type %T in result", ev)
				}
				got = append(got, txt)
			}
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRetryStopsOnSuccess(t *testing.T) {
	setRetryDelay(t, time.Millisecond)
	var saw []events.Event
	calls := 0
	next := func(evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
		calls++
		saw = evs
		if calls < 3 {
			return nil, boom
		}
		return &events.ModelResponse{ResponseID: "ok"}, nil
	}
	in := []events.Event{textEv("q")}
	resp, err := Retry(3)().OnLLMCall(next, in, newTestCtx(t))
	if err != nil || resp.ResponseID != "ok" {
		t.Fatalf("err=%v resp=%v", err, resp)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	if len(saw) != 1 {
		t.Fatalf("next must receive the history, got %d events", len(saw))
	}
	if txt, ok := saw[0].(events.TextInput); !ok || txt.Content != "q" {
		t.Fatalf("next must receive the history unchanged, got %v", saw)
	}
}

func TestRetryExhaustsAttempts(t *testing.T) {
	setRetryDelay(t, time.Millisecond)
	calls := 0
	next := func(evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
		calls++
		return nil, boom
	}
	resp, err := Retry(2)().OnLLMCall(next, nil, newTestCtx(t))
	if !errors.Is(err, boom) || resp != nil {
		t.Fatalf("err=%v resp=%v", err, resp)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestRetrySingleAttemptByDefault(t *testing.T) {
	setRetryDelay(t, time.Millisecond)
	for _, attempts := range []int{0, 1, -5} {
		calls := 0
		next := func(evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
			calls++
			return nil, boom
		}
		if _, err := Retry(attempts)().OnLLMCall(next, nil, newTestCtx(t)); !errors.Is(err, boom) {
			t.Fatalf("attempts=%d: err=%v", attempts, err)
		}
		if calls != 1 {
			t.Fatalf("attempts=%d: calls = %d, want 1", attempts, calls)
		}
	}
}

func TestRetryDoesNotRetryCancellation(t *testing.T) {
	setRetryDelay(t, time.Millisecond)
	for _, cause := range []error{stdcontext.Canceled, stdcontext.DeadlineExceeded} {
		calls := 0
		next := func(evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
			calls++
			return nil, fmt.Errorf("call failed: %w", cause)
		}
		_, err := Retry(5)().OnLLMCall(next, nil, newTestCtx(t))
		if !errors.Is(err, cause) {
			t.Fatalf("%v: err=%v", cause, err)
		}
		if calls != 1 {
			t.Fatalf("%v: calls = %d, want 1", cause, calls)
		}
	}
}

func TestRetryLinearBackoff(t *testing.T) {
	setRetryDelay(t, 2*time.Millisecond)
	next := func(evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
		return nil, boom
	}
	t0 := time.Now()
	_, err := Retry(3)().OnLLMCall(next, nil, newTestCtx(t))
	elapsed := time.Since(t0)
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	// delays before attempts 2 and 3: 2ms + 4ms = 6ms minimum.
	if elapsed < 6*time.Millisecond {
		t.Fatalf("elapsed %v < linear backoff minimum 6ms", elapsed)
	}
}

func TestRetryOnlyWrapsLLMCall(t *testing.T) {
	m := Retry(5)()
	calls := 0
	next := func(ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
		calls++
		return nil, boom
	}
	call := events.NewToolCallEvent("t")
	if _, err := m.OnToolExecution(next, &call, newTestCtx(t)); !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	if calls != 1 {
		t.Fatalf("OnToolExecution must not be retried: calls = %d", calls)
	}
}

var boom = errors.New("boom")

func setRetryDelay(t *testing.T, d time.Duration) {
	t.Helper()
	old := retryBaseDelay
	retryBaseDelay = d
	t.Cleanup(func() { retryBaseDelay = old })
}

func TestLoggingHooks(t *testing.T) {
	var buf bytes.Buffer
	oldDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(oldDefault) })

	m := Logging()()
	ctx := newTestCtx(t)
	nextErr := errors.New("next failed")

	_, _ = m.OnTurn(func(ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
		return &events.ModelResponse{}, nil
	}, &events.ModelRequest{}, ctx)
	_, _ = m.OnLLMCall(func(evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
		return &events.ModelResponse{Model: "m1", FinishReason: "stop"}, nil
	}, []events.Event{textEv("q")}, ctx)
	call := events.NewToolCallEvent("deploy")
	_, _ = m.OnToolExecution(func(ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
		return events.FromResultCall(*ev, events.NewToolResult(false, "ok")), nil
	}, &call, ctx)
	_, _ = m.OnHumanInput(func(ev *events.HumanInputRequest, ctx *agocontext.Context) (*events.HumanMessage, error) {
		return &events.HumanMessage{Content: "ans"}, nil
	}, &events.HumanInputRequest{}, ctx)
	_, _ = m.OnLLMCall(func(evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
		return nil, nextErr
	}, nil, ctx)

	out := buf.String()
	for _, want := range []string{
		"middleware: turn started", "middleware: turn finished",
		"middleware: llm call", "middleware: llm response",
		"middleware: tool execute", "middleware: tool result",
		"middleware: human input requested", "middleware: human input answered",
		// attributes
		"history_events=1", "model=m1", "finish_reason=stop", "tool=deploy", "result_type=ToolResultEvent",
		"duration=", "err=\"next failed\"",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %q;\n%s", want, out)
		}
	}
}

// approvalContext builds a context whose HITL hook replays scripted answers and
// records the questions it was asked.
func approvalContext(t *testing.T, answers ...string) (*agocontext.Context, func() []string) {
	t.Helper()
	c := agocontext.New(stream.NewMemoryStream())
	var asked []string
	i := 0
	c.SetHitlHook(func(_ stdcontext.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
		asked = append(asked, req.Content)
		var ans string
		if i < len(answers) {
			ans = answers[i]
			i++
		}
		return &events.HumanMessage{Content: ans}, nil
	})
	return c, func() []string { return asked }
}

func toolCall(name, source, args string) *events.ToolCallEvent {
	call := events.NewToolCallEvent(name)
	call.Source = source
	call.Arguments = args
	return &call
}

func TestApprovalGrantedAnswers(t *testing.T) {
	for _, answer := range []string{"y", "yes", "YES", " approve ", "always"} {
		t.Run(answer, func(t *testing.T) {
			ctx, asked := approvalContext(t, answer)
			m := Approval()()
			nextCalled := false
			next := func(ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
				nextCalled = true
				return events.FromResultCall(*ev, events.NewToolResult(false, "ran")), nil
			}
			call := toolCall("deploy", "tk1", `{"env":"prod"}`)
			res, err := m.OnToolExecution(next, call, ctx)
			if err != nil || !nextCalled {
				t.Fatalf("answer %q: nextCalled=%v err=%v", answer, nextCalled, err)
			}
			if _, ok := res.(events.ToolResultEvent); !ok {
				t.Fatalf("answer %q: result type %T", answer, res)
			}
			if q := asked(); len(q) != 1 || !strings.Contains(q[0], "deploy") || !strings.Contains(q[0], `{"env":"prod"}`) {
				t.Fatalf("answer %q: asked = %q", answer, q)
			}
		})
	}
}

func TestApprovalDenied(t *testing.T) {
	ctx, asked := approvalContext(t, "n")
	m := Approval()()
	nextCalled := false
	next := func(ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
		nextCalled = true
		return events.FromResultCall(*ev, events.NewToolResult(false, "ran")), nil
	}
	call := toolCall("deploy", "tk1", "{}")
	res, err := m.OnToolExecution(next, call, ctx)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nextCalled {
		t.Fatal("denied call must not run the tool")
	}
	terr, ok := res.(events.ToolErrorEvent)
	if !ok {
		t.Fatalf("result type %T, want ToolErrorEvent", res)
	}
	if terr.Error != "denied by human" {
		t.Fatalf("error text %q", terr.Error)
	}
	if terr.ParentID != call.ID {
		t.Fatalf("ParentID %q, want call id %q", terr.ParentID, call.ID)
	}
	if q := asked(); len(q) != 1 {
		t.Fatalf("asked %d times", len(q))
	}
}

func TestApprovalAlwaysGrantsBySource(t *testing.T) {
	ctx, asked := approvalContext(t, "always")
	m := Approval()()
	next := func(ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
		return events.FromResultCall(*ev, events.NewToolResult(false, "ran")), nil
	}
	first := toolCall("deploy", "tk1", "{}")
	second := toolCall("other", "tk1", "{}")
	if _, err := m.OnToolExecution(next, first, ctx); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := m.OnToolExecution(next, second, ctx); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if q := asked(); len(q) != 1 {
		t.Fatalf("same source must be granted after \"always\", asked %d times", len(q))
	}
}

func TestApprovalAlwaysIsolatedBySource(t *testing.T) {
	ctx, asked := approvalContext(t, "always", "y")
	m := Approval()()
	next := func(ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
		return events.FromResultCall(*ev, events.NewToolResult(false, "ran")), nil
	}
	first := toolCall("deploy", "tk1", "{}")
	other := toolCall("deploy", "tk2", "{}")
	if _, err := m.OnToolExecution(next, first, ctx); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := m.OnToolExecution(next, other, ctx); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if q := asked(); len(q) != 2 {
		t.Fatalf("different source must be asked again, asked %d times", len(q))
	}
}

func TestApprovalSourceFallsBackToName(t *testing.T) {
	ctx, asked := approvalContext(t, "always", "y", "y")
	m := Approval()()
	next := func(ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
		return events.FromResultCall(*ev, events.NewToolResult(false, "ran")), nil
	}
	first := toolCall("deploy", "", "{}")
	same := toolCall("deploy", "", "{}")
	other := toolCall("other", "", "{}")
	if _, err := m.OnToolExecution(next, first, ctx); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := m.OnToolExecution(next, same, ctx); err != nil {
		t.Fatalf("same-name call: %v", err)
	}
	if _, err := m.OnToolExecution(next, other, ctx); err != nil {
		t.Fatalf("other-name call: %v", err)
	}
	// ask #1: deploy; #2 skipped (granted by name); #3: other name asked again.
	if q := asked(); len(q) != 2 {
		t.Fatalf("asked %d times, want 2 (grant keyed by tool name)", len(q))
	}
}

func TestApprovalGrantsArePerInstance(t *testing.T) {
	ctx, asked := approvalContext(t, "always", "y")
	f := Approval()
	m1, m2 := f(), f() // two turns from the same factory
	next := func(ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
		return events.FromResultCall(*ev, events.NewToolResult(false, "ran")), nil
	}
	first := toolCall("deploy", "tk1", "{}")
	second := toolCall("deploy", "tk1", "{}")
	if _, err := m1.OnToolExecution(next, first, ctx); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if _, err := m2.OnToolExecution(next, second, ctx); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if q := asked(); len(q) != 2 {
		t.Fatalf("grants must not outlive the middleware instance, asked %d times", len(q))
	}
}

func TestApprovalWithoutHookPropagatesError(t *testing.T) {
	c := agocontext.New(stream.NewMemoryStream()) // no HITL hook
	m := Approval()()
	nextCalled := false
	next := func(ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
		nextCalled = true
		return events.FromResultCall(*ev, events.NewToolResult(false, "ran")), nil
	}
	call := toolCall("deploy", "tk1", "{}")
	res, err := m.OnToolExecution(next, call, c)
	if res != nil || err == nil {
		t.Fatalf("res=%v err=%v, want nil res and an error", res, err)
	}
	var notProvided *hitl.NotProvidedError
	if !errors.As(err, &notProvided) {
		t.Fatalf("err = %v, want hitl.NotProvidedError", err)
	}
	if nextCalled {
		t.Fatal("tool must not run when the question cannot be asked")
	}
}

func TestApprovalHookFailurePropagates(t *testing.T) {
	c := agocontext.New(stream.NewMemoryStream())
	c.SetHitlHook(func(_ stdcontext.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
		return nil, errors.New("channel down")
	})
	m := Approval()()
	next := func(ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
		return events.FromResultCall(*ev, events.NewToolResult(false, "ran")), nil
	}
	call := toolCall("deploy", "tk1", "{}")
	res, err := m.OnToolExecution(next, call, c)
	if res != nil || err == nil || !strings.Contains(err.Error(), "channel down") {
		t.Fatalf("res=%v err=%v", res, err)
	}
}
