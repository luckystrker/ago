package response

import (
	agocontext "github.com/luckystrker/ago/context"
)

// PromptedSchema drives structured output through the system prompt instead of the
// provider's native response-format support: JSONSchema() is nil (so providers skip
// native structured output) and SystemPrompt() carries the schema instructions. Use it
// for models or providers without built-in structured output.
//
// Validation is delegated to the wrapped schema (parse only).
type PromptedSchema struct {
	inner        Proto
	systemPrompt string
}

// PromptedOption customizes a PromptedSchema.
type PromptedOption func(*PromptedSchema)

// NewPromptedSchema wraps inner (a *Schema[T], RawSchemaImpl or any other Proto).
// Returns nil when inner is nil.
func NewPromptedSchema(inner Proto, opts ...PromptedOption) *PromptedSchema {
	if inner == nil {
		return nil
	}
	p := &PromptedSchema{
		inner:        inner,
		systemPrompt: renderSystemPrompt(inner.JSONSchema()),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// WithPromptTemplate replaces the default instruction template; "{schema}" receives the
// schema JSON. An empty schema yields an empty system prompt regardless.
func WithPromptTemplate(template string) PromptedOption {
	return func(p *PromptedSchema) {
		p.systemPrompt = applyPromptTemplate(template, p.inner.JSONSchema())
	}
}

func (p *PromptedSchema) Name() string        { return p.inner.Name() }
func (p *PromptedSchema) Description() string { return p.inner.Description() }

// JSONSchema is nil: structured output is prompt-driven (Python parity).
func (p *PromptedSchema) JSONSchema() map[string]any { return nil }

func (p *PromptedSchema) SystemPrompt() string { return p.systemPrompt }

// Validate delegates to the wrapped schema.
func (p *PromptedSchema) Validate(response string, ctx *agocontext.Context) (any, error) {
	return p.inner.Validate(response, ctx)
}
