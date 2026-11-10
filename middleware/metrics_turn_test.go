package middleware_test

import (
	stdcontext "context"
	"math"
	"testing"

	"github.com/luckystrker/ago/agent"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/hitl"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
)

// toolResponse scripts one model call that asks for a tool.
func toolResponse(name, args string) *events.ModelResponse {
	call := events.ToolCallEvent{ID: "call-" + name, Name: name, Arguments: args}
	return &events.ModelResponse{
		ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{call}},
		Usage:     events.Usage{PromptTokens: events.I64(1), TotalTokens: events.I64(1)},
	}
}

// textResponse scripts one final model answer with token usage.
func usageTextResponse(content string, prompt, completion, total int64) *events.ModelResponse {
	return &events.ModelResponse{
		Message: &events.ModelMessage{Content: content},
		Usage: events.Usage{
			PromptTokens:     events.I64(prompt),
			CompletionTokens: events.I64(completion),
			TotalTokens:      events.I64(total),
		},
	}
}

// counter reads a counter sample, failing when the series was never recorded.
func counter(t *testing.T, reg *middleware.MetricsRegistry, name string, labels map[string]string) float64 {
	t.Helper()
	s, ok := reg.Sample(name, labels)
	if !ok {
		t.Fatalf("series %s%v was not recorded", name, labels)
	}
	return s.Value
}

// histogram reads a histogram sample.
func histogram(t *testing.T, reg *middleware.MetricsRegistry, name string, labels map[string]string) middleware.MetricSample {
	t.Helper()
	s, ok := reg.Sample(name, labels)
	if !ok {
		t.Fatalf("series %s%v was not recorded", name, labels)
	}
	return s
}

// TestMetricsScriptedTurn pins the counters after a real agent turn: one tool
// call, two LLM calls and one human approval.
func TestMetricsScriptedTurn(t *testing.T) {
	reg := middleware.NewMetricsRegistry()
	cfg := agotest.NewTestConfig(
		toolResponse("echo", `{"x":"1"}`),
		usageTextResponse("done", 2, 3, 5),
	)
	echo, err := tools.NewFunctionTool("echo", "echo the argument",
		func(args struct {
			X string `json:"x"`
		}) (string, error) {
			return args.X, nil
		})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	approve := hitl.HumanHook(func(stdcontext.Context, *events.HumanInputRequest) (*events.HumanMessage, error) {
		return &events.HumanMessage{Content: "y"}, nil
	})
	a := agent.New("main", "", agent.WithConfig(cfg), agent.WithTools(echo),
		agent.WithHitlHook(approve),
		agent.WithMiddleware(
			middleware.Metrics(reg, middleware.WithMetricsAgentName("main")),
			middleware.Approval(),
		))
	if _, err := a.Ask(stdcontext.Background(), "go"); err != nil {
		t.Fatalf("ask: %v", err)
	}

	common := map[string]string{"agent": "main", "provider": "test", "model": "test"}
	if got := counter(t, reg, middleware.MetricAgentTurnsTotal, map[string]string{"agent": "main", "outcome": "success", "error_type": ""}); got != 1 {
		t.Fatalf("agent turns = %v, want 1", got)
	}
	if got := counter(t, reg, middleware.MetricLLMCallsTotal, map[string]string{
		"agent": "main", "provider": "test", "model": "test", "outcome": "success", "finish_reason": "unknown", "error_type": "",
	}); got != 2 {
		t.Fatalf("llm calls = %v, want 2", got)
	}
	if got := counter(t, reg, middleware.MetricToolCallsTotal, map[string]string{"agent": "main", "tool": "echo", "outcome": "success", "error_type": ""}); got != 1 {
		t.Fatalf("tool calls = %v, want 1", got)
	}
	if got := counter(t, reg, middleware.MetricHumanInputRequestsTotal, map[string]string{"agent": "main", "outcome": "success", "error_type": ""}); got != 1 {
		t.Fatalf("human input requests = %v, want 1", got)
	}
	for tokenType, want := range map[string]float64{
		middleware.TokenTypeInput:  3, // 1 + 2
		middleware.TokenTypeOutput: 3, // 0 + 3
		middleware.TokenTypeTotal:  6, // 1 + 5
	} {
		labels := map[string]string{"agent": common["agent"], "provider": common["provider"], "model": common["model"], "token_type": tokenType}
		if got := counter(t, reg, middleware.MetricLLMTokensTotal, labels); got != want {
			t.Fatalf("%s tokens = %v, want %v", tokenType, got, want)
		}
	}
	if _, ok := reg.Sample(middleware.MetricLLMTokensTotal, map[string]string{
		"agent": "main", "provider": "test", "model": "test", "token_type": middleware.TokenTypeCacheReadInput,
	}); ok {
		t.Fatal("an unset cache token count must not create a series")
	}
}

