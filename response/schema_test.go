package response

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type point struct {
	X float64 `json:"x" agschema:"required"`
	Y float64 `json:"y" agschema:"required"`
}

func TestSchemaMetadata(t *testing.T) {
	s := NewSchema[point]("Point", "A 2D point")
	if s.Name() != "Point" {
		t.Errorf("Name() = %q, want %q", s.Name(), "Point")
	}
	if s.Description() != "A 2D point" {
		t.Errorf("Description() = %q", s.Description())
	}
	want, _ := SchemaOf(reflect.TypeOf(point{}))
	if got := s.JSONSchema(); !reflect.DeepEqual(got, want) {
		t.Errorf("JSONSchema() =\n%v\nwant\n%v", got, want)
	}
	prompt := s.SystemPrompt()
	if !strings.Contains(prompt, "valid JSON") || !strings.Contains(prompt, "```json") {
		t.Errorf("SystemPrompt() missing the JSON instruction: %q", prompt)
	}
	if !strings.Contains(prompt, `"x"`) || !strings.Contains(prompt, `"required"`) {
		t.Errorf("SystemPrompt() missing the schema body: %q", prompt)
	}

	unnamed := NewSchema[point]("", "")
	if unnamed.Name() != "ResponseSchema" {
		t.Errorf("default Name() = %q, want %q", unnamed.Name(), "ResponseSchema")
	}
}

func TestSchemaValidateStruct(t *testing.T) {
	s := NewSchema[point]("Point", "")
	v, err := s.Validate(`{"x":1.5,"y":2.5}`, nil)
	if err != nil {
		t.Fatalf("Validate error: %v", err)
	}
	p, ok := v.(point)
	if !ok {
		t.Fatalf("Validate returned %T, want point", v)
	}
	if p != (point{X: 1.5, Y: 2.5}) {
		t.Errorf("Validate returned %+v", p)
	}
}

func TestSchemaValidateScalarEnvelope(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want any
	}{
		{"int", `{"data": 42}`, 42},
		{"string", `{"data": "hi"}`, "hi"},
		{"bool", `{"data": true}`, true},
		{"float", `{"data": 2.5}`, 2.5},
	}
	validate := func(s Proto, raw string) (any, error) { return s.Validate(raw, nil) }
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s Proto
			switch tt.want.(type) {
			case int:
				s = NewSchema[int]("N", "")
			case string:
				s = NewSchema[string]("S", "")
			case bool:
				s = NewSchema[bool]("B", "")
			case float64:
				s = NewSchema[float64]("F", "")
			}
			v, err := validate(s, tt.raw)
			if err != nil {
				t.Fatalf("Validate error: %v", err)
			}
			if v != tt.want {
				t.Errorf("Validate = %v (%T), want %v (%T)", v, v, tt.want, tt.want)
			}
		})
	}
}

func TestSchemaValidateNamedString(t *testing.T) {
	v, err := NewSchema[status]("Role", "").Validate(`{"data":"admin"}`, nil)
	if err != nil {
		t.Fatalf("Validate error: %v", err)
	}
	if v != status("admin") {
		t.Errorf("Validate = %v, want admin", v)
	}
}

func TestSchemaValidateTime(t *testing.T) {
	s := NewSchema[time.Time]("When", "")
	if got, want := s.JSONSchema(), map[string]any{"type": "string", "format": "date-time"}; !reflect.DeepEqual(got, want) {
		t.Errorf("JSONSchema() =\n%v\nwant\n%v (time.Time must not be wrapped)", got, want)
	}
	v, err := s.Validate(`"2026-10-29T12:00:00Z"`, nil)
	if err != nil {
		t.Fatalf("Validate error: %v", err)
	}
	when, ok := v.(time.Time)
	if !ok || when != time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC) {
		t.Errorf("Validate = %v (%T)", v, v)
	}
}

func TestSchemaValidateFailures(t *testing.T) {
	type checked struct {
		A string `json:"a" agschema:"required"`
		B string `json:"b" agschema:"enum=x|y,required"`
	}
	tests := []struct {
		name string
		raw  string
	}{
		{"not json", "not a number"},
		{"empty", ""},
		{"missing required", `{"a":"1"}`},
		{"enum violation", `{"a":"1","b":"z"}`},
		{"wrong shape", `[1,2]`},
		{"wrong member type", `{"a":1,"b":"x"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := NewSchema[checked]("C", "").Validate(tt.raw, nil)
			if err == nil {
				t.Fatalf("Validate(%q) = %v, want an error", tt.raw, v)
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("Validate(%q) error is %T, want *ValidationError", tt.raw, err)
			}
			if ve.Response != tt.raw {
				t.Errorf("ValidationError.Response = %q, want %q", ve.Response, tt.raw)
			}
			if !errors.Is(err, ErrInvalidResponse) {
				t.Errorf("Validate(%q) error does not match ErrInvalidResponse: %v", tt.raw, err)
			}
		})
	}
}

func TestSchemaValidateScalarFailures(t *testing.T) {
	s := NewSchema[int]("N", "")
	for _, raw := range []string{`{}`, `{"data":"42"}`, `42`, `{"data": 1.5}`} {
		if _, err := s.Validate(raw, nil); err == nil {
			t.Errorf("Validate(%q) expected an error", raw)
		}
	}
}

func TestSchemaRoundTrip(t *testing.T) {
	in := person{
		Name:    "Alice",
		Role:    "admin",
		Scores:  []float64{1, 2.5},
		Address: address{City: "Oslo"},
		Attrs:   map[string]string{"k": "v"},
		Joined:  time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}
	v, err := NewSchema[person]("Person", "").Validate(string(b), nil)
	if err != nil {
		t.Fatalf("Validate error: %v", err)
	}
	if got := v.(person); !reflect.DeepEqual(got, in) {
		t.Errorf("round trip = %+v, want %+v", got, in)
	}

	// scalar envelope round trip
	sb, err := json.Marshal(map[string]any{"data": 42})
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}
	sv, err := NewSchema[int]("N", "").Validate(string(sb), nil)
	if err != nil || sv != 42 {
		t.Errorf("scalar round trip = %v, %v", sv, err)
	}
}

func TestSchemaJSONSchemaCopyIsolation(t *testing.T) {
	s := NewSchema[point]("Point", "")
	m := s.JSONSchema()
	m["type"] = "mutated"
	m["properties"].(map[string]any)["x"] = "mutated"
	fresh, _ := SchemaOf(reflect.TypeOf(point{}))
	if got := s.JSONSchema(); !reflect.DeepEqual(got, fresh) {
		t.Errorf("mutating the returned map changed the schema:\n%v", got)
	}
}

func TestSchemaConcurrentUse(t *testing.T) {
	s := NewSchema[point]("Point", "")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Validate(`{"x":1,"y":2}`, nil); err != nil {
				t.Errorf("Validate error: %v", err)
			}
			if _, err := s.Validate(`{"x":"bad"}`, nil); err == nil {
				t.Error("Validate expected an error")
			}
			_ = s.JSONSchema()
			_ = s.SystemPrompt()
		}()
	}
	wg.Wait()
}
