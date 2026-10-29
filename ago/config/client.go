package config

import (
	"errors"

	"ago/response"
)

// CallOptions carries the per-call extras of one model call.
type CallOptions struct {
	// Tools are the schemas the model may call: tools.Schema values (function
	// tools) or tools.ToolSchema implementations (hosted tools). Providers map
	// what they support and fail fast on the rest (ErrUnsupportedTool).
	Tools []any
	// ResponseSchema is the structured-output contract; nil = plain text.
	// Providers with native support send it as the API's response format,
	// others rely on its SystemPrompt.
	ResponseSchema response.Proto
}

// Errors reported by model configs and clients. Match with errors.Is.
var (
	// ErrNotProvided reports a missing required config value (model, API key,
	// base URL) — Python parity: ConfigNotProvidedError.
	ErrNotProvided = errors.New("config: not provided")
	// ErrUnsupportedInput reports an input part the provider cannot carry —
	// Python parity: UnsupportedInputError. Fail fast, never silently dropped.
	ErrUnsupportedInput = errors.New("config: unsupported input")
	// ErrUnsupportedTool reports a tool schema the provider cannot carry —
	// Python parity: UnsupportedToolError.
	ErrUnsupportedTool = errors.New("config: unsupported tool")
	// ErrNotImplemented reports a provider or feature whose client has not
	// been ported yet (stub configs return it from Create()).
	ErrNotImplemented = errors.New("config: not implemented")
	// ErrStreamError reports an error the API delivered inside a successful
	// SSE stream — an Anthropic `error` event or a Chat-Completions
	// {"error": ...} chunk. Python parity: the SDK's stream iterator raises
	// the provider's typed exception, failing the call instead of ending the
	// stream as a successful empty response.
	ErrStreamError = errors.New("config: stream error")
)
