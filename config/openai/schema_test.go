package openai

import (
	"sort"
	"testing"

	"ago/response"
)

func TestStrictifySchema(t *testing.T) {
	in := map[string]any{
		"title":    "Root",
		"examples": []any{map[string]any{"a": 1}},
		"type":     "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "title": "Name", "default": "x"},
			"tags": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string", "title": "Tag"},
			},
			"child": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"n": map[string]any{"type": "integer", "examples": []any{1}},
				},
			},
			"either": map[string]any{"anyOf": []any{
				map[string]any{"type": "null", "title": "Nothing"},
				map[string]any{"type": "string"},
			}},
		},
		"$defs": map[string]any{
			"extra": map[string]any{
				"type":       "object",
				"title":      "Extra",
				"properties": map[string]any{"q": map[string]any{"type": "string"}},
			},
		},
	}
	out := StrictifySchema(in)

	// Root: annotation keys stripped, strict object rules added.
	if _, ok := out["title"]; ok {
		t.Fatalf("root title must be removed: %+v", out)
	}
	if _, ok := out["examples"]; ok {
		t.Fatalf("root examples must be removed: %+v", out)
	}
	if out["additionalProperties"] != false {
		t.Fatalf("root must gain additionalProperties=false: %+v", out["additionalProperties"])
	}
	props := out["properties"].(map[string]any)
	if got := out["required"].([]string); len(got) != len(props) {
		t.Fatalf("required must list all properties: %v", got)
	}

	// Nested property: title/default stripped.
	name := props["name"].(map[string]any)
	if _, ok := name["title"]; ok {
		t.Fatalf("nested title must be removed: %+v", name)
	}
	if _, ok := name["default"]; ok {
		t.Fatalf("nested default must be removed: %+v", name)
	}

	// Array items are strictified.
	tag := props["tags"].(map[string]any)["items"].(map[string]any)
	if _, ok := tag["title"]; ok {
		t.Fatalf("items title must be removed: %+v", tag)
	}

	// Nested objects gain their own additionalProperties/required.
	child := props["child"].(map[string]any)
	if child["additionalProperties"] != false {
		t.Fatalf("nested object must gain additionalProperties=false: %+v", child)
	}
	if req := child["required"].([]string); len(req) != 1 || req[0] != "n" {
		t.Fatalf("nested required must list its properties: %v", req)
	}
	if _, ok := child["properties"].(map[string]any)["n"].(map[string]any)["examples"]; ok {
		t.Fatal("nested examples must be removed")
	}

	// anyOf branches are strictified.
	anyOf := props["either"].(map[string]any)["anyOf"].([]any)
	if _, ok := anyOf[0].(map[string]any)["title"]; ok {
		t.Fatal("anyOf branch title must be removed")
	}

	// $defs entries are strictified.
	def := out["$defs"].(map[string]any)["extra"].(map[string]any)
	if _, ok := def["title"]; ok {
		t.Fatal("$defs title must be removed")
	}
	if def["additionalProperties"] != false {
		t.Fatal("$defs object must gain additionalProperties=false")
	}

	// The input map must not be mutated.
	if in["title"] != "Root" {
		t.Fatalf("input map was mutated: %+v", in)
	}
	if _, ok := in["required"]; ok {
		t.Fatalf("input map gained a required key: %+v", in)
	}
}

func TestStrictifySchemaNonObject(t *testing.T) {
	out := StrictifySchema(map[string]any{"type": "string", "title": "S"})
	if len(out) != 1 || out["type"] != "string" {
		t.Fatalf("non-object schema must only be stripped: %+v", out)
	}
}

func TestStrictifySchemaExplicitAdditionalPropertiesKept(t *testing.T) {
	out := StrictifySchema(map[string]any{
		"type": "object", "additionalProperties": true,
		"properties": map[string]any{},
	})
	if out["additionalProperties"] != true {
		t.Fatalf("explicit additionalProperties must survive: %+v", out["additionalProperties"])
	}
}

func TestStrictifySchemaNil(t *testing.T) {
	if got := StrictifySchema(nil); got != nil {
		t.Fatalf("nil schema must stay nil, got %+v", got)
	}
}

func TestResponseFormat(t *testing.T) {
	schema := response.RawSchema(
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "title": "Name"},
			},
		},
		response.WithRawName("Out"),
		response.WithRawDescription("a test schema"),
	)
	rf := ResponseFormat(schema)
	if rf == nil {
		t.Fatal("schema with a JSON schema must produce a response_format")
	}
	if rf["type"] != "json_schema" {
		t.Fatalf("bad envelope type: %+v", rf)
	}
	inner := rf["json_schema"].(map[string]any)
	if inner["name"] != "Out" || inner["strict"] != true || inner["description"] != "a test schema" {
		t.Fatalf("bad json_schema envelope: %+v", inner)
	}
	st := inner["schema"].(map[string]any)
	if st["additionalProperties"] != false {
		t.Fatal("schema must be strictified")
	}
	nameProp := st["properties"].(map[string]any)["name"].(map[string]any)
	if _, ok := nameProp["title"]; ok {
		t.Fatal("schema title must be stripped by StrictifySchema")
	}
	req := st["required"].([]string)
	sort.Strings(req)
	if len(req) != 1 || req[0] != "name" {
		t.Fatalf("required must list all properties: %v", req)
	}
}

func TestResponseFormatNil(t *testing.T) {
	if ResponseFormat(nil) != nil {
		t.Fatal("nil schema must produce no response_format")
	}
	// A schema without a JSON schema (prompted-style) produces none either.
	if ResponseFormat(response.RawSchema(nil)) != nil {
		t.Fatal("empty JSON schema must produce no response_format")
	}
}
