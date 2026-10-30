package mcp

import (
	"fmt"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
)

// Tools discovers the server's tools and wraps each one as a tools.Tool
// (parity with Python MCPToolkit's lazy discovery, made explicit: the caller
// lists once, the agent resolves the returned tools per turn). Every wrapper
// reports DeclaredInCode()==false, so code-declared tools keep name precedence.
//
// The config's filters apply to the server-side names (empty AllowedTools means
// all; BlockedTools is applied after), and ToolNamePrefix namespaces the
// local names. Neither reaches the server: calls go out under the original name.
func Tools(cl *Client) ([]tools.Tool, error) {
	if cl == nil {
		return nil, fmt.Errorf("%w: nil client", ErrUnsupportedConfig)
	}
	remote, err := cl.ListTools()
	if err != nil {
		return nil, err
	}
	allowed := cl.allowedTools()
	blocked := cl.blockedTools()
	prefix := cl.toolNamePrefix()
	out := make([]tools.Tool, 0, len(remote))
	seen := map[string]bool{}
	for _, r := range remote {
		if allowed != nil && !allowed[r.Name] {
			continue
		}
		if blocked[r.Name] {
			continue
		}
		t := buildRemoteTool(cl, r, prefix)
		if seen[t.name] {
			// A server listing the same name twice keeps its first entry
			// (Python parity: setdefault).
			continue
		}
		seen[t.name] = true
		out = append(out, t)
	}
	return out, nil
}

// buildRemoteTool wraps one listed tool (also exercised directly by tests).
func buildRemoteTool(cl *Client, r RemoteTool, prefix string) *remoteTool {
	name := prefix + r.Name
	if r.InputSchema == nil {
		// Providers require an object schema; a server that omitted one gets the
		// no-argument object (parity: parameters = dict(raw.input_schema or {})).
		r.InputSchema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return &remoteTool{
		cl:          cl,
		remoteName:  r.Name,
		name:        name,
		description: r.Description,
		schema: tools.Schema{
			Name:        name,
			Description: r.Description,
			Parameters:  r.InputSchema,
		},
		source: fmt.Sprintf("mcp:%s:%s", cl.endpoint(), r.Name),
	}
}

// remoteTool forwards calls to the MCP server (Python parity: _MCPProxyTool).
// Instances are immutable after construction; the Client serializes exchanges,
// so one wrapper is safe for concurrent use.
type remoteTool struct {
	cl          *Client
	remoteName  string // the server-side name; the prefix never reaches the server
	name        string // the prefixed local name the model sees
	description string
	schema      tools.Schema
	source      string
}

func (t *remoteTool) Name() string         { return t.name }
func (t *remoteTool) DeclaredInCode() bool { return false }
func (t *remoteTool) Source() string       { return t.source }

func (t *remoteTool) Schemas(_ *agocontext.Context) ([]any, error) {
	return []any{t.schema}, nil
}

// Register subscribes the tool on the conversation stream for calls matching its
// local (prefixed) name. The agent middleware (mws[0] outermost — the repo-wide
// Chain convention) wraps the remote invocation; the result event is sent back
// for the executor to collect.
func (t *remoteTool) Register(cx *agocontext.Context, mw []middleware.Middleware) ([]stream.SubID, error) {
	exec := t.exec
	for i := len(mw) - 1; i >= 0; i-- {
		m, inner := mw[i], exec
		exec = func(ev *events.ToolCallEvent, cx *agocontext.Context) (events.Event, error) {
			return m.OnToolExecution(inner, ev, cx)
		}
	}
	cond := events.Equal(func(ev events.Event) string {
		if call, ok := events.AsValue(ev).(events.ToolCallEvent); ok {
			return call.Name
		}
		return ""
	}, t.name)
	handler := func(ev events.Event) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("tool %s middleware panicked: %v", t.name, r)
			}
		}()
		call, ok := events.AsValue(ev).(events.ToolCallEvent)
		if !ok {
			return nil
		}
		// Python parity: the chain sees the call stamped with the server identity.
		stamped := call.HandledBy(t.source)
		result, err := exec(&stamped, cx.WithToolCallID(call.ID))
		if err != nil {
			// A failed human-input hand-off is not a tool result; it travels
			// back to the executor through the send error.
			return err
		}
		return cx.Send(result)
	}
	id := cx.Stream.Subscribe(handler, stream.SubscribeOptions{Condition: cond})
	return []stream.SubID{id}, nil
}

// exec invokes the server: the call's JSON arguments go out under the remote
// name; a failure becomes a ToolErrorEvent, a success a ToolResultEvent whose
// parts are the mapped content blocks.
func (t *remoteTool) exec(ev *events.ToolCallEvent, _ *agocontext.Context) (events.Event, error) {
	args, err := ev.SerializedArguments()
	if err != nil {
		return events.FromErrorCall(*ev, err), nil
	}
	outcome, err := t.cl.CallTool(t.remoteName, args)
	if err != nil {
		return events.FromErrorCall(*ev, err), nil
	}
	values := make([]any, len(outcome.Parts))
	for i, p := range outcome.Parts {
		values[i] = p
	}
	return events.FromResultCall(*ev, events.NewToolResult(false, values...)), nil
}
