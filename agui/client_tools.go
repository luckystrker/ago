package agui

import (
	"log/slog"
	"sync"

	"github.com/luckystrker/ago/agent"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
)

// clientToolsName is the tool-set's name in precedence diagnostics. It is never
// a callable name: the set contributes the client's own tool schemas.
const clientToolsName = "ag_ui_client_tools"

// ClientTools returns the agent option that lets a served turn expose the tools
// the AG-UI client declares in RunAgentInput.tools: the model sees them, and a
// call it makes is handed back to the client (events.ClientToolCallEvent, which
// the wire carries as TOOL_CALL_START/ARGS/END) and ends the turn; the client
// runs the tool and answers in a follow-up run whose history carries the
// result (Python parity: stream.py _serve_turn's per-turn ClientTool list).
//
// Server.Options and Server.ServeAgent already include it. An agent served
// through ServeAgent or Server.Handler without it ignores the client's tools,
// with a warning.
//
// AGO-DEVIATION: Python extends the tools of one ask() call. AGo's agent takes
// its tools at construction (docs/DECISIONS.md D-037), so the declared tools
// reach it through one tool-set that reads them, by the turn's stream, from
// what serveTurn declared before starting the run. The set is one precedence
// unit, so a client tool named like a tool the agent already has is dropped
// before the turn (with a warning, as Python's precedence does) rather than
// dropping the whole set.
func ClientTools() agent.Option { return agent.WithTools(clientToolSet{}) }

// declaredClientTools is what each in-flight turn's client declared, by stream id.
var declaredClientTools sync.Map // stream id -> []tools.Tool (each a *tools.ClientTool)

// clientToolSet is the tools.Tool carrying the client's declared tools for the
// turn it is resolved in. It holds no state: the turn's tools are looked up by
// the stream the turn runs on, so one value serves every concurrent turn.
type clientToolSet struct{}

var _ tools.Tool = clientToolSet{}

func (clientToolSet) Name() string { return clientToolsName }

// DeclaredInCode is false: the tools come from the client application, so a
// tool the agent declares itself wins a name clash.
func (clientToolSet) DeclaredInCode() bool { return false }

func (clientToolSet) turnTools(cx *agocontext.Context) []tools.Tool {
	if cx == nil || cx.Stream == nil {
		return nil
	}
	v, ok := declaredClientTools.Load(cx.Stream.ID())
	if !ok {
		return nil
	}
	return v.([]tools.Tool)
}

func (s clientToolSet) Schemas(cx *agocontext.Context) ([]any, error) {
	schemas := []any{}
	for _, t := range s.turnTools(cx) {
		ss, err := t.Schemas(cx)
		if err != nil {
			return nil, err
		}
		schemas = append(schemas, ss...)
	}
	return schemas, nil
}

// Register subscribes every declared tool; on failure nothing stays subscribed.
func (s clientToolSet) Register(cx *agocontext.Context, mw []middleware.Middleware) ([]stream.SubID, error) {
	var subs []stream.SubID
	for _, t := range s.turnTools(cx) {
		ids, err := t.Register(cx, mw)
		if err != nil {
			for _, id := range subs {
				cx.Stream.Unsubscribe(id)
			}
			return nil, err
		}
		subs = append(subs, ids...)
	}
	return subs, nil
}

// declareClientTools makes the run input's tools the ones the turn running on
// streamID resolves, and returns the function that withdraws them. Tools that
// cannot be built, or that the agent already has under the same name, are
// skipped with a warning; a client declaring nothing leaves the turn as it was.
func declareClientTools(a *agent.Agent, streamID string, declared []Tool) (release func()) {
	if len(declared) == 0 {
		return func() {}
	}
	if !carriesClientTools(a) {
		slog.Warn("agui: ignoring the tools the client declared: the served agent was built without agui.ClientTools()")
		return func() {}
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
	if len(built) == 0 {
		return func() {}
	}
	declaredClientTools.Store(streamID, built)
	return func() { declaredClientTools.Delete(streamID) }
}

// carriesClientTools reports whether the agent was built with ClientTools().
func carriesClientTools(a *agent.Agent) bool {
	for _, t := range a.Tools() {
		if _, ok := t.(clientToolSet); ok {
			return true
		}
	}
	return false
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
		if _, ok := t.(clientToolSet); ok {
			return
		}
		names[t.Name()] = true
	}
	for _, t := range a.Tools() {
		walk(t)
	}
	return names
}
