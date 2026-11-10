// Package mcpserver serves one agent (or one Ask bridge) as an MCP server:
// JSON-RPC 2.0 over newline-delimited stdio or over streamable HTTP, with the
// agent exposed as a single conversational "ask" tool — the inverse of the
// consume-side ago/mcp client. Parity with Python AG2 ag2/mcp/ (server.py,
// executor.py, tools.py, transport.py, sessions.py, resources.py, prompts.py,
// elicitation.py, sampling.py) on the mcp 2.x SDK's handshake-era wire.
//
// Served surface: initialize (version negotiation over the four handshake
// revisions), tools/list and tools/call (arguments validated against the
// advertised schema), resources/list, resources/read and
// resources/templates/list, prompts/list and prompts/get, ping.
//
// The zero TransportConfig is the stateless, JSON-answering transport. With
// TransportConfig.Stateful the server issues Mcp-Session-Id and keeps per-session
// state (the client's capabilities, the conversation continuing across calls,
// the standalone GET stream); with StreamResponses each request is answered as an
// SSE stream, which carries a served agent's progress and log notifications and
// its questions to the client (elicitation/create, sampling/createMessage) ahead
// of the response; an EventStore makes streams resumable with Last-Event-ID.
//
// Not served: the modern per-request-envelope era (2026-07-28) and the pause/
// resume of suspended turns it needs, MCP Apps, and the OAuth 2.1 resource
// server (docs/DECISIONS.md D-058). Deployment gates that carry no server state
// exist as plain config: an Authorize hook gates the protocol surface, Host/
// Origin allowlists gate the HTTP transport, and the stdio loop runs each
// exchange on its own deadline.
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
	"time"

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

	// defaultMaxStreams caps the concurrent GET listener streams when
	// ServerConfig.MaxStreams is unset. The streamable-HTTP shape is one
	// stream per client session, so the cap leaves room for a fleet of local
	// clients while bounding what a stream-opening loop can pin — each held
	// GET is one connection plus one goroutine until the client goes away.
	defaultMaxStreams = 64

	// defaultCallTimeout bounds one stdio exchange when ServerConfig.
	// CallTimeout is unset: far above a legitimate agent turn, far below the
	// forever a stuck handler would otherwise hold the single-threaded loop.
	// Raise it — or disable it with any negative value — when an exchange
	// legitimately runs longer, e.g. behind a human-approval gate.
	defaultCallTimeout = 10 * time.Minute
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

	// Authorize gates the protocol surface: it runs before dispatch on every
	// initialize and tools/* message, and a returned error refuses the call
	// with a fixed unauthorized error (the error text stays off the wire; a
	// refused notification is dropped, since no caller is waiting). It must
	// be safe for concurrent use. Nil leaves the surface open — do not
	// expose a server beyond loopback without either this hook or an
	// authenticating proxy in front of it.
	//
	// The ctx carries the caller's transport: over HTTP,
	// RequestFromContext returns the originating request (headers, remote
	// address) for credential checks; over stdio it returns nil, and the
	// hook can still gate by method.
	//
	// AGO-DEVIATION: Python gates the surface with an OAuth 2.1 resource
	// server requirement (security.py — bearer middleware answering HTTP
	// 401); the Go subset takes this plain hook, and the OAuth routes stay
	// deferred with D-058.
	Authorize func(ctx context.Context, method string) error

	// MaxStreams caps how many GET listener streams the HTTP transport holds
	// open at once; a GET over the cap is answered 503 and holds nothing.
	// Zero means defaultMaxStreams. AGO-DEVIATION: the ported transport has
	// no such cap (D-058); without one, a loop of GETs exhausts connections
	// and goroutines because this subset holds each stream until the client
	// goes away, with no idle reaping.
	MaxStreams int

	// CallTimeout bounds one stdio exchange: when it fires, the exchange's
	// context is canceled, its reply carries the failure, and the loop reads
	// the next message instead of blocking forever. The bound only
	// interrupts handlers that honor their context — one that blocks outside
	// it cannot be stopped in-process. Zero means defaultCallTimeout; any
	// negative value disables the bound.
	//
	// AGO-DEVIATION: the ported stdio loop runs on context.Background()
	// (D-058); one stuck tool would otherwise stop the transport forever.
	CallTimeout time.Duration

	// AllowedHosts, when non-empty, admits on the HTTP transport only
	// requests whose Host header matches one entry exactly (host or
	// host:port, case-insensitive); anything else answers 403. This closes
	// DNS-rebinding against loopback deployments, where a rebinding page
	// reaches the server with its own Host. Nil disables the check.
	//
	// AGO-DEVIATION: TransportConfig's validation deltas were deferred with
	// the OAuth surface (D-058); this allowlist is the Go-native stand-in.
	AllowedHosts []string

	// AllowedOrigins, when non-empty, requires every HTTP request carrying
	// an Origin header (browsers send it cross-origin) to match one entry
	// exactly (scheme://host[:port], case-insensitive); anything else
	// answers 403. Requests without Origin — every native MCP client — pass,
	// so this is a browser-request fence, not authentication. Nil disables
	// the check.
	//
	// AGO-DEVIATION: as AllowedHosts.
	AllowedOrigins []string

	// ToolMeta is the presentation metadata advertised per served tool, keyed
	// by tool name (the conversational tool's included): title, annotations,
	// outputSchema, _meta (Python parity: mcp_tool(title=, annotations=,
	// output_schema=, meta=)). A key naming no served tool, or an output
	// schema that is not an object schema, fails construction with
	// ErrInvalidToolMeta. A tool whose metadata carries an output schema
	// answers a result that is one JSON object as structuredContent as well.
	// Nil advertises nothing extra.
	//
	// AGO-DEVIATION: Python carries this metadata on the tool (mcp_tool
	// arguments); see ToolMeta.
	ToolMeta map[string]ToolMeta

	// Variables and Dependencies are what each request's tools resolve
	// tools.Variable and tools.Inject slots against, and ToolMeta.Resolve
	// reads (Python parity: the variables and dependencies of the request's
	// MCPExecutionContext). They are snapshotted at NewServer; every request
	// works on its own copy.
	Variables    map[string]any
	Dependencies map[string]any

	// ContextProvider supplies the per-request scope, with the request in hand
	// (Python parity: AgentExecutor's context_provider, mcp/executor.py:325-345
	// and server._request_context). It runs on EVERY request: tools/list, and
	// every tools/call — the conversational ask and deterministic WithTools
	// tools alike. The scope feeds the agent's RunOptions (Variables,
	// Dependencies, Tools, Prompt), the deterministic tools' Variables/Inject
	// slots, and ToolMeta.Resolve. A returned error becomes a JSON-RPC error
	// and nothing runs. Nil keeps the defaults (ServerConfig.Variables/
	// Dependencies). Fields left nil in RequestScope keep those defaults;
	// non-nil maps are merged over them with the provider's value winning.
	ContextProvider ContextProvider

	// Transport tunes the streamable-HTTP transport: stateful sessions, SSE
	// answers, resumable streams. The zero value is the stateless JSON transport.
	Transport TransportConfig

	// Conversations turns on multi-turn history for a served agent (only through
	// NewServerForAgent): the conversational tool then takes an optional
	// "conversation" handle. Nil keeps every call independent.
	Conversations *ConversationConfig

	// Resources, ResourceTemplates and Prompts are served over resources/* and
	// prompts/* (Python parity: MCPServer resources=, resource_templates=,
	// prompts=). Each capability is advertised only when something is registered
	// for it, and the sets are fixed at construction, so no list_changed
	// notification is ever promised or sent (Python sends none either).
	Resources         []Resource
	ResourceTemplates []ResourceTemplate
	Prompts           []Prompt

	// ElicitationPolicy decides whether a served agent's questions reach the
	// calling client; the zero value is ElicitationAsk (Python's default). Only a
	// served agent (NewServerForAgent) asks questions.
	ElicitationPolicy ElicitationPolicy
	// ClientModel runs the served agent on the calling client's model through
	// sampling/createMessage (Python parity: client_model=True). Off by default:
	// borrowing the caller's model moves cost, capability and reproducibility to
	// them. A client that advertised no sampling capability is served on the
	// agent's own model, or refused with -32021 when it has none.
	ClientModel bool
	// DisableProgress stops the served agent's stream events being forwarded to
	// the client as progress and log notifications (Python: stream_progress=False).
	DisableProgress bool

	// askWithScope is NewServerForAgent's internal ask path: it needs the
	// provider's RequestScope and the request context, which the public Ask
	// signature does not carry. NewServer prefers it over Ask.
	askWithScope func(ctx context.Context, text string, scope *RequestScope, req *RequestContext) (string, error)
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

