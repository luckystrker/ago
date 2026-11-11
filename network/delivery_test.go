package network

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luckystrker/ago/network/transport"
)

// Store-and-forward delivery (wave-4 invariants 1–4): regression tests that
// replay the counterexample traces of the model's mutation table
// (docs/acceptance/wave4-invariants.md) one by one.

// openAdapter is a free-form channel for any number of participants (the
// built-in adapters either limit the count or take turns), so one channel can
// have several addressees.
type openAdapter struct{ ConversationAdapter }

func (openAdapter) Type() string                         { return "open" }
func (openAdapter) ValidateCreate(ChannelManifest) error { return nil }

// dlvRig: a hub with a poster z and the addressees a, b, c plus an outsider n,
// two channels — c1 {z,a,b,c} and c2 {z,a} — and nothing connected yet.
type dlvRig struct {
	*wireRig
	ids map[string]string // name → agent id
	c1  string
	c2  string
}

func newDlvRig(t *testing.T, opts ...HubOption) *dlvRig {
	t.Helper()
	rig := newWireRig(t, opts...)
	rig.hub.RegisterAdapter(openAdapter{}, "1")
	d := &dlvRig{wireRig: rig, ids: map[string]string{}}
	for _, name := range []string{"z", "a", "b", "c", "n"} {
		p, err := rig.hub.Register(Passport{Name: name}, nil)
		if err != nil {
			t.Fatal(err)
		}
		d.ids[name] = p.AgentID
		id := p.AgentID
		_ = rig.hub.Attach(id, func(env Envelope) { // in-process: acks invites until a wire client binds the agent
			if env.EventType == EnvChannelInvite {
				_, _ = rig.hub.Send(NewChannelAck(env, id, false, ""))
			}
		})
	}
	c1, err := rig.hub.CreateChannel(ChannelManifest{Type: "open"}, []string{d.ids["z"], d.ids["a"], d.ids["b"], d.ids["c"]})
	if err != nil {
		t.Fatal(err)
	}
	c2, err := rig.hub.CreateChannel(ChannelManifest{Type: "open"}, []string{d.ids["z"], d.ids["a"]})
	if err != nil {
		t.Fatal(err)
	}
	d.c1, d.c2 = c1.ID, c2.ID
	return d
}

// post sends a text from z into ch (audience "" = everyone) and returns its id.
func (d *dlvRig) post(ch, audience, text string) string {
	d.t.Helper()
	env := NewTextEnvelope(ch, d.ids["z"], d.ids[audience], text)
	got, err := d.hub.Send(env)
	if err != nil {
		d.t.Fatalf("post %q: %v", text, err)
	}
	return got.ID
}

// expected lists, per channel, the ids of the envelopes the hub's log holds
// that are addressed to the agent: the spec's Eligible(a, k), written out here
// independently of the hub's filter.
func (d *dlvRig) expected(name string, channels ...string) []string {
	var out []string
	for _, ch := range channels {
		log, _ := d.hub.ReadWAL(ch)
		for _, e := range log {
			if eligibleFor(d.hub, e, d.ids[name]) {
				out = append(out, e.ID)
			}
		}
	}
	return out
}

func eligibleFor(h *Hub, e Envelope, agent string) bool {
	ch, err := h.Channel(e.ChannelID)
	if err != nil || !ch.hasParticipant(agent) {
		return false
	}
	switch e.EventType {
	case EnvChannelAck, EnvChannelReject:
		return false
	}
	if e.Audience != "" && e.Audience != agent {
		return false
	}
	return e.SenderID != agent // nobody is owed her own envelope
}

// permittedFor is eligibleFor plus what live dispatch may additionally send:
// a lifecycle broadcast (opened/closed/expired) to its own sender, the creator.
func permittedFor(h *Hub, e Envelope, agent string) bool {
	if eligibleFor(h, e, agent) {
		return true
	}
	ch, err := h.Channel(e.ChannelID)
	return err == nil && ch.hasParticipant(agent) && e.SenderID == agent && isLifecycleEvent(e.EventType)
}

// scripted is a hand-driven client of one agent: it reads notifies in a
// goroutine, records them in arrival order and acks only when told to.
type scripted struct {
	t    *testing.T
	link transport.Link
	mu   sync.Mutex
	got  []Envelope
	live bool
}

