package context

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
)

// Scope declares how a context variable travels into a delegated child
// context (parity: ag2/scopes.py Scope).
type Scope string

const (
	// ScopeCopied is the default: every child branch receives its own deep copy,
	// so a child mutation stays invisible to the parent and to siblings.
	ScopeCopied Scope = "copied"
	// ScopeLocal keeps the value out of every child branch.
	ScopeLocal Scope = "local"
	// ScopeShared binds one value to the whole invocation tree: a rebinding in
	// any branch is visible in the root and in sibling branches.
	ScopeShared Scope = "shared"
)

// ErrScope is the sentinel behind every *ScopeError.
var ErrScope = errors.New("context: variable scope conflict")

// ScopeError reports a variable declaration that conflicts with its ownership
// or with the scope it inherited.
type ScopeError struct {
	// Key is the variable name; empty for an invalid scope literal.
	Key string
	// Scope is the rejected scope.
	Scope Scope
	// Detail explains the conflict.
	Detail string
}

func (e *ScopeError) Error() string {
	if e.Key == "" {
		return "context: " + e.Detail
	}
	return fmt.Sprintf("context: variable %q: %s", e.Key, e.Detail)
}

// Unwrap makes errors.Is(err, ErrScope) work for every scope conflict.
func (e *ScopeError) Unwrap() error { return ErrScope }

// ScopedValue attaches a declaration to a value assigned into Variables. The
// wrapper is never stored: Set unwraps it and records the scope. A zero Scope
// means ScopeCopied.
type ScopedValue struct {
	Value any
	Scope Scope
}

// NewScopedValue validates and normalizes a declaration. An empty scope means
// ScopeCopied; anything other than the three defined scopes is a *ScopeError.
func NewScopedValue(value any, scope Scope) (ScopedValue, error) {
	s, err := normalizeScope(scope)
	if err != nil {
		return ScopedValue{}, err
	}
	return ScopedValue{Value: value, Scope: s}, nil
}

func normalizeScope(scope Scope) (Scope, error) {
	switch scope {
	case "":
		return ScopeCopied, nil
	case ScopeCopied, ScopeLocal, ScopeShared:
		return scope, nil
	default:
		return "", &ScopeError{Scope: scope, Detail: fmt.Sprintf("unknown variable scope %q", scope)}
	}
}

// ReservedVariablePrefixes are the framework's control-plane key prefixes.
// They are never inherited by a child context and can never be declared SHARED
// (parity: ag2/context.py RESERVED_VARIABLE_PREFIXES).
var ReservedVariablePrefixes = []string{"ag:", "a2a:"}