// WithAuthorize sets the hook gating initialize and tools/*; see
// ServerConfig.Authorize.
func WithAuthorize(fn func(ctx context.Context, method string) error) ServerOption {
	return func(c *ServerConfig) { c.Authorize = fn }
}

// WithMaxStreams caps the concurrent GET listener streams; see
// ServerConfig.MaxStreams.
func WithMaxStreams(n int) ServerOption { return func(c *ServerConfig) { c.MaxStreams = n } }

// WithCallTimeout bounds one stdio exchange; a negative value disables the
// bound (see ServerConfig.CallTimeout).
func WithCallTimeout(d time.Duration) ServerOption {
	return func(c *ServerConfig) { c.CallTimeout = d }
}

// WithAllowedHosts restricts the Host values the HTTP transport serves; see
// ServerConfig.AllowedHosts.
func WithAllowedHosts(hosts ...string) ServerOption {
	return func(c *ServerConfig) { c.AllowedHosts = hosts }
}

// WithAllowedOrigins restricts the browser origins the HTTP transport
// serves; see ServerConfig.AllowedOrigins.
func WithAllowedOrigins(origins ...string) ServerOption {
	return func(c *ServerConfig) { c.AllowedOrigins = origins }
}

// toolEntry is one advertised MCP tool: its declaration and the way a call
// runs. Instances are immutable after NewServer.
type toolEntry struct {
	name        string
	description string
	inputSchema map[string]any
	meta        ToolMeta
	// call runs one tools/call. The outcome is a tool result even on failure:
	// an agent/tool failure is isError content (Python parity: the executor's
	// tool_error arm), while unknown names are refused before dispatch as a
	// JSON-RPC invalid-params error.
	call func(ctx context.Context, args map[string]any, scope *RequestScope, req *RequestContext) toolCallOutcome
}

