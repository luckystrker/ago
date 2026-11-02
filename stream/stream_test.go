package stream

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
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

// TestGetBuffersBurst pins audit L-21: Get must not drop matching events when a
// burst exceeds any fixed channel capacity — the answer to a HITL question has
// to survive whatever else arrives around it.
func TestGetBuffersBurst(t *testing.T) {
	s := NewMemoryStream(WithLogger(discardLogger()))
	const burst = 64
	got := make(chan events.Event, 1)
	errCh := make(chan error, 1)
	go func() {
		ev, err := s.Get(context.Background(), events.TypeOf[events.ModelMessage]())
		if err != nil {
			errCh <- err
			return
		}
		got <- ev
	}()
	time.Sleep(100 * time.Millisecond) // let Get's subscription register
	for i := 0; i < burst; i++ {
		if err := s.Send(&events.ModelMessage{Content: strconv.Itoa(i)}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	select {
	case ev := <-got:
		if m, ok := events.AsValue(ev).(events.ModelMessage); !ok || m.Content != "0" {
			t.Fatalf("first matching event lost or reordered: %#v", ev)
		}
	case err := <-errCh:
		t.Fatalf("get: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Get lost the matching events under burst")
	}
}

// TestInboxCapEvictsOldest pins audit M-28: with WithInboxCap set, an Enqueue
// past the cap evicts the oldest pending request instead of growing forever.
func TestInboxCapEvictsOldest(t *testing.T) {
	s := NewMemoryStream(WithLogger(discardLogger()), WithInboxCap(2))
	for i := 0; i < 3; i++ {
		if err := s.Enqueue(&events.TextInput{Content: strconv.Itoa(i)}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	pending := s.Pending()
	if len(pending) != 2 {
		t.Fatalf("pending = %d requests, want 2", len(pending))
	}
	for i, want := range []string{"1", "2"} {
		if ti, ok := pending[i].Parts[0].(*events.TextInput); !ok || ti.Content != want {
			t.Fatalf("pending[%d] = %#v, want TextInput %q", i, pending[i].Parts[0], want)
		}
	}
}

// TestTranscriptCapEvictsOldest pins audit M-28: with WithTranscriptCap set,
// writes past the cap evict the oldest events from the durable transcript.
func TestTranscriptCapEvictsOldest(t *testing.T) {
	s := NewMemoryStream(WithLogger(discardLogger()), WithTranscriptCap(3))
	for i := 0; i < 5; i++ {
		if err := s.Send(&events.HumanMessage{Content: strconv.Itoa(i)}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	evs, err := s.History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("transcript = %d events, want 3", len(evs))
	}
	for i, want := range []string{"2", "3", "4"} {
		if hm, ok := events.AsValue(evs[i]).(events.HumanMessage); !ok || hm.Content != want {
			t.Fatalf("evs[%d] = %#v, want HumanMessage %q", i, evs[i], want)
		}
	}
}

// TestEnqueueOriginDefaults pins the inbox origin: Enqueue stays service (the
// Python Stream.enqueue default), EnqueueOrigin carries the sender's origin and
// normalizes an empty origin to service.
func TestEnqueueOriginDefaults(t *testing.T) {
	s := NewMemoryStream()
	if err := s.Enqueue(textInput("svc")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := s.EnqueueOrigin(events.OriginUser, textInput("usr")); err != nil {
		t.Fatalf("enqueue origin: %v", err)
	}
	if err := s.EnqueueOrigin("", textInput("empty")); err != nil {
		t.Fatalf("enqueue empty origin: %v", err)
	}
	pending := s.Pending()
	if len(pending) != 3 {
		t.Fatalf("pending = %d requests, want 3", len(pending))
	}
	for i, want := range []events.RequestOrigin{events.OriginService, events.OriginUser, events.OriginService} {
		if pending[i].Origin != want {
			t.Fatalf("pending[%d].Origin = %q, want %q", i, pending[i].Origin, want)
		}
	}
}

// TestSubStreamEnqueueOriginDelegates pins that a filtered view forwards the
// origin to the shared parent inbox (and keeps the empty-parts no-op).
func TestSubStreamEnqueueOriginDelegates(t *testing.T) {
	s := NewMemoryStream()
	view := s.Where(events.TypeOf[events.ModelResponse]())
	if err := view.EnqueueOrigin(events.OriginUser, textInput("via-view")); err != nil {
		t.Fatalf("view enqueue: %v", err)
	}
	pending := s.Pending()
	if len(pending) != 1 || pending[0].Origin != events.OriginUser {
		t.Fatalf("parent inbox = %#v, want one user-origin request", pending)
	}
	if err := view.EnqueueOrigin(events.OriginUser); err != nil {
		t.Fatalf("empty view enqueue: %v", err)
	}
	// Enqueue with no parts stays a no-op on the view too: an empty request must
	// never enter the shared inbox.
	if err := view.Enqueue(); err != nil {
		t.Fatalf("empty view enqueue: %v", err)
	}
	if len(s.Pending()) != 1 {
		t.Fatalf("empty parts must stay a no-op on the view, inbox = %d requests", len(s.Pending()))
	}
}

// spyStorage records persistence calls, so the WithStorage wiring can be pinned
// by identity instead of by reading the history back through the default store.
type spyStorage struct {
	mu   sync.Mutex
	saved []events.Event
	replaced [][]events.Event
}

func (s *spyStorage) SaveEvent(id string, ev events.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = append(s.saved, ev)
	return nil
}

func (s *spyStorage) GetHistory(id string) ([]events.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]events.Event, len(s.saved))
	copy(out, s.saved)
	return out, nil
}

func (s *spyStorage) SetHistory(id string, evs []events.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]events.Event, len(evs))
	copy(cp, evs)
	s.replaced = append(s.replaced, cp)
	s.saved = cp
	return nil
}

func (s *spyStorage) DropHistory(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = nil
	return nil
}

func (s *spyStorage) snapshot() []events.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]events.Event, len(s.saved))
	copy(out, s.saved)
	return out
}

