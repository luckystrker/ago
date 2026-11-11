package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luckystrker/ago/knowledge"
	"github.com/luckystrker/ago/network/transport"
)

// hubProcess is one run of a hub over a shared store, served on its own
// listener — "restarting the hub" replaces it by another one on the same store.
type hubProcess struct {
	hub  *Hub
	ln   *transport.MemListener
	stop context.CancelFunc
	done chan struct{}
}

func startHubProcess(t *testing.T, store knowledge.Store, cur *atomic.Pointer[transport.MemListener]) *hubProcess {
	t.Helper()
	hub, err := OpenHub(WithHubStore(store), WithAckTimeout(3*time.Second),
		WithTTLSweepInterval(0), WithExpectationSweepInterval(0))
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	ln := transport.NewMemListener(1024)
	ctx, stop := context.WithCancel(context.Background())
	p := &hubProcess{hub: hub, ln: ln, stop: stop, done: make(chan struct{})}
	go func() { defer close(p.done); _ = hub.Serve(ctx, ln) }()
	cur.Store(ln)
	return p
}

func (p *hubProcess) kill() {
	p.stop()
	_ = p.ln.Close()
	<-p.done
	_ = p.hub.Close()
}

type textLog struct {
	mu    sync.Mutex
	texts []string
}

func (l *textLog) handler() AgentOption {
	return WithEnvironment(func(env Envelope) (string, error) {
		if env.EventType == EnvText {
			l.mu.Lock()
			l.texts = append(l.texts, textOf(env))
			l.mu.Unlock()
		}
		return "", nil
	})
}

func (l *textLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.texts...)
}

