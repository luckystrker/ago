package middleware

import (
	"math"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// Metric names emitted by Metrics. They match the Python MetricsMiddleware
// (middleware/builtin/metrics.py) so dashboards built for AG2 read the same
// series.
const (
	MetricLLMCallsTotal             = "ag2_llm_calls_total"
	MetricLLMCallDurationSeconds    = "ag2_llm_call_duration_seconds"
	MetricLLMTokensTotal            = "ag2_llm_tokens_total"
	MetricToolCallsTotal            = "ag2_tool_calls_total"
	MetricToolDurationSeconds       = "ag2_tool_duration_seconds"
	MetricAgentTurnsTotal           = "ag2_agent_turns_total"
	MetricAgentTurnDurationSeconds  = "ag2_agent_turn_duration_seconds"
	MetricHumanInputRequestsTotal   = "ag2_human_input_requests_total"
	MetricHumanInputDurationSeconds = "ag2_human_input_duration_seconds"
)

// Outcome label values (Python parity: Outcome enum).
const (
	OutcomeSuccess = "success"
	OutcomeError   = "error"
)

// Token type label values (Python parity).
const (
	TokenTypeInput              = "input"
	TokenTypeOutput             = "output"
	TokenTypeTotal              = "total"
	TokenTypeCacheReadInput     = "cache_read_input"
	TokenTypeCacheCreationInput = "cache_creation_input"
	TokenTypeThinking           = "thinking"
)

// Dependency ids Metrics reads from the conversation context. ModelConfigDependencyKey
// is the id the agent already publishes the per-turn model config under;
// AgentDependencyKey is the id an agent/host may publish itself under (a string
// or a value with Name() string). Without it the agent label is "unknown"
// (Python parity: a missing dependency normalizes to "unknown").
const (
	AgentDependencyKey       = "ago:agent"
	ModelConfigDependencyKey = "ago:config"
)

// MetricKind distinguishes counters from histograms.
type MetricKind string

const (
	MetricCounter   MetricKind = "counter"
	MetricHistogram MetricKind = "histogram"
)

// HistogramBucket is one cumulative bucket of a histogram series: Count is the
// number of observations at or below UpperBound.
type HistogramBucket struct {
	UpperBound float64
	Count      uint64
}

// MetricSample is one time series in a registry snapshot.
type MetricSample struct {
	Name   string
	Kind   MetricKind
	Labels map[string]string
	// Value is the counter value, or the histogram's observed sum.
	Value float64
	// Count is the number of histogram observations (0 for counters).
	Count uint64
	// Buckets is the cumulative histogram layout (nil for counters).
	Buckets []HistogramBucket
}

// MetricsRegistry collects the metric series Metrics emits. It is the stdlib
// stand-in for a Prometheus CollectorRegistry: no client library and no OTEL
// export — a host reads Samples and feeds its own backend.
//
// Safe for concurrent use.
type MetricsRegistry struct {
	mu       sync.Mutex
	families map[string]*metricFamily
}

type metricFamily struct {
	name       string
	kind       MetricKind
	labelNames []string
	bounds     []float64
	series     map[string]*metricSeries
}

type metricSeries struct {
	labels       map[string]string
	value        float64
	count        uint64
	sum          float64
	bucketCounts []uint64
}

// llmHistogramBuckets and friends are Python's bucket layouts, in seconds.
var (
	llmHistogramBuckets  = []float64{0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0, 15.0, 20.0, 30.0, inf}
	toolHistogramBuckets = []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0, inf}
	turnHistogramBuckets = []float64{0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0, 20.0, 30.0, 60.0, 120.0, 300.0, 600.0, inf}
	hitlHistogramBuckets = []float64{1.0, 5.0, 10.0, 30.0, 60.0, 120.0, 300.0, 600.0, 1200.0, 1800.0, 3600.0, inf}
)

// inf is the open-ended final histogram bucket (Python: float("inf")).
var inf = math.Inf(1)

