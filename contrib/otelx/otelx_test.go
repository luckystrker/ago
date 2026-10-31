package otelx

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// newRecorder builds an SDK provider feeding an in-memory span recorder.
func newRecorder(t *testing.T) (*sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return tp, sr
}

// ended snapshots the finished spans as field-accessible stubs.
func ended(sr *tracetest.SpanRecorder) []tracetest.SpanStub {
	return tracetest.SpanStubsFromReadOnlySpans(sr.Ended())
}

func findSpan(t *testing.T, spans []tracetest.SpanStub, name string) tracetest.SpanStub {
	t.Helper()
	for _, s := range spans {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("span %q not recorded; recorded: %v", name, spanNames(spans))
	return tracetest.SpanStub{}
}

func spanNames(spans []tracetest.SpanStub) []string {
	names := make([]string, 0, len(spans))
	for _, s := range spans {
		names = append(names, s.Name)
	}
	return names
}

func attrMap(s tracetest.SpanStub) map[attribute.Key]attribute.Value {
	m := make(map[attribute.Key]attribute.Value, len(s.Attributes))
	for _, kv := range s.Attributes {
		m[kv.Key] = kv.Value
	}
	return m
}

func wantInt64(t *testing.T, m map[attribute.Key]attribute.Value, key attribute.Key, want int64) {
	t.Helper()
	got, ok := m[key]
	if !ok {
		t.Errorf("attribute %s missing", key)
		return
	}
	if got.AsInt64() != want {
		t.Errorf("attribute %s = %d, want %d", key, got.AsInt64(), want)
	}
}

func wantString(t *testing.T, m map[attribute.Key]attribute.Value, key attribute.Key, want string) {
	t.Helper()
	got, ok := m[key]
	if !ok {
		t.Errorf("attribute %s missing", key)
		return
	}
	if got.AsString() != want {
		t.Errorf("attribute %s = %q, want %q", key, got.AsString(), want)
	}
}

func TestMiddlewareNestsSpansUnderTurn(t *testing.T) {
	tp, sr := newRecorder(t)
	tel := Middleware(tp)()

	_, err := tel.OnTurn(func(ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
		if _, err := tel.OnLLMCall(func(evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
			return &events.ModelResponse{Model: "gpt-4o-mini", Provider: "openai"}, nil
		}, nil, nil); err != nil {
			return nil, err
		}
		if _, err := tel.OnToolExecution(func(ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
			return events.ToolResultEvent{}, nil
		}, &events.ToolCallEvent{ID: "call-1", Name: "lookup"}, nil); err != nil {
			return nil, err
		}
		return &events.ModelResponse{}, nil
	}, &events.ModelRequest{}, nil)
	if err != nil {
		t.Fatalf("OnTurn: %v", err)
	}

	spans := ended(sr)
	if len(spans) != 3 {
		t.Fatalf("recorded %d spans (%v), want 3", len(spans), spanNames(spans))
	}
	turn := findSpan(t, spans, spanTurn)
	llm := findSpan(t, spans, spanLLM)
	tool := findSpan(t, spans, spanTool)

	if turn.SpanContext.TraceID() != llm.SpanContext.TraceID() ||
		turn.SpanContext.TraceID() != tool.SpanContext.TraceID() {
		t.Fatalf("spans not on one trace: turn=%v llm=%v tool=%v",
			turn.SpanContext.TraceID(), llm.SpanContext.TraceID(), tool.SpanContext.TraceID())
	}
	if turn.Parent.IsValid() {
		t.Errorf("turn span should be a root, parent=%v", turn.Parent)
	}
	for _, s := range []tracetest.SpanStub{llm, tool} {
		if s.Parent.SpanID() != turn.SpanContext.SpanID() {
			t.Errorf("span %s parent %v, want turn span %v", s.Name, s.Parent.SpanID(), turn.SpanContext.SpanID())
		}
	}
	if tool.SpanKind != trace.SpanKindInternal {
		t.Errorf("tool span kind %v, want internal", tool.SpanKind)
	}
	if llm.SpanKind != trace.SpanKindClient {
		t.Errorf("llm span kind %v, want client", llm.SpanKind)
	}
	if turn.Status.Code == codes.Error || llm.Status.Code == codes.Error || tool.Status.Code == codes.Error {
		t.Errorf("success path marked spans as error: %v %v %v", turn.Status, llm.Status, tool.Status)
	}
}

func TestMiddlewareLLMCallAttributes(t *testing.T) {
	tp, sr := newRecorder(t)
	tel := Middleware(tp)()

	resp := &events.ModelResponse{
		Model:        "gpt-4o-mini",
		Provider:     "openai",
		FinishReason: "stop",
		Usage: events.Usage{
			PromptTokens:             events.I64(10),
			CompletionTokens:         events.I64(5),
			TotalTokens:              events.I64(15),
			CacheReadInputTokens:     events.I64(2),
			CacheCreationInputTokens: events.I64(3),
			ThinkingTokens:           events.I64(4),
		},
	}
	if _, err := tel.OnLLMCall(func(evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
		return resp, nil
	}, nil, nil); err != nil {
		t.Fatalf("OnLLMCall: %v", err)
	}

	m := attrMap(findSpan(t, ended(sr), spanLLM))
	wantString(t, m, "gen_ai.operation.name", "chat")
	wantString(t, m, "gen_ai.provider.name", "openai")
	wantString(t, m, "gen_ai.request.model", "gpt-4o-mini")
	wantString(t, m, "gen_ai.response.model", "gpt-4o-mini")
	wantInt64(t, m, "gen_ai.usage.input_tokens", 10)
	wantInt64(t, m, "gen_ai.usage.output_tokens", 5)
	wantInt64(t, m, "gen_ai.usage.total_tokens", 15)
	wantInt64(t, m, "gen_ai.usage.cache_read_input_tokens", 2)
	wantInt64(t, m, "gen_ai.usage.cache_creation_input_tokens", 3)
	wantInt64(t, m, "gen_ai.usage.thinking_tokens", 4)
	if got, ok := m["gen_ai.response.finish_reasons"]; !ok || got.AsStringSlice()[0] != "stop" {
		t.Errorf("finish_reasons = %v, want [stop]", m["gen_ai.response.finish_reasons"])
	}
}

func TestMiddlewareLLMCallEmptyResponseOmitsAttributes(t *testing.T) {
	tp, sr := newRecorder(t)
	tel := Middleware(tp)()

	if _, err := tel.OnLLMCall(func(evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
		return &events.ModelResponse{}, nil
	}, nil, nil); err != nil {
		t.Fatalf("OnLLMCall: %v", err)
	}

	m := attrMap(findSpan(t, ended(sr), spanLLM))
	for _, key := range []attribute.Key{
		"gen_ai.request.model", "gen_ai.response.model", "gen_ai.provider.name",
		"gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens", "gen_ai.usage.total_tokens",
	} {
		if _, ok := m[key]; ok {
			t.Errorf("attribute %s set on an empty response, want omitted", key)
		}
	}
	if _, ok := m["gen_ai.operation.name"]; !ok {
		t.Errorf("attribute gen_ai.operation.name missing")
	}
}

func TestMiddlewareErrorMarksSpan(t *testing.T) {
	tp, sr := newRecorder(t)
	tel := Middleware(tp)()
	boom := errors.New("boom")

	if _, err := tel.OnLLMCall(func(evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
		return nil, boom
	}, nil, nil); !errors.Is(err, boom) {
		t.Fatalf("OnLLMCall error = %v, want boom", err)
	}
	span := findSpan(t, ended(sr), spanLLM)
	if span.Status.Code != codes.Error {
		t.Errorf("status = %v, want error", span.Status.Code)
	}
	if span.Status.Description != "boom" {
		t.Errorf("status description %q, want %q", span.Status.Description, "boom")
	}
	if !hasExceptionEvent(span) {
		t.Errorf("no exception event recorded on %s", span.Name)
	}
}

func TestMiddlewareTurnErrorMarksSpan(t *testing.T) {
	tp, sr := newRecorder(t)
	tel := Middleware(tp)()

	if _, err := tel.OnTurn(func(ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
		return nil, errors.New("turn failed")
	}, &events.ModelRequest{}, nil); err == nil {
		t.Fatal("OnTurn error swallowed")
	}
	span := findSpan(t, ended(sr), spanTurn)
	if span.Status.Code != codes.Error {
		t.Errorf("status = %v, want error", span.Status.Code)
	}
	if got, ok := attrMap(span)["gen_ai.operation.name"]; !ok || got.AsString() != "invoke_agent" {
		t.Errorf("gen_ai.operation.name = %v, want invoke_agent", span.Attributes)
	}
}

func TestMiddlewareToolSpan(t *testing.T) {
	tp, sr := newRecorder(t)
	tel := Middleware(tp)()

	if _, err := tel.OnToolExecution(func(ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
		return events.ToolResultEvent{ParentID: ev.ID}, nil
	}, &events.ToolCallEvent{ID: "call-1", Name: "lookup"}, nil); err != nil {
		t.Fatalf("OnToolExecution: %v", err)
	}

	span := findSpan(t, ended(sr), spanTool)
	m := attrMap(span)
	wantString(t, m, "gen_ai.operation.name", "execute_tool")
	wantString(t, m, "gen_ai.tool.name", "lookup")
	wantString(t, m, "gen_ai.tool.call.id", "call-1")
	wantString(t, m, "gen_ai.tool.type", "function")
	if span.Status.Code == codes.Error {
		t.Errorf("successful tool marked as error: %v", span.Status)
	}
}

func TestMiddlewareToolErrorEventMarksSpan(t *testing.T) {
	for name, result := range map[string]events.Event{
		"value":   events.ToolErrorEvent{ToolResultEvent: events.ToolResultEvent{ParentID: "call-1"}, Error: "kaput"},
		"pointer": &events.ToolErrorEvent{ToolResultEvent: events.ToolResultEvent{ParentID: "call-1"}, Error: "kaput"},
	} {
		t.Run(name, func(t *testing.T) {
			tp, sr := newRecorder(t)
			tel := Middleware(tp)()

			if _, err := tel.OnToolExecution(func(ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
				return result, nil
			}, &events.ToolCallEvent{ID: "call-1", Name: "lookup"}, nil); err != nil {
				t.Fatalf("OnToolExecution: %v", err)
			}
			span := findSpan(t, ended(sr), spanTool)
			if span.Status.Code != codes.Error {
				t.Errorf("status = %v, want error", span.Status.Code)
			}
			if span.Status.Description != "kaput" {
				t.Errorf("status description %q, want %q", span.Status.Description, "kaput")
			}
			if !hasExceptionEvent(span) {
				t.Errorf("no exception event recorded on %s", span.Name)
			}
		})
	}
}

func hasExceptionEvent(s tracetest.SpanStub) bool {
	for _, ev := range s.Events {
		if ev.Name == "exception" {
			return true
		}
	}
	return false
}

// exceptionMessage returns the exception.message attribute of the first
// exception event on the span, or "" when none was recorded.
func exceptionMessage(s tracetest.SpanStub) string {
	for _, ev := range s.Events {
		if ev.Name != "exception" {
			continue
		}
		for _, kv := range ev.Attributes {
			if kv.Key == "exception.message" {
				return kv.Value.AsString()
			}
		}
	}
	return ""
}

func TestTruncateMessage(t *testing.T) {
	cyrillic := strings.Repeat("ж", 200) // 2 bytes per rune: cut lands mid-rune at odd limits
	tests := []struct {
		name  string
		msg   string
		limit int
		want  string
	}{
		{name: "short unchanged", msg: "boom", limit: maxSpanErrorMessage, want: "boom"},
		{name: "exactly at limit", msg: strings.Repeat("a", 32), limit: 32, want: strings.Repeat("a", 32)},
		{name: "over limit cut", msg: strings.Repeat("a", 33), limit: 32, want: strings.Repeat("a", 32) + truncationSuffix},
		{
			// The byte cut at 33 falls inside a 2-byte rune; the cut must back
			// off to a rune boundary so the recorded text stays valid UTF-8.
			name:  "multibyte boundary",
			msg:   cyrillic,
			limit: 33,
			want:  strings.Repeat("ж", 16) + truncationSuffix,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := truncateMessage(tt.msg, tt.limit); got != tt.want {
				t.Errorf("truncateMessage(%q, %d) = %q, want %q", tt.msg, tt.limit, got, tt.want)
			}
		})
	}
}

