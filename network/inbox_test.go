package network

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// conversationPair opens a conversation channel between two raw-notifier
// agents (alice creator, bob recipient) and returns the channel id plus a
// recorder of what bob received.
func rawConversation(t *testing.T, h *Hub, aliceRule, bobRule *Rule) (alice, bob, chID string, bobGot *recorder) {
	t.Helper()
	a, err := h.Register(Passport{Name: "alice"}, aliceRule)
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Register(Passport{Name: "bob"}, bobRule)
	if err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	rec.attach(h, a.AgentID, true)
	rec.attach(h, b.AgentID, true)
	ch, err := h.CreateChannel(ChannelManifest{Type: TypeConversation}, []string{a.AgentID, b.AgentID})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	return a.AgentID, b.AgentID, ch.ID, rec
}

func say(h *Hub, ch, from, text string) error {
	_, err := h.Send(NewTextEnvelope(ch, from, "", text))
	return err
}

func TestInboxRejectOverflow(t *testing.T) {
	h := newTestHub(t)
	alice, bob, ch, rec := rawConversation(t, h, nil, &Rule{
		InboundGlobs: []string{"*"}, OutboundGlobs: []string{"*"},
		Inbox: InboxBlock{MaxPending: 2},
	})
	if got := h.InboxPending(bob); got != 0 {
		t.Fatalf("protocol envelopes must not count: pending %d", got)
	}

	if err := say(h, ch, alice, "one"); err != nil {
		t.Fatal(err)
	}
	if err := say(h, ch, alice, "two"); err != nil {
		t.Fatal(err)
	}
	if got := h.InboxPending(bob); got != 2 {
		t.Fatalf("pending after two sends: %d", got)
	}

	walBefore, _ := h.ReadWAL(ch)
	delivered := len(rec.forAgent(bob))
	err := say(h, ch, alice, "three")
	if !errors.Is(err, ErrInboxFull) {
		t.Fatalf("third send into a full inbox: want ErrInboxFull, got %v", err)
	}
	if errors.Is(err, ErrAccessDenied) {
		t.Fatalf("a full inbox is not an access denial: %v", err)
	}
	if walAfter, _ := h.ReadWAL(ch); len(walAfter) != len(walBefore) {
		t.Fatalf("rejected envelope leaked into the WAL: %d → %d", len(walBefore), len(walAfter))
	}
	if len(rec.forAgent(bob)) != delivered {
		t.Fatal("rejected envelope was delivered")
	}

	// The recipient finishing one envelope frees one slot.
	h.AckInbox(bob, 1)
	if err := say(h, ch, alice, "three"); err != nil {
		t.Fatalf("send after ack: %v", err)
	}
	if got := h.InboxPending(bob); got != 2 {
		t.Fatalf("pending after refill: %d", got)
	}
	// Over-acking never goes negative.
	h.AckInbox(bob, 99)
	if got := h.InboxPending(bob); got != 0 {
		t.Fatalf("over-ack: %d", got)
	}
}

func TestInboxUnlimitedByDefaultAndForDropPolicies(t *testing.T) {
	// MaxPending 0 ⇒ no cap; drop policies never gate at the hub.
	for name, rule := range map[string]*Rule{
		"zero value":  nil,
		"drop_newest": {InboundGlobs: []string{"*"}, Inbox: InboxBlock{MaxPending: 1, Overflow: OverflowDropNewest}},
		"drop_oldest": {InboundGlobs: []string{"*"}, Inbox: InboxBlock{MaxPending: 1, Overflow: OverflowDropOldest}},
	} {
		h := newTestHub(t)
		alice, _, ch, _ := rawConversation(t, h, nil, rule)
		for i := 0; i < 5; i++ {
			if err := say(h, ch, alice, "m"); err != nil {
				t.Fatalf("%s: send %d: %v", name, i, err)
			}
		}
	}
}

