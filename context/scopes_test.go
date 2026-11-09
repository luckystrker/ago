package context

import (
	"errors"
	"slices"
	"sort"
	"sync"
	"testing"
)

// widget proves the explicit Clone hook wins over the reflection fallback.
type widget struct{ N int }

func (w widget) Clone() any { return widget{N: w.N + 100} }

// treeNode exercises cycle handling in the reflection fallback.
type treeNode struct {
	N    int
	Next *treeNode
}

// hiddenFields pins the documented limit: unexported struct fields are
// shallow-copied because reflection cannot set them.
type hiddenFields struct {
	Public  map[string]int
	private map[string]int
}

func TestDeepCopyUsesCloneHook(t *testing.T) {
	got := DeepCopy(widget{N: 7})
	w, ok := got.(widget)
	if !ok {
		t.Fatalf("DeepCopy returned %T, want widget", got)
	}
	if w.N != 107 {
		t.Fatalf("Clone hook not used: N=%d, want 107", w.N)
	}
}

func TestDeepCopyReflectionFallback(t *testing.T) {
	type payload struct {
		M map[string][]int
		S []string
	}
	in := &payload{
		M: map[string][]int{"k": {1, 2}},
		S: []string{"a", "b"},
	}
	out, ok := DeepCopy(in).(*payload)
	if !ok {
		t.Fatalf("DeepCopy returned %T, want *payload", DeepCopy(in))
	}
	if out == in {
		t.Fatal("pointer must be copied")
	}
	out.M["k"][0] = 99
	out.S[0] = "changed"
	if in.M["k"][0] != 1 || in.S[0] != "a" {
		t.Fatalf("copy aliases the original: %+v", in)
	}
}

func TestDeepCopyPreservesCycles(t *testing.T) {
	in := &treeNode{N: 1}
	in.Next = in
	out, ok := DeepCopy(in).(*treeNode)
	if !ok {
		t.Fatalf("DeepCopy returned %T, want *treeNode", DeepCopy(in))
	}
	if out == in {
		t.Fatal("root pointer must be copied")
	}
	if out.Next != out {
		t.Fatal("cycle was not preserved")
	}
}

func TestDeepCopyUnexportedFieldsAreShallow(t *testing.T) {
	in := hiddenFields{
		Public:  map[string]int{"a": 1},
		private: map[string]int{"a": 1},
	}
	out := DeepCopy(in).(hiddenFields)
	out.Public["a"] = 2
	if in.Public["a"] != 1 {
		t.Fatal("exported map must be deep-copied")
	}
	out.private["a"] = 2
	if in.private["a"] != 2 {
		t.Fatal("unexported map must stay shared (documented limit)")
	}
}

func TestScopedValueValidation(t *testing.T) {
	if _, err := NewScopedValue(1, ScopeCopied); err != nil {
		t.Fatalf("copied: %v", err)
	}
	if _, err := NewScopedValue(1, ""); err != nil {
		t.Fatalf("empty scope must default to copied: %v", err)
	}
	_, err := NewScopedValue(1, Scope("bogus"))
	var se *ScopeError
	if !errors.As(err, &se) {
		t.Fatalf("want *ScopeError, got %v", err)
	}
	if !errors.Is(err, ErrScope) {
		t.Fatalf("want errors.Is ErrScope, got %v", err)
	}
}

func TestDeriveChildVariablesScopes(t *testing.T) {
	root, err := NewVariables(map[string]any{
		"copied": map[string]any{"n": 1},
		"local":  ScopedValue{Value: "root only", Scope: ScopeLocal},
		"shared": ScopedValue{Value: 0, Scope: ScopeShared},
		"ag:x":   "control",
		"a2a:y":  "control",
	})
	if err != nil {
		t.Fatalf("new root: %v", err)
	}

	child := DeriveChildVariables(root)
	if _, ok := child.Get("local"); ok {
		t.Fatal("LOCAL must not be inherited")
	}
	for _, key := range []string{"ag:x", "a2a:y"} {
		if _, ok := child.Get(key); ok {
			t.Fatalf("reserved key %q must not be inherited", key)
		}
	}
	if v, ok := child.Get("shared"); !ok || v != 0 {
		t.Fatalf("shared = %v (ok=%v), want 0", v, ok)
	}
	if child.IsRoot() {
		t.Fatal("derived store must not be root")
	}

	// COPIED is a deep copy: mutating the child map cannot touch the root.
	cv, _ := child.Get("copied")
	cv.(map[string]any)["n"] = 2
	rv, _ := root.Get("copied")
	if rv.(map[string]any)["n"] != 1 {
		t.Fatal("COPIED value aliases the parent")
	}

	// SHARED rebinding is visible in the root and in a sibling branch.
	sibling := DeriveChildVariables(root)
	if err := child.Set("shared", 7); err != nil {
		t.Fatalf("set shared: %v", err)
	}
	if v, _ := root.Get("shared"); v != 7 {
		t.Fatalf("root shared = %v, want 7", v)
	}
	if v, _ := sibling.Get("shared"); v != 7 {
		t.Fatalf("sibling shared = %v, want 7", v)
	}
	// The root's LOCAL value never left the root.
	if v, _ := root.Get("local"); v != "root only" {
		t.Fatalf("root local = %v", v)
	}
}

