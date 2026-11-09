package stream

import (
	"errors"
	"testing"
	"time"

	"github.com/luckystrker/ago/events"
)

func TestSendValidatesDeliveryBeforeEachHandler(t *testing.T) {
	s := NewMemoryStream()
	calls := 0
	validate := func() error {
		calls++
		return nil
	}
	var got events.Event
	s.Subscribe(func(ev events.Event) error { return nil }, SubscribeOptions{Interrupt: true})
	s.Subscribe(func(ev events.Event) error { got = ev; return nil }, SubscribeOptions{})

	inner := &events.ModelResponse{Message: &events.ModelMessage{Content: "hello"}}
	routed := events.Routed(inner, events.Delivery{Validate: validate})
	if err := s.Send(routed); err != nil {
		t.Fatalf("send: %v", err)
	}
	// interrupt, persistence, normal subscriber.
	if calls != 3 {
		t.Fatalf("validate calls = %d, want 3 (before every handler)", calls)
	}
	if _, ok := got.(*events.RoutedEvent); !ok {
		t.Fatalf("subscriber saw %T, want the routed wrapper", got)
	}
	evs, err := s.History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("history holds %d events, want the inner event only", len(evs))
	}
	if _, ok := events.AsValue(evs[0]).(events.ModelResponse); !ok {
		t.Fatalf("persisted %T, want ModelResponse", evs[0])
	}
}

func TestSendStopsWhenDeliveryIsNoLongerValid(t *testing.T) {
	s := NewMemoryStream()
	calls := 0
	boom := errors.New("delegation ended")
	validate := func() error {
		calls++
		if calls >= 2 {
			return boom
		}
		return nil
	}
	var normal bool
	s.Subscribe(func(ev events.Event) error { return nil }, SubscribeOptions{Interrupt: true})
	s.Subscribe(func(ev events.Event) error { normal = true; return nil }, SubscribeOptions{})

	err := s.Send(events.Routed(&events.ModelResponse{}, events.Delivery{Validate: validate}))
	if !errors.Is(err, boom) {
		t.Fatalf("send error = %v, want %v", err, boom)
	}
	if normal {
		t.Fatal("normal subscriber ran after delivery validation failed")
	}
	evs, _ := s.History().GetEvents()
	if len(evs) != 0 {
		t.Fatalf("invalid delivery was persisted: %d events", len(evs))
	}
}

// TestRoutedWithoutValidatorIsDelivered pins that the delivery wrapper is
// optional transport data: a routed copy with no guard still reaches handlers
// and the transcript as the authored event.
func TestRoutedWithoutValidatorIsDelivered(t *testing.T) {
	s := NewMemoryStream()
	inner := &events.ModelResponse{Message: &events.ModelMessage{Content: "ok"}}
	if err := s.Send(events.Routed(inner, events.Delivery{})); err != nil {
		t.Fatalf("send routed without validator: %v", err)
	}
	evs, err := s.History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("history holds %d events, want 1", len(evs))
	}
	if _, ok := evs[0].(*events.ModelResponse); !ok {
		t.Fatalf("persisted %T, want *ModelResponse", evs[0])
	}
}

// TestPersistedEventUnwrapsOnlyCompleteRouted pins the transport-only nature of
// the wrapper: only a populated routed copy is stripped for storage; a typed nil
// and an empty wrapper pass through untouched.
func TestPersistedEventUnwrapsOnlyCompleteRouted(t *testing.T) {
	inner := &events.ModelResponse{Message: &events.ModelMessage{Content: "x"}}
	routed := events.Routed(inner, events.Delivery{})
	if got := persistedEvent(routed); got != inner {
		t.Fatalf("populated wrapper persisted as %#v, want the inner event", got)
	}

	empty := &events.RoutedEvent{}
	if got := persistedEvent(empty); got != empty {
		t.Fatalf("empty wrapper = %#v, want the wrapper itself", got)
	}

	var nilRouted *events.RoutedEvent
	got := persistedEvent(nilRouted)
	if got == nil {
		t.Fatal("typed-nil wrapper collapsed to a nil event")
	}
	if _, ok := got.(*events.RoutedEvent); !ok {
		t.Fatalf("typed-nil wrapper persisted as %T", got)
	}

	plain := &events.TextInput{Content: "plain"}
	if got := persistedEvent(plain); got != plain {
		t.Fatalf("ordinary event = %#v, want the same event", got)
	}
}

// TestRoutedStampNowReachesInner pins timestamp stamping through the transport
// wrapper: the authored event gets the creation time even when only the wrapper
// travelled through Send, and nil wrappers are ignored.
func TestRoutedStampNowReachesInner(t *testing.T) {
	now := time.Now()
	inner := &events.ModelResponse{}
	routed := events.Routed(inner, events.Delivery{})
	routed.StampNow(now)
	if !inner.CreatedAt.Equal(now) {
		t.Fatalf("inner timestamp = %v, want %v", inner.CreatedAt, now)
	}

	// A value-form inner event has no StampNow method; the wrapper must not
	// panic and must leave it untouched.
	events.Routed(events.ModelResponse{}, events.Delivery{}).StampNow(now)

	var nilRouted *events.RoutedEvent
	nilRouted.StampNow(now)
	(&events.RoutedEvent{}).StampNow(now)
}

// TestRoutedTransientNotPersisted pins the storage policy of the authored
// event: a transient wrapped event is delivered but never stored.
func TestRoutedTransientNotPersisted(t *testing.T) {
	s := NewMemoryStream()
	var got bool
	s.Subscribe(func(ev events.Event) error {
		_, got = events.AsValue(ev).(events.ModelMessageChunk)
		return nil
	}, SubscribeOptions{})
	if err := s.Send(events.Routed(&events.ModelMessageChunk{Content: "chunk"}, events.Delivery{})); err != nil {
		t.Fatalf("send: %v", err)
	}
	if !got {
		t.Fatal("transient routed event did not reach the subscriber")
	}
	if evs, _ := s.History().GetEvents(); len(evs) != 0 {
		t.Fatalf("transient routed event was persisted: %d events", len(evs))
	}
}

// TestSendValidatesBeforeFirstHandler pins that an invalid delivery stops
// delivery at the first handler, before any interrupt subscriber reacts.
func TestSendValidatesBeforeFirstHandler(t *testing.T) {
	s := NewMemoryStream()
	interrupted := false
	s.Subscribe(func(ev events.Event) error { interrupted = true; return nil }, SubscribeOptions{Interrupt: true})
	boom := errors.New("gone")
	err := s.Send(events.Routed(&events.ModelResponse{}, events.Delivery{Validate: func() error { return boom }}))
	if !errors.Is(err, boom) {
		t.Fatalf("send error = %v, want %v", err, boom)
	}
	if interrupted {
		t.Fatal("interrupt subscriber ran before delivery validation")
	}
}