// TestMetricsHistograms pins that the histograms record one observation per
// hook and keep cumulative buckets.
func TestMetricsHistograms(t *testing.T) {
	reg := middleware.NewMetricsRegistry()
	cfg := agotest.NewTestConfig(
		toolResponse("echo", `{"x":"1"}`),
		usageTextResponse("done", 1, 1, 2),
	)
	echo, err := tools.NewFunctionTool("echo", "echo", func(args struct {
		X string `json:"x"`
	}) (string, error) {
		return args.X, nil
	})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	a := agent.New("main", "", agent.WithConfig(cfg), agent.WithTools(echo),
		agent.WithMiddleware(middleware.Metrics(reg, middleware.WithMetricsAgentName("main"))))
	if _, err := a.Ask(stdcontext.Background(), "go"); err != nil {
		t.Fatalf("ask: %v", err)
	}

	turn := histogram(t, reg, middleware.MetricAgentTurnDurationSeconds, map[string]string{"agent": "main", "outcome": "success", "error_type": ""})
	if turn.Count != 1 || len(turn.Buckets) != 14 {
		t.Fatalf("turn histogram = %+v, want one observation and 14 buckets", turn)
	}
	llm := histogram(t, reg, middleware.MetricLLMCallDurationSeconds, map[string]string{"agent": "main", "provider": "test", "model": "test", "outcome": "success", "error_type": ""})
	if llm.Count != 2 {
		t.Fatalf("llm duration count = %d, want 2", llm.Count)
	}
	tool := histogram(t, reg, middleware.MetricToolDurationSeconds, map[string]string{"agent": "main", "tool": "echo", "outcome": "success", "error_type": ""})
	if tool.Count != 1 {
		t.Fatalf("tool duration count = %d, want 1", tool.Count)
	}
	for _, s := range []middleware.MetricSample{turn, llm, tool} {
		if !math.IsInf(s.Buckets[len(s.Buckets)-1].UpperBound, 1) {
			t.Fatalf("%s last bucket = %v, want +Inf", s.Name, s.Buckets[len(s.Buckets)-1].UpperBound)
		}
		if s.Buckets[len(s.Buckets)-1].Count != s.Count {
			t.Fatalf("%s +Inf bucket = %d, want the observation count %d", s.Name, s.Buckets[len(s.Buckets)-1].Count, s.Count)
		}
		for i := 1; i < len(s.Buckets); i++ {
			if s.Buckets[i].Count < s.Buckets[i-1].Count {
				t.Fatalf("%s buckets are not cumulative: %v", s.Name, s.Buckets)
			}
		}
	}
}

// TestMetricsErrorLabels pins the error path labels for every hook.
func TestMetricsErrorLabels(t *testing.T) {
	reg := middleware.NewMetricsRegistry()
	mw := middleware.Metrics(reg, middleware.WithMetricsAgentName("main"))()
	ctx := agocontext.New(stream.NewMemoryStream())
	boom := errBoom{}

	if _, err := mw.OnLLMCall(func([]events.Event, *agocontext.Context) (*events.ModelResponse, error) {
		return nil, boom
	}, nil, ctx); err == nil {
		t.Fatal("OnLLMCall must propagate the error")
	}
	if got := counter(t, reg, middleware.MetricLLMCallsTotal, map[string]string{
		"agent": "main", "provider": "unknown", "model": "unknown", "outcome": "error", "finish_reason": "unknown", "error_type": "errBoom",
	}); got != 1 {
		t.Fatalf("llm error calls = %v, want 1", got)
	}

	if _, err := mw.OnTurn(func(*events.ModelRequest, *agocontext.Context) (*events.ModelResponse, error) {
		return nil, boom
	}, &events.ModelRequest{}, ctx); err == nil {
		t.Fatal("OnTurn must propagate the error")
	}
	if got := counter(t, reg, middleware.MetricAgentTurnsTotal, map[string]string{"agent": "main", "outcome": "error", "error_type": "errBoom"}); got != 1 {
		t.Fatalf("turn errors = %v, want 1", got)
	}

	if _, err := mw.OnToolExecution(func(*events.ToolCallEvent, *agocontext.Context) (events.Event, error) {
		return nil, boom
	}, &events.ToolCallEvent{Name: "shell"}, ctx); err == nil {
		t.Fatal("OnToolExecution must propagate the error")
	}
	if got := counter(t, reg, middleware.MetricToolCallsTotal, map[string]string{"agent": "main", "tool": "shell", "outcome": "error", "error_type": "errBoom"}); got != 1 {
		t.Fatalf("tool errors = %v, want 1", got)
	}

	if _, err := mw.OnHumanInput(func(*events.HumanInputRequest, *agocontext.Context) (*events.HumanMessage, error) {
		return nil, boom
	}, &events.HumanInputRequest{}, ctx); err == nil {
		t.Fatal("OnHumanInput must propagate the error")
	}
	if got := counter(t, reg, middleware.MetricHumanInputRequestsTotal, map[string]string{"agent": "main", "outcome": "error", "error_type": "errBoom"}); got != 1 {
		t.Fatalf("human input errors = %v, want 1", got)
	}
}

