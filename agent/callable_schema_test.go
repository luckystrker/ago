package agent

import (
	stdcontext "context"
	"errors"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/response"
	agotest "github.com/luckystrker/ago/testing"
)

// verdictAnswer is what the callable-schema turns decode into.
type verdictAnswer struct {
	OK     bool   `json:"ok" agschema:"required"`
	Reason string `json:"reason"`
}

// TestCallableResponseSchemaValidatesTheTurn pins that a function-as-schema
// (response.NewCallableSchema) is a first-class response schema: the agent decodes
// the final answer into the schema's type, then runs the validator with the
// conversation context, which is the dependency-injection seam it reads from.
func TestCallableResponseSchemaValidatesTheTurn(t *testing.T) {
	var seen verdictAnswer
	schema := response.NewCallableSchema(func(ctx *agocontext.Context, v verdictAnswer) (any, error) {
		seen = v
		if _, ok := ctx.Dependency("tenant"); !ok {
			return nil, errors.New("no tenant in the conversation")
		}
		return v, nil
	})
	cfg := agotest.NewTestConfig(&events.ModelResponse{
		Message: &events.ModelMessage{Content: `{"ok":true,"reason":"because"}`},
	})
	a := New("validator", "", WithConfig(cfg), WithResponseSchema(schema),
		WithDependencies(map[string]any{"tenant": "acme"}))

	reply, err := a.Ask(stdcontext.Background(), "decide")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	got, err := reply.Content()
	if err != nil {
		t.Fatalf("Content: %v", err)
	}
	if string(got) != `{"ok":true,"reason":"because"}` {
		t.Fatalf("Content = %s, want the validated raw answer", got)
	}
	if seen != (verdictAnswer{OK: true, Reason: "because"}) {
		t.Fatalf("validator saw %+v, want the decoded answer", seen)
	}
}

// TestCallableResponseSchemaFailureReachesTheCaller pins that a rejecting
// validator surfaces as the agent's validation error, not a silent pass.
func TestCallableResponseSchemaFailureReachesTheCaller(t *testing.T) {
	schema := response.NewCallableSchema(func(*agocontext.Context, verdictAnswer) (any, error) {
		return nil, &response.ValidationError{Err: errors.New("not a verdict")}
	})
	cfg := agotest.NewTestConfig(&events.ModelResponse{
		Message: &events.ModelMessage{Content: `{"ok":false}`},
	})
	a := New("validator", "", WithConfig(cfg), WithResponseSchema(schema))
	reply, err := a.Ask(stdcontext.Background(), "decide")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if _, err := reply.Content(); !errors.Is(err, response.ErrInvalidResponse) {
		t.Fatalf("Content error = %v, want ErrInvalidResponse", err)
	}
}
