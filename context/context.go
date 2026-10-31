// Package context provides the conversation context: the handle every subscriber,
// tool and middleware uses to read state, enqueue input, send events and ask the
// human operator (HITL).
//
// Parity with Python AG2 ag2/context.py ConversationContext. The import alias
// `agocontext` distinguishes it from the standard library "context" package.
package context

import (
	stdcontext "context"
	"errors"
	"math"
	"reflect"
	"sync"
	"time"

	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/hitl"
	"github.com/luckystrker/ago/stream"
)

// Context carries per-conversation state. Safe for concurrent use.
type Context struct {
	Stream stream.Stream

	// Dependencies holds injected values addressed by string id (DI parity:
	// Python ctx.dependencies / fast_depends Inject).
	Dependencies map[string]any
	// Variables holds serializable per-conversation variables; prefixes "ag:" and
	// "a2a:" are reserved for framework use and stripped on the wire.
	Variables map[string]any
	// Prompt holds system-prompt fragments joined for each model call.
	Prompt []string
	// ToolCallID is set while a tool executes (parity: Python tool_call_id).
	ToolCallID string

	mu      sync.RWMutex
	hook    hitl.HumanHook
	turnCtx stdcontext.Context
}

func New(s stream.Stream) *Context {
	return &Context{
		Stream:       s,
		Dependencies: map[string]any{},
		Variables:    map[string]any{},
	}
}

func NewWithDeps(s stream.Stream, deps map[string]any, vars map[string]any) *Context {
	c := New(s)
	for k, v := range deps {
		c.Dependencies[k] = v
	}
	for k, v := range vars {
		c.Variables[k] = v
	}
	return c
}

// --- transcript & inbox -----------------------------------------------------

// History returns a snapshot of the durable transcript.
func (c *Context) History() ([]events.Event, error) { return c.Stream.History().GetEvents() }

// Pending returns a snapshot of the inbox.
func (c *Context) Pending() []*events.ModelRequest { return c.Stream.Pending() }

// Drain empties the inbox.
func (c *Context) Drain() []*events.ModelRequest { return c.Stream.Drain() }

// Enqueue appends input parts to the inbox.
func (c *Context) Enqueue(parts ...events.Input) error { return c.Stream.Enqueue(parts...) }

// Send publishes an event on the conversation stream.
func (c *Context) Send(ev events.Event) error { return c.Stream.Send(ev) }

// SpawnBackground runs fn on a stream-tracked goroutine.
func (c *Context) SpawnBackground(ctx stdcontext.Context, fn func(stdcontext.Context)) {
	c.Stream.SpawnBackground(ctx, fn)
}

// --- dependency / variable access -------------------------------------------

func (c *Context) Dependency(id string) (any, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.Dependencies[id]
	return v, ok
}

func (c *Context) SetDependency(id string, v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Dependencies == nil {
		c.Dependencies = map[string]any{}
	}
	c.Dependencies[id] = v
}

func (c *Context) MustDependency(id string) (any, error) {
	if v, ok := c.Dependency(id); ok {
		return v, nil
	}
	return nil, errors.New("context: missing dependency " + id)
}

// DependenciesSnapshot returns a shallow copy of the dependencies map, taken
// under the lock (Python parity: ctx.dependencies.copy() — the copy handed to a
// sub-task so the child cannot mutate the parent's map and vice versa).
func (c *Context) DependenciesSnapshot() map[string]any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]any, len(c.Dependencies))
	for k, v := range c.Dependencies {
		out[k] = v
	}
	return out
}

// VariablesSnapshot returns a shallow copy of the variables map, taken under
// the lock (Python parity: ctx.variables.copy()).
func (c *Context) VariablesSnapshot() map[string]any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]any, len(c.Variables))
	for k, v := range c.Variables {
		out[k] = v
	}
	return out
}

func (c *Context) Variable(name string) (any, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.Variables[name]
	return v, ok
}

func (c *Context) SetVariable(name string, v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Variables == nil {
		c.Variables = map[string]any{}
	}
	c.Variables[name] = v
}

// AddPrompt appends system-prompt fragments.
func (c *Context) AddPrompt(fragments ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Prompt = append(c.Prompt, fragments...)
}

// PromptLines returns a copy of the prompt fragments.
func (c *Context) PromptLines() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, len(c.Prompt))
	copy(out, c.Prompt)
	return out
}

