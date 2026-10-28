package response

import (
	"errors"
	"strings"
	"testing"
)

func TestPromptedSchemaDelegates(t *testing.T) {
	inner := NewSchema[int]("N", "a number")
	p := NewPromptedSchema(inner)
	if p.Name() != "N" || p.Description() != "a number" {
		t.Errorf("Name/Description = %q/%q, want N/a number", p.Name(), p.Description())
	}
	if got := p.JSONSchema(); got != nil {
		t.Errorf("JSONSchema() = %v, want nil (prompt-driven, no native schema)", got)
	}
	prompt := p.SystemPrompt()
	if !strings.Contains(prompt, "valid JSON") || !strings.Contains(prompt, "```json") {
		t.Errorf("SystemPrompt() missing the JSON instruction: %q", prompt)
	}
	if !strings.Contains(prompt, `"data"`) || !strings.Contains(prompt, `"required"`) {
		t.Errorf("SystemPrompt() missing the inner schema: %q", prompt)
	}
}

func TestPromptedSchemaValidateDelegates(t *testing.T) {
	p := NewPromptedSchema(NewSchema[int]("N", ""))
	v, err := p.Validate(`{"data": 7}`, nil)
	if err != nil {
		t.Fatalf("Validate error: %v", err)
	}
	if v != 7 {
		t.Errorf("Validate = %v, want 7", v)
	}
	if _, err := p.Validate("nope", nil); err == nil {
		t.Fatal("Validate expected an error")
	} else {
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("delegated error is %T, want *ValidationError", err)
		}
	}
}

func TestPromptedSchemaCustomTemplate(t *testing.T) {
	p := NewPromptedSchema(NewSchema[int]("N", ""), WithPromptTemplate("SCHEMA: {schema} END"))
	want := "SCHEMA: " + mustJSON(t, NewSchema[int]("N", "").JSONSchema()) + " END"
	if got := p.SystemPrompt(); got != want {
		t.Errorf("SystemPrompt() = %q, want %q", got, want)
	}
}

func TestPromptedSchemaWithoutSchema(t *testing.T) {
	p := NewPromptedSchema(RawSchema(nil, WithRawName("raw")))
	if p.SystemPrompt() != "" {
		t.Errorf("SystemPrompt() = %q, want empty for a schema-less inner", p.SystemPrompt())
	}
}

func TestPromptedSchemaNilInner(t *testing.T) {
	if p := NewPromptedSchema(nil); p != nil {
		t.Errorf("NewPromptedSchema(nil) = %v, want nil", p)
	}
}