// IsReservedVariable reports whether key belongs to the framework control
// plane.
func IsReservedVariable(key string) bool {
	for _, p := range ReservedVariablePrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// Clone is the explicit deep-copy hook checked before the reflection fallback.
// Clone must return an independent value of the same dynamic type; returning
// nil yields the type's zero value.
type Clone interface {
	Clone() any
}

// DeepCopy returns an independent copy of value, if the value's type allows it.
//
// AGO-DEVIATION: Python's copy.deepcopy walks arbitrary objects; the stdlib-only
// port prefers the explicit Clone hook and otherwise reflects over pointers,
// maps, slices, arrays, structs and interfaces. Limits (docs/DECISIONS.md
// D-090a):
//   - unexported struct fields are shallow-copied (reflection cannot set them);
//   - funcs, chans and unsafe pointers are shared by reference;
//   - types containing locks should implement Clone;
//   - cycles are preserved through pointer identity;
//   - concurrent mutation of value while it is copied is a data race the caller
//     must prevent.
func DeepCopy(value any) any {
	if value == nil {
		return nil
	}
	out := deepCopyValue(reflect.ValueOf(value), make(map[copyVisit]reflect.Value))
	if !out.IsValid() {
		return nil
	}
	return out.Interface()
}

type copyVisit struct {
	typ reflect.Type
	ptr uintptr
}

func deepCopyValue(v reflect.Value, seen map[copyVisit]reflect.Value) reflect.Value {
	if !v.IsValid() {
		return v
	}
	if v.Kind() == reflect.Interface {
		if v.IsNil() {
			return v
		}
		return deepCopyValue(v.Elem(), seen)
	}
	if v.CanInterface() {
		if c, ok := v.Interface().(Clone); ok && !isNilValue(v) {
			out := reflect.ValueOf(c.Clone())
			if !out.IsValid() {
				return reflect.Zero(v.Type())
			}
			if out.Type().AssignableTo(v.Type()) {
				return out
			}
			// A Clone returning the wrong type falls back to reflection rather
			// than handing a value the caller cannot use.
		}
	}

	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		key := copyVisit{typ: v.Type(), ptr: v.Pointer()}
		if out, ok := seen[key]; ok {
			return out
		}
		out := reflect.New(v.Type().Elem())
		seen[key] = out
		out.Elem().Set(deepCopyValue(v.Elem(), seen))
		return out
	case reflect.Map:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		key := copyVisit{typ: v.Type(), ptr: v.Pointer()}
		if out, ok := seen[key]; ok {
			return out
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		seen[key] = out
		iter := v.MapRange()
		for iter.Next() {
			out.SetMapIndex(deepCopyValue(iter.Key(), seen), deepCopyValue(iter.Value(), seen))
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		if v.Len() == 0 {
			return reflect.MakeSlice(v.Type(), 0, 0)
		}
		key := copyVisit{typ: v.Type(), ptr: v.Pointer()}
		if out, ok := seen[key]; ok {
			return out
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		seen[key] = out
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(deepCopyValue(v.Index(i), seen))
		}
		return out
	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(deepCopyValue(v.Index(i), seen))
		}
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		// Shallow baseline preserves unexported fields; exported fields are
		// replaced with deep copies below.
		out.Set(v)
		for i := 0; i < v.NumField(); i++ {
			if !out.Field(i).CanSet() {
				continue
			}
			out.Field(i).Set(deepCopyValue(v.Field(i), seen))
		}
		return out
	default:
		return v
	}
}

func isNilValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Interface, reflect.Chan, reflect.Func:
		return v.IsNil()
	default:
		return false
	}
}

// sharedStore is the tree-wide binding table behind SHARED variables. Its lock
// guards the map structure; callers still synchronize compound mutation of a
// shared value itself.
type sharedStore struct {
	mu     sync.RWMutex
	values map[string]any
}

func newSharedStore() *sharedStore { return &sharedStore{values: map[string]any{}} }

func (s *sharedStore) get(key string) (any, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.values[key]
	return v, ok
}

func (s *sharedStore) set(key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = map[string]any{}
	}
	s.values[key] = value
}

func (s *sharedStore) delete(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.values[key]; !ok {
		return false
	}
	delete(s.values, key)
	return true
}

func (s *sharedStore) keys() []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.values))
	for k := range s.values {
		out = append(out, k)
	}
	return out
}

// Variables is a variable store with branch-owned values and tree-owned
// SHARED bindings (parity: ag2/scopes.py ScopedVariables). It is safe for
// concurrent use; SHARED values provide visibility, not synchronization of the
// value itself (guard compound mutations with a caller-supplied lock).
//
// AGO-DEVIATION: Go slices are headers, so append/len growth on a SHARED slice
// obtained from Get is not visible through the store; rebind it with Set, or
// share a map/pointer, where Python's list object mutates in place
// (docs/DECISIONS.md D-090a).
type Variables struct {
	mu       sync.RWMutex
	own      map[string]any
	declared map[string]Scope
	shared   *sharedStore
	root     bool
}

// NewVariables builds a root store. Every value is deep-copied, so the caller
// keeps ownership of the input map; a ScopedValue value declares its scope.
func NewVariables(values map[string]any) (*Variables, error) {
	v := &Variables{
		own:      map[string]any{},
		declared: map[string]Scope{},
		shared:   newSharedStore(),
		root:     true,
	}
	for key, value := range values {
		if sv, ok := value.(ScopedValue); ok {
			if err := v.SetScoped(key, DeepCopy(sv.Value), sv.Scope); err != nil {
				return nil, err
			}
			continue
		}
		if err := v.Set(key, DeepCopy(value)); err != nil {
			return nil, err
		}
	}
	return v, nil
}

