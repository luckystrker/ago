package response

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/stream"
)

// answer is the type the typed-callable tests decode into.
type answer struct {
	OK     bool   `json:"ok" agschema:"required"`
	Reason string `json:"reason"`
}

// TestCallableSchemaDecodesThenValidates pins the decorator contract: the
// response is decoded into T first, the caller's function receives the typed
// value, and its result is what validation returns.
func TestCallableSchemaDecodesThenValidates(t *testing.T) {
	var seen answer
	s := NewCallableSchema(func(_ *agocontext.Context, v answer) (any, error) {
		seen = v
		return "verdict:" + v.Reason, nil
	})
	if s == nil {
		t.Fatal("NewCallableSchema returned nil")
	}

	v, err := s.Validate(`{"ok":true,"reason":"because"}`, nil)
	if err != nil {
		t.Fatalf("Validate error: %v", err)
	}
	if seen != (answer{OK: true, Reason: "because"}) {
		t.Fatalf("validator saw %+v, want the decoded value", seen)
	}
	if v != "verdict:because" {
		t.Fatalf("Validate = %v, want the validator's value", v)
	}
}

// TestCallableSchemaBadResponse pins that the shared schema machinery rejects a
// malformed answer before the validator runs.
func TestCallableSchemaBadResponse(t *testing.T) {
	ran := false
	s := NewCallableSchema(func(*agocontext.Context, answer) (any, error) {
		ran = true
		return nil, nil
	})
	_, err := s.Validate("not json", nil)
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("Validate error = %v, want ErrInvalidResponse", err)
	}
	if ran {
		t.Fatal("the validator must not run on an undecodable response")
	}
}

// TestCallableSchemaInheritsSchemaMetadata pins that the typed path shares
// Schema[T]: the generated JSON schema, the prompt, and the scalar envelope rule
// (with the embed opt-out).
func TestCallableSchemaInheritsSchemaMetadata(t *testing.T) {
	s := NewCallableSchema(func(*agocontext.Context, answer) (any, error) { return nil, nil })
	plain := NewSchema[answer]("", "")
	if !reflect.DeepEqual(s.JSONSchema(), plain.JSONSchema()) {
		t.Fatalf("JSONSchema = %v, want the generated one %v", s.JSONSchema(), plain.JSONSchema())
	}
	if s.Name() != "ResponseSchema" || s.SystemPrompt() == "" {
		t.Fatalf("metadata = %q / prompt %q, want the schema defaults", s.Name(), s.SystemPrompt())
	}

	// A bare scalar keeps the {"data": ...} envelope unless told otherwise.
	scalar := NewCallableSchema(func(_ *agocontext.Context, n int) (any, error) { return n, nil })
	if v, err := scalar.Validate(`{"data":7}`, nil); err != nil || v != 7 {
		t.Fatalf("scalar Validate = %v, %v; want 7 through the envelope", v, err)
	}
	bare := NewCallableSchema(func(_ *agocontext.Context, n int) (any, error) { return n, nil },
		WithCallableEmbed(false))
	if v, err := bare.Validate(`7`, nil); err != nil || v != 7 {
		t.Fatalf("bare Validate = %v, %v; want 7 without the envelope", v, err)
	}
	if strings.Contains(bare.SystemPrompt(), `"data"`) {
		t.Fatalf("bare prompt still asks for the envelope: %q", bare.SystemPrompt())
	}
}

// TestCallableSchemaOptions pins the metadata overrides (Python parity:
// response_schema(name=, description=, schema=)).
func TestCallableSchemaOptions(t *testing.T) {
	js := map[string]any{"type": "object"}
	s := NewCallableSchema(func(*agocontext.Context, answer) (any, error) { return "x", nil },
		WithCallableName("Verdict"), WithCallableDescription("yes or no"), WithCallableJSONSchema(js))
	if s.Name() != "Verdict" || s.Description() != "yes or no" {
		t.Fatalf("metadata = %q/%q", s.Name(), s.Description())
	}
	if !reflect.DeepEqual(s.JSONSchema(), js) {
		t.Fatalf("JSONSchema = %v, want the override %v", s.JSONSchema(), js)
	}
}

// TestCallableSchemaDependencyInjection pins the DI seam: the validator reads
// what it needs from the conversation instead of taking injected parameters.
func TestCallableSchemaDependencyInjection(t *testing.T) {
	s := NewCallableSchema(func(ctx *agocontext.Context, v answer) (any, error) {
		tenant, ok := ctx.Dependency("tenant")
		if !ok {
			return nil, errors.New("no tenant dependency")
		}
		return tenant.(string) + "/" + v.Reason, nil
	})
	conv := agocontext.New(stream.NewMemoryStream())
	conv.SetDependency("tenant", "acme")
	v, err := s.Validate(`{"ok":true,"reason":"row"}`, conv)
	if err != nil {
		t.Fatalf("Validate error: %v", err)
	}
	if v != "acme/row" {
		t.Fatalf("Validate = %v, want acme/row", v)
	}
	if _, err := s.Validate(`{"ok":true}`, agocontext.New(stream.NewMemoryStream())); err == nil {
		t.Fatal("a missing dependency must surface as the validator's error")
	}
}

// TestCallableSchemaNilValidator pins the free-text case.
func TestCallableSchemaNilValidator(t *testing.T) {
	if NewCallableSchema[answer](nil) != nil {
		t.Fatal("NewCallableSchema(nil) must return nil (free text)")
	}
	if NewRawCallable(nil) != nil {
		t.Fatal("NewRawCallable(nil) must return nil (free text)")
	}
}

// TestRawCallable pins the escape hatch: the validator gets the response text,
// untouched, and there is no schema machinery behind it.
func TestRawCallable(t *testing.T) {
	js := map[string]any{"type": "string"}
	s := NewRawCallable(func(_ *agocontext.Context, raw string) (any, error) {
		return "raw:" + raw, nil
	}, WithCallableJSONSchema(js))
	if s.Name() != "ResponseSchema" {
		t.Fatalf("default Name() = %q", s.Name())
	}
	if s.SystemPrompt() != "" {
		t.Fatalf("raw callable prompt = %q, want empty", s.SystemPrompt())
	}
	if !reflect.DeepEqual(s.JSONSchema(), js) {
		t.Fatalf("JSONSchema = %v, want %v", s.JSONSchema(), js)
	}
	v, err := s.Validate("plain text", nil)
	if err != nil {
		t.Fatalf("Validate error: %v", err)
	}
	if v != "raw:plain text" {
		t.Fatalf("Validate = %v, want the raw text passed through", v)
	}
}

// TestCallableSchemaValidationError pins that a validator's error reaches the
// agent's retry loop as a *ValidationError (it is the validator's to build).
func TestCallableSchemaValidationError(t *testing.T) {
	s := NewCallableSchema(func(*agocontext.Context, answer) (any, error) {
		return nil, &ValidationError{Err: errors.New("not a verdict"), Response: "{}"}
	})
	_, err := s.Validate(`{"ok":true}`, nil)
	var ve *ValidationError
	if !errors.As(err, &ve) || !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("Validate error = %v, want *ValidationError", err)
	}
}

// TestCallableSchemaJSONRoundTrip pins that the schema a provider sees is the
// generated one, wire-encodable.
func TestCallableSchemaJSONRoundTrip(t *testing.T) {
	s := NewCallableSchema(func(*agocontext.Context, answer) (any, error) { return nil, nil })
	b, err := json.Marshal(s.JSONSchema())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"ok"`) {
		t.Fatalf("JSONSchema JSON = %s, want the decoded type's members", b)
	}
}
