package response

import (
	"encoding/json"
	"fmt"
)

// checkRequired verifies the structural part of a generated schema that encoding/json
// does not enforce by itself: required object members and enum membership. Type
// mismatches are left to the typed unmarshal that runs afterwards, so the walk returns
// nil (not an error) whenever the raw value does not have the expected shape.
func checkRequired(raw []byte, schema map[string]any) error {
	if len(schema) == 0 {
		return nil
	}
	switch typ, _ := schema["type"].(string); typ {
	case "object":
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil // not an object: the typed unmarshal reports it
		}
		for _, name := range requiredNames(schema) {
			if _, ok := obj[name]; !ok {
				return fmt.Errorf("missing required field %q", name)
			}
		}
		props, _ := schema["properties"].(map[string]any)
		for name, value := range obj {
			sub, _ := props[name].(map[string]any)
			if sub == nil {
				continue
			}
			if err := checkRequired(value, sub); err != nil {
				return err
			}
			if err := checkEnumValue(value, sub); err != nil {
				return err
			}
		}
	case "array":
		items, _ := schema["items"].(map[string]any)
		if items == nil {
			return nil
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil
		}
		for _, item := range arr {
			if err := checkRequired(item, items); err != nil {
				return err
			}
			if err := checkEnumValue(item, items); err != nil {
				return err
			}
		}
	}
	return nil
}

// requiredNames normalizes the "required" representation the generator ([]string) and
// JSON-decoded schemas ([]any) produce.
func requiredNames(schema map[string]any) []string {
	switch r := schema["required"].(type) {
	case []string:
		return r
	case []any:
		out := make([]string, 0, len(r))
		for _, v := range r {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// checkEnumValue verifies a scalar value against an enum constraint, if the node has one.
func checkEnumValue(raw []byte, schema map[string]any) error {
	allowed := enumAny(schema["enum"])
	if allowed == nil {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil // malformed values are reported by the typed unmarshal
	}
	for _, want := range allowed {
		if scalarEqual(v, want) {
			return nil
		}
	}
	return fmt.Errorf("value %s is not one of the allowed enum values", raw)
}

// enumAny normalizes the enum representations the generator and decoded JSON produce.
func enumAny(v any) []any {
	switch e := v.(type) {
	case []any:
		return e
	case []string:
		out := make([]any, len(e))
		for i, s := range e {
			out[i] = s
		}
		return out
	case []int64:
		out := make([]any, len(e))
		for i, n := range e {
			out[i] = n
		}
		return out
	case []float64:
		out := make([]any, len(e))
		for i, f := range e {
			out[i] = f
		}
		return out
	case []bool:
		out := make([]any, len(e))
		for i, b := range e {
			out[i] = b
		}
		return out
	default:
		return nil
	}
}

func scalarEqual(got, want any) bool {
	g, gok := numericValue(got)
	w, wok := numericValue(want)
	if gok && wok {
		return g == w
	}
	return got == want
}

func numericValue(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	default:
		return 0, false
	}
}