// Clone shares the stream and hook but copies the mutable maps (per-invocation
// overrides are applied to the clone, Python parity: replace(context, ...)).
func (c *Context) Clone() *Context {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cc := &Context{
		Stream:     c.Stream,
		ToolCallID: c.ToolCallID,
		hook:       c.hook,
		turnCtx:    c.turnCtx,
	}
	cc.Dependencies = make(map[string]any, len(c.Dependencies))
	for k, v := range c.Dependencies {
		cc.Dependencies[k] = v
	}
	cc.Variables = make(map[string]any, len(c.Variables))
	for k, v := range c.Variables {
		cc.Variables[k] = v
	}
	cc.Prompt = make([]string, len(c.Prompt))
	copy(cc.Prompt, c.Prompt)
	return cc
}

// WithToolCallID returns a clone stamped with a tool call id (executor parity).
func (c *Context) WithToolCallID(id string) *Context {
	cc := c.Clone()
	cc.ToolCallID = id
	return cc
}

// SetTurnContext installs the cancellable std-context of the turn currently
// owning the conversation. Middleware hooks carry no stdlib context parameter,
// so this is the channel through which turn cancellation reaches code that
// only sees the conversation context — most importantly Context.Ask callers
// waiting on a human answer (audit M-26). The agent sets it when a turn scope
// opens; a nil ctx clears it.
func (c *Context) SetTurnContext(ctx stdcontext.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.turnCtx = ctx
}

// TurnContext returns the installed turn context, or stdcontext.Background()
// when no turn is running. The result is never nil, so callers can hand it
// straight to operations that honor cancellation; outside a turn it is simply
// not cancellable.
func (c *Context) TurnContext() stdcontext.Context {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.turnCtx == nil {
		return stdcontext.Background()
	}
	return c.turnCtx
}

// --- human-in-the-loop ------------------------------------------------------

// SetHitlHook installs the hook answering human input requests. The agent wraps the
// user hook with on_human_input middleware before installing it.
func (c *Context) SetHitlHook(h hitl.HumanHook) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hook = h
}

// Hook returns the installed hook (nil when none).
func (c *Context) Hook() hitl.HumanHook {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.hook
}