func TestInboxPressureHookFiresOncePerCrossing(t *testing.T) {
	type call struct {
		id           string
		pending, max int
	}
	var mu sync.Mutex
	var calls []call
	h := newTestHub(t, WithInboxPressureHook(func(id string, pending, max int) {
		mu.Lock()
		calls = append(calls, call{id, pending, max})
		mu.Unlock()
	}))
	alice, bob, ch, _ := rawConversation(t, h, nil, &Rule{
		InboundGlobs: []string{"*"}, OutboundGlobs: []string{"*"},
		Inbox: InboxBlock{MaxPending: 10}, // default high-water: 8
	})
	for i := 0; i < 10; i++ {
		if err := say(h, ch, alice, "m"); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	got := append([]call(nil), calls...)
	mu.Unlock()
	if len(got) != 1 || got[0] != (call{bob, 8, 10}) {
		t.Fatalf("want a single crossing at 8/10, got %+v", got)
	}

	// Draining below the mark re-arms the signal.
	h.AckInbox(bob, 5)
	_ = say(h, ch, alice, "m") // 5 → 6, still below
	_ = say(h, ch, alice, "m") // 7
	_ = say(h, ch, alice, "m") // 8: crossing again
	mu.Lock()
	n := len(calls)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("crossing after drain: %d calls", n)
	}
}

func TestInboxPressureDisabled(t *testing.T) {
	fired := 0
	h := newTestHub(t, WithInboxPressureHook(func(string, int, int) { fired++ }))
	alice, _, ch, _ := rawConversation(t, h, nil, &Rule{
		InboundGlobs: []string{"*"}, Inbox: InboxBlock{MaxPending: 4, HighWater: -1},
	})
	for i := 0; i < 4; i++ {
		_ = say(h, ch, alice, "m")
	}
	if fired != 0 {
		t.Fatalf("negative high-water must disable the signal, fired %d", fired)
	}
}

func TestRegisterRejectsUnknownInboxOverflow(t *testing.T) {
	h := newTestHub(t)
	_, err := h.Register(Passport{Name: "x"}, &Rule{Inbox: InboxBlock{MaxPending: 1, Overflow: "spill"}})
	if !errors.Is(err, ErrBadIdentity) {
		t.Fatalf("unknown overflow policy: %v", err)
	}
}

// --- queue-owning clients ---------------------------------------------------

// gatedBob is a client whose handler blocks until released, recording every
// text it was asked to process.
type gatedBob struct {
	client  *AgentClient
	started chan struct{} // receives once per handler entry
	release chan struct{}
	mu      sync.Mutex
	seen    []string
}