func TestSharedDeclarations(t *testing.T) {
	root, err := NewVariables(map[string]any{
		"x": ScopedValue{Value: 1, Scope: ScopeCopied},
	})
	if err != nil {
		t.Fatalf("new root: %v", err)
	}
	child := DeriveChildVariables(root)

	err = child.SetScoped("new", 1, ScopeShared)
	var se *ScopeError
	if !errors.As(err, &se) {
		t.Fatalf("new shared below root: want *ScopeError, got %v", err)
	}
	if err := child.Set("other", ScopedValue{Value: 1, Scope: ScopeShared}); err == nil {
		t.Fatal("declaring via Set(ScopedValue) must fail too")
	}

	if err := root.SetScoped("x", 2, ScopeShared); err == nil {
		t.Fatal("redeclaring a different scope must fail")
	}
	if err := root.SetScoped("x", 2, ScopeCopied); err != nil {
		t.Fatalf("same scope redeclaration must pass: %v", err)
	}

	_, err = NewVariables(map[string]any{
		"ag:private": ScopedValue{Value: 1, Scope: ScopeShared},
	})
	if !errors.As(err, &se) {
		t.Fatalf("reserved shared declaration: want *ScopeError, got %v", err)
	}

	if _, err := NewVariables(map[string]any{
		"x": ScopedValue{Value: 1, Scope: Scope("bogus")},
	}); !errors.As(err, &se) {
		t.Fatalf("struct literal with unknown scope: want *ScopeError, got %v", err)
	}
}

func TestVariablesSnapshotDeleteAndKeys(t *testing.T) {
	root, err := NewVariables(map[string]any{
		"own":    1,
		"shared": ScopedValue{Value: "s", Scope: ScopeShared},
	})
	if err != nil {
		t.Fatalf("new root: %v", err)
	}
	keys := root.Keys()
	sort.Strings(keys)
	if len(keys) != 2 || keys[0] != "own" || keys[1] != "shared" {
		t.Fatalf("keys = %v", keys)
	}
	if root.Len() != 2 {
		t.Fatalf("len = %d", root.Len())
	}
	snap := root.Snapshot()
	if snap["own"] != 1 || snap["shared"] != "s" {
		t.Fatalf("snapshot = %v", snap)
	}
	if !root.Delete("shared") {
		t.Fatal("delete shared")
	}
	if _, ok := root.Get("shared"); ok {
		t.Fatal("shared value still present")
	}
	// The declaration survives Delete: the next plain set must still target
	// the shared binding (upstream behavior).
	if err := root.Set("shared", "again"); err != nil {
		t.Fatalf("set after delete: %v", err)
	}
	child := DeriveChildVariables(root)
	if v, _ := child.Get("shared"); v != "again" {
		t.Fatalf("shared after delete/set = %v, want again", v)
	}
}

func TestMergeAndOverlayVariables(t *testing.T) {
	root, err := NewVariables(map[string]any{"keep": "parent", "shared": ScopedValue{Value: 0, Scope: ScopeShared}})
	if err != nil {
		t.Fatalf("new root: %v", err)
	}
	child := DeriveChildVariables(root)
	merged, err := MergeVariables(map[string]any{
		"keep":   "child",
		"extra":  []string{"child"},
		"shared": ScopedValue{Value: 99, Scope: ScopeShared},
	}, child)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if merged != child {
		t.Fatal("merge must return the inherited view")
	}
	if v, _ := merged.Get("keep"); v != "parent" {
		t.Fatalf("inherited value must win: %v", v)
	}
	if v, _ := merged.Get("extra"); !slices.Equal(v.([]string), []string{"child"}) {
		t.Fatalf("extra = %v", v)
	}
	if v, _ := merged.Get("shared"); v != 0 {
		t.Fatalf("shared default must not overwrite the inherited value: %v", v)
	}

	_, err = MergeVariables(map[string]any{
		"keep": ScopedValue{Value: "child", Scope: ScopeShared},
	}, child)
	var se *ScopeError
	if !errors.As(err, &se) {
		t.Fatalf("scope conflict: want *ScopeError, got %v", err)
	}

	fresh, err := MergeVariables(map[string]any{"a": 1}, nil)
	if err != nil {
		t.Fatalf("fresh merge: %v", err)
	}
	if v, _ := fresh.Get("a"); v != 1 {
		t.Fatalf("fresh a = %v", v)
	}
	if !fresh.IsRoot() {
		t.Fatal("fresh merge must be root")
	}

	over, err := OverlayVariables(nil, map[string]any{
		"s": ScopedValue{Value: "v", Scope: ScopeShared},
	})
	if err != nil {
		t.Fatalf("overlay nil: %v", err)
	}
	over2, err := OverlayVariables(over, map[string]any{"s": "w"})
	if err != nil {
		t.Fatalf("overlay in place: %v", err)
	}
	if over2 != over {
		t.Fatal("overlay must update the given store")
	}
	if v, _ := over.Get("s"); v != "w" {
		t.Fatalf("overlay s = %v", v)
	}
}

