package agent

import (
	"fmt"

	"github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/observers"
	"github.com/luckystrker/ago/tools"
)

// applyRunOverrides writes the context-level per-invocation overrides of one
// turn: dependencies, variables and the prompt (Python parity:
// _ContextOverrides.apply, run under the stream turn lock). The config, tools,
// middleware, observers, plugins, schema and hook overrides are consumed where
// their scope is built instead (see AgentRun.open).
//
// The context is private to this run — a fresh one or a clone — and the turn
// goroutine has not started, so replacing the prompt needs no lock on the
// context (it is not shared yet).
func (r *AgentRun) applyRunOverrides() error {
	o := r.runOpts
	for k, v := range o.Dependencies {
		r.cx.SetDependency(k, v)
	}
	if len(o.Variables) > 0 {
		// OverlayVariables keeps scope declarations intact and applies the
		// values in place (parity: scopes.overlay_variables).
		if _, err := context.OverlayVariables(r.cx.VariableStore(), o.Variables); err != nil {
			return fmt.Errorf("agent: apply invocation variables: %w", err)
		}
	}
	if len(o.Prompt) > 0 {
		r.cx.Prompt = append([]string(nil), o.Prompt...)
	}
	return nil
}

// turnPluginContributions is the flattened contribution of the plugins attached
// to one invocation (Python parity: _TurnPlugins).
type turnPluginContributions struct {
	tools      []tools.Tool
	middleware []middleware.Factory
	fragments  []string
	hooks      []PromptHook
	observers  []observers.Observer
}

// contributions flattens the invocation plugins in attachment order.
func (o RunOptions) contributions() turnPluginContributions {
	var out turnPluginContributions
	for _, p := range o.Plugins {
		if p == nil {
			continue
		}
		out.tools = append(out.tools, p.Tools()...)
		out.middleware = append(out.middleware, p.Middleware()...)
		out.fragments = append(out.fragments, p.PromptFragments()...)
		for _, h := range p.PromptHooks() {
			out.hooks = append(out.hooks, PromptHook(h))
		}
	}
	return out
}