func (d *dlvRig) client() *scripted {
	d.t.Helper()
	l, err := d.ln.Dial()
	if err != nil {
		d.t.Fatal(err)
	}
	s := &scripted{t: d.t, link: l, live: true}
	d.t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			f, err := l.Recv(context.Background())
			if err != nil {
				return
			}
			if f.Kind != transport.FrameNotify {
				continue
			}
			var env Envelope
			if json.Unmarshal(f.Payload, &env) != nil {
				continue
			}
			s.mu.Lock()
			s.got = append(s.got, env)
			s.mu.Unlock()
		}
	}()
	return s
}

// hello binds the connection to name; since maps channel → mark (nil: none).
func (s *scripted) hello(name string, since map[string]string, legacy *string) {
	s.t.Helper()
	body, _ := json.Marshal(HelloBody{Name: name, Since: since, SinceEnvelopeID: legacy})
	if err := s.link.Send(transport.Frame{Kind: transport.FrameHello, RequestID: "h-" + name, Payload: body}); err != nil {
		s.t.Fatal(err)
	}
}

func (s *scripted) receipt(agent, ch, env, code string) {
	_ = s.link.Send(transport.Frame{Kind: transport.FrameReceipt, RecipientID: agent, ChannelID: ch, EnvelopeID: env, Code: code, Message: "test"})
}

func (s *scripted) ids() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.got))
	for i, e := range s.got {
		out[i] = e.ID
	}
	return out
}

func (s *scripted) waitCount(n int) {
	s.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(s.ids()) >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	s.t.Fatalf("got %d envelopes, want %d: %v", len(s.ids()), n, s.ids())
}

// settle gives the hub time to process what was sent (frames of one connection
// are processed in order; this is for "nothing arrives" checks).
func settle() { time.Sleep(60 * time.Millisecond) }

// perChannel splits ids by channel, keeping order.
func (d *dlvRig) perChannel(s *scripted) map[string][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]string{}
	for _, e := range s.got {
		out[e.ChannelID] = append(out[e.ChannelID], e.ID)
	}
	return out
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func (d *dlvRig) ackAll(s *scripted, name string) {
	s.mu.Lock()
	envs := append([]Envelope(nil), s.got...)
	s.mu.Unlock()
	for _, e := range envs {
		s.receipt(d.ids[name], e.ChannelID, e.ID, ReceiptAck)
	}
}

func (d *dlvRig) waitCursor(name, ch, want string) {
	d.t.Helper()
	waitFor(d.t, func() bool { return d.hub.InboxCursor(d.ids[name], ch) == want })
}

// --- store and forward ---------------------------------------------------------------------------

// Invariant 1: what arrives while the addressee is offline is delivered at
// attach — everything addressed to it, in log order per channel, once.
func TestOfflineEnvelopesAreDeliveredAtAttachInOrder(t *testing.T) {
	d := newDlvRig(t)
	for i := 0; i < 4; i++ {
		d.post(d.c1, "", fmt.Sprintf("c1-%d", i))
		d.post(d.c2, "", fmt.Sprintf("c2-%d", i))
	}
	d.post(d.c1, "b", "only for b") // audience: a must not get it
	a := d.client()
	a.hello("a", nil, nil)
	want := d.expected("a", d.c1, d.c2)
	a.waitCount(len(want))
	settle()
	got := d.perChannel(a)
	wantBy := map[string][]string{}
	for _, ch := range []string{d.c1, d.c2} {
		wantBy[ch] = d.expected("a", ch)
	}
	for _, ch := range []string{d.c1, d.c2} {
		if strings.Join(got[ch], ",") != strings.Join(wantBy[ch], ",") {
			t.Fatalf("channel %s: got %v want %v", ch[:6], got[ch], wantBy[ch])
		}
	}
	if len(a.ids()) != len(want) {
		t.Fatalf("%d envelopes, want exactly %d (duplicates or extras)", len(a.ids()), len(want))
	}
}

