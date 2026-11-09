package events

import "reflect"

// Condition is a predicate over events. It replaces the Python descriptor condition
// DSL (Event.field == value, A | B, ~c) — see docs/DECISIONS.md D-005.
type Condition func(Event) bool

// AllOf matches when every condition matches (empty matches everything).
func AllOf(cs ...Condition) Condition {
	return func(ev Event) bool {
		for _, c := range cs {
			if c != nil && !c(ev) {
				return false
			}
		}
		return true
	}
}

// AnyOf matches when at least one condition matches (empty matches nothing).
func AnyOf(cs ...Condition) Condition {
	return func(ev Event) bool {
		for _, c := range cs {
			if c != nil && c(ev) {
				return true
			}
		}
		return false
	}
}

// Not negates a condition.
func Not(c Condition) Condition {
	return func(ev Event) bool { return !c(ev) }
}

// TypeOf matches events whose dynamic type is T (value or pointer form).
// Delivery wrappers are unwrapped: a routed copy keeps the authored type, so
// typed subscriptions still receive it.
func TypeOf[T Event]() Condition {
	t := reflect.TypeOf((*T)(nil)).Elem()
	return func(ev Event) bool {
		return typeMatches(reflect.TypeOf(AsValue(ev)), t)
	}
}

// TypeOfAny matches events whose dynamic type is one of the given types.
func TypeOfAny(types ...reflect.Type) Condition {
	return func(ev Event) bool {
		rt := reflect.TypeOf(AsValue(ev))
		for _, t := range types {
			if typeMatches(rt, t) {
				return true
			}
		}
		return false
	}
}

// typeMatches accepts both the value and pointer spelling of a type, including
// T itself being spelled as a pointer.
func typeMatches(rt, t reflect.Type) bool {
	if rt == nil || t == nil {
		return false
	}
	if rt == t || rt == reflect.PointerTo(t) {
		return true
	}
	if t.Kind() == reflect.Pointer {
		e := t.Elem()
		return rt == e || rt == reflect.PointerTo(e)
	}
	return false
}

// Equal matches when the extracted comparable value equals want.
func Equal[T comparable](get func(Event) T, want T) Condition {
	return func(ev Event) bool { return get(ev) == want }
}
