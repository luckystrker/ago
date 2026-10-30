package acp

import (
	"errors"
	"fmt"
	"sync"

	"github.com/luckystrker/ago/mcp"
)

// MCPServer is one entry of the mcpServers a client may declare in
// session/new (Python parity: the acp schema McpServer — HTTP and stdio
// transports; the sse/acp transports of the SDK are deferred). A server named
// by a client is context, not authorization: nothing is dialed until the
// session's Gateway is used (agent.py new_session).
type MCPServer struct {
	// Name is the client's label for the server; the gateway routes tool
	// calls by it.
	Name string `json:"name"`
	// URL names an HTTP (streamable) MCP server endpoint, scheme included.
	URL string `json:"url,omitempty"`
	// Headers travel on every HTTP request (custom auth, tracing, ...).
	Headers map[string]string `json:"headers,omitempty"`
	// Command launches a stdio MCP server as a local subprocess.
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	// Env adds environment variables for the stdio subprocess (on top of the
	// inherited environment).
	Env map[string]string `json:"env,omitempty"`
}

// ErrServerNotFound reports a tool call naming an MCP server the peer never
// declared.
var ErrServerNotFound = errors.New("acp: no such MCP server")

// ErrServerNotDeclaredable reports a Bridge-side session/new declaring an MCP
// server the subset cannot announce to a subprocess agent.
var ErrUndeclarableServer = errors.New("acp: MCP server cannot be declared to an ACP agent")

// ToolGateway proxies tool calls to client-declared MCP servers through the
// ago/mcp client — one connection per declared server, dialed lazily on the
// first call that names it (Python parity: tool_gateway.py, reduced to the
// consume direction: the subset has no in-process HTTP server to expose local
// tools to the agent, docs/DECISIONS.md D-066).
//
// A ToolGateway is safe for concurrent use; operations serialize per server
// (one exchange in flight per ago/mcp client). Close it when the session is
// done — Serve tears its sessions' gateways down with the connection; a
// standalone gateway's owner calls Close.
type ToolGateway struct {
	mu      sync.Mutex
	clients map[string]*mcp.Client
	servers map[string]MCPServer
	closed  bool
}

// NewToolGateway builds a gateway over the declared servers. Nothing is
// dialed here: declarations are recorded, the connection happens on the first
// call — a client's declaration must not spawn subprocesses on the agent's
// behalf until the agent's handler asks for one.
func NewToolGateway(servers []MCPServer) *ToolGateway {
	g := &ToolGateway{
		clients: map[string]*mcp.Client{},
		servers: map[string]MCPServer{},
	}
	for _, s := range servers {
		if s.Name != "" {
			g.servers[s.Name] = s
		}
	}
	return g
}

// client returns the (lazily dialed) client for server. The caller holds g.mu.
func (g *ToolGateway) client(server string) (*mcp.Client, error) {
	if g.closed {
		return nil, ErrClosed
	}
	decl, ok := g.servers[server]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrServerNotFound, server)
	}
	if c, ok := g.clients[server]; ok {
		return c, nil
	}
	var (
		c   *mcp.Client
		err error
	)
	if decl.URL != "" {
		c, err = mcp.New(&mcp.MCPServerConfig{URL: decl.URL, Headers: decl.Headers})
	} else {
		c, err = mcp.New(&mcp.MCPStdioConfig{Command: decl.Command, Args: decl.Args, Env: envList(decl.Env)})
	}
	if err != nil {
		return nil, fmt.Errorf("acp: MCP server %q: %w", server, err)
	}
	g.clients[server] = c
	return c, nil
}

// envList renders the declared env map in os/exec "KEY=VALUE" form, on top of
// the inherited environment (Python parity: {**os.environ, **env}).
func envList(env map[string]string) []string {
	var out []string
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// Call invokes tool on the named server with args and returns the result's
// text content (the ago/mcp outcome's text parts concatenated). A result the
// server flagged as an error, and transport/JSON-RPC failures, come back as
// errors wrapping the ago/mcp sentinels.
//
// AGO-DEVIATION: no context.Context — the ago/mcp client's operations carry
// none (pinned there), so the gateway cannot thread one through.
func (g *ToolGateway) Call(server, tool string, args map[string]any) (string, error) {
	g.mu.Lock()
	c, err := g.client(server)
	g.mu.Unlock()
	if err != nil {
		return "", err
	}
	outcome, err := c.CallTool(tool, args)
	if err != nil {
		return "", fmt.Errorf("acp: MCP server %q tool %s: %w", server, tool, err)
	}
	return partsText(outcome.Parts), nil
}

// Servers lists the declared server names, in declaration order.
func (g *ToolGateway) Servers() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, 0, len(g.servers))
	for name := range g.servers {
		out = append(out, name)
	}
	return out
}

// Close tears every dialed connection down (terminating stdio subprocesses).
// Idempotent; calls afterwards report ErrClosed.
func (g *ToolGateway) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	g.closed = true
	for _, c := range g.clients {
		// Teardown noise is discarded — the connections are going away either way.
		_ = c.Close()
	}
	g.clients = map[string]*mcp.Client{}
}

// declarable validates an MCPServer the Bridge side may announce in
// session/new: the ACP schema's HTTP transport. A stdio entry names a command
// only the declarer can run — a subprocess agent cannot dial it, so it is
// refused rather than silently useless (Python parity: session.ensure takes
// acp.schema.HttpMcpServer entries only).
func declarable(s MCPServer) error {
	if s.Name == "" {
		return fmt.Errorf("%w: an MCP server needs a name", ErrUndeclarableServer)
	}
	if s.URL == "" {
		return fmt.Errorf("%w: %q has no URL (only HTTP MCP servers can be declared)", ErrUndeclarableServer, s.Name)
	}
	return nil
}

// declaredServers is SessionNewWithServers' params shape.
type declaredServersParams struct {
	CWD        string      `json:"cwd"`
	MCPServers []MCPServer `json:"mcpServers,omitempty"`
}
