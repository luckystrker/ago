package tools

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"ago/response"
)

// emptyObjectSchema is the parameters schema of a tool without model-provided
// arguments: the JSON-Schema object form providers require.
func emptyObjectSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

// isDISlotType reports whether t is one of the DI annotation types (Inject/Variable),
// which never appear in a generated schema.
func isDISlotType(t reflect.Type) bool {
	dt := derefType(t)
	return dt == injectType || dt == variableType
}

// argStructSchema builds the parameters schema for the tool's argument struct: one
// property per model-provided field, DI slots excluded. Fields the user left without
// an `agschema` tag are marked required (Python parity: every parameter is required
// unless Optional); pointer fields are optional, and fields carrying their own
// agschema tag keep it verbatim.
//
// AGO-DEVIATION: Python generates the schema from parameter names via
// fast_depends/Pydantic; Go reflection exposes no parameter names, so the schema is
// generated from the argument struct's fields via response.SchemaOf. Per-property
// "title" entries of the Python output are not produced (the AGo contract forbids
// titles in Parameters).
func argStructSchema(structType reflect.Type) (map[string]any, error) {
	fields := make([]reflect.StructField, 0, structType.NumField())
	for i := 0; i < structType.NumField(); i++ {
		f := structType.Field(i)
		if f.PkgPath != "" {
			// Unexported: encoding/json ignores them, so does the schema.
			continue
		}
		if _, _, skip := jsonFieldName(f); skip {
			continue
		}
		_, isDI, err := diSlotForField(f)
		if err != nil {
			return nil, err
		}
		if isDI {
			continue
		}
		if f.Tag.Get("agschema") == "" && f.Type.Kind() != reflect.Pointer {
			// Untagged non-pointer fields are mandatory arguments; pointer fields
			// are Optional[X] and stay optional.
			f.Tag = structTagWith(f, "agschema", "required")
		}
		fields = append(fields, f)
	}
	if len(fields) == 0 {
		return emptyObjectSchema(), nil
	}
	return schemaOfSyntheticStruct(fields)
}

// schemaOfSyntheticStruct runs response.SchemaOf over a rebuilt struct type.
// StructOf panics on malformed input; a panic is surfaced as an error because tool
// construction must not crash the process.
func schemaOfSyntheticStruct(fields []reflect.StructField) (schema map[string]any, err error) {
	defer func() {
		if r := recover(); r != nil {
			schema, err = nil, fmt.Errorf("tools: cannot build a schema for the argument struct: %v", r)
		}
	}()
	sch, err := response.SchemaOf(reflect.StructOf(fields))
	if err != nil {
		return nil, fmt.Errorf("tools: %w", err)
	}
	return sch, nil
}

// stripTitle removes a top-level "title" key from a parameters map (Python parity:
// FunctionToolSchema.__post_init__).
func stripTitle(params map[string]any) {
	if params != nil {
		delete(params, "title")
	}
}

// deepCopySchema clones a schema map so a stored explicit schema cannot be mutated
// through the caller's reference.
func deepCopySchema(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		// Schema maps are JSON values by construction; fall back to the reference.
		return in
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return in
	}
	return out
}

// toolSource renders the diagnostic identity of a tool for precedence warnings
// (Python: `TypeName('name')`, nested with " > ").
func toolSource(t Tool) string {
	rt := reflect.TypeOf(t)
	for rt != nil && rt.Kind() == reflect.Pointer {
		rt = rt.Elem()
	}
	name := "<nil>"
	if rt != nil {
		name = rt.Name()
	}
	return fmt.Sprintf("%s(%q)", name, t.Name())
}

type schemaKey struct {
	name       string
	isFunction bool
}

// schemaKeyOf extracts the callable identity of one schema: the function name for
// function schemas, the provider-side type for hosted-tool schemas.
func schemaKeyOf(s any) (schemaKey, error) {
	switch s := s.(type) {
	case Schema:
		return schemaKey{name: s.Name, isFunction: true}, nil
	case *Schema:
		if s == nil {
			return schemaKey{}, fmt.Errorf("%w: nil function schema", ErrResolution)
		}
		return schemaKey{name: s.Name, isFunction: true}, nil
	case ToolSchema:
		return schemaKey{name: s.SchemaType(), isFunction: false}, nil
	default:
		return schemaKey{}, fmt.Errorf("%w: unsupported schema type %T", ErrResolution, s)
	}
}

// keysFromSchemas maps every schema to its callable identity.
func keysFromSchemas(schemas []any) ([]schemaKey, error) {
	keys := make([]schemaKey, 0, len(schemas))
	for _, s := range schemas {
		k, err := schemaKeyOf(s)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// normalizeJSONArgs treats empty arguments as "{}" (Python parity:
// `json.loads(arguments or "{}")`).
func normalizeJSONArgs(arguments string) string {
	if strings.TrimSpace(arguments) == "" {
		return "{}"
	}
	return arguments
}