// TestSharedValueMutationIsVisible pins the reference-type form of upstream's
// mutable-shared-value contract: a child mutating a shared map in place is seen
// by the root. (Appending to a shared Go slice needs Set to rebind the header;
// see the Variables doc.)
func TestSharedValueMutationIsVisible(t *testing.T) {
	root, err := NewVariables(map[string]any{
		"kv": ScopedValue{Value: map[string]any{}, Scope: ScopeShared},
	})
	if err != nil {
		t.Fatalf("new root: %v", err)
	}
	child := DeriveChildVariables(root)
	kv, ok := child.Get("kv")
	if !ok {
		t.Fatal("child cannot see the shared map")
	}
	kv.(map[string]any)["injected"] = true
	rootKV, _ := root.Get("kv")
	if rootKV.(map[string]any)["injected"] != true {
		t.Fatal("shared map mutation is not visible in the root")
	}
}

// TestContextScopedVariables pins the Context surface: SetVariable unwraps a
// declaration, Clone keeps SHARED rebinding tree-wide, and snapshots never leak
// the wrapper.
func TestContextScopedVariables(t *testing.T) {
	c, err := NewWithDeps(nil, map[string]any{"dep": 1}, map[string]any{
		"shared": ScopedValue{Value: "root", Scope: ScopeShared},
		"plain":  "v",
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	cc := c.Clone()
	if err := cc.SetVariable("shared", "changed"); err != nil {
		t.Fatalf("set shared: %v", err)
	}
	if v, _ := c.Variable("shared"); v != "changed" {
		t.Fatalf("clone shared = %v, want changed", v)
	}
	if err := cc.SetVariable("plain", "branch"); err != nil {
		t.Fatalf("set plain: %v", err)
	}
	if v, _ := c.Variable("plain"); v != "v" {
		t.Fatalf("clone mutated a plain value: %v", v)
	}
	snap := c.VariablesSnapshot()
	if _, wrapped := snap["shared"].(ScopedValue); wrapped {
		t.Fatal("snapshot leaked a ScopedValue wrapper")
	}
	var se *ScopeError
	if err := c.SetVariable("bad", ScopedValue{Value: 1, Scope: Scope("bad")}); !errors.As(err, &se) {
		t.Fatalf("want *ScopeError, got %v", err)
	}
}

// TestVariablesConcurrentShared exercises the map-level safety of SHARED
// bindings from sibling branches. Value-level compound mutation is the
// caller's responsibility (documented), so this test only rebinds scalars.
func TestVariablesConcurrentShared(t *testing.T) {
	root, err := NewVariables(map[string]any{
		"shared": ScopedValue{Value: 0, Scope: ScopeShared},
		"own":    0,
	})
	if err != nil {
		t.Fatalf("new root: %v", err)
	}
	children := []*Variables{DeriveChildVariables(root), DeriveChildVariables(root)}
	var wg sync.WaitGroup
	for i, child := range children {
		wg.Add(1)
		go func(i int, child *Variables) {
			defer wg.Done()
			for n := 0; n < 200; n++ {
				if err := child.Set("shared", i*1000+n); err != nil {
					t.Errorf("set shared: %v", err)
					return
				}
				_, _ = child.Get("shared")
				if err := child.Set("own", n); err != nil {
					t.Errorf("set own: %v", err)
					return
				}
				_ = child.Snapshot()
				_ = child.Keys()
			}
		}(i, child)
	}
	wg.Wait()
	if v, ok := root.Get("shared"); !ok || v == nil {
		t.Fatalf("shared lost: %v (ok=%v)", v, ok)
	}
}
