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
// brief pins the signatures); they run on context.Background().
type Client struct {
	kind     configKind
	httpCfg  *MCPServerConfig
	stdioCfg *MCPStdioConfig

	mu      sync.Mutex
	tr      transport
	nextID  int64
	ready   bool
	closed  bool
	session ServerInfo
}

// New validates the config and builds a Client; nothing is sent until
// Initialize (or the first operation, which initializes lazily). Both value and
// pointer forms of the two config types are accepted.
func New(config any) (*Client, error) {
	switch cfg := config.(type) {
	case nil:
		return nil, fmt.Errorf("%w: nil", ErrUnsupportedConfig)
	case *MCPServerConfig:
		if cfg == nil {
			return nil, fmt.Errorf("%w: nil", ErrUnsupportedConfig)
		}
		tr, err := newHTTPTransport(cfg)
		if err != nil {
			return nil, err
		}
		return &Client{kind: kindHTTP, httpCfg: copyHTTPConfig(cfg), tr: tr}, nil
	case MCPServerConfig:
		return New(&cfg)
	case *MCPStdioConfig:
		if cfg == nil {
			return nil, fmt.Errorf("%w: nil", ErrUnsupportedConfig)
		}
		if _, err := newStdioTransport(cfg); err != nil {
			return nil, err
		}
		return &Client{kind: kindStdio, stdioCfg: copyStdioConfig(cfg)}, nil
	case MCPStdioConfig:
		return New(&cfg)
	default:
		return nil, fmt.Errorf("%w: %T", ErrUnsupportedConfig, config)
	}
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

// startSession opens the transport (spawning the subprocess for stdio), runs the
// handshake and marks the client ready. The caller holds c.mu.
func (c *Client) startSession(ctx context.Context) (ServerInfo, error) {
	var tr transport
	switch c.kind {
	case kindHTTP:
		// Same transport, a fresh logical session: drop a stale session id so
		// initialize goes out unattributed.
		if ht, ok := c.tr.(*httpTransport); ok {
			ht.sessionID = ""
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

	c.nextID++
	res, err := tr.call(ctx, c.nextID, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
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

// request runs one JSON-RPC exchange over the current session, rebuilding the
// session and retrying once when the server reports it lost (HTTP 404).
func (c *Client) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureSession(ctx); err != nil {
		return nil, err
	}
	c.nextID++
	res, err := c.tr.call(ctx, c.nextID, method, params)
	if !errors.Is(err, errSessionExpired) {
		return res, err
	}
	c.tr.close()
	c.ready = false
	if _, err := c.startSession(ctx); err != nil {
		return nil, err
	}
	c.nextID++
	return c.tr.call(ctx, c.nextID, method, params)
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

type wireCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type callResult struct {
	Content []map[string]any `json:"content"`
	IsError bool             `json:"isError"`
}

// CallTool invokes a remote tool by its server-side name. A result the server
// flagged isError=true is an error wrapping ErrToolCall; transport, JSON-RPC and
// content failures wrap ErrProtocol.
func (c *Client) CallTool(name string, args map[string]any) (ToolOutcome, error) {
	if args == nil {
		args = map[string]any{}
	}
	ctx := context.Background()
	res, err := c.request(ctx, "tools/call", wireCallParams{Name: name, Arguments: args})
	if err != nil {
		return ToolOutcome{}, err
	}
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