// Server exposes one Ask bridge as an MCP server. The zero flow: Handler()
// is an http.Handler and Stdio() serves a process; the JSON-RPC method
// dispatch itself is transport-independent. A Server is safe for concurrent
// use: the tool registry is fixed at construction and the only mutable state
// is the initialized flag (atomic); the GET listener-stream slots are a
// buffered channel.
type Server struct {
	name         string
	version      string
	instructions string

	ask    *toolEntry
	extras []*toolEntry // construction order; the ask tool is always first on the wire

	authorize       func(ctx context.Context, method string) error // nil = surface open
	contextProvider ContextProvider
	streams         chan struct{} // GET listener-stream slots
	callTimeout     time.Duration // stdio exchange bound; < 0 = off
	allowedHosts    []string
	allowedOrigins  []string
	variables       map[string]any
	dependencies    map[string]any

	initialized atomic.Bool

	tr          TransportConfig
	sessions    *sessions // nil on a stateless transport
	convs       *conversations
	resources   *resourceProvider
	prompts     *promptProvider
	elicitation ElicitationPolicy
	clientModel bool
	progress    bool
}

// NewServer builds a Server from cfg.
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Ask == nil && cfg.askWithScope == nil {
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
	maxStreams := cfg.MaxStreams
	if maxStreams <= 0 {
		maxStreams = defaultMaxStreams
	}
	callTimeout := cfg.CallTimeout
	if callTimeout == 0 {
		callTimeout = defaultCallTimeout
	}

	ask := &toolEntry{
		name:        toolName,
		description: cfg.ToolDescription,
		inputSchema: askInputSchema(),
	}
	if ask.description == "" {
		ask.description = fmt.Sprintf("Send a message to the '%s' agent and receive its reply.", cfg.Name)
	}
	askInvoke := cfg.askWithScope
	if askInvoke == nil {
		askCall := cfg.Ask
		askInvoke = func(ctx context.Context, text string, _ *RequestScope, _ *RequestContext) (string, error) {
			return askCall(ctx, text)
		}
	}
	ask.call = func(ctx context.Context, args map[string]any, scope *RequestScope, req *RequestContext) toolCallOutcome {
		return askOutcome(ctx, askInvoke, args, scope, req, cfg.Conversations != nil)
	}

	if cc := cfg.Conversations; cc != nil && (cc.MaxConversations < 0 || cc.TTL < 0) {
		return nil, fmt.Errorf("%w: ConversationConfig.MaxConversations and TTL must not be negative", ErrNotProvided)
	}
	if cfg.Conversations != nil && cfg.askWithScope == nil {
		return nil, fmt.Errorf("%w: ServerConfig.Conversations needs a served agent (NewServerForAgent)", ErrNotProvided)
	}
	switch cfg.ElicitationPolicy {
	case "", ElicitationAsk, ElicitationDecline:
	default:
		return nil, fmt.Errorf("%w: unknown elicitation policy %q", ErrNotProvided, cfg.ElicitationPolicy)
	}
	s := &Server{
		tr:              cfg.Transport,
		elicitation:     cfg.ElicitationPolicy,
		clientModel:     cfg.ClientModel,
		progress:        !cfg.DisableProgress,
		name:            cfg.Name,
		version:         cfg.Version,
		instructions:    cfg.Instructions,
		ask:             ask,
		authorize:       cfg.Authorize,
		contextProvider: cfg.ContextProvider,
		streams:         make(chan struct{}, maxStreams),
		callTimeout:     callTimeout,
		allowedHosts:    cfg.AllowedHosts,
		allowedOrigins:  cfg.AllowedOrigins,
		variables:       copyAnyMap(cfg.Variables),
		dependencies:    copyAnyMap(cfg.Dependencies),
	}
	if cfg.Transport.Stateful {
		idle := cfg.Transport.SessionIdleTimeout
		if idle == 0 {
			idle = defaultSessionIdleTimeout
		}
		maxSessions := cfg.Transport.MaxSessions
		if maxSessions == 0 {
			maxSessions = defaultMaxSessions
		}
		s.sessions = newSessions(idle, maxSessions)
		if f, ok := cfg.Transport.EventStore.(StreamForgetter); ok {
			s.sessions.onEnd = func(id string) func() {
				return func() { f.ForgetStreams(context.Background(), id+"/") }
			}
		}
	}
	if cfg.Conversations != nil {
		s.convs = newConversations(*cfg.Conversations)
		ask.inputSchema = askInputSchemaWithConversation(s.convs.describeBounds())
	}
	if len(cfg.Resources) > 0 || len(cfg.ResourceTemplates) > 0 {
		rp, err := newResourceProvider(cfg.Resources, cfg.ResourceTemplates)
		if err != nil {
			return nil, err
		}
		s.resources = rp
	}
	if len(cfg.Prompts) > 0 {
		pp, err := newPromptProvider(cfg.Prompts)
		if err != nil {
			return nil, err
		}
		s.prompts = pp
	}
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
				call: func(ctx context.Context, args map[string]any, scope *RequestScope, req *RequestContext) toolCallOutcome {
					return s.registeredToolOutcome(ctx, t, served.Name, args, scope, req)
				},
			})
		}
	}
	if err := validateToolMeta(cfg.ToolMeta, seen); err != nil {
		return nil, err
	}
	s.ask.meta = cfg.ToolMeta[toolName]
	for _, e := range s.extras {
		e.meta = cfg.ToolMeta[e.name]
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
	// Snapshot the configured maps: the server's per-request copies must not
	// follow later writes to the caller's map (Python parity: the provider, not
	// a live map, is the per-request seam).
	cfg.Variables = copyAnyMap(cfg.Variables)
	cfg.Dependencies = copyAnyMap(cfg.Dependencies)
	var srv *Server // the closure below reads the conversation store NewServer builds
	cfg.askWithScope = func(ctx context.Context, userText string, scope *RequestScope, req *RequestContext) (string, error) {
		// Python parity: executor.py:325-345 hands ask() the provider's
		// variables, dependencies, tools and prompt; nil fields keep the ask
		// defaults. The request context travels too, under RequestDependency,
		// so a served agent's tool resolves the same Inject slot a
		// deterministic tool does.
		vars := copyAnyMap(cfg.Variables)
		deps := copyAnyMap(cfg.Dependencies)
		var extraTools []tools.Tool
		var prompt []string
		if scope != nil {
			for k, v := range scope.Variables {
				vars[k] = v
			}
			for k, v := range scope.Dependencies {
				deps[k] = v
			}
			extraTools = scope.Tools
			prompt = scope.Prompt
		}
		if req == nil {
			toolName := cfg.ToolName
			if toolName == "" {
				toolName = defaultToolName
			}
			req = &RequestContext{Method: "tools/call", Tool: toolName, HTTP: RequestFromContext(ctx)}
		}
		deps[RequestDependency] = req
		opts := agent.RunOptions{
			Variables:    vars,
			Dependencies: deps,
			Tools:        extraTools,
			Prompt:       prompt,
		}
		env, ex, sess := callEnvFrom(ctx), exchangeFrom(ctx), sessionFrom(ctx)

		// The calling client's model, when the operator asked for it and the
		// client can lend one; otherwise the agent's own, or a refusal naming the
		// missing capability (Python parity: executor._client_model_config).
		if cfg.ClientModel {
			switch {
			case ex != nil && ex.canRequest && sess != nil && sess.clientCapabilities().sampling:
				opts.Config = clientModelConfig{ex: ex}
			case a.Config() != nil:
			default:
				if env != nil {
					env.protocolErr = samplingUnavailableError()
				}
				return "", ErrSamplingUnavailable
			}
		}
		// The client's human answers the agent's questions, when it declared it
		// can; otherwise the agent's own hook does (Python parity: the elicitor
		// registered ahead of the agent's hook, falling through when the channel is
		// unusable).
		if canElicit(cfg.ElicitationPolicy, ex, sess) {
			opts.HitlHook = elicitationHook(ex)
		}

		exec := func(st stream.Stream) (string, error) {
			runOpts := opts
			runOpts.Stream = st
			run, err := a.RunWith(ctx, runOpts, userText)
			if err != nil {
				return "", fmt.Errorf("mcpserver: start turn: %w", err)
			}
			defer run.Close()
			if !cfg.DisableProgress {
				sub := run.Stream().Subscribe(newProgressForwarder(ctx, ex, env).handle, stream.SubscribeOptions{})
				defer run.Stream().Unsubscribe(sub)
			}
			if err := run.Start(ctx); err != nil {
				return "", err
			}
			reply, err := run.Result(ctx)
			if err != nil {
				return "", err
			}
			return reply.Body(), nil
		}
		if srv == nil || srv.convs == nil {
			return exec(nil)
		}
		// Conversations: the named handle wins; otherwise the MCP session's own
		// conversation (stdio's one shared session included); otherwise a new one
		// under a fresh handle (Python parity: executor._conversation_cm).
		var out string
		turn := func(c conversation) error {
			if env != nil {
				env.handle = c.handle
			}
			var err error
			out, err = exec(c.stream)
			return err
		}
		var err error
		switch handle := ""; {
		case env != nil && strings.TrimSpace(env.conversation) != "":
			handle = strings.TrimSpace(env.conversation)
			err = srv.convs.byHandleRun(handle, turn)
		case sess != nil:
			err = srv.convs.session(sess.id, turn)
		case isStdio(ctx):
			err = srv.convs.session(stdioSession, turn)
		default:
			err = srv.convs.fresh(turn)
		}
		return out, err
	}
	var err error
	srv, err = NewServer(cfg)
	return srv, err
}