// TestSpanErrorMessagesTruncated: a tool error carrying a long text (as if a
// response body were embedded) must reach the span capped at
// maxSpanErrorMessage — both in the status description and the exception event.
func TestSpanErrorMessagesTruncated(t *testing.T) {
	const tail = "SENSITIVE-TAIL"
	long := strings.Repeat("e", maxSpanErrorMessage+200) + tail

	tp, sr := newRecorder(t)
	tel := Middleware(tp)()
	if _, err := tel.OnToolExecution(func(ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
		return events.ToolErrorEvent{ToolResultEvent: events.ToolResultEvent{ParentID: ev.ID}, Error: long}, nil
	}, &events.ToolCallEvent{ID: "call-1", Name: "lookup"}, nil); err != nil {
		t.Fatalf("OnToolExecution: %v", err)
	}

	span := findSpan(t, ended(sr), spanTool)
	want := strings.Repeat("e", maxSpanErrorMessage) + truncationSuffix
	if span.Status.Description != want {
		t.Errorf("status description len %d, want %d bytes ending in suffix", len(span.Status.Description), len(want))
	}
	if msg := exceptionMessage(span); msg != want {
		t.Errorf("exception message len %d, want %d bytes ending in suffix", len(msg), len(want))
	}
	if strings.Contains(span.Status.Description, tail) || strings.Contains(exceptionMessage(span), tail) {
		t.Errorf("span error text leaks the truncated tail")
	}
}

