package stream

import (
	"errors"
	"testing"

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