// The finish line of N3: an agent goes offline, messages arrive, the hub
// restarts (OpenHub over the store), and the agent reconnects over the link and
// gets everything it missed exactly once and in order; the other agent's client
// finds its way back by itself.
func TestOfflineAgentGetsEverythingAfterAHubRestart(t *testing.T) {
	store := knowledge.NewMemoryStore()
	var cur atomic.Pointer[transport.MemListener]
	dial := func(ctx context.Context) (transport.Link, error) { return cur.Load().Dial() }
	hp := startHubProcess(t, store, &cur)
	defer func() { hp.kill() }()

	events := make(chan ConnectionEvent, 16)
	pol := ReconnectPolicy{Initial: 5 * time.Millisecond, Max: 20 * time.Millisecond, Factor: 2, MaxAttempts: 200}
	hcA, err := ConnectHub(context.Background(), RemoteConfig{Dial: dial, RPCTimeout: 5 * time.Second, Reconnect: &pol,
		OnEvent: func(ev ConnectionEvent) { events <- ev }})
	if err != nil {
		t.Fatal(err)
	}
	defer hcA.Close()
	hcB, err := ConnectHub(context.Background(), RemoteConfig{Dial: dial, RPCTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	bobLog1 := &textLog{}
	bob, err := hcB.Register("bob", bobLog1.handler())
	if err != nil {
		t.Fatal(err)
	}
	alice, err := hcA.Register("alice", WithEnvironment(func(Envelope) (string, error) { return "", nil }))
	if err != nil {
		t.Fatal(err)
	}
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	if err := alice.Say(ch.ID, "m1"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(bobLog1.snapshot()) == 1 })
	waitFor(t, func() bool { return hp.hub.InboxCursor(bob.AgentID(), ch.ID) != "" }) // m1 acked and persisted

	// bob goes offline; m2..m4 arrive while he is away.
	if err := hcB.Close(); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"m2", "m3", "m4"} {
		if err := alice.Say(ch.ID, m); err != nil {
			t.Fatal(err)
		}
	}

	// The hub restarts: everything in memory is gone, the store stays.
	hp.kill()
	hp = startHubProcess(t, store, &cur)
	// alice's client reconnects by itself (backoff policy) and re-attaches.
	deadline := time.After(10 * time.Second)
	for reconnected := false; !reconnected; {
		select {
		case ev := <-events:
			reconnected = ev.Kind == ConnReconnected
		case <-deadline:
			t.Fatal("alice's client never reconnected")
		}
	}
	if err := alice.Say(ch.ID, "m5"); err != nil {
		t.Fatalf("say after the hub restart: %v", err)
	}

	// bob comes back over the link.
	hcB2, err := ConnectHub(context.Background(), RemoteConfig{Dial: dial, RPCTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer hcB2.Close()
	bobLog2 := &textLog{}
	if _, err := hcB2.Attach("bob", bobLog2.handler()); err != nil {
		t.Fatalf("attach: %v", err)
	}
	waitFor(t, func() bool { return len(bobLog2.snapshot()) >= 4 })
	if err := alice.Say(ch.ID, "m6"); err != nil { // live, after the replay
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(bobLog2.snapshot()) >= 5 })
	time.Sleep(50 * time.Millisecond)
	want := []string{"m2", "m3", "m4", "m5", "m6"}
	if got := bobLog2.snapshot(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("bob got %v, want %v (exactly once, in order, m1 not again)", got, want)
	}
	if got := bobLog1.snapshot(); fmt.Sprint(got) != "[m1]" {
		t.Fatalf("the first life of bob saw %v", got)
	}
}

// Automatic reconnect: the policy's backoff and limit.
func TestReconnectPolicyBackoff(t *testing.T) {
	p := ReconnectPolicy{Initial: 100 * time.Millisecond, Max: 1 * time.Second, Factor: 2}
	for i, want := range []time.Duration{100, 200, 400, 800, 1000, 1000} {
		if got := p.delay(i+1, nil); got != want*time.Millisecond {
			t.Errorf("attempt %d waits %v, want %v", i+1, got, want*time.Millisecond)
		}
	}
	p.Jitter = 0.5
	if lo, hi := p.delay(1, func() float64 { return 0 }), p.delay(1, func() float64 { return 0.999999 }); lo != 50*time.Millisecond || hi < 149*time.Millisecond || hi > 150*time.Millisecond {
		t.Errorf("jitter ±50%% of 100ms: %v .. %v", lo, hi)
	}
	d := DefaultReconnectPolicy()
	if d.Initial <= 0 || d.Max < d.Initial || d.Factor <= 1 || d.MaxAttempts <= 0 {
		t.Errorf("default policy %+v must back off and be bounded", d)
	}
}

func TestAutoReconnectGivesUpAfterTheLimit(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	ln := transport.NewMemListener(16)
	hub := newTestHub(t)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = hub.Serve(ctx, ln) }()
	defer func() { stop(); <-done }()
	events := make(chan ConnectionEvent, 16)
	hc, err := ConnectHub(ctx, RemoteConfig{
		Dial: func(context.Context) (transport.Link, error) {
			if !up.Load() {
				return nil, errors.New("connection refused")
			}
			return ln.Dial()
		},
		RPCTimeout: time.Second,
		Reconnect:  &ReconnectPolicy{Initial: time.Millisecond, Max: 2 * time.Millisecond, Factor: 2, MaxAttempts: 3},
		OnEvent:    func(ev ConnectionEvent) { events <- ev },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer hc.Close()
	if _, err := hc.Register("alice"); err != nil {
		t.Fatal(err)
	}
	up.Store(false)
	stop() // hub goes away: the link closes, redials are refused
	var kinds []string
	timeout := time.After(5 * time.Second)
	for len(kinds) < 2 || kinds[len(kinds)-1] != ConnGaveUp {
		select {
		case ev := <-events:
			kinds = append(kinds, ev.Kind)
			if ev.Kind == ConnGaveUp && (ev.Attempt != 3 || ev.Err == nil) {
				t.Fatalf("gave up after attempt %d with %v, want 3 attempts and a cause", ev.Attempt, ev.Err)
			}
		case <-timeout:
			t.Fatalf("events so far: %v", kinds)
		}
	}
	if kinds[0] != ConnLost {
		t.Fatalf("event order %v", kinds)
	}
	// Requests fail typed rather than hang.
	if err := hc.Call(context.Background(), OpGetAgent, nameOrIDParams{NameOrID: "alice"}, nil); !errors.Is(err, ErrConnectionLost) {
		t.Fatalf("call after giving up: %v", err)
	}
}

// lossyLink drops the receipts the client sends while lossy is set — an
// acknowledgement lost together with its connection.
type lossyLink struct {
	transport.Link
	lossy *atomic.Bool
}

func (l lossyLink) Send(f transport.Frame) error {
	if f.Kind == transport.FrameReceipt && l.lossy.Load() {
		return nil
	}
	return l.Link.Send(f)
}

// D13: an ack lost with the connection is made good by the client re-sending
// its high-water mark after the reconnect, so the persisted cursor catches up
// although the replay (bounded by the since mark) has nothing left to send.
func TestReconnectReacksTheHighWaterMark(t *testing.T) {
	rig := newWireRig(t)
	var lossy atomic.Bool
	hcA, hcB := rig.connect(), rig.connect()
	hcL, err := ConnectHub(rig.ctx, RemoteConfig{RPCTimeout: 5 * time.Second, Dial: func(ctx context.Context) (transport.Link, error) {
		l, err := rig.ln.Dial()
		return lossyLink{Link: l, lossy: &lossy}, err
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer hcL.Close()
	_ = hcB
	log := &textLog{}
	bob, err := hcL.Register("bob", log.handler())
	if err != nil {
		t.Fatal(err)
	}
	alice, _ := hcA.Register("alice", WithEnvironment(func(Envelope) (string, error) { return "", nil }))
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return rig.hub.InboxCursor(bob.AgentID(), ch.ID) != "" }) // invite and opened acked
	base := rig.hub.InboxCursor(bob.AgentID(), ch.ID)
	lossy.Store(true)
	sent, err := alice.Send(NewTextEnvelope(ch.ID, alice.AgentID(), "", "lost ack"))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(log.snapshot()) == 1 })
	time.Sleep(30 * time.Millisecond)
	if got := rig.hub.InboxCursor(bob.AgentID(), ch.ID); got != base {
		t.Fatalf("the lossy link let an ack through: cursor %s", got)
	}
	lossy.Store(false)
	if err := hcL.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return rig.hub.InboxCursor(bob.AgentID(), ch.ID) == sent.ID })
	time.Sleep(30 * time.Millisecond)
	if got := log.snapshot(); len(got) != 1 {
		t.Fatalf("the reconnect redelivered the envelope although the client had it: %v", got)
	}
}

