package middleware

import (
	"fmt"
	"reflect"
)

// MiddlewareDescription reports a middleware's type and configuration for logs
// and spec output (Python parity: middleware/describe.py).
//
// Kind is the Python middleware kind so descriptions compare across the two
// ports. Config is a flat settings map and deliberately excludes state that
// changes during a run. Complete is false when the configuration could not be
// fully captured; an inner description that is incomplete also makes the outer
// one incomplete. Inner carries the wrapped middleware of a composite.
type MiddlewareDescription struct {
	Kind     string
	Config   map[string]any
	Complete bool
	Inner    []MiddlewareDescription
}

// Describe reports this middleware. Implement it to opt in; middleware without
// it is described by Describe with Complete=false.
type Describable interface {
	Describe() MiddlewareDescription
}

// Describe returns the description of m: its own Describe when implemented,
// otherwise a best-effort type name marked incomplete. It never panics on nil.
func Describe(m Middleware) MiddlewareDescription {
	if m == nil {
		return MiddlewareDescription{Kind: "Middleware", Complete: false}
	}
	if d, ok := m.(Describable); ok {
		return d.Describe()
	}
	return MiddlewareDescription{Kind: typeName(m), Complete: false}
}

// DescribeFactory instantiates f and describes the instance. A factory is a
// constructor with no identity surface in Go, so Python's factory-level
// describe() lives here on the per-turn instance instead.
//
// AGO-DEVIATION: Python describes the factory object; AGo's Factory is a
// closure, so the description is taken from a fresh instance.
func DescribeFactory(f Factory) MiddlewareDescription {
	if f == nil {
		return MiddlewareDescription{Kind: "Middleware", Complete: false}
	}
	return Describe(f())
}

// typeName is the Go analog of Python's type(m).__qualname__.
func typeName(v any) string {
	t := reflect.TypeOf(v)
	if t == nil {
		return "unknown"
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Name()
}

// withInner attaches inner descriptions, propagating their completeness.
func withInner(d MiddlewareDescription, inner ...MiddlewareDescription) MiddlewareDescription {
	d.Inner = inner
	for _, in := range inner {
		if !in.Complete {
			d.Complete = false
		}
	}
	return d
}

// describeCondition names a condition. Go conditions are closures with no type
// identity, so a named condition cannot be recovered; the description says
// only whether any condition is set and is marked incomplete.
func describeCondition(cond any) (string, bool) {
	if cond == nil {
		return "all", true
	}
	return fmt.Sprintf("%T", cond), false
}

// Describe implements Describable.
func (h *historyLimiter) Describe() MiddlewareDescription {
	return MiddlewareDescription{Kind: "HistoryLimiter", Config: map[string]any{"max_events": h.max}, Complete: true}
}

// Describe implements Describable.
func (t *tokenLimiter) Describe() MiddlewareDescription {
	charsPerToken := t.charsPerToken
	if charsPerToken <= 0 {
		charsPerToken = 4 // events.EstimatedTokens default
	}
	return MiddlewareDescription{
		Kind:     "TokenLimiter",
		Config:   map[string]any{"max_tokens": t.budget, "chars_per_token": charsPerToken},
		Complete: true,
	}
}

// Describe implements Describable.
func (r *retry) Describe() MiddlewareDescription {
	config := map[string]any{}
	if r.retriesOnly {
		config["max_retries"] = r.attempts
	} else {
		config["attempts"] = r.attempts
	}
	// A Go retry filter is a function value: its identity (Python's tuple of
	// exception class names) cannot be recovered, so the description says only
	// that a custom filter is installed and is marked incomplete.
	complete := true
	if r.filter != nil {
		config["retry_on"] = "custom"
		complete = false
	}
	return MiddlewareDescription{Kind: "RetryMiddleware", Config: config, Complete: complete}
}

// Describe implements Describable.
func (l *logging) Describe() MiddlewareDescription {
	return MiddlewareDescription{
		Kind:     "LoggingMiddleware",
		Config:   map[string]any{"logger": "slog.Default", "err_text_limit": l.opts.errTextLimit},
		Complete: true,
	}
}

// Describe implements Describable.
func (a *approval) Describe() MiddlewareDescription {
	message := a.opts.message
	if !a.opts.hasMessage {
		message = approvalPrompt
		if !a.opts.allowAlways {
			message = approvalPromptNoAlways
		}
	}
	return MiddlewareDescription{
		Kind: "ApprovalRequired",
		Config: map[string]any{
			"message":            message,
			"denied_message":     a.opts.deniedMessage,
			"timeout":            a.opts.timeoutSecs,
			"allow_always":       a.opts.allowAlways,
			"args_scoped_always": a.opts.argsScopedAlways,
		},
		Complete: true,
	}
}

// Describe implements Describable.
func (s *statusbar) Describe() MiddlewareDescription {
	return MiddlewareDescription{
		Kind:     "statusbar_middleware",
		Config:   map[string]any{"persist": s.persist, "callback": s.source},
		Complete: true,
	}
}

// Describe implements Describable.
func (c *conditional) Describe() MiddlewareDescription {
	kind, complete := describeCondition(c.cond)
	return withInner(MiddlewareDescription{
		Kind:     "ConditionalMiddleware",
		Config:   map[string]any{"condition": kind},
		Complete: complete,
	}, Describe(c.inner))
}

// Describe implements Describable.
func (c *chain) Describe() MiddlewareDescription {
	inner := make([]MiddlewareDescription, len(c.mws))
	for i, mw := range c.mws {
		inner[i] = Describe(mw)
	}
	return withInner(MiddlewareDescription{Kind: "chain", Complete: true}, inner...)
}
