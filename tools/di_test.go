package tools

import (
	"errors"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/stream"
)

// TestResolveDILookupPropagatesSetVariableError: materializing a declared
// default is a context write now, so a scope conflict must reach the caller
// instead of being silently dropped.
func TestResolveDILookupPropagatesSetVariableError(t *testing.T) {
	parent, err := agocontext.NewVariables(map[string]any{})
	if err != nil {
		t.Fatalf("new parent: %v", err)
	}
	child := agocontext.DeriveChildVariables(parent)
	cx := agocontext.NewWithVariables(stream.NewMemoryStream(), nil, child)
	slot := diSlot{
		kind:       diVariable,
		key:        "new",
		hasDefault: true,
		def:        agocontext.ScopedValue{Value: 1, Scope: agocontext.ScopeShared},
	}
	v, ok, err := resolveDILookup(cx, slot)
	if err == nil {
		t.Fatalf("expected a scope error, got value %v (ok=%v)", v, ok)
	}
	if !errors.Is(err, agocontext.ErrScope) {
		t.Fatalf("error %v does not wrap context.ErrScope", err)
	}
}
