package observers

import (
	"errors"
	"sync"
	"testing"

	agocontext "ago/context"
	"ago/events"
	"ago/stream"
)

// Interface compliance: every shipped type satisfies Observer.
var (
	_ Observer = (*StreamObserver)(nil)
	_ Observer = (*LoopDetector)(nil)
	_ Observer = (*TokenMonitor)(nil)
	_ Observer = (*CompositeObserver)(nil)
)

// newConversation builds a stream plus its conversation context.
func newConversation() (*stream.MemoryStream, *agocontext.Context) {
	s := stream.NewMemoryStream()
	return s, agocontext.New(s)
}

// collectAlerts subscribes an ObserverAlert collector on s; the returned
// function snapshots the alerts seen so far.
func collectAlerts(s *stream.MemoryStream) func() []*events.ObserverAlert {
	var mu sync.Mutex
	var alerts []*events.ObserverAlert
	s.Subscribe(func(ev events.Event) error {
		switch a := events.AsValue(ev).(type) {
		case events.ObserverAlert:
			mu.Lock()
			defer mu.Unlock()
			alerts = append(alerts, &a)
		}
		return nil
	}, stream.SubscribeOptions{Condition: events.TypeOf[events.ObserverAlert]()})
	return func() []*events.ObserverAlert {
		mu.Lock()
		defer mu.Unlock()
		return append([]*events.ObserverAlert(nil), alerts...)
	}
}

// resp builds a ModelResponse carrying the given text.
func resp(content string) *events.ModelResponse {
	return &events.ModelResponse{Message: &events.ModelMessage{Content: content}}
}

// usageEvt builds a UsageEvent with the given total.
func usageEvt(total int64) *events.UsageEvent {
	return &events.UsageEvent{Usage: events.Usage{TotalTokens: events.I64(total)}, Kind: events.UsageKindModelCall}
}

func TestStreamObserverDeliversMatchingEvents(t *testing.T) {
	s, cx := newConversation()

	var got []string
	obs := NewStreamObserver("responses", events.TypeOf[events.ModelResponse](), func(ev events.Event) {
		if r, ok := ev.(*events.ModelResponse); ok {
			got = append(got, r.Content())
		}
	})
	if err := obs.Register(cx); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := s.Send(resp("one")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := s.Send(&events.TextInput{Content: "noise"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := s.Send(resp("two")); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("callback saw %v, want [one two]", got)
	}
	if obs.Name() != "responses" {
		t.Fatalf("Name() = %q, want %q", obs.Name(), "responses")
	}
}

func TestStreamObserverNilConditionMatchesAll(t *testing.T) {
	s, cx := newConversation()

	var count int
	obs := NewStreamObserver("all", nil, func(ev events.Event) { count++ })
	if err := obs.Register(cx); err != nil {
		t.Fatalf("Register: %v", err)
	}

	for _, ev := range []events.Event{resp("a"), &events.TextInput{Content: "b"}, usageEvt(1)} {
		if err := s.Send(ev); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	if count != 3 {
		t.Fatalf("callback ran %d times, want 3", count)
	}
}

func TestStreamObserverRegisterErrors(t *testing.T) {
	obs := NewStreamObserver("x", nil, func(events.Event) {})

	if err := obs.Register(nil); !errors.Is(err, ErrNilContext) {
		t.Fatalf("Register(nil) = %v, want ErrNilContext", err)
	}
	cx := &agocontext.Context{} // Stream unset
	if err := obs.Register(cx); !errors.Is(err, ErrNoStream) {
		t.Fatalf("Register(no stream) = %v, want ErrNoStream", err)
	}
	_, cx2 := newConversation()
	if err := NewStreamObserver("y", nil, nil).Register(cx2); !errors.Is(err, ErrNilCallback) {
		t.Fatalf("Register(nil callback) = %v, want ErrNilCallback", err)
	}
}

func TestCompositeObserverRegistersChildren(t *testing.T) {
	s, cx := newConversation()
	alerts := collectAlerts(s)

	loop := NewLoopDetector(2)
	monitor := NewTokenMonitor(100)
	comp := NewCompositeObserver(loop, monitor, NewStreamObserver("spy", nil, func(events.Event) {}))
	if comp.Name() != "composite" {
		t.Fatalf("Name() = %q", comp.Name())
	}
	if err := comp.Register(cx); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := s.Send(resp("same")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := s.Send(resp("same")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := s.Send(usageEvt(150)); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := alerts()
	if len(got) != 2 {
		t.Fatalf("got %d alerts, want 2 (loop + token)", len(got))
	}
	if got[0].Source != "loop_detector" || got[0].Severity != events.SeverityWarning {
		t.Fatalf("alert[0] = %s/%s, want loop_detector/warning", got[0].Source, got[0].Severity)
	}
	if got[1].Source != "token_monitor" || got[1].Severity != events.SeverityError {
		t.Fatalf("alert[1] = %s/%s, want token_monitor/error", got[1].Source, got[1].Severity)
	}
	if monitor.Total() != 150 {
		t.Fatalf("monitor.Total() = %d, want 150", monitor.Total())
	}
}

func TestCompositeObserverNilMember(t *testing.T) {
	_, cx := newConversation()

	comp := NewCompositeObserver(NewLoopDetector(2), nil)
	err := comp.Register(cx)
	if !errors.Is(err, ErrNilObserver) {
		t.Fatalf("Register with nil member = %v, want ErrNilObserver", err)
	}
}

func TestCompositeObserverWrapsChildError(t *testing.T) {
	_, cx := newConversation()

	broken := NewStreamObserver("no-callback", nil, nil) // Register fails: nil fn
	comp := NewCompositeObserver(broken)
	err := comp.Register(cx)
	if !errors.Is(err, ErrNilCallback) {
		t.Fatalf("Register = %v, want wrapped ErrNilCallback", err)
	}
}
