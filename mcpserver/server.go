// Package mcpserver serves one agent (or one Ask bridge) as an MCP server:
// JSON-RPC 2.0 over newline-delimited stdio or over streamable HTTP, with the
// agent exposed as a single conversational "ask" tool — the inverse of the
// consume-side ago/mcp client. Parity with Python AG2 ag2/mcp/ (server.py,
// executor.py, tools.py, transport.py).
//
// The functional subset covers the core serving surface: initialize (version
// negotiation), tools/list (the ask tool plus optional deterministic tools),
// tools/call, ping and notifications/initialized. Everything that binds server
// state across calls — conversation sessions, paused runs, MCP Apps UI, OAuth
// resource-server security — is deferred (docs/DECISIONS.md D-056); the server
// is stateless, and every tools/call runs an independent exchange.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/luckystrker/ago/agent"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
)

// Package-level sentinels. Match with errors.Is.
var (
	// ErrNotProvided reports an incomplete ServerConfig (no Ask bridge, or a
	// nil agent to NewServerForAgent).
	ErrNotProvided = errors.New("mcpserver: config value not provided")
	// ErrConflict reports a tool-name collision among the served tools: an
	// entry against the conversational tool's reserved name, or two entries
	// sharing one name (Python parity: MCPToolNameConflictError).
	ErrConflict = errors.New("mcpserver: tool name conflict")
)

// protocolVersion is the one MCP revision this subset speaks — the same
// revision the ago/mcp client offers in its handshake (mcp/jsonrpc.go). A
// client requesting anything else is answered with this one, which is the
// SDK's negotiation shape reduced to a single-element support set.
const protocolVersion = "2024-11-05"

const (
	defaultServerName    = "ago-mcp-server"
	defaultServerVersion = "0.0.0" // Python parity: _DEFAULT_VERSION
	defaultToolName      = "ask"
)

// ServerConfig wires a Server.
type ServerConfig struct {
	// Name and Version populate the initialize handshake's serverInfo
	// (defaults "ago-mcp-server" / "0.0.0").
	Name    string
	Version string

	// Instructions is client-facing guidance carried on the initialize result
	// (Python parity: MCPServer instructions). Empty omits the field.
	Instructions string

	// Ask bridges one tools/call of the conversational tool to the served
	// agent: userText is the call's message (with an optional context argument
	// folded in front) and the returned string becomes the reply's text
	// content. A nil Ask is refused at construction; a returned error becomes
	// a tool-level error result, never a JSON-RPC error.
	Ask func(ctx context.Context, userText string) (string, error)

	// Tools are additional deterministic tools served next to the
	// conversational tool (Python parity: the mcp_tool tools= entries —
	// schema plus function). A tool contributes one MCP tool per function
	// schema its Schemas reports; hosted (non-function) schemas cannot be
	// served and are refused at construction. A name colliding with the
	// conversational tool's name or with another entry fails construction.
	Tools []tools.Tool

	// ToolName renames the conversational tool (Python parity: tool_name,
	// default "ask").
	ToolName string

	// ToolDescription overrides the conversational tool's description.
	ToolDescription string
}

// ServerOption overrides ServerConfig fields for NewServerForAgent.
type ServerOption func(*ServerConfig)

// WithServerName sets the handshake's serverInfo name.
func WithServerName(name string) ServerOption { return func(c *ServerConfig) { c.Name = name } }

// WithServerVersion sets the handshake's serverInfo version.
func WithServerVersion(version string) ServerOption {
	return func(c *ServerConfig) { c.Version = version }
}

// WithServerInstructions sets the handshake's client-facing instructions.
func WithServerInstructions(instructions string) ServerOption {
	return func(c *ServerConfig) { c.Instructions = instructions }
}

// WithToolName renames the conversational tool.
func WithToolName(name string) ServerOption { return func(c *ServerConfig) { c.ToolName = name } }

// WithToolDescription overrides the conversational tool's description.
func WithToolDescription(description string) ServerOption {
	return func(c *ServerConfig) { c.ToolDescription = description }
}

// WithTools adds deterministic tools served next to the conversational tool.
func WithTools(ts ...tools.Tool) ServerOption {
	return func(c *ServerConfig) { c.Tools = append(c.Tools, ts...) }
}

