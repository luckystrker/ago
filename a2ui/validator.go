package a2ui

import (
	"encoding/json"
	"fmt"
	"strings"
)

// A hand-rolled JSON Schema subset validator (the Python side uses jsonschema
// + referencing; neither carries into the zero-dependency core). Supported
// keywords: $ref, allOf, anyOf, oneOf (with the diagnostics.py const-tag
// narrowing), not, type, const, enum, required, properties,
// additionalProperties (false), items, minItems. Everything else
// (patternProperties, propertyNames, format, unevaluatedProperties, …) is
// treated as always-pass.
//
// AGO-DEVIATION (D-076): the unsupported keywords make the validator accept
// some forms Python's full engine rejects — a false-negative surface by
// design, never a false-positive one. Diagnostic leaf wordings are this
// port's own (abbreviated); only the oneOf-narrowing "unknown <tag> <value>"
// message is Python-verbatim.

// Issue is one reason an A2UI message does not match the schema.
type Issue struct {
	Path    []any // object keys and list indices from the message root
	Message string
}

// Render formats the issue as a sentence for the LLM, naming the component it
// sits in when the path crosses one (diagnostics.py A2UIIssue.render).
func (i Issue) Render(message map[string]any) string {
	node := any(message)
	var component map[string]any
loop:
	for _, part := range i.Path {
		switch p := part.(type) {
		case string:
			m, ok := node.(map[string]any)
			if !ok {
				break loop
			}
			node = m[p]
		case int:
			s, ok := node.([]any)
			if !ok || p < 0 || p >= len(s) {
				break loop
			}
			node = s[p]
		}
		if m, ok := node.(map[string]any); ok {
			if _, isComponent := m["component"].(string); isComponent {
				component = m
			}
		}
	}
	if component != nil {
		id, _ := component["id"].(string)
		if id == "" {
			id = "?"
		}
		kind, _ := component["component"].(string)
		return fmt.Sprintf("Component '%s' (%s): %s", id, kind, i.Message)
	}
	if len(i.Path) == 0 {
		return i.Message
	}
	parts := make([]string, len(i.Path))
	for i, p := range i.Path {
		switch v := p.(type) {
		case string:
			parts[i] = v
		case int:
			parts[i] = fmt.Sprint(v)
		}
	}
	return fmt.Sprintf("%s (at /%s)", i.Message, strings.Join(parts, "/"))
}

// leaf is an unreduced validation failure (diagnostics.py _Leaf).
type leaf struct {
	path    []any
	message string
	keyword string
}

// validator checks instances against schemas resolved from the registry.
type validator struct {
	mgr            *SchemaManager
	agentInitiated bool
}

func unmarshalJSON(raw string, v any) error {
	return json.Unmarshal([]byte(raw), v)
}

// resolveRef resolves a JSON Schema $ref against the registry. Supported
// forms: "#/pointer" (within doc), "<doc-uri-or-name>#/pointer", and a bare
// "<doc-uri-or-name>". The returned doc is the document further nested refs
// resolve within.
func (v *validator) resolveRef(doc map[string]any, ref string) (target any, targetDoc map[string]any, err error) {
	file, pointer := doc, ""
	if i := strings.Index(ref, "#"); i >= 0 {
		file, pointer = nil, ref[i+1:]
		if i > 0 {
			name := ref[:i]
			d, ok := v.mgr.docs[name]
			if !ok {
				return nil, nil, fmt.Errorf("unresolvable reference %q", ref)
			}
			file = d
		} else {
			file = doc
		}
	} else {
		d, ok := v.mgr.docs[ref]
		if !ok {
			return nil, nil, fmt.Errorf("unresolvable reference %q", ref)
		}
		return d, d, nil
	}
	target = any(file)
	if pointer != "" && pointer != "/" {
		for _, token := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
			token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
			switch t := target.(type) {
			case map[string]any:
				next, ok := t[token]
				if !ok {
					return nil, nil, fmt.Errorf("unresolvable reference %q", ref)
				}
				target = next
			case []any:
				var idx int
				if _, e := fmt.Sscanf(token, "%d", &idx); e != nil || idx < 0 || idx >= len(t) {
					return nil, nil, fmt.Errorf("unresolvable reference %q", ref)
				}
				target = t[idx]
			default:
				return nil, nil, fmt.Errorf("unresolvable reference %q", ref)
			}
		}
	}
	return target, file, nil
}

