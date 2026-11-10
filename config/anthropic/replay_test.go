package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
)

// thinkingToolResponse answers with every block kind a thinking tool-use turn
// carries: a signed thinking block, a redacted one, and the tool call.
const thinkingToolResponse = `{
  "id": "msg_t",
  "model": "claude-sonnet-4",
  "content": [
    {"type": "thinking", "thinking": "let me think", "signature": "sig-abc"},
    {"type": "redacted_thinking", "data": "opaque-xyz"},
    {"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "Paris"}}
  ],
  "stop_reason": "tool_use",
  "usage": {"input_tokens": 10, "output_tokens": 5}
}`

// replayEvents picks the durable replay carrier out of what the call put on the
// conversation stream, in order — exactly what a real history would hold.
func replayEvents(sent []events.Event) []events.Event {
	var out []events.Event
	for _, ev := range sent {
		switch events.AsValue(ev).(type) {
		case ThinkingBlockEvent, RedactedThinkingEvent:
			out = append(out, ev)
		}
	}
	return out
}

// assistantTurnJSON renders a captured turn's content for a byte-level compare.
func assistantTurnJSON(t *testing.T, turn map[string]any) string {
	t.Helper()
	if turn["role"] != "assistant" {
		t.Fatalf("turn role = %v, want assistant", turn["role"])
	}
	blocks, ok := turn["content"].([]any)
	if !ok {
		// A single-element turn built by ConvertMessages is []map[string]any.
		if mb, ok := turn["content"].([]map[string]any); ok {
			blocks = make([]any, len(mb))
			for i, b := range mb {
				blocks[i] = b
			}
		} else {
			t.Fatalf("assistant content = %T", turn["content"])
		}
	}
	b, err := json.Marshal(blocks)
	if err != nil {
		t.Fatalf("marshal assistant turn: %v", err)
	}
	return string(b)
}

func userHistory() []events.Event {
	return []events.Event{&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "weather in Paris?"}}}}
}

var toolsSchemaGetWeather = tools.Schema{
	Name:        "get_weather",
	Description: "current weather",
	Parameters:  map[string]any{"properties": map[string]any{"city": map[string]any{"type": "string"}}},
}

// TestThinkingReplayTwoCalls is the finish line for the replay item: call 1
// returns the signed blocks, and call 2's body carries them verbatim, in their
// original order, before tool_use.
func TestThinkingReplayTwoCalls(t *testing.T) {
	srv, reqs := captureServer(t, respondWith(thinkingToolResponse))
	conv, sent := newCaptureConv()
	cl := newTestClient(srv, nil)

	resp, err := cl.Call(context.Background(), userHistory(), conv, config.CallOptions{
		Tools: []any{toolsSchemaGetWeather},
	})
	if err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if len(resp.ToolCalls.Calls) != 1 {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}

	history := append(userHistory(), replayEvents(*sent)...)
	history = append(history, resp)
	history = append(history, &events.ToolResultsEvent{Results: []events.ToolResultEvent{
		events.FromResultCall(resp.ToolCalls.Calls[0], events.NewToolResult(false, "sunny, 21C")),
	}})
	if _, err := cl.Call(context.Background(), history, nil, config.CallOptions{}); err != nil {
		t.Fatalf("call 2: %v", err)
	}
	if len(*reqs) != 2 {
		t.Fatalf("got %d requests", len(*reqs))
	}

	msgs := (*reqs)[1]["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("call 2 turns = %d: %v", len(msgs), msgs)
	}
	got := assistantTurnJSON(t, msgs[1].(map[string]any))
	// encoding/json sorts object keys, so the comparison pins the block
	// sequence and every value; object key order is not significant to the API.
	want := `[{"signature":"sig-abc","thinking":"let me think","type":"thinking"},` +
		`{"data":"opaque-xyz","type":"redacted_thinking"},` +
		`{"id":"toolu_1","input":{"city":"Paris"},"name":"get_weather","type":"tool_use"}]`
	if got != want {
		t.Fatalf("replayed assistant turn =\n%s\nwant\n%s", got, want)
	}
}

