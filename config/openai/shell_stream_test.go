package openai

import (
	"testing"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/tools"
)

// A streamed hosted shell call shows both halves — command and output — as
// increments, in arrival order, next to the unchanged finished call/result.
func TestResponsesStreamShellDeltas(t *testing.T) {
	shellCall := map[string]any{
		"id": "sh_1", "type": "shell_call", "call_id": "call_1", "status": "completed",
		"action": map[string]any{"commands": []any{"echo hi", "ls"}},
	}
	shellOutput := map[string]any{
		"id": "sho_1", "type": "shell_call_output", "call_id": "call_1", "status": "completed",
		"output": []any{
			map[string]any{"stdout": "hi\n", "stderr": ""},
			map[string]any{"stdout": "", "stderr": "warning\n"},
		},
	}
	body := sseBody(
		map[string]any{"type": "response.shell_call_command.added", "output_index": 0, "command_index": 0, "command": ""},
		map[string]any{"type": "response.shell_call_command.delta", "output_index": 0, "command_index": 0, "delta": "echo "},
		map[string]any{"type": "response.shell_call_command.delta", "output_index": 0, "command_index": 0, "delta": "hi"},
		map[string]any{"type": "response.shell_call_command.delta", "output_index": 0, "command_index": 1, "delta": "ls"},
		map[string]any{"type": "response.output_item.done", "output_index": 0, "item": shellCall},
		map[string]any{"type": "response.shell_call_output_content.delta", "output_index": 1, "command_index": 0, "item_id": "sho_1",
			"delta": map[string]any{"stdout": "hi\n", "stderr": nil}},
		map[string]any{"type": "response.shell_call_output_content.delta", "output_index": 1, "command_index": 1, "item_id": "sho_1",
			"delta": map[string]any{"stdout": nil, "stderr": "warning\n"}},
		map[string]any{"type": "response.shell_call_output_content.delta", "output_index": 1, "command_index": 1, "item_id": "sho_1",
			"delta": map[string]any{"stdout": "both\n", "stderr": "at once\n"}},
		map[string]any{"type": "response.output_item.done", "output_index": 1, "item": shellOutput},
		map[string]any{"type": "response.output_text.delta", "output_index": 2, "delta": "all done"},
		map[string]any{"type": "response.completed", "response": completedResponse()},
	)
	srv, _ := captureServer(t, "text/event-stream", 200, body)
	cfg := NewOpenAIResponses("gpt-test")
	cfg.APIKey = "test-key"
	cfg.BaseURL = srv.URL + "/v1"
	cfg.Streaming = true

	req := events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "go"}}}
	_, sent := responsesCall(t, cfg, []events.Event{&req}, config.CallOptions{})

	// Order of everything the call published, by short label.
	var order []string
	var commands []ShellCommandChunk
	var outputs []ShellOutputChunk
	var messageChunks []string
	for _, ev := range *sent {
		switch e := events.AsValue(ev).(type) {
		case ShellCommandChunk:
			order = append(order, "cmd")
			commands = append(commands, e)
		case ShellOutputChunk:
			order = append(order, "out")
			outputs = append(outputs, e)
		case events.BuiltinToolCallEvent:
			order = append(order, "call")
		case events.BuiltinToolResultEvent:
			order = append(order, "result")
			if e.Name != tools.ShellToolName {
				t.Fatalf("result name: %q", e.Name)
			}
		case events.ModelMessageChunk:
			order = append(order, "msg")
			messageChunks = append(messageChunks, e.Content)
		}
	}
	wantOrder := []string{"cmd", "cmd", "cmd", "call", "out", "out", "out", "result", "msg"}
	if len(order) != len(wantOrder) {
		t.Fatalf("event order: want %v, got %v", wantOrder, order)
	}
	for i := range wantOrder {
		if order[i] != wantOrder[i] {
			t.Fatalf("event order: want %v, got %v", wantOrder, order)
		}
	}

	var gotCmd []string
	var gotIdx []int
	for _, c := range commands {
		gotCmd = append(gotCmd, c.Content)
		gotIdx = append(gotIdx, c.CommandIndex)
		if c.OutputIndex != 0 || !c.Transient() || c.Conversational() {
			t.Fatalf("command chunk metadata: %+v", c)
		}
	}
	if len(gotCmd) != 3 || gotCmd[0] != "echo " || gotCmd[1] != "hi" || gotCmd[2] != "ls" {
		t.Fatalf("command text: %q", gotCmd)
	}
	if gotIdx[0] != 0 || gotIdx[1] != 0 || gotIdx[2] != 1 {
		t.Fatalf("command indexes: %v", gotIdx)
	}

	want := []struct {
		stdout, stderr string
		hasOut, hasErr bool
		cmd            int
	}{
		{"hi\n", "", true, false, 0},
		{"", "warning\n", false, true, 1},
		{"both\n", "at once\n", true, true, 1},
	}
	if len(outputs) != len(want) {
		t.Fatalf("output chunks: %+v", outputs)
	}
	for i, w := range want {
		o := outputs[i]
		if o.ItemID != "sho_1" || o.OutputIndex != 1 || o.CommandIndex != w.cmd || !o.Transient() {
			t.Fatalf("output chunk %d: %+v", i, o)
		}
		if (o.Stdout != nil) != w.hasOut || (o.Stderr != nil) != w.hasErr {
			t.Fatalf("output chunk %d presence: %+v", i, o)
		}
		if w.hasOut && *o.Stdout != w.stdout || w.hasErr && *o.Stderr != w.stderr {
			t.Fatalf("output chunk %d text: %+v", i, o)
		}
	}
	if len(messageChunks) != 1 || messageChunks[0] != "all done" {
		t.Fatalf("shell output leaked into message chunks: %q", messageChunks)
	}
}