// TestSpanPipelineErrorTruncated: the same cap applies to provider errors
// returned by the pipeline (endSpan path).
func TestSpanPipelineErrorTruncated(t *testing.T) {
	const tail = "SENSITIVE-TAIL"
	long := strings.Repeat("p", maxSpanErrorMessage+100) + tail

	tp, sr := newRecorder(t)
	tel := Middleware(tp)()
	if _, err := tel.OnLLMCall(func(evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
		return nil, errors.New(long)
	}, nil, nil); err == nil {
		t.Fatal("OnLLMCall error swallowed")
	}

	span := findSpan(t, ended(sr), spanLLM)
	want := strings.Repeat("p", maxSpanErrorMessage) + truncationSuffix
	if span.Status.Description != want {
		t.Errorf("status description len %d, want %d bytes ending in suffix", len(span.Status.Description), len(want))
	}
	if msg := exceptionMessage(span); msg != want {
		t.Errorf("exception message len %d, want %d bytes ending in suffix", len(msg), len(want))
	}
	if strings.Contains(span.Status.Description, tail) || strings.Contains(exceptionMessage(span), tail) {
		t.Errorf("span error text leaks the truncated tail")
	}
}

// TestMiddlewareConcurrentToolExecution exercises the parent bookkeeping under
// the concurrency the tool executor applies to one instance.
func TestMiddlewareConcurrentToolExecution(t *testing.T) {
	tp, sr := newRecorder(t)
	tel := Middleware(tp)()
	const n = 8

	_, err := tel.OnTurn(func(ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
		var wg sync.WaitGroup
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = tel.OnToolExecution(func(ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
					return events.ToolResultEvent{}, nil
				}, &events.ToolCallEvent{ID: "call", Name: "lookup"}, nil)
			}(i)
		}
		wg.Wait()
		for _, e := range errs {
			if e != nil {
				return nil, e
			}
		}
		return &events.ModelResponse{}, nil
	}, &events.ModelRequest{}, nil)
	if err != nil {
		t.Fatalf("OnTurn: %v", err)
	}

	turn := findSpan(t, ended(sr), spanTurn)
	tools := 0
	for _, s := range ended(sr) {
		if s.Name != spanTool {
			continue
		}
		tools++
		if s.SpanContext.TraceID() != turn.SpanContext.TraceID() {
			t.Errorf("tool span on trace %v, want %v", s.SpanContext.TraceID(), turn.SpanContext.TraceID())
		}
		if s.Parent.SpanID() != turn.SpanContext.SpanID() {
			t.Errorf("tool span parent %v, want turn span %v", s.Parent.SpanID(), turn.SpanContext.SpanID())
		}
	}
	if tools != n {
		t.Fatalf("recorded %d tool spans, want %d", tools, n)
	}
}

