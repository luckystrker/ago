package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
)

// ErrInvalidToolMeta reports a ServerConfig.ToolMeta entry that names no served
// tool or declares an output schema that is not an object schema.
var ErrInvalidToolMeta = errors.New("mcpserver: invalid tool metadata")

// RequestDependency is the dependency id under which every tool call (and every
// ToolMeta.Resolve) finds the *RequestContext of the MCP request being served:
// a tools.Inject slot with this id receives it (Python parity: the
// MCPRequestContext parameter, resolved from the request-scoped dependency map).
const RequestDependency = "mcp_request"

// RequestContext is what one served MCP request carries to the tools it runs.
type RequestContext struct {
	// Method is the JSON-RPC method being served ("tools/call", "tools/list").
	Method string
	// Tool is the called tool's name; empty for tools/list.
	Tool string
	// HTTP is the HTTP request the message arrived on; nil over stdio.
	HTTP *http.Request
}

// ToolAnnotations are the behavior hints a host may use to present a tool
// (Python parity: mcp.types.ToolAnnotations). Unset hints are omitted.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// ToolMeta is the presentation metadata one served tool advertises in
// tools/list beyond its name, description and input schema (Python parity:
// mcp_tool(title=, annotations=, output_schema=, meta=)). The zero value
// advertises nothing extra.
//
// AGO-DEVIATION: Python carries this metadata on the tool itself (mcp_tool
// arguments). AGo serves tools.Tool values, whose Schema has no title,
// annotations or output schema, so the metadata lives in ServerConfig.ToolMeta
// keyed by tool name. See docs/DECISIONS.md.
type ToolMeta struct {
	// Title is the human-readable display name.
	Title string
	// Annotations are the behavior hints.
	Annotations *ToolAnnotations
	// OutputSchema describes the tool's structuredContent and must be an
	// object schema (type "object"). A call whose result is one JSON object
	// answers it as structuredContent next to the usual text content.
	OutputSchema map[string]any
	// Meta travels as the tool's "_meta" extension slot.
	Meta map[string]any
	// Resolve, when set, is called for each tools/list and tools/call request
	// with that request's context (variables, dependencies, RequestDependency,
	// the request's std context through TurnContext). Its non-zero fields
	// replace the static ones, which is how a title or hint follows the
	// request (Python parity: a Variable inside title/annotations/meta).
	Resolve func(cx *agocontext.Context) ToolMeta
}

// merged is m with the non-zero fields of the per-request resolution laid over.
func (m ToolMeta) merged(cx *agocontext.Context) ToolMeta {
	if m.Resolve == nil {
		return m
	}
	r := m.Resolve(cx)
	if r.Title != "" {
		m.Title = r.Title
	}
	if r.Annotations != nil {
		m.Annotations = r.Annotations
	}
	if r.OutputSchema != nil {
		m.OutputSchema = r.OutputSchema
	}
	if r.Meta != nil {
		m.Meta = r.Meta
	}
	return m
}

// WithToolMeta sets the metadata advertised for the served tool called name
// (the conversational tool included); see ServerConfig.ToolMeta.
func WithToolMeta(name string, meta ToolMeta) ServerOption {
	return func(c *ServerConfig) {
		if c.ToolMeta == nil {
			c.ToolMeta = map[string]ToolMeta{}
		}
		c.ToolMeta[name] = meta
	}
}

// RequestScope is what a ContextProvider returns for one request: the values
// that go into the agent's ask through agent.RunOptions (Python parity:
// mcp.executor.AskContext). A nil field keeps the ask default; non-nil
// Variables and Dependencies are merged over ServerConfig's maps with the
// provider's value winning.
type RequestScope struct {
	Variables    map[string]any
	Dependencies map[string]any
	Tools        []tools.Tool
	Prompt       []string
}

// ContextProvider resolves the per-request scope, with the request in hand
// (Python parity: AgentExecutor's context_provider, which receives the
// request's AccessToken or nil, called through server._request_context on every
// request). It runs once for tools/list and once for every tools/call —
// deterministic WithTools tools included, not only the conversational ask. The
// scope feeds the agent's RunOptions, the deterministic tools' Variable/Inject
// slots, and ToolMeta.Resolve; request is the same *RequestContext those see,
// including the HTTP request.
//
// AGO-DEVIATION: the Go server has no OAuth access-token store (OAuth routes
// are deferred with D-058), so token is always ""; the HTTP Authorization
// header remains reachable through request.HTTP. An error refuses the request
// as a JSON-RPC error without running anything.
type ContextProvider func(ctx context.Context, token string, request *RequestContext) (RequestScope, error)

// WithContextProvider sets the per-request context provider; see
// ServerConfig.ContextProvider.
func WithContextProvider(fn ContextProvider) ServerOption {
	return func(c *ServerConfig) { c.ContextProvider = fn }
}

