package eval

import (
	stdcontext "context"
	"testing"

	"ago/agent"
	"ago/events"
	agotest "ago/testing"
	"ago/tools"
)

func textResp(content string) *events.ModelResponse {
	return &events.ModelResponse{
		Message: &events.ModelMessage{Content: content},
		Usage:   events.Usage{PromptTokens: events.I64(1), CompletionTokens: events.I64(2), TotalTokens: events.I64(3)},
	}
}

func toolResp(name, argsJSON string) *events.ModelResponse {
	call := events.ToolCallEvent{ID: "call-" + name, Name: name, Arguments: argsJSON}
	return &events.ModelResponse{
		ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{call}},
		Usage:     events.Usage{PromptTokens: events.I64(1), TotalTokens: events.I64(1)},
	}
}

func scriptedAgent(t *testing.T, responses ...*events.ModelResponse) *agent.Agent {
	t.Helper()
	weather := func(args struct {
		City string `json:"city" agschema:"description=City name,required"`
	}) (string, error) {
		return "sunny in " + args.City, nil
	}
	tool, err := tools.NewFunctionTool("get_weather", "Get the weather", weather)
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	return agent.New("evaluated", "", agent.WithConfig(agotest.NewTestConfig(responses...)), agent.WithTools(tool))
}

func TestRunSuite(t *testing.T) {
	a := scriptedAgent(t,
		toolResp("get_weather", `{"city":"Paris"}`),
		textResp("It is sunny in Paris today."),
		textResp("The capital of France is Lyon."),
	)
	suite := Suite{
		{
			Name:  "weather",
			Input: "Weather in Paris?",
			Scorers: []Scorer{
				ToolsCalled{Names: []string{"get_weather"}},
				Correctness{Expected: "sunny", Mode: MatchContains},
				TokenBudget{Max: 100},
			},
		},
		{
			Name:     "capital",
			Input:    "Capital of France?",
			Expected: "Lyon",
			Scorers: []Scorer{
				Correctness{Expected: "Lyon", Mode: MatchContains},
				TokenBudget{Max: 3},
			},
		},
		{
			Name:     "off-script",
			Input:    "Anything?",
			Expected: "nope",
			Scorers:  []Scorer{Correctness{Expected: "nope", Mode: MatchExact}},
		},
	}

	res, err := Run(stdcontext.Background(), a, suite)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Cases) != 3 {
		t.Fatalf("cases: %d", len(res.Cases))
	}

	w := res.Cases[0]
	if w.Name != "weather" || w.Output != "It is sunny in Paris today." || !w.Passed {
		t.Errorf("weather case: passed=%v output=%q", w.Passed, w.Output)
	}
	if w.Trace == nil || w.Trace.AgentName != "evaluated" {
		t.Errorf("trace agent name: %+v", w.Trace)
	}
	if len(w.Trace.ToolCalls) != 1 || w.Trace.ToolCalls[0].Name != "get_weather" {
		t.Errorf("tool calls: %+v", w.Trace.ToolCalls)
	}
	if w.Trace.ToolCalls[0].Result != "sunny in Paris" {
		t.Errorf("tool result: %q", w.Trace.ToolCalls[0].Result)
	}
	if len(w.Trace.LLMCalls) != 2 {
		t.Errorf("weather trace llm calls: %d", len(w.Trace.LLMCalls))
	}
	if len(w.Scores) != 3 {
		t.Fatalf("weather scores: %d", len(w.Scores))
	}
	for _, s := range w.Scores {
		if !s.Passed {
			t.Errorf("weather score %s failed: %+v", s.Name, s)
		}
	}

	// capital: answer correct, but the token budget (input+output = 3... plus
	// nothing else) is exactly 3 → passes; make sure the math is per trace.
	c := res.Cases[1]
	if c.Output != "The capital of France is Lyon." {
		t.Errorf("capital output: %q", c.Output)
	}
	if !c.Passed {
		t.Errorf("capital should pass: %+v", c.Scores)
	}
	if got := *c.Trace.Tokens().PromptTokens; got != 1 {
		t.Errorf("capital prompt tokens: %d", got)
	}

	// off-script: the scripted queue ran out — the ask fails, the case records
	// the error and fails without aborting the run.
	o := res.Cases[2]
	if o.Passed || o.Err == "" {
		t.Errorf("off-script case: passed=%v err=%q", o.Passed, o.Err)
	}
	if o.Output != "" {
		t.Errorf("failed ask has no output: %q", o.Output)
	}
	if len(o.Scores) != 1 || o.Scores[0].Passed {
		t.Errorf("off-script scores: %+v", o.Scores)
	}

	if res.PassRate != 2.0/3.0 {
		t.Errorf("pass rate: %v", res.PassRate)
	}
}

func TestRunEmptySuiteAndNilAgent(t *testing.T) {
	a := scriptedAgent(t)
	if _, err := Run(stdcontext.Background(), nil, Suite{}); err == nil {
		t.Error("nil agent must error")
	}
	res, err := Run(stdcontext.Background(), a, Suite{})
	if err != nil {
		t.Fatalf("empty suite: %v", err)
	}
	if len(res.Cases) != 0 || res.PassRate != 0 {
		t.Errorf("empty suite result: %+v", res)
	}
}
