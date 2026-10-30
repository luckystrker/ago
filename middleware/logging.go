package middleware

import (
	"log/slog"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// Logging logs every hook through slog: turn, LLM call, tool execution and human
// input, each on entry and exit with the duration. The logger is resolved at call
// time (slog.Default()), so slog.SetDefault by the host or tests is honored.
func Logging() Factory {
	return func() Middleware { return &logging{} }
}

type logging struct{ Base }

func errAttr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (l *logging) OnTurn(next TurnFunc, ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
	logger := slog.Default()
	logger.Info("middleware: turn started")
	t0 := time.Now()
	resp, err := next(ev, ctx)
	logger.Info("middleware: turn finished", "duration", time.Since(t0), "err", errAttr(err))
	return resp, err
}

func (l *logging) OnLLMCall(next LLMCallFunc, evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
	logger := slog.Default()
	logger.Info("middleware: llm call", "history_events", len(evs))
	t0 := time.Now()
	resp, err := next(evs, ctx)
	attrs := []any{"duration", time.Since(t0), "err", errAttr(err)}
	if resp != nil {
		attrs = append(attrs, "model", resp.Model, "finish_reason", resp.FinishReason)
	}
	logger.Info("middleware: llm response", attrs...)
	return resp, err
}

func (l *logging) OnToolExecution(next ToolExecFunc, ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
	logger := slog.Default()
	logger.Info("middleware: tool execute", "tool", ev.Name)
	t0 := time.Now()
	result, err := next(ev, ctx)
	attrs := []any{"tool", ev.Name, "duration", time.Since(t0), "err", errAttr(err)}
	if result != nil {
		attrs = append(attrs, "result_type", result.EventType())
	}
	logger.Info("middleware: tool result", attrs...)
	return result, err
}

func (l *logging) OnHumanInput(next HumanInputFunc, ev *events.HumanInputRequest, ctx *agocontext.Context) (*events.HumanMessage, error) {
	logger := slog.Default()
	logger.Info("middleware: human input requested")
	t0 := time.Now()
	msg, err := next(ev, ctx)
	logger.Info("middleware: human input answered", "duration", time.Since(t0), "err", errAttr(err))
	return msg, err
}

var _ Middleware = (*logging)(nil)
