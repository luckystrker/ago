package mcp

import (
	"context"
	"encoding/json"
)

// transport is one open connection to an MCP server: it carries requests whose
// ids the Client allocates, sends notifications, and tears the connection down.
// Implementations must be safe for serial use (the Client serializes operations
// under its lock).
type transport interface {
	// call sends one request and returns the raw "result" of its reply. A JSON-RPC
	// error object or any wire violation is an error (wrapping ErrProtocol); a
	// server-side session loss is errSessionExpired.
	call(ctx context.Context, id int64, method string, params any) (json.RawMessage, error)
	// notify sends a notification (no id, no reply expected).
	notify(ctx context.Context, method string, params any) error
	// close releases the connection; it must be safe to call more than once.
	close() error
}