func newGatedBob(t *testing.T, hc *HubClient, inbox InboxBlock) *gatedBob {
	t.Helper()
	g := &gatedBob{started: make(chan struct{}, 16), release: make(chan struct{})}
	rule := DefaultRule()
	rule.Inbox = inbox
	c, err := hc.Register("bob", WithAgentRule(rule), WithAgentFunc(func(msg string) (string, error) {
		g.mu.Lock()
		g.seen = append(g.seen, msg)
		g.mu.Unlock()
		g.started <- struct{}{}
		<-g.release
		return "", nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	g.client = c
	return g
}

func (g *gatedBob) texts() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.seen...)
}

func waitStarted(t *testing.T, g *gatedBob) {
	t.Helper()
	select {
	case <-g.started:
	case <-time.After(3 * time.Second):
		t.Fatal("handler never started")
	}
}

// waitTexts blocks until the handler has seen n envelopes (WaitIdle can
// return in the instant between the pump popping an envelope and marking
// itself busy).
func (g *gatedBob) waitTexts(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for len(g.texts()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("handler saw %v, want %d envelopes", g.texts(), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitPending blocks until the hub's pending count for id reaches want (the
// pump acks just after the handler returns).
func waitPending(t *testing.T, h *Hub, id string, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for h.InboxPending(id) != want {
		if time.Now().After(deadline) {
			t.Fatalf("pending %d, want %d", h.InboxPending(id), want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// drain releases the handler for every remaining envelope and waits for idle.
func (g *gatedBob) drain(t *testing.T) {
	t.Helper()
	go func() {
		for {
			select {
			case g.release <- struct{}{}:
			case <-time.After(300 * time.Millisecond):
				return
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := g.client.WaitIdle(ctx); err != nil {
		t.Fatalf("bob never went idle: %v", err)
	}
}

func clientConversation(t *testing.T, inbox InboxBlock) (*Hub, *AgentClient, *gatedBob, string) {
	t.Helper()
	h := newTestHub(t)
	hc := NewHubClient(h)
	alice, err := hc.Register("alice")
	if err != nil {
		t.Fatal(err)
	}
	g := newGatedBob(t, hc, inbox)
	t.Cleanup(func() { _ = alice.Close(); _ = g.client.Close() })
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, g.client.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	return h, alice, g, ch.ID
}

func TestAgentClientInboxRejectEndToEnd(t *testing.T) {
	h, alice, g, ch := clientConversation(t, InboxBlock{MaxPending: 2})
	if err := alice.Say(ch, "m1"); err != nil {
		t.Fatal(err)
	}
	waitStarted(t, g) // m1 is in flight and still counts as pending
	if err := alice.Say(ch, "m2"); err != nil {
		t.Fatal(err)
	}
	if err := alice.Say(ch, "m3"); !errors.Is(err, ErrInboxFull) {
		t.Fatalf("m3: want ErrInboxFull, got %v", err)
	}
	if got := g.client.Dropped(); got != 0 {
		t.Fatalf("reject must stop the envelope at the hub, not drop it at the client: %d", got)
	}

	g.drain(t)
	g.waitTexts(t, 2)
	waitPending(t, h, g.client.AgentID(), 0)
	if err := alice.Say(ch, "m4"); err != nil {
		t.Fatalf("send after the inbox drained: %v", err)
	}
	g.drain(t)
	g.waitTexts(t, 3)
	if got := g.texts(); got[0] != "m1" || got[1] != "m2" || got[2] != "m4" {
		t.Fatalf("processed: %v", got)
	}
}

func TestAgentClientInboxDropNewest(t *testing.T) {
	h, alice, g, ch := clientConversation(t, InboxBlock{MaxPending: 2, Overflow: OverflowDropNewest})
	if err := alice.Say(ch, "m1"); err != nil {
		t.Fatal(err)
	}
	waitStarted(t, g)
	for _, m := range []string{"m2", "m3", "m4"} {
		if err := alice.Say(ch, m); err != nil {
			t.Fatalf("%s: drop policies accept the send: %v", m, err)
		}
	}
	if got := g.client.Dropped(); got != 1 {
		t.Fatalf("dropped: %d", got)
	}
	if err := g.client.Err(); !errors.Is(err, ErrInboxFull) {
		t.Fatalf("a drop must surface through Err, got %v", err)
	}
	g.drain(t)
	g.waitTexts(t, 3)
	if got := g.texts(); got[0] != "m1" || got[1] != "m2" || got[2] != "m3" {
		t.Fatalf("drop_newest must lose m4: %v", got)
	}
	waitPending(t, h, g.client.AgentID(), 0)
	// Every send was accepted and recorded.
	wal, _ := h.ReadWAL(ch)
	texts := 0
	for _, e := range wal {
		if e.EventType == EnvText {
			texts++
		}
	}
	if texts != 4 {
		t.Fatalf("WAL texts: %d", texts)
	}
}

func TestAgentClientInboxDropOldest(t *testing.T) {
	_, alice, g, ch := clientConversation(t, InboxBlock{MaxPending: 2, Overflow: OverflowDropOldest})
	if err := alice.Say(ch, "m1"); err != nil {
		t.Fatal(err)
	}
	waitStarted(t, g)
	for _, m := range []string{"m2", "m3", "m4"} {
		if err := alice.Say(ch, m); err != nil {
			t.Fatal(err)
		}
	}
	if got := g.client.Dropped(); got != 1 {
		t.Fatalf("dropped: %d", got)
	}
	g.drain(t)
	g.waitTexts(t, 3)
	if got := g.texts(); got[0] != "m1" || got[1] != "m3" || got[2] != "m4" {
		t.Fatalf("drop_oldest must evict m2: %v", got)
	}
}

// Without an Inbox rule an overflowing buffer behaves as it always did: the
// envelope is dropped, Err() stays nil, and only Dropped() shows it.
func TestAgentClientNoInboxRuleDropsSilentlyInErr(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)
	alice, _ := hc.Register("alice")
	gate := make(chan struct{})
	started := make(chan struct{}, 4)
	bob, err := hc.Register("bob", WithAgentInbox(1), WithAgentFunc(func(string) (string, error) {
		started <- struct{}{}
		<-gate
		return "", nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { close(gate); _ = alice.Close(); _ = bob.Close() }()
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	_ = alice.Say(ch.ID, "m1")
	<-started
	_ = alice.Say(ch.ID, "m2") // fills the 1-slot buffer
	_ = alice.Say(ch.ID, "m3") // overflows
	if bob.Dropped() != 1 {
		t.Fatalf("dropped: %d", bob.Dropped())
	}
	if bob.Err() != nil {
		t.Fatalf("without an Inbox rule Err() must stay nil, got %v", bob.Err())
	}
}

func TestHumanClientRecvFreesInboxSlot(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)
	alice, _ := hc.Register("alice")
	rule := DefaultRule()
	rule.Inbox = InboxBlock{MaxPending: 1}
	human, err := hc.RegisterHuman("hana", WithHumanRule(rule))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = alice.Close(); human.Close() }()
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, human.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	if err := alice.Say(ch.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := alice.Say(ch.ID, "again"); !errors.Is(err, ErrInboxFull) {
		t.Fatalf("second send to a 1-slot human inbox: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for { // protocol envelopes arrive first and carry no slot
		env, err := human.Recv(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if env.EventType == EnvText {
			break
		}
	}
	if err := alice.Say(ch.ID, "again"); err != nil {
		t.Fatalf("send after Recv: %v", err)
	}
}

// --- rate limiting ----------------------------------------------------------

func TestRateLimitBlocksSendsAndRefills(t *testing.T) {
	clock := newFakeClock()
	h := newTestHub(t, WithHubClock(clock.Now))
	rule := DefaultRule()
	rule.Rate = RateBlock{PerMinute: 60, Burst: 2}
	alice, _, ch, _ := rawConversation(t, h, &rule, nil)

	// Channel creation (invite/ack/opened) is protocol traffic and spent no
	// tokens: the whole burst is still available.
	if err := say(h, ch, alice, "1"); err != nil {
		t.Fatal(err)
	}
	if err := say(h, ch, alice, "2"); err != nil {
		t.Fatal(err)
	}
	walBefore, _ := h.ReadWAL(ch)
	err := say(h, ch, alice, "3")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("burst exhausted: want ErrRateLimited, got %v", err)
	}
	if walAfter, _ := h.ReadWAL(ch); len(walAfter) != len(walBefore) {
		t.Fatal("rate-limited envelope was recorded")
	}

	clock.Advance(time.Second) // 60/min = one token per second
	if err := say(h, ch, alice, "3"); err != nil {
		t.Fatalf("after one second: %v", err)
	}
	if err := say(h, ch, alice, "4"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("only one token had refilled: %v", err)
	}
}

func TestRateLimitIsPerSender(t *testing.T) {
	clock := newFakeClock()
	h := newTestHub(t, WithHubClock(clock.Now))
	rule := DefaultRule()
	rule.Rate = RateBlock{PerMinute: 60, Burst: 1}
	alice, bob, ch, _ := rawConversation(t, h, &rule, nil)
	if err := say(h, ch, alice, "a1"); err != nil {
		t.Fatal(err)
	}
	if err := say(h, ch, alice, "a2"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("alice: %v", err)
	}
	// bob has no rate rule, so alice's exhaustion does not touch him.
	for i := 0; i < 5; i++ {
		if err := say(h, ch, bob, "b"); err != nil {
			t.Fatalf("bob send %d: %v", i, err)
		}
	}
}

func TestNoRateRuleNeverLimits(t *testing.T) {
	h := newTestHub(t)
	alice, _, ch, _ := rawConversation(t, h, nil, nil)
	for i := 0; i < 200; i++ {
		if err := say(h, ch, alice, "m"); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
}

// A send addressed to oneself never books a pending slot against the sender.
func TestInboxSelfAddressedSendIsNotCounted(t *testing.T) {
	h := newTestHub(t)
	alice, bob, ch, _ := rawConversation(t, h, nil, nil)
	if _, err := h.Send(NewTextEnvelope(ch, alice, alice, "note to self")); err != nil {
		t.Fatal(err)
	}
	if got := h.InboxPending(alice); got != 0 {
		t.Fatalf("self-addressed send counted against the sender: %d", got)
	}
	if got := h.InboxPending(bob); got != 0 {
		t.Fatalf("self-addressed send counted against the peer: %d", got)
	}
}
