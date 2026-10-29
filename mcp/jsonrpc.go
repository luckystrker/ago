package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Package-level sentinels. Match with errors.Is.
var (
	// ErrUnsupportedConfig reports a New() config value that is neither
	// MCPServerConfig nor MCPStdioConfig (value or pointer).
	ErrUnsupportedConfig = errors.New("mcp: unsupported server config")
	// ErrNotProvided reports an incomplete config: an HTTP config without a URL
	// or a stdio config without a command.
	ErrNotProvided = errors.New("mcp: config value not provided")
	// ErrProtocol reports a wire-level failure: malformed JSON-RPC, an error
	// object from the server, a broken content block, or a runaway listing.
	ErrProtocol = errors.New("mcp: protocol error")
	// ErrToolCall reports a tools/call the server answered with isError=true.
	ErrToolCall = errors.New("mcp: remote tool failed")
	// ErrClosed reports an operation on a closed Client.
	ErrClosed = errors.New("mcp: client is closed")
)

// errSessionExpired marks a transport whose server-side session is gone (HTTP 404);
// the Client rebuilds the session and retries once. It never escapes the package.
var errSessionExpired = errors.New("mcp: session expired")

const protocolVersion = "2024-11-05"

// clientInfo identifies AGo in the initialize handshake.
const clientName = "ago-mcp-client"
const clientVersion = "0.1.0"

// --- JSON-RPC 2.0 wire shapes -------------------------------------------------

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcResponse is one reply carrying a result or an error object.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if len(e.Data) > 0 {
		return fmt.Sprintf("code %d: %s: %s", e.Code, e.Message, string(e.Data))
	}
	return fmt.Sprintf("code %d: %s", e.Code, e.Message)
}

// wireMessage is the discriminating shape used while scanning a stream of
// incoming lines (stdio) or SSE events (HTTP): a message with a "method" is a
// notification or a server-to-client request — both skipped, the minimum client
// answers none — and one with an id is awaited or ignored by id.
type wireMessage struct {
	ID     *int64          `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// decodeResponse parses one JSON-RPC reply and unwraps its result. A reply whose
// id does not match the awaited one is a protocol violation.
func decodeResponse(data []byte, wantID int64) (json.RawMessage, error) {
	var resp rpcResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("%w: malformed response: %v", ErrProtocol, err)
	}
	if resp.ID == nil {
		return nil, fmt.Errorf("%w: response without id", ErrProtocol)
	}
	if *resp.ID != wantID {
		return nil, fmt.Errorf("%w: response id %d, want %d", ErrProtocol, *resp.ID, wantID)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("%w: %s", ErrProtocol, resp.Error.Error())
	}
	return resp.Result, nil
}