// D1: the replay starts strictly after the persisted cursor, not at it.
func TestReplayStartsStrictlyAfterTheCursor(t *testing.T) {
	d := newDlvRig(t)
	e1, e2, e3 := d.post(d.c1, "", "1"), d.post(d.c1, "", "2"), d.post(d.c1, "", "3")
	_ = e1
	if err := d.hub.AdvanceInboxCursor(d.ids["a"], d.c1, e2); err != nil {
		t.Fatal(err)
	}
	a := d.client()
	a.hello("a", nil, nil)
	waitFor(t, func() bool { return len(a.perChannel(d.c1)) >= 1 })
	settle()
	if got := a.perChannel(d.c1); len(got) != 1 || got[0] != e3 {
		t.Fatalf("replay after cursor %s: got %v, want only %s", e2[:6], got, e3[:6])
	}
}

func (s *scripted) perChannel(ch string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.got {
		if e.ChannelID == ch {
			out = append(out, e.ID)
		}
	}
	return out
}

// D14: the client's since mark counts, not only the cursor; D8: the mark is
// per channel — a high mark in c1 must not hide an envelope of c2.
func TestReplayHonoursPerChannelSinceMarks(t *testing.T) {
	d := newDlvRig(t)
	p1, p2 := d.post(d.c1, "", "x1"), d.post(d.c1, "", "x2")
	q1, q2 := d.post(d.c2, "", "y1"), d.post(d.c2, "", "y2")
	_ = p1
	a := d.client()
	// c1: the client has everything up to p2; c2: only "since nothing" (absent)
	a.hello("a", map[string]string{d.c1: p2}, nil)
	a.waitCount(len(d.expected("a", d.c2)) + 0)
	settle()
	if got := a.perChannel(d.c1); len(got) != 0 {
		t.Fatalf("since mark of c1 ignored: replayed %v", got)
	}
	if got := a.perChannel(d.c2); len(got) < 2 || got[len(got)-2] != q1 || got[len(got)-1] != q2 {
		t.Fatalf("c2 must be replayed in full although c1 has a high mark: %v", got)
	}
	// Legacy single value: applies to every channel the map does not name.
	b := d.client()
	legacy := q1
	b.hello("a", map[string]string{d.c1: ""}, &legacy)
	waitFor(t, func() bool { return len(b.perChannel(d.c1)) > 0 })
	settle()
	if got := b.perChannel(d.c2); len(got) != 1 || got[0] != q2 {
		t.Fatalf("legacy since_envelope_id=%s for c2: got %v", q1[:6], got)
	}
	if got := b.perChannel(d.c1); len(got) < 2 {
		t.Fatalf("the explicit empty mark of c1 replays everything: %v", got)
	}
}

// 2c: an ack in one channel never suppresses the replay of another.
func TestAckInOneChannelLeavesAnotherReplayed(t *testing.T) {
	d := newDlvRig(t)
	e1 := d.post(d.c1, "", "c1")
	d.post(d.c2, "", "c2")
	a := d.client()
	a.hello("a", nil, nil)
	a.waitCount(len(d.expected("a", d.c1, d.c2)))
	a.receipt(d.ids["a"], d.c1, e1, ReceiptAck) // ack only the c1 envelope
	d.waitCursor("a", d.c1, e1)
	if got := d.hub.InboxCursor(d.ids["a"], d.c2); got != "" {
		t.Fatalf("an ack in c1 moved the cursor of c2: %q", got)
	}
	_ = a.link.Close()
	b := d.client()
	b.hello("a", nil, nil)
	waitFor(t, func() bool { return len(b.perChannel(d.c2)) > 0 })
	settle()
	if got := b.perChannel(d.c1); len(got) != 0 {
		t.Fatalf("c1 envelopes acked up to %s were replayed: %v", e1[:6], got)
	}
	if got := b.perChannel(d.c2); len(got) != len(d.expected("a", d.c2)) {
		t.Fatalf("c2 must be replayed in full: %v", got)
	}
}

