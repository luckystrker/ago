package events

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
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
