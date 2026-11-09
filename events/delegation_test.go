package events

import (
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func testUsageEvent(id string) *UsageEvent {
	return &UsageEvent{EventID: id, Usage: Usage{TotalTokens: I64(30)}, Kind: UsageKindModelCall}
}

func TestSourceEventLocalIsItself(t *testing.T) {
	ue := testUsageEvent("local")
	if got := SourceEvent(ue); got != Event(ue) {
		t.Fatalf("SourceEvent(UsageEvent) = %T, want the same event", got)
	}
	if got := SourceEvent(nil); got != nil {
		t.Fatalf("SourceEvent(nil) = %v, want nil", got)
	}
}

func TestSourceEventDescendantSurvivesObservationAndRouting(t *testing.T) {
	source := testUsageEvent("origin")
	observation := Event(source)
	for depth := 1; depth <= 2; depth++ {
		observation = &ChildEvent{
			Inner:         observation,
			TaskID:        "child",
			Path:          []string{"child"},
			AgentName:     "worker",
			OriginEventID: source.EventID,
		}
		if got := SourceEvent(observation); got != Event(source) {
			t.Fatalf("depth %d: SourceEvent = %T, want the source UsageEvent", depth, got)
		}
	}

	routed := Routed(observation, Delivery{})
	if got := SourceEvent(routed); got != Event(source) {
		t.Fatalf("SourceEvent(routed observation) = %T, want the source UsageEvent", got)
	}
}

func TestDownEventIsItsOwnSource(t *testing.T) {
	audit := &DownEvent{
		Inner:     testUsageEvent("inner"),
		TaskID:    "child",
		Path:      []string{"child"},
		AgentName: "worker",
	}
	if got := SourceEvent(audit); got != Event(audit) {
		t.Fatalf("SourceEvent(DownEvent) = %T, want the audit itself", got)
	}
	if EstimatedTokens(audit, 4) != 0 {
		t.Fatalf("DownEvent must not occupy the context budget, got %d tokens", EstimatedTokens(audit, 4))
	}
	if got := RenderForPrompt(audit); got != "" {
		t.Fatalf("RenderForPrompt(DownEvent) = %q, want empty", got)
	}
}

func TestDelegationEventsRoundTrip(t *testing.T) {
	child := &ChildEvent{
		Inner:         testUsageEvent("inner-1"),
		TaskID:        "t1",
		Path:          []string{"root", "t1"},
		AgentName:     "worker",
		OriginEventID: "inner-1",
	}
	down := &DownEvent{
		Inner:          testUsageEvent("inner-2"),
		TaskID:         "t2",
		Path:           []string{"root", "t2"},
		AgentName:      "worker",
		OriginTaskID:   "t0",
		OriginPath:     []string{"root"},
		OriginStreamID: "stream-1",
	}
	cases := []Event{
		child,
		&TransientChildEvent{ChildEvent: *child},
		&DeliveryFailed{TaskID: "t3", Reason: "not executing"},
		&DeliveryFailed{Reason: "no task"},
		down,
		&TransientDownEvent{DownEvent: *down},
	}
	for _, ev := range cases {
		t.Run(ev.EventType(), func(t *testing.T) {
			data, err := MarshalEvent(ev)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if strings.Contains(string(data), "source_event") {
				t.Fatalf("source_event leaked into the wire format: %s", data)
			}
			back, err := UnmarshalEvent(data)
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if back.EventType() != ev.EventType() {
				t.Fatalf("type = %s, want %s", back.EventType(), ev.EventType())
			}
			again, err := MarshalEvent(back)
			if err != nil {
				t.Fatalf("remarshal: %v", err)
			}
			if string(again) != string(data) {
				t.Fatalf("round trip changed payload:\n first %s\nsecond %s", data, again)
			}
			if got, want := back.Transient(), ev.Transient(); got != want {
				t.Fatalf("Transient = %v, want %v", got, want)
			}
			if back.Conversational() {
				t.Fatal("delegation events are never conversational")
			}
		})
	}
}

func TestDelegationEventInnerTypeAndDefaults(t *testing.T) {
	child := &ChildEvent{
		Inner:         testUsageEvent("nested"),
		TaskID:        "t",
		Path:          []string{"t"},
		AgentName:     "worker",
		OriginEventID: "nested",
	}
	data, err := MarshalEvent(child)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"direction":"up"`) {
		t.Fatalf("ChildEvent direction default missing: %s", data)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("json: %v", err)
	}
	inner, ok := wire["inner"].(map[string]any)
	if !ok {
		t.Fatalf("inner is not an object: %T", wire["inner"])
	}
	if inner["@type"] != "UsageEvent" {
		t.Fatalf("inner @type = %v, want UsageEvent", inner["@type"])
	}

	back, err := UnmarshalEvent(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	decoded, ok := back.(*ChildEvent)
	if !ok {
		t.Fatalf("decoded %T, want *ChildEvent", back)
	}
	if decoded.Direction != DirectionUp {
		t.Fatalf("direction = %q, want %q", decoded.Direction, DirectionUp)
	}
	innerEv, ok := decoded.Inner.(*UsageEvent)
	if !ok || innerEv.EventID != "nested" {
		t.Fatalf("inner = %#v, want the UsageEvent with event_id nested", decoded.Inner)
	}

	down := &DownEvent{Inner: testUsageEvent("d"), TaskID: "t", Path: []string{"t"}, AgentName: "w"}
	data, err = MarshalEvent(down)
	if err != nil {
		t.Fatalf("marshal down: %v", err)
	}
	if !strings.Contains(string(data), `"direction":"down"`) {
		t.Fatalf("DownEvent direction default missing: %s", data)
	}
	if !strings.Contains(string(data), `"origin_path":[]`) {
		t.Fatalf("DownEvent origin_path must serialize as an empty array: %s", data)
	}
	back, err = UnmarshalEvent(data)
	if err != nil {
		t.Fatalf("unmarshal down: %v", err)
	}
	if decoded, ok := back.(*DownEvent); !ok || decoded.Direction != DirectionDown {
		t.Fatalf("down direction = %#v, want %q", back, DirectionDown)
	}
}

func TestDeliveryMarker(t *testing.T) {
	src := testUsageEvent("delivered")
	var calls int
	routed := Routed(src, Delivery{Validate: func() error { calls++; return nil }})

	if !IsRouted(routed) {
		t.Fatal("Routed must report IsRouted")
	}
	if IsRouted(src) {
		t.Fatal("ordinary events are not routed")
	}
	d, ok := DeliveryOf(routed)
	if !ok || d.Validate == nil {
		t.Fatalf("DeliveryOf = %#v, %v; want the marker", d, ok)
	}
	if err := d.Validate(); err != nil || calls != 1 {
		t.Fatalf("Validate error = %v, calls = %d; want nil, 1", err, calls)
	}
	if _, ok := DeliveryOf(src); ok {
		t.Fatal("DeliveryOf(ordinary) must be false")
	}
	if routed.EventType() != src.EventType() || routed.Transient() != src.Transient() ||
		routed.Conversational() != src.Conversational() {
		t.Fatal("Routed must delegate the event shape to its inner event")
	}

	data, err := MarshalEvent(routed)
	if err != nil {
		t.Fatalf("marshal routed: %v", err)
	}
	if strings.Contains(string(data), "delivery") || strings.Contains(string(data), "validate") {
		t.Fatalf("delivery marker leaked into the wire format: %s", data)
	}
	if !strings.Contains(string(data), `"@type":"UsageEvent"`) {
		t.Fatalf("routed payload lost the inner @type: %s", data)
	}
	unwrapped, ok := AsValue(routed).(UsageEvent)
	if !ok || unwrapped.EventID != "delivered" {
		t.Fatalf("AsValue(routed) = %#v, want the inner UsageEvent", AsValue(routed))
	}
}

func TestDelegationConditionsSeeThroughRouting(t *testing.T) {
	routed := Routed(&ModelResponse{Message: &ModelMessage{Content: "hi"}}, Delivery{})
	if !TypeOf[ModelResponse]()(routed) {
		t.Fatal("TypeOf[ModelResponse] must match a routed ModelResponse")
	}
	if !TypeOf[ChildEvent]()(&ChildEvent{}) {
		t.Fatal("TypeOf[ChildEvent] must match a ChildEvent")
	}
	observation := &ChildEvent{Inner: &ModelResponse{}}
	if TypeOf[ModelResponse]()(observation) {
		t.Fatal("a ChildEvent is an observation, not a local ModelResponse trigger")
	}
}

func TestDelegationEventsWeighNothing(t *testing.T) {
	for _, ev := range []Event{
		&ChildEvent{Inner: &TextInput{Content: strings.Repeat("x", 400)}},
		&TransientChildEvent{ChildEvent: ChildEvent{Inner: &TextInput{Content: "x"}}},
		&DeliveryFailed{Reason: strings.Repeat("x", 400)},
		&DownEvent{Inner: &TextInput{Content: "x"}},
		&TransientDownEvent{DownEvent: DownEvent{Inner: &TextInput{Content: "x"}}},
	} {
		if got := EstimatedTokens(ev, 4); got != 0 {
			t.Errorf("%s: EstimatedTokens = %d, want 0", ev.EventType(), got)
		}
		if got := RenderForPrompt(ev); got != "" {
			t.Errorf("%s: RenderForPrompt = %q, want empty", ev.EventType(), got)
		}
	}
}

func TestUsageEventID(t *testing.T) {
	u := NewUsageEvent(Usage{TotalTokens: I64(3)}, UsageKindModelCall)
	if len(u.EventID) != 32 || strings.Contains(u.EventID, "-") {
		t.Fatalf("NewUsageEvent id = %q, want 32 hex chars", u.EventID)
	}
	if got := u.EnsureEventID(); got != u.EventID {
		t.Fatalf("EnsureEventID = %q, want the existing %q", got, u.EventID)
	}
	zero := &UsageEvent{Kind: UsageKindModelCall}
	first := zero.EnsureEventID()
	if first == "" || zero.EnsureEventID() != first {
		t.Fatal("EnsureEventID must generate once and stay stable")
	}

	data, err := MarshalEvent(u)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	back, err := UnmarshalEvent(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := back.(*UsageEvent).EventID; got != u.EventID {
		t.Fatalf("round-trip event_id = %q, want %q", got, u.EventID)
	}

	missing := []byte(`{"@type":"UsageEvent","kind":"model_call","usage":{}}`)
	back, err = UnmarshalEvent(missing)
	if err != nil {
		t.Fatalf("unmarshal missing id: %v", err)
	}
	if got := back.(*UsageEvent).EventID; got == "" {
		t.Fatal("an archived UsageEvent without event_id must get a fresh one")
	}
	empty := []byte(`{"@type":"UsageEvent","event_id":"","kind":"model_call","usage":{}}`)
	back, err = UnmarshalEvent(empty)
	if err != nil {
		t.Fatalf("unmarshal empty id: %v", err)
	}
	if got := back.(*UsageEvent).EventID; got != "" {
		t.Fatalf("an explicit empty event_id must be preserved, got %q", got)
	}
}

func TestDelegationEventsRegistered(t *testing.T) {
	registered := map[string]bool{}
	for _, id := range RegisteredTypes() {
		registered[id] = true
	}
	for _, id := range []string{"ChildEvent", "TransientChildEvent", "DeliveryFailed", "DownEvent", "TransientDownEvent"} {
		if !registered[id] {
			t.Errorf("%s is not registered", id)
		}
	}
	if registered["RoutedEvent"] {
		t.Error("RoutedEvent is a transport wrapper, not a wire type")
	}
}

func TestDelegationDegenerateValues(t *testing.T) {
	// An envelope may omit the inner event and paths; the wire stays explicit.
	child := &ChildEvent{TaskID: "t", AgentName: "w"}
	data, err := MarshalEvent(child)
	if err != nil {
		t.Fatalf("marshal nil inner: %v", err)
	}
	if !strings.Contains(string(data), `"inner":null`) || !strings.Contains(string(data), `"path":[]`) {
		t.Fatalf("degenerate ChildEvent wire = %s", data)
	}
	back, err := UnmarshalEvent(data)
	if err != nil {
		t.Fatalf("unmarshal nil inner: %v", err)
	}
	if decoded := back.(*ChildEvent); decoded.Inner != nil || decoded.Direction != DirectionUp {
		t.Fatalf("decoded = %#v, want nil inner and direction up", decoded)
	}
	if SourceEvent(back) != nil {
		t.Fatal("an envelope with no inner event has no source")
	}

	// Explicit empty directions decode to their defaults.
	childJSON := []byte(`{"@type":"ChildEvent","inner":null,"task_id":"t","path":[],"agent_name":"w","origin_event_id":"","direction":""}`)
	back, err = UnmarshalEvent(childJSON)
	if err != nil {
		t.Fatalf("unmarshal empty direction: %v", err)
	}
	if got := back.(*ChildEvent).Direction; got != DirectionUp {
		t.Fatalf("direction = %q, want %q", got, DirectionUp)
	}

	if _, err := MarshalEvent(nil); err == nil {
		t.Fatal("MarshalEvent(nil) must return an error")
	}
	if _, ok := DeliveryOf((*RoutedEvent)(nil)); ok {
		t.Fatal("a nil routed wrapper carries no delivery")
	}
	var nilRouted *RoutedEvent
	if AsValue(nilRouted) != nil {
		t.Fatal("AsValue(nil routed) must return nil")
	}
	if !nilRouted.At().IsZero() || nilRouted.Transient() || nilRouted.Conversational() {
		t.Fatal("nil routed wrapper must delegate to zero values")
	}
	if got := nilRouted.EventType(); got != "RoutedEvent" {
		t.Fatalf("nil routed type = %q", got)
	}
	if got, err := nilRouted.MarshalJSON(); err != nil || string(got) != "null" {
		t.Fatalf("nil routed JSON = %s, %v", got, err)
	}
}

func TestDelegationConstructorsNormalizePaths(t *testing.T) {
	inner := testUsageEvent("inner")
	child := NewChildEvent(inner, "t", nil, "w", "inner")
	if child.Path == nil || len(child.Path) != 0 {
		t.Fatalf("NewChildEvent path = %#v, want an empty non-nil slice", child.Path)
	}
	if child.Direction != DirectionUp {
		t.Fatalf("NewChildEvent direction = %q", child.Direction)
	}
	down := NewDownEvent(inner, "t", nil, "w")
	if down.Path == nil || len(down.Path) != 0 {
		t.Fatalf("NewDownEvent path = %#v, want an empty non-nil slice", down.Path)
	}
	if down.OriginPath == nil || len(down.OriginPath) != 0 {
		t.Fatalf("NewDownEvent origin path = %#v, want an empty non-nil slice", down.OriginPath)
	}
	if down.Direction != DirectionDown {
		t.Fatalf("NewDownEvent direction = %q", down.Direction)
	}
}

func TestSourceEventTypedNil(t *testing.T) {
	cases := []Event{
		(*ChildEvent)(nil),
		(*TransientChildEvent)(nil),
		(*DownEvent)(nil),
		(*TransientDownEvent)(nil),
		(*RoutedEvent)(nil),
		(*UsageEvent)(nil),
	}
	for _, ev := range cases {
		if got := SourceEvent(ev); got != nil {
			t.Errorf("SourceEvent(%T(nil)) = %v, want nil", ev, got)
		}
	}
}

func TestDelegationConcurrentRead(t *testing.T) {
	source := testUsageEvent("race")
	observation := &ChildEvent{Inner: source, TaskID: "t", Path: []string{"t"}, AgentName: "w", OriginEventID: "race"}
	routed := Routed(observation, Delivery{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if SourceEvent(routed) != Event(source) {
					t.Error("SourceEvent race result changed")
					return
				}
				if !TypeOf[ChildEvent]()(routed) {
					t.Error("TypeOf race result changed")
					return
				}
				if _, ok := DeliveryOf(routed); !ok {
					t.Error("DeliveryOf race result changed")
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestDelegationConstructors(t *testing.T) {
	inner := testUsageEvent("origin")
	path := []string{"root", "child"}

	child := NewChildEvent(inner, "child", path, "worker", "origin")
	if child.Direction != DirectionUp {
		t.Fatalf("NewChildEvent direction = %q, want %q", child.Direction, DirectionUp)
	}
	if child.TaskID != "child" || child.AgentName != "worker" || child.OriginEventID != "origin" {
		t.Fatalf("NewChildEvent dropped provenance: %#v", child)
	}
	if !reflect.DeepEqual(child.Path, path) {
		t.Fatalf("NewChildEvent path = %#v, want %#v", child.Path, path)
	}
	if got := SourceEvent(child); got != Event(inner) {
		t.Fatalf("SourceEvent(NewChildEvent) = %T, want the authored event", got)
	}

	down := NewDownEvent(inner, "child", path, "worker")
	if down.Direction != DirectionDown {
		t.Fatalf("NewDownEvent direction = %q, want %q", down.Direction, DirectionDown)
	}
	if down.TaskID != "child" || down.AgentName != "worker" {
		t.Fatalf("NewDownEvent dropped provenance: %#v", down)
	}
	if !reflect.DeepEqual(down.Path, path) {
		t.Fatalf("NewDownEvent path = %#v, want %#v", down.Path, path)
	}
}

// wireObject marshals ev through the public entry point (so the @type
// discriminator is present) and returns the decoded JSON object.
func wireObject(t *testing.T, ev Event) map[string]any {
	t.Helper()
	data, err := MarshalEvent(ev)
	if err != nil {
		t.Fatalf("marshal %T: %v", ev, err)
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatalf("decode %T JSON: %v", ev, err)
	}
	return obj
}

func assertWireKeys(t *testing.T, obj map[string]any, want ...string) {
	t.Helper()
	got := make([]string, 0, len(obj))
	for k := range obj {
		got = append(got, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wire fields = %v, want %v (upstream snake_case names)", got, want)
	}
}

func TestDelegationWireFieldsMatchUpstream(t *testing.T) {
	inner := testUsageEvent("inner")

	child := NewChildEvent(inner, "t1", []string{"root", "t1"}, "worker", "inner")
	obj := wireObject(t, child)
	assertWireKeys(t, obj, "@type", "created_at", "inner", "task_id", "path", "agent_name", "origin_event_id", "direction")
	if obj["@type"] != "ChildEvent" || obj["direction"] != DirectionUp {
		t.Fatalf("ChildEvent discriminator/defaults = %v/%v", obj["@type"], obj["direction"])
	}
	innerObj, ok := obj["inner"].(map[string]any)
	if !ok || innerObj["@type"] != "UsageEvent" || innerObj["event_id"] != "inner" {
		t.Fatalf("nested inner = %#v, want a UsageEvent carrying its event_id", obj["inner"])
	}
	if _, ok := obj["source_event"]; ok {
		t.Fatal("source_event is derived, never serialized")
	}

	down := NewDownEvent(inner, "t2", []string{"root", "t2"}, "worker")
	down.OriginTaskID = "t1"
	down.OriginPath = []string{"root"}
	down.OriginStreamID = "stream-1"
	obj = wireObject(t, down)
	assertWireKeys(t, obj, "@type", "created_at", "inner", "task_id", "path", "agent_name", "direction",
		"origin_task_id", "origin_path", "origin_stream_id")
	if obj["@type"] != "DownEvent" || obj["direction"] != DirectionDown {
		t.Fatalf("DownEvent discriminator/defaults = %v/%v", obj["@type"], obj["direction"])
	}

	// task_id is None upstream; AGo omits an empty optional field.
	obj = wireObject(t, &DeliveryFailed{Reason: "gone"})
	assertWireKeys(t, obj, "@type", "created_at", "reason")
	obj = wireObject(t, &DeliveryFailed{TaskID: "t3", Reason: "gone"})
	assertWireKeys(t, obj, "@type", "created_at", "task_id", "reason")
}

func TestDownEventWireNormalizesDefaults(t *testing.T) {
	down := &DownEvent{Inner: testUsageEvent("d"), TaskID: "t", AgentName: "w"}
	obj := wireObject(t, down)
	if obj["direction"] != DirectionDown {
		t.Fatalf("direction = %v, want %q", obj["direction"], DirectionDown)
	}
	if path, ok := obj["path"].([]any); !ok || len(path) != 0 {
		t.Fatalf("path = %#v, want []", obj["path"])
	}
	if originPath, ok := obj["origin_path"].([]any); !ok || len(originPath) != 0 {
		t.Fatalf("origin_path = %#v, want []", obj["origin_path"])
	}

	raw := []byte(`{"@type":"DownEvent","inner":{"@type":"UsageEvent","event_id":"d","kind":"model_call","usage":{}},"task_id":"t","agent_name":"w"}`)
	back, err := UnmarshalEvent(raw)
	if err != nil {
		t.Fatalf("unmarshal down without direction: %v", err)
	}
	decoded, ok := back.(*DownEvent)
	if !ok || decoded.Direction != DirectionDown {
		t.Fatalf("decoded = %#v, want direction %q", back, DirectionDown)
	}
}

func TestRoutedDelegatesShape(t *testing.T) {
	created := time.Unix(1_700_000_000, 0)
	cases := []struct {
		name           string
		inner          Event
		transient      bool
		conversational bool
	}{
		{"usage", &UsageEvent{Base: Base{CreatedAt: created}}, false, false},
		{"chunk", &ModelMessageChunk{Base: Base{CreatedAt: created}}, true, false},
		{"text", &TextInput{Base: Base{CreatedAt: created}, Content: "x"}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			routed := Routed(tc.inner, Delivery{})
			if got := routed.EventType(); got != tc.inner.EventType() {
				t.Fatalf("EventType = %q, want %q", got, tc.inner.EventType())
			}
			if got := routed.Transient(); got != tc.transient {
				t.Fatalf("Transient = %v, want %v", got, tc.transient)
			}
			if got := routed.Conversational(); got != tc.conversational {
				t.Fatalf("Conversational = %v, want %v", got, tc.conversational)
			}
			if got := routed.At(); !got.Equal(created) {
				t.Fatalf("At = %v, want %v", got, created)
			}
		})
	}

	empty := Routed(nil, Delivery{})
	if !empty.At().IsZero() || empty.Transient() || empty.Conversational() {
		t.Fatalf("nil inner must delegate to zero values: %v %v %v",
			empty.At(), empty.Transient(), empty.Conversational())
	}
	if got := empty.EventType(); got != "RoutedEvent" {
		t.Fatalf("nil inner EventType = %q", got)
	}
}

func TestTypeOfAnySeesRoutedAuthorType(t *testing.T) {
	responseType := reflect.TypeOf(ModelResponse{})
	routed := Routed(&ModelResponse{Message: &ModelMessage{Content: "hi"}}, Delivery{})
	if !TypeOfAny(responseType)(routed) {
		t.Fatal("TypeOfAny must match the authored type through a routed wrapper")
	}
	if !TypeOfAny(responseType)(&ModelResponse{}) {
		t.Fatal("TypeOfAny must keep matching an ordinary pointer event")
	}
	if TypeOfAny(responseType)(nil) {
		t.Fatal("TypeOfAny must not match a nil event")
	}

	observation := Routed(&ChildEvent{Inner: &ModelResponse{}}, Delivery{})
	if !TypeOfAny(reflect.TypeOf(ChildEvent{}))(observation) {
		t.Fatal("a routed ChildEvent keeps the observation type, not its inner type")
	}
	if TypeOfAny(responseType)(observation) {
		t.Fatal("routing must not turn an observation into a local response")
	}

	if typeMatches(nil, nil) || typeMatches(nil, responseType) || typeMatches(responseType, nil) {
		t.Fatal("typeMatches must reject nil types instead of panicking")
	}
}

func TestAllOfIgnoresNilConditions(t *testing.T) {
	ev := &ModelResponse{}
	if !AllOf(nil, TypeOf[ModelResponse]())(ev) {
		t.Fatal("AllOf must ignore a nil condition")
	}
	if !AllOf(nil)(ev) {
		t.Fatal("AllOf(nil) matches everything")
	}
	if AllOf(nil, TypeOf[ModelResponse]())(&TextInput{Content: "x"}) {
		t.Fatal("AllOf must still require its non-nil conditions")
	}
}

func TestRegisterEventIsIdempotent(t *testing.T) {
	before := RegisteredTypes()
	RegisterEvent(UsageEvent{})
	RegisterEvent(ChildEvent{})
	if after := RegisteredTypes(); !reflect.DeepEqual(before, after) {
		t.Fatalf("re-registering a type changed the registry: %v -> %v", before, after)
	}
	back, err := UnmarshalEvent([]byte(`{"@type":"UsageEvent","event_id":"x","kind":"model_call","usage":{}}`))
	if err != nil {
		t.Fatalf("unmarshal after re-registration: %v", err)
	}
	if got := back.(*UsageEvent).EventID; got != "x" {
		t.Fatalf("re-registration replaced the factory: event_id = %q", got)
	}
}

// marshalFailEvent exercises the nested-event error path: its payload cannot
// be serialized, so an envelope must surface the error instead of guessing.
type marshalFailEvent struct{}

func (marshalFailEvent) EventType() string    { return "MarshalFailEvent" }
func (marshalFailEvent) At() time.Time        { return time.Time{} }
func (marshalFailEvent) Transient() bool      { return false }
func (marshalFailEvent) Conversational() bool { return false }
func (marshalFailEvent) MarshalJSON() ([]byte, error) {
	return nil, errors.New("marshalFailEvent: boom")
}

func TestDelegationNestedErrorsAreReported(t *testing.T) {
	if _, err := MarshalEvent(&ChildEvent{Inner: marshalFailEvent{}}); err == nil {
		t.Fatal("ChildEvent must propagate an inner marshal error")
	}
	if _, err := MarshalEvent(&DownEvent{Inner: marshalFailEvent{}}); err == nil {
		t.Fatal("DownEvent must propagate an inner marshal error")
	}
	for _, raw := range [][]byte{
		[]byte(`{"@type":"ChildEvent","inner":{"no_type":true}}`),
		[]byte(`{"@type":"DownEvent","inner":{"no_type":true}}`),
	} {
		if _, err := UnmarshalEvent(raw); err == nil {
			t.Fatalf("inner payload without @type must fail: %s", raw)
		}
	}
	var usage UsageEvent
	if err := usage.UnmarshalJSON([]byte(`{`)); err == nil {
		t.Fatal("UsageEvent must report malformed JSON")
	}
}

func TestSourceEventAndAsValueAreCycleSafe(t *testing.T) {
	// A malformed self-referential envelope must terminate instead of hanging.
	child := &ChildEvent{TaskID: "cycle"}
	child.Inner = child
	if got := SourceEvent(child); got != Event(child) {
		t.Fatalf("SourceEvent(cycle) = %T, want the envelope after the bounded walk", got)
	}

	var nilRouted *RoutedEvent
	if got := SourceEvent(nilRouted); got != nil {
		t.Fatalf("SourceEvent(nil routed) = %v, want nil", got)
	}

	routed := &RoutedEvent{}
	routed.Inner = routed
	if got := AsValue(routed); got != Event(routed) {
		t.Fatalf("AsValue(cycle) = %T, want the wrapper after the bounded walk", got)
	}
}

func TestChildEventArchivedInnerGetsEventID(t *testing.T) {
	// An archived observation whose usage payload predates event_id: the inner
	// record must decode with a generated id so ancestors can deduplicate it.
	raw := []byte(`{"@type":"ChildEvent","inner":{"@type":"UsageEvent","kind":"model_call","usage":{"total_tokens":30}},"task_id":"t","path":["t"],"agent_name":"w","origin_event_id":"legacy","direction":"up"}`)
	back, err := UnmarshalEvent(raw)
	if err != nil {
		t.Fatalf("unmarshal archived ChildEvent: %v", err)
	}
	child, ok := back.(*ChildEvent)
	if !ok {
		t.Fatalf("decoded %T, want *ChildEvent", back)
	}
	inner, ok := child.Inner.(*UsageEvent)
	if !ok {
		t.Fatalf("inner = %T, want *UsageEvent", child.Inner)
	}
	if len(inner.EventID) != 32 || strings.Contains(inner.EventID, "-") {
		t.Fatalf("generated inner event_id = %q, want 32 hex chars", inner.EventID)
	}
	if got := SourceEvent(child); got != Event(inner) {
		t.Fatalf("SourceEvent(archived child) = %T, want the generated UsageEvent", got)
	}
	obj := wireObject(t, child)
	innerObj, ok := obj["inner"].(map[string]any)
	if !ok || innerObj["event_id"] != inner.EventID {
		t.Fatalf("re-marshaled inner event_id = %#v, want %q", innerObj["event_id"], inner.EventID)
	}
}

// TestRoutedEventStampNowReachesInner pins timestamp stamping through the
// transport wrapper: a routed copy that reaches the stream without its inner
// being sent directly still gets a creation time, and nil wrappers are ignored.
func TestRoutedEventStampNowReachesInner(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	inner := &ModelResponse{}
	Routed(inner, Delivery{}).StampNow(now)
	if !inner.CreatedAt.Equal(now) {
		t.Fatalf("inner CreatedAt = %v, want %v", inner.CreatedAt, now)
	}

	// A value-form inner event has no StampNow method; the wrapper must not panic.
	Routed(ModelResponse{}, Delivery{}).StampNow(now)

	var nilRouted *RoutedEvent
	nilRouted.StampNow(now)
	(&RoutedEvent{}).StampNow(now)
}