// A nack stops the channel on the connection — including envelopes already in
// flight — and a reconnect brings the rest back in order.
func TestRemoteNackStallsTheChannelUntilReconnect(t *testing.T) {
	n := newFakeNet()
	rc := newRemoteOnFake(t, n)
	var mu sync.Mutex
	var seen []string
	if err := rc.attach("ag", func(env Envelope) {
		if env.ID == "e2" {
			panic("cannot handle e2")
		}
		mu.Lock()
		seen = append(seen, env.ID)
		mu.Unlock()
	}); err != nil {
		t.Fatal(err)
	}
	cn, _ := rc.current()
	notify := func(id string) {
		payload, _ := json.Marshal(Envelope{ID: id, ChannelID: "ch", EventType: EnvText})
		rc.onNotify(cn, transport.Frame{Kind: transport.FrameNotify, RecipientID: "ag", ChannelID: "ch", EnvelopeID: id, Payload: payload})
	}
	for _, id := range []string{"e1", "e2", "e3"} {
		notify(id)
	}
	waitFor(t, func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return len(n.toHub) >= 2
	})
	time.Sleep(30 * time.Millisecond)
	n.mu.Lock()
	var codes []string
	for _, hf := range n.toHub {
		codes = append(codes, hf.f.EnvelopeID+":"+hf.f.Code)
	}
	n.mu.Unlock()
	if fmt.Sprint(codes) != "[e1:ack e2:nack]" {
		t.Fatalf("receipts %v, want e1 acked and e2 nacked and nothing for e3", codes)
	}
	mu.Lock()
	if fmt.Sprint(seen) != "[e1]" {
		t.Fatalf("delivered %v: e3 must not be delivered behind a nacked e2", seen)
	}
	mu.Unlock()
	// the high-water mark stays below the nacked envelope
	ag := rc.agents["ag"]
	ag.mu.Lock()
	high := ag.high["ch"]
	ag.mu.Unlock()
	if high != "e1" {
		t.Fatalf("high-water mark %q passed the nacked envelope", high)
	}
	// reconnect: the stall is cleared
	if err := rc.reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	cn, _ = rc.current()
	notify("e4")
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(seen) == 2 })
}

