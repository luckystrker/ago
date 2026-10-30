package tools

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	agocontext "github.com/luckystrker/ago/context"
)

// Inject marks a tool parameter resolved from Context.Dependencies instead of the
// model's JSON arguments (Python parity: ag2.annotations.Inject).
//
// Go reflection exposes no parameter names, so a tool's arguments are the fields of
// one struct parameter; a DI slot is a field of type Inject or a field carrying an
// `aginject:"<id>"` tag. The lookup key is the tag value, falling back to the field's
// JSON name. When the dependency is missing and a default was declared, the default
// is used and materialized into the context (Python parity: first-use materialization).
// The framework delivers the resolved dependency in Default; HasDefault is true on
// the delivered value.
type Inject struct {
	ID         string
	HasDefault bool
	Default    any
}

// NewInject builds an Inject annotation for id; the first optional value is the
// default used (and materialized into the conversation) when the dependency is
// absent.
func NewInject(id string, def ...any) Inject {
	in := Inject{ID: id}
	if len(def) > 0 {
		in.HasDefault, in.Default = true, def[0]
	}
	return in
}

// Variable marks a tool parameter resolved from Context.Variables instead of the
// model's JSON arguments (Python parity: ag2.annotations.Variable). The slot rules
// mirror Inject; the lookup key is an `agvariable:"<name>"` tag value or the field's
// JSON name.
type Variable struct {
	Name       string
	HasDefault bool
	Default    any
}

// NewVariable builds a Variable annotation for name; the first optional value is the
// default used (and materialized into the conversation) when the variable is absent.
func NewVariable(name string, def ...any) Variable {
	v := Variable{Name: name}
	if len(def) > 0 {
		v.HasDefault, v.Default = true, def[0]
	}
	return v
}

var (
	injectType   = reflect.TypeOf(Inject{})
	variableType = reflect.TypeOf(Variable{})
)

// diKind classifies one dependency-injected slot.
type diKind string

const (
	diInject   diKind = "inject"   // from Context.Dependencies
	diVariable diKind = "variable" // from Context.Variables
)

// diSlot is the resolved DI binding of one struct field.
type diSlot struct {
	kind       diKind
	key        string
	hasDefault bool
	def        any
	typed      bool // the field's type is Inject/Variable: the framework delivers the annotation value
}

// diSlotForField inspects one struct field as a potential DI slot. The second result
// is false when the field is a plain model-provided argument.
func diSlotForField(f reflect.StructField) (diSlot, bool, error) {
	tagIn := strings.TrimSpace(f.Tag.Get("aginject"))
	tagVar := strings.TrimSpace(f.Tag.Get("agvariable"))
	if tagIn != "" && tagVar != "" {
		return diSlot{}, false, fmt.Errorf("field %s: both aginject and agvariable tags set", f.Name)
	}
	var slot diSlot
	switch f.Type {
	case injectType:
		slot = diSlot{kind: diInject, typed: true, key: tagNameOr(f, tagIn)}
	case variableType:
		slot = diSlot{kind: diVariable, typed: true, key: tagNameOr(f, tagVar)}
	default:
		switch {
		case tagIn != "":
			slot = diSlot{kind: diInject, key: tagIn}
		case tagVar != "":
			slot = diSlot{kind: diVariable, key: tagVar}
		default:
			return diSlot{}, false, nil
		}
	}
	if slot.typed && f.Tag.Get("agdefault") != "" {
		return diSlot{}, false, fmt.Errorf("field %s: agdefault is not supported on Inject/Variable-typed fields", f.Name)
	}
	if raw := f.Tag.Get("agdefault"); raw != "" {
		def, err := defaultForKind(raw, derefType(f.Type))
		if err != nil {
			return diSlot{}, false, fmt.Errorf("field %s: %w", f.Name, err)
		}
		slot.hasDefault, slot.def = true, def
	}
	return slot, true, nil
}

// tagNameOr prefers the explicit tag value; without one the slot is addressed by the
// field's JSON name (the name the model would have used).
func tagNameOr(f reflect.StructField, tagValue string) string {
	if tagValue != "" {
		return tagValue
	}
	name, _, _ := jsonFieldName(f)
	return name
}

// defaultForKind parses an `agdefault` tag literal per the field's kind (JSON-ish
// scalar defaults; complex defaults must be prepopulated in the context).
func defaultForKind(raw string, t reflect.Type) (any, error) {
	switch t.Kind() {
	case reflect.String:
		return raw, nil
	case reflect.Bool:
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("bad agdefault %q for bool: %w", raw, err)
		}
		return v, nil
	case reflect.Float32, reflect.Float64:
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("bad agdefault %q for %s: %w", raw, t, err)
		}
		return v, nil
	default:
		if isIntKind(t.Kind()) {
			v, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("bad agdefault %q for %s: %w", raw, t, err)
			}
			return v, nil
		}
		return nil, fmt.Errorf("agdefault supports scalar fields only, not %s", t)
	}
}

// resolveDILookup fetches the current value of a slot from the context, applying the
// declared default (and materializing it into the context) when absent.
func resolveDILookup(cx *agocontext.Context, slot diSlot) (any, bool) {
	var (
		v  any
		ok bool
	)
	if slot.kind == diInject {
		v, ok = cx.Dependency(slot.key)
	} else {
		v, ok = cx.Variable(slot.key)
	}
	if ok || !slot.hasDefault {
		return v, ok
	}
	// AGO-DEVIATION: Python materializes a declared default into the
	// conversation-scoped dependency registry on first use. The Go Context.Clone
	// copies its maps (no shared registry), so the default materializes into the
	// per-call context copy only. See docs/DECISIONS.md.
	if slot.kind == diInject {
		cx.SetDependency(slot.key, slot.def)
	} else {
		cx.SetVariable(slot.key, slot.def)
	}
	return slot.def, true
}
