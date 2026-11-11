package response

import (
	"fmt"

	agocontext "github.com/luckystrker/ago/context"
)

// CallableSchema is a response schema whose validation ends in a caller-supplied
// function instead of stopping at a decoded value. It is the Go shape of Python's
// `response_schema` decorator (`ag2/response/callable.py`): there the function's
// parameters define the JSON schema, the model response is decoded into them, and
// fast_depends injects the rest.
//
// Two constructors cover the two halves of that:
//
//   - NewCallableSchema[T] is the decorator port: the response decodes into T
//     through Schema[T] (same reflection-generated JSON schema, same {"data": ...}
//     rule for bare scalars), then the function runs with the decoded value.
//     AGO-DEVIATION: fast_depends parameter injection has no Go counterpart, so
//     the dependencies a Python function declares as parameters travel in the
//     conversation context, which the function reads itself
//     (ctx.Dependency / ctx.Variable).
//   - NewRawCallable passes the raw response text instead, for answers that are
//     not JSON.
//
// SystemPrompt inherits Schema[T]'s prompt on the typed path; the raw path has
// none. JSONSchema can be overridden with WithCallableJSONSchema either way.
//
// A CallableSchema is immutable after construction and safe for concurrent use as
// long as the wrapped function is.
type CallableSchema struct {
	name         string
	description  string
	schema       map[string]any
	systemPrompt string
	// inner decodes and (for the typed path) validates the response before the
	// caller's function runs. nil on the raw path.
	inner Proto
	// run invokes the caller's function with the decoded value (typed path) or
	// the raw response (raw path).
	run func(ctx *agocontext.Context, value any) (any, error)
}

// callableOptions is what the options collect; embed defaults to true (Python
// parity: ResponseSchema(embed=True)).
type callableOptions struct {
	name        string
	description string
	schema      map[string]any
	embed       bool
}

// CallableOption configures a CallableSchema.
type CallableOption func(*callableOptions)

// WithCallableName sets the schema name (Python parity: response_schema(name=...)).
// The typed path defaults to Schema[T]'s name, the raw path to "ResponseSchema".
func WithCallableName(name string) CallableOption {
	return func(o *callableOptions) { o.name = name }
}

// WithCallableDescription sets the schema description (Python parity:
// response_schema(description=...)). The typed path defaults to Schema[T]'s
// description.
func WithCallableDescription(description string) CallableOption {
	return func(o *callableOptions) { o.description = description }
}

// WithCallableJSONSchema overrides the JSON schema the provider may enforce
// natively (Python parity: response_schema(schema=...)). The map is copied, so
// the caller keeps ownership of its own value. Without it the typed path uses the
// schema generated from T; the raw path has none.
func WithCallableJSONSchema(schema map[string]any) CallableOption {
	return func(o *callableOptions) { o.schema = copySchema(schema) }
}

// WithCallableEmbed controls the {"data": ...} envelope on the typed path; see
// WithSchemaEmbed (Python parity: response_schema(embed=...)).
func WithCallableEmbed(embed bool) CallableOption {
	return func(o *callableOptions) { o.embed = embed }
}

// collectCallableOptions applies opts with the defaults.
func collectCallableOptions(opts []CallableOption) callableOptions {
	o := callableOptions{embed: true}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// NewCallableSchema builds a schema over T whose validation decodes the response
// into T and then calls validate with it (Python parity: the `response_schema`
// decorator). A nil validate returns nil, like NewPromptedSchema: no schema at
// all is free text.
func NewCallableSchema[T any](validate func(ctx *agocontext.Context, value T) (any, error), opts ...CallableOption) *CallableSchema {
	if validate == nil {
		return nil
	}
	o := collectCallableOptions(opts)
	inner := NewSchema[T]("", "", WithSchemaEmbed(o.embed))
	run := func(ctx *agocontext.Context, value any) (any, error) {
		typed, ok := value.(T)
		if !ok {
			return nil, fmt.Errorf("response: callable schema decoded %T, want %T", value, typed)
		}
		return validate(ctx, typed)
	}
	return newCallable(inner, run, o)
}

// RawValidator validates the raw model response text (see NewRawCallable).
type RawValidator func(ctx *agocontext.Context, response string) (any, error)

// NewRawCallable builds a schema whose validation hands the raw response text to
// validate, for answers that are not JSON. It is the Go escape hatch Python does
// not need: a Python callable always receives decoded parameters.
func NewRawCallable(validate RawValidator, opts ...CallableOption) *CallableSchema {
	if validate == nil {
		return nil
	}
	o := collectCallableOptions(opts)
	run := func(ctx *agocontext.Context, value any) (any, error) {
		text, _ := value.(string)
		return validate(ctx, text)
	}
	return newCallable(nil, run, o)
}

// newCallable assembles the schema metadata: the inner schema's when there is
// one, an explicit JSON schema last (Python parity: schema=schema or inner's).
func newCallable(inner Proto, run func(*agocontext.Context, any) (any, error), o callableOptions) *CallableSchema {
	c := &CallableSchema{inner: inner, run: run, name: o.name, description: o.description}
	if inner != nil {
		if c.name == "" {
			c.name = inner.Name()
		}
		if c.description == "" {
			c.description = inner.Description()
		}
		c.schema = inner.JSONSchema()
		c.systemPrompt = inner.SystemPrompt()
	}
	if c.name == "" {
		c.name = "ResponseSchema"
	}
	if o.schema != nil {
		c.schema = copySchema(o.schema)
	}
	return c
}

func (c *CallableSchema) Name() string        { return c.name }
func (c *CallableSchema) Description() string { return c.description }

// JSONSchema returns a deep copy of the schema (the generated one on the typed
// path, an override or nil otherwise).
func (c *CallableSchema) JSONSchema() map[string]any { return copySchema(c.schema) }

// SystemPrompt is the typed path's Schema[T] instruction (empty on the raw path).
//
// AGO-DEVIATION: Python's CallableResponse keeps system_prompt=None and relies on
// provider-native structured output; the typed path keeps the Go Schema[T] prompt
// so prompt-driven providers can still answer in the right shape.
func (c *CallableSchema) SystemPrompt() string { return c.systemPrompt }

// Validate decodes the response into T (typed path, surfacing a *ValidationError
// from the shared schema machinery), then runs the caller's function with the
// decoded value and the conversation.
func (c *CallableSchema) Validate(response string, ctx *agocontext.Context) (any, error) {
	value := any(response)
	if c.inner != nil {
		v, err := c.inner.Validate(response, ctx)
		if err != nil {
			return nil, err
		}
		value = v
	}
	return c.run(ctx, value)
}
