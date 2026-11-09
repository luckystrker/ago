package agent

import (
	stdcontext "context"
	"sync"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/observers"
	"github.com/luckystrker/ago/stream"
	agotest "github.com/luckystrker/ago/testing"
)

// lifecycleObserver records the observer lifecycle events it sees and counts its
// registrations.
type lifecycleObserver struct {
	name string

	mu         sync.Mutex
	registered int
	seen       []string
}

func (o *lifecycleObserver) Name() string { return o.name }

func (o *lifecycleObserver) Register(cx *agocontext.Context) error {
	o.mu.Lock()
	o.registered++
	o.mu.Unlock()
	cx.Stream.Subscribe(func(ev events.Event) error {
		switch e := events.AsValue(ev).(type) {
		case events.ObserverStarted:
			o.mu.Lock()
			o.seen = append(o.seen, "started:"+e.Name)
			o.mu.Unlock()
		case events.ObserverCompleted:
			o.mu.Lock()
			o.seen = append(o.seen, "completed:"+e.Name)
			o.mu.Unlock()
		}
		return nil
	}, stream.SubscribeOptions{})
	return nil
}

func (o *lifecycleObserver) events() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.seen...)
}

// TestConstructorObserversLifecycle pins the observer lifecycle (Python parity:
// Agent(observers=...) + _observer_lifecycle): the observer registers on the
// turn's stream, ObserverStarted reaches it after registration, and
// ObserverCompleted reaches it when the turn scope closes.
func TestConstructorObserversLifecycle(t *testing.T) {
	base := stream.NewMemoryStream()
	obs := &lifecycleObserver{name: "recorder"}
	a := New("observed", "", WithConfig(agotest.NewTestConfig(textResp("ok"))),
		WithStreamBase(base), WithObservers(obs))
	if _, err := a.Ask(stdcontext.Background(), "hi"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if obs.registered != 1 {
		t.Fatalf("registrations = %d, want 1", obs.registered)
	}
	want := []string{"started:recorder", "completed:recorder"}
	got := obs.events()
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("observer lifecycle = %v, want %v", got, want)
	}
}

// TestPerInvocationObservers pins observers= on one call: only that invocation
// registers the observer and brackets it, the next call does not.
func TestPerInvocationObservers(t *testing.T) {
	base := stream.NewMemoryStream()
	obs := &lifecycleObserver{name: "per-call"}
	a := New("observed", "", WithConfig(agotest.NewTestConfig(textResp("one"), textResp("two"))),
		WithStreamBase(base))
	if _, err := a.AskWith(stdcontext.Background(), RunOptions{Observers: []observers.Observer{obs}}, "hi"); err != nil {
		t.Fatalf("first ask: %v", err)
	}
	if obs.registered != 1 {
		t.Fatalf("registrations after first ask = %d, want 1", obs.registered)
	}
	if _, err := a.Ask(stdcontext.Background(), "again"); err != nil {
		t.Fatalf("second ask: %v", err)
	}
	if obs.registered != 1 {
		t.Fatalf("registrations after second ask = %d, want 1", obs.registered)
	}
}
