package network

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luckystrker/ago/knowledge"
	"github.com/luckystrker/ago/network/transport"
)

// Property test for wave-4 invariants 1–4 (spec/tla/HubCore.tla and its
// scenarios): random schedules of posts (some retried under one causation key),
// deliveries, acks, nacks, forged acks, link drops, client restarts, reconnects
// with stale or fresh since marks and hub crashes (OpenHub over the store),
// with three addressees on two channels, audiences, a non-participant and the
// creator. The links are manual: nothing moves unless the schedule moves it, so
// a seed fixes the whole interleaving.
//
// Checked: (1) after a final reconnect and quiescence every agent has received
// and acked (cursor ≥ id) every envelope addressed to it; (2) a persisted
// cursor never moves back, and every send names an id strictly above the
// persisted cursor and above the connection's since mark, in log order within
// a channel; (3) a post retried under one key is in the WAL once; (4) every
// delivery is addressed to its recipient (participant, in the audience, not the
// sender), and an ack that names an agent the connection does not hold changes
// no cursor.

const (
	deliveryPropSequences = 2500
	deliveryPropBaseSeed  = 20261113000
)

func TestDeliveryProperty(t *testing.T) {
	// every sequence opens a hub that accepts "none": the warning is tested elsewhere
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(prev)
	if s := os.Getenv("AGO_PROP_SEED"); s != "" {
		seed, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		runDeliverySequence(t, seed, nil)
		return
	}
	sequences := deliveryPropSequences
	if v, err := strconv.Atoi(os.Getenv("AGO_PROP_SEQUENCES")); err == nil && v > 0 {
		sequences = v
	}
	stats := map[string]int{}
	for i := 0; i < sequences; i++ {
		if !runDeliverySequence(t, int64(deliveryPropBaseSeed+i), stats) {
			return
		}
	}
	t.Logf("%d sequences, seeds %d..%d; coverage %v", sequences, deliveryPropBaseSeed, deliveryPropBaseSeed+sequences-1, stats)
}

// --- the manual link -----------------------------------------------------------------------------

// pconn is one connection. The hub end is a transport.Link; the client end is
// driven by the schedule: frames the client sends wait in toHub until the
// schedule delivers them, frames the hub sends wait in toClient.
type pconn struct {
	run      *deliveryRun
	client   string // agent name of the client on this connection
	inbox    chan transport.Frame
	done     chan struct{}
	closeOne sync.Once
	entered  atomic.Int64 // calls of the hub end's Recv
	pushed   atomic.Int64 // frames handed to the hub

	mu       sync.Mutex
	toHub    []transport.Frame
	toClient []transport.Frame
	dead     bool
	floor    map[string]string // channel → exclusive id floor of the walk (cursor and since at hello)
	lastSent map[string]string // channel → last id the hub sent here
	nacked   map[string]bool   // client side: channels this connection nacked
	hubNack  map[string]bool   // hub side: channels whose nack the hub has handled
}

type hubEnd struct{ c *pconn }

func (e hubEnd) Send(f transport.Frame) error {
	c := e.c
	select {
	case <-c.done:
		return transport.ErrLinkClosed
	default:
	}
	if f.Kind == transport.FrameNotify {
		c.run.checkSend(c, f)
	}
	c.mu.Lock()
	c.toClient = append(c.toClient, f)
	c.mu.Unlock()
	return nil
}

func (e hubEnd) Recv(ctx context.Context) (transport.Frame, error) {
	e.c.entered.Add(1)
	select {
	case f := <-e.c.inbox:
		return f, nil
	case <-e.c.done:
		return transport.Frame{}, transport.ErrLinkClosed
	case <-ctx.Done():
		return transport.Frame{}, ctx.Err()
	}
}

func (e hubEnd) Close() error { e.c.kill(); return nil }

func (c *pconn) kill() {
	c.closeOne.Do(func() {
		c.mu.Lock()
		c.dead = true
		c.toHub, c.toClient = nil, nil // frames in flight die with the connection
		c.mu.Unlock()
		close(c.done)
	})
}

func (c *pconn) isDead() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.dead }

// --- the run -------------------------------------------------------------------------------------

type pclient struct {
	name     string
	id       string
	high     map[string]string // channel → highest id received (the client's since)
	received map[string]bool   // ghost: every id ever received
	conn     *pconn
}

