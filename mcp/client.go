package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// ServerInfo is what the server advertises in the initialize handshake.
type ServerInfo struct {
	ProtocolVersion string
	Capabilities    map[string]any
	Name            string
	Version         string
}

// RemoteTool is one tool the server lists.
type RemoteTool struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// ToolOutcome is the outcome of a successful tools/call: the content blocks
// mapped to typed Input parts (see content.go) and the raw result object.
type ToolOutcome struct {
	Parts []events.Input
	// Raw is the full JSON-RPC result object, verbatim as the server sent it.
	Raw json.RawMessage
}

// maxToolPages bounds tools/list pagination (a repeated cursor aborts earlier).
const maxToolPages = 100

// Client is a minimal MCP (Model Context Protocol) client: JSON-RPC 2.0 over
// streamable HTTP or over a local subprocess's stdio. One Client owns one
// session.
//
// AGO-DEVIATION: the Python proxy opens a fresh short-lived session per tool
// call because its sessions are async context managers; a Go Client owns its
// process/connection for its lifetime, and a lost session (HTTP 404) is
// re-established transparently with one retry. The brief's contract —
// Initialize/ListTools/CallTool on one object — implies the persistent session.
//
// Operations serialize under the Client's lock: at most one JSON-RPC exchange is
// in flight. A Client is safe for concurrent use.
//
// AGO-DEVIATION: the exported operations carry no context.Context (the porting
// brief pins the signatures); they run on context.Background(). CallToolWith is
// the additive form that takes one, plus the conversation a server's own
// requests are answered against. The transports
// still cap every exchange with their own deadline (MCPServerConfig.Timeout,
// MCPStdioConfig.Timeout), so a hung server fails the call after the deadline
// instead of holding this Client's lock forever.
type Client struct {
	kind     configKind
	httpCfg  *MCPServerConfig
	stdioCfg *MCPStdioConfig

	policy AnswerPolicy
	mode   ProtocolMode

	mu      sync.Mutex
	tr      transport
	nextID  int64
	ready   bool
	closed  bool
	session ServerInfo
	// modern is set when the session settled on the modern era (server/discover
	// answered): requests then carry per-request metadata and no initialize
	// handshake happened.
	modern         bool
	onNotification func(Notification)
	// scope is the tool call in flight, against which the server's own requests
	// are answered; nil between calls.
	scope   *callScope
	scopeMu sync.Mutex
}

// Notification is a server-to-client notification: progress on a call, a log
// message, a list that changed. It carries no id and expects no reply.
type Notification struct {
	Method string
	Params json.RawMessage
}

// WithNotifications delivers the server's notifications to fn. fn runs on a
// transport goroutine (the stdio reader, an HTTP stream reader, or the goroutine
// waiting on a call's reply) and must return quickly; hand heavy work to another
// goroutine. It must not call back into this Client: a notification can arrive
// while a call holds the Client's lock, so a re-entrant call would deadlock.
// Without it notifications are dropped.
func WithNotifications(fn func(Notification)) Option {
	return func(c *Client) { c.onNotification = fn }
}

// Option configures a Client built by New.
type Option func(*Client)

// WithAnswering enables the server-initiated requests the policy names
// (elicitation, sampling, roots) and sets the input_required round bound. Without
// it nothing is advertised and nothing but ping is answered.
func WithAnswering(p AnswerPolicy) Option {
	return func(c *Client) { c.policy = p }
}

// New validates the config and builds a Client; nothing is sent until
// Initialize (or the first operation, which initializes lazily). Both value and
// pointer forms of the two config types are accepted.
func New(config any, opts ...Option) (*Client, error) {
	var c *Client
	switch cfg := config.(type) {
	case nil:
		return nil, fmt.Errorf("%w: nil", ErrUnsupportedConfig)
	case *MCPServerConfig:
		if cfg == nil {
			return nil, fmt.Errorf("%w: nil", ErrUnsupportedConfig)
		}
		if !cfg.ProtocolMode.valid() {
			return nil, fmt.Errorf("%w: unknown protocol mode %q", ErrUnsupportedConfig, cfg.ProtocolMode)
		}
		tr, err := newHTTPTransport(cfg)
		if err != nil {
			return nil, err
		}
		c = &Client{kind: kindHTTP, httpCfg: copyHTTPConfig(cfg), tr: tr, mode: cfg.ProtocolMode}
	case MCPServerConfig:
		return New(&cfg, opts...)
	case *MCPStdioConfig:
		if cfg == nil {
			return nil, fmt.Errorf("%w: nil", ErrUnsupportedConfig)
		}
		if !cfg.ProtocolMode.valid() {
			return nil, fmt.Errorf("%w: unknown protocol mode %q", ErrUnsupportedConfig, cfg.ProtocolMode)
		}
		if _, err := newStdioTransport(cfg); err != nil {
			return nil, err
		}
		c = &Client{kind: kindStdio, stdioCfg: copyStdioConfig(cfg), mode: cfg.ProtocolMode}
	case MCPStdioConfig:
		return New(&cfg, opts...)
	default:
		return nil, fmt.Errorf("%w: %T", ErrUnsupportedConfig, config)
	}
	for _, opt := range opts {
		opt(c)
	}
	if err := c.policy.validate(); err != nil {
		if c.tr != nil {
			c.tr.close()
		}
		return nil, err
	}
	return c, nil
}

