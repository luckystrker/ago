package events

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"sync"
)

// The event registry maps type ids to factories for deserialization.
var (
	regMu    sync.RWMutex
	registry = map[string]func() Event{}
)

// RegisterEvent registers an event type by its EventType() id. Idempotent; registering
// the same id with a different dynamic type panics (contract violation). Factories
// produce pointer events (the canonical form on streams).
func RegisterEvent(sample Event) {
	regMu.Lock()
	defer regMu.Unlock()
	id := sample.EventType()
	t := reflect.TypeOf(sample)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if existing, ok := registry[id]; ok {
		et := reflect.TypeOf(existing())
		for et.Kind() == reflect.Pointer {
			et = et.Elem()
		}
		if et != t {
			panic(fmt.Sprintf("events: type id %q already registered as %s", id, et))
		}
		return
	}
	registry[id] = func() Event { return reflect.New(t).Interface().(Event) }
}

// RegisteredTypes lists all registered event type ids (sorted; used in tests and docs).
func RegisteredTypes() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	ids := make([]string, 0, len(registry))
	for id := range registry {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// MarshalEvent serializes any event to JSON with a "@type" discriminator.
func MarshalEvent(ev Event) ([]byte, error) {
	raw, err := json.Marshal(ev)
	if err != nil {
		return nil, err
	}
	// Events with custom MarshalJSON (ModelRequest, ToolResult, ...) already embed
	// "@type"; plain structs need it injected here.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("events: %T does not marshal to a JSON object: %w", ev, err)
	}
	if _, ok := obj["@type"]; !ok {
		obj["@type"] = json.RawMessage(`"` + ev.EventType() + `"`)
	}
	return json.Marshal(obj)
}

// UnmarshalEvent rebuilds an event from JSON produced by MarshalEvent.
func UnmarshalEvent(data []byte) (Event, error) {
	var head struct {
		Type string `json:"@type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("events: bad event payload: %w", err)
	}
	if head.Type == "" {
		return nil, fmt.Errorf("events: missing @type discriminator")
	}
	regMu.RLock()
	factory, ok := registry[head.Type]
	regMu.RUnlock()
	if !ok {
		var obj map[string]any
		_ = json.Unmarshal(data, &obj)
		delete(obj, "@type")
		return UnknownEvent{TypeName: head.Type, Data: obj}, nil
	}
	ev := factory()
	if err := json.Unmarshal(data, ev); err != nil {
		return nil, fmt.Errorf("events: decoding %s: %w", head.Type, err)
	}
	return ev, nil
}

// UnmarshalInput is UnmarshalEvent constrained to Input implementations.
func UnmarshalInput(data []byte) (Input, error) {
	ev, err := UnmarshalEvent(data)
	if err != nil {
		return nil, err
	}
	in, ok := ev.(Input)
	if !ok {
		return nil, fmt.Errorf("events: %s is not an Input", ev.EventType())
	}
	return in, nil
}
