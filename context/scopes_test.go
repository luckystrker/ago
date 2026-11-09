package context

import (
	"errors"
	"math"
	"slices"
	"sort"
	"strings"
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

// foreignClone returns a different dynamic type; DeepCopy must fall back to
// reflection rather than hand the caller a value it cannot use.
type foreignClone struct{ N int }

func (foreignClone) Clone() any { return "not a foreignClone" }

// nilClone returns nil; DeepCopy must yield the type's zero value.
type nilClone struct{ N int }

func (nilClone) Clone() any { return nil }

func TestDeepCopyNilAndArrayEdges(t *testing.T) {
	var ptr *int
	if got, ok := DeepCopy(ptr).(*int); !ok || got != nil {
		t.Fatalf("nil pointer copied to %v (%T)", got, DeepCopy(ptr))
	}
	var m map[string]int
	if got, ok := DeepCopy(m).(map[string]int); !ok || got != nil {
		t.Fatalf("nil map copied to %v (%T)", got, DeepCopy(m))
	}
	var sl []int
	if got, ok := DeepCopy(sl).([]int); !ok || got != nil {
		t.Fatalf("nil slice copied to %v (%T)", got, DeepCopy(sl))
	}
	// A nil any stays untyped nil; a typed nil keeps its dynamic type.
	var iface any
	if got := DeepCopy(iface); got != nil {
		t.Fatalf("nil interface copied to %v", got)
	}
	typedNil := (*int)(nil)
	if got, ok := DeepCopy(any(typedNil)).(*int); !ok || got != nil {
		t.Fatalf("typed nil interface = %v (%T)", got, DeepCopy(any(typedNil)))
	}

	// Arrays are values too: every element must be copied independently.
	arr := [2]map[string]int{{"a": 1}, {"b": 2}}
	got := DeepCopy(arr).([2]map[string]int)
	got[0]["a"] = 9
	if arr[0]["a"] != 1 {
		t.Fatal("array element aliases the original")
	}
	if got[1]["b"] != 2 {
		t.Fatalf("array tail not copied: %v", got)
	}
}

func TestDeepCopyCloneFallbacks(t *testing.T) {
	if got := DeepCopy(foreignClone{N: 3}).(foreignClone); got.N != 3 {
		t.Fatalf("wrong-typed Clone must fall back to reflection: %+v", got)
	}
	if got := DeepCopy(nilClone{N: 4}).(nilClone); got.N != 0 {
		t.Fatalf("nil Clone must yield the zero value: %+v", got)
	}
}

func TestScopeErrorMessage(t *testing.T) {
	msg := (&ScopeError{Key: "k", Scope: ScopeShared, Detail: "conflict"}).Error()
	if !strings.Contains(msg, `"k"`) || !strings.Contains(msg, "conflict") {
		t.Fatalf("message %q must name the key and the conflict", msg)
	}
	if got := (&ScopeError{Detail: "plain"}).Error(); got != "context: plain" {
		t.Fatalf("keyless message = %q", got)
	}
}

func TestDeleteOwnAndMissing(t *testing.T) {
	root, err := NewVariables(map[string]any{"own": 1})
	if err != nil {
		t.Fatalf("new root: %v", err)
	}
	if !root.Delete("own") {
		t.Fatal("delete existing own binding")
	}
	if _, ok := root.Get("own"); ok {
		t.Fatal("own binding survived delete")
	}
	if root.Delete("own") {
		t.Fatal("second delete must report absent")
	}
	if root.Delete("missing") {
		t.Fatal("delete of a missing name must report absent")
	}
}

func TestMultipleSharedBindings(t *testing.T) {
	root, err := NewVariables(map[string]any{
		"a": ScopedValue{Value: 1, Scope: ScopeShared},
		"b": ScopedValue{Value: 2, Scope: ScopeShared},
	})
	if err != nil {
		t.Fatalf("new root: %v", err)
	}
	for key, want := range map[string]any{"a": 1, "b": 2} {
		if got, ok := root.Get(key); !ok || got != want {
			t.Fatalf("%s = %v (ok=%v), want %v", key, got, ok, want)
		}
	}
	child := DeriveChildVariables(root)
	if err := child.Set("a", 10); err != nil {
		t.Fatalf("set shared: %v", err)
	}
	if v, _ := root.Get("a"); v != 10 {
		t.Fatalf("rebound a = %v, want 10", v)
	}
	if v, _ := root.Get("b"); v != 2 {
		t.Fatalf("unrelated shared binding b = %v, want 2", v)
	}
}

// TestMergeVariablesScopedDefaultOnMissingKey drives the SetScoped path of the
// inherited branch (a scoped default whose key the child does not have yet).
func TestMergeVariablesScopedDefaultOnMissingKey(t *testing.T) {
	root, err := NewVariables(map[string]any{"s": ScopedValue{Value: 0, Scope: ScopeShared}})
	if err != nil {
		t.Fatalf("new root: %v", err)
	}
	child := DeriveChildVariables(root)
	merged, err := MergeVariables(map[string]any{
		"extra": ScopedValue{Value: []string{"x"}, Scope: ScopeCopied},
	}, child)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if merged != child {
		t.Fatal("merge must return the inherited view")
	}
	if v, ok := merged.Get("extra"); !ok || !slices.Equal(v.([]string), []string{"x"}) {
		t.Fatalf("extra = %v (ok=%v)", v, ok)
	}
}

func TestOverlayVariablesScopedInPlaceAndPlainRoot(t *testing.T) {
	over, err := OverlayVariables(nil, map[string]any{
		"s": ScopedValue{Value: "v", Scope: ScopeShared},
	})
	if err != nil {
		t.Fatalf("overlay nil: %v", err)
	}
	over2, err := OverlayVariables(over, map[string]any{
		"s": ScopedValue{Value: "w", Scope: ScopeShared},
	})
	if err != nil {
		t.Fatalf("scoped overlay in place: %v", err)
	}
	if over2 != over {
		t.Fatal("scoped overlay must update the given store")
	}
	if v, _ := over.Get("s"); v != "w" {
		t.Fatalf("scoped overlay s = %v, want w", v)
	}

	plain, err := OverlayVariables(nil, map[string]any{"p": 7})
	if err != nil {
		t.Fatalf("plain overlay nil: %v", err)
	}
	if !plain.IsRoot() {
		t.Fatal("plain overlay must build a root")
	}
	if v, _ := plain.Get("p"); v != 7 {
		t.Fatalf("plain overlay p = %v", v)
	}
}

func TestVariablesDeclaredAndBranch(t *testing.T) {
	root, err := NewVariables(map[string]any{
		"plain":  map[string]int{"n": 1},
		"shared": ScopedValue{Value: 0, Scope: ScopeShared},
	})
	if err != nil {
		t.Fatalf("new root: %v", err)
	}
	declared := root.Declared()
	if declared["shared"] != ScopeShared {
		t.Fatalf("declared shared = %q", declared["shared"])
	}
	if _, ok := declared["plain"]; ok {
		t.Fatal("undeclared key must not appear in Declared")
	}

	branch := root.Branch()
	if !branch.IsRoot() {
		t.Fatal("a continuation keeps the branch's rootness")
	}
	if branch.Declared()["shared"] != ScopeShared {
		t.Fatal("Branch must carry declarations")
	}
	// Branch copies own values shallowly (the continuation contract), while
	// SHARED rebinding stays tree-wide.
	bv, _ := branch.Get("plain")
	bv.(map[string]int)["n"] = 2
	if v, _ := root.Get("plain"); v.(map[string]int)["n"] != 2 {
		t.Fatal("Branch must stay shallow")
	}
	if err := branch.Set("shared", 5); err != nil {
		t.Fatalf("set shared: %v", err)
	}
	if v, _ := root.Get("shared"); v != 5 {
		t.Fatalf("Branch broke the shared binding: %v", v)
	}
}

func TestContextNewWithVariablesAndZeroValue(t *testing.T) {
	store, err := NewVariables(map[string]any{"k": 1})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	c := NewWithVariables(nil, map[string]any{"dep": 2}, store)
	if v, ok := c.Variable("k"); !ok || v != 1 {
		t.Fatalf("provided store ignored: k=%v (ok=%v)", v, ok)
	}
	if v, ok := c.Dependency("dep"); !ok || v != 2 {
		t.Fatalf("dependency dep = %v (ok=%v)", v, ok)
	}
	if NewWithVariables(nil, nil, nil) == nil {
		t.Fatal("nil store must fall back to an empty store")
	}

	var zero Context
	if got := zero.VariablesSnapshot(); len(got) != 0 {
		t.Fatalf("zero context snapshot = %v", got)
	}
	if _, ok := zero.Variable("x"); ok {
		t.Fatal("zero context must not report a binding")
	}
	if err := zero.SetVariable("x", 3); err != nil {
		t.Fatalf("zero context set: %v", err)
	}
	if v, ok := zero.Variable("x"); !ok || v != 3 {
		t.Fatalf("zero context x = %v (ok=%v)", v, ok)
	}
}

func TestSanitizeTimeoutRejectsNonFinite(t *testing.T) {
	for _, in := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1} {
		if got := sanitizeTimeout(in); got != 0 {
			t.Fatalf("sanitizeTimeout(%v) = %v, want 0", in, got)
		}
	}
	if got := sanitizeTimeout(1.5); got != 1.5 {
		t.Fatalf("finite timeout changed: %v", got)
	}
}

