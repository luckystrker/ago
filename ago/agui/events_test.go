package agui

import (
	"encoding/json"
	"testing"

	"ago/events"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return string(b)
}

// The wire form: camelCase fields, "type" always present, optional fields
// omitted when unset — the SDK's serialization rule.
func TestEventJSONForms(t *testing.T) {
	ts := int64(1234)
	tests := []struct {
		name string
		ev   any
		want string
	}{
		{
			name: "text message content",
			ev: TextMessageContentEvent{
				baseEvent: baseEvent{Type: EventTextMessageContent, Timestamp: &ts},
				MessageID: "m1", Delta: "hi",
			},
			want: `{"type":"TEXT_MESSAGE_CONTENT","timestamp":1234,"messageId":"m1","delta":"hi"}`,
		},
		{
			name: "text message start omits unset role",
			ev: TextMessageStartEvent{
				baseEvent: baseEvent{Type: EventTextMessageStart, Timestamp: &ts},
				MessageID: "m1",
			},
			want: `{"type":"TEXT_MESSAGE_START","timestamp":1234,"messageId":"m1"}`,
		},
		{
			name: "tool call start",
			ev: ToolCallStartEvent{
				baseEvent:    baseEvent{Type: EventToolCallStart, Timestamp: &ts},
				ToolCallID:   "c1",
				ToolCallName: "get_weather",
			},
			want: `{"type":"TOOL_CALL_START","timestamp":1234,"toolCallId":"c1","toolCallName":"get_weather"}`,
		},
		{
			name: "run finished with success outcome and usage",
			ev: RunFinishedEvent{
				baseEvent: baseEvent{Type: EventRunFinished, Timestamp: &ts},
				ThreadID:  "t1", RunID: "r1",
				Outcome: SuccessOutcome{PendingToolCallIDs: []string{"c1"}},
				Usage:   []TokenUsage{{Provider: "p", Model: "m", InputTokens: i64(1), TotalTokens: i64(1)}},
			},
			want: `{"type":"RUN_FINISHED","timestamp":1234,"threadId":"t1","runId":"r1",` +
				`"outcome":{"type":"success","pendingToolCallIds":["c1"]},` +
				`"usage":[{"provider":"p","model":"m","inputTokens":1,"totalTokens":1}]}`,
		},
		{
			name: "run finished cancelled outcome",
			ev: RunFinishedEvent{
				baseEvent: baseEvent{Type: EventRunFinished, Timestamp: &ts},
				ThreadID:  "t1", RunID: "r1",
				Outcome: CancelledOutcome{},
			},
			want: `{"type":"RUN_FINISHED","timestamp":1234,"threadId":"t1","runId":"r1","outcome":{"type":"cancelled"}}`,
		},
		{
			name: "run error",
			ev: RunErrorEvent{
				baseEvent: baseEvent{Type: EventRunError, Timestamp: &ts},
				Message:   "boom", Code: CodeUnsupportedProtocolVersion,
			},
			want: `{"type":"RUN_ERROR","timestamp":1234,"message":"boom","code":"UNSUPPORTED_PROTOCOL_VERSION"}`,
		},
		{
			name: "reasoning message start carries its role",
			ev: ReasoningMessageStartEvent{
				baseEvent: baseEvent{Type: EventReasoningMessageStart, Timestamp: &ts},
				MessageID: "m1", Role: reasoningRole,
			},
			want: `{"type":"REASONING_MESSAGE_START","timestamp":1234,"messageId":"m1","role":"reasoning"}`,
		},
		{
			name: "tool call result with parts content",
			ev: ToolCallResultEvent{
				baseEvent: baseEvent{Type: EventToolCallResult, Timestamp: &ts},
				MessageID: "m2", ToolCallID: "c1",
				Content: []any{ContentPart{Type: PartText, Text: "sunny"}, ContentPart{
					Type:   PartImage,
					Source: &PartSource{Type: SourceData, Value: "QUJD", MimeType: "image/png"},
				}},
				Role: "tool",
			},
			want: `{"type":"TOOL_CALL_RESULT","timestamp":1234,"messageId":"m2","toolCallId":"c1",` +
				`"content":[{"type":"text","text":"sunny"},{"type":"image","source":{"type":"data","value":"QUJD","mimeType":"image/png"}}],` +
				`"role":"tool"}`,
		},
		{
			name: "state snapshot",
			ev: StateSnapshotEvent{
				baseEvent: baseEvent{Type: EventStateSnapshot, Timestamp: &ts},
				Snapshot:  map[string]any{"k": "v"},
			},
			want: `{"type":"STATE_SNAPSHOT","timestamp":1234,"snapshot":{"k":"v"}}`,
		},
		{
			name: "custom",
			ev: CustomEvent{
				baseEvent: baseEvent{Type: EventCustom, Timestamp: &ts},
				Name:      "progress", Value: 7,
			},
			want: `{"type":"CUSTOM","timestamp":1234,"name":"progress","value":7}`,
		},
		{
			name: "subagent started",
			ev: SubagentStartedEvent{
				baseEvent:     baseEvent{Type: "SUBAGENT_STARTED", Timestamp: &ts},
				SubagentRunID: "task1", Name: "helper", Description: "does things", ParentToolCallID: "c9",
			},
			want: `{"type":"SUBAGENT_STARTED","timestamp":1234,"subagentRunId":"task1","name":"helper","description":"does things","parentToolCallId":"c9"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mustJSON(t, tt.ev); got != tt.want {
				t.Fatalf("wire form mismatch\n got: %s\nwant: %s", got, tt.want)
			}
		})
	}
}

func TestUnmarshalEvent(t *testing.T) {
	t.Run("round trip through the typed structs", func(t *testing.T) {
		raw := []byte(`{"type":"RUN_STARTED","timestamp":5,"threadId":"t1","runId":"r1","protocolVersion":"1.0"}`)
		ev, err := UnmarshalEvent(raw)
		if err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		started, ok := ev.(RunStartedEvent)
		if !ok {
			t.Fatalf("got %T, want RunStartedEvent", ev)
		}
		if started.ThreadID != "t1" || started.RunID != "r1" || started.ProtocolVersion != "1.0" {
			t.Fatalf("fields lost: %+v", started)
		}
		if got := mustJSON(t, started); string(raw) != got {
			t.Fatalf("round trip changed the bytes\n got: %s\nwant: %s", got, raw)
		}
	})

	t.Run("tool call result content decodes as string or parts", func(t *testing.T) {
		str, err := UnmarshalEvent([]byte(`{"type":"TOOL_CALL_RESULT","messageId":"m","toolCallId":"c","content":"sunny"}`))
		if err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if res := str.(ToolCallResultEvent); res.Content != "sunny" {
			t.Fatalf("content = %#v, want the string sunny", res.Content)
		}
		parts, err := UnmarshalEvent([]byte(`{"type":"TOOL_CALL_RESULT","messageId":"m","toolCallId":"c","content":[{"type":"text","text":"a"}]}`))
		if err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		got, ok := parts.(ToolCallResultEvent).Content.([]any)
		if !ok || len(got) != 1 {
			t.Fatalf("content = %#v, want one decoded part", parts.(ToolCallResultEvent).Content)
		}
	})

	t.Run("an unmodelled type comes back as Raw, verbatim", func(t *testing.T) {
		raw := []byte(`{"type":"STEP_STARTED","timestamp":1,"stepName":"s1"}`)
		ev, err := UnmarshalEvent(raw)
		if err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		r, ok := ev.(Raw)
		if !ok {
			t.Fatalf("got %T, want Raw", ev)
		}
		if r.eventType() != "STEP_STARTED" {
			t.Fatalf("Raw type = %q", r.eventType())
		}
		if got := mustJSON(t, r); got != string(raw) {
			t.Fatalf("Raw changed the bytes: %s", got)
		}
	})

	t.Run("malformed payload is an error", func(t *testing.T) {
		if _, err := UnmarshalEvent([]byte(`{not json`)); err == nil {
			t.Fatal("want an error for a malformed payload")
		}
	})
}

// AGUIEvent is a registered ago event: it survives the transcript round trip and
// carries its payload verbatim.
func TestAGUIEventTranscriptRoundTrip(t *testing.T) {
	payload := []byte(`{"type":"CUSTOM","name":"n","value":[1,2]}`)
	ag := AGUIEvent{Payload: payload}

	b, err := events.MarshalEvent(&ag)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var head struct {
		Type    string          `json:"@type"`
		Payload json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if head.Type != "AGUIEvent" || string(head.Payload) != string(payload) {
		t.Fatalf("transcript form = %s", b)
	}

	back, err := events.UnmarshalEvent(b)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ag2, ok := back.(*AGUIEvent)
	if !ok {
		t.Fatalf("got %T, want *AGUIEvent", back)
	}
	if string(ag2.Payload) != string(payload) {
		t.Fatalf("payload changed: %s", ag2.Payload)
	}
}

func i64(v int64) *int64 { return &v }