// TestWithStorageWiredIntoHistory pins the storage identity: WithStorage hands
// the transcript its argument, so a caller-backed store sees every persisted
// event and the history reads through it (Python parity: MemoryStream(storage=...)
// keys one store under the stream id).
func TestWithStorageWiredIntoHistory(t *testing.T) {
	st := &spyStorage{}
	s := NewMemoryStream(WithStreamID("pinned"), WithStorage(st))
	if s.History().Storage() != Storage(st) {
		t.Fatal("WithStorage must hand the exact storage to the transcript")
	}
	if err := s.Send(&events.HumanMessage{Content: "pin"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := len(st.snapshot()); got != 1 {
		t.Fatalf("spy storage saw %d persisted events, want 1", got)
	}
	evs, err := s.History().GetEvents()
	if err != nil || len(evs) != 1 {
		t.Fatalf("history through the spy storage = %d events, %v", len(evs), err)
	}
	// Without WithStorage the default in-memory store takes over (fresh stream,
	// separate transcript): the spy must stay untouched.
	plain := NewMemoryStream(WithStreamID("plain"))
	_ = plain.Send(&events.HumanMessage{Content: "elsewhere"})
	if got := len(st.snapshot()); got != 1 {
		t.Fatalf("an unrelated stream wrote into the spy storage: %d events", got)
	}
}

// TestSendReturnsFirstSubscriberError pins the error aggregation: with several
// failing normal subscribers, Send surfaces the FIRST error, not the last.
func TestSendReturnsFirstSubscriberError(t *testing.T) {
	errA := errors.New("first fails")
	errB := errors.New("second fails")
	s := NewMemoryStream(WithLogger(discardLogger()))
	s.Subscribe(func(events.Event) error { return errA }, SubscribeOptions{})
	s.Subscribe(func(events.Event) error { return errB }, SubscribeOptions{})
	err := s.Send(&events.TextInput{Content: "x"})
	if !errors.Is(err, errA) {
		t.Fatalf("Send = %v, want the first subscriber's error %v", err, errA)
	}
	if errors.Is(err, errB) {
		t.Fatalf("Send = %v, the second error must not overwrite the first", err)
	}
}

// TestUnsubscribeRemovesOnlyTarget pins subscription cancellation: Unsubscribe
// removes exactly the subscription with that id — the others keep receiving,
// an unknown id removes nothing.
func TestUnsubscribeRemovesOnlyTarget(t *testing.T) {
	s := NewMemoryStream()
	var mu sync.Mutex
	var ran []string
	record := func(name string) Subscriber {
		return func(events.Event) error {
			mu.Lock()
			ran = append(ran, name)
			mu.Unlock()
			return nil
		}
	}
	idA := s.Subscribe(record("a"), SubscribeOptions{})
	idB := s.Subscribe(record("b"), SubscribeOptions{})
	idC := s.Subscribe(record("c"), SubscribeOptions{})
	_ = idA
	s.Unsubscribe(idB)
	_ = s.Send(&events.TextInput{Content: "x"})
	mu.Lock()
	got := strings.Join(ran, ",")
	ran = nil
	mu.Unlock()
	if got != "a,c" {
		t.Fatalf("after Unsubscribe(b) the send reached [%s], want [a,c]", got)
	}
	s.Unsubscribe(idA)
	s.Unsubscribe(999) // unknown id: no-op, must not remove anything
	_ = s.Send(&events.TextInput{Content: "y"})
	mu.Lock()
	got = strings.Join(ran, ",")
	mu.Unlock()
	if got != "c" {
		t.Fatalf("after Unsubscribe(a) and an unknown id the send reached [%s], want [c]", got)
	}
	_ = idC
}

// TestSubStreamGetUsesViewCondition pins the filtered view's wait: Get with no
// caller condition waits for an event matching the view, not for the first
// event of any kind on the shared stream.
func TestSubStreamGetUsesViewCondition(t *testing.T) {
	s := NewMemoryStream()
	view := s.Where(events.TypeOf[events.ModelResponse]())
	type outcome struct {
		ev  events.Event
		err error
	}
	got := make(chan outcome, 1)
	go func() {
		ev, err := view.Get(context.Background(), nil)
		got <- outcome{ev, err}
	}()
	time.Sleep(100 * time.Millisecond) // let Get's subscription register
	// A non-matching event must not resolve the wait.
	if err := s.Send(&events.TextInput{Content: "noise"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case o := <-got:
		t.Fatalf("Get resolved on a non-matching event: %#v (%v)", o.ev, o.err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := s.Send(&events.ModelResponse{Message: &events.ModelMessage{Content: "matched"}}); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case o := <-got:
		if o.err != nil {
			t.Fatalf("get: %v", o.err)
		}
		if mr, ok := o.ev.(*events.ModelResponse); !ok || mr.Content() != "matched" {
			t.Fatalf("Get returned %#v, want the matching ModelResponse", o.ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Get never resolved on the matching event")
	}
}

// TestSubStreamGetMergesConditions pins the intersection: a Get that passes its
// own condition onto a filtered view must match the view's condition AND the
// caller's — an event passing only one of the two must not resolve the wait
// (Python parity: stream.py SubStream.get builds the conjunction).
func TestSubStreamGetMergesConditions(t *testing.T) {
	s := NewMemoryStream()
	view := s.Where(events.TypeOf[events.HumanMessage]())
	contentOf := func(ev events.Event) string {
		switch v := events.AsValue(ev).(type) {
		case events.TextInput:
			return v.Content
		case events.HumanMessage:
			return v.Content
		}
		return ""
	}
	needle := events.Equal(contentOf, "needle")
	type outcome struct {
		ev  events.Event
		err error
	}
	got := make(chan outcome, 1)
	go func() {
		ev, err := view.Get(context.Background(), needle)
		got <- outcome{ev, err}
	}()
	time.Sleep(100 * time.Millisecond) // let Get's subscription register
	// Passes the caller's condition but not the view's: the wait must hold.
	if err := s.Send(&events.TextInput{Content: "needle"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case o := <-got:
		t.Fatalf("Get resolved on an event outside the view: %#v (%v)", o.ev, o.err)
	case <-time.After(200 * time.Millisecond):
	}
	// Passes both conditions: the wait resolves with the matching event.
	if err := s.Send(&events.HumanMessage{Content: "needle"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case o := <-got:
		if o.err != nil {
			t.Fatalf("get: %v", o.err)
		}
		if hm, ok := o.ev.(*events.HumanMessage); !ok || hm.Content != "needle" {
			t.Fatalf("Get returned %#v, want HumanMessage needle", o.ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Get never resolved on the event matching both conditions")
	}
}

// TestWhereChainsAndConditions pins the view-chain fold: Where on a filtered
// view narrows it further — the chain folds into one AND-condition, so an
// event must pass every link to reach a subscriber of the narrowed view, and
// a permissive later link cannot widen an earlier one away.
func TestWhereChainsAndConditions(t *testing.T) {
	s := NewMemoryStream()
	calls := s.Where(events.TypeOf[events.ToolCallEvent]())
	// A link that matches everything must not undo the tool-call link in front
	// of it: the fold is a conjunction.
	permissive := calls.Where(events.Condition(func(events.Event) bool { return true }))
	var mu sync.Mutex
	var got []string
	permissive.Subscribe(func(ev events.Event) error {
		mu.Lock()
		switch v := events.AsValue(ev).(type) {
		case events.ToolCallEvent:
			got = append(got, "call:"+v.Name)
		case events.HumanMessage:
			got = append(got, "human")
		}
		mu.Unlock()
		return nil
	}, SubscribeOptions{})

	matching := events.NewToolCallEvent("needle")
	_ = s.Send(&matching)
	_ = s.Send(&events.HumanMessage{Content: "not a call"})

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "call:needle" {
		t.Fatalf("chained view delivered %v, want only [call:needle] — a later link must not widen the chain", got)
	}
}