// Closing a HubClient stops everything it started: the pump and the delivery
// queue of every agent and human. Goroutines do not pile up across clients.
func TestHubClientCloseStopsEveryPump(t *testing.T) {
	rig := newWireRig(t)
	runtimeSettle := func() int {
		time.Sleep(50 * time.Millisecond)
		return runtime.NumGoroutine()
	}
	base := runtimeSettle()
	var clients []*HubClient
	for i := 0; i < 5; i++ {
		hc := rig.connect()
		if _, err := hc.Register(fmt.Sprint("agent", i), WithEnvironment(func(Envelope) (string, error) { return "", nil })); err != nil {
			t.Fatal(err)
		}
		if _, err := hc.RegisterHuman(fmt.Sprint("human", i)); err != nil {
			t.Fatal(err)
		}
		clients = append(clients, hc)
	}
	during := runtimeSettle()
	if during < base+5 {
		t.Fatalf("expected the clients to run goroutines (%d → %d)", base, during)
	}
	for _, hc := range clients {
		if err := hc.Close(); err != nil {
			t.Fatal(err)
		}
	}
	var after int
	waitFor(t, func() bool { after = runtimeSettle(); return after <= base+3 })
	if after > base+3 {
		t.Fatalf("%d goroutines before, %d after closing 5 clients with 10 identities", base, after)
	}
	// an in-process client stops its pumps too
	hub := newTestHub(t)
	local := NewHubClient(hub)
	for i := 0; i < 4; i++ {
		if _, err := local.Register(fmt.Sprint("local", i), WithEnvironment(func(Envelope) (string, error) { return "", nil })); err != nil {
			t.Fatal(err)
		}
	}
	runtimeBefore := runtimeSettle()
	_ = local.Close()
	waitFor(t, func() bool { return runtimeSettle() <= runtimeBefore-4 })
}

// A request whose result is empty (an op that answers null) succeeds even when
// the caller supplied a destination.
func TestCallWithEmptyResultLeavesTheDestinationAlone(t *testing.T) {
	rig := newWireRig(t)
	hc := rig.connect()
	alice, _ := hc.Register("alice")
	out := Rule{InboundGlobs: []string{"keep"}}
	if err := hc.Call(context.Background(), OpSetRule, setRuleParams{AgentID: alice.AgentID(), Rule: DefaultRule()}, &out); err != nil {
		t.Fatalf("an op with an empty result: %v", err)
	}
	if len(out.InboundGlobs) != 1 || out.InboundGlobs[0] != "keep" {
		t.Fatalf("an empty result changed the destination: %+v", out)
	}
	if err := hc.Call(context.Background(), OpGetRule, agentIDParams{AgentID: alice.AgentID()}, &out); err != nil || out.InboundGlobs[0] != "*" {
		t.Fatalf("a real result must fill it: %+v %v", out, err)
	}
}

