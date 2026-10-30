package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// SuiteFromJSONL loads a Suite from a JSONL file, one case per line (Python
// parity: dataset/suite.py Suite.from_jsonl, reduced to the fields the AGo Case
// carries — AGO-DEVIATION: Python's suite name/source metadata and tags have no
// slot on Suite []Case and are dropped — D-061).
//
// Each non-blank line is a JSON object:
//
//	{"name": "weather", "input": "Weather in Paris?", "expected": "sunny",
//	 "scorers": ["correctness", {"name": "tools_called", "params": {"names": ["get_weather"]}}]}
//
// "input" is required (the prompt the agent is asked); "name" auto-fills
// "task-0000", "task-0001", … in dataset order; "expected" feeds the case and
// the scorers' scope. "scorers" entries are a registered scorer name or an
// object {"name": …, "params": {…}}, resolved through r — nil r with scorers
// present is an error. Blank lines are skipped; a UTF-8 BOM is tolerated
// (Python parity: encoding="utf-8-sig"). Errors carry the file and line number.
func SuiteFromJSONL(path string, r *ScorerRegistry) (Suite, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("eval: read suite: %w", err)
	}
	text := strings.TrimPrefix(string(data), "\ufeff")
	var suite Suite
	for i, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		lineNo := i + 1
		if line == "" {
			continue
		}
		var parsed any
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			return nil, fmt.Errorf("eval: %s: line %d is not valid JSON: %w", path, lineNo, err)
		}
		item, ok := parsed.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("eval: %s: line %d must be a JSON object, got %s", path, lineNo, jsonKind(parsed))
		}
		c, err := caseFromItem(item, len(suite), r)
		if err != nil {
			return nil, fmt.Errorf("eval: %s: line %d: %w", path, lineNo, err)
		}
		suite = append(suite, c)
	}
	return suite, nil
}

func caseFromItem(item map[string]any, index int, r *ScorerRegistry) (Case, error) {
	c := Case{}
	if v, err := requireString(item, "input", index); err != nil {
		return c, err
	} else {
		c.Input = v
	}
	if v, ok, err := optString(item, "name"); err != nil {
		return c, err
	} else if ok && v != "" {
		c.Name = v
	} else {
		c.Name = fmt.Sprintf("task-%04d", index) // Python parity: auto "task-0000" in dataset order
	}
	if v, ok, err := optString(item, "expected"); err != nil {
		return c, err
	} else if ok {
		c.Expected = v
	}
	if raw, ok := item["scorers"]; ok {
		specs, err := scorerSpecs(raw)
		if err != nil {
			return c, err
		}
		if r == nil {
			return c, fmt.Errorf("case %q names scorers but no scorer registry was given", c.Name)
		}
		scorers, err := r.BuildScorers(specs, CaseScope{Expected: c.Expected})
		if err != nil {
			return c, err
		}
		c.Scorers = scorers
	}
	return c, nil
}

// scorerSpecs decodes the "scorers" value: each entry is a scorer-name string
// or an object {"name": …, "params": {…}}.
func scorerSpecs(raw any) ([]ScorerSpec, error) {
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("field \"scorers\" must be a list, got %T", raw)
	}
	specs := make([]ScorerSpec, 0, len(items))
	for i, item := range items {
		switch v := item.(type) {
		case string:
			specs = append(specs, ScorerSpec{Name: v})
		case map[string]any:
			name, _ := v["name"].(string)
			if name == "" {
				return nil, fmt.Errorf("scorer entry %d needs a \"name\"", i)
			}
			spec := ScorerSpec{Name: name}
			if params, ok := v["params"].(map[string]any); ok {
				spec.Params = params
			}
			specs = append(specs, spec)
		default:
			return nil, fmt.Errorf("scorer entry %d must be a string or an object, got %T", i, item)
		}
	}
	return specs, nil
}

func requireString(item map[string]any, key string, index int) (string, error) {
	raw, ok := item[key]
	if !ok {
		return "", fmt.Errorf("task at index %d is missing required field %q", index, key)
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("field %q must be a string, got %T", key, raw)
	}
	return s, nil
}

func optString(item map[string]any, key string) (string, bool, error) {
	raw, ok := item[key]
	if !ok {
		return "", false, nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", false, fmt.Errorf("field %q must be a string, got %T", key, raw)
	}
	return s, true, nil
}

// jsonKind names a decoded JSON value's kind for error messages (Python parity:
// "got %s" with the type name).
func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	default:
		return "object"
	}
}
