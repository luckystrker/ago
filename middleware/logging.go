package middleware

import (
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// defaultErrTextLimit caps the "err" attribute of every log line. Provider and
// transport errors routinely embed fragments of the failed request (URLs,
// headers, echoed payloads), so an unbounded err.Error() in the log leaks them
// (audit L-20).
const defaultErrTextLimit = 512

// LoggingOption configures Logging.
type LoggingOption func(*loggingOptions)

type loggingOptions struct {
	errTextLimit int
}

// WithErrTextLimit sets the cap, in bytes, for the "err" attribute of every log
// line; text past the cap is replaced by a "...[N more chars]" marker. n <= 0
// disables truncation (the full error text is logged).
func WithErrTextLimit(n int) LoggingOption {
	return func(o *loggingOptions) { o.errTextLimit = n }
}

// Logging logs every hook through slog: turn, LLM call, tool execution and human
// input, each on entry and exit with the duration. The logger is resolved at call
// time (slog.Default()), so slog.SetDefault by the host or tests is honored.
//
// Error attributes are truncated to defaultErrTextLimit bytes (disable or resize
// with WithErrTextLimit) so request fragments echoed inside provider errors do
// not reach the log verbatim.
func Logging(opts ...LoggingOption) Factory {
	var o loggingOptions
	o.errTextLimit = defaultErrTextLimit
	for _, opt := range opts {
		opt(&o)
	}
	return func() Middleware { return &logging{opts: o} }
}

type logging struct {
	Base
	opts loggingOptions
}

// errAttr renders the "err" attribute: empty for nil, truncated to the
// configured limit otherwise.
func (l *logging) errAttr(err error) string {
	if err == nil {
		return ""
	}
	return truncateErrText(err.Error(), l.opts.errTextLimit)
}

// truncateErrText keeps the first limit bytes of s — never splitting a UTF-8
// rune — and appends a marker counting the dropped characters. limit <= 0 keeps
// s unchanged.
func truncateErrText(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	end := limit
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + fmt.Sprintf("...[%d more chars]", len(s)-end)
}

func (l *logging) OnTurn(next TurnFunc, ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error) {
	logger := slog.Default()
	logger.Info("middleware: turn started")
	t0 := time.Now()
	resp, err := next(ev, ctx)
	logger.Info("middleware: turn finished", "duration", time.Since(t0), "err", l.errAttr(err))
	return resp, err
}

func (l *logging) OnLLMCall(next LLMCallFunc, evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error) {
	logger := slog.Default()
	logger.Info("middleware: llm call", "history_events", len(evs))
	t0 := time.Now()
	resp, err := next(evs, ctx)
	attrs := []any{"duration", time.Since(t0), "err", l.errAttr(err)}
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
	attrs := []any{"tool", ev.Name, "duration", time.Since(t0), "err", l.errAttr(err)}
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
	logger.Info("middleware: human input answered", "duration", time.Since(t0), "err", l.errAttr(err))
	return msg, err
}

var _ Middleware = (*logging)(nil)