// toolEntry is one advertised MCP tool: its declaration and the way a call
// runs. Instances are immutable after NewServer.
type toolEntry struct {
	name        string
	description string
	inputSchema map[string]any
	// call runs one tools/call. The outcome is a tool result even on failure:
	// an agent/tool failure is isError content (Python parity: the executor's
	// tool_error arm), while unknown names are refused before dispatch as a
	// JSON-RPC invalid-params error.
	call func(ctx context.Context, args map[string]any) toolCallOutcome
}

// Server exposes one Ask bridge as an MCP server. The zero flow: Handler()
// is an http.Handler and Stdio() serves a process; the JSON-RPC method
// dispatch itself is transport-independent. A Server is safe for concurrent
// use: the tool registry is fixed at construction and the only mutable state
// is the initialized flag (atomic).
type Server struct {
	name         string
	version      string
	instructions string

	ask    *toolEntry
	extras []*toolEntry // construction order; the ask tool is always first on the wire

	initialized atomic.Bool
}

// NewServer builds a Server from cfg.
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Ask == nil {
		return nil, fmt.Errorf("%w: ServerConfig.Ask is not set", ErrNotProvided)
	}
	if cfg.Name == "" {
		cfg.Name = defaultServerName
	}
	if cfg.Version == "" {
		cfg.Version = defaultServerVersion
	}
	toolName := cfg.ToolName
	if toolName == "" {
		toolName = defaultToolName
	}

	ask := &toolEntry{
		name:        toolName,
		description: cfg.ToolDescription,
		inputSchema: askInputSchema(),
	}
	if ask.description == "" {
		ask.description = fmt.Sprintf("Send a message to the '%s' agent and receive its reply.", cfg.Name)
	}
	askCall := cfg.Ask
	ask.call = func(ctx context.Context, args map[string]any) toolCallOutcome {
		return askOutcome(ctx, askCall, args)
	}

	s := &Server{name: cfg.Name, version: cfg.Version, instructions: cfg.Instructions, ask: ask}
	seen := map[string]bool{toolName: true}
	// The scratch context exists because the Tool contract hands Schemas a
	// conversation context; function tools ignore it, but a caller's tool may
	// not.
	schemaCtx := agocontext.New(stream.NewMemoryStream())
	for _, t := range cfg.Tools {
		if t == nil {
			return nil, fmt.Errorf("%w: nil tools.Tool in ServerConfig.Tools", ErrNotProvided)
		}
		schemas, err := t.Schemas(schemaCtx)
		if err != nil {
			return nil, fmt.Errorf("mcpserver: tool %q: %w", t.Name(), err)
		}
		for _, raw := range schemas {
			schema, ok := raw.(tools.Schema)
			if !ok {
				// Hosted tools run at the provider; there is no local function
				// to answer tools/call with, so advertising one would be a lie.
				return nil, fmt.Errorf("mcpserver: tool %q: only function schemas can be served over MCP", t.Name())
			}
			if seen[schema.Name] {
				return nil, fmt.Errorf("%w: %q collides with the conversational tool or another entry", ErrConflict, schema.Name)
			}
			seen[schema.Name] = true
			served := schema
			s.extras = append(s.extras, &toolEntry{
				name:        schema.Name,
				description: schema.Description,
				inputSchema: normalizeInputSchema(schema.Parameters),
				call: func(ctx context.Context, args map[string]any) toolCallOutcome {
					return registeredToolOutcome(t, served.Name, args)
				},
			})
		}
	}
	return s, nil
}

// NewServerForAgent serves a *agent.Agent: every conversational tools/call is
// one blocking agent turn — Run, Start, Result — on a fresh stream, and the
// reply body becomes the text content. The handshake's serverInfo name
// defaults to the agent's; opts override the ServerConfig fields.
//
// The agent must be usable with a fresh stream per call — serving shares the
// agent, and a WithStreamBase stream would carry one conversation's history
// into every client's turn (same caveat as agui.ServeAgent). Conversation
// continuity across calls is deferred with the session store (D-056).
func NewServerForAgent(a *agent.Agent, opts ...ServerOption) (*Server, error) {
	if a == nil {
		return nil, fmt.Errorf("%w: NewServerForAgent needs a non-nil *agent.Agent", ErrNotProvided)
	}
	cfg := ServerConfig{Name: a.Name(), Version: defaultServerVersion}
	for _, o := range opts {
		o(&cfg)
	}
	cfg.Ask = func(ctx context.Context, userText string) (string, error) {
		run, err := a.Run(ctx, userText)
		if err != nil {
			return "", fmt.Errorf("mcpserver: start turn: %w", err)
		}
		defer run.Close()
		if err := run.Start(ctx); err != nil {
			return "", err
		}
		reply, err := run.Result(ctx)
		if err != nil {
			return "", err
		}
		return reply.Body(), nil
	}
	return NewServer(cfg)
}

