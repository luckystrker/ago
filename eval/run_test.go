package eval

import (
	stdcontext "context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/events"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
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

	// Concurrency 1: the scripted queue is per-case FIFO, so the deterministic
	// sequential mode is what a scripted suite needs.
	res, err := Run(stdcontext.Background(), a, suite, WithConcurrency(1))
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

// rendezvousSuite gives every case its own prompt and a contains-check on the
// scripted reply.
func rendezvousSuite(n int) Suite {
	suite := make(Suite, n)
	for i := range suite {
		suite[i] = Case{
			Name:     fmt.Sprintf("case-%d", i),
			Input:    fmt.Sprintf("q%d", i),
			Expected: "done",
			Scorers:  []Scorer{Correctness{Expected: "done", Mode: MatchContains}},
		}
	}
	return suite
}

// rendezvousResponses scripts every case to call the tool once, then answer.
func rendezvousResponses(n int) []*events.ModelResponse {
	resps := make([]*events.ModelResponse, 0, 2*n)
	for i := 0; i < n; i++ {
		resps = append(resps,
			toolResp("rendezvous", "{}"),
			textResp("all done"),
		)
	}
	return resps
}

func TestRunConcurrentCases(t *testing.T) {
	const n = 4
	var peak atomic.Int64
	a := agent.New("evaluated", "",
		agent.WithConfig(agotest.NewTestConfig(rendezvousResponses(n)...)),
		agent.WithTools(mustRendezvousTool(t, 100*time.Millisecond, &peak)),
	)

	// Default concurrency (4): the four cases overlap inside the tool.
	res, err := Run(stdcontext.Background(), a, rendezvousSuite(n))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if peak.Load() < 2 {
		t.Errorf("cases did not overlap, peak in-flight = %d", peak.Load())
	}
	if res.PassRate != 1.0 {
		t.Fatalf("pass rate: %v (%+v)", res.PassRate, res.Cases)
	}
	// The result stays in suite order regardless of completion order.
	for i, cr := range res.Cases {
		if cr.Name != fmt.Sprintf("case-%d", i) || cr.Output != "all done" {
			t.Errorf("case %d out of place: %+v", i, cr)
		}
	}
}

func TestRunConcurrencyLimits(t *testing.T) {
	const n = 3
	// A fresh agent per run: the scripted queue is consumed once per run, so
	// each limit check needs its own script and its own peak counter.
	runLimit := func(limit int) int64 {
		t.Helper()
		var peak atomic.Int64
		a := agent.New("evaluated", "",
			agent.WithConfig(agotest.NewTestConfig(rendezvousResponses(n)...)),
			agent.WithTools(mustRendezvousTool(t, 30*time.Millisecond, &peak)),
		)
		res, err := Run(stdcontext.Background(), a, rendezvousSuite(n), WithConcurrency(limit))
		if err != nil {
			t.Fatalf("run limit %d: %v", limit, err)
		}
		if res.PassRate != 1.0 {
			t.Fatalf("run limit %d pass rate: %v", limit, res.PassRate)
		}
		return peak.Load()
	}
	if got := runLimit(1); got != 1 {
		t.Errorf("WithConcurrency(1) must serialize cases, peak = %d", got)
	}
	// Zero or negative clamps to 1 (Python parity: max(1, concurrency)).
	if got := runLimit(0); got != 1 {
		t.Errorf("zero concurrency must clamp to 1, peak = %d", got)
	}
}

func mustRendezvousTool(t *testing.T, d time.Duration, peak *atomic.Int64) tools.Tool {
	t.Helper()
	var inFlight atomic.Int64
	fn := func(struct{}) (string, error) {
		cur := inFlight.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		time.Sleep(d)
		inFlight.Add(-1)
		return "done", nil
	}
	tool, err := tools.NewFunctionTool("rendezvous", "Touch the barrier", fn)
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	return tool
}

// TestRunFailureMarksTraceErr pins the ask error onto the trace, where the
// failure attribution scorer reads it (Python parity: Trace.exception). The
// suite overdraws the script: whichever case misses the single scripted
// response fails, and exactly one does.
func TestRunFailureMarksTraceErr(t *testing.T) {
	a := scriptedAgent(t, textResp("only one answer"))
	suite := Suite{
		{Name: "a", Input: "q", Expected: "only", Scorers: []Scorer{Correctness{Expected: "only", Mode: MatchContains}, FailureAttribution{}}},
		{Name: "b", Input: "q2", Expected: "only", Scorers: []Scorer{Correctness{Expected: "only", Mode: MatchContains}, FailureAttribution{}}},
	}
	res, err := Run(stdcontext.Background(), a, suite, WithConcurrency(2))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var failed, ok *CaseResult
	for i := range res.Cases {
		if res.Cases[i].Err != "" {
			failed = &res.Cases[i]
		} else {
			ok = &res.Cases[i]
		}
	}
	if failed == nil || ok == nil {
		t.Fatalf("exactly one case must fail: %+v", res.Cases)
	}
	if !ok.Passed || ok.Trace.Err != "" {
		t.Errorf("clean case: %+v", ok)
	}
	if failed.Trace.Err == "" {
		t.Errorf("failed ask must stamp Trace.Err")
	}
	attr := failed.Scores[1]
	if attr.Name != "failure" || attr.Passed || attr.Details["error_mode"] != "crash" {
		t.Errorf("attribution on failed ask: %+v", attr)
	}
}
