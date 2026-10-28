package response

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

var timeType = reflect.TypeOf(time.Time{})

// SchemaOf builds a JSON schema for t by reflection. It is the shared generator used by
// response schemas and, in package tools, for function-parameter schemas.
//
// Type mapping:
//
//	string          → {"type":"string"}     int/uint → {"type":"integer"}
//	float           → {"type":"number"}     bool     → {"type":"boolean"}
//	[]T / [N]T      → {"type":"array","items":<T>}
//	[]byte          → {"type":"string"} (encoding/json marshals byte slices base64)
//	struct          → {"type":"object","properties":…,"required":[…]}
//	map[string]T    → {"type":"object","additionalProperties":<T>}
//	time.Time       → {"type":"string","format":"date-time"}
//	interface / any → {} (anything)
//
// A bare scalar at the top level is wrapped into the one-field envelope
// {"type":"object","properties":{"data":<scalar>},"required":["data"]} (Python parity).
// Structs, maps and arrays are never wrapped.
//
// Struct fields take their JSON name from the `json` tag (the Go field name when the
// tag is absent, matching encoding/json; `json:"-"` skips). Embedded struct fields are
// flattened the way encoding/json promotes them. The `agschema` tag carries schema
// metadata as comma-separated directives: `description=<text>` (the text cannot contain
// commas), `required`, `enum=a|b|c` (| separated, parsed per the field kind) and
// `min=<n>` / `max=<n>` (minimum/maximum on numbers, minLength/maxLength on strings).
// Unknown directives are an error.
//
// AGO-DEVIATION: Python also wraps top-level lists into the {"data": ...} envelope and
// treats a bare `str` schema as plain text (no schema at all); the AGo contract wraps
// scalars only — string included — and never arrays, maps or structs. See
// docs/DECISIONS.md D-013.
func SchemaOf(t reflect.Type) (map[string]any, error) {
	if t == nil {
		return nil, fmt.Errorf("response: cannot build a JSON schema for a nil type")
	}
	if dt := deref(t); isScalarKind(dt.Kind()) {
		inner, err := scalarSchema(dt, fieldTags{})
		if err != nil {
			return nil, err
		}
		return wrapDataSchema(inner), nil
	}
	return typeSchema(t, map[reflect.Type]bool{})
}

// typeSchema maps one type without the top-level scalar envelope.
func typeSchema(t reflect.Type, inProgress map[reflect.Type]bool) (map[string]any, error) {
	t = deref(t)
	if t == timeType {
		return map[string]any{"type": "string", "format": "date-time"}, nil
	}
	if isScalarKind(t.Kind()) {
		return scalarSchema(t, fieldTags{})
	}
	switch t.Kind() {
	case reflect.Struct:
		return structSchema(t, inProgress)
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return map[string]any{"type": "string"}, nil
		}
		return arraySchema(t, inProgress)
	case reflect.Array:
		return arraySchema(t, inProgress)
	case reflect.Map:
		return mapSchema(t, inProgress)
	case reflect.Interface:
		return map[string]any{}, nil
	default:
		return nil, fmt.Errorf("response: cannot build a JSON schema for %s", t)
	}
}

func structSchema(t reflect.Type, inProgress map[reflect.Type]bool) (map[string]any, error) {
	if inProgress[t] {
		return nil, fmt.Errorf("response: recursive type %s is not supported", t)
	}
	inProgress[t] = true
	defer delete(inProgress, t)

	properties := make(map[string]any)
	required := []string{} // always non-nil: an object schema carries "required" even when empty
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, tagged, skip := jsonFieldName(f)
		if skip {
			continue
		}
		if f.Anonymous && !tagged {
			embedded := deref(f.Type)
			if embedded.Kind() == reflect.Struct {
				if embedded == timeType {
					// embedding time.Time promotes only its unexported fields
					continue
				}
				sub, err := structSchema(embedded, inProgress)
				if err != nil {
					return nil, err
				}
				if props, ok := sub["properties"].(map[string]any); ok {
					for k, v := range props {
						properties[k] = v
					}
				}
				if names, ok := sub["required"].([]string); ok {
					required = append(required, names...)
				}
				continue
			}
			// Anonymous non-struct field: encoding/json names it after the type.
			name = f.Name
		}
		if f.PkgPath != "" {
			// Unexported. Embedded struct types were flattened above (encoding/json
			// promotes their exported fields); everything else stays out of the wire.
			continue
		}
		sch, fieldRequired, err := fieldSchema(f, inProgress)
		if err != nil {
			return nil, err
		}
		properties[name] = sch
		if fieldRequired {
			required = append(required, name)
		}
	}
	return map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   dedupeStrings(required),
	}, nil
}

