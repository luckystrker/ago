package agent

import (
	stdcontext "context"
	"reflect"
	"testing"

	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
)

// requestTexts lists the text of every user-side request (the original and the
// drained ones) in the transcript a model call was handed.
func requestTexts(call []events.Event) []string {
	var out []string
	for _, ev := range call {
		var parts []events.Input
		switch v := events.AsValue(ev).(type) {
		case events.ModelRequest:
			parts = v.Parts
		case events.DrainedModelRequest:
			parts = v.Parts
		default:
			continue
		}
		for _, p := range parts {
			if tp, ok := events.AsValue(p).(events.TextInput); ok {
				out = append(out, tp.Content)
			}
		}
	}
	return out
}

func pendingTexts(s stream.Stream) []string {
	var out []string
	for _, req := range s.Pending() {
		for _, p := range req.Parts {
			if tp, ok := events.AsValue(p).(events.TextInput); ok {
				out = append(out, tp.Content)
			}
		}
	}
	return out
}

func mustCall(t *testing.T, name string, calls ...events.ToolCallEvent) *events.ModelResponse {
	t.Helper()
	return &events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: calls}}
}

// TestForcedFinalToolWithQueuedInputWaitsForNextTurn: input enqueued while a
// final=true tool runs must not trigger another model call in the same turn;
// the final result stays the reply and the input is consumed by the next turn
// (D-127; upstream 3bee17b3c6).
func TestForcedFinalToolWithQueuedInputWaitsForNextTurn(t *testing.T) {
	var run *AgentRun
	escalate, err := tools.NewFunctionTool("escalate", "Hand the ticket to a person", func() (events.ToolResult, error) {
		if err := run.Enqueue(userText("What about my order?")...); err != nil {
			return events.ToolResult{}, err
		}
		return events.ToolResult{Parts: []events.Input{events.TextInput{Content: "Ticket escalated."}}, Final: true}, nil
	})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	cfg := agotest.NewTestConfig(
		mustCall(t, "escalate", events.ToolCallEvent{ID: "c1", Name: "escalate", Arguments: "{}"}),
		textResp("Your order shipped."),
	)
	a := New("a", "", WithConfig(cfg), WithTools(escalate))
	run, err = a.Run(stdcontext.Background(), "I need a person.")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	defer run.Close()
	reply, err := run.Result(stdcontext.Background())
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	// An open run holds the stream's turn lock, which the next turn needs.
	run.Close()

	if reply.Body() != "Ticket escalated." {
		t.Fatalf("reply = %q, want the final tool result", reply.Body())
	}
	if n := len(cfg.Calls()); n != 1 {
		t.Fatalf("model calls after the forced turn = %d, want 1 (no extra call)", n)
	}
	if got := pendingTexts(run.Stream()); !reflect.DeepEqual(got, []string{"What about my order?"}) {
		t.Fatalf("inbox after the forced turn = %v, want the queued input kept", got)
	}

	next, err := reply.Ask(stdcontext.Background(), "Thanks!")
	if err != nil {
		t.Fatalf("next ask: %v", err)
	}
	if next.Body() != "Your order shipped." {
		t.Fatalf("next reply = %q", next.Body())
	}
	calls := cfg.Calls()
	if len(calls) != 2 {
		t.Fatalf("model calls = %d, want 2", len(calls))
	}
	// The queued message is consumed by the next turn, after the first turn's
	// own request and before the new one.
	want := []string{"I need a person.", "What about my order?", "Thanks!"}
	if got := requestTexts(calls[1]); !reflect.DeepEqual(got, want) {
		t.Fatalf("next call saw requests %v, want %v", got, want)
	}
	if left := pendingTexts(run.Stream()); len(left) != 0 {
		t.Fatalf("inbox after the next turn = %v, want empty", left)
	}
}