// TestVariablesConcurrentBranchesStress drives many sibling branches through
// every read path at once. Without -race this still catches deadlocks and map
// corruption; lock order is branch -> shared everywhere.
func TestVariablesConcurrentBranchesStress(t *testing.T) {
	root, err := NewVariables(map[string]any{
		"shared": ScopedValue{Value: 0, Scope: ScopeShared},
		"copied": 0,
	})
	if err != nil {
		t.Fatalf("new root: %v", err)
	}
	const (
		goroutines = 16
		iterations = 100
	)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		child := DeriveChildVariables(root)
		wg.Add(1)
		go func(i int, child *Variables) {
			defer wg.Done()
			for n := 0; n < iterations; n++ {
				if err := child.Set("shared", i*iterations+n); err != nil {
					return
				}
				_, _ = child.Get("shared")
				if err := child.Set("copied", n); err != nil {
					return
				}
				_ = child.Snapshot()
				_ = child.Keys()
				_ = child.Declared()
				_ = child.Len()
				_ = child.Branch()
			}
		}(i, child)
	}
	wg.Wait()
	if v, ok := root.Get("shared"); !ok || v == nil {
		t.Fatalf("shared lost: %v (ok=%v)", v, ok)
	}
	if got := len(DeriveChildVariables(root).Keys()); got != 2 {
		t.Fatalf("child keys = %d, want 2", got)
	}
}