// copyAnyMap copies a string-keyed map, treating nil as empty.
func copyAnyMap(src map[string]any) map[string]any {
	out := make(map[string]any, len(src)+1)
	for k, v := range src {
		out[k] = v
	}
	return out
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

// askInputSchemaWithConversation adds the optional "conversation" argument, which
// exists only when a conversation store is configured (Python parity:
// info.build_ask_tool(conversation_bounds=...)).
func askInputSchemaWithConversation(lifetime string) map[string]any {
	schema := askInputSchema()
	props := schema["properties"].(map[string]any)
	props["conversation"] = map[string]any{
		"type": "string",
		"description": "Opaque handle naming the conversation to continue, as returned by an earlier " +
			"call to this tool. Omit it to start a new conversation; the handle for it comes back with the reply. " + lifetime,
	}
	return schema
}

// --- JSON-RPC wire shapes ------------------------------------------------------

// JSON-RPC 2.0 error codes (the reserved range the MCP methods use).
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603

	// codeUnauthorized refuses a call whose ServerConfig.Authorize check
	// failed. JSON-RPC reserves -32000..-32099 for implementation-defined
	// server errors; MCP carries auth failures at the HTTP layer (401 with
	// WWW-Authenticate), which this subset's always-JSON answers replace
	// until the OAuth routes land (D-058) — so the refusal travels the one
	// channel both transports share, with a fixed message (the hook's error
	// text stays off the wire).
	codeUnauthorized = -32001
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
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
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
	if s.authorize != nil && gatedMethod(req.Method) {
		if err := s.authorize(ctx, req.Method); err != nil {
			// A notification has no caller to answer; a refusal drops it.
			if req.ID == nil || isNullID(req.ID) {
				return nil
			}
			return &rpcResponse{JSONRPC: "2.0", ID: req.ID,
				Error: rpcErr(codeUnauthorized, "unauthorized")}
		}
	}
	sess := sessionFrom(ctx)
	if req.ID == nil || isNullID(req.ID) {
		// A notification: only the initialized confirmation carries meaning.
		if req.Method == "notifications/initialized" {
			s.markInitialized(sess)
		}
		return nil
	}
	fail := func(e *rpcError) *rpcResponse { return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: e} }
	reply := func(result any) *rpcResponse {
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
	}
	notReady := rpcErr(codeInvalidRequest, "Call initialize before listing or calling tools.")
	switch req.Method {
	case "initialize":
		// The handshake request itself opens the gate (the confirmation
		// notification sets it too); the other methods refuse until one ran.
		return reply(s.initialize(sess, req.Params))
	case "notifications/initialized":
		// Sent as a request rather than a notification: confirm with an empty
		// result so the caller is not left waiting.
		s.markInitialized(sess)
		return reply(map[string]any{})
	case "ping":
		return reply(map[string]any{})
	case "tools/list":
		if !s.ready(sess) {
			return fail(notReady)
		}
		reqCtx, scope, err := s.resolveRequestScope(ctx, "tools/list", "")
		if err != nil {
			return fail(rpcErr(codeInternalError, "context provider: %v", err))
		}
		return reply(s.listTools(ctx, reqCtx, scope))
	case "tools/call":
		if !s.ready(sess) {
			return fail(notReady)
		}
		return s.callTool(ctx, req.ID, req.Params)
	case "resources/list", "resources/read", "resources/templates/list":
		if s.resources == nil || (req.Method == "resources/templates/list" && !s.resources.hasTemplates()) {
			return fail(rpcErr(codeMethodNotFound, "method %q is not supported", req.Method))
		}
		if !s.ready(sess) {
			return fail(notReady)
		}
		return s.serveResources(ctx, req)
	case "prompts/list", "prompts/get":
		if s.prompts == nil {
			return fail(rpcErr(codeMethodNotFound, "method %q is not supported", req.Method))
		}
		if !s.ready(sess) {
			return fail(notReady)
		}
		return s.servePrompts(ctx, req)
	default:
		return fail(rpcErr(codeMethodNotFound, "method %q is not supported", req.Method))
	}
}