// TestForcedClientHandoffWithQueuedInputWaitsForClientResult: calls handed to a
// client tool end the turn; input queued meanwhile triggers no model call and
// reaches the model right after the client's result when the client resumes the
// turn on the same stream.
func TestForcedClientHandoffWithQueuedInputWaitsForClientResult(t *testing.T) {
	pickFile, err := tools.NewClientTool(map[string]any{"name": "pick_file", "description": "Pick a file."})
	if err != nil {
		t.Fatalf("client tool: %v", err)
	}
	handedOff := events.ToolCallEvent{ID: "c1", Name: "pick_file", Arguments: "{}"}
	cfg := agotest.NewTestConfig(mustCall(t, "pick_file", handedOff))
	a := New("a", "", WithConfig(cfg), WithTools(pickFile))

	run, err := a.Run(stdcontext.Background(), "Attach my invoice.")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	defer run.Close()
	// Queue the input when the hand-off reaches the stream, i.e. mid-turn.
	run.Stream().Subscribe(func(ev events.Event) error {
		if _, ok := events.AsValue(ev).(events.ClientToolCallEvent); ok {
			return run.Enqueue(userText("What about my order?")...)
		}
		return nil
	}, stream.SubscribeOptions{})

	reply, err := run.Result(stdcontext.Background())
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	// An open run holds the stream's turn lock, which the resume needs.
	run.Close()
	if got := reply.Response().ToolCalls.Calls; len(got) != 1 || got[0].ID != "c1" {
		t.Fatalf("reply tool calls = %+v, want the handed-off call", got)
	}
	if n := len(cfg.Calls()); n != 1 {
		t.Fatalf("model calls after the hand-off = %d, want 1 (no extra call)", n)
	}
	if got := pendingTexts(run.Stream()); !reflect.DeepEqual(got, []string{"What about my order?"}) {
		t.Fatalf("inbox after the hand-off = %v, want the queued input kept", got)
	}

	// The client ran the call and resumes on the same stream; a fresh agent
	// serves the resume, as the A2A and AG-UI bridges build one per request.
	resumedCfg := agotest.NewTestConfig(textResp("Your order shipped."))
	resumedAgent := New("a", "", WithConfig(resumedCfg), WithTools(pickFile))
	history, err := run.Stream().History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	picked := events.ToolResultsEvent{Results: []events.ToolResultEvent{{
		ParentID: "c1", Name: "pick_file", Result: events.ToolResult{Parts: []events.Input{events.TextInput{Content: "invoice.pdf"}}},
	}}}
	resumed, err := resumedAgent.ResumeWith(stdcontext.Background(), RunOptions{Stream: run.Stream()},
		append(history, picked)...)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.Body() != "Your order shipped." {
		t.Fatalf("resumed reply = %q", resumed.Body())
	}
	resumedCalls := resumedCfg.Calls()
	if len(resumedCalls) != 1 {
		t.Fatalf("resumed model calls = %d, want 1", len(resumedCalls))
	}
	// The queued message is the last request and sits after the client's result.
	call := resumedCalls[0]
	lastResults, lastQueued := -1, -1
	for i, ev := range call {
		switch events.AsValue(ev).(type) {
		case events.ToolResultsEvent:
			lastResults = i
		case events.DrainedModelRequest:
			lastQueued = i
		}
	}
	if lastResults < 0 || lastQueued < lastResults {
		t.Fatalf("queued message must follow the client's result: results at %d, queued at %d", lastResults, lastQueued)
	}
	if got := requestTexts(call); got[len(got)-1] != "What about my order?" {
		t.Fatalf("resumed call requests = %v, want the queued message last", got)
	}
}

// TestForcedNotSetToolRoundMergesQueuedInput pins the unchanged path of an
// ordinary (not forced) turn: input queued while a tool runs is merged into the
// model call that follows the tool result, which answers it; no further call.
func TestForcedNotSetToolRoundMergesQueuedInput(t *testing.T) {
	var run *AgentRun
	note, err := tools.NewFunctionTool("note", "Record a note", func() (string, error) {
		if err := run.Enqueue(userText("one more thing")...); err != nil {
			return "", err
		}
		return "noted", nil
	})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	cfg := agotest.NewTestConfig(
		mustCall(t, "note", events.ToolCallEvent{ID: "c1", Name: "note", Arguments: "{}"}),
		textResp("first answer"),
		textResp("answer to the extra input"),
	)
	a := New("a", "", WithConfig(cfg), WithTools(note))
	run, err = a.Run(stdcontext.Background(), "go")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	defer run.Close()
	reply, err := run.Result(stdcontext.Background())
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if n := len(cfg.Calls()); n != 2 {
		t.Fatalf("model calls = %d, want 2 (tool call, then the call that saw the queued input)", n)
	}
	if reply.Body() != "first answer" {
		t.Fatalf("reply = %q", reply.Body())
	}
	if got := requestTexts(cfg.Calls()[1]); !reflect.DeepEqual(got, []string{"go", "one more thing"}) {
		t.Fatalf("second call saw requests %v", got)
	}
}

// TestForcedNotSetLateInputGetsExtraModelCall: input that arrives after the
// last model call has already answered (and no tool follows) is still flushed
// by one extra call — the old behavior for non-forced replies.
func TestForcedNotSetLateInputGetsExtraModelCall(t *testing.T) {
	cfg := agotest.NewTestConfig(textResp("first answer"), textResp("answer to the extra input"))
	a := New("a", "", WithConfig(cfg))
	run, err := a.Run(stdcontext.Background(), "go")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	defer run.Close()
	// Enqueue when the first reply lands: the turn body has not drained yet.
	run.Stream().Subscribe(func(ev events.Event) error {
		if mr, ok := events.AsValue(ev).(events.ModelResponse); ok && mr.Content() == "first answer" {
			return run.Enqueue(userText("late input")...)
		}
		return nil
	}, stream.SubscribeOptions{})
	reply, err := run.Result(stdcontext.Background())
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if n := len(cfg.Calls()); n != 2 {
		t.Fatalf("model calls = %d, want 2 (extra call for the queued input)", n)
	}
	if reply.Body() != "answer to the extra input" {
		t.Fatalf("reply = %q, want the extra call's answer", reply.Body())
	}
	if left := pendingTexts(run.Stream()); len(left) != 0 {
		t.Fatalf("inbox after the turn = %v, want empty", left)
	}
}
