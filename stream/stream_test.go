package stream

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/luckystrker/ago/events"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestSendDispatchOrderAndConditions(t *testing.T) {
	s := NewMemoryStream()
	var order []string
	s.Subscribe(func(ev events.Event) error { order = append(order, "a"); return nil }, SubscribeOptions{})
	s.Subscribe(func(ev events.Event) error { order = append(order, "b"); return nil }, SubscribeOptions{})
	s.Subscribe(func(ev events.Event) error { order = append(order, "c"); return nil },
		SubscribeOptions{Condition: events.TypeOf[events.ModelResponse]()})

	if err := s.Send(&events.ModelMessage{Content: "x"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(order) != 2 {
		t.Fatalf("condition-filtered subscriber must not run: %v", order)
	}
	if err := s.Send(&events.ModelResponse{}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(order) != 5 || order[4] != "c" {
		t.Fatalf("unexpected order: %v", order)
	}
}

func TestSendInterruptSwallows(t *testing.T) {
	s := NewMemoryStream()
	var ran []string
	s.Subscribe(func(ev events.Event) error {
		return errors.New("stop here")
	}, SubscribeOptions{Interrupt: true, Condition: events.TypeOf[events.TextInput]()})
	s.Subscribe(func(ev events.Event) error { ran = append(ran, "normal"); return nil }, SubscribeOptions{})

	err := s.Send(&events.TextInput{Content: "swallowed"})
	if err == nil {
		t.Fatal("interrupt error must surface from Send")
	}
	if len(ran) != 0 {
		t.Fatal("normal subscriber must not run after interrupt error")
	}
	// Non-matching interrupt passes through.
	if err := s.Send(&events.ModelMessage{Content: "ok"}); err != nil {
		t.Fatalf("non-matching interrupt: %v", err)
	}
	if len(ran) != 1 {
		t.Fatalf("normal subscriber should have run: %v", ran)
	}
}

func TestSendSubscriberErrorLoggedNotFatal(t *testing.T) {
	s := NewMemoryStream(WithLogger(discardLogger()))
	var ran []string
	s.Subscribe(func(ev events.Event) error { return errors.New("boom") }, SubscribeOptions{})
	s.Subscribe(func(ev events.Event) error { ran = append(ran, "after"); return nil }, SubscribeOptions{})
	if err := s.Send(&events.TextInput{Content: "x"}); err == nil {
		t.Fatal("first subscriber error should surface")
	}
	if len(ran) != 1 {
		t.Fatalf("delivery must continue after normal-subscriber error: %v", ran)
	}
}

func TestPersistenceTransientAndPersistAll(t *testing.T) {
	s := NewMemoryStream()
	_ = s.Send(&events.TextInput{Content: "durable"})
	_ = s.Send(&events.ModelMessageChunk{Content: "transient"})
	h, _ := s.History().GetEvents()
	if len(h) != 1 {
		t.Fatalf("want 1 persisted event, got %d", len(h))
	}
	s2 := NewMemoryStream(WithPersistAll())
	_ = s2.Send(&events.TextInput{Content: "durable"})
	_ = s2.Send(&events.ModelMessageChunk{Content: "transient"})
	h2, _ := s2.History().GetEvents()
	if len(h2) != 2 {
		t.Fatalf("persist-all: want 2, got %d", len(h2))
	}
}

func TestEnqueueDrainPending(t *testing.T) {
	s := NewMemoryStream()
	if err := s.Enqueue(&events.TextInput{Content: "one"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := s.Enqueue(&events.TextInput{Content: "two"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if len(s.Pending()) != 2 {
		t.Fatalf("pending: want 2, got %d", len(s.Pending()))
	}
	drained := s.Drain()
	if len(drained) != 2 || drained[0].Parts[0].(*events.TextInput).Content != "one" {
		t.Fatalf("drain order wrong: %+v", drained)
	}
	if len(s.Pending()) != 0 || len(s.Drain()) != 0 {
		t.Fatal("drain must empty the inbox")
	}
}

func TestReentrantSendNoDeadlock(t *testing.T) {
	s := NewMemoryStream()
	done := make(chan struct{})
	s.Subscribe(func(ev events.Event) error {
		if _, ok := ev.(*events.TextInput); ok {
			return s.Send(&events.ModelMessage{Content: "nested"})
		}
		close(done)
		return nil
	}, SubscribeOptions{})
	go func() {
		_ = s.Send(&events.TextInput{Content: "outer"})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reentrant send deadlocked")
	}
}

func TestSubStreamSubscribeAndSend(t *testing.T) {
	// WithPersistAll: the forwarded event below is a ModelMessage (transient by
	// default), and the test asserts it reaches the shared transcript.
	s := NewMemoryStream(WithPersistAll())
	// Unconditional recorder on the parent: a send through the filtered view
	// must still reach it (Python parity: stream.py SubStream.send forwards).
	var parentGot []string
	s.Subscribe(func(ev events.Event) error {
		parentGot = append(parentGot, ev.EventType())
		return nil
	}, SubscribeOptions{})
	toolView := s.Where(events.TypeOf[events.ToolCallEvent]())
	var got []string
	toolView.Subscribe(func(ev events.Event) error {
		got = append(got, ev.(*events.ToolCallEvent).Name)
		return nil
	}, SubscribeOptions{})

	_ = s.Send(&events.ModelMessage{Content: "not a call"})
	call := events.NewToolCallEvent("search")
	_ = s.Send(&call)
	if len(got) != 1 || got[0] != "search" {
		t.Fatalf("tool view saw %v", got)
	}
	// Send through the view forwards unconditionally to the parent: the parent
	// dispatches to all subscribers and persists the event; only this view's
	// own filtered subscription ignores non-matching events.
	if err := toolView.Send(&events.ModelMessage{Content: "not a call"}); err != nil {
		t.Fatalf("view send: %v", err)
	}
	if len(parentGot) != 3 || parentGot[2] != "ModelMessage" {
		t.Fatalf("parent must receive non-matching view sends: %v", parentGot)
	}
	if len(got) != 1 {
		t.Fatal("filtered subscription must still ignore non-matching sends")
	}
	h, _ := s.History().GetEvents()
	persisted := false
	for _, ev := range h {
		if ev.EventType() == "ModelMessage" {
			persisted = true
		}
	}
	if !persisted {
		t.Fatal("event sent through the view must persist on the shared transcript")
	}
	nested := events.NewToolCallEvent("calc")
	if err := toolView.Send(&nested); err != nil {
		t.Fatalf("view send: %v", err)
	}
	if len(got) != 2 || got[1] != "calc" {
		t.Fatalf("view send should deliver: %v", got)
	}
	if len(parentGot) != 4 {
		t.Fatalf("parent saw %v", parentGot)
	}
}

func TestGetRespectsCancellation(t *testing.T) {
	s := NewMemoryStream()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.Get(ctx, events.TypeOf[events.ModelResponse]()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	// Get resolves on a matching incoming event.
	go func() {
		time.Sleep(10 * time.Millisecond)
		_ = s.Send(&events.ModelResponse{Message: &events.ModelMessage{Content: "done"}})
	}()
	ev, err := s.Get(context.Background(), events.TypeOf[events.ModelResponse]())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ev.(*events.ModelResponse).Content() != "done" {
		t.Fatalf("wrong event: %v", ev)
	}
}

func TestHistoryCloseUnanswered(t *testing.T) {
	s := NewMemoryStream()
	call := events.NewToolCallEvent("t")
	if err := s.Send(&call); err != nil {
		t.Fatalf("send: %v", err)
	}
	n, err := s.History().CloseUnanswered()
	if err != nil || n != 1 {
		t.Fatalf("close unanswered: n=%d err=%v", n, err)
	}
	h, _ := s.History().GetEvents()
	if len(h) != 2 {
		t.Fatalf("want call+repair persisted, got %d", len(h))
	}
	if n2, _ := s.History().CloseUnanswered(); n2 != 0 {
		t.Fatalf("second repair pass must find nothing, got %d", n2)
	}
}

func TestLoadHistoryAndWhere(t *testing.T) {
	seed := []events.Event{
		&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "hi"}}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: "hello"}},
	}
	s, err := LoadHistory("fixed", NewMemoryStorage(), seed)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if s.ID() != "fixed" {
		t.Fatalf("id: %s", s.ID())
	}
	h, _ := s.History().GetEvents()
	if len(h) != 2 {
		t.Fatalf("history: %d", len(h))
	}
}

func TestSpawnBackgroundWait(t *testing.T) {
	s := NewMemoryStream()
	fin := make(chan struct{})
	s.SpawnBackground(context.Background(), func(ctx context.Context) { close(fin) })
	s.WaitBackground()
	select {
	case <-fin:
	default:
		t.Fatal("background task did not finish before WaitBackground returned")
	}
}

func TestTimestampsStampedOnSend(t *testing.T) {
	s := NewMemoryStream()
	if err := s.Send(&events.TextInput{Content: "x"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	h, _ := s.History().GetEvents()
	if h[0].At().IsZero() {
		t.Fatal("CreatedAt must be stamped on send")
	}
}

// TestSendErrHandledSwallowsWithoutPersist pins the swallow semantics: an
// interrupt subscriber returning ErrHandled stops delivery, Send reports success
// and the transcript never sees the event (Python parity: an interrupter
// returning None stops send before the persistence subscriber).
func TestSendErrHandledSwallowsWithoutPersist(t *testing.T) {
	s := NewMemoryStream(WithPersistAll())
	var ran []string
	s.Subscribe(func(ev events.Event) error { ran = append(ran, "swallower"); return ErrHandled },
		SubscribeOptions{Interrupt: true, Condition: events.TypeOf[events.TextInput]()})
	s.Subscribe(func(ev events.Event) error { ran = append(ran, "late-interrupt"); return nil },
		SubscribeOptions{Interrupt: true})
	s.Subscribe(func(ev events.Event) error { ran = append(ran, "normal"); return nil }, SubscribeOptions{})

	if err := s.Send(&events.TextInput{Content: "swallowed"}); err != nil {
		t.Fatalf("swallowed send must succeed, got %v", err)
	}
	if len(ran) != 1 || ran[0] != "swallower" {
		t.Fatalf("delivery must stop at ErrHandled: %v", ran)
	}
	if h, _ := s.History().GetEvents(); len(h) != 0 {
		t.Fatalf("swallowed event must not persist, got %d events", len(h))
	}

	// An event outside the swallower's condition flows: the later interrupt and
	// normal subscribers run, and the event persists.
	if err := s.Send(&events.ModelResponse{Message: &events.ModelMessage{Content: "delivered"}}); err != nil {
		t.Fatalf("non-matching interrupt: %v", err)
	}
	if len(ran) != 3 || ran[1] != "late-interrupt" || ran[2] != "normal" {
		t.Fatalf("delivery must continue past a non-matching interrupt: %v", ran)
	}
	if h, _ := s.History().GetEvents(); len(h) != 1 {
		t.Fatalf("delivered event must persist, got %d events", len(h))
	}
}
