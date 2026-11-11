package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/luckystrker/ago/network/transport"
)

// --- a scriptable network ---------------------------------------------------------------------
//
// fakeNet stands in for the hub and the wire: every request frame the client
// sends waits in toHub until the test serves it (the response then waits in
// limbo until the test delivers it — once, twice, late, to the wrong
// connection, in any order). Nothing moves unless the test moves it, so a
// seeded schedule fully decides the interleaving of requests, answers, drops
// and reconnects (wave-4 model LinkRPC).

type fakeNet struct {
	mu    sync.Mutex
	links []*fakeLink // dial order; the last one is the newest connection
	toHub []hubFrame
	limbo []hubFrame
	sent  int
	ids   map[string]int // request id → how many times a request frame carried it
}

type hubFrame struct {
	origin *fakeLink
	f      transport.Frame
}

type fakeLink struct {
	net    *fakeNet
	serial int
	inbox  chan transport.Frame
	done   chan struct{}
	once   sync.Once
}

func newFakeNet() *fakeNet { return &fakeNet{ids: map[string]int{}} }

func (n *fakeNet) dialer() Dialer {
	return func(ctx context.Context) (transport.Link, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		l := &fakeLink{net: n, serial: len(n.links) + 1, inbox: make(chan transport.Frame, 8192), done: make(chan struct{})}
		n.links = append(n.links, l)
		return l, nil
	}
}

func (l *fakeLink) Send(f transport.Frame) error {
	select {
	case <-l.done:
		return transport.ErrLinkClosed
	default:
	}
	l.net.mu.Lock()
	defer l.net.mu.Unlock()
	if f.Kind == transport.FrameRequest {
		l.net.ids[f.RequestID]++
	}
	l.net.toHub = append(l.net.toHub, hubFrame{origin: l, f: f})
	l.net.sent++
	return nil
}

func (l *fakeLink) Recv(ctx context.Context) (transport.Frame, error) {
	select {
	case f := <-l.inbox:
		return f, nil
	case <-l.done:
		return transport.Frame{}, transport.ErrLinkClosed
	case <-ctx.Done():
		return transport.Frame{}, ctx.Err()
	}
}

func (l *fakeLink) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

// serve answers toHub[i]: the response echoes the request's payload and is
// stamped with the serial of the connection the HUB received the request on —
// information the client never wrote — then waits in limbo.
func (n *fakeNet) serve(i int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	hf := n.toHub[i]
	n.toHub = append(n.toHub[:i], n.toHub[i+1:]...)
	var body map[string]any
	_ = json.Unmarshal(hf.f.Payload, &body)
	body["conn"] = hf.origin.serial
	payload, _ := json.Marshal(body)
	n.limbo = append(n.limbo, hubFrame{origin: hf.origin, f: transport.Frame{
		Kind: transport.FrameResponse, RequestID: hf.f.RequestID, Payload: payload,
	}})
}

// deliver pushes limbo[i] to the link it came from; misroute sends it to the
// newest link instead (a transport that mixes connections up); keep leaves a
// copy in limbo (the transport delivers a frame twice).
func (n *fakeNet) deliver(i int, keep, misroute bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	hf := n.limbo[i]
	if !keep {
		n.limbo = append(n.limbo[:i], n.limbo[i+1:]...)
	}
	target := hf.origin
	if misroute {
		target = n.links[len(n.links)-1]
	}
	select {
	case <-target.done:
	default:
		select {
		case target.inbox <- hf.f:
		default:
		}
	}
}

func (n *fakeNet) counts() (toHub, limbo, links int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.toHub), len(n.limbo), len(n.links)
}

func (n *fakeNet) newest() *fakeLink {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.links[len(n.links)-1]
}

// echoParams is what each test request carries and every response echoes.
type echoParams struct {
	Serial int `json:"serial"`
	Conn   int `json:"conn"` // stamped by the fake hub: the connection that carried the request
}

// outcome of one issued request.
type outcome struct {
	serial, epoch int
	done          chan struct{}
	got           echoParams
	err           error
	completions   int
}

func newRemoteOnFake(t testing.TB, n *fakeNet) *remoteClient {
	t.Helper()
	rc := &remoteClient{cfg: RemoteConfig{Dial: n.dialer(), RPCTimeout: 3 * time.Second},
		agents: map[string]*remoteAgent{}, early: map[string][]transport.Frame{}}
	if err := rc.connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rc.close() })
	return rc
}