// TestDeepCopySliceViewsKeepTheirLength is the regression for the memo key:
// two views of one backing buffer share the data pointer, so the key must
// include the view length or the second view silently gets the first's length.
func TestDeepCopySliceViewsKeepTheirLength(t *testing.T) {
	buf := []int{1, 2, 3, 4}
	type views struct {
		A []int
		B []int
	}
	in := views{A: buf[:3], B: buf[:2]}
	out, ok := DeepCopy(in).(views)
	if !ok {
		t.Fatalf("DeepCopy returned %T", DeepCopy(in))
	}
	if len(out.A) != 3 || len(out.B) != 2 {
		t.Fatalf("view lengths = %d,%d, want 3,2", len(out.A), len(out.B))
	}
	if !slices.Equal(out.A, []int{1, 2, 3}) || !slices.Equal(out.B, []int{1, 2}) {
		t.Fatalf("view contents = %v,%v", out.A, out.B)
	}
	out.A[0] = 99
	if buf[0] != 1 {
		t.Fatal("copied view aliases the original buffer")
	}
}

// TestSharedDeclarationClearsOwnValue pins the own/shared consistency rule:
// declaring a key SHARED drops its branch value, so Snapshot/Keys cannot
// resurrect a value Get no longer sees (upstream raises KeyError here).
func TestSharedDeclarationClearsOwnValue(t *testing.T) {
	root, err := NewVariables(map[string]any{"k": "plain"})
	if err != nil {
		t.Fatalf("new root: %v", err)
	}
	if err := root.SetScoped("k", "shared", ScopeShared); err != nil {
		t.Fatalf("set scoped: %v", err)
	}
	if v, ok := root.Get("k"); !ok || v != "shared" {
		t.Fatalf("shared k = %v (ok=%v)", v, ok)
	}
	if err := root.Declare("k", ScopeShared); err != nil {
		t.Fatalf("redeclare same scope: %v", err)
	}
	if !root.Delete("k") {
		t.Fatal("shared delete")
	}
	if _, ok := root.Get("k"); ok {
		t.Fatal("deleted shared value still visible")
	}
	if snap := root.Snapshot(); len(snap) != 0 {
		t.Fatalf("snapshot resurrected deleted value: %v", snap)
	}
	if keys := root.Keys(); len(keys) != 0 {
		t.Fatalf("keys resurrected deleted value: %v", keys)
	}
	if root.Len() != 0 {
		t.Fatalf("len = %d, want 0", root.Len())
	}

	// Declaring SHARED without a value must also drop the old branch value.
	declared, err := NewVariables(map[string]any{"k": "plain"})
	if err != nil {
		t.Fatalf("new root: %v", err)
	}
	if err := declared.Declare("k", ScopeShared); err != nil {
		t.Fatalf("declare: %v", err)
	}
	if _, ok := declared.Get("k"); ok {
		t.Fatal("declared SHARED without a value resurrects the branch value")
	}
	if snap := declared.Snapshot(); len(snap) != 0 {
		t.Fatalf("declare snapshot = %v", snap)
	}
	if keys := declared.Keys(); len(keys) != 0 {
		t.Fatalf("declare keys = %v", keys)
	}
}

