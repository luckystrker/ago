package response

import (
	"reflect"
	"testing"
	"time"
)

type status string

type address struct {
	City string `json:"city"`
}

type person struct {
	Name     string            `json:"name" agschema:"description=Full name,required"`
	Age      int               `json:"age" agschema:"min=0,max=150"`
	Role     status            `json:"role" agschema:"enum=admin|user,required"`
	Scores   []float64         `json:"scores"`
	Address  address           `json:"address"`
	Attrs    map[string]string `json:"attrs"`
	Joined   time.Time         `json:"joined"`
	Nick     *string           `json:"nick"`
	Blob     []byte            `json:"blob"`
	Internal string            // untagged: Go field name, encoding/json parity
	hidden   string            // unexported: skipped
}

func personSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":     map[string]any{"type": "string", "description": "Full name"},
			"age":      map[string]any{"type": "integer", "minimum": int64(0), "maximum": int64(150)},
			"role":     map[string]any{"type": "string", "enum": []any{"admin", "user"}},
			"scores":   map[string]any{"type": "array", "items": map[string]any{"type": "number"}},
			"address":  map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []string{}},
			"attrs":    map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
			"joined":   map[string]any{"type": "string", "format": "date-time"},
			"nick":     map[string]any{"type": "string"},
			"blob":     map[string]any{"type": "string"},
			"Internal": map[string]any{"type": "string"},
		},
		"required": []string{"name", "role"},
	}
}

func TestSchemaOfScalarsWrapped(t *testing.T) {
	tests := []struct {
		name string
		typ  reflect.Type
		want map[string]any
	}{
		{"string", reflect.TypeOf(""), wrapDataSchema(map[string]any{"type": "string"})},
		{"int", reflect.TypeOf(int(0)), wrapDataSchema(map[string]any{"type": "integer"})},
		{"uint8", reflect.TypeOf(uint8(0)), wrapDataSchema(map[string]any{"type": "integer"})},
		{"float64", reflect.TypeOf(float64(0)), wrapDataSchema(map[string]any{"type": "number"})},
		{"bool", reflect.TypeOf(false), wrapDataSchema(map[string]any{"type": "boolean"})},
		{"named string", reflect.TypeOf(status("")), wrapDataSchema(map[string]any{"type": "string"})},
		{"pointer to int", reflect.TypeOf((*int)(nil)), wrapDataSchema(map[string]any{"type": "integer"})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SchemaOf(tt.typ)
			if err != nil {
				t.Fatalf("SchemaOf(%s) error: %v", tt.typ, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("SchemaOf(%s) =\n%v\nwant\n%v", tt.typ, got, tt.want)
			}
		})
	}
}

func TestSchemaOfStruct(t *testing.T) {
	got, err := SchemaOf(reflect.TypeOf(person{}))
	if err != nil {
		t.Fatalf("SchemaOf(person) error: %v", err)
	}
	if want := personSchema(); !reflect.DeepEqual(got, want) {
		t.Errorf("SchemaOf(person) =\n%v\nwant\n%v", got, want)
	}
}

func TestSchemaOfCollectionsNotWrapped(t *testing.T) {
	tests := []struct {
		name string
		typ  reflect.Type
		want map[string]any
	}{
		{"slice", reflect.TypeOf([]string{}), map[string]any{
			"type": "array", "items": map[string]any{"type": "string"},
		}},
		{"array", reflect.TypeOf([2]int{}), map[string]any{
			"type": "array", "items": map[string]any{"type": "integer"},
		}},
		{"map", reflect.TypeOf(map[string]int{}), map[string]any{
			"type": "object", "additionalProperties": map[string]any{"type": "integer"},
		}},
		{"time", reflect.TypeOf(time.Time{}), map[string]any{"type": "string", "format": "date-time"}},
		{"pointer to time", reflect.TypeOf((*time.Time)(nil)), map[string]any{"type": "string", "format": "date-time"}},
		{"interface", reflect.TypeOf((*any)(nil)).Elem(), map[string]any{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SchemaOf(tt.typ)
			if err != nil {
				t.Fatalf("SchemaOf(%s) error: %v", tt.typ, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("SchemaOf(%s) =\n%v\nwant\n%v", tt.typ, got, tt.want)
			}
		})
	}
}

func TestSchemaOfStringBounds(t *testing.T) {
	type named struct {
		Alias string `json:"alias" agschema:"min=1,max=10"`
	}
	got, err := SchemaOf(reflect.TypeOf(named{}))
	if err != nil {
		t.Fatalf("SchemaOf error: %v", err)
	}
	want := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"alias": map[string]any{"type": "string", "minLength": 1, "maxLength": 10},
		},
		"required": []string{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got\n%v\nwant\n%v", got, want)
	}
}

func TestSchemaOfEmbeddedFlattens(t *testing.T) {
	type base struct {
		ID string `json:"id" agschema:"required"`
	}
	type extended struct {
		base
		Extra string `json:"extra"`
	}
	got, err := SchemaOf(reflect.TypeOf(extended{}))
	if err != nil {
		t.Fatalf("SchemaOf error: %v", err)
	}
	want := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":    map[string]any{"type": "string"},
			"extra": map[string]any{"type": "string"},
		},
		"required": []string{"id"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got\n%v\nwant\n%v", got, want)
	}
}

func TestSchemaOfErrors(t *testing.T) {
	type funcField struct {
		F func() `json:"f"`
	}
	type mapIntKey struct {
		M map[int]string `json:"m"`
	}
	type badEnum struct {
		E int `json:"e" agschema:"enum=a|b"`
	}
	type unknownDirective struct {
		E string `json:"e" agschema:"weird=1"`
	}
	type boolBound struct {
		B bool `json:"b" agschema:"min=1"`
	}
	type enumOnSlice struct {
		S []string `json:"s" agschema:"enum=a|b"`
	}
	type node struct {
		Next *node `json:"next"`
	}
	tests := []struct {
		name string
		typ  reflect.Type
	}{
		{"nil type", nil},
		{"func field", reflect.TypeOf(funcField{})},
		{"non-string map key", reflect.TypeOf(mapIntKey{})},
		{"bad enum value", reflect.TypeOf(badEnum{})},
		{"unknown directive", reflect.TypeOf(unknownDirective{})},
		{"bound on bool", reflect.TypeOf(boolBound{})},
		{"enum on slice", reflect.TypeOf(enumOnSlice{})},
		{"recursive type", reflect.TypeOf(node{})},
		{"channel", reflect.TypeOf(make(chan int))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := SchemaOf(tt.typ); err == nil {
				t.Errorf("SchemaOf(%v) expected an error, got none", tt.typ)
			}
		})
	}
}