func jsonTypeName(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64:
		if t == float64(int64(t)) {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "unknown"
	}
}

func typeMatches(schemaType string, v any) bool {
	switch schemaType {
	case "null":
		return v == nil
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == float64(int64(f))
	case "array":
		_, ok := v.([]any)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	default:
		return true
	}
}

func pyTypeName(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64:
		if t == float64(int64(t)) {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "unknown"
	}
}

// extendPath returns path + part as a fresh slice. A plain append would
// reuse the parent's backing array whenever it has spare capacity, so two
// sibling failures would share one buffer and each would overwrite the
// other's last segment — every stored leaf path must be independently owned.
func extendPath(path []any, part any) []any {
	out := make([]any, len(path), len(path)+1)
	copy(out, path)
	return append(out, part)
}

// check validates instance against schema and returns the leaf failures with
// paths from the instance root. doc is the document schema was reached from
// (its "#/..." refs resolve inside it).
func (v *validator) check(instance any, schema any, doc map[string]any, path []any) []leaf {
	sm, ok := schema.(map[string]any)
	if !ok {
		return nil // booleans and empty schemas always pass in these documents
	}

	if ref, ok := sm["$ref"].(string); ok {
		target, targetDoc, err := v.resolveRef(doc, ref)
		if err != nil {
			return []leaf{{path, err.Error(), "$ref"}}
		}
		// sibling keywords alongside $ref are ignored (2020-12 behavior)
		return v.check(instance, target, targetDoc, path)
	}

	var leaves []leaf

	if t, ok := sm["type"].(string); ok && !typeMatches(t, instance) {
		leaves = append(leaves, leaf{path,
			fmt.Sprintf("expected %s, got %s", t, pyTypeName(instance)), "type"})
	}
	if c, ok := sm["const"]; ok {
		if !deepEqual(instance, c) {
			leaves = append(leaves, leaf{path,
				fmt.Sprintf("%s is not the required value %s", pyRepr(instance), pyRepr(c)), "const"})
		}
	}
	if e, ok := sm["enum"].([]any); ok {
		found := false
		for _, allowed := range e {
			if deepEqual(instance, allowed) {
				found = true
				break
			}
		}
		if !found {
			leaves = append(leaves, leaf{path,
				fmt.Sprintf("%s is not one of %s", pyRepr(instance), pyJSON(e)), "enum"})
		}
	}

	instanceMap, isMap := instance.(map[string]any)
	instanceList, isList := instance.([]any)

	if req, ok := sm["required"].([]any); ok && isMap {
		for _, r := range req {
			name, _ := r.(string)
			if _, present := instanceMap[name]; !present {
				leaves = append(leaves, leaf{path,
					fmt.Sprintf("'%s' is a required property", name), "required"})
			}
		}
	}

	props, _ := sm["properties"].(map[string]any)
	if isMap {
		names := sortedKeys(instanceMap)
		for _, key := range names {
			if sub, ok := props[key]; ok {
				leaves = append(leaves, v.check(instanceMap[key], sub, doc, extendPath(path, key))...)
			}
		}
		if addl, ok := sm["additionalProperties"].(bool); ok && !addl {
			for _, key := range names {
				if _, declared := props[key]; declared {
					continue
				}
				leaves = append(leaves, leaf{extendPath(path, key),
					fmt.Sprintf("additional property '%s' is not allowed", key), "additionalProperties"})
			}
		}
	}

	if items, ok := sm["items"]; ok && isList {
		for i, item := range instanceList {
			leaves = append(leaves, v.check(item, items, doc, extendPath(path, i))...)
		}
	}
	if min, ok := sm["minItems"].(float64); ok && isList && float64(len(instanceList)) < min {
		leaves = append(leaves, leaf{path,
			fmt.Sprintf("the list must contain at least %d items", int64(min)), "minItems"})
	}

	if allOf, ok := sm["allOf"].([]any); ok {
		for _, sub := range allOf {
			leaves = append(leaves, v.check(instance, sub, doc, path)...)
		}
	}
	if not, ok := sm["not"]; ok {
		if len(v.check(instance, not, doc, path)) == 0 {
			leaves = append(leaves, leaf{path, "should not be valid", "not"})
		}
	}
	if anyOf, ok := sm["anyOf"].([]any); ok {
		if !v.anyBranchPasses(instance, anyOf, doc, path) {
			leaves = append(leaves, leaf{path, "is not valid under any of the given schemas", "anyOf"})
		}
	}
	if oneOf, ok := sm["oneOf"].([]any); ok {
		leaves = append(leaves, v.checkOneOf(instance, oneOf, doc, path)...)
	}
	return leaves
}

