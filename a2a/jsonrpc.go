package a2a

import (
	"encoding/json"
	"fmt"
)

// JSON-RPC 2.0 envelope for the AGo a2a package. Kept local (not shared with
// ago/mcp) so the two protocols stay independently changeable.

// Method names of the JSON-RPC surface served by Server. The push-config
// names are the wire spellings of the v0.3 surface (a2a-sdk
// src/a2a/compat/v0_3/jsonrpc_adapter.py:60-63), kept consistent with the
// message/send-style names this package already serves; the 1.x dispatcher
// renames them to the gRPC-derived CreateTaskPushNotificationConfig family.
const (
	MethodMessageSend   = "message/send"
	MethodMessageStream = "message/stream"
	MethodTasksGet      = "tasks/get"
	MethodTasksCancel   = "tasks/cancel"

	MethodPushSet    = "tasks/pushNotificationConfig/set"
	MethodPushGet    = "tasks/pushNotificationConfig/get"
	MethodPushList   = "tasks/pushNotificationConfig/list"
	MethodPushDelete = "tasks/pushNotificationConfig/delete"
)

// JSON-RPC error codes. -32700..-32603 are the JSON-RPC 2.0 standard codes;
// -32001/-32002/-32004 are the A2A server-error range (task not found, task
// not cancelable, unsupported operation) as used by the a2a-sdk transports
// the Python side delegates to.
const (
	codeParseError           = -32700
	codeInvalidRequest       = -32600
	codeMethodNotFound       = -32601
	codeInvalidParams        = -32602
	codeInternalError        = -32603
	codeTaskNotFound         = -32001
	codeTaskNotCancelable    = -32002
	codeUnsupportedOperation = -32004
)

// rpcRequest is one incoming call. ID and Params stay raw: the server echoes
// the id back byte-for-byte and re-unmarshals params per method.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// rpcResponse is one reply carrying a result or an error object. A nil Result
// is omitted so error responses carry only "error".
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is the error object of a JSON-RPC reply. The client surfaces it as
// a Go error matched by errors.Is against ErrTaskNotFound, ErrTaskNotCancelable
// or ErrRPC.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error implements error.
func (e *RPCError) Error() string {
	if len(e.Data) > 0 {
		return fmt.Sprintf("code %d: %s: %s", e.Code, e.Message, string(e.Data))
	}
	return fmt.Sprintf("code %d: %s", e.Code, e.Message)
}

// idOr echoes the request id, or JSON null when the request carried none.
func idOr(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}