type deliveryRun struct {
	t     *testing.T
	seed  int64
	rng   *rand.Rand
	store *knowledge.MemoryStore

	hub     *Hub
	stop    context.CancelFunc
	served  sync.WaitGroup
	ctx     context.Context
	ids     map[string]string
	c1, c2  string
	clients map[string]*pclient
	conns   []*pconn

	cursors  map[string]string // "agent|channel" → last seen persisted cursor (monotonicity)
	trace    []string
	failed   bool
	stats    map[string]int
	nextPost int
}

var propAgents = []string{"z", "a", "b", "c", "n"}

func runDeliverySequence(t *testing.T, seed int64, stats map[string]int) bool {
	t.Helper()
	r := &deliveryRun{t: t, seed: seed, rng: rand.New(rand.NewSource(seed)), store: knowledge.NewMemoryStore(),
		ids: map[string]string{}, clients: map[string]*pclient{}, cursors: map[string]string{}, stats: stats}
	r.startHub()
	defer r.shutdown()
	r.setup()
	steps := 25 + r.rng.Intn(40)
	for i := 0; i < steps && !r.failed; i++ {
		r.step()
	}
	if !r.failed {
		r.finish()
	}
	return !r.failed
}

func (r *deliveryRun) bump(k string) {
	if r.stats != nil {
		r.stats[k]++
	}
}

func (r *deliveryRun) logf(format string, args ...any) {
	r.trace = append(r.trace, fmt.Sprintf(format, args...))
}

func (r *deliveryRun) fail(format string, args ...any) {
	if r.failed {
		return
	}
	r.failed = true
	r.t.Helper()
	r.t.Errorf("seed %d (rerun: AGO_PROP_SEED=%d go test ./network -run TestDeliveryProperty)\n  %s\ntrace:\n    %s",
		r.seed, r.seed, fmt.Sprintf(format, args...), strings.Join(r.trace, "\n    "))
}

// startHub opens the hub the way a process would (OpenHub over the shared
// store) and serves its listener-less links one by one (see connect).
func (r *deliveryRun) startHub() {
	hub, err := OpenHub(WithHubStore(r.store), WithHubAdapter(openAdapter{}, "1"), WithAckTimeout(2*time.Second),
		WithTTLSweepInterval(0), WithExpectationSweepInterval(0))
	if err != nil {
		r.t.Fatalf("OpenHub: %v", err)
	}
	r.hub = hub
	r.ctx, r.stop = context.WithCancel(context.Background())
}

// crashHub kills the hub process: every connection dies, the store survives.
func (r *deliveryRun) crashHub() {
	r.stop()
	for _, c := range r.conns {
		c.kill()
	}
	r.served.Wait()
	_ = r.hub.Close()
	for _, cl := range r.clients {
		cl.conn = nil
	}
	r.startHub()
}

func (r *deliveryRun) shutdown() {
	r.stop()
	for _, c := range r.conns {
		c.kill()
	}
	r.served.Wait()
	_ = r.hub.Close()
}

// setup registers the agents and opens the two channels (invitees ack in
// process); nothing is connected afterwards.
func (r *deliveryRun) setup() {
	h := r.hub
	for _, name := range propAgents {
		p, err := h.Register(Passport{Name: name}, nil)
		if err != nil {
			r.t.Fatal(err)
		}
		r.ids[name] = p.AgentID
		id := p.AgentID
		_ = h.Attach(id, func(env Envelope) {
			if env.EventType == EnvChannelInvite {
				_, _ = h.Send(NewChannelAck(env, id, false, ""))
			}
		})
		r.clients[name] = &pclient{name: name, id: id, high: map[string]string{}, received: map[string]bool{}}
	}
	c1, err := h.CreateChannel(ChannelManifest{Type: "open"}, []string{r.ids["z"], r.ids["a"], r.ids["b"], r.ids["c"]})
	if err != nil {
		r.t.Fatal(err)
	}
	c2, err := h.CreateChannel(ChannelManifest{Type: "open"}, []string{r.ids["z"], r.ids["a"]})
	if err != nil {
		r.t.Fatal(err)
	}
	r.c1, r.c2 = c1.ID, c2.ID
}

func (r *deliveryRun) channels() []string { return []string{r.c1, r.c2} }

// persisted cursor of an agent in a channel, as the hub holds it now.
func (r *deliveryRun) cursor(name, ch string) string { return r.hub.InboxCursor(r.ids[name], ch) }

