package mcpserver

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// validateArguments checks a tools/call's arguments against the tool's advertised
// inputSchema and returns the first violation's message ("" when they pass),
// worded like the Python jsonschema library the reference uses
// (mappers.input_validation_error), so a client sees the same text from either
// server. The supported vocabulary is the one tool schemas are written in: type,
// enum/const, properties/required/additionalProperties, items, numeric and
// string bounds, pattern, minItems/maxItems/uniqueItems, contains/minContains/
// maxContains, prefixItems, minProperties/maxProperties, propertyNames,
// dependentRequired/dependentSchemas, if/then/else, anyOf/oneOf/allOf/not, boolean
// schemas and local $ref ($defs / definitions). Annotations (title, description,
// default, examples, format, ...) are ignored, as jsonschema ignores format by
// default.
//
// AGO-DEVIATION: not enforced — unevaluatedProperties/unevaluatedItems, $dynamicRef,
// references outside the document, any pattern that RE2 cannot compile (a
// pattern using lookahead or backreferences), and the draft-07 array form of
// items (Python's jsonschema raises SchemaError for it, which the executor
// flattens into a tool error naming the schema; here the call proceeds). The
// work spent on one value's keyword chain is capped (see maxSchemaSteps). A schema relying on them is checked
// less strictly than Python's jsonschema would check it; the agent or tool still
// validates what it receives. Enforcing a keyword wrongly would reject calls
// Python accepts, so the unsupported ones are skipped rather than guessed.
func validateArguments(args map[string]any, schema map[string]any) string {
	// The advertised schema is built from Go values ([]string, typed maps); a JSON
	// round trip gives the validator the one shape it reads.
	if raw, err := json.Marshal(schema); err == nil {
		var norm map[string]any
		if json.Unmarshal(raw, &norm) == nil {
			schema = norm
		}
	}
	v := &schemaValidator{root: schema}
	var instance any = args
	if args == nil {
		instance = map[string]any{}
	}
	return v.child(instance, schema)
}

type schemaValidator struct {
	root map[string]any
}

// child validates a value nested inside the instance — a property, an item, a
// property name — with a fresh step budget: only chains of keywords applied to
// the *same* instance value (a $ref cycle, nested allOf/anyOf/oneOf/not/if) draw
// on one budget, so the size of the instance never spends it.
func (v *schemaValidator) child(inst any, raw any) string {
	return v.check(inst, raw, &chain{left: maxSchemaSteps})
}

// chain is the state shared by the keyword applications on one instance value:
// the step budget left and the $refs currently being followed.
type chain struct {
	left int
	refs map[string]bool
}

// maxSchemaSteps bounds the keyword applications chained on ONE instance value.
// A $ref cycle is cut exactly (see chain.refs), so the budget only guards the
// finite-but-exponential case: a schema whose anyOf/allOf lists fan out several
// ways per level. It is per value, renewed for every property and item, so the
// size of an instance never spends it. Past the budget the rest of that value's
// keyword chain is not enforced (the schema is the operator's, the arguments the
// caller's: the budget protects the server, not the caller).
const maxSchemaSteps = 50000

