package openai

import (
	"github.com/luckystrker/ago/response"
)

// unsupportedKeys are the schema annotation keys OpenAI strict structured
// output does not accept; StrictifySchema removes them at every level.
var unsupportedKeys = []string{"title", "examples", "default"}

// StrictifySchema recursively coerces a JSON schema into the strict subset
// OpenAI structured output accepts (Python parity: _strictify_schema): every
// object level gains additionalProperties=false and a required list naming all
// its properties, and the unsupported annotation keys (title, examples,
// default) are removed everywhere, including nested properties, array items,
// $defs and anyOf/oneOf/allOf branches.
//
// AGO-DEVIATION: the Python mapper only adds additionalProperties/required and
// keeps title/examples/default (the strict API ignores them); the porting
// brief requires their removal, so AGo strips them.
//
// The input is not mutated; the result is a fresh map sharing leaf values. The
// order of the required list follows map iteration and carries no meaning.
func StrictifySchema(schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}
	out := make(map[string]any, len(schema))
	for k, v := range schema {
		out[k] = v
	}
	for _, k := range unsupportedKeys {
		delete(out, k)
	}
	if t, ok := out["type"].(string); ok && t == "object" {
		if _, ok := out["additionalProperties"]; !ok {
			out["additionalProperties"] = false
		}
	}
	if props, ok := out["properties"].(map[string]any); ok {
		strictProps := make(map[string]any, len(props))
		keys := make([]string, 0, len(props))
		for k, v := range props {
			keys = append(keys, k)
			if sv, ok := v.(map[string]any); ok {
				strictProps[k] = StrictifySchema(sv)
			} else {
				strictProps[k] = v
			}
		}
		out["properties"] = strictProps
		out["required"] = keys
	}
	if defs, ok := out["$defs"].(map[string]any); ok {
		strictDefs := make(map[string]any, len(defs))
		for k, v := range defs {
			if sv, ok := v.(map[string]any); ok {
				strictDefs[k] = StrictifySchema(sv)
			} else {
				strictDefs[k] = v
			}
		}
		out["$defs"] = strictDefs
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		items, ok := out[key].([]any)
		if !ok {
			continue
		}
		strictItems := make([]any, len(items))
		for i, item := range items {
			if sv, ok := item.(map[string]any); ok {
				strictItems[i] = StrictifySchema(sv)
			} else {
				strictItems[i] = item
			}
		}
		out[key] = strictItems
	}
	if items, ok := out["items"].(map[string]any); ok {
		out["items"] = StrictifySchema(items)
	}
	return out
}

// ResponseFormat converts a response schema into the Chat Completions
// response_format: {"type":"json_schema","json_schema":{"name":…,
// "strict":true,"schema":StrictifySchema(…)}}, carrying the schema description
// when present. A nil result means the schema has no native JSON schema (e.g.
// a prompted schema) — output is driven by its system prompt instead. Python
// parity: response_proto_to_schema.
func ResponseFormat(schema response.Proto) map[string]any {
	if schema == nil {
		return nil
	}
	js := schema.JSONSchema()
	if len(js) == 0 {
		return nil
	}
	inner := map[string]any{
		"name":   schema.Name(),
		"strict": true,
		"schema": StrictifySchema(js),
	}
	if d := schema.Description(); d != "" {
		inner["description"] = d
	}
	return map[string]any{"type": "json_schema", "json_schema": inner}
}