func TestReconnectPolicyDelayBoundaries(t *testing.T) {
	// Factor 0 and 1 mean a constant wait; factors below 1 never shrink it.
	for _, f := range []float64{0, 1, 0.5} {
		p := ReconnectPolicy{Initial: 100 * time.Millisecond, Max: time.Second, Factor: f}
		for n := 1; n <= 5; n++ {
			if got := p.delay(n, nil); got != 100*time.Millisecond {
				t.Errorf("factor %v attempt %d waits %v, want a constant 100ms", f, n, got)
			}
		}
	}
	// A cap that is exactly reached, then held.
	p := ReconnectPolicy{Initial: 100 * time.Millisecond, Max: 400 * time.Millisecond, Factor: 2}
	for n, want := range []time.Duration{100, 200, 400, 400, 400} {
		if got := p.delay(n+1, nil); got != want*time.Millisecond {
			t.Errorf("attempt %d waits %v, want %v", n+1, got, want*time.Millisecond)
		}
	}
	// An initial wait above the cap is capped; no cap means unbounded growth.
	if got := (ReconnectPolicy{Initial: 5 * time.Second, Max: time.Second, Factor: 2}).delay(1, nil); got != time.Second {
		t.Errorf("initial above the cap: %v", got)
	}
	if got := (ReconnectPolicy{Initial: time.Second, Factor: 3}).delay(4, nil); got != 27*time.Second {
		t.Errorf("no cap: %v", got)
	}
	// Jitter never produces a negative wait, and no jitter source means none applied.
	j := ReconnectPolicy{Initial: 100 * time.Millisecond, Jitter: 5}
	if got := j.delay(1, func() float64 { return 0 }); got != 0 {
		t.Errorf("jitter beyond 100%% must clamp at zero, got %v", got)
	}
	if got := j.delay(1, nil); got != 100*time.Millisecond {
		t.Errorf("without a random source the wait is exact: %v", got)
	}
	// Jitter widens symmetrically around the wait.
	k := ReconnectPolicy{Initial: 100 * time.Millisecond, Jitter: 0.25}
	if lo, hi := k.delay(1, func() float64 { return 0 }), k.delay(1, func() float64 { return 0.5 }); lo != 75*time.Millisecond || hi != 100*time.Millisecond {
		t.Errorf("jitter ±25%%: %v / %v", lo, hi)
	}
}

// A repeated delivery (the hub re-sent it after a lost ack) is not handed to
// the agent again but is acknowledged again; an envelope with no id cannot be a
// repeat of anything.
func TestRemoteRepeatedNotifyIsAckedNotRedelivered(t *testing.T) {
	n := newFakeNet()
	rc := newRemoteOnFake(t, n)
	var mu sync.Mutex
	var seen []string
	if err := rc.attach("ag", func(env Envelope) { mu.Lock(); seen = append(seen, env.ID); mu.Unlock() }); err != nil {
		t.Fatal(err)
	}
	cn, _ := rc.current()
	notify := func(id string) {
		payload, _ := json.Marshal(Envelope{ID: id, ChannelID: "ch", EventType: EnvText})
		rc.onNotify(cn, transport.Frame{Kind: transport.FrameNotify, RecipientID: "ag", ChannelID: "ch", EnvelopeID: id, Payload: payload})
	}
	notify("e2")
	notify("e2") // the same again
	notify("e1") // an older one (a duplicate of something already passed)
	notify("")   // no id
	notify("e3")
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(seen) >= 3 })
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	got := fmt.Sprint(seen)
	mu.Unlock()
	if got != "[e2  e3]" {
		t.Fatalf("delivered %q, want e2, the id-less envelope and e3 once each", got)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	var acks []string
	for _, hf := range n.toHub {
		if hf.f.Kind == transport.FrameReceipt {
			acks = append(acks, hf.f.EnvelopeID)
		}
	}
	if fmt.Sprint(acks) != "[e2 e2 e1 e3]" {
		t.Fatalf("acks %v: every repeat is acknowledged again, the id-less one cannot be", acks)
	}
}

