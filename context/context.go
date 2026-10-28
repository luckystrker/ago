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
	"sync"
	"time"

	"ago/events"
	"ago/hitl"
	"ago/stream"
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

	mu   sync.RWMutex
	hook hitl.HumanHook
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

// Ask requests human input and waits for the answer. Errors are normalized to
// hitl errors: no hook → NotProvidedError, hook failure → FailedError,
// deadline → TimeoutError.
func (c *Context) Ask(ctx stdcontext.Context, req *events.HumanInputRequest) (string, error) {
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
		var cancel stdcontext.CancelFunc
		ctx, cancel = stdcontext.WithTimeout(ctx, time.Duration(req.Timeout*float64(time.Second)))
		defer cancel()
	}
	msg, err := hook(ctx, req)
	if err != nil {
		if errors.Is(err, hitl.ErrNotProvided) {
			return "", &hitl.NotProvidedError{Detail: err.Error()}
		}
		if errors.Is(ctx.Err(), stdcontext.DeadlineExceeded) {
			return "", &hitl.TimeoutError{Timeout: req.Timeout}
		}
		return "", &hitl.FailedError{Cause: err}
	}
	if msg == nil {
		return "", &hitl.FailedError{Cause: errors.New("hook returned no message")}
	}
	if msg.ParentID == "" {
		msg.ParentID = req.ID
	}
	if err := c.Send(msg); err != nil {
		return "", &hitl.FailedError{Cause: err}
	}
	return msg.Content, nil
}

// Input asks the human a plain-text question.
func (c *Context) Input(ctx stdcontext.Context, message string, timeout time.Duration) (string, error) {
	req := events.NewHumanInputRequest(message, timeout.Seconds())
	return c.Ask(ctx, &req)
}
