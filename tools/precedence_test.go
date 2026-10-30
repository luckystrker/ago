package tools

import (
	"errors"
	"fmt"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
)

type hostedSchema struct{ typ string }

func (h hostedSchema) SchemaType() string { return h.typ }

func names(r *ResolvedTools) []string {
	out := []string{}
	for _, t := range r.Tools {
		out = append(out, t.Name())
	}
	return out
}

func functionNames(r *ResolvedTools) []string {
	out := []string{}
	for _, s := range r.Schemas {
		switch s := s.(type) {
		case Schema:
			out = append(out, s.Name)
		case *Schema:
			out = append(out, s.Name)
		case ToolSchema:
			out = append(out, s.SchemaType())
		}
	}
	return out
}

func fnTool(t *testing.T, name string) *FunctionTool {
	t.Helper()
	return mustFunctionTool(t, name, "", func() string { return "" })
}

func containsToolIdentity(ts []Tool, want Tool) bool {
	for _, t := range ts {
		if t == want {
			return true
		}
	}
	return false
}

func toolkitNames(tk *Toolkit) []string {
	out := []string{}
	for _, m := range tk.Tools() {
		out = append(out, m.Name())
	}
	return out
}

// hostedTool is a schema-only carrier for a hosted (provider-executed) tool.
type hostedTool struct {
	name string
	typ  string
}

func (h *hostedTool) Name() string         { return h.name }
func (h *hostedTool) DeclaredInCode() bool { return true }
func (h *hostedTool) Schemas(*agocontext.Context) ([]any, error) {
	return []any{hostedSchema{typ: h.typ}}, nil
}
func (h *hostedTool) Register(*agocontext.Context, []middleware.Middleware) ([]stream.SubID, error) {
	return nil, nil
}

type failingTool struct{}

func (failingTool) Name() string         { return "failing" }
func (failingTool) DeclaredInCode() bool { return true }
func (failingTool) Schemas(*agocontext.Context) ([]any, error) {
	return nil, fmt.Errorf("discovery failed: %w", ErrResolution)
}
func (failingTool) Register(*agocontext.Context, []middleware.Middleware) ([]stream.SubID, error) {
	return nil, nil
}

type junkSchemaTool struct{}

func (junkSchemaTool) Name() string         { return "junk" }
func (junkSchemaTool) DeclaredInCode() bool { return true }
func (junkSchemaTool) Schemas(*agocontext.Context) ([]any, error) {
	return []any{42}, nil
}
func (junkSchemaTool) Register(*agocontext.Context, []middleware.Middleware) ([]stream.SubID, error) {
	return nil, nil
}

func TestResolveToolsNoTools(t *testing.T) {
	_, cx := newTestContext()
	r, err := ResolveTools(nil, cx)
	if err != nil {
		t.Fatalf("ResolveTools: %v", err)
	}
	if len(r.Tools) != 0 || len(r.Schemas) != 0 || len(r.Known) != 0 {
		t.Fatalf("empty resolution = %+v", r)
	}
}

func TestResolveToolsLaterDeclaredToolWins(t *testing.T) {
	_, cx := newTestContext()
	first := fnTool(t, "deploy")
	second := fnTool(t, "deploy")
	r, err := ResolveTools([]Tool{first, second}, cx)
	if err != nil {
		t.Fatalf("ResolveTools: %v", err)
	}
	got := names(r)
	if len(got) != 1 || got[0] != "deploy" {
		t.Fatalf("resolved = %v, want one deploy", got)
	}
	// The later tool won: it is the one kept.
	if !containsToolIdentity(r.Tools, second) || containsToolIdentity(r.Tools, first) {
		t.Fatalf("the later declared tool must win")
	}
	if fn := functionNames(r); len(fn) != 1 || fn[0] != "deploy" {
		t.Fatalf("schemas = %v, want [deploy]", fn)
	}
	if !r.Known["deploy"] {
		t.Fatalf("known = %v, want deploy", r.Known)
	}
}

func TestResolveToolsRuntimeToolNeverOverridesCode(t *testing.T) {
	_, cx := newTestContext()
	code := fnTool(t, "deploy")
	remote, err := NewClientTool(map[string]any{"name": "deploy"})
	if err != nil {
		t.Fatalf("NewClientTool: %v", err)
	}
	for _, order := range [][]Tool{{code, remote}, {remote, code}} {
		r, err := ResolveTools(order, cx)
		if err != nil {
			t.Fatalf("ResolveTools: %v", err)
		}
		if got := names(r); len(got) != 1 || got[0] != "deploy" {
			t.Fatalf("resolved = %v, want the code tool", got)
		}
		if _, isFT := r.Tools[0].(*FunctionTool); !isFT {
			t.Fatalf("resolved tool type %T, want FunctionTool", r.Tools[0])
		}
		if !containsToolIdentity(r.Tools, code) {
			t.Fatalf("the code-declared tool must win regardless of position")
		}
	}
}