// Only the current connection's loss starts a reconnect; one that was already
// replaced, or a client that is closed, starts nothing.
func TestConnLostOnlyForTheCurrentConnection(t *testing.T) {
	n := newFakeNet()
	events := make(chan ConnectionEvent, 8)
	rc := &remoteClient{cfg: RemoteConfig{Dial: n.dialer(), RPCTimeout: time.Second, OnEvent: func(ev ConnectionEvent) { events <- ev }},
		agents: map[string]*remoteAgent{}, early: map[string][]transport.Frame{}}
	if err := rc.connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	old, _ := rc.current()
	if err := rc.reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	cur, _ := rc.current()
	drain := func() {
		for len(events) > 0 {
			<-events
		}
	}
	drain()
	rc.connLost(old, errors.New("old link ended")) // already replaced
	select {
	case ev := <-events:
		t.Fatalf("the end of a replaced connection was reported: %+v", ev)
	case <-time.After(30 * time.Millisecond):
	}
	rc.connLost(cur, errors.New("link ended"))
	select {
	case ev := <-events:
		if ev.Kind != ConnLost {
			t.Fatalf("event %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("the loss of the current connection was not reported")
	}
	_ = rc.close()
	drain()
	rc.connLost(cur, errors.New("after close"))
	select {
	case ev := <-events:
		t.Fatalf("a closed client reported %+v", ev)
	case <-time.After(30 * time.Millisecond):
	}
}

// The automatic reconnect gives up at once when the hub refuses the identity
// (retrying cannot help), and stops promptly when the client is closed during a
// backoff wait.
func TestAutoReconnectStopsOnRefusalAndOnClose(t *testing.T) {
	rig := newWireRig(t)
	events := make(chan ConnectionEvent, 16)
	hc, err := ConnectHub(rig.ctx, RemoteConfig{Dial: ListenerDialer(rig.ln), RPCTimeout: 2 * time.Second,
		Reconnect: &ReconnectPolicy{Initial: 5 * time.Millisecond, Max: 10 * time.Millisecond, Factor: 2, MaxAttempts: 50},
		OnEvent:   func(ev ConnectionEvent) { events <- ev }})
	if err != nil {
		t.Fatal(err)
	}
	alice, err := hc.Register("alice", WithEnvironment(func(Envelope) (string, error) { return "", nil }))
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.hub.Unregister(alice.AgentID()); err != nil { // the identity vanishes behind the client
		t.Fatal(err)
	}
	hc.remote.mu.Lock()
	cn := hc.remote.conn
	hc.remote.mu.Unlock()
	cn.link.Close() // the link drops
	var kinds []string
	var gaveUp ConnectionEvent
	for gaveUp.Kind == "" {
		select {
		case ev := <-events:
			kinds = append(kinds, ev.Kind)
			if ev.Kind == ConnGaveUp {
				gaveUp = ev
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no give-up after the identity vanished; events %v", kinds)
		}
	}
	if gaveUp.Attempt != 1 || !errors.Is(gaveUp.Err, ErrNotFound) {
		t.Fatalf("gave up at attempt %d with %v, want attempt 1 and ErrNotFound (retrying cannot help)", gaveUp.Attempt, gaveUp.Err)
	}
	_ = hc.Close()

	// closed during the wait before an attempt: the loop ends without another event
	hc2, err := ConnectHub(rig.ctx, RemoteConfig{Dial: ListenerDialer(rig.ln), RPCTimeout: 2 * time.Second,
		Reconnect: &ReconnectPolicy{Initial: time.Hour, Max: time.Hour, Factor: 1},
		OnEvent:   func(ev ConnectionEvent) { events <- ev }})
	if err != nil {
		t.Fatal(err)
	}
	for len(events) > 0 {
		<-events
	}
	hc2.remote.mu.Lock()
	cn2 := hc2.remote.conn
	hc2.remote.mu.Unlock()
	cn2.link.Close()
	waitFor(t, func() bool { hc2.remote.mu.Lock(); defer hc2.remote.mu.Unlock(); return hc2.remote.reconnecting })
	_ = hc2.Close()
	waitFor(t, func() bool { hc2.remote.mu.Lock(); defer hc2.remote.mu.Unlock(); return !hc2.remote.reconnecting })
}