// TestContextSnapshotAfterSharedDelete mirrors the agui/a2a state export path:
// the snapshot must agree with Variable after a SHARED declaration + delete.
func TestContextSnapshotAfterSharedDelete(t *testing.T) {
	c, err := NewWithDeps(nil, nil, map[string]any{"k": "plain"})
	if err != nil {
		t.Fatalf("new context: %v", err)
	}
	if err := c.SetVariable("k", ScopedValue{Value: "shared", Scope: ScopeShared}); err != nil {
		t.Fatalf("set shared: %v", err)
	}
	if !c.VariableStore().Delete("k") {
		t.Fatal("delete shared")
	}
	if _, ok := c.Variable("k"); ok {
		t.Fatal("variable still visible")
	}
	if snap := c.VariablesSnapshot(); len(snap) != 0 {
		t.Fatalf("context snapshot resurrected deleted value: %v", snap)
	}
}

// TestDeepCopyPreservesAliasingAcrossValues pins Python deepcopy memo parity:
// one store-level copy must keep {"x": m, "y": m} aliased in both the root and
// a derived child.
func TestDeepCopyPreservesAliasingAcrossValues(t *testing.T) {
	shared := map[string]int{"n": 1}
	root, err := NewVariables(map[string]any{"x": shared, "y": shared})
	if err != nil {
		t.Fatalf("new root: %v", err)
	}
	x, _ := root.Get("x")
	y, _ := root.Get("y")
	x.(map[string]int)["n"] = 2
	if y.(map[string]int)["n"] != 2 {
		t.Fatal("root lost aliasing between equal values")
	}
	if shared["n"] != 1 {
		t.Fatal("root values alias the input map")
	}

	child := DeriveChildVariables(root)
	cx, _ := child.Get("x")
	cy, _ := child.Get("y")
	cx.(map[string]int)["n"] = 3
	if cy.(map[string]int)["n"] != 3 {
		t.Fatal("child lost aliasing between equal values")
	}
	if v, _ := root.Get("x"); v.(map[string]int)["n"] != 2 {
		t.Fatal("child copy mutated the parent")
	}
}

// TestReservedVariablePrefixesAreImmutable: the exported accessor must not hand
// out the package's backing storage.
func TestReservedVariablePrefixesAreImmutable(t *testing.T) {
	prefixes := ReservedVariablePrefixes()
	if len(prefixes) == 0 {
		t.Fatal("no reserved prefixes")
	}
	prefixes[0] = "zz:"
	if !IsReservedVariable("ag:private") {
		t.Fatal("caller mutated the package's reserved prefixes")
	}
}

