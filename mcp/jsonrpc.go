package mcp

import (
	"context"
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
	// ErrTimeout reports an exchange that exceeded its deadline: the transport
	// tears the connection down and the session must be re-established.
	ErrTimeout = errors.New("mcp: exchange timed out")
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
// incoming lines (stdio) or SSE events (HTTP): a message with a "method" and an
// id is a server-to-client request (answered, see requestHandler), one with a
// method and no id is a notification (skipped), and one with an id and no
// method is the reply to a request of ours.
type wireMessage struct {
	// ID stays raw: a server may pick string ids for its own requests, and the
	// reply must echo exactly what it sent.
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// hasID reports whether the message carries a non-null id.
func (m wireMessage) hasID() bool {
	return len(m.ID) > 0 && string(m.ID) != "null"
}

// isRequest reports a server-to-client request: a method plus an id.
func (m wireMessage) isRequest() bool { return m.Method != "" && m.hasID() }

// replyTo reports whether the message is the reply (result or error) to the
// request with the given id.
func (m wireMessage) replyTo(id int64) bool {
	if m.Method != "" || !m.hasID() {
		return false
	}
	var got int64
	if err := json.Unmarshal(m.ID, &got); err != nil {
		return false
	}
	return got == id
}

// outcome unwraps a reply's result or error.
func (m wireMessage) outcome() (json.RawMessage, error) {
	if m.Error != nil {
		// Both wrapped: callers match ErrProtocol, and the era negotiation
		// reads the error code through errors.As.
		return nil, fmt.Errorf("%w: %w", ErrProtocol, m.Error)
	}
	return m.Result, nil
}

// requestHandler answers one server-to-client request: a result to send back,
// or a JSON-RPC error object. It runs on the goroutine waiting for the reply to
// the client's own request, with that exchange's context.
type requestHandler func(ctx context.Context, method string, params json.RawMessage) (any, *rpcError)

// JSON-RPC error codes used when answering a server.
const (
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInvalidRequest = -32600
	codeInternalError  = -32603
	// codeUnsupportedProtocolVersion answers a modern-era request whose
	// protocol version the server does not speak; its data names the versions
	// the server does (mcp_types.jsonrpc.UNSUPPORTED_PROTOCOL_VERSION).
	codeUnsupportedProtocolVersion = -32022
	// codeHeaderMismatch answers a modern-era request whose HTTP headers
	// disagree with its body (mcp_types.jsonrpc.HEADER_MISMATCH).
	codeHeaderMismatch = -32020
)

// rpcResultReply and rpcErrorReply are the two shapes of a client reply to a
// server-initiated request.
type rpcResultReply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
}

type rpcErrorReply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   *rpcError       `json:"error"`
}

// answerRequest runs the handler (method-not-found when none is installed) and
// builds the reply message for the request.
func answerRequest(ctx context.Context, h requestHandler, msg wireMessage) any {
	if h == nil {
		return rpcErrorReply{JSONRPC: "2.0", ID: msg.ID, Error: &rpcError{Code: codeMethodNotFound, Message: "method not found: " + msg.Method}}
	}
	result, rpcErr := h(ctx, msg.Method, msg.Params)
	if rpcErr != nil {
		return rpcErrorReply{JSONRPC: "2.0", ID: msg.ID, Error: rpcErr}
	}
	if result == nil {
		result = map[string]any{}
	}
	return rpcResultReply{JSONRPC: "2.0", ID: msg.ID, Result: result}
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