// TestMiddlewareInstancesAreIndependent: a Factory builds per-turn instances;
// one turn's span must not parent another instance's spans.
func TestMiddlewareInstancesAreIndependent(t *testing.T) {
	tp, sr := newRecorder(t)
	outer := Middleware(tp)()
	inner := Middleware(tp)()

	if _, err := outer.OnTurn(func(ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
		if _, err := inner.OnLLMCall(func(evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
			return &events.ModelResponse{}, nil
		}, nil, nil); err != nil {
			return nil, err
		}
		return &events.ModelResponse{}, nil
	}, &events.ModelRequest{}, nil); err != nil {
		t.Fatalf("OnTurn: %v", err)
	}

	llm := findSpan(t, ended(sr), spanLLM)
	if llm.Parent.IsValid() {
		t.Errorf("other instance's llm span parented under %v, want a root", llm.Parent)
	}
}

// TestMiddlewareNilProviderFallsBackToGlobal: a nil provider uses the global
// one — with nothing installed the noop provider makes the hooks safe no-ops.
func TestMiddlewareNilProviderFallsBackToGlobal(t *testing.T) {
	tel := Middleware(nil)()
	if _, err := tel.OnTurn(func(ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
		return &events.ModelResponse{}, nil
	}, &events.ModelRequest{}, nil); err != nil {
		t.Fatalf("OnTurn with nil provider: %v", err)
	}
}

func TestMiddlewareOnHumanInputPassesThrough(t *testing.T) {
	tp, sr := newRecorder(t)
	tel := Middleware(tp)()

	if _, err := tel.OnHumanInput(func(ev *events.HumanInputRequest, ctx *agocontext.Context) (*events.HumanMessage, error) {
		return &events.HumanMessage{}, nil
	}, &events.HumanInputRequest{}, nil); err != nil {
		t.Fatalf("OnHumanInput: %v", err)
	}
	if n := len(ended(sr)); n != 0 {
		t.Errorf("OnHumanInput recorded %d spans, want 0 (pass-through)", n)
	}
}
