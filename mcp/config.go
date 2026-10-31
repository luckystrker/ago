package mcp

import "time"

// Configs for reaching an MCP server: over streamable HTTP (MCPServerConfig) or a
// local subprocess speaking newline-delimited JSON-RPC on its stdio pipes
// (MCPStdioConfig). Parity with Python ag2/tools/toolkits/mcp_server/types.py,
// reduced to the fields the AGo minimum consumes (AGo configs are plain values —
// no Variable indirection, no protocol-era negotiation).

// MCPServerConfig reaches a remote MCP server over HTTP: one JSON-RPC 2.0 request
// per POST, answers accepted as application/json or an SSE stream.
type MCPServerConfig struct {
	// URL is the full server endpoint, scheme included.
	URL string
	// Headers travel on every request (custom auth, tracing, ...).
	Headers map[string]string
	// AuthorizationToken is sent as "Authorization: Bearer <token>" unless Headers
	// already carries an Authorization entry (case-insensitive).
	AuthorizationToken string
	// ServerLabel names the server for diagnostics; it never reaches the wire.
	ServerLabel string
	// AllowedTools lists the server-side tool names to expose; empty means all.
	AllowedTools []string
	// BlockedTools hides server-side tool names, applied after AllowedTools.
	BlockedTools []string
	// ToolNamePrefix is put in front of the local tool names so two servers
	// exposing the same generic name do not collide. The server never sees it:
	// filters and the outbound call use the server's own names.
	ToolNamePrefix string
	// Timeout bounds one HTTP exchange — connect, request, and reading the
	// response (Python parity: connection_timeout, default 30s). Zero means
	// the default; a negative value removes the cap.
	Timeout time.Duration
}

// MCPStdioConfig launches a local MCP server as a subprocess and speaks
// newline-delimited JSON-RPC over its stdin/stdout (no Content-Length framing).
type MCPStdioConfig struct {
	// Command is the executable to launch.
	Command string
	// Args are passed to the executable.
	Args []string
	// Env is the subprocess environment in os/exec "KEY=VALUE" form; nil or empty
	// inherits this process's environment (os/exec semantics).
	Env []string
	// CWD is the subprocess working directory; empty inherits the parent's.
	CWD string
	// Timeout bounds one exchange — writing the request line and waiting for
	// the matching reply. Zero means the default (30s, the same cap the HTTP
	// branch gets via connection_timeout); a negative value removes the cap.
	// AGO-DEVIATION: Python's MCPStdioServerConfig has no timeout — a hung
	// subprocess there blocks the tool call forever; AGo fails the exchange,
	// kills the subprocess and rebuilds the session on the next operation.
	Timeout time.Duration
	// AllowedTools lists the server-side tool names to expose; nil means all.
	// AGO-DEVIATION: the brief's struct sketch omitted the filter fields, but
	// Tools() must filter both transports — they match Python's
	// MCPStdioServerConfig (allowed_tools/blocked_tools/tool_name_prefix).
	AllowedTools []string
	// BlockedTools hides server-side tool names, applied after AllowedTools.
	BlockedTools []string
	// ToolNamePrefix namespaces the local tool names; the server never sees it.
	ToolNamePrefix string
}

// configKind tags which transport a Client uses.
type configKind int

const (
	kindNone configKind = iota
	kindHTTP
	kindStdio
)