// fieldSchema maps one struct field, applying its agschema metadata.
func fieldSchema(f reflect.StructField, inProgress map[reflect.Type]bool) (map[string]any, bool, error) {
	tags, err := parseAgschema(f.Tag.Get("agschema"))
	if err != nil {
		return nil, false, fmt.Errorf("response: field %s: %w", f.Name, err)
	}
	sch, err := typeSchema(f.Type, inProgress)
	if err != nil {
		return nil, false, fmt.Errorf("response: field %s: %w", f.Name, err)
	}
	if tags.description != "" {
		sch["description"] = tags.description
	}
	if err := applyConstraints(sch, deref(f.Type), tags); err != nil {
		return nil, false, fmt.Errorf("response: field %s: %w", f.Name, err)
	}
	return sch, tags.required, nil
}

func arraySchema(t reflect.Type, inProgress map[reflect.Type]bool) (map[string]any, error) {
	items, err := typeSchema(t.Elem(), inProgress)
	if err != nil {
		return nil, err
	}
	return map[string]any{"type": "array", "items": items}, nil
}

func mapSchema(t reflect.Type, inProgress map[reflect.Type]bool) (map[string]any, error) {
	if t.Key().Kind() != reflect.String {
		return nil, fmt.Errorf("response: map keys must be strings for a JSON schema, got %s", t)
	}
	additional, err := typeSchema(t.Elem(), inProgress)
	if err != nil {
		return nil, err
	}
	return map[string]any{"type": "object", "additionalProperties": additional}, nil
}

func scalarSchema(t reflect.Type, tags fieldTags) (map[string]any, error) {
	var sch map[string]any
	switch t.Kind() {
	case reflect.String:
		sch = map[string]any{"type": "string"}
	case reflect.Bool:
		sch = map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		sch = map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		sch = map[string]any{"type": "number"}
	default:
		return nil, fmt.Errorf("response: %s is not a scalar type", t)
	}
	if err := applyConstraints(sch, t, tags); err != nil {
		return nil, err
	}
	return sch, nil
}

// applyConstraints writes the agschema enum/min/max constraints onto a scalar schema
// node. Constraints on non-scalar nodes are an error.
func applyConstraints(sch map[string]any, t reflect.Type, tags fieldTags) error {
	if len(tags.enum) == 0 && !tags.hasMin && !tags.hasMax {
		return nil
	}
	if !isScalarKind(t.Kind()) {
		return fmt.Errorf("enum/min/max constraints apply only to scalar fields, not %s", t)
	}
	if len(tags.enum) > 0 {
		values, err := parseEnumValues(tags.enum, t)
		if err != nil {
			return err
		}
		sch["enum"] = values
	}
	if tags.hasMin {
		if err := setBound(sch, t, tags.min, true); err != nil {
			return err
		}
	}
	if tags.hasMax {
		if err := setBound(sch, t, tags.max, false); err != nil {
			return err
		}
	}
	return nil
}

func setBound(sch map[string]any, t reflect.Type, raw string, minimum bool) error {
	key := "maximum"
	if minimum {
		key = "minimum"
	}
	if t.Kind() == reflect.String {
		key = "maxLength"
		if minimum {
			key = "minLength"
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("bad %s value %q: %w", key, raw, err)
		}
		sch[key] = n
		return nil
	}
	switch {
	case isIntKind(t.Kind()):
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("bad %s value %q: %w", key, raw, err)
		}
		sch[key] = n
	case isFloatKind(t.Kind()):
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("bad %s value %q: %w", key, raw, err)
		}
		sch[key] = f
	default:
		return fmt.Errorf("%s constraints do not apply to %s", key, t)
	}
	return nil
}

