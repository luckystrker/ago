package agui

import (
	"context"
	"strings"
	"testing"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
)

// enqueueOnFirstCall wraps a scripted config: the first model call queues one
// message on the conversation, as a background task or a concurrent producer
// would while the turn is driving.
type enqueueOnFirstCall struct {
	*agotest.TestConfig
	queued string
}

func (c enqueueOnFirstCall) Create() (config.LLMClient, error) {
	return &enqueueingClient{cfg: c, first: true}, nil
}

func (c enqueueOnFirstCall) Clone(m map[string]any) (config.ModelConfig, error) {
	inner, err := c.TestConfig.Clone(m)
	if err != nil {
		return nil, err
	}
	c.TestConfig = inner.(*agotest.TestConfig)
	return c, nil
}

type enqueueingClient struct {
	cfg   enqueueOnFirstCall
	first bool
}

func (c *enqueueingClient) Call(ctx context.Context, msgs []events.Event, cx *agocontext.Context, opts config.CallOptions) (*events.ModelResponse, error) {
	if c.first {
		c.first = false
		if err := cx.Enqueue(&events.TextInput{Content: c.cfg.queued}); err != nil {
			return nil, err
		}
	}
	return c.cfg.TestConfig.Call(ctx, msgs, cx, opts)
}

func textOf(evs []Event) string {
	var b strings.Builder
	for _, e := range evs {
		if c, ok := e.(TextMessageChunkEvent); ok {
			b.WriteString(c.Delta)
		}
	}
	return b.String()
}

// TestForcedClientHandoffEndsRunWithQueuedInput: calls handed to the client end
// the run. Input queued while the turn was driving must not cause another model
// call whose reply would replace the hand-off (D-127; upstream 3bee17b3c6). The
// script holds only the hand-off response, so a second model call would exhaust
// it and surface as RUN_ERROR.
func TestForcedClientHandoffEndsRunWithQueuedInput(t *testing.T) {
	cfg := enqueueOnFirstCall{
		TestConfig: agotest.NewTestConfig(clientToolCall()),
		queued:     "What about my order?",
	}
	h := ServeAgent(agent.New("served", "You are served.", agent.WithConfig(cfg)))

	evs, raw := postRun(t, h, frontendRunBody(""))
	want := []EventType{
		EventRunStarted,
		EventToolCallStart, EventToolCallArgs, EventToolCallEnd,
		EventRunFinished,
	}
	if got := eventTypes(evs); !equalTypes(got, want) {
		t.Fatalf("types = %v\nwant  = %v\nraw: %q", got, want, raw)
	}
	if start := evs[1].(ToolCallStartEvent); start.ToolCallID != "c1" || start.ToolCallName != "draw_chart" {
		t.Fatalf("start = %+v, want the handed-off call", start)
	}
	if n := len(cfg.Calls()); n != 1 {
		t.Fatalf("model calls = %d, want 1 (the queued input must not trigger another)", n)
	}
}

// TestForcedFinalToolEndsRunWithQueuedInput: a final=true tool result is the
// run's reply even when input was queued while the tool ran.
func TestForcedFinalToolEndsRunWithQueuedInput(t *testing.T) {
	escalate, err := tools.NewFunctionTool("escalate", "Hand the ticket to a person",
		func(args struct{}, cx *agocontext.Context) (events.ToolResult, error) {
			if err := cx.Enqueue(&events.TextInput{Content: "What about my order?"}); err != nil {
				return events.ToolResult{}, err
			}
			return events.ToolResult{Parts: []events.Input{events.TextInput{Content: "Ticket escalated."}}, Final: true}, nil
		})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	cfg := agotest.NewTestConfig(
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "c1", Name: "escalate", Arguments: "{}"},
		}}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: "Your order shipped."}},
	)
	h := ServeAgent(agent.New("served", "", agent.WithConfig(cfg), agent.WithTools(escalate)))

	body := `{"threadId":"t1","runId":"r1","messages":[{"id":"m1","role":"user","content":"I need a person."}]}`
	evs, raw := postRun(t, h, body)
	got := eventTypes(evs)
	if got[0] != EventRunStarted || got[len(got)-1] != EventRunFinished {
		t.Fatalf("types = %v\nraw: %q", got, raw)
	}
	if text := textOf(evs); text != "Ticket escalated." {
		t.Fatalf("run text = %q, want the final tool result", text)
	}
	if n := len(cfg.Calls()); n != 1 {
		t.Fatalf("model calls = %d, want 1 (the queued input must not trigger another)", n)
	}
}