// issue starts a request on the current connection and waits until its frame
// is on the wire (or it failed at once), so the schedule is reproducible.
func issue(rc *remoteClient, n *fakeNet, serial int) *outcome {
	cn, _ := rc.current()
	epoch := 0
	if cn != nil {
		epoch = int(cn.epoch)
	}
	o := &outcome{serial: serial, epoch: epoch, done: make(chan struct{})}
	n.mu.Lock()
	before := n.sent
	n.mu.Unlock()
	go func() {
		defer close(o.done)
		err := rc.call(context.Background(), "echo", echoParams{Serial: serial}, &o.got)
		o.err = err
		o.completions++
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n.mu.Lock()
		sent := n.sent
		n.mu.Unlock()
		select {
		case <-o.done:
			return o
		default:
		}
		if sent > before {
			return o
		}
		time.Sleep(50 * time.Microsecond)
	}
	return o
}

func waitDone(t testing.TB, o *outcome, what string) {
	t.Helper()
	select {
	case <-o.done:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s: request %d never completed (a pending request hung)", what, o.serial)
	}
}

func waitConnDown(t testing.TB, rc *remoteClient) {
	t.Helper()
	cn, _ := rc.current()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		cn.mu.Lock()
		closed := cn.closed
		cn.mu.Unlock()
		if closed {
			return
		}
		time.Sleep(50 * time.Microsecond)
	}
	t.Fatal("client never noticed the dropped link")
}

// --- regression tests: counterexamples of the LinkRPC mutation table -------------------------------

// L1: a response is matched to its waiter by id, never by position.
func TestRemoteRPCMatchesResponsesByID(t *testing.T) {
	n := newFakeNet()
	rc := newRemoteOnFake(t, n)
	a := issue(rc, n, 1)
	b := issue(rc, n, 2)
	// Serve in order, deliver in reverse: position matching would swap them.
	n.serve(0)
	n.serve(0)
	n.deliver(1, false, false)
	n.deliver(0, false, false)
	waitDone(t, a, "a")
	waitDone(t, b, "b")
	if a.err != nil || a.got.Serial != 1 || b.err != nil || b.got.Serial != 2 {
		t.Fatalf("responses crossed: a=%+v/%v b=%+v/%v", a.got, a.err, b.got, b.err)
	}
}

// L2: request ids are unique for the whole process lifetime, not a
// per-connection counter that restarts at a reconnect; and a response of the
// old connection arriving on the new link satisfies nothing.
func TestRemoteRPCIDsAreUniqueAcrossReconnect(t *testing.T) {
	n := newFakeNet()
	rc := newRemoteOnFake(t, n)
	old := issue(rc, n, 1) // first request of connection 1
	n.serve(0)             // its answer exists, but is not delivered yet
	n.newest().Close()
	waitConnDown(t, rc)
	waitDone(t, old, "old")
	if !errors.Is(old.err, ErrConnectionLost) {
		t.Fatalf("pending request on a dropped link: %v", old.err)
	}
	if err := rc.reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	fresh := issue(rc, n, 2) // first request of connection 2
	// The stale answer shows up on the NEW link, while the new request waits.
	n.deliver(0, false, true)
	time.Sleep(20 * time.Millisecond)
	select {
	case <-fresh.done:
		t.Fatalf("a response of the old connection satisfied a new request: %+v / %v", fresh.got, fresh.err)
	default:
	}
	n.serve(0)
	n.deliver(0, false, false)
	waitDone(t, fresh, "fresh")
	if fresh.err != nil || fresh.got.Serial != 2 {
		t.Fatalf("fresh request: %+v / %v", fresh.got, fresh.err)
	}
	for id, c := range n.ids {
		if c != 1 {
			t.Fatalf("request id %s was used %d times", id, c)
		}
	}
	if len(n.ids) != 2 {
		t.Fatalf("expected two distinct request ids, got %v", n.ids)
	}
}

// L3: a request is satisfied once; a duplicated response frame is ignored.
func TestRemoteRPCDuplicateResponseIgnored(t *testing.T) {
	n := newFakeNet()
	rc := newRemoteOnFake(t, n)
	a := issue(rc, n, 1)
	n.serve(0)
	n.deliver(0, true, false) // keep a copy
	waitDone(t, a, "a")
	n.deliver(0, false, false) // the duplicate
	b := issue(rc, n, 2)
	n.serve(0)
	n.deliver(0, false, false)
	waitDone(t, b, "b")
	if a.err != nil || a.got.Serial != 1 || a.completions != 1 || b.err != nil || b.got.Serial != 2 {
		t.Fatalf("a=%+v/%v (completions %d) b=%+v/%v", a.got, a.err, a.completions, b.got, b.err)
	}
}