// NewMetricsRegistry returns an empty registry with the metric families Metrics
// emits pre-registered.
func NewMetricsRegistry() *MetricsRegistry {
	r := &MetricsRegistry{families: map[string]*metricFamily{}}
	r.register(MetricLLMCallsTotal, MetricCounter, []string{"agent", "provider", "model", "outcome", "finish_reason", "error_type"}, nil)
	r.register(MetricLLMCallDurationSeconds, MetricHistogram, []string{"agent", "provider", "model", "outcome", "error_type"}, llmHistogramBuckets)
	r.register(MetricLLMTokensTotal, MetricCounter, []string{"agent", "provider", "model", "token_type"}, nil)
	r.register(MetricToolCallsTotal, MetricCounter, []string{"agent", "tool", "outcome", "error_type"}, nil)
	r.register(MetricToolDurationSeconds, MetricHistogram, []string{"agent", "tool", "outcome", "error_type"}, toolHistogramBuckets)
	r.register(MetricAgentTurnsTotal, MetricCounter, []string{"agent", "outcome", "error_type"}, nil)
	r.register(MetricAgentTurnDurationSeconds, MetricHistogram, []string{"agent", "outcome", "error_type"}, turnHistogramBuckets)
	r.register(MetricHumanInputRequestsTotal, MetricCounter, []string{"agent", "outcome", "error_type"}, nil)
	r.register(MetricHumanInputDurationSeconds, MetricHistogram, []string{"agent", "outcome", "error_type"}, hitlHistogramBuckets)
	return r
}

// register installs one family. Bounds are copied.
func (r *MetricsRegistry) register(name string, kind MetricKind, labelNames []string, bounds []float64) {
	r.families[name] = &metricFamily{
		name:       name,
		kind:       kind,
		labelNames: append([]string(nil), labelNames...),
		bounds:     append([]float64(nil), bounds...),
		series:     map[string]*metricSeries{},
	}
}

// seriesKey renders labels in the family's declared order.
func seriesKey(f *metricFamily, labels map[string]string) string {
	var b strings.Builder
	for i, name := range f.labelNames {
		if i > 0 {
			b.WriteByte(0)
		}
		b.WriteString(labels[name])
	}
	return b.String()
}

// inc adds delta to a counter series; unknown metrics are ignored (the registry
// is sealed to the middleware's own families).
func (r *MetricsRegistry) inc(name string, labels map[string]string, delta float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.families[name]
	if f == nil || f.kind != MetricCounter {
		return
	}
	r.seriesFor(f, labels).value += delta
}

// observe records one histogram observation.
func (r *MetricsRegistry) observe(name string, labels map[string]string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.families[name]
	if f == nil || f.kind != MetricHistogram {
		return
	}
	s := r.seriesFor(f, labels)
	s.count++
	s.sum += v
	for i, bound := range f.bounds {
		if v <= bound {
			s.bucketCounts[i]++
		}
	}
}

// seriesFor returns the series for labels, creating it on first use. The caller
// holds r.mu.
func (r *MetricsRegistry) seriesFor(f *metricFamily, labels map[string]string) *metricSeries {
	key := seriesKey(f, labels)
	s := f.series[key]
	if s == nil {
		s = &metricSeries{labels: make(map[string]string, len(f.labelNames)), bucketCounts: make([]uint64, len(f.bounds))}
		for _, name := range f.labelNames {
			s.labels[name] = labels[name]
		}
		f.series[key] = s
	}
	return s
}

// Samples returns a stable snapshot, sorted by metric name and then labels.
func (r *MetricsRegistry) Samples() []MetricSample {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]MetricSample, 0, len(r.families))
	for _, f := range r.families {
		for _, s := range f.series {
			out = append(out, f.sample(s))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return sampleKey(out[i]) < sampleKey(out[j])
	})
	return out
}

// Sample looks up one series. The second result is false when no such series
// has been recorded.
func (r *MetricsRegistry) Sample(name string, labels map[string]string) (MetricSample, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.families[name]
	if f == nil {
		return MetricSample{}, false
	}
	s := f.series[seriesKey(f, labels)]
	if s == nil {
		return MetricSample{}, false
	}
	return f.sample(s), true
}

// sample copies one series into a MetricSample.
func (f *metricFamily) sample(s *metricSeries) MetricSample {
	labels := make(map[string]string, len(s.labels))
	for k, v := range s.labels {
		labels[k] = v
	}
	out := MetricSample{Name: f.name, Kind: f.kind, Labels: labels, Value: s.value, Count: s.count}
	if f.kind == MetricHistogram {
		out.Value = s.sum
		out.Buckets = make([]HistogramBucket, len(f.bounds))
		for i, bound := range f.bounds {
			out.Buckets[i] = HistogramBucket{UpperBound: bound, Count: s.bucketCounts[i]}
		}
	}
	return out
}

// sampleKey sorts samples with identical names.
func sampleKey(s MetricSample) string {
	keys := make([]string, 0, len(s.Labels))
	for k := range s.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(s.Labels[k])
		b.WriteByte(0)
	}
	return b.String()
}

// MetricsOption configures Metrics.
type MetricsOption func(*metricsOptions)

