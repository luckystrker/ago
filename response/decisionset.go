package response

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// AnswerKey is the name of the one question a scalar response schema is asked under.
const AnswerKey = "answer"

// ErrInvalidDecisionSet is wrapped by every NewDecisionSet failure on a struct schema:
// a field that is not a decision, two fields under one JSON key, a field without a
// question, a struct without fields. It wraps ErrNotADecision, so either sentinel
// matches with errors.Is. The message names the field.
var ErrInvalidDecisionSet = fmt.Errorf("%w: invalid decision set", ErrNotADecision)

// ErrMissingAnswer is returned by DecisionSet.Render when a question has no value.
var ErrMissingAnswer = errors.New("response: no value for a question of the decision set")

// questionTag is the struct tag that carries a field's question text. It outranks the
// `description=` directive of the `agschema` tag, which stays the fallback (it is the
// text SchemaOf puts on the field's schema node).
//
// AGO-DEVIATION: Python reads a question from Question(...), Field(description=) or an
// Enum docstring and describes options with Question(options=); Go has no such
// introspection, so only the field's own question is carried and struct fields have no
// option descriptions. See D-128.
const questionTag = "question"

// NamedDecision is one question of a DecisionSet: the key it is sent under (the JSON
// key of the struct field, or AnswerKey for a scalar schema) and its spec.
type NamedDecision struct {
	Name string
	Spec DecisionSpec
}

// DecisionSet is a response schema read as the named questions of one request. A scalar
// schema (bool, string enum, int enum, a 0..1 number) is one question named AnswerKey;
// a flat struct is one question per field, in field order, named by the field's `json`
// tag and asked by its `question` tag (fallback: the `description=` directive of the
// `agschema` tag). All questions go out in one request, so the shared input is paid
// for once.
//
// A struct field must be a bool (yes/no), a string with `agschema:"enum=a|b"` (choice),
// an integer with `agschema:"enum=0|1|2"` (score) or a float with
// `agschema:"min=0,max=1"` (probability). Nested structs, pointers (the analogue of
// Python's Optional), slices, arrays, maps, empty structs and fields that end up under
// one JSON key (a repeated `json` name, a name promoted by an embedded struct) are
// rejected by NewDecisionSet with ErrInvalidDecisionSet; embedded structs are
// otherwise flattened the way encoding/json promotes them. The schema's own
// description is a question about the struct as a whole and has nowhere to go, so it
// is rejected too.
//
// A DecisionSet is immutable after construction and safe for concurrent use; every
// accessor hands out copies.
type DecisionSet struct {
	scalar bool
	names  []string
	specs  []DecisionSpec
}

// goTyped is implemented by schemas generated from a Go type; the field order, json
// keys and `question` tags of a struct are only visible there.
type goTyped interface {
	goType() reflect.Type
}

// NewDecisionSet reads p as a decision set.
//
// AGO-DEVIATION: a struct with a pointer, a nested struct, a collection, a shared JSON
// key or a description on the schema is refused with ErrInvalidDecisionSet; Python's
// defaults, AliasPath and extra-properties rejections have no Go analogue. See D-128. A scalar schema yields one question named
// AnswerKey, exactly as FromResponse reads it (a failure wraps ErrNotADecision). A
// struct schema built with NewSchema yields one question per field (a failure wraps
// ErrInvalidDecisionSet). An object schema that was not built from a Go struct has no
// field order or question tags to read and is rejected with ErrNotADecision.
func NewDecisionSet(p Proto) (*DecisionSet, error) {
	if typed, ok := p.(goTyped); ok {
		if t := deref(typed.goType()); t.Kind() == reflect.Struct && t != timeType {
			return newClassSet(p, t)
		}
	} else if p != nil {
		if root := p.JSONSchema(); isObjectSchema(root) {
			if _, embedded := decisionNode(root); !embedded {
				return nil, fmt.Errorf("%w: schema %q is an object schema not built from a Go struct; "+
					"use response.NewSchema[T] with a struct T so the fields can be read in order", ErrNotADecision, p.Name())
			}
		}
	}
	spec, err := FromResponse(p)
	if err != nil {
		return nil, err
	}
	return &DecisionSet{scalar: true, names: []string{AnswerKey}, specs: []DecisionSpec{*spec}}, nil
}

func isObjectSchema(root map[string]any) bool {
	typ, _ := root["type"].(string)
	_, hasProps := root["properties"].(map[string]any)
	return typ == "object" && hasProps
}

func newClassSet(p Proto, t reflect.Type) (*DecisionSet, error) {
	if p.Description() != "" {
		return nil, fmt.Errorf("%w: a struct schema is one question per field, so the schema takes no "+
			"description of its own (%q): put what the fields share in the agent prompt and each field's "+
			"question in its `question` tag", ErrInvalidDecisionSet, p.Description())
	}
	var fields []decisionField
	if err := collectDecisionFields(t, nil, &fields, map[string]string{}); err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("%w: struct %s has no fields; a struct needs at least one question",
			ErrInvalidDecisionSet, t)
	}
	d := &DecisionSet{names: make([]string, 0, len(fields)), specs: make([]DecisionSpec, 0, len(fields))}
	for _, f := range fields {
		d.names = append(d.names, f.key)
		d.specs = append(d.specs, f.spec)
	}
	return d, nil
}

type decisionField struct {
	key  string
	spec DecisionSpec
}

