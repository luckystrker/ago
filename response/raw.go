package response

import (
	agocontext "github.com/luckystrker/ago/context"
)

// RawSchemaImpl wraps a ready-made JSON schema. It performs no validation: Validate
// returns the raw response string unchanged (Python parity: RawSchema warns and passes
// the text through — validate the content yourself).
type RawSchemaImpl struct {
	name        string
	description string
	schema      map[string]any
}

// RawOption customizes a RawSchemaImpl.
type RawOption func(*RawSchemaImpl)

// RawSchema wraps schema. The name defaults to "ResponseSchema" (Python parity).
func RawSchema(schema map[string]any, opts ...RawOption) *RawSchemaImpl {
	r := &RawSchemaImpl{name: "ResponseSchema", schema: schema}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// WithRawName sets the schema name.
func WithRawName(name string) RawOption {
	return func(r *RawSchemaImpl) { r.name = name }
}

// WithRawDescription sets the schema description.
func WithRawDescription(description string) RawOption {
	return func(r *RawSchemaImpl) { r.description = description }
}

func (r *RawSchemaImpl) Name() string        { return r.name }
func (r *RawSchemaImpl) Description() string { return r.description }

// JSONSchema returns a deep copy of the wrapped schema (mutation-safe sharing).
func (r *RawSchemaImpl) JSONSchema() map[string]any { return copySchema(r.schema) }

// SystemPrompt is empty: raw schemas rely on provider-native structured output.
func (r *RawSchemaImpl) SystemPrompt() string { return "" }

// Validate performs no validation and returns the response as-is.
func (r *RawSchemaImpl) Validate(response string, ctx *agocontext.Context) (any, error) {
	return response, nil
}