// D15: live dispatch and replay are one ordered sender — a live envelope that
// arrives while the backlog is pending is queued behind it, never ahead.
func TestLiveEnvelopeDoesNotOvertakeReplay(t *testing.T) {
	d := newDlvRig(t)
	old := []string{d.post(d.c1, "", "old1"), d.post(d.c1, "", "old2")}
	a := d.client()
	// bind the agent without running the replay: the window in which a live post can race it
	ws := &d.hub.ext.wire
	ws.mu.Lock()
	var ep *wireEndpoint
	for _, e := range ws.endpoints {
		ep = e
	}
	ws.mu.Unlock()
	if ep == nil {
		waitFor(t, func() bool { ws.mu.Lock(); defer ws.mu.Unlock(); return len(ws.endpoints) > 0 })
		ws.mu.Lock()
		for _, e := range ws.endpoints {
			ep = e
		}
		ws.mu.Unlock()
	}
	if err := d.hub.bindEndpoint(ep, d.ids["a"], HelloSince{}); err != nil {
		t.Fatal(err)
	}
	live := d.post(d.c1, "", "live") // triggers the sender before any replay ran
	want := append([]string(nil), d.expected("a", d.c1)...)
	a.waitCount(len(want))
	settle()
	got := a.perChannel(d.c1)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("delivery order %v, want log order %v (old %v, live %s)", got, want, old, live[:6])
	}
	if !sort.StringsAreSorted(got) {
		t.Fatalf("not in id order: %v", got)
	}
}

// D19 / design requirement 6: after a nack the channel stops being delivered
// on this connection until it re-binds, so a later ack cannot move the cursor
// past the nacked envelope.
func TestNackStopsTheChannelUntilReconnect(t *testing.T) {
	d := newDlvRig(t)
	a := d.client()
	a.hello("a", nil, nil)
	a.waitCount(len(d.expected("a", d.c1, d.c2)))
	e1 := d.post(d.c1, "", "one")
	a.waitCount(len(d.expected("a", d.c1, d.c2)))
	a.receipt(d.ids["a"], d.c1, e1, ReceiptNack)
	waitFor(t, func() bool { return len(d.hub.Nacks(d.ids["a"])) == 1 })
	e2 := d.post(d.c1, "", "two") // must not be sent on the stalled channel
	f1 := d.post(d.c2, "", "other channel")
	waitFor(t, func() bool { return contains(a.perChannel(d.c2), f1) })
	settle()
	if contains(a.perChannel(d.c1), e2) {
		t.Fatal("a nacked channel kept receiving on the same connection")
	}
	if got := d.hub.InboxCursor(d.ids["a"], d.c1); got >= e1 {
		t.Fatalf("cursor %s reached the nacked envelope %s", got, e1)
	}
	// Reconnect: the replay starts at the cursor, so the nacked envelope comes back, then the rest.
	_ = a.link.Close()
	b := d.client()
	b.hello("a", nil, nil)
	waitFor(t, func() bool { return contains(b.perChannel(d.c1), e2) })
	got := b.perChannel(d.c1)
	i1, i2 := indexOf(got, e1), indexOf(got, e2)
	if i1 < 0 || i2 < 0 || i1 > i2 {
		t.Fatalf("after the reconnect the nacked envelope must come back before the newer one: %v", got)
	}
}

func contains(list []string, s string) bool { return indexOf(list, s) >= 0 }
func indexOf(list []string, s string) int {
	for i, x := range list {
		if x == s {
			return i
		}
	}
	return -1
}

// D5 / D12: an ack from a connection that does not hold the agent — a forged
// one, or one still coming from the connection the agent has just left — moves
// no cursor.
func TestAckFromAConnectionThatDoesNotHoldTheAgentIsIgnored(t *testing.T) {
	d := newDlvRig(t)
	e1 := d.post(d.c1, "", "x")
	old := d.client()
	old.hello("a", nil, nil)
	old.waitCount(len(d.expected("a", d.c1, d.c2)))
	fresh := d.client()
	fresh.hello("a", nil, nil) // the agent moves to the new connection
	fresh.waitCount(len(d.expected("a", d.c1, d.c2)))
	old.receipt(d.ids["a"], d.c1, e1, ReceiptAck) // late ack of the replaced connection
	forger := d.client()
	forger.hello("b", nil, nil)
	forger.receipt(d.ids["a"], d.c1, e1, ReceiptAck) // b's connection names a
	settle()
	if got := d.hub.InboxCursor(d.ids["a"], d.c1); got != "" {
		t.Fatalf("a foreign or stale ack moved a's cursor to %s", got)
	}
	fresh.receipt(d.ids["a"], d.c1, e1, ReceiptAck)
	d.waitCursor("a", d.c1, e1)
}

