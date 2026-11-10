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
	embedded     bool // T is a bare scalar and the envelope is on: responses travel in {"data": ...}
}

// SchemaOption customizes a Schema.
type SchemaOption func(*schemaOptions)

// schemaOptions collects the options NewSchema applies before the schema is
// generated; embed defaults to true (Python parity).
type schemaOptions struct {
	embed bool
}

// WithSchemaEmbed controls the {"data": ...} envelope bare scalar responses
// travel in (Python parity: ResponseSchema(embed=...)). The default, true, wraps
// every scalar so the wire shape is always a JSON object; false prompts for and
// validates the bare value ("42", `"hi"`), for providers or models that answer
// with a scalar directly. It changes nothing for non-scalar types.
//
// AGO-DEVIATION: for T = string the envelope is off but the answer is still a
// JSON string ("hi", quotes included). Python's ResponseSchema(str) accepts any
// text at all; use no schema (free text) for that.
func WithSchemaEmbed(embed bool) SchemaOption {
	return func(o *schemaOptions) { o.embed = embed }
}

// NewSchema builds a schema for T. An empty name defaults to "ResponseSchema" (Python
// parity); the description is used as given. Options tune the envelope (see
// WithSchemaEmbed).
func NewSchema[T any](name, description string, opts ...SchemaOption) *Schema[T] {
	o := schemaOptions{embed: true}
	for _, opt := range opts {
		opt(&o)
	}
	rt := reflect.TypeOf((*T)(nil)).Elem()
	scalar := isScalarKind(deref(rt).Kind())
	s := &Schema[T]{
		name:        name,
		description: description,
		embedded:    o.embed && scalar,
	}
	if s.name == "" {
		s.name = "ResponseSchema"
	}
	var (
		sch map[string]any
		err error
	)
	if scalar && !s.embedded {
		// The bare scalar schema, without the wrapper SchemaOf applies at the top
		// level: the model must answer "42", not {"data": 42}.
		sch, err = typeSchema(deref(rt), map[reflect.Type]bool{})
	} else {
		sch, err = SchemaOf(rt)
	}
	if err == nil {
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
// travel in the {"data": ...} envelope and are unwrapped here unless the schema was
// built with WithSchemaEmbed(false), in which case the bare value is parsed; every
// other type is parsed directly. Structural constraints the JSON unmarshal cannot see
// (required members, enum membership) are checked against the generated schema.
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