// A response block with an empty thinking text but a signature is still a
// block the API wants back.
func TestThinkingReplaySignatureOnly(t *testing.T) {
	response := `{"id":"msg_s","content":[{"type":"thinking","thinking":"","signature":"sig-only"}],"stop_reason":"end_turn"}`
	srv, _ := captureServer(t, respondWith(response))
	conv, sent := newCaptureConv()
	cl := newTestClient(srv, nil)
	if _, err := cl.Call(context.Background(), userHistory(), conv, config.CallOptions{}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	replayed := replayEvents(*sent)
	if len(replayed) != 1 {
		t.Fatalf("replay events = %v", replayed)
	}
	ev, ok := events.AsValue(replayed[0]).(ThinkingBlockEvent)
	if !ok || ev.Signature != "sig-only" {
		t.Fatalf("replay event = %+v", replayed[0])
	}
}

const thinkingSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_s","model":"claude-sonnet-4","role":"assistant","content":[],"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"step one "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"step two"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-stream"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"opaque-stream"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_2","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"Paris\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}
`

func TestThinkingReplayStreaming(t *testing.T) {
	srv, reqs := captureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, thinkingSSE)
	})
	conv, sent := newCaptureConv()
	cl := newTestClient(srv, func(c *AnthropicConfig) { c.Streaming = true })

	resp, err := cl.Call(context.Background(), userHistory(), conv, config.CallOptions{})
	if err != nil {
		t.Fatalf("call 1: %v", err)
	}
	replayed := replayEvents(*sent)
	if len(replayed) != 2 {
		t.Fatalf("replay events = %v", replayed)
	}
	thinking, ok := events.AsValue(replayed[0]).(ThinkingBlockEvent)
	if !ok {
		t.Fatalf("first replay event = %T", replayed[0])
	}
	if thinking.Thinking != "step one step two" || thinking.Signature != "sig-stream" {
		t.Fatalf("streamed thinking block = %+v (deltas and signature are one block)", thinking)
	}
	redacted, ok := events.AsValue(replayed[1]).(RedactedThinkingEvent)
	if !ok || redacted.Data != "opaque-stream" {
		t.Fatalf("streamed redacted block = %+v", replayed[1])
	}

	history := append(userHistory(), replayed...)
	history = append(history, resp)
	history = append(history, &events.ToolResultsEvent{Results: []events.ToolResultEvent{
		events.FromResultCall(resp.ToolCalls.Calls[0], events.NewToolResult(false, "sunny, 21C")),
	}})
	if _, err := cl.Call(context.Background(), history, nil, config.CallOptions{}); err != nil {
		t.Fatalf("call 2: %v", err)
	}
	msgs := (*reqs)[1]["messages"].([]any)
	got := assistantTurnJSON(t, msgs[1].(map[string]any))
	want := `[{"signature":"sig-stream","thinking":"step one step two","type":"thinking"},` +
		`{"data":"opaque-stream","type":"redacted_thinking"},` +
		`{"id":"toolu_2","input":{"city":"Paris"},"name":"get_weather","type":"tool_use"}]`
	if got != want {
		t.Fatalf("replayed streaming turn =\n%s\nwant\n%s", got, want)
	}
}

// The replay carrier is a durable event, so a conversational policy keeps it and
// a transcript round-trip preserves it.
func TestReplayEventsRoundTrip(t *testing.T) {
	for _, ev := range []events.Event{
		ThinkingBlockEvent{Thinking: "why", Signature: "sig"},
		RedactedThinkingEvent{Data: "opaque"},
		CacheDiagnostics{Reason: "tools_changed", CacheMissedInputTokens: i64ptr(12), PreviousMessageID: "msg_1"},
	} {
		raw, err := events.MarshalEvent(ev)
		if err != nil {
			t.Fatalf("MarshalEvent(%T): %v", ev, err)
		}
		back, err := events.UnmarshalEvent(raw)
		if err != nil {
			t.Fatalf("UnmarshalEvent(%T): %v", ev, err)
		}
		again, err := events.MarshalEvent(back)
		if err != nil {
			t.Fatalf("MarshalEvent(back %T): %v", ev, err)
		}
		if string(again) != string(raw) {
			t.Fatalf("round trip %T =\n%s\nwant\n%s", ev, again, raw)
		}
	}
}

// The agent persists telemetry (a UsageEvent) between the blocks a call emitted
// and the durable ModelResponse; the blocks must still join one assistant turn.
func TestReplayJoinsResponseAcrossTelemetry(t *testing.T) {
	call := events.NewToolCallEvent("get_weather")
	call.ID = "toolu_1"
	history := append(userHistory(),
		ThinkingBlockEvent{Thinking: "why", Signature: "sig"},
		RedactedThinkingEvent{Data: "opaque"},
		events.NewUsageEvent(events.Usage{}, events.UsageKindModelCall),
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{call}}},
		&events.ToolResultsEvent{Results: []events.ToolResultEvent{
			events.FromResultCall(call, events.NewToolResult(false, "sunny")),
		}},
	)
	turns, err := ConvertMessages(history)
	if err != nil {
		t.Fatalf("ConvertMessages: %v", err)
	}
	if len(turns) != 3 {
		t.Fatalf("turns = %d: %v (the blocks and the response are one assistant turn)", len(turns), turns)
	}
	blocks := turns[1]["content"].([]map[string]any)
	if len(blocks) != 3 || blocks[0]["type"] != "thinking" || blocks[1]["type"] != "redacted_thinking" ||
		blocks[2]["type"] != "tool_use" {
		t.Fatalf("assistant blocks = %v", blocks)
	}
}

// A streamed thinking block may carry no text at all and only a signature
// (max_tokens cut); it is still a block the API wants back.
func TestThinkingReplayStreamingSignatureOnly(t *testing.T) {
	sse := `event: message_start