func TestResolveToolsRemoteVsRemoteFirstWins(t *testing.T) {
	_, cx := newTestContext()
	a, _ := NewClientTool(map[string]any{"name": "shared"})
	b, _ := NewClientTool(map[string]any{"name": "shared"})
	r, err := ResolveTools([]Tool{a, b}, cx)
	if err != nil {
		t.Fatalf("ResolveTools: %v", err)
	}
	if got := names(r); len(got) != 1 {
		t.Fatalf("resolved = %v, want one shared", got)
	}
	if !containsToolIdentity(r.Tools, a) || containsToolIdentity(r.Tools, b) {
		t.Fatalf("the first remote tool must win")
	}
}

func TestResolveToolsHostedSchemasMayRepeat(t *testing.T) {
	_, cx := newTestContext()
	h1 := &hostedTool{name: "hosted1", typ: "web_search"}
	h2 := &hostedTool{name: "hosted2", typ: "web_search"}
	r, err := ResolveTools([]Tool{h1, h2}, cx)
	if err != nil {
		t.Fatalf("ResolveTools: %v", err)
	}
	if got := names(r); len(got) != 2 {
		t.Fatalf("resolved = %v, want both hosted tools", got)
	}
	if fn := functionNames(r); len(fn) != 2 || fn[0] != "web_search" {
		t.Fatalf("schemas = %v", fn)
	}
	if !r.Known["web_search"] {
		t.Fatalf("known = %v", r.Known)
	}
}

func TestResolveToolsFunctionNameClashesWithHostedType(t *testing.T) {
	_, cx := newTestContext()
	h := &hostedTool{name: "hosted", typ: "web_search"}
	f := fnTool(t, "web_search")
	r, err := ResolveTools([]Tool{h, f}, cx)
	if err != nil {
		t.Fatalf("ResolveTools: %v", err)
	}
	if got := names(r); len(got) != 1 {
		t.Fatalf("resolved = %v, want the function tool only", got)
	}
	if _, isFT := r.Tools[0].(*FunctionTool); !isFT {
		t.Fatalf("resolved tool type %T, want FunctionTool", r.Tools[0])
	}
}

func TestResolveToolsPrunesToolkitMembers(t *testing.T) {
	_, cx := newTestContext()
	member1 := fnTool(t, "deploy")
	member2 := fnTool(t, "other")
	tk, err := NewToolkit("tk", nil, member1, member2)
	if err != nil {
		t.Fatalf("NewToolkit: %v", err)
	}
	code := fnTool(t, "deploy")

	// Runtime/code tool after the toolkit: the toolkit's deploy member loses and
	// is pruned from the toolkit for this turn.
	r, err := ResolveTools([]Tool{tk, code}, cx)
	if err != nil {
		t.Fatalf("ResolveTools: %v", err)
	}
	if got := names(r); len(got) != 2 || got[0] != "tk" || got[1] != "deploy" {
		t.Fatalf("resolved = %v, want [tk deploy]", got)
	}
	pruned := r.Tools[0].(*Toolkit)
	if got := toolkitNames(pruned); len(got) != 1 || got[0] != "other" {
		t.Fatalf("pruned members = %v, want [other]", got)
	}
	if fn := functionNames(r); len(fn) != 2 || fn[0] != "other" || fn[1] != "deploy" {
		t.Fatalf("schemas = %v, want [other deploy]", fn)
	}
	if !r.Known["deploy"] || !r.Known["other"] {
		t.Fatalf("known = %v, want deploy and other", r.Known)
	}

	// The original toolkit is never mutated.
	if got := toolkitNames(tk); len(got) != 2 {
		t.Fatalf("original toolkit mutated: %v", got)
	}
}

func TestResolveToolsNestedToolkitPruning(t *testing.T) {
	_, cx := newTestContext()
	inner, _ := NewToolkit("inner", nil, fnTool(t, "deploy"), fnTool(t, "keep"))
	outer, _ := NewToolkit("outer", nil, inner)
	code := fnTool(t, "deploy")
	r, err := ResolveTools([]Tool{outer, code}, cx)
	if err != nil {
		t.Fatalf("ResolveTools: %v", err)
	}
	if got := names(r); len(got) != 2 || got[0] != "outer" || got[1] != "deploy" {
		t.Fatalf("resolved = %v, want [outer deploy]", got)
	}
	gotInner := r.Tools[0].(*Toolkit).Tools()[0].(*Toolkit)
	if got := toolkitNames(gotInner); len(got) != 1 || got[0] != "keep" {
		t.Fatalf("nested toolkit not pruned: %v", got)
	}
	if fn := functionNames(r); len(fn) != 2 || fn[0] != "keep" || fn[1] != "deploy" {
		t.Fatalf("schemas = %v, want [keep deploy]", fn)
	}
}

func TestResolveToolsSchemaErrorPropagates(t *testing.T) {
	_, cx := newTestContext()
	r, err := ResolveTools([]Tool{failingTool{}}, cx)
	if err == nil || !errors.Is(err, ErrResolution) {
		t.Fatalf("err = %v, want ErrResolution", err)
	}
	if r != nil {
		t.Fatalf("resolution result = %+v, want nil", r)
	}
}

func TestResolveToolsUnsupportedSchemaType(t *testing.T) {
	_, cx := newTestContext()
	_, err := ResolveTools([]Tool{junkSchemaTool{}}, cx)
	if err == nil || !errors.Is(err, ErrResolution) {
		t.Fatalf("err = %v, want ErrResolution", err)
	}
}