// checkCursors: a persisted cursor never moves back (also across hub restarts).
func (r *deliveryRun) checkCursors() {
	for _, name := range propAgents {
		for _, ch := range r.channels() {
			key := name + "|" + ch
			now := r.cursor(name, ch)
			if now < r.cursors[key] {
				r.fail("cursor of %s in %s moved back from %s to %s", name, ch[:6], r.cursors[key], now)
				return
			}
			r.cursors[key] = now
		}
	}
}

// --- hub-side check of every send ----------------------------------------------------------------

func (r *deliveryRun) checkSend(c *pconn, f transport.Frame) {
	var env Envelope
	if err := json.Unmarshal(f.Payload, &env); err != nil {
		r.fail("undecodable notify: %v", err)
		return
	}
	name := c.client
	if f.RecipientID != r.ids[name] {
		r.fail("notify for %s sent on the connection of %s", f.RecipientID, name)
		return
	}
	ch := env.ChannelID
	cur := r.hub.InboxCursor(f.RecipientID, ch)
	if env.ID <= cur {
		r.fail("%s: envelope %s sent although the persisted cursor is %s (delivery after the ack)", name, env.ID[:8], cur[:min(8, len(cur))])
	}
	c.mu.Lock()
	floor, last := c.floor[ch], c.lastSent[ch]
	c.lastSent[ch] = env.ID
	c.mu.Unlock()
	if env.ID <= floor {
		r.fail("%s: envelope %s sent although the walk started strictly after %s", name, env.ID[:8], floor[:min(8, len(floor))])
	}
	c.mu.Lock()
	afterNack := c.hubNack[ch]
	c.mu.Unlock()
	if afterNack {
		r.fail("%s: envelope %s sent on channel %s after the hub handled its nack (design requirement 6)", name, env.ID[:8], ch[:6])
	}
	if env.ID <= last {
		r.fail("%s: envelope %s sent after %s in channel %s (not in log order)", name, env.ID[:8], last[:8], ch[:6])
	}
}

// --- schedule steps ------------------------------------------------------------------------------

func (r *deliveryRun) step() {
	switch x := r.rng.Intn(100); {
	case x < 18:
		r.post()
	case x < 38:
		r.deliverToClient(r.pickLive(), false)
	case x < 70:
		r.deliverToHub(r.pickLive())
	case x < 80:
		r.connect(r.pickClient())
	case x < 84:
		r.drop(r.pickLive())
	case x < 88:
		r.restartClient(r.pickLive())
	case x < 93:
		r.deliverToClient(r.pickLive(), true) // nack instead of ack
	case x < 96:
		r.forge()
	case x < 98:
		r.logf("hub crash")
		r.bump("hub crash")
		r.crashHub()
	}
	r.checkCursors()
}

// pickLive picks a client that has a live connection (nil when none has).
func (r *deliveryRun) pickLive() *pclient {
	var live []*pclient
	for _, name := range propAgents {
		if cl := r.clients[name]; cl.conn != nil && !cl.conn.isDead() {
			live = append(live, cl)
		}
	}
	if len(live) == 0 {
		return nil
	}
	return live[r.rng.Intn(len(live))]
}

func (r *deliveryRun) pickClient() *pclient {
	return r.clients[propAgents[r.rng.Intn(len(propAgents))]]
}

// post sends a text from a participant: audience and causation key vary, a key
// may repeat (a retry).
func (r *deliveryRun) post() {
	ch := r.channels()[r.rng.Intn(2)]
	senders := []string{"z", "a", "b", "c"}
	if ch == r.c2 {
		senders = []string{"z", "a"}
	}
	sender := senders[r.rng.Intn(len(senders))]
	audience := ""
	if r.rng.Intn(3) == 0 {
		audience = senders[r.rng.Intn(len(senders))]
		if audience == sender {
			audience = ""
		}
	}
	cause := ""
	if r.rng.Intn(2) == 0 {
		cause = []string{"p1", "p2"}[r.rng.Intn(2)]
	}
	r.nextPost++
	env := NewTextEnvelope(ch, r.ids[sender], r.ids[audience], fmt.Sprint("m", r.nextPost))
	env.CausationID = cause
	r.logf("post ch=%s sender=%s aud=%q cause=%q", ch[:4], sender, audience, cause)
	if _, err := r.hub.Send(env); err != nil {
		r.fail("post failed: %v", err)
	}
	r.bump("post")
	if cause != "" {
		r.bump("keyed post")
	}
}