// ready reports whether the handshake ran: on this session when the transport is
// stateful, on the server otherwise.
func (s *Server) ready(sess *mcpSession) bool {
	if sess != nil {
		return sess.isInitialized()
	}
	return s.initialized.Load()
}

func (s *Server) markInitialized(sess *mcpSession) {
	if sess != nil {
		sess.mu.Lock()
		sess.initialized = true
		sess.mu.Unlock()
		return
	}
	s.initialized.Store(true)
}

// gatedMethod reports whether ServerConfig.Authorize runs for a method:
// initialize and the data methods are everything an unauthenticated caller
// could learn from or drive. ping and the notifications answer nothing about
// the served agent, so they stay open.
func gatedMethod(method string) bool {
	return method == "initialize" || strings.HasPrefix(method, "tools/") ||
		strings.HasPrefix(method, "resources/") || strings.HasPrefix(method, "prompts/")
}

// callCtx bounds one stdio exchange (ServerConfig.CallTimeout; negative
// disables the bound). The deadline only interrupts handlers that honor the
// context — the loop itself stays single-threaded until the handler returns.
func (s *Server) callCtx() (context.Context, context.CancelFunc) {
	if s.callTimeout > 0 {
		return context.WithTimeout(context.Background(), s.callTimeout)
	}
	return context.Background(), func() {}
}