type metricsOptions struct {
	agentName string
}

// WithMetricsAgentName fixes the "agent" label. Without it the label is read
// from the AgentDependencyKey dependency on each call and is "unknown" when
// nothing published it.
func WithMetricsAgentName(name string) MetricsOption {
	return func(o *metricsOptions) { o.agentName = name }
}

// Metrics records counters and histograms for turns, LLM calls, tool executions
// and human-input requests into r. A nil registry gets a private one (useful
// only when the caller does not need to read the series).
//
// The labels and metric names match Python's MetricsMiddleware; there is no
// client library and no OTEL exporter (D-048 packaging: a backend would live in
// contrib, not in the core).
//
// Every hook measures its section, records the outcome, and on an error records
// the error's Go type name as error_type (Python parity: type(exc).__name__).
func Metrics(r *MetricsRegistry, opts ...MetricsOption) Factory {
	if r == nil {
		r = NewMetricsRegistry()
	}
	var o metricsOptions
	for _, opt := range opts {
		opt(&o)
	}
	return func() Middleware {
		return &metrics{registry: r, fixedAgent: o.agentName}
	}
}

type metrics struct {
	Base
	registry   *MetricsRegistry
	fixedAgent string
}

// Describe implements Describable.
func (m *metrics) Describe() MiddlewareDescription {
	// The registry is live state; report presence only (Python parity).
	return MiddlewareDescription{
		Kind:     "MetricsMiddleware",
		Config:   map[string]any{"registry": m.registry != nil},
		Complete: true,
	}
}

// agentLabel resolves the "agent" label.
func (m *metrics) agentLabel(ctx *agocontext.Context) string {
	if m.fixedAgent != "" {
		return m.fixedAgent
	}
	if ctx != nil {
		if v, ok := ctx.Dependency(AgentDependencyKey); ok {
			switch a := v.(type) {
			case string:
				return normalizeLabel(a)
			case interface{ Name() string }:
				return normalizeLabel(a.Name())
			}
		}
	}
	return "unknown"
}

// modelLabels resolves the "provider" and "model" labels.
//
// The config is read through reflection because the documented package graph
// keeps middleware above events/context and forbids a config import; the value
// the agent publishes is a config.ModelConfig with Provider() and Model().
func modelLabels(ctx *agocontext.Context) (provider, model string) {
	if ctx == nil {
		return "unknown", "unknown"
	}
	v, ok := ctx.Dependency(ModelConfigDependencyKey)
	if !ok || v == nil {
		return "unknown", "unknown"
	}
	rv := reflect.ValueOf(v)
	return callString(rv, "Provider"), callString(rv, "Model")
}

// callString calls a no-argument method returning a string-kinded value. A
// value it cannot inspect safely — a typed-nil pointer or a foreign method
// signature — falls back to "unknown" instead of taking the process down: the
// dependency is caller-supplied and its shape is not guaranteed.
func callString(rv reflect.Value, method string) string {
	if !rv.IsValid() {
		return "unknown"
	}
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return "unknown"
		}
	}
	m := rv.MethodByName(method)
	if !m.IsValid() {
		return "unknown"
	}
	mt := m.Type()
	if mt.NumIn() != 0 || mt.NumOut() != 1 || mt.Out(0).Kind() != reflect.String {
		return "unknown"
	}
	return normalizeLabel(m.Call(nil)[0].String())
}

// normalizeLabel turns an empty label value into "unknown" (Python parity).
func normalizeLabel(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
}

// errorType names an error's Go type without its package path.
//
// AGO-DEVIATION: Python records type(exc).__name__; Go errors are wrapped, so
// the outermost concrete type is reported (an unnamed type falls back to its
// String form).
func errorType(err error) string {
	if err == nil {
		return ""
	}
	t := reflect.TypeOf(err)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if name := t.Name(); name != "" {
		return name
	}
	return t.String()
}

// OnTurn measures one agent turn.
func (m *metrics) OnTurn(next TurnFunc, ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
	agent := m.agentLabel(ctx)
	start := time.Now()
	resp, err := next(ev, ctx)
	d := time.Since(start).Seconds()
	outcome, errName := OutcomeSuccess, ""
	if err != nil {
		outcome, errName = OutcomeError, errorType(err)
	}
	m.registry.inc(MetricAgentTurnsTotal, map[string]string{"agent": agent, "outcome": outcome, "error_type": errName}, 1)
	m.registry.observe(MetricAgentTurnDurationSeconds, map[string]string{"agent": agent, "outcome": outcome, "error_type": errName}, d)
	return resp, err
}