// connect opens a connection for a client without one: its hello carries the
// client's since marks — current, stale or omitted — and the schedule delivers
// the hello like any other frame.
func (r *deliveryRun) connect(cl *pclient) {
	if cl.conn != nil && !cl.conn.isDead() {
		return
	}
	c := &pconn{run: r, client: cl.name, inbox: make(chan transport.Frame, 1024), done: make(chan struct{}),
		floor: map[string]string{}, lastSent: map[string]string{}, nacked: map[string]bool{}, hubNack: map[string]bool{}}
	cl.conn = c
	r.conns = append(r.conns, c)
	r.served.Add(1)
	go func() {
		defer r.served.Done()
		_ = r.hub.ServeLink(r.ctx, hubEnd{c})
	}()
	body := HelloBody{Name: cl.name}
	mode := r.rng.Intn(10)
	switch {
	case mode < 6: // fresh marks
		body.Since = copyMarks(cl.high)
	case mode < 8: // stale marks: lower than what the client has
		body.Since = map[string]string{}
		for ch, id := range cl.high {
			if r.rng.Intn(2) == 0 {
				body.Since[ch] = id
			}
		}
	case mode < 9: // the legacy single value, valid for every channel
		low := ""
		for _, id := range cl.high {
			if low == "" || id < low {
				low = id
			}
		}
		if len(cl.high) < len(r.channels()) {
			low = "" // a channel without a mark: only "" is safe for it
		}
		body.SinceEnvelopeID = &low
	}
	payload, _ := json.Marshal(body)
	r.logf("connect %s since=%v legacy=%v", cl.name, body.Since, body.SinceEnvelopeID != nil)
	r.bump("connect")
	c.mu.Lock()
	c.toHub = append(c.toHub, transport.Frame{Kind: transport.FrameHello, RequestID: "hello", Payload: payload})
	c.mu.Unlock()
}

