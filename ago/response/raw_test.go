package response

import (
	"reflect"
	"testing"

	agocontext "ago/context"
)

func TestRawSchemaDefaults(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"a": map[string]any{"type": "string"},
	}}
	r := RawSchema(schema)
	if r.Name() != "ResponseSchema" {
		t.Errorf("Name() = %q, want %q", r.Name(), "ResponseSchema")
	}
	if r.Description() != "" {
		t.Errorf("Description() = %q, want empty", r.Description())
	}
	if r.SystemPrompt() != "" {
		t.Errorf("SystemPrompt() = %q, want empty", r.SystemPrompt())
	}
	if got := r.JSONSchema(); !reflect.DeepEqual(got, schema) {
		t.Errorf("JSONSchema() =\n%v\nwant\n%v", got, schema)
	}
}

func TestRawSchemaOptions(t *testing.T) {
	r := RawSchema(map[string]any{"type": "string"}, WithRawName("Raw"), WithRawDescription("d"))
	if r.Name() != "Raw" || r.Description() != "d" {
		t.Errorf("Name/Description = %q/%q, want Raw/d", r.Name(), r.Description())
	}
}

func TestRawSchemaValidatePassesThrough(t *testing.T) {
	r := RawSchema(nil, WithRawName("X"))
	v, err := r.Validate(`anything {"a":1}`, nil)
	if err != nil {
		t.Fatalf("Validate error: %v", err)
	}
	if v != `anything {"a":1}` {
		t.Errorf("Validate = %v, want the raw response unchanged", v)
	}
}

func TestRawSchemaJSONSchemaCopyIsolation(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"a": map[string]any{"type": "string"},
	}}
	r := RawSchema(schema)
	got := r.JSONSchema()
	got["properties"].(map[string]any)["a"].(map[string]any)["type"] = "mutated"
	if schema["properties"].(map[string]any)["a"].(map[string]any)["type"] != "string" {
		t.Error("mutating the returned map changed the wrapped schema")
	}
}

func TestRawSchemaValidateWithContext(t *testing.T) {
	// Validate ignores the conversation context (D-007); exercise the signature.
	r := RawSchema(map[string]any{"type": "string"}, WithRawName("X"))
	stream := streamNewForTest()
	ctx := agocontext.New(stream)
	v, err := r.Validate("ok", ctx)
	if err != nil || v != "ok" {
		t.Errorf("Validate with context = %v, %v", v, err)
	}
}