// TestMetricsToolErrorEvent pins that a ToolErrorEvent result is an error
// outcome without a Go error.
func TestMetricsToolErrorEvent(t *testing.T) {
	reg := middleware.NewMetricsRegistry()
	mw := middleware.Metrics(reg, middleware.WithMetricsAgentName("main"))()
	ctx := agocontext.New(stream.NewMemoryStream())
	result, err := mw.OnToolExecution(func(*events.ToolCallEvent, *agocontext.Context) (events.Event, error) {
		return events.ToolErrorEvent{Error: "boom"}, nil
	}, &events.ToolCallEvent{Name: "shell"}, ctx)
	if err != nil {
		t.Fatalf("OnToolExecution: %v", err)
	}
	if _, ok := result.(events.ToolErrorEvent); !ok {
		t.Fatalf("result = %T, want ToolErrorEvent pass-through", result)
	}
	if got := counter(t, reg, middleware.MetricToolCallsTotal, map[string]string{"agent": "main", "tool": "shell", "outcome": "error", "error_type": "tool_error"}); got != 1 {
		t.Fatalf("tool error calls = %v, want 1", got)
	}
}

// TestMetricsAgentLabelFromDependency pins the dependency fallback and the
// unknown default.
func TestMetricsAgentLabelFromDependency(t *testing.T) {
	reg := middleware.NewMetricsRegistry()
	ctx := agocontext.New(stream.NewMemoryStream())
	ctx.SetDependency(middleware.AgentDependencyKey, namedAgent("dep-agent"))
	mw := middleware.Metrics(reg)()
	if _, err := mw.OnTurn(func(*events.ModelRequest, *agocontext.Context) (*events.ModelResponse, error) {
		return &events.ModelResponse{}, nil
	}, &events.ModelRequest{}, ctx); err != nil {
		t.Fatalf("OnTurn: %v", err)
	}
	if got := counter(t, reg, middleware.MetricAgentTurnsTotal, map[string]string{"agent": "dep-agent", "outcome": "success", "error_type": ""}); got != 1 {
		t.Fatalf("turns = %v, want 1 for the dependency agent", got)
	}

	bareReg := middleware.NewMetricsRegistry()
	bare := middleware.Metrics(bareReg)()
	if _, err := bare.OnTurn(func(*events.ModelRequest, *agocontext.Context) (*events.ModelResponse, error) {
		return &events.ModelResponse{}, nil
	}, &events.ModelRequest{}, agocontext.New(stream.NewMemoryStream())); err != nil {
		t.Fatalf("OnTurn: %v", err)
	}
	if got := counter(t, bareReg, middleware.MetricAgentTurnsTotal, map[string]string{"agent": "unknown", "outcome": "success", "error_type": ""}); got != 1 {
		t.Fatalf("turns = %v, want 1 for the unknown agent", got)
	}
}

// TestMetricsSamplesAreSorted pins the stable snapshot order.
func TestMetricsSamplesAreSorted(t *testing.T) {
	reg := middleware.NewMetricsRegistry()
	mw := middleware.Metrics(reg, middleware.WithMetricsAgentName("main"))()
	ctx := agocontext.New(stream.NewMemoryStream())
	_, _ = mw.OnTurn(func(*events.ModelRequest, *agocontext.Context) (*events.ModelResponse, error) {
		return &events.ModelResponse{}, nil
	}, &events.ModelRequest{}, ctx)
	samples := reg.Samples()
	if len(samples) == 0 {
		t.Fatal("no samples")
	}
	for i := 1; i < len(samples); i++ {
		if samples[i-1].Name > samples[i].Name {
			t.Fatalf("samples not sorted: %q before %q", samples[i-1].Name, samples[i].Name)
		}
	}
	if _, ok := reg.Sample("no_such_metric", nil); ok {
		t.Fatal("unknown metric must not resolve")
	}
}