func copyMarks(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (r *deliveryRun) drop(cl *pclient) {
	if cl == nil {
		return
	}
	if cl.conn == nil || cl.conn.isDead() {
		return
	}
	r.logf("drop %s", cl.name)
	r.bump("drop")
	cl.conn.kill()
	cl.conn = nil
}

func (r *deliveryRun) restartClient(cl *pclient) {
	if cl == nil {
		return
	}
	r.logf("client restart %s", cl.name)
	r.bump("client restart")
	r.drop(cl)
	cl.high = map[string]string{} // the client's own memory is gone
}

// deliverToClient hands the oldest frame the hub sent to the client; the
// client acks it (queues the receipt) or, with nack, rejects it.
func (r *deliveryRun) deliverToClient(cl *pclient, nack bool) {
	if cl == nil {
		return
	}
	c := cl.conn
	if c == nil || c.isDead() {
		return
	}
	c.mu.Lock()
	if len(c.toClient) == 0 {
		c.mu.Unlock()
		return
	}
	f := c.toClient[0]
	c.toClient = c.toClient[1:]
	c.mu.Unlock()
	switch f.Kind {
	case transport.FrameWelcome:
		// re-ack the high-water marks (model D13)
		for ch, id := range cl.high {
			r.queueReceipt(c, cl, ch, id, ReceiptAck)
		}
	case transport.FrameNotify:
		r.clientReceives(c, cl, f, nack)
	}
}

func (r *deliveryRun) clientReceives(c *pconn, cl *pclient, f transport.Frame, nack bool) {
	var env Envelope
	if err := json.Unmarshal(f.Payload, &env); err != nil {
		r.fail("undecodable notify at the client: %v", err)
		return
	}
	// (4) the delivery is addressed to this client.
	if !permittedFor(r.hub, env, cl.id) {
		r.fail("%s received %s (%s, audience %q, sender %s) which is not addressed to it", cl.name, env.ID[:8], env.EventType, env.Audience, env.SenderID[:6])
		return
	}
	c.mu.Lock()
	stalled := c.nacked[env.ChannelID]
	if nack && !stalled {
		c.nacked[env.ChannelID] = true
	}
	c.mu.Unlock()
	switch {
	case stalled:
		r.bump("dropped after nack")
		return // a nacked channel: nothing more is delivered or acked on this connection
	case nack:
		r.logf("%s nacks %s", cl.name, env.ID[:8])
		r.bump("nack")
		r.queueReceipt(c, cl, env.ChannelID, env.ID, ReceiptNack)
		return
	case env.ID <= cl.high[env.ChannelID]:
		r.bump("duplicate")
	default:
		cl.high[env.ChannelID] = env.ID
	}
	cl.received[env.ID] = true
	r.bump("delivered")
	r.queueReceipt(c, cl, env.ChannelID, env.ID, ReceiptAck)
}

func (r *deliveryRun) queueReceipt(c *pconn, cl *pclient, ch, id, code string) {
	c.mu.Lock()
	c.toHub = append(c.toHub, transport.Frame{Kind: transport.FrameReceipt, RecipientID: cl.id, ChannelID: ch, EnvelopeID: id, Code: code})
	c.mu.Unlock()
}

// forge: a connection sends a receipt that names another agent.
func (r *deliveryRun) forge() {
	cl := r.pickLive()
	if cl == nil {
		return
	}
	c := cl.conn
	if c == nil || c.isDead() {
		return
	}
	victim := r.clients[propAgents[r.rng.Intn(len(propAgents))]]
	if victim == cl {
		return
	}
	log, _ := r.hub.ReadWAL(r.channels()[r.rng.Intn(2)])
	if len(log) == 0 {
		return
	}
	e := log[r.rng.Intn(len(log))]
	r.logf("forged ack by %s for %s (%s)", cl.name, victim.name, e.ID[:8])
	r.bump("forged ack")
	// delivered at once so the "unchanged" check is exact
	before := r.snapshotCursors()
	r.pushToHub(c, transport.Frame{Kind: transport.FrameReceipt, RecipientID: victim.id, ChannelID: e.ChannelID, EnvelopeID: e.ID, Code: ReceiptAck})
	if after := r.snapshotCursors(); after != before {
		r.fail("an ack of %s's connection naming %s changed a cursor:\n  before %s\n  after  %s", cl.name, victim.name, before, after)
	}
}

func (r *deliveryRun) snapshotCursors() string {
	var parts []string
	for _, name := range propAgents {
		for _, ch := range r.channels() {
			parts = append(parts, name+":"+r.cursor(name, ch))
		}
	}
	return strings.Join(parts, ",")
}

// deliverToHub hands one pending client frame — any of them: handlers may run
// in any order — to the hub and waits until the hub has handled it.
func (r *deliveryRun) deliverToHub(cl *pclient) {
	if cl == nil {
		return
	}
	c := cl.conn
	if c == nil || c.isDead() {
		return
	}
	c.mu.Lock()
	if len(c.toHub) == 0 {
		c.mu.Unlock()
		return
	}
	i := r.rng.Intn(len(c.toHub))
	f := c.toHub[i]
	c.toHub = append(c.toHub[:i], c.toHub[i+1:]...)
	c.mu.Unlock()
	if f.Kind == transport.FrameHello {
		r.captureFloor(c, cl, f)
	}
	r.bump("frame to hub: " + f.Kind)
	r.pushToHub(c, f)
	r.noteHubNack(c, f)
}

// captureFloor records where the walk of every channel must start: strictly
// after max(persisted cursor, the hello's mark) — computed here from the
// frame, independently of the hub.
func (r *deliveryRun) captureFloor(c *pconn, cl *pclient, f transport.Frame) {
	var body HelloBody
	_ = json.Unmarshal(f.Payload, &body)
	marks := body.SinceMarks()
	c.mu.Lock()
	for _, ch := range r.channels() {
		floor := r.cursor(cl.name, ch)
		if v, ok := marks.Lookup(ch); ok && v > floor {
			floor = v
		}
		c.floor[ch] = floor
	}
	c.mu.Unlock()
}

// noteHubNack records that the hub has handled a nack: it must not send that
// channel on this connection any more.
func (r *deliveryRun) noteHubNack(c *pconn, f transport.Frame) {
	if f.Kind == transport.FrameReceipt && f.Code == ReceiptNack {
		c.mu.Lock()
		c.hubNack[f.ChannelID] = true
		c.mu.Unlock()
	}
}

func (r *deliveryRun) pushToHub(c *pconn, f transport.Frame) {
	// The hub's loop calls Recv once before the first frame and once more after
	// handling each frame, so frame k is handled when Recv has been entered k+1 times.
	pushed := c.pushed.Add(1)
	select {
	case c.inbox <- f:
	case <-c.done:
		return
	}
	deadline := time.Now().Add(3 * time.Second)
	for spin := 0; c.entered.Load() < pushed+1; spin++ {
		if c.isDead() {
			return
		}
		if spin < 2000 {
			runtime.Gosched()
			continue
		}
		if time.Now().After(deadline) {
			r.fail("the hub never finished handling a %s frame", f.Kind)
			return
		}
		time.Sleep(20 * time.Microsecond)
	}
}

// finish: reconnect every client with its true marks (clearing every nack
// stall), run the links to quiescence and check eventual delivery.
func (r *deliveryRun) finish() {
	r.logf("-- final reconnect and quiescence")
	for _, name := range propAgents {
		cl := r.clients[name]
		r.drop(cl)
		r.connectFresh(cl)
	}
	for round := 0; round < 200; round++ {
		moved := false
		for _, name := range propAgents {
			cl := r.clients[name]
			c := cl.conn
			if c == nil || c.isDead() {
				continue
			}
			for {
				c.mu.Lock()
				n := len(c.toClient)
				c.mu.Unlock()
				if n == 0 {
					break
				}
				r.deliverToClient(cl, false)
				moved = true
			}
			for {
				c.mu.Lock()
				n := len(c.toHub)
				var f transport.Frame
				if n > 0 {
					f = c.toHub[0]
					c.toHub = c.toHub[1:]
				}
				c.mu.Unlock()
				if n == 0 {
					break
				}
				if f.Kind == transport.FrameHello {
					r.captureFloor(c, cl, f)
				}
				r.pushToHub(c, f)
				r.noteHubNack(c, f)
				moved = true
			}
		}
		if !moved || r.failed {
			break
		}
	}
	r.checkCursors()
	if r.failed {
		return
	}
	r.checkEventualDelivery()
	r.checkKeysOnce()
}

// connectFresh is connect with the client's real, current marks.
func (r *deliveryRun) connectFresh(cl *pclient) {
	c := &pconn{run: r, client: cl.name, inbox: make(chan transport.Frame, 1024), done: make(chan struct{}),
		floor: map[string]string{}, lastSent: map[string]string{}, nacked: map[string]bool{}, hubNack: map[string]bool{}}
	cl.conn = c
	r.conns = append(r.conns, c)
	r.served.Add(1)
	go func() {
		defer r.served.Done()
		_ = r.hub.ServeLink(r.ctx, hubEnd{c})
	}()
	payload, _ := json.Marshal(HelloBody{Name: cl.name, Since: copyMarks(cl.high)})
	c.mu.Lock()
	c.toHub = append(c.toHub, transport.Frame{Kind: transport.FrameHello, RequestID: "hello", Payload: payload})
	c.mu.Unlock()
}

// (1) every agent has received and acked every envelope addressed to it.
func (r *deliveryRun) checkEventualDelivery() {
	for _, name := range propAgents {
		cl := r.clients[name]
		for _, ch := range r.channels() {
			log, _ := r.hub.ReadWAL(ch)
			for _, e := range log {
				if !eligibleFor(r.hub, e, cl.id) {
					continue
				}
				if !cl.received[e.ID] {
					r.fail("%s never received %s (%s) of channel %s", name, e.ID[:8], e.EventType, ch[:6])
					return
				}
				if cur := r.cursor(name, ch); cur < e.ID {
					r.fail("%s received %s but its persisted cursor is only %q", name, e.ID[:8], cur)
					return
				}
			}
		}
	}
	for _, name := range propAgents {
		for id := range r.clients[name].received {
			ok := false
			for _, ch := range r.channels() {
				log, _ := r.hub.ReadWAL(ch)
				for _, e := range log {
					if e.ID == id {
						ok = permittedFor(r.hub, e, r.ids[name])
					}
				}
			}
			if !ok {
				r.fail("%s received %s which is not addressed to it", name, id[:8])
				return
			}
		}
	}
}

// (3) a post retried under one causation key was accepted once.
func (r *deliveryRun) checkKeysOnce() {
	for _, ch := range r.channels() {
		log, _ := r.hub.ReadWAL(ch)
		seen := map[string]int{}
		for _, e := range log {
			if e.CausationID != "" && e.EventType == EnvText {
				seen[e.SenderID+"|"+e.CausationID]++
			}
		}
		for k, n := range seen {
			if n != 1 {
				keys := make([]string, 0)
				for kk := range seen {
					keys = append(keys, kk)
				}
				sort.Strings(keys)
				r.fail("key %s accepted %d times in %s", k, n, ch[:6])
				return
			}
		}
	}
}