// A request issued on a connection that is down fails with a typed error at
// once; after Reconnect requests work again.
func TestRemoteRPCFailsTypedOnDeadLinkAndRecovers(t *testing.T) {
	n := newFakeNet()
	rc := newRemoteOnFake(t, n)
	n.newest().Close()
	waitConnDown(t, rc)
	dead := issue(rc, n, 1)
	waitDone(t, dead, "dead")
	if !errors.Is(dead.err, ErrConnectionLost) || !errors.Is(dead.err, transport.ErrLinkClosed) {
		t.Fatalf("request on a dead link: %v", dead.err)
	}
	if err := rc.reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	ok := issue(rc, n, 2)
	n.serve(0)
	n.deliver(0, false, false)
	waitDone(t, ok, "ok")
	if ok.err != nil || ok.got.Serial != 2 {
		t.Fatalf("after reconnect: %+v / %v", ok.got, ok.err)
	}
}

// A request nobody answers fails with ErrRPCTimeout instead of hanging.
func TestRemoteRPCTimeout(t *testing.T) {
	n := newFakeNet()
	rc := newRemoteOnFake(t, n)
	rc.cfg.RPCTimeout = 30 * time.Millisecond
	o := issue(rc, n, 1)
	waitDone(t, o, "silent hub")
	if !errors.Is(o.err, ErrRPCTimeout) {
		t.Fatalf("unanswered request: %v", o.err)
	}
}

// --- property test: invariant 6 -----------------------------------------------------------------------
//
// Random schedules of requests, serving, delivery (late, duplicated, to the
// wrong connection, reordered), link drops and reconnects. Checked for every
// request: it completes exactly once and never hangs; a success carries its
// OWN answer (the id matches), and one issued on connection N is never
// satisfied by a response of another connection; a failure is typed
// (ErrConnectionLost).

const (
	linkPropSequences = 2500
	linkPropBaseSeed  = 20261112000
)

func TestRemoteRPCProperty(t *testing.T) {
	if s := os.Getenv("AGO_PROP_SEED"); s != "" {
		seed, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		runLinkPropSequence(t, seed)
		return
	}
	sequences := linkPropSequences
	if v, err := strconv.Atoi(os.Getenv("AGO_PROP_SEQUENCES")); err == nil && v > 0 {
		sequences = v
	}
	var stats = map[string]int{}
	for i := 0; i < sequences; i++ {
		if !runLinkPropSequence(t, int64(linkPropBaseSeed+i), stats) {
			return
		}
	}
	t.Logf("%d sequences, seeds %d..%d; coverage %v", sequences, linkPropBaseSeed, linkPropBaseSeed+sequences-1, stats)
}