func parseEnumValues(raw []string, t reflect.Type) ([]any, error) {
	out := make([]any, 0, len(raw))
	for _, r := range raw {
		var v any
		var err error
		switch {
		case t.Kind() == reflect.String:
			v = r
		case t.Kind() == reflect.Bool:
			v, err = strconv.ParseBool(r)
		case isIntKind(t.Kind()):
			var n int64
			n, err = strconv.ParseInt(r, 10, 64)
			v = n
		case isFloatKind(t.Kind()):
			v, err = strconv.ParseFloat(r, 64)
		default:
			return nil, fmt.Errorf("enum values are not supported for %s", t)
		}
		if err != nil {
			return nil, fmt.Errorf("bad enum value %q for %s: %w", r, t, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// fieldTags is the parsed `agschema` struct tag.
type fieldTags struct {
	description string
	required    bool
	enum        []string
	hasMin      bool
	min         string
	hasMax      bool
	max         string
}

func parseAgschema(tag string) (fieldTags, error) {
	var ft fieldTags
	if tag == "" {
		return ft, nil
	}
	for _, part := range strings.Split(tag, ",") {
		part = strings.TrimSpace(part)
		switch {
		case part == "":
			continue
		case part == "required":
			ft.required = true
		case strings.HasPrefix(part, "description="):
			ft.description = strings.TrimPrefix(part, "description=")
		case strings.HasPrefix(part, "enum="):
			ft.enum = strings.Split(strings.TrimPrefix(part, "enum="), "|")
		case strings.HasPrefix(part, "min="):
			ft.min, ft.hasMin = strings.TrimPrefix(part, "min="), true
		case strings.HasPrefix(part, "max="):
			ft.max, ft.hasMax = strings.TrimPrefix(part, "max="), true
		default:
			return ft, fmt.Errorf("response: unknown agschema directive %q", part)
		}
	}
	return ft, nil
}

// jsonFieldName returns the JSON name (json tag or Go field name), whether the json tag
// carried an explicit name, and whether the field is skipped (`json:"-"`).
func jsonFieldName(f reflect.StructField) (name string, tagged bool, skip bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false, true
	}
	if i := strings.Index(tag, ","); i >= 0 {
		tag = tag[:i]
	}
	if tag != "" {
		return tag, true, false
	}
	return f.Name, false, false
}

// wrapDataSchema wraps a scalar schema into the one-field {"data": ...} envelope
// (Python parity for top-level scalars).
func wrapDataSchema(inner map[string]any) map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"data": inner},
		"required":   []string{"data"},
	}
}

func deref(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func isScalarKind(k reflect.Kind) bool {
	return k == reflect.String || k == reflect.Bool || isIntKind(k) || isFloatKind(k)
}

func isIntKind(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return true
	}
	return false
}

func isFloatKind(k reflect.Kind) bool { return k == reflect.Float32 || k == reflect.Float64 }

func dedupeStrings(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// copySchema deep-copies a schema map so the immutable originals can be shared with
// callers that legitimately rewrite their own copy (provider strict-mode mappers).
func copySchema(s map[string]any) map[string]any {
	if s == nil {
		return nil
	}
	out := make(map[string]any, len(s))
	for k, v := range s {
		out[k] = copyValue(v)
	}
	return out
}

func copyValue(v any) any {
	switch v := v.(type) {
	case map[string]any:
		return copySchema(v)
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = copyValue(e)
		}
		return out
	case []string:
		out := make([]string, len(v))
		copy(out, v)
		return out
	case []int64:
		out := make([]int64, len(v))
		copy(out, v)
		return out
	case []float64:
		out := make([]float64, len(v))
		copy(out, v)
		return out
	case []bool:
		out := make([]bool, len(v))
		copy(out, v)
		return out
	default:
		return v
	}
}