// check validates inst against a schema, which is an object or a boolean schema.
func (v *schemaValidator) check(inst any, raw any, c *chain) string {
	if c.left--; c.left < 0 {
		return ""
	}
	if b, ok := raw.(bool); ok {
		if !b {
			return fmt.Sprintf("False schema does not allow %s", pyRepr(inst))
		}
		return ""
	}
	schema, _ := raw.(map[string]any)
	if schema == nil {
		return ""
	}
	if ref, ok := schema["$ref"].(string); ok {
		target := v.resolve(ref)
		// A $ref met again while it is being followed, on the same value, is a
		// cycle that consumes none of the instance: it constrains nothing further.
		if target != nil && !c.refs[ref] {
			if c.refs == nil {
				c.refs = map[string]bool{}
			}
			c.refs[ref] = true
			msg := v.check(inst, target, c)
			delete(c.refs, ref)
			if msg != "" {
				return msg
			}
		}
	}
	if t, ok := schema["type"]; ok {
		if !typeMatches(inst, t) {
			return fmt.Sprintf("%s is not of type %s", pyRepr(inst), pyTypes(t))
		}
	}
	if enum, ok := schema["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if jsonEqual(inst, e) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Sprintf("%s is not one of %s", pyRepr(inst), pyRepr(enum))
		}
	}
	if c, ok := schema["const"]; ok && !jsonEqual(inst, c) {
		return fmt.Sprintf("%s was expected", pyRepr(c))
	}
	switch val := inst.(type) {
	case map[string]any:
		if msg := v.checkObject(val, schema, c); msg != "" {
			return msg
		}
	case []any:
		if msg := v.checkArray(val, schema, c); msg != "" {
			return msg
		}
	case string:
		if n, ok := num(schema["minLength"]); ok && float64(utf8.RuneCountInString(val)) < n {
			return fmt.Sprintf("%s is too short", pyRepr(val))
		}
		if n, ok := num(schema["maxLength"]); ok && float64(utf8.RuneCountInString(val)) > n {
			return fmt.Sprintf("%s is too long", pyRepr(val))
		}
		if p, ok := schema["pattern"].(string); ok {
			if re := compilePattern(p); re != nil && !re.MatchString(val) {
				return fmt.Sprintf("%s does not match %s", pyRepr(val), pyRepr(p))
			}
		}
	case float64:
		if msg := checkNumber(val, schema); msg != "" {
			return msg
		}
	}
	if sub, ok := schema["allOf"].([]any); ok {
		for _, s := range sub {
			if msg := v.check(inst, s, c); msg != "" {
				return msg
			}
		}
	}
	if cond, ok := schema["if"]; ok {
		if v.check(inst, cond, c) == "" {
			if then, has := schema["then"]; has {
				if msg := v.check(inst, then, c); msg != "" {
					return msg
				}
			}
		} else if els, has := schema["else"]; has {
			if msg := v.check(inst, els, c); msg != "" {
				return msg
			}
		}
	}
	if sub, ok := schema["anyOf"].([]any); ok && !v.anyValid(inst, sub, c) {
		return fmt.Sprintf("%s is not valid under any of the given schemas", pyRepr(inst))
	}
	if sub, ok := schema["oneOf"].([]any); ok {
		valid := 0
		for _, s := range sub {
			if v.check(inst, s, c) == "" {
				valid++
			}
		}
		switch {
		case valid == 0:
			return fmt.Sprintf("%s is not valid under any of the given schemas", pyRepr(inst))
		case valid > 1:
			return fmt.Sprintf("%s is valid under each of the given schemas", pyRepr(inst))
		}
	}
	if not, ok := schema["not"]; ok && v.check(inst, not, c) == "" {
		return fmt.Sprintf("%s should not be valid under %s", pyRepr(inst), pyRepr(not))
	}
	return ""
}

func (v *schemaValidator) anyValid(inst any, sub []any, c *chain) bool {
	for _, s := range sub {
		if v.check(inst, s, c) == "" {
			return true
		}
	}
	return false
}