func runLinkPropSequence(t *testing.T, seed int64, statsOpt ...map[string]int) bool {
	t.Helper()
	var stats map[string]int
	if len(statsOpt) > 0 {
		stats = statsOpt[0]
	}
	bump := func(k string) {
		if stats != nil {
			stats[k]++
		}
	}
	rng := rand.New(rand.NewSource(seed))
	n := newFakeNet()
	rc := &remoteClient{cfg: RemoteConfig{Dial: n.dialer(), RPCTimeout: 3 * time.Second},
		agents: map[string]*remoteAgent{}, early: map[string][]transport.Frame{}}
	if err := rc.connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer rc.close()

	var trace []string
	fail := func(format string, args ...any) bool {
		t.Helper()
		t.Errorf("seed %d (rerun: AGO_PROP_SEED=%d go test ./network -run TestRemoteRPCProperty)\n  %s\ntrace: %v",
			seed, seed, fmt.Sprintf(format, args...), trace)
		return false
	}

	var reqs []*outcome
	drops, maxReq := 0, 4+rng.Intn(6)
	steps := 10 + rng.Intn(30)
	for i := 0; i < steps; i++ {
		toHub, limbo, _ := n.counts()
		switch x := rng.Intn(100); {
		case x < 30 && len(reqs) < maxReq:
			o := issue(rc, n, len(reqs)+1)
			reqs = append(reqs, o)
			trace = append(trace, fmt.Sprintf("issue#%d@conn%d", o.serial, o.epoch))
			bump("issue")
		case x < 55 && toHub > 0:
			k := rng.Intn(toHub)
			trace = append(trace, fmt.Sprintf("serve[%d]", k))
			n.serve(k)
		case x < 85 && limbo > 0:
			k := rng.Intn(limbo)
			keep, mis := rng.Intn(4) == 0, rng.Intn(4) == 0
			trace = append(trace, fmt.Sprintf("deliver[%d] keep=%v misroute=%v", k, keep, mis))
			if keep {
				bump("duplicate")
			}
			if mis {
				bump("misroute")
			}
			n.deliver(k, keep, mis)
		case x < 89 && drops < 2:
			cn, _ := rc.current()
			cn.mu.Lock()
			up := !cn.closed
			cn.mu.Unlock()
			if up {
				trace = append(trace, "drop")
				drops++
				bump("drop")
				n.newest().Close()
				waitConnDown(t, rc)
			}
		default:
			cn, _ := rc.current()
			cn.mu.Lock()
			down := cn.closed
			cn.mu.Unlock()
			if down {
				trace = append(trace, "reconnect")
				bump("reconnect")
				if err := rc.reconnect(context.Background()); err != nil {
					return fail("reconnect: %v", err)
				}
			}
		}
	}
	// Quiescence: reconnect if down, then serve and deliver everything.
	if cn, _ := rc.current(); cn != nil {
		cn.mu.Lock()
		down := cn.closed
		cn.mu.Unlock()
		if down {
			if err := rc.reconnect(context.Background()); err != nil {
				return fail("final reconnect: %v", err)
			}
		}
	}
	for {
		toHub, limbo, _ := n.counts()
		if toHub == 0 && limbo == 0 {
			break
		}
		if toHub > 0 {
			n.serve(0)
		} else {
			n.deliver(0, false, false)
		}
	}
	for _, o := range reqs {
		select {
		case <-o.done:
		case <-time.After(3 * time.Second):
			return fail("request %d (conn %d) never completed", o.serial, o.epoch)
		}
		switch {
		case o.completions != 1:
			return fail("request %d completed %d times", o.serial, o.completions)
		case o.err == nil && o.got.Serial != o.serial:
			return fail("request %d was answered with the response of request %d", o.serial, o.got.Serial)
		case o.err == nil && o.got.Conn != o.epoch:
			return fail("request %d of connection %d was satisfied by a response of connection %d", o.serial, o.epoch, o.got.Conn)
		case o.err != nil && !errors.Is(o.err, ErrConnectionLost):
			return fail("request %d failed with an untyped error: %v", o.serial, o.err)
		}
		if o.err == nil {
			bump("ok")
		} else {
			bump("failed")
		}
	}
	for id, c := range n.ids {
		if c != 1 {
			return fail("request id %s sent %d times", id, c)
		}
	}
	return true
}

var _ = json.Marshal

// A notify that arrives from a connection that is no longer the current one is
// rejected: the hub re-delivers what the new connection has not acknowledged,
// and a stale delivery must not reach the agent (or be acked on the new link).
func TestRemoteNotifyFromOldConnectionRejected(t *testing.T) {
	n := newFakeNet()
	rc := newRemoteOnFake(t, n)
	var mu sync.Mutex
	var got []string
	if err := rc.attach("agent-1", func(env Envelope) {
		mu.Lock()
		got = append(got, env.ID)
		mu.Unlock()
	}); err != nil {
		t.Fatal(err)
	}
	notify := func(id string) transport.Frame {
		payload, _ := json.Marshal(Envelope{ID: id, ChannelID: "ch", EventType: EnvText})
		return transport.Frame{Kind: transport.FrameNotify, RecipientID: "agent-1", ChannelID: "ch", EnvelopeID: id, Payload: payload}
	}
	oldConn, _ := rc.current()
	if err := rc.reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	newConn, _ := rc.current()
	rc.onNotify(oldConn, notify("stale"))
	rc.onNotify(newConn, notify("fresh"))
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) >= 1 })
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "fresh" {
		t.Fatalf("delivered %v, want only the notify of the current connection", got)
	}
	// Only the fresh delivery was acknowledged, and on the new link.
	n.mu.Lock()
	defer n.mu.Unlock()
	var receipts []string
	for _, hf := range n.toHub {
		if hf.f.Kind == transport.FrameReceipt {
			if hf.origin != newConn.link {
				t.Errorf("receipt for %s went out on the old link", hf.f.EnvelopeID)
			}
			receipts = append(receipts, hf.f.EnvelopeID)
		}
	}
	if len(receipts) != 1 || receipts[0] != "fresh" {
		t.Fatalf("receipts %v", receipts)
	}
}
