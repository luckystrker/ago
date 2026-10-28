// Package response carries the response-schema contract: how an agent constrains and
// validates model answers. A Proto describes the contract (name, description, JSON
// schema, system-prompt fragment) and validates a raw model response against it.
//
// Parity with Python AG2 ag2/response/ (proto.py, schema.py, prompted.py, decision.py).
// The reflection-based JSON-schema generator (SchemaOf) is shared with package tools
// for function-parameter schemas. See docs/specs/core.md §2 ("Response schemas") and
// docs/DECISIONS.md D-007 (Validate drops the Python provider argument).
package response

import (
	"errors"

	agocontext "ago/context"
)

// Proto is the response-schema protocol every response schema implements (Python
// ResponseProto). Implementations are immutable after construction and safe for
// concurrent use.
type Proto interface {
	Name() string
	Description() string
	// JSONSchema returns the JSON schema providers use for native structured output.
	// An empty (or nil) result means "no native schema"; callers must treat the
	// returned map as read-only.
	JSONSchema() map[string]any
	// SystemPrompt is the prompt fragment instructing the model about the schema;
	// empty when none.
	SystemPrompt() string
	// Validate parses response and checks it against the schema; the returned any
	// holds the parsed value. ctx is the conversation the response belongs to; the
	// builtin schemas ignore it (D-007: the Python provider argument is dropped).
	Validate(response string, ctx *agocontext.Context) (any, error)
}

// Compile-time interface conformance.
var (
	_ Proto = (*Schema[int])(nil)
	_ Proto = (*RawSchemaImpl)(nil)
	_ Proto = (*PromptedSchema)(nil)
)

// ErrInvalidResponse is wrapped by every ValidationError; match with errors.Is when the
// concrete error type does not matter.
var ErrInvalidResponse = errors.New("response: model response does not match the schema")

// ValidationError reports a model response that failed schema validation. The agent
// surfaces Err's text back to the model when re-asking (the retry loop).
type ValidationError struct {
	Err      error  // underlying reason: JSON syntax, type mismatch, missing field, bad enum value
	Response string // the rejected raw response
}

func (e *ValidationError) Error() string {
	if e.Err == nil {
		return ErrInvalidResponse.Error()
	}
	return ErrInvalidResponse.Error() + ": " + e.Err.Error()
}

// Unwrap exposes both the sentinel and the underlying reason to errors.Is/As.
func (e *ValidationError) Unwrap() []error {
	if e.Err == nil {
		return []error{ErrInvalidResponse}
	}
	return []error{ErrInvalidResponse, e.Err}
}