// OnLLMCall measures one model invocation and its token usage.
func (m *metrics) OnLLMCall(next LLMCallFunc, evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
	agent := m.agentLabel(ctx)
	provider, model := modelLabels(ctx)
	start := time.Now()
	resp, err := next(evs, ctx)
	d := time.Since(start).Seconds()
	if err != nil {
		errName := errorType(err)
		labels := map[string]string{"agent": agent, "provider": provider, "model": model, "outcome": OutcomeError, "error_type": errName}
		m.registry.observe(MetricLLMCallDurationSeconds, labels, d)
		callLabels := map[string]string{"agent": agent, "provider": provider, "model": model, "outcome": OutcomeError, "finish_reason": "unknown", "error_type": errName}
		m.registry.inc(MetricLLMCallsTotal, callLabels, 1)
		return nil, err
	}
	m.registry.observe(MetricLLMCallDurationSeconds, map[string]string{
		"agent": agent, "provider": provider, "model": model, "outcome": OutcomeSuccess, "error_type": "",
	}, d)
	finishReason := "unknown"
	if resp != nil {
		finishReason = normalizeLabel(resp.FinishReason)
	}
	m.registry.inc(MetricLLMCallsTotal, map[string]string{
		"agent": agent, "provider": provider, "model": model,
		"outcome": OutcomeSuccess, "finish_reason": finishReason, "error_type": "",
	}, 1)
	if resp != nil {
		m.recordTokens(agent, provider, model, resp.Usage)
	}
	return resp, nil
}

// recordTokens records every reported token count, skipping unset and zero
// fields (Python parity: only truthy counts are incremented).
func (m *metrics) recordTokens(agent, provider, model string, usage events.Usage) {
	inc := func(tokenType string, value *int64) {
		if value == nil || *value == 0 {
			return
		}
		m.registry.inc(MetricLLMTokensTotal, map[string]string{
			"agent": agent, "provider": provider, "model": model, "token_type": tokenType,
		}, float64(*value))
	}
	inc(TokenTypeInput, usage.PromptTokens)
	inc(TokenTypeOutput, usage.CompletionTokens)
	inc(TokenTypeTotal, usage.TotalTokens)
	inc(TokenTypeCacheReadInput, usage.CacheReadInputTokens)
	inc(TokenTypeCacheCreationInput, usage.CacheCreationInputTokens)
	inc(TokenTypeThinking, usage.ThinkingTokens)
}

// OnToolExecution measures one tool call.
func (m *metrics) OnToolExecution(next ToolExecFunc, ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
	agent := m.agentLabel(ctx)
	tool := normalizeLabel(ev.Name)
	start := time.Now()
	result, err := next(ev, ctx)
	d := time.Since(start).Seconds()
	if err != nil {
		errName := errorType(err)
		labels := map[string]string{"agent": agent, "tool": tool, "outcome": OutcomeError, "error_type": errName}
		m.registry.observe(MetricToolDurationSeconds, labels, d)
		m.registry.inc(MetricToolCallsTotal, labels, 1)
		return nil, err
	}
	outcome, errName := OutcomeSuccess, ""
	if _, isErr := events.AsValue(result).(events.ToolErrorEvent); isErr {
		// AGO-DEVIATION: Python records type(exc).__name__ from the result's
		// error; the Go ToolErrorEvent carries only the message text, so the
		// stable label is the event kind instead of a Go type name.
		outcome, errName = OutcomeError, "tool_error"
	}
	labels := map[string]string{"agent": agent, "tool": tool, "outcome": outcome, "error_type": errName}
	m.registry.observe(MetricToolDurationSeconds, labels, d)
	m.registry.inc(MetricToolCallsTotal, labels, 1)
	return result, err
}

// OnHumanInput measures one HITL request.
func (m *metrics) OnHumanInput(next HumanInputFunc, ev *events.HumanInputRequest, ctx *agocontext.Context) (*events.HumanMessage, error) {
	agent := m.agentLabel(ctx)
	start := time.Now()
	msg, err := next(ev, ctx)
	d := time.Since(start).Seconds()
	outcome, errName := OutcomeSuccess, ""
	if err != nil {
		outcome, errName = OutcomeError, errorType(err)
	}
	labels := map[string]string{"agent": agent, "outcome": outcome, "error_type": errName}
	m.registry.observe(MetricHumanInputDurationSeconds, labels, d)
	m.registry.inc(MetricHumanInputRequestsTotal, labels, 1)
	return msg, err
}

var _ Middleware = (*metrics)(nil)