func (v *schemaValidator) checkObject(obj map[string]any, schema map[string]any, c *chain) string {
	if req, ok := schema["required"].([]any); ok {
		for _, r := range req {
			if name, ok := r.(string); ok {
				if _, present := obj[name]; !present {
					return fmt.Sprintf("%s is a required property", pyRepr(name))
				}
			}
		}
	}
	props, _ := schema["properties"].(map[string]any)
	names := make([]string, 0, len(obj))
	for k := range obj {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if ps, ok := props[k]; ok {
			if msg := v.child(obj[k], ps); msg != "" {
				return msg
			}
		}
	}
	pp, _ := schema["patternProperties"].(map[string]any)
	if len(pp) > 0 {
		patterns := make([]string, 0, len(pp))
		for p := range pp {
			patterns = append(patterns, p)
		}
		sort.Strings(patterns)
		for _, k := range names {
			for _, p := range patterns {
				if re := compilePattern(p); re != nil && re.MatchString(k) {
					if msg := v.child(obj[k], pp[p]); msg != "" {
						return msg
					}
				}
			}
		}
	}
	if pn, ok := schema["propertyNames"]; ok {
		for _, k := range names {
			if msg := v.child(k, pn); msg != "" {
				return msg
			}
		}
	}
	if dr, ok := schema["dependentRequired"].(map[string]any); ok {
		for _, k := range names {
			if need, ok := dr[k].([]any); ok {
				for _, n := range need {
					if name, ok := n.(string); ok {
						if _, present := obj[name]; !present {
							return fmt.Sprintf("%s is a dependency of %s", pyRepr(name), pyRepr(k))
						}
					}
				}
			}
		}
	}
	if ds, ok := schema["dependentSchemas"].(map[string]any); ok {
		for _, k := range names {
			if sub, ok := ds[k]; ok {
				if msg := v.check(obj, sub, c); msg != "" {
					return msg
				}
			}
		}
	}
	switch ap := schema["additionalProperties"].(type) {
	case bool:
		if !ap {
			var extra []string
			for _, k := range names {
				if _, known := props[k]; !known && !matchesPatternProps(k, pp) {
					extra = append(extra, pyRepr(k))
				}
			}
			switch len(extra) {
			case 0:
			case 1:
				return fmt.Sprintf("Additional properties are not allowed (%s was unexpected)", extra[0])
			default:
				return fmt.Sprintf("Additional properties are not allowed (%s were unexpected)", strings.Join(extra, ", "))
			}
		}
	case map[string]any:
		for _, k := range names {
			if _, known := props[k]; !known && !matchesPatternProps(k, pp) {
				if msg := v.child(obj[k], ap); msg != "" {
					return msg
				}
			}
		}
	}
	if n, ok := num(schema["minProperties"]); ok && float64(len(obj)) < n {
		return fmt.Sprintf("%s does not have enough properties", pyRepr(obj))
	}
	if n, ok := num(schema["maxProperties"]); ok && float64(len(obj)) > n {
		return fmt.Sprintf("%s has too many properties", pyRepr(obj))
	}
	return ""
}

// matchesPatternProps reports whether the key matches any patternProperties
// pattern (those keys are governed by the pattern's schema, not by
// additionalProperties).
func matchesPatternProps(key string, pp map[string]any) bool {
	for p := range pp {
		if re := compilePattern(p); re != nil && re.MatchString(key) {
			return true
		}
	}
	return false
}

// patternCache holds compiled patterns by source; a pattern RE2 cannot compile is
// cached as nil, so it is not recompiled (and not enforced) on every call. The
// keys come from the operator's schemas, so the cache is bounded by them.
var patternCache sync.Map

func compilePattern(p string) *regexp.Regexp {
	if c, ok := patternCache.Load(p); ok {
		re, _ := c.(*regexp.Regexp)
		return re
	}
	re, err := regexp.Compile(p)
	if err != nil {
		re = nil
	}
	patternCache.Store(p, re)
	return re
}

func (v *schemaValidator) checkArray(arr []any, schema map[string]any, c *chain) string {
	prefix, _ := schema["prefixItems"].([]any)
	for i, el := range arr {
		if i < len(prefix) {
			if msg := v.child(el, prefix[i]); msg != "" {
				return msg
			}
		}
	}
	if items, ok := schema["items"]; ok {
		for i := len(prefix); i < len(arr); i++ {
			if msg := v.child(arr[i], items); msg != "" {
				return msg
			}
		}
	}
	if c, ok := schema["contains"]; ok {
		matches := 0
		for _, el := range arr {
			if v.child(el, c) == "" {
				matches++
			}
		}
		min := 1.0
		if m, ok := num(schema["minContains"]); ok {
			min = m
		}
		if float64(matches) < min {
			if matches == 0 {
				return fmt.Sprintf("%s does not contain items matching the given schema", pyRepr(arr))
			}
			return fmt.Sprintf("Too few items match the given schema (expected at least %s but only %d matched)", pyRepr(min), matches)
		}
		if m, ok := num(schema["maxContains"]); ok && float64(matches) > m {
			return fmt.Sprintf("Too many items match the given schema (expected at most %s)", pyRepr(m))
		}
	}
	if n, ok := num(schema["minItems"]); ok && float64(len(arr)) < n {
		return fmt.Sprintf("%s is too short", pyRepr(arr))
	}
	if n, ok := num(schema["maxItems"]); ok && float64(len(arr)) > n {
		return fmt.Sprintf("%s is too long", pyRepr(arr))
	}
	if u, ok := schema["uniqueItems"].(bool); ok && u {
		for i := range arr {
			for j := i + 1; j < len(arr); j++ {
				if jsonEqual(arr[i], arr[j]) {
					return fmt.Sprintf("%s has non-unique elements", pyRepr(arr))
				}
			}
		}
	}
	return ""
}

