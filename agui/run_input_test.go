package agui

import (
	"strings"
	"testing"

	"github.com/luckystrker/ago/events"
)

func TestReadRunInput(t *testing.T) {
	t.Run("a valid camelCase body", func(t *testing.T) {
		body := `{
		  "threadId": "t1", "runId": "r1", "protocolVersion": "1.0",
		  "state": {"count": 3},
		  "messages": [
			{"id": "m0", "role": "system", "content": "be nice"},
			{"id": "m1", "role": "user", "content": "hello"},
			{"id": "m2", "role": "assistant", "content": "calling",
			 "toolCalls": [{"id": "c1", "type": "function", "function": {"name": "f", "arguments": "{\"x\":1}"}}]},
			{"id": "m3", "role": "tool", "toolCallId": "c1", "content": "done"},
			{"id": "m4", "role": "activity", "activityType": "tick", "content": {}}
		  ],
		  "tools": [{"name": "f", "description": "does f", "parameters": {"type": "object"}}],
		  "context": [{"description": "tz", "value": "UTC"}],
		  "forwardedProps": {"any": true}
		}`
		in, err := ReadRunInput([]byte(body))
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if in.ThreadID != "t1" || in.RunID != "r1" || in.ProtocolVersion != "1.0" {
			t.Fatalf("ids lost: %+v", in)
		}
		if len(in.Messages) != 5 || len(in.Tools) != 1 || len(in.Context) != 1 {
			t.Fatalf("decoding lost members: %+v", in)
		}
		if in.Tools[0].Name != "f" || in.Context[0].Value != "UTC" {
			t.Fatalf("tool/context lost: %+v %+v", in.Tools[0], in.Context[0])
		}
		tc := in.Messages[2].ToolCalls[0]
		if tc.ID != "c1" || tc.Function.Name != "f" || tc.Function.Arguments != "{\"x\":1}" {
			t.Fatalf("tool call lost: %+v", tc)
		}
	})

	t.Run("a message of unrecognised role is stripped, the run served", func(t *testing.T) {
		in, err := ReadRunInput([]byte(`{"threadId":"t","runId":"r","messages":[
		  {"id":"m1","role":"widget","content":"?"},
		  {"id":"m2","role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(in.Messages) != 1 || in.Messages[0].Role != "user" {
			t.Fatalf("messages = %+v, want only the user message", in.Messages)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		cases := []struct {
			name string
			body string
		}{
			{"not json", `{nope`},
			{"no thread", `{"runId":"r","messages":[]}`},
			{"no run", `{"threadId":"t","messages":[]}`},
			{"no messages", `{"threadId":"t","runId":"r"}`},
			{"malformed base64 in a data source", `{"threadId":"t","runId":"r","messages":[
				{"id":"m1","role":"user","content":[{"type":"image","source":{"type":"data","value":"!!not-base64!!","mimeType":"image/png"}}]}]}`},
			{"malformed base64 in an encrypted value", `{"threadId":"t","runId":"r","messages":[
				{"id":"m1","role":"assistant","content":"x","toolCalls":[{"id":"c1","function":{"name":"f","arguments":"{}"},"encryptedValue":"@@@"}]}]}`},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := ReadRunInput([]byte(tc.body)); err == nil {
					t.Fatal("want an error")
				}
			})
		}
	})
}

func TestDecodeContent(t *testing.T) {
	text, parts, err := decodeContent([]byte(`"plain"`))
	if err != nil || text != "plain" || parts != nil {
		t.Fatalf("string content: %q %v %v", text, parts, err)
	}
	_, parts, err = decodeContent([]byte(`[{"type":"text","text":"a"},{"type":"image","source":{"type":"url","value":"http://x"}}]`))
	if err != nil || len(parts) != 2 {
		t.Fatalf("parts content: %v %v", parts, err)
	}
	if parts[0].Text != "a" || parts[1].Source.Type != SourceURL || parts[1].Source.Value != "http://x" {
		t.Fatalf("parts lost: %+v", parts)
	}
	if _, _, err := decodeContent([]byte(`42`)); err == nil {
		t.Fatal("a bare number is neither text nor parts: want an error")
	}
}

// The mapping of AG-UI history into what the agent is handed (Python parity:
// map_agui_messages_to_events).
func TestMapRunInputMessages(t *testing.T) {
	input := RunAgentInput{
		ThreadID: "t", RunID: "r",
		Context: []ContextEntry{{Description: "tz", Value: "UTC"}},
		Messages: []Message{
			{ID: "m0", Role: "system", Content: jsonRaw(`"be brief"`)},
			{ID: "m1", Role: "user", Content: jsonRaw(`"hi"`)},
			{ID: "m2", Role: "assistant", Content: jsonRaw(`"calling"`),
				ToolCalls: []ToolCall{{ID: "c1", Function: FunctionCall{Name: "get_weather", Arguments: `{"city":"Paris"}`}}}},
			{ID: "m3", Role: "tool", ToolCallID: "c1", Content: jsonRaw(`"sunny"`)},
			{ID: "m4", Role: "reasoning", Content: jsonRaw(`"hmm"`)},
			{ID: "m5", Role: "activity", ActivityType: "tick", Content: jsonRaw(`{"x":1}`)},
			{ID: "m6", Role: "user", Content: jsonRaw(`[{"type":"text","text":"and now?"},{"type":"image","source":{"type":"data","value":"QUJD","mimeType":"image/png"}}]`)},
		},
	}
	got := mapRunInputMessages(input)

	if len(got.history) != 5 {
		t.Fatalf("history has %d events, want 5:\n%s", len(got.history), renderEvents(got.history))
	}
	// [0] the opening request: the shared context block, then the system message.
	req, ok := got.history[0].(*events.ModelRequest)
	if !ok {
		t.Fatalf("history[0] = %T, want *ModelRequest", got.history[0])
	}
	if len(req.Parts) != 2 {
		t.Fatalf("opening request carries %d parts, want 2", len(req.Parts))
	}
	if p, ok := events.AsValue(req.Parts[0]).(events.TextInput); !ok ||
		!strings.Contains(p.Content, "## Context from the application") || !strings.Contains(p.Content, "- tz: UTC") {
		t.Fatalf("shared context block missing: %#v", req.Parts[0])
	}
	if p, ok := events.AsValue(req.Parts[1]).(events.TextInput); !ok || p.Content != "be brief" {
		t.Fatalf("system message missing: %#v", req.Parts[1])
	}
	// [1] the prior user turn.
	if req, ok := got.history[1].(*events.ModelRequest); !ok || len(req.Parts) != 1 {
		t.Fatalf("history[1] = %#v, want the prior user turn", got.history[1])
	} else if p := events.AsValue(req.Parts[0]).(events.TextInput); p.Content != "hi" {
		t.Fatalf("prior user turn content = %q", p.Content)
	}
	// [2] the assistant turn with its tool call.
	resp, ok := got.history[2].(*events.ModelResponse)
	if !ok {
		t.Fatalf("history[2] = %T, want *ModelResponse", got.history[2])
	}
	if resp.Content() != "calling" || len(resp.ToolCalls.Calls) != 1 {
		t.Fatalf("assistant turn lost: %+v", resp)
	}
	call := resp.ToolCalls.Calls[0]
	if call.ID != "c1" || call.Name != "get_weather" || call.Arguments != `{"city":"Paris"}` {
		t.Fatalf("tool call lost: %+v", call)
	}
	// [3] the tool result, named off the assistant's calls; the reasoning entry.
	results, ok := got.history[3].(*events.ToolResultsEvent)
	if !ok || len(results.Results) != 1 {
		t.Fatalf("history[3] = %#v, want one tool result", got.history[3])
	}
	tr := results.Results[0]
	if tr.ParentID != "c1" || tr.Name != "get_weather" {
		t.Fatalf("tool result not tied to its call: %+v", tr)
	}
	if p, ok := events.AsValue(tr.Result.Parts[0]).(events.TextInput); !ok || p.Content != "sunny" {
		t.Fatalf("tool result content lost: %#v", tr.Result.Parts[0])
	}
	if _, ok := got.history[4].(*events.ModelReasoning); !ok {
		t.Fatalf("history[4] = %T, want *ModelReasoning", got.history[4])
	}
	// The current turn: the trailing user run, text and inline image.
	if len(got.current) != 2 {
		t.Fatalf("current turn has %d inputs, want 2", len(got.current))
	}
	if p, ok := events.AsValue(got.current[0]).(events.TextInput); !ok || p.Content != "and now?" {
		t.Fatalf("current text lost: %#v", got.current[0])
	}
	img, ok := events.AsValue(got.current[1]).(events.BinaryInput)
	if !ok || img.MediaType != "image/png" || img.Kind != events.BinaryImage || string(img.Data) != "ABC" {
		t.Fatalf("current image lost: %#v", got.current[1])
	}
}

func TestMapRunInputMessagesSkips(t *testing.T) {
	t.Run("an error leads a tool message; an empty body is still answered", func(t *testing.T) {
		got := mapRunInputMessages(RunAgentInput{
			ThreadID: "t", RunID: "r",
			Messages: []Message{
				{ID: "m1", Role: "tool", ToolCallID: "c1", Error: "boom"},
			},
		})
		results := got.history[0].(*events.ToolResultsEvent)
		parts := results.Results[0].Result.Parts
		if len(parts) != 1 {
			t.Fatalf("parts = %d, want the error text alone", len(parts))
		}
		if p := events.AsValue(parts[0]).(events.TextInput); p.Content != "boom" {
			t.Fatalf("error text = %q", p.Content)
		}
	})

	t.Run("unmappable parts are skipped with the run served", func(t *testing.T) {
		got := mapRunInputMessages(RunAgentInput{
			ThreadID: "t", RunID: "r",
			Messages: []Message{
				{ID: "m1", Role: "user", Content: jsonRaw(`[
				  {"type":"text","text":"keep"},
				  {"type":"chart","source":{"type":"data","value":"QUJD","mimeType":"x"}},
				  {"type":"image","source":{"type":"file","value":"handle","provider":"openai"}}
				]`)},
			},
		})
		if len(got.current) != 1 {
			t.Fatalf("current = %d inputs, want the text alone", len(got.current))
		}
		if p := events.AsValue(got.current[0]).(events.TextInput); p.Content != "keep" {
			t.Fatalf("text = %q", p.Content)
		}
	})

	t.Run("an untagged file handle passes", func(t *testing.T) {
		got := mapRunInputMessages(RunAgentInput{
			ThreadID: "t", RunID: "r",
			Messages: []Message{
				{ID: "m1", Role: "user", Content: jsonRaw(`[{"type":"document","source":{"type":"file","value":"h1"}}]`)},
			},
		})
		f, ok := events.AsValue(got.current[0]).(events.FileIdInput)
		if !ok || f.FileID != "h1" {
			t.Fatalf("file input lost: %#v", got.current[0])
		}
	})
}

func jsonRaw(s string) []byte { return []byte(s) }

func renderEvents(evs []events.Event) string {
	out := ""
	for _, ev := range evs {
		out += ev.EventType() + "\n"
	}
	return out
}
