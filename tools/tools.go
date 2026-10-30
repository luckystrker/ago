// Package tools carries the tool abstraction: what a tool contributes to the model
// request (schemas) and how it receives calls (a stream subscription). Tools are
// stream subscribers; everything a tool execution does is expressed as events.
//
// Parity with Python AG2 ag2/tools/ (tool.py, schemas.py, final/function_tool.py,
// final/toolkit.py, final/client_tool.py, executor.py, precedence.py, annotations.py).
// The ExitStack-based registration lifetime is replaced by per-turn registration on
// the turn's stream (see docs/DECISIONS.md). Parameter schemas are generated through
// response.SchemaOf over the tool function's argument struct.
package tools

import (
	"errors"
	"fmt"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
)

// Schema is the function-tool schema handed to providers (Python parity:
// FunctionToolSchema, flattened — AGO-DEVIATION: Python nests it under
// `function`; the AGo contract carries the fields at the top level).
type Schema struct {
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	Parameters   map[string]any `json:"parameters"` // JSON schema object; never carries a "title"
	DeferLoading bool           `json:"defer_loading,omitempty"`
}

// SchemaType identifies the schema kind on the wire ("function" for Schema).
func (s Schema) SchemaType() string { return "function" }

// ToolSchema is a provider-neutral schema for a builtin or hosted tool: its
// SchemaType is the provider-side tool type (e.g. "web_search"). Hosted tools are
// executed at the provider; the framework only carries their schemas.
type ToolSchema interface {
	SchemaType() string
}

// Tool is one callable capability: schemas() declares what the model may call,
// register() subscribes the tool on the conversation stream for calls matching its
// name. Both are invoked per turn by the agent's turn scope.
type Tool interface {
	Name() string
	// DeclaredInCode reports whether the tool comes from the program (true) or from
	// a runtime peer such as an MCP server or the client application (false).
	// Code-declared tools take precedence over runtime-provided ones.
	DeclaredInCode() bool
	// Schemas returns the schemas this tool contributes to the model request:
	// Schema values for callable functions, ToolSchema impls for hosted tools.
	// Toolkit implementations use the call as their member-discovery hook.
	Schemas(ctx *agocontext.Context) ([]any, error)
	// Register subscribes the tool on ctx.Stream for its calls. mw carries the
	// agent's middleware instances for this turn (OnToolExecution layers around the
	// tool's own execution); implementations must treat it as read-only. The
	// returned ids are the stream subscriptions created; the turn scope owns them
	// and must unsubscribe before the next turn registers on the same stream
	// (AGO-DEVIATION: no ExitStack — Go cleanup is agent-owned, see
	// docs/DECISIONS.md D-032). On error nothing may remain subscribed.
	Register(ctx *agocontext.Context, mw []middleware.Middleware) ([]stream.SubID, error)
}

// Errors reported by the package. Match with errors.Is.
var (
	// ErrConflict reports a name collision: a toolkit asked to register two tools
	// with the same name (Python ToolConflictError).
	ErrConflict = errors.New("tools: conflict")
	// ErrNotFound reports a missing entry: an unknown dependency or variable
	// requested by a tool parameter, or a tool name the executor cannot resolve.
	ErrNotFound = errors.New("tools: not found")
	// ErrResolution reports a turn-level tool resolution failure: schemas that
	// cannot be read or understood, or an AgentSpec tool missing from the pool.
	ErrResolution = errors.New("tools: resolution failed")
)

// notFoundError builds the tool-not-found error text (Python ToolNotFoundError).
func notFoundError(name string) error {
	return fmt.Errorf("Tool `%s` not found: %w", name, ErrNotFound)
}