// collectDecisionFields walks the exported fields of t in declaration order, the way
// encoding/json sees them: `json:"-"` and unexported fields are skipped, untagged
// embedded structs are flattened. owners maps each JSON key to the Go field that took
// it, so a second field on the same key is reported instead of shadowed.
func collectDecisionFields(t reflect.Type, path []string, out *[]decisionField, owners map[string]string) error {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		key, tagged, skip := jsonFieldName(f)
		if skip {
			continue
		}
		goName := strings.Join(append(append([]string{}, path...), f.Name), ".")
		if f.Anonymous && !tagged {
			if embedded := deref(f.Type); embedded.Kind() == reflect.Struct {
				if f.Type.Kind() == reflect.Pointer {
					return fieldErr(goName, "an embedded pointer is optional; every question must be a required field")
				}
				if embedded == timeType {
					continue // promotes only unexported fields
				}
				if err := collectDecisionFields(embedded, append(append([]string{}, path...), f.Name), out, owners); err != nil {
					return err
				}
				continue
			}
		}
		if f.PkgPath != "" {
			continue
		}
		if other, dup := owners[key]; dup {
			return fmt.Errorf("%w: fields %s and %s are both under the JSON key %q; each question needs a key of its own",
				ErrInvalidDecisionSet, other, goName, key)
		}
		owners[key] = goName
		spec, err := decisionFieldSpec(f, goName)
		if err != nil {
			return err
		}
		*out = append(*out, decisionField{key: key, spec: *spec})
	}
	return nil
}

func fieldErr(goName, reason string) error {
	return fmt.Errorf("%w: field %s: %s", ErrInvalidDecisionSet, goName, reason)
}

// decisionFieldSpec classifies one struct field as a question.
func decisionFieldSpec(f reflect.StructField, goName string) (*DecisionSpec, error) {
	switch k := f.Type.Kind(); {
	case k == reflect.Pointer:
		return nil, fieldErr(goName, "a pointer is optional; every question must be a required field")
	case k == reflect.Struct:
		return nil, fieldErr(goName, "a nested struct is not a decision; each question must be a flat field")
	case k == reflect.Slice || k == reflect.Array:
		return nil, fieldErr(goName, "a list is not a decision")
	case k == reflect.Map:
		return nil, fieldErr(goName, "a map is not a decision")
	case !isScalarKind(k):
		return nil, fieldErr(goName, fmt.Sprintf("type %s is not a decision", f.Type))
	}
	node, _, err := fieldSchema(f, map[reflect.Type]bool{})
	if err != nil {
		return nil, fmt.Errorf("%w: field %s: %v", ErrInvalidDecisionSet, goName, err)
	}
	question := f.Tag.Get(questionTag)
	if strings.TrimSpace(question) == "" {
		question, _ = node["description"].(string)
	}
	spec := classifyDecision(node, question, false)
	if spec == nil {
		return nil, fieldErr(goName, "not a decision type: use a bool, a string with agschema enum=a|b, "+
			"an integer with agschema enum=0|1|2, or a float with agschema min=0,max=1")
	}
	if strings.TrimSpace(question) == "" {
		// OpenAI requires instructions on every question and the agent prompt is shared by all fields.
		return nil, fieldErr(goName, "has no question: add a `question:\"...\"` tag or an agschema description=")
	}
	return spec, nil
}

// IsScalar reports whether the schema is one value asked as AnswerKey, rather than a
// struct of fields.
func (d *DecisionSet) IsScalar() bool { return d.scalar }

// Len is the number of questions.
func (d *DecisionSet) Len() int { return len(d.names) }

// Names are the question names in field order.
func (d *DecisionSet) Names() []string { return append([]string(nil), d.names...) }

// Questions are the questions in field order. The specs are copies.
func (d *DecisionSet) Questions() []NamedDecision {
	out := make([]NamedDecision, len(d.names))
	for i, name := range d.names {
		out[i] = NamedDecision{Name: name, Spec: cloneSpec(d.specs[i])}
	}
	return out
}

// Spec returns a copy of the named question's spec.
func (d *DecisionSet) Spec(name string) (DecisionSpec, bool) {
	for i, n := range d.names {
		if n == name {
			return cloneSpec(d.specs[i]), true
		}
	}
	return DecisionSpec{}, false
}

func cloneSpec(s DecisionSpec) DecisionSpec {
	s.Options = append([]DecisionOption(nil), s.Options...)
	return s
}

// Render marshals the value of every question into the JSON the response schema
// validates. A scalar set renders its AnswerKey value through the schema (inside the
// {"data": ...} envelope when the schema has one); a struct set renders an object with
// one member per question, in field order. A question without a value fails with
// ErrMissingAnswer; values for names outside the set are ignored.
func (d *DecisionSet) Render(values map[string]any) (string, error) {
	if d.scalar {
		v, ok := values[AnswerKey]
		if !ok {
			return "", fmt.Errorf("%w: %q", ErrMissingAnswer, AnswerKey)
		}
		return d.specs[0].Render(v)
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, name := range d.names {
		v, ok := values[name]
		if !ok {
			return "", fmt.Errorf("%w: %q", ErrMissingAnswer, name)
		}
		key, err := json.Marshal(name)
		if err != nil {
			return "", err
		}
		val, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(val)
	}
	b.WriteByte('}')
	return b.String(), nil
}