// initialize answers the handshake: the requested revision is echoed when the
// server speaks it and answered with its latest otherwise (the SDK's version
// negotiation over the handshake revisions), the client's declared capabilities
// are recorded on the session, and only the capabilities actually served are
// advertised.
func (s *Server) initialize(sess *mcpSession, params json.RawMessage) map[string]any {
	var p struct {
		ProtocolVersion string          `json:"protocolVersion"`
		Capabilities    json.RawMessage `json:"capabilities"`
	}
	_ = json.Unmarshal(params, &p)
	version := latestHandshakeVersion
	if supportedHandshake(p.ProtocolVersion) {
		version = p.ProtocolVersion
	}
	if sess != nil {
		sess.mu.Lock()
		sess.initialized = true
		sess.protocolVersion = version
		sess.caps = parseClientCaps(p.Capabilities)
		sess.mu.Unlock()
	} else {
		s.initialized.Store(true)
	}
	caps := map[string]any{
		// Truthful advertisement: the served set is fixed at construction, so
		// there is never a list-changed notification to promise.
		"tools": map[string]any{"listChanged": false},
	}
	if s.resources != nil {
		caps["resources"] = map[string]any{"subscribe": false, "listChanged": false}
	}
	if s.prompts != nil {
		caps["prompts"] = map[string]any{"listChanged": false}
	}
	result := map[string]any{
		"protocolVersion": version,
		"capabilities":    caps,
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
// entries). The request's scope is resolved once by the caller and supplies
// ToolMeta.Resolve with its variables/dependencies.
func (s *Server) listTools(ctx context.Context, req *RequestContext, scope *RequestScope) map[string]any {
	all := append([]*toolEntry{s.ask}, s.extras...)
	out := make([]map[string]any, 0, len(all))
	cx, err := s.requestContextFor(ctx, req, scope)
	if err != nil {
		cx = nil // static metadata is still truthful
	}
	for _, t := range all {
		out = append(out, s.advertise(ctx, t, cx))
	}
	return map[string]any{"tools": out}
}

// serveResources answers resources/list, resources/templates/list and
// resources/read. A read of an unmatched URI is invalid params (-32602) carrying
// the URI; any other failure of the reader is an internal error.
func (s *Server) serveResources(ctx context.Context, req *rpcRequest) *rpcResponse {
	fail := func(e *rpcError) *rpcResponse { return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: e} }
	switch req.Method {
	case "resources/list":
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: s.resources.list()}
	case "resources/templates/list":
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: s.resources.listTemplates()}
	}
	var p struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.URI == "" {
		return fail(rpcErr(codeInvalidParams, "resources/read params.uri is required"))
	}
	rc, scope, err := s.resolveRequestScope(ctx, "resources/read", "")
	if err != nil {
		return fail(rpcErr(codeInternalError, "context provider: %v", err))
	}
	cx, err := s.requestContextFor(ctx, rc, scope)
	if err != nil {
		return fail(rpcErr(codeInternalError, "%v", err))
	}
	res, err := s.resources.read(ctx, cx, p.URI)
	switch {
	case errors.Is(err, ErrResourceNotFound):
		e := rpcErr(codeInvalidParams, "No resource matches URI '%s'.", p.URI)
		e.Data, _ = json.Marshal(map[string]any{"uri": p.URI})
		return fail(e)
	case err != nil:
		return fail(rpcErr(codeInternalError, "%v", err))
	}
	return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: res}
}