// D9–D11: nothing reaches a non-audience member, a non-participant or the sender.
func TestDeliveryRespectsAudienceMembershipAndSender(t *testing.T) {
	d := newDlvRig(t)
	toB := d.post(d.c1, "b", "for b only")
	toA := d.post(d.c2, "a", "for a only")
	all := d.post(d.c1, "", "for all")
	ca, cb, cc, cn, cz := d.client(), d.client(), d.client(), d.client(), d.client()
	for name, c := range map[string]*scripted{"a": ca, "b": cb, "c": cc, "n": cn, "z": cz} {
		c.hello(name, nil, nil)
	}
	for name, c := range map[string]*scripted{"a": ca, "b": cb, "c": cc, "z": cz} {
		c.waitCount(len(d.expected(name, d.c1, d.c2)))
	}
	settle()
	if contains(ca.ids(), toB) || contains(cc.ids(), toB) || !contains(cb.ids(), toB) {
		t.Fatalf("audience b: a=%v b=%v c=%v", contains(ca.ids(), toB), contains(cb.ids(), toB), contains(cc.ids(), toB))
	}
	if !contains(ca.ids(), toA) || contains(cb.ids(), toA) || contains(cc.ids(), toA) {
		t.Fatal("c2 envelope addressed to a leaked to a non-participant or non-audience member")
	}
	if len(cn.ids()) != 0 {
		t.Fatalf("a non-participant received %v", cn.ids())
	}
	for _, id := range cz.ids() {
		for _, e := range cz.got {
			if e.ID == id && e.SenderID == d.ids["z"] && e.EventType == EnvText {
				t.Fatalf("the sender received her own text %s", id[:6])
			}
		}
	}
	_ = all
}

// X1 / D16: cursors are per agent: one addressee's ack never suppresses the replay for another.
func TestCursorsArePerAgent(t *testing.T) {
	d := newDlvRig(t)
	e1 := d.post(d.c1, "", "x")
	ca := d.client()
	ca.hello("a", nil, nil)
	ca.waitCount(len(d.expected("a", d.c1, d.c2)))
	ca.receipt(d.ids["a"], d.c1, e1, ReceiptAck)
	d.waitCursor("a", d.c1, e1)
	cb := d.client()
	cb.hello("b", nil, nil)
	cb.waitCount(len(d.expected("b", d.c1)))
	if !contains(cb.ids(), e1) {
		t.Fatalf("b's replay was suppressed by a's ack: %v", cb.ids())
	}
	if got := d.hub.InboxCursor(d.ids["b"], d.c1); got != "" {
		t.Fatalf("a's ack set b's cursor: %s", got)
	}
}

// X2: a drop of one addressee's link leaves the other addressee's delivery alone.
func TestDropOfOneLinkDoesNotDisturbAnother(t *testing.T) {
	d := newDlvRig(t)
	ca, cb := d.client(), d.client()
	ca.hello("a", nil, nil)
	cb.hello("b", nil, nil)
	ca.waitCount(len(d.expected("a", d.c1, d.c2)))
	cb.waitCount(len(d.expected("b", d.c1)))
	_ = ca.link.Close()
	e := d.post(d.c1, "", "after a is gone")
	cb.waitCount(len(d.expected("b", d.c1)))
	if !contains(cb.ids(), e) {
		t.Fatalf("b missed an envelope when a's link dropped: %v", cb.ids())
	}
	// a reconnects and gets it from the log.
	ca2 := d.client()
	ca2.hello("a", nil, nil)
	waitFor(t, func() bool { return contains(ca2.ids(), e) })
}