func (v *validator) anyBranchPasses(instance any, branches []any, doc map[string]any, path []any) bool {
	for _, sub := range branches {
		if len(v.check(instance, sub, doc, path)) == 0 {
			return true
		}
	}
	return false
}

// checkOneOf implements exact-one semantics with the diagnostics.py narrowing:
// a failed oneOf reports the closest variant's leaves instead of the whole
// operation. Branches ruled out only by a const tag mismatch collapse into the
// "unknown <tag> <value>" message.
func (v *validator) checkOneOf(instance any, branches []any, doc map[string]any, path []any) []leaf {
	type branchResult struct {
		leaves []leaf
		tag    string // set when every leaf is a const mismatch on this one property
		tagged bool
	}
	results := make([]branchResult, len(branches))
	passing := 0
	for i, sub := range branches {
		l := v.check(instance, sub, doc, path)
		if len(l) == 0 {
			passing++
		}
		results[i] = branchResult{leaves: l}
		// A branch that fails only on a top-level const ("component: 'Slider'",
		// "@call: 'openUrl'") is simply not the variant the instance meant.
		if tag, is := constTagOf(l, len(path)); is {
			results[i].tag, results[i].tagged = tag, true
		}
	}
	switch {
	case passing == 1:
		return nil
	case passing > 1:
		return []leaf{{path, "is valid under more than one of the given schemas", "oneOf"}}
	}

	var candidates []branchResult
	var tags []string
	for _, r := range results {
		if r.tagged {
			tags = append(tags, r.tag)
			continue
		}
		candidates = append(candidates, r)
	}
	if len(candidates) == 0 && len(tags) > 0 {
		tag := tags[0]
		value := any(nil)
		if m, ok := instance.(map[string]any); ok {
			value = m[tag]
		}
		return []leaf{{path, fmt.Sprintf("unknown %s %s", tag, pyRepr(value)), "oneOf"}}
	}
	if len(candidates) == 0 {
		return []leaf{{path, "is not valid under any of the given schemas", "oneOf"}}
	}
	best := candidates[0]
	for _, r := range candidates[1:] {
		if len(r.leaves) < len(best.leaves) {
			best = r
		}
	}
	return best.leaves
}

// constTagOf reports the property whose const value ruled this branch out,
// when the branch has a const mismatch at its top level — the same rule
// diagnostics.py applies (_tag_mismatch): a variant that fails on a const tag
// ("component: 'Slider'", "@call: 'openUrl'") is simply not the one the
// instance meant, even when it has other failures besides.
func constTagOf(leaves []leaf, baseDepth int) (string, bool) {
	for _, l := range leaves {
		if l.keyword != "const" || len(l.path) != baseDepth+1 {
			continue
		}
		if name, ok := l.path[baseDepth].(string); ok {
			return name, true
		}
	}
	return "", false
}

func deepEqual(a, b any) bool {
	// Both sides come from encoding/json, so == compares reliably for scalars
	// and maps/slices need a structural walk.
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			other, ok := bv[k]
			if !ok || !deepEqual(v, other) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !deepEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}