// normalizeInputSchema guarantees the object schema MCP requires (Python
// parity: a tool with no advertised parameters gets the no-argument object).
func normalizeInputSchema(params map[string]any) map[string]any {
	if params == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return params
}

// askInputSchema is the conversational tool's declaration (Python parity:
// info.py build_ask_tool without the conversation argument, which exists only
// when a session store is configured — deferred here, D-056).
func askInputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"message": map[string]any{
				"type":        "string",
				"description": "The message or task to send to the agent.",
			},
			"context": map[string]any{
				"type":        "string",
				"description": "Optional additional context to prepend to the message.",
			},
		},
		"required": []string{"message"},
	}
}

// --- JSON-RPC wire shapes ------------------------------------------------------

// JSON-RPC 2.0 error codes (the reserved range the MCP methods use).
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func rpcErr(code int, format string, args ...any) *rpcError {
	return &rpcError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// isNullID reports a literal null id, which JSON-RPC answers like a
// notification (no addressable caller).
func isNullID(raw json.RawMessage) bool {
	return string(raw) == "null"
}

// handle processes one incoming JSON-RPC message and returns the reply for a
// request — nil for a notification or a message that produced no reply (a
// notification is never answered). Malformed JSON and non-2.0 envelopes never
// reach here: the transports refuse them before dispatch, since a reply id
// cannot be trusted from an unparseable message.
func (s *Server) handle(ctx context.Context, req *rpcRequest) *rpcResponse {
	if req.Method == "" {
		return nil // a reply to a request this server never sent
	}
	if req.ID == nil || isNullID(req.ID) {
		// A notification: only the initialized confirmation carries meaning.
		if req.Method == "notifications/initialized" {
			s.initialized.Store(true)
		}
		return nil
	}
	reply := func(result any) *rpcResponse {
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
	}
	switch req.Method {
	case "initialize":
		// The handshake request itself opens the gate (the confirmation
		// notification sets it too); the tools methods refuse until one ran.
		s.initialized.Store(true)
		return reply(s.initializeResult(req.Params))
	case "notifications/initialized":
		// Sent as a request rather than a notification: confirm with an empty
		// result so the caller is not left waiting.
		s.initialized.Store(true)
		return reply(map[string]any{})
	case "ping":
		return reply(map[string]any{})
	case "tools/list":
		if !s.initialized.Load() {
			return &rpcResponse{JSONRPC: "2.0", ID: req.ID,
				Error: rpcErr(codeInvalidRequest, "Call initialize before listing or calling tools.")}
		}
		return reply(s.listTools())
	case "tools/call":
		if !s.initialized.Load() {
			return &rpcResponse{JSONRPC: "2.0", ID: req.ID,
				Error: rpcErr(codeInvalidRequest, "Call initialize before listing or calling tools.")}
		}
		return s.callTool(ctx, req.ID, req.Params)
	default:
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: rpcErr(codeMethodNotFound, "method %q is not supported", req.Method)}
	}
}

// initializeResult builds the handshake answer. The requested revision is
// echoed when this subset speaks it and answered with its own (latest)
// revision otherwise (Python parity: the SDK's version negotiation over a
// single-element support set).
func (s *Server) initializeResult(params json.RawMessage) map[string]any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	version := protocolVersion
	if p.ProtocolVersion == protocolVersion {
		version = p.ProtocolVersion
	}
	result := map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			// Truthful advertisement: the served set is fixed at construction,
			// so there is never a list-changed notification to promise.
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{
			"name":    s.name,
			"version": s.version,
		},
	}
	if s.instructions != "" {
		result["instructions"] = s.instructions
	}
	return result
}

// listTools serves the registry in construction order: the conversational
// tool first (Python parity: executor.list_tools plus the provider's
// entries). The set is small and fixed, so there is no cursor to hand back.
func (s *Server) listTools() map[string]any {
	all := append([]*toolEntry{s.ask}, s.extras...)
	out := make([]map[string]any, 0, len(all))
	for _, t := range all {
		out = append(out, map[string]any{
			"name":        t.name,
			"description": t.description,
			"inputSchema": t.inputSchema,
		})
	}
	return map[string]any{"tools": out}
}

