package agent

import (
	stdcontext "context"
	"testing"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/events"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
)

type lookupArgs struct {
	Q string `json:"q"`
}

// TestFinalToolBatchReplaysExactlyOnce pins the conversation-replay regression
// for a turn ended by a final=true tool (Python parity: the final_turn_history
// fixture of upstream be11666a5, #3562): a finish(final=true)+lookup batch is
// recorded into the transcript as one ToolResultsEvent — written, not
// published, so it cannot drive the next model call — and the next model
// request therefore replays every tool answer exactly once. Before the fix the
// batch was never recorded and the replayed turn went out as tool calls
// without answers, which providers reject.
func TestFinalToolBatchReplaysExactlyOnce(t *testing.T) {
	finish, err := tools.NewFunctionTool("finish", "End the task", func() events.ToolResult {
		return events.ToolResult{Parts: []events.Input{events.TextInput{Content: "the end"}}, Final: true}
	})
	if err != nil {
		t.Fatalf("finish tool: %v", err)
	}
	lookup, err := tools.NewFunctionTool("lookup", "Look things up", func(a lookupArgs) string {
		return "42"
	})
	if err != nil {
		t.Fatalf("lookup tool: %v", err)
	}
	cfg := agotest.NewTestConfig(
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "call_finish", Name: "finish", Arguments: "{}"},
			{ID: "call_lookup", Name: "lookup", Arguments: `{"q":"x"}`},
		}}},
		textResp("done"),
	)
	a := New("a", "", WithConfig(cfg), WithTools(finish, lookup))
	r1, err := a.Ask(stdcontext.Background(), "go")
	if err != nil {
		t.Fatalf("first ask: %v", err)
	}
	if r1.Body() != "the end" {
		t.Fatalf("first ask body = %q, want the forced final answer", r1.Body())
	}
	if _, err := r1.Ask(stdcontext.Background(), "again"); err != nil {
		t.Fatalf("second ask: %v", err)
	}
	calls := cfg.Calls()
	if len(calls) != 2 {
		t.Fatalf("model calls = %d, want 2", len(calls))
	}
	replayed := calls[1]

	// The transcript carries exactly one recorded batch with both answers,
	// in the order the model requested them.
	wrappers := 0
	for _, ev := range replayed {
		if w, ok := events.AsValue(ev).(events.ToolResultsEvent); ok {
			wrappers++
			if len(w.Results) != 2 ||
				w.Results[0].ParentID != "call_finish" || w.Results[1].ParentID != "call_lookup" {
				t.Fatalf("recorded batch = %+v, want call_finish then call_lookup", w.Results)
			}
		}
	}
	if wrappers != 1 {
		t.Fatalf("recorded final batches = %d, want 1", wrappers)
	}

	// Chat-compat replay: exactly one tool message per call, with the real
	// contents (the loose results the tools published are skipped in favor of
	// the recorded batch).
	msgs, err := config.BuildMessages(nil, replayed)
	if err != nil {
		t.Fatalf("BuildMessages: %v", err)
	}
	seen := map[string]string{}
	for _, m := range msgs {
		if m["role"] != "tool" {
			continue
		}
		id, _ := m["tool_call_id"].(string)
		if _, dup := seen[id]; dup {
			t.Fatalf("tool answer for %s replayed more than once", id)
		}
		content, _ := m["content"].(string)
		seen[id] = content
	}
	if len(seen) != 2 || seen["call_finish"] != "the end" || seen["call_lookup"] != "42" {
		t.Fatalf("replayed tool answers = %v, want exactly the real answer per call", seen)
	}
}
