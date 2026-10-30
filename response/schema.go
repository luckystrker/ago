package response

import (
	"encoding/json"
	"reflect"
	"strings"

	agocontext "github.com/luckystrker/ago/context"
)

// defaultPromptTemplate is the canonical JSON-schema instruction (Python
// PromptedSchema's default template). "{schema}" receives the schema JSON.
const defaultPromptTemplate = "You must respond with valid JSON that conforms to the following JSON schema:\n```json\n{schema}\n```\nDo not include any text, markdown formatting, or explanation outside the JSON object."

func renderSystemPrompt(schema map[string]any) string {
	return applyPromptTemplate(defaultPromptTemplate, schema)
}

func applyPromptTemplate(template string, schema map[string]any) string {
	if len(schema) == 0 {
		return ""
	}
	b, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(template, "{schema}", string(b))
}

// Schema is a response schema whose JSON schema is generated from the Go type T by
// reflection (see SchemaOf). Validate parses the model response into T; failures come
// back as *ValidationError. A Schema is immutable after construction and safe for
// concurrent use.
//
// If T contains a type the generator cannot map (channels, functions, recursive
// structs), the schema stays empty: JSONSchema returns nil and SystemPrompt an empty
// string, so providers skip native structured output. Validation keeps working —
// use SchemaOf directly to surface the generation error.
type Schema[T any] struct {
	name         string
	description  string
	schema       map[string]any
	systemPrompt string
	embedded     bool // T is a bare scalar: responses travel in the {"data": ...} envelope
}

// NewSchema builds a schema for T. An empty name defaults to "ResponseSchema" (Python
// parity); the description is used as given.
func NewSchema[T any](name, description string) *Schema[T] {
	rt := reflect.TypeOf((*T)(nil)).Elem()
	s := &Schema[T]{
		name:        name,
		description: description,
		embedded:    isScalarKind(deref(rt).Kind()),
	}
	if s.name == "" {
		s.name = "ResponseSchema"
	}
	if sch, err := SchemaOf(rt); err == nil {
		s.schema = sch
		s.systemPrompt = renderSystemPrompt(sch)
	}
	return s
}

// newSchemaFromMap builds a Schema around a ready-made schema map (used by
// QuestionSchema, which decorates the generated shape with question text and enums).
func newSchemaFromMap[T any](name, description string, schema map[string]any) *Schema[T] {
	rt := reflect.TypeOf((*T)(nil)).Elem()
	return &Schema[T]{
		name:         name,
		description:  description,
		schema:       schema,
		systemPrompt: renderSystemPrompt(schema),
		embedded:     isScalarKind(deref(rt).Kind()),
	}
}

func (s *Schema[T]) Name() string        { return s.name }
func (s *Schema[T]) Description() string { return s.description }

// JSONSchema returns a deep copy of the schema: provider mappers may rewrite their copy
// (strict-mode constraints) without racing on the shared original.
func (s *Schema[T]) JSONSchema() map[string]any { return copySchema(s.schema) }

// SystemPrompt returns the standard "answer with JSON matching the schema" instruction
// with the schema embedded, or an empty string when no schema was generated.
func (s *Schema[T]) SystemPrompt() string { return s.systemPrompt }

// Validate parses response into T. Bare scalar types (strings, integers, floats, bools)
// travel in the {"data": ...} envelope and are unwrapped here; every other type is
// parsed directly. Structural constraints the JSON unmarshal cannot see (required
// members, enum membership) are checked against the generated schema.
//
// AGO-DEVIATION: pydantic coerces loosely ("5" → 5); encoding/json is strict, so type
// mismatches are validation errors. See docs/DECISIONS.md D-013.
func (s *Schema[T]) Validate(response string, ctx *agocontext.Context) (any, error) {
	raw := []byte(response)
	if len(s.schema) > 0 {
		if err := checkRequired(raw, s.schema); err != nil {
			return nil, &ValidationError{Err: err, Response: response}
		}
	}
	if s.embedded {
		var envelope struct {
			Data T `json:"data"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return nil, &ValidationError{Err: err, Response: response}
		}
		return envelope.Data, nil
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &ValidationError{Err: err, Response: response}
	}
	return out, nil
}