func copyHTTPConfig(cfg *MCPServerConfig) *MCPServerConfig {
	cp := *cfg
	cp.Headers = make(map[string]string, len(cfg.Headers))
	for k, v := range cfg.Headers {
		cp.Headers[k] = v
	}
	cp.AllowedTools = append([]string(nil), cfg.AllowedTools...)
	cp.BlockedTools = append([]string(nil), cfg.BlockedTools...)
	return &cp
}

func copyStdioConfig(cfg *MCPStdioConfig) *MCPStdioConfig {
	cp := *cfg
	cp.Args = append([]string(nil), cfg.Args...)
	cp.Env = append([]string(nil), cfg.Env...)
	return &cp
}

// Initialize performs the handshake: the initialize request, the server's
// advertisement, and the notifications/initialized confirmation. It is
// idempotent — a second call returns the recorded info without another exchange.
func (c *Client) Initialize() (ServerInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ServerInfo{}, ErrClosed
	}
	if c.ready {
		return c.session, nil
	}
	return c.startSession(context.Background())
}

type initializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
}

// discoverResult is the server/discover answer of the modern era
// (mcp_types.DiscoverResult). The server's identity is not a field of its own:
// it travels as a display-only stamp in _meta.
type discoverResult struct {
	SupportedVersions []string       `json:"supportedVersions"`
	Capabilities      map[string]any `json:"capabilities"`
	Instructions      string         `json:"instructions"`
	Meta              struct {
		ServerInfo *struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"io.modelcontextprotocol/serverInfo"`
	} `json:"_meta"`
}

// modernProtocolVersions are the protocol revisions that use the stateless
// per-request envelope, oldest to newest (mcp_types.version.MODERN_PROTOCOL_VERSIONS).
// Exactly this list is accepted: a revision newer than the client knows is not
// assumed to behave like one it does.
var modernProtocolVersions = []string{"2026-07-28"}

// handshakeProtocolVersions are the revisions reachable through the initialize
// handshake (HANDSHAKE_PROTOCOL_VERSIONS).
var handshakeProtocolVersions = []string{"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25"}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// pickModernVersion returns the newest modern revision both sides speak, "" when
// there is none.
func pickModernVersion(supported []string) string {
	best := ""
	for _, v := range modernProtocolVersions {
		if containsString(supported, v) {
			best = v
		}
	}
	return best
}

// startSession opens the transport (spawning the subprocess for stdio), settles
// the protocol era and marks the client ready. The caller holds c.mu.
func (c *Client) startSession(ctx context.Context) (ServerInfo, error) {
	var tr transport
	switch c.kind {
	case kindHTTP:
		// Same transport, a fresh logical session: drop a stale session id so
		// initialize goes out unattributed.
		if ht, ok := c.tr.(*httpTransport); ok {
			ht.sessionID = ""
			ht.protocolVersion = ""
		}
		tr = c.tr
	case kindStdio:
		st, err := newStdioTransport(c.stdioCfg)
		if err != nil {
			return ServerInfo{}, err
		}
		if err := st.start(); err != nil {
			return ServerInfo{}, err
		}
		tr = st
	default:
		return ServerInfo{}, fmt.Errorf("%w: no transport", ErrUnsupportedConfig)
	}
	tr.setHandler(c.handleServerRequest)
	tr.setNotifier(c.onNotification)
	c.modern = false

	if c.mode == ProtocolAuto {
		info, ok, err := c.discover(ctx, tr)
		if err != nil {
			tr.close()
			return ServerInfo{}, err
		}
		if ok {
			c.tr, c.ready, c.session, c.modern = tr, true, info, true
			return info, nil
		}
	}

	c.nextID++
	res, err := tr.call(ctx, c.nextID, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    c.policy.capabilities(),
		"clientInfo":      map[string]any{"name": clientName, "version": clientVersion},
	})
	if err != nil {
		tr.close()
		return ServerInfo{}, err
	}
	var parsed initializeResult
	if err := json.Unmarshal(res, &parsed); err != nil {
		tr.close()
		return ServerInfo{}, fmt.Errorf("%w: initialize result: %v", ErrProtocol, err)
	}
	if err := tr.notify(ctx, "notifications/initialized", nil); err != nil {
		tr.close()
		return ServerInfo{}, err
	}
	// The standalone stream opens once the session is confirmed, as in the SDK.
	tr.startListener()
	// AGO-DEVIATION: the server's protocol version is recorded but never
	// negotiated further — the minimum client accepts whatever a server that
	// answered the 2024-11-05 offer speaks.
	info := ServerInfo{
		ProtocolVersion: parsed.ProtocolVersion,
		Capabilities:    parsed.Capabilities,
		Name:            parsed.ServerInfo.Name,
		Version:         parsed.ServerInfo.Version,
	}
	c.tr, c.ready, c.session = tr, true, info
	return info, nil
}

// discover probes server/discover (ProtocolAuto), stamped like any modern
// request: the _meta envelope plus the protocol-version and method headers, or a
// modern server refuses it. It reports ok=true when the server answered with a
// modern revision — the session is then ready and no handshake follows — and
// ok=false when the caller should fall back to the handshake. As in the SDK
// (mcp.client._probe.negotiate_auto) that is every error reply and every
// non-2xx status, an unparseable result, and a result advertising no modern
// revision; only the transport's own failures and a modern-only server sharing
// no revision with this client (-32022 naming no handshake revision) are errors.
//
// The SDK retries once at a mutual revision after -32022; with a single modern
// revision on each side the retry would repeat the same probe, so it is omitted.
func (c *Client) discover(ctx context.Context, tr transport) (ServerInfo, bool, error) {
	version := modernProtocolVersions[len(modernProtocolVersions)-1]
	tr.setProtocolVersion(version)
	c.nextID++
	res, err := tr.call(ctx, c.nextID, "server/discover", map[string]any{"_meta": c.envelope(version)})
	if err != nil {
		tr.setProtocolVersion("")
		var rpcErr *rpcError
		if errors.As(err, &rpcErr) && rpcErr.Code == codeUnsupportedProtocolVersion {
			var data struct {
				Supported []string `json:"supported"`
			}
			if json.Unmarshal(rpcErr.Data, &data) == nil && data.Supported != nil {
				legacy := false
				for _, v := range data.Supported {
					legacy = legacy || containsString(handshakeProtocolVersions, v)
				}
				if !legacy {
					return ServerInfo{}, false, err // modern-only and disjoint: a real incompatibility
				}
			}
		}
		if errors.Is(err, ErrProtocol) {
			return ServerInfo{}, false, nil
		}
		return ServerInfo{}, false, err
	}
	var parsed discoverResult
	if json.Unmarshal(res, &parsed) != nil {
		tr.setProtocolVersion("")
		return ServerInfo{}, false, nil
	}
	picked := pickModernVersion(parsed.SupportedVersions)
	if picked == "" {
		tr.setProtocolVersion("")
		return ServerInfo{}, false, nil
	}
	tr.setProtocolVersion(picked)
	info := ServerInfo{ProtocolVersion: picked, Capabilities: parsed.Capabilities}
	if si := parsed.Meta.ServerInfo; si != nil {
		info.Name, info.Version = si.Name, si.Version
	}
	return info, true, nil
}

// envelope is the modern era's per-request _meta: the era is stateless, so every
// request restates the revision, who the client is and what it answers
// (mcp.client.session._make_modern_stamp; keys from mcp_types).
func (c *Client) envelope(version string) map[string]any {
	return map[string]any{
		"io.modelcontextprotocol/protocolVersion":    version,
		"io.modelcontextprotocol/clientInfo":         map[string]any{"name": clientName, "version": clientVersion},
		"io.modelcontextprotocol/clientCapabilities": c.policy.capabilities(),
	}
}

// ensureSession makes the client usable, initializing lazily. The caller holds c.mu.
func (c *Client) ensureSession(ctx context.Context) error {
	if c.closed {
		return ErrClosed
	}
	if c.ready {
		return nil
	}
	_, err := c.startSession(ctx)
	return err
}

// request runs one JSON-RPC exchange over the current session.
func (c *Client) request(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requestLocked(ctx, method, params)
}

// requestLocked is request with c.mu already held: it rebuilds the session and
// retries once when the server reports it lost (HTTP 404).
func (c *Client) requestLocked(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if err := c.ensureSession(ctx); err != nil {
		return nil, err
	}
	c.nextID++
	res, err := c.tr.call(ctx, c.nextID, method, c.withMeta(params))
	if errors.Is(err, ErrTimeout) {
		// The exchange missed its deadline and the transport killed the
		// connection (a stdio subprocess, or an HTTP request cut mid-flight).
		// The session is broken: drop it so the next operation rebuilds one
		// instead of writing into dead pipes.
		c.tr.close()
		c.ready = false
		return res, err
	}
	if !errors.Is(err, errSessionExpired) {
		return res, err
	}
	c.tr.close()
	c.ready = false
	if _, err := c.startSession(ctx); err != nil {
		return nil, err
	}
	c.nextID++
	return c.tr.call(ctx, c.nextID, method, c.withMeta(params))
}

// withMeta adds the modern era's per-request metadata (a copy; the caller's
// map is never mutated).
func (c *Client) withMeta(params map[string]any) map[string]any {
	if !c.modern {
		return params
	}
	out := make(map[string]any, len(params)+1)
	for k, v := range params {
		out[k] = v
	}
	env := c.envelope(c.session.ProtocolVersion)
	if own, ok := params["_meta"].(map[string]any); ok {
		for k, v := range own { // the caller's keys (progressToken) ride beside the envelope
			if _, reserved := env[k]; !reserved {
				env[k] = v
			}
		}
	}
	out["_meta"] = env
	return out
}

// Request sends one JSON-RPC request the typed helpers do not cover —
// resources/list, resources/read, prompts/get and the like — and returns the raw
// result. A JSON-RPC error object is an error wrapping ErrProtocol whose text
// carries the code and message.
func (c *Client) Request(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return c.request(ctx, method, params)
}

// ListTools lists the server's tools, following the cursor pagination to the end
// (at most maxToolPages pages; a repeated cursor aborts with ErrProtocol).
func (c *Client) ListTools() ([]RemoteTool, error) {
	ctx := context.Background()
	var (
		out    []RemoteTool
		seen   = map[string]bool{}
		cursor string
	)
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := c.request(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var page struct {
			Tools      []RemoteTool `json:"tools"`
			NextCursor string       `json:"nextCursor"`
		}
		if err := json.Unmarshal(res, &page); err != nil {
			return nil, fmt.Errorf("%w: tools/list result: %v", ErrProtocol, err)
		}
		out = append(out, page.Tools...)
		if page.NextCursor == "" {
			return out, nil
		}
		if seen[page.NextCursor] {
			return nil, fmt.Errorf("%w: MCP server returned a repeated pagination cursor while listing tools", ErrProtocol)
		}
		if len(seen)+1 >= maxToolPages {
			return nil, fmt.Errorf("%w: MCP server did not end tools/list pagination within %d pages", ErrProtocol, maxToolPages)
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
}

type callResult struct {
	Content []map[string]any `json:"content"`
	IsError bool             `json:"isError"`
}

// CallTool invokes a remote tool by its server-side name. A result the server
// flagged isError=true is an error wrapping ErrToolCall; transport, JSON-RPC and
// content failures wrap ErrProtocol. Server requests that need a conversation (an
// elicitation for the human) cannot be answered here — use CallToolWith.
func (c *Client) CallTool(name string, args map[string]any) (ToolOutcome, error) {
	return c.CallToolWith(context.Background(), nil, name, args)
}

// CallToolWith is CallTool bound to a context and a conversation: ctx bounds the
// whole call, including the time spent answering the server's requests, and cx is
// where those requests are answered — an elicitation goes to cx.Input (the
// agent's HITL hook) and sampling falls back to the model config cx carries. A
// server request that cannot be answered fails the call with ErrAnswering; a
// server that keeps replying input_required fails it with ErrMaxRounds.
func (c *Client) CallToolWith(ctx context.Context, cx *agocontext.Context, name string, args map[string]any) (ToolOutcome, error) {
	return c.CallToolMeta(ctx, cx, name, args, nil)
}

// CallToolMeta is CallToolWith with request metadata: meta travels as the call's
// params._meta, which is where a progressToken goes to ask the server for
// progress notifications on this call (delivered through WithNotifications).
func (c *Client) CallToolMeta(ctx context.Context, cx *agocontext.Context, name string, args map[string]any, meta map[string]any) (ToolOutcome, error) {
	if args == nil {
		args = map[string]any{}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	scope := &callScope{cx: cx}
	c.scopeMu.Lock()
	c.scope = scope
	c.scopeMu.Unlock()
	defer func() {
		c.scopeMu.Lock()
		c.scope = nil
		c.scopeMu.Unlock()
	}()

	params := map[string]any{"name": name, "arguments": args}
	if len(meta) > 0 {
		params["_meta"] = meta
	}
	rounds := 0
	backoff := stateOnlyBackoffInitial
	for {
		res, err := c.requestLocked(ctx, "tools/call", params)
		if scope.failed() != nil {
			// A request the server made could not be answered; however the
			// server went on, the call is lost (Python parity: the ErrorData
			// aborts the call).
			return ToolOutcome{}, scope.failed()
		}
		if err != nil {
			return ToolOutcome{}, err
		}
		ir, asks := asInputRequired(res)
		if !asks {
			return decodeCallResult(res)
		}
		// A bound on the rounds, so a server that re-asks the same thing
		// forever ends the call with an error naming that.
		if rounds >= c.policy.maxRounds() {
			return ToolOutcome{}, fmt.Errorf("%w: still asking after %d rounds", ErrMaxRounds, rounds)
		}
		rounds++
		// Retry the original call carrying the answers and the server's state,
		// echoed byte-exact and never inspected. A round that carries only a
		// state token has nothing to answer: back off (50ms doubling to 250ms)
		// and retry without inputResponses, as the SDK does.
		params = map[string]any{"name": name, "arguments": args}
		if len(meta) > 0 {
			params["_meta"] = meta
		}
		if len(ir.InputRequests) > 0 {
			backoff = stateOnlyBackoffInitial
			responses, err := c.answerInputRequests(ctx, ir)
			if err != nil {
				return ToolOutcome{}, err
			}
			params["inputResponses"] = responses
		} else {
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ToolOutcome{}, ctx.Err()
			}
			backoff = min(backoff*2, stateOnlyBackoffCap)
		}
		if ir.RequestState != "" {
			params["requestState"] = ir.RequestState
		}
	}
}

const (
	stateOnlyBackoffInitial = 50 * time.Millisecond
	stateOnlyBackoffCap     = 250 * time.Millisecond
)

func decodeCallResult(res json.RawMessage) (ToolOutcome, error) {
	var result callResult
	if err := json.Unmarshal(res, &result); err != nil {
		return ToolOutcome{}, fmt.Errorf("%w: tools/call result: %v", ErrProtocol, err)
	}
	if result.IsError {
		return ToolOutcome{}, fmt.Errorf("%w: %s", ErrToolCall, errorText(result.Content, res))
	}
	parts, err := mapContent(result.Content, res)
	if err != nil {
		return ToolOutcome{}, err
	}
	return ToolOutcome{Parts: parts, Raw: res}, nil
}

// Close tears the session down (terminating a stdio subprocess). Idempotent;
// operations afterwards report ErrClosed.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	c.ready = false
	if c.tr != nil {
		return c.tr.close()
	}
	return nil
}

// endpoint names which server this client reaches, without secrets — the Source
// stamp parity ("mcp:<endpoint>:<tool>"). A stdio server is its command plus a
// digest of its arguments; an HTTP server is its URL minus credentials and query.
func (c *Client) endpoint() string {
	if c.kind == kindStdio {
		sum := sha256.Sum256([]byte(strings.Join(c.stdioCfg.Args, "\x00")))
		return fmt.Sprintf("%s#%s", c.stdioCfg.Command, hex.EncodeToString(sum[:6]))
	}
	u, err := parseAbsoluteURL(c.httpCfg.URL)
	if err != nil {
		return c.httpCfg.URL
	}
	return u.Scheme + "://" + u.Host + u.Path
}

// allowedTools returns the allow-list as a set (nil = every tool passes).
func (c *Client) allowedTools() map[string]bool {
	var names []string
	if c.kind == kindStdio {
		names = c.stdioCfg.AllowedTools
	} else {
		names = c.httpCfg.AllowedTools
	}
	if names == nil {
		return nil
	}
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set
}

// blockedTools returns the block-list as a set.
func (c *Client) blockedTools() map[string]bool {
	var names []string
	if c.kind == kindStdio {
		names = c.stdioCfg.BlockedTools
	} else {
		names = c.httpCfg.BlockedTools
	}
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set
}

// toolNamePrefix returns the local name prefix.
func (c *Client) toolNamePrefix() string {
	if c.kind == kindHTTP {
		return c.httpCfg.ToolNamePrefix
	}
	return c.stdioCfg.ToolNamePrefix
}
