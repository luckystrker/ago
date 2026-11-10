package agui

import (
	"log/slog"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/tools"
)

// clientToolsForRun builds the tools the client declared for one run, ready for
// agent.RunOptions.Tools. A tool the agent already declares under the same name
// is dropped with a warning (the agent's own wins; a later declared tool would
// otherwise override it under tools.ResolveTools precedence), and a malformed
// declaration is skipped the same way. The returned tools live only for that
// run: the run scope registers and tears them down, so concurrent turns cannot
// see each other's and nothing survives a failure while the history is seeded.
func clientToolsForRun(a *agent.Agent, declared []Tool) []tools.Tool {
	if len(declared) == 0 {
		return nil
	}
	own := ownToolNames(a)
	var built []tools.Tool
	for _, d := range declared {
		if own[d.Name] {
			slog.Warn("agui: ignoring a client tool: the agent declares a tool with that name", "tool", d.Name)
			continue
		}
		schema := map[string]any{"name": d.Name}
		if d.Description != "" {
			schema["description"] = d.Description
		}
		if d.Parameters != nil {
			schema["parameters"] = d.Parameters
		}
		t, err := tools.NewClientTool(schema)
		if err != nil {
			slog.Warn("agui: ignoring a client tool that cannot be declared", "err", err)
			continue
		}
		built = append(built, t)
	}
	return built
}

// ownToolNames is the names the agent's own tools answer to, read without a
// turn context: a toolkit's members count, and a tool whose name is only known
// per turn does not (a clash with one is left to the turn's precedence rules).
func ownToolNames(a *agent.Agent) map[string]bool {
	names := map[string]bool{}
	var walk func(t tools.Tool)
	walk = func(t tools.Tool) {
		if tk, ok := t.(*tools.Toolkit); ok {
			for _, m := range tk.Tools() {
				walk(m)
			}
			return
		}
		names[t.Name()] = true
	}
	for _, t := range a.Tools() {
		walk(t)
	}
	return names
}