// callTool serves tools/call: the registry lookup (unknown names are a
// JSON-RPC invalid-params error, Python parity: the executor's MCPError) and
// the entry's call, whose outcome is always a tool result.
func (s *Server) callTool(ctx context.Context, id json.RawMessage, params json.RawMessage) *rpcResponse {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return &rpcResponse{JSONRPC: "2.0", ID: id,
			Error: rpcErr(codeInvalidParams, "tools/call params: %v", err)}
	}
	if p.Name == "" {
		return &rpcResponse{JSONRPC: "2.0", ID: id,
			Error: rpcErr(codeInvalidParams, "tools/call params.name is required")}
	}
	entry := s.entry(p.Name)
	if entry == nil {
		// Python parity: executor.call raises MCPError(INVALID_PARAMS,
		// "Unknown tool") — a protocol-level refusal the caller cannot reword
		// its way out of.
		return &rpcResponse{JSONRPC: "2.0", ID: id,
			Error: rpcErr(codeInvalidParams, "Unknown tool: %q.", p.Name)}
	}
	if p.Arguments == nil {
		p.Arguments = map[string]any{}
	}
	outcome := entry.call(ctx, p.Arguments)
	result := map[string]any{"content": outcome.content}
	if outcome.isError {
		result["isError"] = true
	}
	return &rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

// entry resolves a name against the registry.
func (s *Server) entry(name string) *toolEntry {
	if s.ask.name == name {
		return s.ask
	}
	for _, t := range s.extras {
		if t.name == name {
			return t
		}
	}
	return nil
}

// --- tool outcomes -------------------------------------------------------------

// toolCallOutcome is one tools/call result: content blocks plus the isError
// flag (Python parity: CallToolResult).
type toolCallOutcome struct {
	content []contentBlock
	isError bool
}

// contentBlock is one tools/call content block. The MCP block variants share
// one wire shape here, each field populated only for the variant that carries
// it (the omitempty encoding keeps a text block a text block).
type contentBlock struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	Data     string        `json:"data,omitempty"`
	MIMEType string        `json:"mimeType,omitempty"`
	Resource *blobResource `json:"resource,omitempty"`
	URI      string        `json:"uri,omitempty"`
	Name     string        `json:"name,omitempty"`
}

// blobResource is the embedded resource of a "resource" content block
// (Python parity: BlobResourceContents).
type blobResource struct {
	URI      string `json:"uri"`
	Blob     string `json:"blob"`
	MIMEType string `json:"mimeType,omitempty"`
}

// toolErrorOutcome shapes a tool-level failure: the message as one text block
// (Python parity: mappers.tool_error).
func toolErrorOutcome(message string) toolCallOutcome {
	return toolCallOutcome{content: []contentBlock{textBlock(message)}, isError: true}
}

func textBlock(text string) contentBlock { return contentBlock{Type: "text", Text: text} }

// askOutcome runs the conversational tool: the message argument into the Ask
// bridge, an optional context folded in front (Python parity: executor
// _build_inputs — a "Context:\n…" input ahead of the message; the two inputs
// collapse into the one string the bridge takes, joined with a newline), the
// reply body as text content.
func askOutcome(ctx context.Context, ask func(context.Context, string) (string, error), args map[string]any) toolCallOutcome {
	message, _ := args["message"].(string)
	if message == "" {
		// Argument validation against the advertised schema is deferred
		// (D-056), so the executor's own guard stands in (Python parity:
		// executor.call "Missing required 'message' argument.").
		return toolErrorOutcome("Missing required 'message' argument.")
	}
	if convo, _ := args["conversation"].(string); convo != "" {
		// An unadvertised argument carrying a handle: refused rather than
		// dropped (Python parity: the sessions=False arm of executor.call).
		return toolErrorOutcome("This server does not maintain conversations, so the 'conversation' argument is " +
			"not supported; omit it. Each call is independent.")
	}
	userText := message
	if context, _ := args["context"].(string); context != "" {
		userText = "Context:\n" + context + "\n" + message
	}
	reply, err := ask(ctx, userText)
	if err != nil {
		// The wire carries the message only (Python parity: the executor's
		// exception arm flattening a failure into a tool result).
		return toolErrorOutcome(err.Error())
	}
	// MCP requires at least one content block; an empty reply maps to ""
	// (Python parity: reply_to_content).
	return toolCallOutcome{content: []contentBlock{textBlock(reply)}}
}

