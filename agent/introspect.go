package agent

import (
	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/observers"
	"github.com/luckystrker/ago/response"
)

// Read-only introspection views of what an agent is made of (Python parity:
// Agent.system_prompt / dynamic_prompt / middleware / dependencies / variables /
// has_hitl_hook / response_schema / observers / tasks). Each returns a copy, so
// a caller cannot mutate the agent through it. They are for tests, logging and
// comparing two agents.
//
// AGO-DEVIATION: Python also exposes `serializer` and `assembly`. AGo has no
// serializer abstraction, and the deprecated `assembly=` policies are not
// carried by the agent (they are ordinary middleware), so neither has a view
// here.

// Config returns the agent's model config (nil when none is set).
func (a *Agent) Config() config.ModelConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.opts.cfg
}

// DynamicPrompt returns the agent's prompt hooks in registration order.
func (a *Agent) DynamicPrompt() []PromptHook {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]PromptHook, len(a.opts.promptHooks))
	copy(out, a.opts.promptHooks)
	return out
}

// Middleware returns the agent's middleware factories in registration order
// (outermost first).
func (a *Agent) Middleware() []middleware.Factory {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]middleware.Factory, len(a.opts.middleware))
	copy(out, a.opts.middleware)
	return out
}

// Dependencies returns a copy of the dependencies the agent injects into tool
// calls.
func (a *Agent) Dependencies() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]any, len(a.opts.dependencies))
	for k, v := range a.opts.dependencies {
		out[k] = v
	}
	return out
}

// Variables returns a copy of the variable defaults the agent seeds a
// conversation with. ScopedValue declarations are returned as declared.
func (a *Agent) Variables() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]any, len(a.opts.variables))
	for k, v := range a.opts.variables {
		out[k] = v
	}
	return out
}

// HasHitlHook reports whether the agent answers human-input requests with a hook
// of its own (as opposed to the caller's or none).
func (a *Agent) HasHitlHook() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.opts.hitl != nil
}

// ResponseSchema returns the agent's response schema, or nil for free text.
func (a *Agent) ResponseSchema() response.Proto {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.opts.schema
}

// Observers returns the observers attached at construction, in order.
func (a *Agent) Observers() []observers.Observer {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]observers.Observer, len(a.opts.observers))
	copy(out, a.opts.observers)
	return out
}

// Tasks returns the sub-task configuration, or nil when delegation tools are
// not enabled. Deprecated with TaskConfig; removed in 1.4.0.
func (a *Agent) Tasks() *TaskConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.opts.tasks == nil {
		return nil
	}
	tc := *a.opts.tasks
	return &tc
}
