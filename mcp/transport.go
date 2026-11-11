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
	// setHandler installs the answerer for server-to-client requests that arrive
	// while a call waits for its reply. Without one, they get "method not found".
	setHandler(h requestHandler)
	// setProtocolVersion records the negotiated protocol revision for transports
	// that announce it on every request (the HTTP MCP-Protocol-Version header);
	// "" clears it.
	setProtocolVersion(version string)
	// setNotifier installs the receiver of server notifications (progress, log
	// messages, list changes); nil drops them. It may be called from another
	// goroutine than the one that called into the transport, so it must not block.
	setNotifier(fn func(Notification))
	// startListener opens the server's standalone stream for messages outside
	// any request, where the transport has one (streamable HTTP); a no-op elsewhere.
	startListener()
	// close releases the connection; it must be safe to call more than once.
	close() error
}