// WithVariables sets the variables each request's tools and ToolMeta.Resolve
// read. The map is snapshotted when the server is constructed; later writes to
// the caller's map do not reach requests.
func WithVariables(vars map[string]any) ServerOption {
	return func(c *ServerConfig) { c.Variables = vars }
}

// WithDependencies sets the dependencies each request's tools resolve
// tools.Inject slots against. The map is snapshotted when the server is
// constructed; later writes to the caller's map do not reach requests.
func WithDependencies(deps map[string]any) ServerOption {
	return func(c *ServerConfig) { c.Dependencies = deps }
}

// validateToolMeta checks every configured entry against the served tool names.
func validateToolMeta(metas map[string]ToolMeta, served map[string]bool) error {
	for name, m := range metas {
		if !served[name] {
			return fmt.Errorf("%w: %q names no served tool", ErrInvalidToolMeta, name)
		}
		if m.OutputSchema != nil {
			if t, _ := m.OutputSchema["type"].(string); t != "object" {
				return fmt.Errorf("%w: outputSchema of %q must be an object schema (type \"object\")", ErrInvalidToolMeta, name)
			}
		}
	}
	return nil
}

// resolveRequestScope builds the per-request context and asks the host's
// provider for the scope (Python parity: server._request_context, called on
// every request — tools/list and every tools/call, deterministic tools
// included). A provider error refuses the request before anything runs.
func (s *Server) resolveRequestScope(ctx context.Context, method, tool string) (*RequestContext, *RequestScope, error) {
	req := &RequestContext{Method: method, Tool: tool, HTTP: RequestFromContext(ctx)}
	if s.contextProvider == nil {
		return req, nil, nil
	}
	// AGO-DEVIATION: no OAuth access-token store exists yet (D-058); the
	// provider still receives the request context.
	scope, err := s.contextProvider(ctx, "", req)
	if err != nil {
		return req, nil, err
	}
	return req, &scope, nil
}

// requestContextFor builds the conversation context one request's tools run
// against: a copy of the server's variables and dependencies with the
// provider's scope laid over, the RequestContext under RequestDependency, and
// the request's std context as the turn context (so
// RequestFromContext(cx.TurnContext()) reaches the HTTP request). Each request
// gets its own stream; nothing is shared between them.
func (s *Server) requestContextFor(ctx context.Context, req *RequestContext, scope *RequestScope) (*agocontext.Context, error) {
	deps := make(map[string]any, len(s.dependencies)+1)
	for k, v := range s.dependencies {
		deps[k] = v
	}
	vars := make(map[string]any, len(s.variables))
	for k, v := range s.variables {
		vars[k] = v
	}
	if scope != nil {
		for k, v := range scope.Variables {
			vars[k] = v
		}
		for k, v := range scope.Dependencies {
			deps[k] = v
		}
	}
	if req == nil {
		req = &RequestContext{Method: "tools/call", Tool: "", HTTP: RequestFromContext(ctx)}
	}
	deps[RequestDependency] = req
	cx, err := agocontext.NewWithDeps(stream.NewMemoryStream(), deps, vars)
	if err != nil {
		return nil, fmt.Errorf("mcpserver: request context: %w", err)
	}
	cx.SetTurnContext(ctx)
	return cx, nil
}

// advertise is the entry's tools/list object, with ToolMeta.Resolve run
// against the request's already-built context.
func (s *Server) advertise(_ context.Context, t *toolEntry, cx *agocontext.Context) map[string]any {
	out := map[string]any{
		"name":        t.name,
		"description": t.description,
		"inputSchema": t.inputSchema,
	}
	meta := t.meta
	if meta.Resolve != nil && cx != nil {
		meta = meta.merged(cx)
	}
	if meta.Title != "" {
		out["title"] = meta.Title
	}
	if meta.Annotations != nil {
		out["annotations"] = meta.Annotations
	}
	if meta.OutputSchema != nil {
		out["outputSchema"] = meta.OutputSchema
	}
	if len(meta.Meta) > 0 {
		out["_meta"] = meta.Meta
	}
	return out
}

// structuredOf is the JSON object a result answers as structuredContent: the
// one data part of a tool result that encodes as an object. Anything else has
// no structured form.
//
// AGO-DEVIATION: Python answers structuredContent for every Mapping result;
// AGo answers it only for a tool advertising an output schema (see
// Server.callTool), so a server configured without ToolMeta keeps the wire it
// had. See docs/DECISIONS.md.
func structuredOf(parts []events.Input) map[string]any {
	if len(parts) != 1 {
		return nil
	}
	data, ok := events.AsValue(parts[0]).(events.DataInput)
	if !ok {
		return nil
	}
	return objectOf(data.Data)
}

// objectOf returns v as a JSON object, or nil when it does not encode as one.
func objectOf(v any) map[string]any {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	return obj
}