// Get returns the current value and whether the name is bound.
func (v *Variables) Get(key string) (any, bool) {
	v.mu.RLock()
	if v.declared[key] == ScopeShared {
		shared := v.shared
		v.mu.RUnlock()
		return shared.get(key)
	}
	value, ok := v.own[key]
	v.mu.RUnlock()
	return value, ok
}

// Set binds a value. A ScopedValue declares its scope and is unwrapped; a key
// already declared SHARED is written to the shared binding.
func (v *Variables) Set(key string, value any) error {
	if sv, ok := value.(ScopedValue); ok {
		return v.SetScoped(key, sv.Value, sv.Scope)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.declared[key] == ScopeShared {
		v.ensureSharedLocked()
		v.shared.set(key, value)
		return nil
	}
	v.ensureOwnLocked()
	v.own[key] = value
	return nil
}

// SetScoped declares scope and binds value. Re-declaring the same scope is
// allowed; a different scope is a *ScopeError.
func (v *Variables) SetScoped(key string, value any, scope Scope) error {
	s, err := normalizeScope(scope)
	if err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.declareLocked(key, s); err != nil {
		return err
	}
	if s == ScopeShared {
		v.ensureSharedLocked()
		v.shared.set(key, value)
		return nil
	}
	v.ensureOwnLocked()
	v.own[key] = value
	return nil
}

// Declare records a scope without touching the bound value.
func (v *Variables) Declare(key string, scope Scope) error {
	s, err := normalizeScope(scope)
	if err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.declareLocked(key, s)
}

func (v *Variables) declareLocked(key string, scope Scope) error {
	if scope == ScopeShared && IsReservedVariable(key) {
		return &ScopeError{Key: key, Scope: scope, Detail: "reserved variables cannot be shared"}
	}
	if v.declared == nil {
		v.declared = map[string]Scope{}
	}
	current, ok := v.declared[key]
	if ok && current != scope {
		return &ScopeError{
			Key:    key,
			Scope:  scope,
			Detail: fmt.Sprintf("cannot be redeclared from %q to %q", current, scope),
		}
	}
	if !v.root && scope == ScopeShared && current != ScopeShared {
		return &ScopeError{Key: key, Scope: scope, Detail: "new shared variables cannot be declared below the root"}
	}
	v.declared[key] = scope
	return nil
}

func (v *Variables) ensureOwnLocked() {
	if v.own == nil {
		v.own = map[string]any{}
	}
}

func (v *Variables) ensureSharedLocked() {
	if v.shared == nil {
		v.shared = newSharedStore()
	}
}

// Delete removes the binding but leaves the scope declaration in place, so a
// later Set still targets the same kind of binding (parity).
func (v *Variables) Delete(key string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.declared[key] == ScopeShared {
		return v.shared.delete(key)
	}
	if _, ok := v.own[key]; !ok {
		return false
	}
	delete(v.own, key)
	return true
}

// Keys returns the union of branch and shared names, sorted for determinism.
func (v *Variables) Keys() []string {
	v.mu.RLock()
	seen := make(map[string]struct{}, len(v.own))
	for k := range v.own {
		seen[k] = struct{}{}
	}
	shared := v.shared
	v.mu.RUnlock()
	for _, k := range shared.keys() {
		seen[k] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Len is the number of bound names.
func (v *Variables) Len() int { return len(v.Keys()) }

// Snapshot returns the resolved values as a plain map, without declarations.
// The map is a new allocation; values themselves are not deep-copied.
func (v *Variables) Snapshot() map[string]any {
	v.mu.RLock()
	out := make(map[string]any, len(v.own))
	for k, value := range v.own {
		out[k] = value
	}
	shared := v.shared
	v.mu.RUnlock()
	if shared != nil {
		shared.mu.RLock()
		for k, value := range shared.values {
			out[k] = value
		}
		shared.mu.RUnlock()
	}
	return out
}

// Declared returns a copy of the scope declarations.
func (v *Variables) Declared() map[string]Scope {
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := make(map[string]Scope, len(v.declared))
	for k, s := range v.declared {
		out[k] = s
	}
	return out
}

// IsRoot reports whether this store may declare new SHARED names.
func (v *Variables) IsRoot() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.root
}

// Branch returns a same-branch copy for a continuation turn: own values are
// copied shallowly (the existing Context.Clone contract) and SHARED bindings
// stay one tree-wide binding.
func (v *Variables) Branch() *Variables {
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := &Variables{
		own:      make(map[string]any, len(v.own)),
		declared: make(map[string]Scope, len(v.declared)),
		shared:   v.shared,
		root:     v.root,
	}
	for k, value := range v.own {
		out.own[k] = value
	}
	for k, scope := range v.declared {
		out.declared[k] = scope
	}
	return out
}

// DeriveChildVariables builds a child branch: LOCAL, SHARED and reserved keys
// are not inherited; surviving own values are deep-copied; the child shares the
// parent's tree-wide binding table and may not declare new SHARED names.
func DeriveChildVariables(parent *Variables) *Variables {
	if parent == nil {
		return &Variables{
			own:      map[string]any{},
			declared: map[string]Scope{},
			shared:   newSharedStore(),
			root:     false,
		}
	}
	parent.mu.RLock()
	declared := make(map[string]Scope, len(parent.declared))
	for k, s := range parent.declared {
		declared[k] = s
	}
	shallow := make(map[string]any, len(parent.own))
	for key, value := range parent.own {
		scope := declared[key]
		if scope == ScopeLocal || scope == ScopeShared || IsReservedVariable(key) {
			continue
		}
		shallow[key] = value
	}
	shared := parent.shared
	parent.mu.RUnlock()

	own := make(map[string]any, len(shallow))
	for key, value := range shallow {
		own[key] = DeepCopy(value)
	}
	childDeclared := make(map[string]Scope, len(declared))
	for key, scope := range declared {
		if scope == ScopeShared {
			childDeclared[key] = scope
			continue
		}
		if _, ok := own[key]; ok {
			childDeclared[key] = scope
		}
	}
	if shared == nil {
		shared = newSharedStore()
	}
	return &Variables{own: own, declared: childDeclared, shared: shared, root: false}
}

// MergeVariables builds a root store, or overlays agent defaults on an
// inherited child branch (parity: ag2/scopes.py merge_variables).
//
// With an inherited store the inherited value wins on a shared key, the
// defaults are declared first (so a conflicting declaration is a *ScopeError),
// and missing keys are deep-copied in. Without one, every default is
// deep-copied into a fresh root.
func MergeVariables(defaults map[string]any, inherited *Variables) (*Variables, error) {
	if inherited == nil {
		return NewVariables(defaults)
	}
	for key, value := range defaults {
		sv, scoped := value.(ScopedValue)
		if scoped {
			if err := inherited.Declare(key, sv.Scope); err != nil {
				return nil, err
			}
		}
		if _, ok := inherited.Get(key); ok {
			continue
		}
		if scoped {
			if err := inherited.SetScoped(key, DeepCopy(sv.Value), sv.Scope); err != nil {
				return nil, err
			}
			continue
		}
		if err := inherited.Set(key, DeepCopy(value)); err != nil {
			return nil, err
		}
	}
	return inherited, nil
}

// OverlayVariables applies continuation or plugin values in place, preserving
// scope declarations (parity: ag2/scopes.py overlay_variables). A nil store
// builds a root: declarations deep-copy the values, a plain map is applied
// shallowly.
//
// AGO-DEVIATION: AGo has no per-ask variables= override and no turn-scoped
// plugin defaults, so this parity helper has no production call site yet
// (docs/DECISIONS.md D-090a).
func OverlayVariables(vars *Variables, values map[string]any) (*Variables, error) {
	if vars != nil {
		for key, value := range values {
			if sv, ok := value.(ScopedValue); ok {
				if err := vars.SetScoped(key, sv.Value, sv.Scope); err != nil {
					return nil, err
				}
				continue
			}
			if err := vars.Set(key, value); err != nil {
				return nil, err
			}
		}
		return vars, nil
	}
	for _, value := range values {
		if _, ok := value.(ScopedValue); ok {
			return NewVariables(values)
		}
	}
	out := &Variables{
		own:      make(map[string]any, len(values)),
		declared: map[string]Scope{},
		shared:   newSharedStore(),
		root:     true,
	}
	for key, value := range values {
		out.own[key] = value
	}
	return out, nil
}
