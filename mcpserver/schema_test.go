package mcpserver

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The messages are the ones Python's jsonschema produces, because a client sees
// them as "Input validation error: <message>" from either server.
func TestValidateArguments(t *testing.T) {
	obj := func(src string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(src), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	for _, tc := range []struct {
		name   string
		schema string
		args   string
		want   string // "" = valid
	}{
		{"valid", `{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`, `{"n":3}`, ""},
		{"missing required", `{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}`, `{}`, "'message' is a required property"},
		{"wrong type", `{"type":"object","properties":{"message":{"type":"string"}}}`, `{"message":5}`, "5 is not of type 'string'"},
		{"bool is not a number", `{"type":"object","properties":{"n":{"type":"number"}}}`, `{"n":true}`, "True is not of type 'number'"},
		{"float is not an integer", `{"type":"object","properties":{"n":{"type":"integer"}}}`, `{"n":1.5}`, "1.5 is not of type 'integer'"},
		{"integral float is an integer", `{"type":"object","properties":{"n":{"type":"integer"}}}`, `{"n":2.0}`, ""},
		{"null", `{"type":"object","properties":{"s":{"type":"string"}}}`, `{"s":null}`, "None is not of type 'string'"},
		{"type union", `{"type":"object","properties":{"s":{"type":["string","null"]}}}`, `{"s":null}`, ""},
		{"enum", `{"type":"object","properties":{"c":{"enum":["red","green"]}}}`, `{"c":"blue"}`, "'blue' is not one of ['red', 'green']"},
		{"const", `{"type":"object","properties":{"c":{"const":"x"}}}`, `{"c":"y"}`, "'x' was expected"},
		{"additionalProperties false", `{"type":"object","properties":{"a":{}},"additionalProperties":false}`, `{"a":1,"b":2}`, "Additional properties are not allowed ('b' was unexpected)"},
		{"additionalProperties false, several", `{"type":"object","additionalProperties":false}`, `{"b":2,"c":3}`, "Additional properties are not allowed ('b', 'c' were unexpected)"},
		{"additionalProperties schema", `{"type":"object","additionalProperties":{"type":"string"}}`, `{"b":2}`, "2 is not of type 'string'"},
		{"minimum", `{"type":"object","properties":{"n":{"minimum":5}}}`, `{"n":3}`, "3 is less than the minimum of 5"},
		{"maximum", `{"type":"object","properties":{"n":{"maximum":5}}}`, `{"n":9}`, "9 is greater than the maximum of 5"},
		{"exclusiveMinimum", `{"type":"object","properties":{"n":{"exclusiveMinimum":5}}}`, `{"n":5}`, "5 is less than or equal to the minimum of 5"},
		{"multipleOf", `{"type":"object","properties":{"n":{"multipleOf":2}}}`, `{"n":3}`, "3 is not a multiple of 2"},
		{"minLength", `{"type":"object","properties":{"s":{"minLength":2}}}`, `{"s":"a"}`, "'a' is too short"},
		{"maxLength counts characters", `{"type":"object","properties":{"s":{"maxLength":2}}}`, `{"s":"éé"}`, ""},
		{"pattern", `{"type":"object","properties":{"s":{"pattern":"^a+$"}}}`, `{"s":"b"}`, "'b' does not match '^a+$'"},
		{"items", `{"type":"object","properties":{"l":{"type":"array","items":{"type":"integer"}}}}`, `{"l":[1,"x"]}`, "'x' is not of type 'integer'"},
		{"minItems", `{"type":"object","properties":{"l":{"minItems":2}}}`, `{"l":[1]}`, "[1] is too short"},
		{"uniqueItems", `{"type":"object","properties":{"l":{"uniqueItems":true}}}`, `{"l":[1,1]}`, "[1, 1] has non-unique elements"},
		{"nested path", `{"type":"object","properties":{"o":{"type":"object","properties":{"k":{"type":"string"}},"required":["k"]}}}`, `{"o":{}}`, "'k' is a required property"},
		{"anyOf", `{"type":"object","properties":{"v":{"anyOf":[{"type":"string"},{"type":"integer"}]}}}`, `{"v":1.5}`, "1.5 is not valid under any of the given schemas"},
		{"oneOf both", `{"type":"object","properties":{"v":{"oneOf":[{"type":"integer"},{"type":"number"}]}}}`, `{"v":1}`, "1 is valid under each of the given schemas"},
		{"allOf", `{"type":"object","properties":{"v":{"allOf":[{"type":"integer"},{"minimum":3}]}}}`, `{"v":1}`, "1 is less than the minimum of 3"},
		{"not", `{"type":"object","properties":{"v":{"not":{"type":"string"}}}}`, `{"v":"x"}`, "'x' should not be valid under {'type': 'string'}"},
		{"local $ref", `{"type":"object","properties":{"p":{"$ref":"#/$defs/P"}},"$defs":{"P":{"type":"integer"}}}`, `{"p":"x"}`, "'x' is not of type 'integer'"},
		{"a $ref cycle terminates", `{"$ref":"#"}`, `{}`, ""},
		{"an allOf cycle terminates", `{"allOf":[{"$ref":"#"}]}`, `{}`, ""},
		{"an anyOf cycle terminates", `{"anyOf":[{"$ref":"#"}]}`, `{}`, ""},

		{"annotations are ignored", `{"type":"object","title":"T","description":"D","properties":{"a":{"format":"email","title":"x"}}}`, `{"a":"not-an-email"}`, ""},
		{"minProperties", `{"type":"object","minProperties":1}`, `{}`, "{} does not have enough properties"},
		{"minLength satisfied", `{"type":"object","properties":{"s":{"minLength":2}}}`, `{"s":"abc"}`, ""},
		{"minLength exact", `{"type":"object","properties":{"s":{"minLength":2}}}`, `{"s":"ab"}`, ""},
		{"maxLength exact", `{"type":"object","properties":{"s":{"maxLength":2}}}`, `{"s":"ab"}`, ""},
		{"maxLength exceeded", `{"type":"object","properties":{"s":{"maxLength":2}}}`, `{"s":"abc"}`, "'abc' is too long"},
		{"pattern satisfied", `{"type":"object","properties":{"s":{"pattern":"^a+$"}}}`, `{"s":"aaa"}`, ""},
		{"a pattern RE2 cannot compile is not enforced", `{"type":"object","properties":{"s":{"pattern":"^(?=a)a$"}}}`, `{"s":"b"}`, ""},
		{"oneOf exactly one", `{"type":"object","properties":{"v":{"oneOf":[{"type":"string"},{"type":"integer"}]}}}`, `{"v":"x"}`, ""},
		{"oneOf second branch", `{"type":"object","properties":{"v":{"oneOf":[{"type":"string"},{"type":"integer"}]}}}`, `{"v":3}`, ""},
		{"anyOf satisfied", `{"type":"object","properties":{"v":{"anyOf":[{"type":"string"},{"type":"integer"}]}}}`, `{"v":3}`, ""},
		{"boundaries are inclusive", `{"type":"object","properties":{"n":{"minimum":5,"maximum":5}}}`, `{"n":5}`, ""},
		{"minItems exact", `{"type":"object","properties":{"l":{"minItems":2,"maxItems":2}}}`, `{"l":[1,2]}`, ""},
		{"maxItems exceeded", `{"type":"object","properties":{"l":{"maxItems":1}}}`, `{"l":[1,2]}`, "[1, 2] is too long"},
		{"contains", `{"type":"object","properties":{"l":{"contains":{"type":"string"}}}}`, `{"l":[1,2]}`, "[1, 2] does not contain items matching the given schema"},
		{"contains satisfied", `{"type":"object","properties":{"l":{"contains":{"type":"string"}}}}`, `{"l":[1,"a"]}`, ""},
		{"minContains", `{"type":"object","properties":{"l":{"contains":{"type":"string"},"minContains":2}}}`, `{"l":["a",1]}`, "Too few items match the given schema (expected at least 2 but only 1 matched)"},
		{"maxContains", `{"type":"object","properties":{"l":{"contains":{"type":"string"},"maxContains":1}}}`, `{"l":["a","b"]}`, "Too many items match the given schema (expected at most 1)"},
		{"prefixItems", `{"type":"object","properties":{"l":{"prefixItems":[{"type":"string"},{"type":"integer"}]}}}`, `{"l":["a","b"]}`, "'b' is not of type 'integer'"},
		{"prefixItems then items", `{"type":"object","properties":{"l":{"prefixItems":[{"type":"string"}],"items":{"type":"integer"}}}}`, `{"l":["a",1,"x"]}`, "'x' is not of type 'integer'"},
		{"items false", `{"type":"object","properties":{"l":{"prefixItems":[{}],"items":false}}}`, `{"l":[1,2]}`, "False schema does not allow 2"},
		{"false property schema", `{"type":"object","properties":{"x":false}}`, `{"x":1}`, "False schema does not allow 1"},
		{"true schema", `{"type":"object","properties":{"x":true}}`, `{"x":1}`, ""},
		{"if then", `{"type":"object","if":{"properties":{"a":{"const":1}},"required":["a"]},"then":{"required":["b"]}}`, `{"a":1}`, "'b' is a required property"},
		{"if else", `{"type":"object","if":{"properties":{"a":{"const":1}},"required":["a"]},"else":{"required":["c"]}}`, `{"a":2}`, "'c' is a required property"},
		{"if then satisfied", `{"type":"object","if":{"properties":{"a":{"const":1}},"required":["a"]},"then":{"required":["b"]}}`, `{"a":1,"b":2}`, ""},
		{"dependentRequired", `{"type":"object","dependentRequired":{"card":["billing"]}}`, `{"card":"x"}`, "'billing' is a dependency of 'card'"},
		{"dependentSchemas", `{"type":"object","dependentSchemas":{"card":{"required":["billing"]}}}`, `{"card":"x"}`, "'billing' is a required property"},
		{"minProperties exact", `{"type":"object","minProperties":1}`, `{"a":1}`, ""},
		{"maxProperties exact", `{"type":"object","maxProperties":1}`, `{"a":1}`, ""},
		{"maxProperties exceeded", `{"type":"object","maxProperties":1}`, `{"a":1,"b":2}`, "{'a': 1, 'b': 2} has too many properties"},
		{"patternProperties admits matching keys", `{"type":"object","patternProperties":{"^x_":{}},"additionalProperties":false}`, `{"x_a":1}`, ""},
		{"patternProperties refuses the rest", `{"type":"object","patternProperties":{"^x_":{}},"additionalProperties":false}`, `{"x_a":1,"y":2}`, "Additional properties are not allowed ('y' was unexpected)"},
		{"items all valid", `{"type":"object","properties":{"l":{"items":{"type":"integer"}}}}`, `{"l":[1,2,3]}`, ""},
		{"prefixItems and items all valid", `{"type":"object","properties":{"l":{"prefixItems":[{"type":"string"}],"items":{"type":"integer"}}}}`, `{"l":["a",1,2]}`, ""},
		{"prefixItems only, short array", `{"type":"object","properties":{"l":{"prefixItems":[{"type":"string"},{"type":"integer"}]}}}`, `{"l":["a"]}`, ""},
		{"maxContains exact", `{"type":"object","properties":{"l":{"contains":{"type":"string"},"maxContains":2}}}`, `{"l":["a","b",1]}`, ""},
		{"minContains exact", `{"type":"object","properties":{"l":{"contains":{"type":"string"},"minContains":2}}}`, `{"l":["a","b"]}`, ""},
		{"uniqueItems all distinct", `{"type":"object","properties":{"l":{"uniqueItems":true}}}`, `{"l":[1,2,3]}`, ""},
		{"uniqueItems false allows duplicates", `{"type":"object","properties":{"l":{"uniqueItems":false}}}`, `{"l":[1,1]}`, ""},
		{"negative numbers need no bound", `{"type":"object","properties":{"n":{"type":"number"}}}`, `{"n":-3}`, ""},
		{"zero needs no bound", `{"type":"object","properties":{"n":{"minimum":-5}}}`, `{"n":0}`, ""},
		{"exclusiveMinimum satisfied", `{"type":"object","properties":{"n":{"exclusiveMinimum":5}}}`, `{"n":6}`, ""},
		{"exclusiveMaximum", `{"type":"object","properties":{"n":{"exclusiveMaximum":5}}}`, `{"n":5}`, "5 is greater than or equal to the maximum of 5"},
		{"exclusiveMaximum satisfied", `{"type":"object","properties":{"n":{"exclusiveMaximum":5}}}`, `{"n":4}`, ""},
		{"multipleOf satisfied", `{"type":"object","properties":{"n":{"multipleOf":2}}}`, `{"n":4}`, ""},
		{"multipleOf zero is ignored", `{"type":"object","properties":{"n":{"multipleOf":0}}}`, `{"n":3}`, ""},
		{"no multipleOf, odd number", `{"type":"object","properties":{"n":{"type":"integer"}}}`, `{"n":3}`, ""},
		{"type union refuses the rest", `{"type":"object","properties":{"v":{"type":["integer","null"]}}}`, `{"v":"x"}`, "'x' is not of type 'integer', 'null'"},
		{"patternProperties value is checked", `{"type":"object","patternProperties":{"^x_":{"type":"integer"}}}`, `{"x_a":"str"}`, "'str' is not of type 'integer'"},
		{"patternProperties value valid", `{"type":"object","patternProperties":{"^x_":{"type":"integer"}}}`, `{"x_a":1,"other":"free"}`, ""},
		{"patternProperties every matching pattern applies", `{"type":"object","patternProperties":{"^x_":{"type":"integer"},"a$":{"minimum":5}}}`, `{"x_a":1}`, "1 is less than the minimum of 5"},
		{"a pattern-matched key is not additional", `{"type":"object","patternProperties":{"^x_":{}},"additionalProperties":{"type":"integer"}}`, `{"x_a":"str"}`, ""},
		{"additionalProperties still covers the rest", `{"type":"object","patternProperties":{"^x_":{}},"additionalProperties":{"type":"integer"}}`, `{"x_a":"str","y":"str"}`, "'str' is not of type 'integer'"},
		{"a declared property is not additional either", `{"type":"object","properties":{"p":{}},"patternProperties":{"^x_":{}},"additionalProperties":{"type":"integer"}}`, `{"p":"str","x_b":"s"}`, ""},
		{"propertyNames", `{"type":"object","propertyNames":{"pattern":"^[a-z]+$"}}`, `{"Bad":1}`, "'Bad' does not match '^[a-z]+$'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var schema map[string]any
			if err := json.Unmarshal([]byte(tc.schema), &schema); err != nil {
				t.Fatal(err)
			}
			if got := validateArguments(obj(tc.args), schema); got != tc.want {
				t.Errorf("validate = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateArgumentsReadsGoBuiltSchemas(t *testing.T) {
	// Tool schemas are assembled from Go values ([]string, typed maps), not parsed
	// JSON; the validator must read them the same.
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"message": map[string]any{"type": "string"}},
		"required":   []string{"message"},
	}
	if got := validateArguments(map[string]any{}, schema); got != "'message' is a required property" {
		t.Errorf("validate = %q", got)
	}
	if got := validateArguments(map[string]any{"message": "x"}, schema); got != "" {
		t.Errorf("validate = %q", got)
	}
	if got := validateArguments(nil, schema); !strings.Contains(got, "required") {
		t.Errorf("nil arguments = %q", got)
	}
}

func TestCyclicSchemasTerminate(t *testing.T) {
	// A cycle recurses without consuming the instance, so the depth cap alone
	// would still allow work exponential in the number of branches; the step
	// budget bounds it. Only termination is asserted: what a self-referencing
	// schema means is undefined.
	for _, src := range []string{
		`{"oneOf":[{"$ref":"#"},{"type":"string"}]}`,
		`{"not":{"$ref":"#"}}`,
		`{"properties":{"a":{"$ref":"#"}},"items":{"$ref":"#"},"if":{"$ref":"#"},"then":{"$ref":"#"},"else":{"$ref":"#"},"anyOf":[{"$ref":"#"},{"$ref":"#"}],"allOf":[{"$ref":"#"},{"$ref":"#"}]}`,
	} {
		var schema map[string]any
		if err := json.Unmarshal([]byte(src), &schema); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			validateArguments(map[string]any{"a": map[string]any{"a": map[string]any{}}}, schema)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("validation of %s did not terminate", src)
		}
	}
}

func TestLargeInstancesDoNotSpendTheStepBudget(t *testing.T) {
	// The budget caps keyword chains on one value, renewed per property and item:
	// a long array must still be checked to its last element.
	for _, tc := range []struct {
		name   string
		schema string
	}{
		{"plain items", `{"type":"object","properties":{"l":{"type":"array","items":{"type":"integer"}}}}`},
		{"applicators on every item", `{"type":"object","properties":{"l":{"type":"array","items":{"anyOf":[{"type":"integer"},{"type":"null"}],"allOf":[{"not":{"type":"string"}}]}}}}`},
		{"a $ref on every item", `{"type":"object","properties":{"l":{"type":"array","items":{"$ref":"#/$defs/n"}}},"$defs":{"n":{"type":"integer"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var schema map[string]any
			if err := json.Unmarshal([]byte(tc.schema), &schema); err != nil {
				t.Fatal(err)
			}
			list := make([]any, 60000)
			for i := range list {
				list[i] = float64(i)
			}
			list = append(list, "bad tail")
			got := validateArguments(map[string]any{"l": list}, schema)
			if !strings.Contains(got, "'bad tail'") {
				t.Errorf("validate = %q, want the bad tail found after 60000 good items", got)
			}
		})
	}
}

func TestPatternCache(t *testing.T) {
	a, b := compilePattern("^cache_test_[0-9]+$"), compilePattern("^cache_test_[0-9]+$")
	if a == nil || a != b {
		t.Errorf("a pattern was recompiled: %p vs %p", a, b)
	}
	if compilePattern("^(?=lookahead)") != nil || compilePattern("^(?=lookahead)") != nil {
		t.Error("a pattern RE2 cannot compile produced a regexp")
	}
	if _, cached := patternCache.Load("^(?=lookahead)"); !cached {
		t.Error("an uncompilable pattern is not remembered")
	}
}

func TestRecursiveSchemasStillValidateTheInstance(t *testing.T) {
	// A $ref that recurses *through the instance* (a tree) is legitimate: cutting
	// cycles on one value must not stop it from checking nested values.
	schema := map[string]any{}
	if err := json.Unmarshal([]byte(`{"$ref":"#/$defs/node","$defs":{"node":{"type":"object","properties":{"v":{"type":"integer"},"child":{"$ref":"#/$defs/node"}}}}}`), &schema); err != nil {
		t.Fatal(err)
	}
	ok := map[string]any{"v": 1.0, "child": map[string]any{"v": 2.0, "child": map[string]any{"v": 3.0}}}
	if got := validateArguments(ok, schema); got != "" {
		t.Errorf("valid tree = %q", got)
	}
	bad := map[string]any{"v": 1.0, "child": map[string]any{"v": 2.0, "child": map[string]any{"v": "deep"}}}
	if got := validateArguments(bad, schema); got != "'deep' is not of type 'integer'" {
		t.Errorf("tree with a bad leaf = %q", got)
	}
}

func TestExponentialFanOutIsBounded(t *testing.T) {
	// Not a cycle: a finite chain of $defs whose anyOf lists double per level
	// (2^40 paths). The per-value budget stops it; only termination is asserted.
	defs := map[string]any{"d0": map[string]any{"type": "string"}}
	for i := 1; i <= 40; i++ {
		prev := map[string]any{"$ref": "#/$defs/d" + strconv.Itoa(i-1)}
		defs["d"+strconv.Itoa(i)] = map[string]any{"anyOf": []any{prev, prev}}
	}
	schema := map[string]any{"$defs": defs, "properties": map[string]any{"x": map[string]any{"$ref": "#/$defs/d40"}}}
	done := make(chan struct{})
	go func() {
		validateArguments(map[string]any{"x": 1.0}, schema)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("an exponential schema was not bounded")
	}
}