// servePrompts answers prompts/list and prompts/get.
func (s *Server) servePrompts(ctx context.Context, req *rpcRequest) *rpcResponse {
	fail := func(e *rpcError) *rpcResponse { return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: e} }
	if req.Method == "prompts/list" {
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: s.prompts.list()}
	}
	var p struct {
		Name      string            `json:"name"`
		Arguments map[string]string `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
		return fail(rpcErr(codeInvalidParams, "prompts/get params.name is required"))
	}
	res, err := s.prompts.get(ctx, p.Name, p.Arguments)
	var missing *errMissingPromptArgs
	switch {
	case errors.Is(err, ErrPromptNotFound):
		return fail(rpcErr(codeInvalidParams, "No prompt named '%s'.", p.Name))
	case errors.As(err, &missing):
		return fail(rpcErr(codeInvalidParams, "%s", missing.Error()))
	case err != nil:
		return fail(rpcErr(codeInternalError, "%v", err))
	}
	return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: res}
}

// callEnv is what one tools/call hands down to the ask path besides its text:
// the call's conversation handle argument, the progress token it asked for, and
// where the ask path reports the conversation it ran in.
type callEnv struct {
	progressToken any
	conversation  string
	handle        string // set by the ask path: the conversation this call ran in
	protocolErr   *rpcError
}

type callEnvKey struct{}

func withCallEnv(ctx context.Context, e *callEnv) context.Context {
	return context.WithValue(ctx, callEnvKey{}, e)
}

func callEnvFrom(ctx context.Context) *callEnv {
	e, _ := ctx.Value(callEnvKey{}).(*callEnv)
	return e
}

// callTool serves tools/call: the registry lookup (unknown names are a
// JSON-RPC invalid-params error, Python parity: the executor's MCPError),
// argument validation against the advertised schema (a violation is a
// tool-level error, as in Python) and the entry's call, whose outcome is always
// a tool result.
func (s *Server) callTool(ctx context.Context, id json.RawMessage, params json.RawMessage) *rpcResponse {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
		Meta      struct {
			ProgressToken any `json:"progressToken"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return &rpcResponse{JSONRPC: "2.0", ID: id,
			Error: rpcErr(codeInvalidParams, "tools/call params: %v", err)}
	}
	if p.Name == "" {
		return &rpcResponse{JSONRPC: "2.0", ID: id,
			Error: rpcErr(codeInvalidParams, "tools/call params.name is required")}
	}
	req, scope, err := s.resolveRequestScope(ctx, "tools/call", p.Name)
	if err != nil {
		// Python parity: the provider runs per request before dispatch; a
		// failure refuses the call and nothing (agent or deterministic tool)
		// runs.
		return &rpcResponse{JSONRPC: "2.0", ID: id,
			Error: rpcErr(codeInternalError, "context provider: %v", err)}
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
	// Python parity (server._on_call_tool): the arguments are validated against
	// the advertised inputSchema before anything runs, and a violation is a
	// tool-level error result — the model can fix its arguments and retry.
	if msg := validateArguments(p.Arguments, entry.inputSchema); msg != "" {
		return &rpcResponse{JSONRPC: "2.0", ID: id, Result: map[string]any{
			"content": []contentBlock{textBlock("Input validation error: " + msg)},
			"isError": true,
		}}
	}
	env := &callEnv{progressToken: p.Meta.ProgressToken}
	if convo, ok := p.Arguments["conversation"].(string); ok {
		env.conversation = convo
	}
	ctx = withCallEnv(ctx, env)
	outcome := entry.call(ctx, p.Arguments, scope, req)
	if env.protocolErr != nil {
		// A refusal the model cannot reword its way out of stays a JSON-RPC error.
		return &rpcResponse{JSONRPC: "2.0", ID: id, Error: env.protocolErr}
	}
	result := map[string]any{"content": outcome.content}
	if outcome.isError {
		result["isError"] = true
	} else if outcome.structured != nil && s.declaresOutputSchema(ctx, entry, req, scope) {
		// AGO-DEVIATION: only a tool advertising an output schema answers
		// structuredContent (Python: any Mapping result); see structuredOf.
		result["structuredContent"] = outcome.structured
	}
	if env.handle != "" {
		result["_meta"] = map[string]any{ConversationMetaKey: env.handle}
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
	// structured is the result as one JSON object when it has that form; it is
	// answered as structuredContent only for a tool advertising an output
	// schema.
	structured map[string]any
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
func askOutcome(ctx context.Context, ask func(context.Context, string, *RequestScope, *RequestContext) (string, error), args map[string]any, scope *RequestScope, req *RequestContext, conversationsOn bool) toolCallOutcome {
	message, _ := args["message"].(string)
	if message == "" {
		// The schema check passes an empty string, so the executor's own guard
		// stands in (Python parity: executor.call "Missing required 'message'
		// argument.").
		return toolErrorOutcome("Missing required 'message' argument.")
	}
	env := callEnvFrom(ctx)
	// A blank handle names no conversation, so it reads as none rather than as an
	// unknown one: a model given an optional string argument routinely sends ""
	// instead of omitting the key (Python parity).
	if convo, _ := args["conversation"].(string); strings.TrimSpace(convo) != "" && !conversationsOn {
		// An unadvertised argument carrying a handle: refused rather than
		// dropped (Python parity: the sessions=False arm of executor.call).
		return toolErrorOutcome("This server does not maintain conversations, so the 'conversation' argument is " +
			"not supported; omit it. Each call is independent.")
	}
	if env != nil && strings.TrimSpace(env.conversation) == "" {
		env.conversation = ""
	}
	userText := message
	if context, _ := args["context"].(string); context != "" {
		userText = "Context:\n" + context + "\n" + message
	}
	reply, err := ask(ctx, userText, scope, req)
	if err != nil {
		if errors.Is(err, ErrUnknownConversation) {
			// A tool-level error, never a JSON-RPC one, so the model can start a
			// new conversation rather than fail the turn.
			return toolErrorOutcome("Unknown or expired conversation handle. Omit the 'conversation' argument to start a new conversation.")
		}
		// The wire carries the message only (Python parity: the executor's
		// exception arm flattening a failure into a tool result).
		return toolErrorOutcome(err.Error())
	}
	// MCP requires at least one content block; an empty reply maps to ""
	// (Python parity: reply_to_content).
	//
	// AGO-DEVIATION: Python validates the reply against the agent's response
	// schema and answers the validated object as structuredContent
	// (mcp/executor.py:370-384). AGo has no response-schema binding on the
	// Ask bridge, so a reply that looks like one JSON object is parsed and
	// carried as structuredContent; a non-object or invalid reply stays text.
	var structured map[string]any
	if strings.HasPrefix(strings.TrimSpace(reply), "{") {
		_ = json.Unmarshal([]byte(reply), &structured)
	}
	content := []contentBlock{textBlock(reply)}
	if env != nil && env.handle != "" {
		// A text block for the model, which does not read protocol metadata; the
		// handle rides _meta too, for clients threading it programmatically.
		content = append(content, textBlock(handleText(env.handle)))
	}
	return toolCallOutcome{content: content, structured: structured}
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
func (s *Server) registeredToolOutcome(ctx context.Context, t tools.Tool, name string, args map[string]any, scope *RequestScope, req *RequestContext) toolCallOutcome {
	cx, err := s.requestContextFor(ctx, req, scope)
	if err != nil {
		return toolErrorOutcome(err.Error())
	}
	ms := cx.Stream
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
		return toolCallOutcome{content: resultContent(result.Parts, name), structured: structuredOf(result.Parts)}
	}
}

// declaresOutputSchema reports whether the entry advertises an output schema
// for this request, its per-request resolution included.
func (s *Server) declaresOutputSchema(ctx context.Context, t *toolEntry, req *RequestContext, scope *RequestScope) bool {
	meta := t.meta
	if meta.Resolve != nil {
		if cx, err := s.requestContextFor(ctx, req, scope); err == nil {
			meta = meta.merged(cx)
		}
	}
	return meta.OutputSchema != nil
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

// WithTransport tunes the streamable-HTTP transport; see TransportConfig.
func WithTransport(tr TransportConfig) ServerOption {
	return func(c *ServerConfig) { c.Transport = tr }
}

// WithConversations turns on multi-turn history for the served agent; see
// ServerConfig.Conversations.
func WithConversations(cc ConversationConfig) ServerOption {
	return func(c *ServerConfig) { c.Conversations = &cc }
}

// WithResources registers static resources; see ServerConfig.Resources.
func WithResources(rs ...Resource) ServerOption {
	return func(c *ServerConfig) { c.Resources = append(c.Resources, rs...) }
}

// WithResourceTemplates registers URI-template resources; see
// ServerConfig.ResourceTemplates.
func WithResourceTemplates(ts ...ResourceTemplate) ServerOption {
	return func(c *ServerConfig) { c.ResourceTemplates = append(c.ResourceTemplates, ts...) }
}

// WithPrompts registers prompts; see ServerConfig.Prompts.
func WithPrompts(ps ...Prompt) ServerOption {
	return func(c *ServerConfig) { c.Prompts = append(c.Prompts, ps...) }
}

// WithElicitationPolicy sets whether the served agent's questions reach the
// calling client; see ServerConfig.ElicitationPolicy.
func WithElicitationPolicy(p ElicitationPolicy) ServerOption {
	return func(c *ServerConfig) { c.ElicitationPolicy = p }
}

// WithClientModel runs the served agent on the calling client's model; see
// ServerConfig.ClientModel.
func WithClientModel() ServerOption { return func(c *ServerConfig) { c.ClientModel = true } }

// WithoutProgress stops forwarding the agent's stream events to the client as
// progress and log notifications; see ServerConfig.DisableProgress.
func WithoutProgress() ServerOption { return func(c *ServerConfig) { c.DisableProgress = true } }
