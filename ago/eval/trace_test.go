package eval

import (
	"context"
	"testing"
	"time"

	"ago/config"
	agocontext "ago/context"
	"ago/events"
)

func at(offset int) time.Time {
	return time.Unix(1_700_000_000, 0).Add(time.Duration(offset) * time.Second)
}

func stamped(base events.Base, offset int) events.Base {
	base.CreatedAt = at(offset)
	return base
}

func usageOf(prompt, completion int64) events.Usage {
	return events.Usage{PromptTokens: events.I64(prompt), CompletionTokens: events.I64(completion)}
}

// scriptClient hands out canned bodies as judge/pairwise verdicts and records
// the prompts it was asked.
type scriptClient struct {
	bodies  []string
	i       int
	prompts []string
}

func (s *scriptClient) Call(_ context.Context, msgs []events.Event, _ *agocontext.Context, _ config.CallOptions) (*events.ModelResponse, error) {
	req, _ := events.AsValue(msgs[0]).(events.ModelRequest)
	s.prompts = append(s.prompts, events.RenderForPrompt(req))
	body := ""
	if s.i < len(s.bodies) {
		body = s.bodies[s.i]
		s.i++
	}
	return &events.ModelResponse{Message: &events.ModelMessage{Content: body}}, nil
}

func TestBuildTraceToolRound(t *testing.T) {
	call := events.ToolCallEvent{ID: "c1", Name: "get_weather", Arguments: `{"city":"Paris"}`}
	evs := []events.Event{
		&events.ModelRequest{Base: stamped(events.Base{}, 0), Parts: []events.Input{&events.TextInput{Content: "weather in Paris?"}}},
		&events.UsageEvent{Base: stamped(events.Base{}, 1), Usage: usageOf(10, 2), Kind: events.UsageKindModelCall, Model: "m1", Provider: "test"},
		&events.ModelResponse{Base: stamped(events.Base{}, 2), ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{call}}, Usage: usageOf(10, 2), Model: "m1", Provider: "test"},
		&call,
		&events.ToolResultEvent{Base: stamped(events.Base{}, 3), ParentID: "c1", Name: "get_weather", Result: events.NewToolResult(false, "21C and sunny")},
		&events.UsageEvent{Base: stamped(events.Base{}, 4), Usage: usageOf(20, 8), Kind: events.UsageKindModelCall, Model: "m1", Provider: "test"},
		&events.ModelResponse{Base: stamped(events.Base{}, 5), Message: &events.ModelMessage{Content: "It is 21C and sunny in Paris."}, Usage: usageOf(20, 8), Model: "m2", Provider: "test"},
	}

	tr := BuildTrace(evs)
	if tr.AgentName != "" {
		t.Errorf("agent name from bare history: %q", tr.AgentName)
	}
	if !tr.StartedAt.Equal(at(0)) || !tr.FinishedAt.Equal(at(5)) {
		t.Errorf("span: %v..%v", tr.StartedAt, tr.FinishedAt)
	}
	if len(tr.LLMCalls) != 2 {
		t.Fatalf("llm calls: %d", len(tr.LLMCalls))
	}
	first := tr.LLMCalls[0]
	if first.InputText != "weather in Paris?" {
		t.Errorf("call 0 input: %q", first.InputText)
	}
	if first.OutputText != "" {
		t.Errorf("tool-call round should have no text output: %q", first.OutputText)
	}
	if got := *first.Usage.PromptTokens; got != 10 {
		t.Errorf("call 0 prompt tokens: %d", got)
	}
	second := tr.LLMCalls[1]
	if second.InputText != "" {
		t.Errorf("tool round must not consume the user input twice: %q", second.InputText)
	}
	if second.OutputText != "It is 21C and sunny in Paris." || second.Model != "m2" {
		t.Errorf("call 1: %+v", second)
	}
	if len(tr.ToolCalls) != 1 {
		t.Fatalf("tool calls: %d", len(tr.ToolCalls))
	}
	tc := tr.ToolCalls[0]
	if tc.Name != "get_weather" || tc.Arguments != `{"city":"Paris"}` {
		t.Errorf("tool call: %+v", tc)
	}
	if tc.Result != "21C and sunny" || tc.Err != "" {
		t.Errorf("tool result: %+v", tc)
	}
	if tr.FinalResponse != "It is 21C and sunny in Paris." {
		t.Errorf("final: %q", tr.FinalResponse)
	}
	// Accounting counts UsageEvents only — ModelResponse.Usage would double every call.
	tk := tr.Tokens()
	if *tk.PromptTokens != 30 || *tk.CompletionTokens != 10 {
		t.Errorf("tokens: %+v", tk)
	}
}

func TestBuildTraceToolErrorAndNotFound(t *testing.T) {
	call := events.ToolCallEvent{ID: "c2", Name: "boom", Arguments: "{}"}
	missing := events.NewToolCallEvent("no_such_tool")
	evs := []events.Event{
		&events.ModelRequest{Base: stamped(events.Base{}, 0), Parts: []events.Input{&events.TextInput{Content: "hi"}}},
		&events.ModelResponse{Base: stamped(events.Base{}, 1), Message: &events.ModelMessage{Content: "sorry"}, Model: "m"},
		&call,
		&events.ToolErrorEvent{ToolResultEvent: events.ToolResultEvent{ParentID: "c2", Name: "boom"}, Error: "exploded"},
		&missing,
		&events.ToolNotFoundEvent{ToolErrorEvent: events.FromErrorCall(missing, context.DeadlineExceeded)},
		&events.ModelResponse{Base: stamped(events.Base{}, 2), Message: &events.ModelMessage{Content: "gave up"}},
	}
	tr := BuildTrace(evs)
	if len(tr.ToolCalls) != 2 {
		t.Fatalf("tool calls: %d", len(tr.ToolCalls))
	}
	if tr.ToolCalls[0].Err != "exploded" || tr.ToolCalls[0].Result != "" {
		t.Errorf("error call: %+v", tr.ToolCalls[0])
	}
	if tr.ToolCalls[1].Name != "no_such_tool" || tr.ToolCalls[1].Err == "" {
		t.Errorf("not-found call: %+v", tr.ToolCalls[1])
	}
	if tr.FinalResponse != "gave up" {
		t.Errorf("final: %q", tr.FinalResponse)
	}
}

func TestBuildTraceEmpty(t *testing.T) {
	tr := BuildTrace(nil)
	if tr == nil || len(tr.LLMCalls) != 0 || len(tr.ToolCalls) != 0 || tr.FinalResponse != "" {
		t.Errorf("empty trace: %+v", tr)
	}
	if !tr.StartedAt.IsZero() || !tr.FinishedAt.IsZero() {
		t.Errorf("empty trace should have no span: %v..%v", tr.StartedAt, tr.FinishedAt)
	}
}