func checkNumber(n float64, schema map[string]any) string {
	if m, ok := num(schema["minimum"]); ok && n < m {
		return fmt.Sprintf("%s is less than the minimum of %s", pyRepr(n), pyRepr(m))
	}
	if m, ok := num(schema["maximum"]); ok && n > m {
		return fmt.Sprintf("%s is greater than the maximum of %s", pyRepr(n), pyRepr(m))
	}
	if m, ok := num(schema["exclusiveMinimum"]); ok && n <= m {
		return fmt.Sprintf("%s is less than or equal to the minimum of %s", pyRepr(n), pyRepr(m))
	}
	if m, ok := num(schema["exclusiveMaximum"]); ok && n >= m {
		return fmt.Sprintf("%s is greater than or equal to the maximum of %s", pyRepr(n), pyRepr(m))
	}
	if m, ok := num(schema["multipleOf"]); ok && m != 0 && math.Mod(n, m) != 0 {
		return fmt.Sprintf("%s is not a multiple of %s", pyRepr(n), pyRepr(m))
	}
	return ""
}

func (v *schemaValidator) resolve(ref string) map[string]any {
	if !strings.HasPrefix(ref, "#") {
		return nil // only same-document references
	}
	cur := any(v.root)
	for _, part := range strings.Split(strings.TrimPrefix(strings.TrimPrefix(ref, "#"), "/"), "/") {
		if part == "" {
			continue
		}
		part = strings.NewReplacer("~1", "/", "~0", "~").Replace(part)
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	m, _ := cur.(map[string]any)
	return m
}

func num(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

func typeMatches(inst any, t any) bool {
	switch tt := t.(type) {
	case string:
		return oneType(inst, tt)
	case []any:
		for _, x := range tt {
			if s, ok := x.(string); ok && oneType(inst, s) {
				return true
			}
		}
	}
	return false
}

func oneType(inst any, t string) bool {
	switch t {
	case "object":
		_, ok := inst.(map[string]any)
		return ok
	case "array":
		_, ok := inst.([]any)
		return ok
	case "string":
		_, ok := inst.(string)
		return ok
	case "boolean":
		_, ok := inst.(bool)
		return ok
	case "null":
		return inst == nil
	case "number":
		_, ok := inst.(float64)
		return ok
	case "integer":
		f, ok := inst.(float64)
		return ok && f == math.Trunc(f) && !math.IsInf(f, 0)
	}
	return false
}

func pyTypes(t any) string {
	switch tt := t.(type) {
	case string:
		return pyRepr(tt)
	case []any:
		// jsonschema joins the alternatives: 'integer', 'null'.
		parts := make([]string, len(tt))
		for i, e := range tt {
			parts[i] = pyRepr(e)
		}
		return strings.Join(parts, ", ")
	}
	return pyRepr(t)
}

func jsonEqual(a, b any) bool { return reflect.DeepEqual(a, b) }

// pyRepr renders a decoded JSON value the way Python's repr does (strings in
// single quotes, True/False/None, dicts with insertion-ordered keys — sorted
// here, since Go maps carry no order), which is how jsonschema words its
// messages.
func pyRepr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return "'" + strings.ReplaceAll(strings.ReplaceAll(x, `\`, `\\`), `'`, `\'`) + "'"
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []string:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = pyRepr(k) + ": " + pyRepr(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}