// registeredToolOutcome runs one deterministic tool through the tools.Tool
// machinery. A tools.Tool executes by subscribing on a conversation stream, so
// the invocation registers it on a scratch stream, sends the call event and
// collects the result event that comes back during the synchronous dispatch.
//
// AGO-DEVIATION: Python calls the tool handler function directly; the Go Tool
// interface has no direct call, and the scratch-stream round trip is the
// machinery every Tool implementation (function tools included) already
// answers through (D-056).
func registeredToolOutcome(t tools.Tool, name string, args map[string]any) toolCallOutcome {
	ms := stream.NewMemoryStream()
	cx := agocontext.New(ms)
	subs, err := t.Register(cx, nil)
	if err != nil {
		return toolErrorOutcome(err.Error())
	}
	for _, id := range subs {
		defer ms.Unsubscribe(id)
	}

	var (
		mu      sync.Mutex
		result  events.ToolResult
		errText string
		got     bool
	)
	call := events.NewToolCallEvent(name)
	if raw, merr := json.Marshal(args); merr == nil {
		call.Arguments = string(raw)
	}
	ms.Subscribe(func(ev events.Event) error {
		switch v := events.AsValue(ev).(type) {
		case events.ToolResultEvent:
			if v.ParentID != call.ID {
				return nil
			}
			mu.Lock()
			result, got = v.Result, true
			mu.Unlock()
		case events.ToolErrorEvent:
			if v.ParentID != call.ID {
				return nil
			}
			mu.Lock()
			errText, got = v.Error, true
			mu.Unlock()
		}
		return nil
	}, stream.SubscribeOptions{})

	if err := ms.Send(call); err != nil {
		return toolErrorOutcome(err.Error())
	}
	mu.Lock()
	defer mu.Unlock()
	switch {
	case errText != "":
		return toolErrorOutcome(errText)
	case !got:
		// A tool that registered but never answered must not hang the caller
		// on an empty reply.
		return toolErrorOutcome(fmt.Sprintf("tool %q returned no result", name))
	default:
		return toolCallOutcome{content: resultContent(result.Parts, name)}
	}
}

// resultContent maps tool result parts onto MCP content blocks (Python parity:
// mappers.reply_to_content / _file_to_content). A part with no closer block
// shape travels as its JSON (the same "future variants are preserved" policy
// as the client's content.go), and an empty result is one empty text block.
func resultContent(parts []events.Input, fallbackName string) []contentBlock {
	if len(parts) == 0 {
		return []contentBlock{textBlock("")}
	}
	out := make([]contentBlock, 0, len(parts))
	for _, part := range parts {
		switch v := events.AsValue(part).(type) {
		case events.TextInput:
			out = append(out, textBlock(v.Content))
		case events.DataInput:
			out = append(out, textBlock(compactJSON(v.Data)))
		case events.BinaryInput:
			out = append(out, binaryBlock(v, fallbackName))
		case events.UrlInput:
			// resource_link's name is required by the spec; the serving tool's
			// name is the only handle available here.
			out = append(out, contentBlock{Type: "resource_link", URI: v.URL, Name: fallbackName})
		default:
			out = append(out, textBlock(compactJSON(v)))
		}
	}
	return out
}

// binaryBlock maps one binary part by its media type (Python parity:
// _file_to_content): image and audio get their own blocks, everything else an
// embedded blob resource. The part carries no name, so the serving tool's name
// mints the resource URI.
func binaryBlock(v events.BinaryInput, fallbackName string) contentBlock {
	mediaType := v.MediaType
	if mediaType == "" {
		switch v.Kind {
		case events.BinaryImage:
			mediaType = "image/png" // Python parity: _DEFAULT_MEDIA_TYPE
		case events.BinaryAudio:
			mediaType = "audio/wav"
		}
	}
	encoded := base64.StdEncoding.EncodeToString(v.Data)
	switch {
	case strings.HasPrefix(mediaType, "image/"):
		return contentBlock{Type: "image", Data: encoded, MIMEType: mediaType}
	case strings.HasPrefix(mediaType, "audio/"):
		return contentBlock{Type: "audio", Data: encoded, MIMEType: mediaType}
	default:
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		return contentBlock{Type: "resource", Resource: &blobResource{
			URI: "resource://" + fallbackName, Blob: encoded, MIMEType: mediaType,
		}}
	}
}

// compactJSON renders a value without insignificant whitespace, falling back
// to fmt when it does not encode (a text block's content is a string, and a
// model is the reader).
func compactJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}