// Ask requests human input and waits for the answer. The request goes out on the
// stream, where a per-request interrupt subscription hands it to the hook (Python
// parity: the wrapped hook is an interrupt subscriber; its wrapper returning None
// stops dispatch, so the question itself is not transcribed). The answer is
// consumed via Stream.Get on HumanMessage.parent_id == req.ID, so an external
// participant can answer instead of the hook. req.Timeout covers both the asking
// and the waiting. Errors are normalized to hitl errors: no hook →
// NotProvidedError, hook failure → FailedError (even at the deadline — Python:
// only the wait can produce a timeout), deadline → TimeoutError.
func (c *Context) Ask(ctx stdcontext.Context, req *events.HumanInputRequest) (string, error) {
	// The timeout travels inside an event that untrusted participants can
	// construct; NaN/Inf/negative values must not reach the deadline
	// arithmetic below (audit L-19).
	req.Timeout = sanitizeTimeout(req.Timeout)
	if req.ID == "" {
		r := events.NewHumanInputRequest(req.Content, req.Timeout)
		r.Base = req.Base
		req = &r
	}
	hook := c.Hook()
	if hook == nil {
		return "", &hitl.NotProvidedError{Detail: "request: " + req.Content}
	}
	if req.Timeout > 0 {
		// A finite but huge value can still overflow the float→Duration
		// conversion (its result is undefined): anything unrepresentable is
		// treated as no deadline, leaving turn cancellation as the backstop.
		if d := time.Duration(req.Timeout * float64(time.Second)); d > 0 {
			var cancel stdcontext.CancelFunc
			ctx, cancel = stdcontext.WithTimeout(ctx, d)
			defer cancel()
		}
	}

	// The answer is consumed through the stream, so the wait must be subscribed
	// before an answer can exist: Get subscribes on entry, hence it starts before
	// the question goes out. A hook answering within dispatch is also handed to
	// Ask directly — Get's subscription lives on another goroutine and could
	// otherwise lose an instant answer (Python enters stream.get's subscription
	// before asking, so it has no such race to close).
	gctx, cancelGet := stdcontext.WithCancel(ctx)
	defer cancelGet()
	got := make(chan *events.HumanMessage, 1)
	getErr := make(chan error, 1)
	go func() {
		ev, err := c.Stream.Get(gctx, answerCondition(req.ID))
		if err != nil {
			getErr <- err
			return
		}
		if hm, ok := ev.(*events.HumanMessage); ok {
			got <- hm
		}
	}()

	// Dispatch is synchronous (D-004), so the hook runs on its own goroutine: a
	// hook that never returns must not pin the stream or outlive the deadline.
	// Go cannot cancel a goroutine — an abandoned hook's eventual answer is
	// discarded (AGO-DEVIATION: Python cancels the in-flight hook via
	// asyncio.wait_for; see docs/DECISIONS.md D-032).
	answer := make(chan *events.HumanMessage, 1)
	hookErr := make(chan error, 1)
	sub := c.Stream.Subscribe(func(ev events.Event) error {
		hr, ok := humanRequest(ev)
		if !ok || hr.ID != req.ID {
			return nil
		}
		go func() {
			msg, err := hook(ctx, hr)
			switch {
			case err != nil:
				hookErr <- err
			case msg == nil:
				hookErr <- errors.New("hitl: hook returned no message")
			default:
				if msg.ParentID == "" {
					msg.ParentID = req.ID
				}
				if err := c.Send(msg); err != nil {
					hookErr <- err
					return
				}
				answer <- msg
			}
		}()
		// The question is swallowed: not transcribed, not shown to normal
		// subscribers (Python parity: the interrupt wrapper returns None).
		return stream.ErrHandled
	}, stream.SubscribeOptions{
		Interrupt: true,
		Condition: events.TypeOfAny(
			reflect.TypeOf(events.HumanInputRequest{}),
			reflect.TypeOf(events.ToolApprovalRequest{}),
		),
	})
	defer c.Stream.Unsubscribe(sub)

	if err := c.Send(req); err != nil {
		// ErrHandled is consumed inside Send; anything else is the channel
		// failing before the hook could run (Python parity: a send failure is
		// the channel failing → HumanInputFailedError).
		return "", &hitl.FailedError{Cause: err}
	}
	for {
		select {
		case msg := <-got:
			return msg.Content, nil
		case msg := <-answer:
			return msg.Content, nil
		case err := <-hookErr:
			if errors.Is(err, hitl.ErrNotProvided) {
				return "", &hitl.NotProvidedError{Detail: err.Error()}
			}
			// A hook surfacing the Ask-imposed cancellation is the deadline
			// firing, not the channel failing (Python parity: wait_for cancels
			// the in-flight hook and reports the timeout). Any other hook
			// failure stays a failure even at the deadline.
			if errors.Is(err, stdcontext.DeadlineExceeded) && errors.Is(ctx.Err(), stdcontext.DeadlineExceeded) {
				return "", &hitl.TimeoutError{Timeout: req.Timeout}
			}
			return "", &hitl.FailedError{Cause: err}
		case err := <-getErr:
			// Get only errors on cancellation of its context.
			if errors.Is(err, stdcontext.DeadlineExceeded) {
				return "", &hitl.TimeoutError{Timeout: req.Timeout}
			}
			return "", &hitl.FailedError{Cause: err}
		case <-ctx.Done():
			if errors.Is(ctx.Err(), stdcontext.DeadlineExceeded) {
				return "", &hitl.TimeoutError{Timeout: req.Timeout}
			}
			return "", &hitl.FailedError{Cause: ctx.Err()}
		}
	}
}

// sanitizeTimeout normalizes a request timeout that may come from untrusted
// data: NaN, infinities and negatives map to 0 (no deadline), because a
// float-to-Duration conversion of an infinity is undefined and a negative
// deadline is indistinguishable from an already-expired one.
func sanitizeTimeout(t float64) float64 {
	if math.IsNaN(t) || math.IsInf(t, 0) || t < 0 {
		return 0
	}
	return t
}

// humanRequest extracts the request behind an event: ToolApprovalRequest embeds
// HumanInputRequest, and Go type identity keeps the embedded value from matching
// a direct assertion.
func humanRequest(ev events.Event) (*events.HumanInputRequest, bool) {
	switch t := ev.(type) {
	case *events.HumanInputRequest:
		return t, true
	case *events.ToolApprovalRequest:
		return &t.HumanInputRequest, true
	}
	return nil, false
}

// answerCondition matches the HumanMessage answering a request with the given id.
// AsValue tolerates both event forms: a value-form HumanMessage (e.g. after a
// marshal round-trip) would panic a direct pointer assertion.
func answerCondition(parentID string) events.Condition {
	return events.AllOf(
		events.TypeOf[events.HumanMessage](),
		events.Equal(func(ev events.Event) string {
			if hm, ok := events.AsValue(ev).(events.HumanMessage); ok {
				return hm.ParentID
			}
			return ""
		}, parentID),
	)
}

// Input asks the human a plain-text question.
func (c *Context) Input(ctx stdcontext.Context, message string, timeout time.Duration) (string, error) {
	req := events.NewHumanInputRequest(message, timeout.Seconds())
	return c.Ask(ctx, &req)
}