// 3a: a creator that is connected when her channel opens is sent `opened`
// live (it addresses her too, as in Python); once she acked it, a reconnect
// does not send it again. A creator that was away is not owed it on replay
// (Python replays only what others sent) and cannot ack her own text.
func TestCreatorsOpenedIsLiveOnlyAndItsAckIsHonoured(t *testing.T) {
	d := newDlvRig(t)
	z := d.client()
	z.hello("z", nil, nil)
	settle()
	// the channels that opened while z was away are not replayed to her
	for _, e := range z.got {
		if e.EventType == EnvChannelOpened {
			t.Fatalf("opened of a channel created before z attached was replayed to its creator: %s", e.ID[:6])
		}
	}
	// a channel opened while she is connected: live
	c3, err := d.hub.CreateChannel(ChannelManifest{Type: "open"}, []string{d.ids["z"], d.ids["a"]})
	if err != nil {
		t.Fatal(err)
	}
	var opened Envelope
	waitFor(t, func() bool {
		z.mu.Lock()
		defer z.mu.Unlock()
		for _, e := range z.got {
			if e.EventType == EnvChannelOpened && e.ChannelID == c3.ID {
				opened = e
				return true
			}
		}
		return false
	})
	z.receipt(d.ids["z"], c3.ID, opened.ID, ReceiptAck)
	d.waitCursor("z", c3.ID, opened.ID) // Python moves the cursor on her ack of her own lifecycle event
	_ = z.link.Close()
	z2 := d.client()
	z2.hello("z", nil, nil)
	settle()
	for _, e := range z2.got {
		if e.EventType == EnvChannelOpened {
			t.Fatalf("opened was sent again to a fresh client: %s", e.ID[:6])
		}
	}
	// z cannot ack her own text
	own := d.post(d.c1, "", "mine")
	z2.receipt(d.ids["z"], d.c1, own, ReceiptAck)
	settle()
	if got := d.hub.InboxCursor(d.ids["z"], d.c1); got == own {
		t.Fatalf("the sender moved her cursor over her own envelope")
	}
}

func hasType(s *scripted, id, typ string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.got {
		if e.ID == id {
			return e.EventType == typ
		}
	}
	return false
}