// TestMetricsFinishReasonLabels pins the finish_reason label: a provider
// reason is used verbatim, an empty one normalizes to "unknown" (Python
// parity: normalize(response.finish_reason)).
func TestMetricsFinishReasonLabels(t *testing.T) {
	tests := []struct {
		reason string
		want   string
	}{
		{reason: "stop", want: "stop"},
		{reason: "", want: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			reg := middleware.NewMetricsRegistry()
			mw := middleware.Metrics(reg, middleware.WithMetricsAgentName("main"))()
			ctx := agocontext.New(stream.NewMemoryStream())
			if _, err := mw.OnLLMCall(func([]events.Event, *agocontext.Context) (*events.ModelResponse, error) {
				return &events.ModelResponse{FinishReason: tt.reason}, nil
			}, nil, ctx); err != nil {
				t.Fatalf("OnLLMCall: %v", err)
			}
			if got := counter(t, reg, middleware.MetricLLMCallsTotal, map[string]string{
				"agent": "main", "provider": "unknown", "model": "unknown", "outcome": "success", "finish_reason": tt.want, "error_type": "",
			}); got != 1 {
				t.Fatalf("llm calls = %v, want 1 with finish_reason=%q", got, tt.want)
			}
		})
	}
}

// TestMetricsErrorTypePointer pins that a pointer error is named by its
// element type (Python parity: the class name, not the pointer spelling).
func TestMetricsErrorTypePointer(t *testing.T) {
	reg := middleware.NewMetricsRegistry()
	mw := middleware.Metrics(reg, middleware.WithMetricsAgentName("main"))()
	ctx := agocontext.New(stream.NewMemoryStream())
	if _, err := mw.OnLLMCall(func([]events.Event, *agocontext.Context) (*events.ModelResponse, error) {
		return nil, &errBoom{}
	}, nil, ctx); err == nil {
		t.Fatal("OnLLMCall must propagate the error")
	}
	if got := counter(t, reg, middleware.MetricLLMCallsTotal, map[string]string{
		"agent": "main", "provider": "unknown", "model": "unknown", "outcome": "error", "finish_reason": "unknown", "error_type": "errBoom",
	}); got != 1 {
		t.Fatalf("llm error calls = %v, want 1 with error_type errBoom", got)
	}
}

// errBoom is a named error type for the error_type label.
type errBoom struct{}

func (errBoom) Error() string { return "boom" }

// argProviderConfig has a Provider method with an argument, so reflect.Call(nil)
// would panic.
type argProviderConfig struct{}

func (argProviderConfig) Provider(extra string) string { return extra }
func (argProviderConfig) Model() string                { return "m" }

// multiResultConfig has a Model method with two results, so it cannot yield a
// single label.
type multiResultConfig struct{}

func (multiResultConfig) Provider() string        { return "p" }
func (multiResultConfig) Model() (string, string) { return "m", "extra" }

// intModelConfig returns a non-string label.
type intModelConfig struct{}

func (intModelConfig) Provider() string { return "p" }
func (intModelConfig) Model() int       { return 7 }

// nilReceiverConfig dereferences its receiver, so calling it on a typed-nil
// pointer would panic.
type nilReceiverConfig struct{ provider string }

func (c *nilReceiverConfig) Provider() string { return c.provider }
func (c *nilReceiverConfig) Model() string    { return c.provider }

// TestMetricsModelLabelsTolerateForeignConfig pins the defensive reflection
// path: a caller-supplied value under the model-config dependency can have a
// foreign method signature or be a typed-nil pointer, and neither may panic the
// middleware.
func TestMetricsModelLabelsTolerateForeignConfig(t *testing.T) {
	tests := []struct {
		name         string
		dep          any
		wantProvider string
		wantModel    string
	}{
		{name: "method takes an argument", dep: argProviderConfig{}, wantProvider: "unknown", wantModel: "m"},
		{name: "method returns two values", dep: multiResultConfig{}, wantProvider: "p", wantModel: "unknown"},
		{name: "method returns a non-string", dep: intModelConfig{}, wantProvider: "p", wantModel: "unknown"},
		{name: "typed-nil pointer", dep: (*nilReceiverConfig)(nil), wantProvider: "unknown", wantModel: "unknown"},
		{name: "value without the methods", dep: "not a config", wantProvider: "unknown", wantModel: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := middleware.NewMetricsRegistry()
			mw := middleware.Metrics(reg, middleware.WithMetricsAgentName("main"))()
			ctx := agocontext.New(stream.NewMemoryStream())
			ctx.SetDependency(middleware.ModelConfigDependencyKey, tt.dep)
			if _, err := mw.OnLLMCall(func([]events.Event, *agocontext.Context) (*events.ModelResponse, error) {
				return &events.ModelResponse{}, nil
			}, nil, ctx); err != nil {
				t.Fatalf("OnLLMCall: %v", err)
			}
			if got := counter(t, reg, middleware.MetricLLMCallsTotal, map[string]string{
				"agent": "main", "provider": tt.wantProvider, "model": tt.wantModel, "outcome": "success", "finish_reason": "unknown", "error_type": "",
			}); got != 1 {
				t.Fatalf("llm calls = %v, want 1 with provider=%q model=%q", got, tt.wantProvider, tt.wantModel)
			}
		})
	}
}

// namedAgent exposes Name() for the agent dependency.
type namedAgent string

func (n namedAgent) Name() string { return string(n) }
