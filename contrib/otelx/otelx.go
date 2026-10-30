// Package otelx bridges the AGo middleware hooks into OpenTelemetry spans: a
// turn, every LLM call and every tool execution become one span each on a
// single trace, errors recorded as span errors. It closes the Python AG2
// TelemetryMiddleware gap as an optional module — the core stays
// dependency-free (D-003), the exporter stays the user's choice: Middleware
// accepts any trace.TracerProvider, InstallDefault wires the SDK one with
// stdout or OTLP/HTTP export.
//
// Go has no ambient context, so unlike Python's start_as_current_span the
// parenting is explicit: each Middleware instance spans one turn (a Factory is
// instantiated per turn) and llm/tool spans started below its OnTurn parent
// under the turn span; started outside a turn they are fresh roots.
//
// AGO-DEVIATION (D-064): span names are ago.turn/ago.llm_call/ago.tool_execution
// (Python: "invoke_agent <agent>"/"chat"/"execute_tool <name>"), per the contrib
// contract; content capture (message bodies, tool arguments/results), the
// record_usage subscription spans, the await_human_input span, the constructor
// knobs (agent/provider/model names, span_attributes) and the traceparent
// dependency relaying are deferred there.
package otelx

import (
	"context"
	"errors"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/middleware"
)

const (
	spanTurn = "ago.turn"
	spanLLM  = "ago.llm_call"
	spanTool = "ago.tool_execution"

	// schemaURL matches the Python tracer (OTEL_SCHEMA_URL), so traces from
	// both ports declare the same schema.
	schemaURL = "https://opentelemetry.io/schemas/1.11.0"

	// instrumentingModule names the tracer scope. Python uses
	// "opentelemetry.instrumentation.ag2"; the Go module path is the Go
	// convention (AGO-DEVIATION, D-064).
	instrumentingModule = "github.com/luckystrker/ago/contrib/otelx"
)

// Middleware returns a middleware.Factory emitting OTel spans around the turn
// hooks: ago.turn (internal), ago.llm_call (client; model, provider and
// usage-token attributes read off the ModelResponse) and ago.tool_execution
// (internal; tool name and call id). A nil tp falls back to the process-global
// provider, mirroring the Python default. The returned instances are per-turn
// and safe for concurrent tool execution.
func Middleware(tp trace.TracerProvider) middleware.Factory {
	return func() middleware.Middleware {
		provider := tp
		if provider == nil {
			provider = otel.GetTracerProvider()
		}
		return &telemetry{
			tracer: provider.Tracer(instrumentingModule, trace.WithSchemaURL(schemaURL)),
		}
	}
}

// telemetry carries the span parent of the turn it was built for. The hook
// signatures carry no context.Context, so the turn span context is stored on
// the instance (written once in OnTurn before any child hook can run, read
// under a mutex because OnToolExecution runs concurrently).
type telemetry struct {
	middleware.Base // OnHumanInput passes through (deferred with D-064)

	tracer trace.Tracer

	mu     sync.Mutex
	parent trace.SpanContext
}

func (t *telemetry) parentContext() context.Context {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.parent.IsValid() {
		return context.Background()
	}
	return trace.ContextWithSpanContext(context.Background(), t.parent)
}

func (t *telemetry) OnTurn(next middleware.TurnFunc, ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
	_, span := t.tracer.Start(context.Background(), spanTurn, trace.WithSpanKind(trace.SpanKindInternal))
	t.mu.Lock()
	t.parent = span.SpanContext()
	t.mu.Unlock()
	span.SetAttributes(attribute.String("gen_ai.operation.name", "invoke_agent"))

	resp, err := next(ev, ctx)
	endSpan(span, err)
	return resp, err
}

func (t *telemetry) OnLLMCall(next middleware.LLMCallFunc, evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
	_, span := t.tracer.Start(t.parentContext(), spanLLM, trace.WithSpanKind(trace.SpanKindClient))

	resp, err := next(evs, ctx)
	if resp != nil {
		setLLMAttributes(span, resp)
	}
	endSpan(span, err)
	return resp, err
}

// setLLMAttributes stamps the reply's model/provider/usage onto the span. Only
// reported fields are set: a nil token count stays absent (Python parity — the
// original guards each `if usage.x:`).
func setLLMAttributes(span trace.Span, resp *events.ModelResponse) {
	span.SetAttributes(attribute.String("gen_ai.operation.name", "chat"))
	if resp.Provider != "" {
		span.SetAttributes(attribute.String("gen_ai.provider.name", resp.Provider))
	}
	if resp.Model != "" {
		span.SetAttributes(
			attribute.String("gen_ai.request.model", resp.Model),
			attribute.String("gen_ai.response.model", resp.Model),
		)
	}
	if resp.FinishReason != "" {
		span.SetAttributes(attribute.StringSlice("gen_ai.response.finish_reasons", []string{resp.FinishReason}))
	}
	u := resp.Usage
	if u.PromptTokens != nil {
		span.SetAttributes(attribute.Int64("gen_ai.usage.input_tokens", *u.PromptTokens))
	}
	if u.CompletionTokens != nil {
		span.SetAttributes(attribute.Int64("gen_ai.usage.output_tokens", *u.CompletionTokens))
	}
	if u.CacheCreationInputTokens != nil {
		span.SetAttributes(attribute.Int64("gen_ai.usage.cache_creation_input_tokens", *u.CacheCreationInputTokens))
	}
	if u.CacheReadInputTokens != nil {
		span.SetAttributes(attribute.Int64("gen_ai.usage.cache_read_input_tokens", *u.CacheReadInputTokens))
	}
	if u.ThinkingTokens != nil {
		span.SetAttributes(attribute.Int64("gen_ai.usage.thinking_tokens", *u.ThinkingTokens))
	}
	if u.TotalTokens != nil {
		span.SetAttributes(attribute.Int64("gen_ai.usage.total_tokens", *u.TotalTokens))
	}
}

func (t *telemetry) OnToolExecution(next middleware.ToolExecFunc, ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
	_, span := t.tracer.Start(t.parentContext(), spanTool, trace.WithSpanKind(trace.SpanKindInternal))
	span.SetAttributes(
		attribute.String("gen_ai.operation.name", "execute_tool"),
		attribute.String("gen_ai.tool.name", ev.Name),
		attribute.String("gen_ai.tool.call.id", ev.ID),
		attribute.String("gen_ai.tool.type", "function"),
	)

	result, err := next(ev, ctx)
	// A ToolErrorEvent is a *successful* next() that reports a failed tool:
	// mark the span like the Python original records the error there (Python
	// parity: isinstance(result, ToolErrorEvent)).
	if err == nil {
		if msg, failed := toolErrorMessage(result); failed {
			span.RecordError(errors.New(msg))
			span.SetStatus(codes.Error, msg)
		}
	}
	endSpan(span, err)
	return result, err
}

// toolErrorMessage reports whether the event answers a tool call with a tool
// failure, and its message. Both value and pointer shapes satisfy events.Event
// (value receiver on EventType), so both are matched.
func toolErrorMessage(result events.Event) (string, bool) {
	switch r := result.(type) {
	case events.ToolErrorEvent:
		return r.Error, true
	case *events.ToolErrorEvent:
		if r == nil {
			return "", false
		}
		return r.Error, true
	}
	return "", false
}

// endSpan closes a hook span: a pipeline error is recorded as an exception
// event with the span status set to Error; success leaves the status Unset
// (the OTel convention for a span that did its job).
func endSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

var _ middleware.Middleware = (*telemetry)(nil)
