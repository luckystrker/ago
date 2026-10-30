// Package plugin bundles an agent's cross-cutting contributions — tools,
// middleware and system-prompt material — behind one named value an agent
// applies at construction (Python parity: ag2/plugin.py Plugin collected via
// PromptObserverMixin; PluginTarget._apply_plugin copied the contributions
// onto the agent).
//
// A plugin carries exactly what the agent already accepts through its own
// options, so WithPlugin expands into them; it introduces no new runtime
// behavior. Contributions Python keeps on a plugin but the AGo surface has no
// slot for — HITL hook, dependencies, variables, observers, assembly policies —
// are not carried (see docs/DECISIONS.md D-044).
package plugin

import (
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/tools"
)

// PromptFragment produces dynamic system-prompt fragments for a turn
// (Python parity: the Plugin.prompt() decorator on async hooks). It is
// structurally identical to agent.PromptHook; the separate type keeps plugin
// free of any import on the agent package.
type PromptFragment func(ctx *agocontext.Context) ([]string, error)

// Plugin is a named set of agent contributions. A zero Plugin (all accessors
// empty, or a nil *Bundle) contributes nothing and is harmless to apply.
// Implementations returned by Bundle hand out copies, so a caller cannot
// reach the builder's internal slices.
type Plugin interface {
	Name() string
	// Tools are appended to the agent's tools (Python parity: _apply_plugin's
	// add_tool loop).
	Tools() []tools.Tool
	// Middleware factories are appended after the agent's own, which places
	// them innermost in the turn onion (Python parity: _apply_plugin routes
	// through add_middleware, whose docstring pins appended = innermost).
	Middleware() []middleware.Factory
	// PromptFragments are the static system-prompt fragments (Python parity:
	// _system_prompt).
	PromptFragments() []string
	// PromptHooks are the dynamic per-turn fragment producers (Python parity:
	// _dynamic_prompt).
	PromptHooks() []PromptFragment
}

// Bundle assembles a Plugin fluently. It is a builder: not safe for
// concurrent use, and a Plugin handed out stays a live view of the bundle —
// apply it to an agent before adding more (the agent copies everything at
// construction, so later additions cannot reach an already-built agent).
type Bundle struct {
	name       string
	tools      []tools.Tool
	middleware []middleware.Factory
	fragments  []string
	hooks      []PromptFragment
}

var _ Plugin = (*Bundle)(nil)

// NewBundle starts a bundle with the given name.
func NewBundle(name string) *Bundle { return &Bundle{name: name} }

// WithTool adds a tool. The bundle is returned for chaining.
func (b *Bundle) WithTool(t tools.Tool) *Bundle {
	if b == nil {
		return b
	}
	b.tools = append(b.tools, t)
	return b
}

// WithMiddleware adds a middleware factory. Appended factories run innermost
// (agent.WithMiddleware semantics).
func (b *Bundle) WithMiddleware(f middleware.Factory) *Bundle {
	if b == nil {
		return b
	}
	b.middleware = append(b.middleware, f)
	return b
}

// WithPrompt adds a static system-prompt fragment.
func (b *Bundle) WithPrompt(fragment string) *Bundle {
	if b == nil {
		return b
	}
	b.fragments = append(b.fragments, fragment)
	return b
}

// WithPromptHook adds a dynamic system-prompt fragment producer. Hooks run
// exactly like agent.WithPromptHook hooks (the agent seeds the system prompt
// from static fragments plus hook output; a continuation whose context
// already carries a prompt does not re-run them).
func (b *Bundle) WithPromptHook(h PromptFragment) *Bundle {
	if b == nil {
		return b
	}
	b.hooks = append(b.hooks, h)
	return b
}

// Plugin returns the bundle as a Plugin. The same bundle is returned on every
// call — the Plugin view is the bundle itself.
func (b *Bundle) Plugin() Plugin { return b }

func (b *Bundle) Name() string {
	if b == nil {
		return ""
	}
	return b.name
}

func (b *Bundle) Tools() []tools.Tool {
	if b == nil {
		return nil
	}
	out := make([]tools.Tool, len(b.tools))
	copy(out, b.tools)
	return out
}

func (b *Bundle) Middleware() []middleware.Factory {
	if b == nil {
		return nil
	}
	out := make([]middleware.Factory, len(b.middleware))
	copy(out, b.middleware)
	return out
}

func (b *Bundle) PromptFragments() []string {
	if b == nil {
		return nil
	}
	out := make([]string, len(b.fragments))
	copy(out, b.fragments)
	return out
}

func (b *Bundle) PromptHooks() []PromptFragment {
	if b == nil {
		return nil
	}
	out := make([]PromptFragment, len(b.hooks))
	copy(out, b.hooks)
	return out
}
