package tools

import (
	"fmt"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
)

// ClientTool is a tool a remote client declares on the wire and executes itself
// (Python parity: ag2/tools/final/client_tool.py). It is never declared in code; a
// call is answered with a ClientToolCallEvent the executor forwards to the client.
type ClientTool struct {
	schema Schema
}

// NewClientTool builds a client tool from its wire schema — a flat map with the
// "name" (required), "description", "parameters" and "defer_loading" keys (Python
// parity: FunctionToolSchema.from_dict, flattened to the AGo Schema shape; a
// top-level "title" is stripped).
func NewClientTool(schema map[string]any) (*ClientTool, error) {
	name, _ := schema["name"].(string)
	if name == "" {
		return nil, fmt.Errorf("%w: client tool schema requires a name", ErrResolution)
	}
	description, _ := schema["description"].(string)
	params, _ := schema["parameters"].(map[string]any)
	deferLoading, _ := schema["defer_loading"].(bool)
	params = deepCopySchema(params)
	stripTitle(params)
	return &ClientTool{schema: Schema{
		Name:         name,
		Description:  description,
		Parameters:   params,
		DeferLoading: deferLoading,
	}}, nil
}

func (t *ClientTool) Name() string         { return t.schema.Name }
func (t *ClientTool) DeclaredInCode() bool { return false }
func (t *ClientTool) Schema() Schema       { return t.schema }
func (t *ClientTool) Schemas(_ *agocontext.Context) ([]any, error) {
	return []any{t.schema}, nil
}

// Register subscribes the tool for calls matching its name; the agent middleware
// wraps the conversion to ClientToolCallEvent (no tool middleware, no timeout: the
// tool runs on the client). The subscription id is returned for the turn scope's
// teardown.
func (t *ClientTool) Register(cx *agocontext.Context, mw []middleware.Middleware) ([]stream.SubID, error) {
	exec := middleware.ToolExecFunc(func(ev *events.ToolCallEvent, _ *agocontext.Context) (events.Event, error) {
		return events.ClientToolCallEvent{ID: ev.ID, Name: ev.Name, Arguments: ev.Arguments}, nil
	})
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
	}, t.schema.Name)
	handler := func(ev events.Event) error {
		call, ok := events.AsValue(ev).(events.ToolCallEvent)
		if !ok {
			return nil
		}
		c := call
		result, err := exec(&c, cx.WithToolCallID(call.ID))
		if err != nil {
			return err
		}
		return cx.Send(result)
	}
	id := cx.Stream.Subscribe(handler, stream.SubscribeOptions{Condition: cond})
	return []stream.SubID{id}, nil
}