// A link that is full stops the walk; it resumes as the client drains it.
func TestFullLinkResumesWhenTheClientDrains(t *testing.T) {
	hub := newTestHub(t)
	hub.RegisterAdapter(openAdapter{}, "1")
	ln := transport.NewMemListener(4) // four frames per direction
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = hub.Serve(ctx, ln) }()
	t.Cleanup(func() { stop(); _ = ln.Close(); <-done })
	z, _ := hub.Register(Passport{Name: "z"}, nil)
	a, _ := hub.Register(Passport{Name: "a"}, nil)
	for _, p := range []Passport{z, a} {
		id := p.AgentID
		_ = hub.Attach(id, func(env Envelope) {
			if env.EventType == EnvChannelInvite {
				_, _ = hub.Send(NewChannelAck(env, id, false, ""))
			}
		})
	}
	ch, err := hub.CreateChannel(ChannelManifest{Type: "open"}, []string{z.AgentID, a.AgentID})
	if err != nil {
		t.Fatal(err)
	}
	const n = 60
	var want []string
	for i := 0; i < n; i++ {
		e, err := hub.Send(NewTextEnvelope(ch.ID, z.AgentID, "", fmt.Sprint("m", i)))
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, e.ID)
	}
	rig := &wireRig{t: t, hub: hub, ln: ln, ctx: ctx}
	c := &dlvRig{wireRig: rig, ids: map[string]string{"a": a.AgentID}}
	cl := c.client()
	cl.hello("a", nil, nil)
	// drain slowly: ack every envelope as it arrives (acks free the link)
	deadline := time.Now().Add(10 * time.Second)
	acked := 0
	for time.Now().Before(deadline) {
		got := cl.ids()
		for ; acked < len(got); acked++ {
			cl.mu.Lock()
			e := cl.got[acked]
			cl.mu.Unlock()
			cl.receipt(a.AgentID, e.ChannelID, e.ID, ReceiptAck)
		}
		var texts []string
		cl.mu.Lock()
		for _, e := range cl.got {
			if e.EventType == EnvText {
				texts = append(texts, e.ID)
			}
		}
		cl.mu.Unlock()
		if len(texts) == n {
			if strings.Join(texts, ",") != strings.Join(want, ",") {
				t.Fatalf("order through a small link: %v", texts)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("only %d of %d envelopes arrived through a 4-frame link", len(cl.ids()), n)
}

// Serving a hub that accepts the "none" scheme logs a loud warning, once; a
// hub with real credentials only does not.
func TestServingANoAuthHubWarns(t *testing.T) {
	var mu sync.Mutex
	var warnings []string
	prev := slog.Default()
	slog.SetDefault(slog.New(captureHandler{mu: &mu, out: &warnings}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	serve := func(h *Hub) {
		ln := transport.NewMemListener(8)
		ctx, stop := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); _ = h.Serve(ctx, ln) }()
		for i := 0; i < 2; i++ {
			c, _ := ln.Dial()
			defer c.Close()
		}
		time.Sleep(50 * time.Millisecond)
		stop()
		<-done
	}
	serve(newTestHub(t))
	mu.Lock()
	n := len(warnings)
	text := strings.Join(warnings, "\n")
	mu.Unlock()
	if n != 1 || !strings.Contains(text, "none") || !strings.Contains(text, "take that agent over") {
		t.Fatalf("a NoAuth hub must warn exactly once, got %d: %q", n, text)
	}
	mu.Lock()
	warnings = nil
	mu.Unlock()
	serve(newTestHub(t, WithHubAuth(NewAuthRegistry(NewApiKeyAuth(map[string]string{"x": "y"})))))
	mu.Lock()
	defer mu.Unlock()
	if len(warnings) != 0 {
		t.Fatalf("a hub without NoAuth warned: %v", warnings)
	}
}

type captureHandler struct {
	mu  *sync.Mutex
	out *[]string
}

func (captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h captureHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		h.mu.Lock()
		*h.out = append(*h.out, r.Message)
		h.mu.Unlock()
	}
	return nil
}
func (h captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h captureHandler) WithGroup(string) slog.Handler      { return h }

// blockingArbiter denies the dispatch of every envelope sent by the named
// agent to the agent "a" (inbound access: a blocks that sender).
type blockingArbiter struct {
	RuleBasedArbiter
	blocked string // passport name of the sender a blocks
}

func (b blockingArbiter) AuthorizeDispatch(env Envelope, sender Passport, recipient Passport, rule Rule) Decision {
	if sender.Name == b.blocked && recipient.Name == "a" {
		return Decision{Allowed: false, Reason: "blocked sender"}
	}
	return b.RuleBasedArbiter.AuthorizeDispatch(env, sender, recipient, rule)
}

// The inbound access policy applies to the delivery of a stored envelope as it
// does at live dispatch: a sender a recipient blocks is not delivered after a
// reconnect either — the same decision on both paths.
func TestBlockedSenderIsNeitherDeliveredLiveNorOnReplay(t *testing.T) {
	d := newDlvRig(t, WithHubArbiter(blockingArbiter{blocked: "b"}))
	postFrom := func(sender, text string) string {
		env := NewTextEnvelope(d.c1, d.ids[sender], "", text)
		got, err := d.hub.Send(env)
		if err != nil {
			t.Fatalf("post %q: %v", text, err)
		}
		return got.ID
	}
	// live path: a is connected
	a := d.client()
	a.hello("a", nil, nil)
	settle()
	liveOK, liveBlocked := postFrom("z", "live from z"), postFrom("b", "live from b")
	waitFor(t, func() bool { return contains(a.ids(), liveOK) })
	settle()
	if contains(a.ids(), liveBlocked) {
		t.Fatal("live dispatch delivered an envelope of a blocked sender")
	}
	// replay path: a is away while both send, then reconnects
	_ = a.link.Close()
	offOK, offBlocked := postFrom("z", "offline from z"), postFrom("b", "offline from b")
	a2 := d.client()
	a2.hello("a", nil, nil)
	waitFor(t, func() bool { return contains(a2.ids(), offOK) })
	settle()
	if contains(a2.ids(), offBlocked) || contains(a2.ids(), liveBlocked) {
		t.Fatalf("replay delivered an envelope of a blocked sender: %v", a2.ids())
	}
	// the blocked sender's own channel members are not affected: c, not blocking b, gets it
	c := d.client()
	c.hello("c", nil, nil)
	waitFor(t, func() bool { return contains(c.ids(), offOK) })
	if !contains(c.ids(), offBlocked) {
		t.Fatalf("c does not block b and must receive b's envelope: %v", c.ids())
	}
}
