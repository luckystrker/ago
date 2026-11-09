package events

import "reflect"

// InertEvent is the embedded base of events that trigger nothing and are
// forwarded to ancestors as themselves rather than wrapped in ChildEvent:
// token accounting (UsageEvent) and task lifecycle (Task*). Path names the
// delegations the event travelled through, outermost first; it is empty for a
// locally authored event.
//
// The struct is immutable after construction and safe for concurrent reads.
//
// AGO-DEVIATION: Python declares InertEvent as a BaseEvent subclass with
// dataclass fields; Go uses embedding because it has no inheritance, and an
// embedded value keeps the wire field on the concrete event (D-092).
type InertEvent struct {
	Path []string `json:"path,omitempty"`
}

// IsInertEvent marks the embedding event as inert.
func (InertEvent) IsInertEvent() bool { return true }

// InertPath returns the delegation path carried by the event.
func (i InertEvent) InertPath() []string { return i.Path }

// SetInertPath replaces the delegation path. Callers work on a copy; see
// StampInert and AsLocal.
func (i *InertEvent) SetInertPath(path []string) { i.Path = path }

// IsInert reports whether ev is an inert event (UsageEvent or Task*), also
// when it is spelled as a pointer or a delivery wrapper.
func IsInert(ev Event) bool {
	if isNilEvent(ev) {
		return false
	}
	_, ok := AsValue(ev).(interface{ IsInertEvent() bool })
	return ok
}

// InertPath returns the provenance path of an inert event, or nil for any
// other event.
func InertPath(ev Event) []string {
	if isNilEvent(ev) {
		return nil
	}
	if p, ok := AsValue(ev).(interface{ InertPath() []string }); ok {
		return p.InertPath()
	}
	return nil
}

// StampInert returns a copy of an inert event stamped with the delegation
// provenance. The first forwarding hop sets path (and, for UsageEvent, the
// spending task and agent); later hops keep the provenance they already carry.
// A non-inert event is returned unchanged.
//
// The input is never mutated: the child transcript keeps the locally authored
// record while the ancestor receives the stamped copy (upstream parity).
func StampInert(ev Event, path []string, taskID, agentName string) Event {
	if !IsInert(ev) {
		return ev
	}
	ev = unwrapRouted(ev)
	stamped := cloneEvent(ev)
	if len(InertPath(ev)) > 0 {
		return stamped
	}
	if setter, ok := stamped.(interface{ SetInertPath([]string) }); ok {
		setter.SetInertPath(cloneStrings(path))
	}
	if setter, ok := stamped.(interface{ SetInertIdentity(string, string) }); ok {
		setter.SetInertIdentity(taskID, agentName)
	}
	return stamped
}

// AsLocal returns the event as a new local publication: inherited delegation
// provenance is dropped and a forwarded UsageEvent gets a fresh source id, so
// republishing a forwarded record does not impersonate the original spend.
// An event without provenance is returned as is.
//
// AGO-DEVIATION: upstream always replaces a UsageEvent.event_id in as_local,
// even one just constructed locally; AGo only does so when inherited
// provenance is actually present, so producers can assign a stable source id
// before publishing with NewUsageEvent (D-092).
func AsLocal(ev Event) Event {
	if !IsInert(ev) {
		return ev
	}
	ev = unwrapRouted(ev)
	if !hasInheritedProvenance(ev) {
		return ev
	}
	local := cloneEvent(ev)
	if setter, ok := local.(interface{ SetInertPath([]string) }); ok {
		setter.SetInertPath(nil)
	}
	if setter, ok := local.(interface{ SetInertIdentity(string, string) }); ok {
		setter.SetInertIdentity("", "")
	}
	if u, ok := local.(*UsageEvent); ok {
		u.EventID = newUsageEventID()
	}
	return local
}

// hasInheritedProvenance reports provenance that makes an event a forwarded
// observation rather than a local publication. TaskID/AgentName are local
// identity for Task* events; only UsageEvent treats them as provenance.
func hasInheritedProvenance(ev Event) bool {
	if len(InertPath(ev)) > 0 {
		return true
	}
	if u, ok := AsValue(ev).(UsageEvent); ok {
		return u.TaskID != "" || u.AgentName != ""
	}
	return false
}

// unwrapRouted drops a transport-only delivery wrapper before provenance is
// copied: the wrapper has no writable path/identity fields, so stamping it
// would silently lose the provenance (and AsLocal could not refresh a
// UsageEvent id through it).
func unwrapRouted(ev Event) Event {
	if r, ok := ev.(*RoutedEvent); ok && r != nil && r.Inner != nil {
		return r.Inner
	}
	return ev
}

// isNilEvent reports a nil interface or a typed-nil pointer, which satisfies
// Event but has no value to inspect.
func isNilEvent(ev Event) bool {
	if ev == nil {
		return true
	}
	rv := reflect.ValueOf(ev)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

// cloneEvent makes a shallow copy of an event as a pointer, so stamping
// provenance cannot mutate the original publication. Struct fields with
// reference types (paths, payload maps) are shared; StampInert only replaces
// the path slice with its own copy.
func cloneEvent(ev Event) Event {
	rv := reflect.ValueOf(ev)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return ev
		}
		cp := reflect.New(rv.Elem().Type())
		cp.Elem().Set(rv.Elem())
		if e, ok := cp.Interface().(Event); ok {
			return e
		}
		return ev
	}
	cp := reflect.New(rv.Type())
	cp.Elem().Set(rv)
	if e, ok := cp.Interface().(Event); ok {
		return e
	}
	return ev
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