// TestContextVariableStoreZeroValue: a zero-value Context must be usable and
// expose one lazily initialized store through the accessor.
func TestContextVariableStoreZeroValue(t *testing.T) {
	var zero Context
	store := zero.VariableStore()
	if store == nil {
		t.Fatal("zero Context did not initialize a store")
	}
	if again := zero.VariableStore(); again != store {
		t.Fatal("accessor replaced an initialized store")
	}
	if err := zero.SetVariable("x", 1); err != nil {
		t.Fatalf("set: %v", err)
	}
	if v, ok := zero.Variable("x"); !ok || v != 1 {
		t.Fatalf("zero context x = %v (ok=%v)", v, ok)
	}
}

// TestDeclareNonSharedKeepsValue pins the "Declare records a scope without
// touching the bound value" contract: only SHARED declarations may drop the
// branch value. It kills the ==/!= and &&/|| mutants on declareLocked's delete
// guard.
func TestDeclareNonSharedKeepsValue(t *testing.T) {
	root, err := NewVariables(map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("new root: %v", err)
	}
	if err := root.Declare("k", ScopeCopied); err != nil {
		t.Fatalf("declare copied: %v", err)
	}
	if v, ok := root.Get("k"); !ok || v != "v" {
		t.Fatalf("Declare(COPIED) lost the value: v=%v (ok=%v)", v, ok)
	}
	if v, ok := root.Snapshot()["k"]; !ok || v != "v" {
		t.Fatalf("Declare(COPIED) lost the snapshot value: %v", root.Snapshot())
	}

	// The same guard is reached through MergeVariables: a scoped COPIED
	// default over an inherited key must not delete the inherited value.
	child, err := NewVariables(map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("new child root: %v", err)
	}
	derived := DeriveChildVariables(child)
	merged, err := MergeVariables(map[string]any{
		"k": ScopedValue{Value: "default", Scope: ScopeCopied},
	}, derived)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if v, ok := merged.Get("k"); !ok || v != "v" {
		t.Fatalf("COPIED default over inherited key lost the inherited value: v=%v (ok=%v)", v, ok)
	}
}

// TestMergeSharedDefaultRestoresInheritedOwnValue covers the restore branch:
// a SHARED default over a root's plain key must move the inherited value into
// the shared table (the key was not there yet). It kills the ==/!= mutant on
// the restoration condition.
func TestMergeSharedDefaultRestoresInheritedOwnValue(t *testing.T) {
	root, err := NewVariables(map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("new root: %v", err)
	}
	merged, err := MergeVariables(map[string]any{
		"k": ScopedValue{Value: "default", Scope: ScopeShared},
	}, root)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if v, ok := merged.Get("k"); !ok || v != "v" {
		t.Fatalf("restored inherited value = %v (ok=%v), want v", v, ok)
	}
	if v, ok := merged.Snapshot()["k"]; !ok || v != "v" {
		t.Fatalf("snapshot lost the restored shared value: %v", merged.Snapshot())
	}
}

// TestLateSharedDeclarationInvisibleToLiveBranch pins Get/Snapshot/Keys
// consistency when the root declares a new SHARED name after a child branch was
// derived. The child shares the live binding table but its declaration map is a
// snapshot, so Get cannot see the name; the exports must agree.
func TestLateSharedDeclarationInvisibleToLiveBranch(t *testing.T) {
	root, err := NewVariables(map[string]any{"a": 1})
	if err != nil {
		t.Fatalf("new root: %v", err)
	}
	child := DeriveChildVariables(root)
	if err := root.SetScoped("late", "v", ScopeShared); err != nil {
		t.Fatalf("late shared: %v", err)
	}
	if v, ok := child.Get("late"); ok {
		t.Fatalf("child sees a late SHARED name: %v", v)
	}
	if _, ok := child.Snapshot()["late"]; ok {
		t.Fatalf("child snapshot leaks a late SHARED name: %v", child.Snapshot())
	}
	if slices.Contains(child.Keys(), "late") {
		t.Fatalf("child keys leak a late SHARED name: %v", child.Keys())
	}
	// The root, which owns the declaration, sees it everywhere.
	if v, ok := root.Get("late"); !ok || v != "v" {
		t.Fatalf("root lost the late shared value: %v (ok=%v)", v, ok)
	}
	if v, ok := root.Snapshot()["late"]; !ok || v != "v" {
		t.Fatalf("root snapshot lost the late shared value: %v", root.Snapshot())
	}
	if !slices.Contains(root.Keys(), "late") {
		t.Fatalf("root keys lost the late shared name: %v", root.Keys())
	}
}