data: {"type":"message_start","message":{"id":"msg_so","model":"claude-sonnet-4","content":[],"usage":{"input_tokens":3,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-only-stream"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}
`
	srv, _ := captureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse)
	})
	conv, sent := newCaptureConv()
	cl := newTestClient(srv, func(c *AnthropicConfig) { c.Streaming = true })

	if _, err := cl.Call(context.Background(), userHistory(), conv, config.CallOptions{}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	replayed := replayEvents(*sent)
	if len(replayed) != 1 {
		t.Fatalf("replay events = %v, want the signature-only block", replayed)
	}
	ev, ok := events.AsValue(replayed[0]).(ThinkingBlockEvent)
	if !ok || ev.Thinking != "" || ev.Signature != "sig-only-stream" {
		t.Fatalf("streamed block = %+v, want empty text and the signature", replayed[0])
	}
}

// A thinking block with neither text nor signature has nothing to replay.
func TestThinkingBlockWithoutTextOrSignatureIsNotRecorded(t *testing.T) {
	srv, _ := captureServer(t, respondWith(`{"id":"msg_e","content":[{"type":"thinking","thinking":"","signature":""},{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`))
	conv, sent := newCaptureConv()
	cl := newTestClient(srv, nil)
	if _, err := cl.Call(context.Background(), userHistory(), conv, config.CallOptions{}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if replayed := replayEvents(*sent); len(replayed) != 0 {
		t.Fatalf("replay events = %v, want none", replayed)
	}
}

// An empty streamed thinking block (no deltas at all) has nothing to replay.
func TestThinkingReplayStreamingEmptyBlockNotRecorded(t *testing.T) {
	sse := `event: message_start
data: {"type":"message_start","message":{"id":"msg_eb","model":"claude-sonnet-4","content":[],"usage":{"input_tokens":3,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}
`
	srv, _ := captureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse)
	})
	conv, sent := newCaptureConv()
	cl := newTestClient(srv, func(c *AnthropicConfig) { c.Streaming = true })
	if _, err := cl.Call(context.Background(), userHistory(), conv, config.CallOptions{}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if replayed := replayEvents(*sent); len(replayed) != 0 {
		t.Fatalf("replay events = %v, want none for an empty block", replayed)
	}
}

// failStorage refuses every transcript write, so the conversation stream's
// Send returns an error — the path that makes a lost event fail the call.
type failStorage struct{}

func (failStorage) SaveEvent(string, events.Event) error { return errors.New("storage unavailable") }
func (failStorage) GetHistory(string) ([]events.Event, error) {
	return nil, errors.New("storage unavailable")
}
func (failStorage) SetHistory(string, []events.Event) error { return errors.New("storage unavailable") }
func (failStorage) DropHistory(string) error                { return errors.New("storage unavailable") }

// A block the conversation stream refuses to persist fails the call instead of
// being dropped silently: the whole point of the durable replay carrier.
func TestStreamSendFailureFailsTheCall(t *testing.T) {
	sse := `event: message_start
data: {"type":"message_start","message":{"id":"msg_fs","model":"claude-sonnet-4","content":[],"usage":{"input_tokens":3,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-fail"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}
`
	srv, _ := captureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse)
	})
	ms := stream.NewMemoryStream(stream.WithStorage(failStorage{}))
	conv := agocontext.New(ms)
	cl := newTestClient(srv, func(c *AnthropicConfig) { c.Streaming = true })

	if _, err := cl.Call(context.Background(), userHistory(), conv, config.CallOptions{}); err == nil {
		t.Fatal("Call succeeded, want the failed replay-store write to surface")
	}
}
