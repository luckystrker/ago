package network

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luckystrker/ago/knowledge"
	"github.com/luckystrker/ago/task"
)

// --- fault store -----------------------------------------------------------------------

var errCrash = errors.New("test: store crashed")

// crashStore wraps a store and "crashes" after a budget of mutating
// operations (Write / Append / Delete): the op that exhausts the budget and
// every later one fails, and the process is considered dead. With
// applyLast the exhausting op reaches the store before its caller sees the
// error (a crash after the write, before the memory update). Reads always
// work: they stand for the restarted process reading the disk.
type crashStore struct {
	knowledge.Store
	mu        sync.Mutex
	budget    int // mutating ops still allowed; < 0 = unlimited
	applyLast bool
	crashed   bool
	ops       int // mutating ops attempted
}

func newCrashStore(inner knowledge.Store) *crashStore {
	return &crashStore{Store: inner, budget: -1}
}

// arm sets the number of further mutating ops allowed before the crash.
func (c *crashStore) arm(budget int, applyLast bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.budget, c.applyLast, c.crashed = budget, applyLast, false
}

func (c *crashStore) disarm() { c.arm(-1, false) }

func (c *crashStore) didCrash() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.crashed
}

// gate reports whether the op may run and whether it must still be applied
// although the caller will see a failure.
func (c *crashStore) gate() (run bool, failAfter bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ops++
	if c.crashed {
		return false, false
	}
	if c.budget < 0 {
		return true, false
	}
	if c.budget == 0 {
		c.crashed = true
		return c.applyLast, c.applyLast
	}
	c.budget--
	return true, false
}

func (c *crashStore) Write(path string, data []byte) error {
	run, failAfter := c.gate()
	if run {
		if err := c.Store.Write(path, data); err != nil {
			return err
		}
	}
	if !run || failAfter {
		return errCrash
	}
	return nil
}

func (c *crashStore) Append(path string, data []byte) error {
	run, failAfter := c.gate()
	if run {
		if err := c.Store.Append(path, data); err != nil {
			return err
		}
	}
	if !run || failAfter {
		return errCrash
	}
	return nil
}

func (c *crashStore) Delete(path string) error {
	run, failAfter := c.gate()
	if run {
		if err := c.Store.Delete(path); err != nil {
			return err
		}
	}
	if !run || failAfter {
		return errCrash
	}
	return nil
}

// --- helpers -----------------------------------------------------------------------------

// openStoreHub starts a hub over store (a "process start": new Hub, adapters
// registered, Hydrate) with a fixed clock, and attaches an invite-acking
// notifier to every restored agent.
func openStoreHub(t testing.TB, store knowledge.Store, clock func() time.Time) *Hub {
	t.Helper()
	h := NewHub(WithHubStore(store), WithAckTimeout(2*time.Second), WithHubClock(clock))
	h.RegisterAdapter(ConsultingAdapter{}, "1")
	h.RegisterAdapter(ConversationAdapter{}, "1")
	h.RegisterAdapter(DiscussionAdapter{}, "1")
	h.RegisterAdapter(WorkflowAdapter{}, "1")
	if err := h.Hydrate(); err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	for _, p := range h.Registrations() {
		ackInvites(h, p.AgentID)
	}
	return h
}

// ackInvites attaches a notifier that acks every channel invite addressed to
// agentID.
func ackInvites(h *Hub, agentID string) {
	_ = h.Attach(agentID, func(env Envelope) {
		if env.EventType == EnvChannelInvite {
			_, _ = h.Send(NewChannelAck(env, agentID, false, ""))
		}
	})
}

func testClock() func() time.Time {
	base := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	n := 0
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		n++
		return base.Add(time.Duration(n) * time.Second)
	}
}

func mustRegister(t testing.TB, h *Hub, name string, r *Rule) Passport {
	t.Helper()
	p, err := h.Register(Passport{Name: name}, r)
	if err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	ackInvites(h, p.AgentID)
	return p
}

func openConversation(t testing.TB, h *Hub, a, b string) *ChannelHandle {
	t.Helper()
	ch, err := h.CreateChannel(ChannelManifest{Type: TypeConversation}, []string{a, b})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	return ch
}

func jsonOf(t testing.TB, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// stateFingerprint canonicalises everything the hub rebuilds on hydrate:
// registry, rules, resumes, capability index, cursors, nacks, channels, the
// in-memory log and the causation index.
func stateFingerprint(h *Hub) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	type chFP struct {
		ID, State, Reason string
		Participants      []string
		Pending, Rejected []string
		Log               []string
	}
	fp := map[string]any{}
	pass := map[string]string{}
	for id, p := range h.passports {
		pass[id] = mustJSONString(p)
	}
	rules := map[string]string{}
	for id, r := range h.rules {
		rules[id] = mustJSONString(r)
	}
	resumes := map[string]string{}
	for id, r := range h.resumes {
		resumes[id] = mustJSONString(r)
	}
	caps := map[string][]string{}
	for c, ids := range h.ext.capIndex {
		for id := range ids {
			caps[c] = append(caps[c], id)
		}
		sort.Strings(caps[c])
	}
	chans := map[string]chFP{}
	for id, ch := range h.channels {
		f := chFP{ID: id, State: string(ch.State), Reason: ch.CloseReason, Participants: ch.ParticipantIDs(),
			Pending: append([]string(nil), ch.PendingAcks...), Rejected: append([]string(nil), ch.RejectedBy...)}
		for _, e := range h.logs[id] {
			f.Log = append(f.Log, e.ID)
		}
		chans[id] = f
	}
	cause := map[string][]string{}
	for chID, m := range h.ext.causation {
		for k, pos := range m {
			cause[chID] = append(cause[chID], fmt.Sprintf("%s|%s|%d", k.sender, k.causation, pos))
		}
		sort.Strings(cause[chID])
	}
	cps := map[string]string{}
	for id, cp := range h.ext.checkpoints {
		cps[id] = cp.writer + ":" + string(cp.state)
	}
	fp["passports"], fp["rules"], fp["resumes"], fp["caps"] = pass, rules, resumes, caps
	fp["cursors"], fp["nacks"] = h.ext.cursors, h.ext.nacks
	fp["channels"], fp["causation"], fp["checkpoints"] = chans, cause, cps
	return mustJSONString(fp)
}

func mustJSONString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "marshal error: " + err.Error()
	}
	return string(b)
}

// --- restart round trips ----------------------------------------------------------------

func TestHubStoreRestartRestoresRegistry(t *testing.T) {
	store := knowledge.NewMemoryStore()
	clock := testClock()
	h1 := openStoreHub(t, store, clock)
	rule := &Rule{InboundGlobs: []string{"*"}, OutboundGlobs: []string{"b*"}, MaxDelegationDepth: 3}
	alice := mustRegister(t, h1, "alice", rule)
	bob := mustRegister(t, h1, "bob", nil)
	if err := h1.SetResume(Resume{AgentID: alice.AgentID, Capabilities: []string{"search", "plan"}, Summary: "finds"}); err != nil {
		t.Fatal(err)
	}
	if err := h1.RecordObservation(alice.AgentID, "search", task.StateCompleted, 40*time.Millisecond, "t1"); err != nil {
		t.Fatal(err)
	}
	if err := h1.AddResumeExample(alice.AgentID, ResumeExample{Title: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := h1.SetRule(bob.AgentID, Rule{InboundGlobs: []string{"a*"}, OutboundGlobs: []string{"*"}}); err != nil {
		t.Fatal(err)
	}
	ch := openConversation(t, h1, alice.AgentID, bob.AgentID)
	env, err := h1.Send(NewTextEnvelope(ch.ID, alice.AgentID, "", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h1.AdvanceInboxCursor(bob.AgentID, ch.ID, env.ID); err != nil {
		t.Fatal(err)
	}
	if err := h1.RecordNack(bob.AgentID, ch.ID, env.ID, "busy"); err != nil {
		t.Fatal(err)
	}
	want := stateFingerprint(h1)

	h2 := openStoreHub(t, store, clock)
	if got := stateFingerprint(h2); got != want {
		t.Fatalf("restored state differs\n got %s\nwant %s", got, want)
	}
	if got := h2.InboxCursor(bob.AgentID, ch.ID); got != env.ID {
		t.Fatalf("cursor: %q want %q", got, env.ID)
	}
	if got := h2.AgentsWithCapability("search"); len(got) != 1 || got[0] != alice.AgentID {
		t.Fatalf("capability index: %v", got)
	}
	r, _ := h2.ResumeOf(alice.AgentID)
	if r.Observed["search"].Completed != 1 || r.Version < 3 {
		t.Fatalf("observed track record lost: %+v", r)
	}
	if ns := h2.Nacks(bob.AgentID); len(ns) != 1 || ns[0].Reason != "busy" {
		t.Fatalf("nacks: %+v", ns)
	}
	// The restored identities keep working: same ids, same names.
	if _, err := h2.Send(NewTextEnvelope(ch.ID, bob.AgentID, "", "back")); err != nil {
		t.Fatalf("send after restart: %v", err)
	}
	if _, err := h2.Register(Passport{Name: "alice"}, nil); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("restored name must stay taken: %v", err)
	}
}

// Python's per-entity layout is the on-disk contract (R-mutations on the
// loader would not notice a renamed file).
func TestHubStoreLayout(t *testing.T) {
	store := knowledge.NewMemoryStore()
	h := openStoreHub(t, store, testClock())
	a := mustRegister(t, h, "alice", nil)
	b := mustRegister(t, h, "bob", nil)
	if err := h.SetResume(Resume{AgentID: a.AgentID, Capabilities: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
	ch := openConversation(t, h, a.AgentID, b.AgentID)
	env, _ := h.Send(NewTextEnvelope(ch.ID, a.AgentID, "", "hi"))
	_ = h.AdvanceInboxCursor(b.AgentID, ch.ID, env.ID)
	_ = h.RecordNack(b.AgentID, ch.ID, env.ID, "r")
	for _, path := range []string{
		"/agents/" + a.AgentID + "/passport.json", "/agents/" + a.AgentID + "/rule.json",
		"/agents/" + a.AgentID + "/resume.json", "/agents/" + b.AgentID + "/inbox.cursors.json",
		"/agents/" + b.AgentID + "/inbox_nacks.jsonl", "/registry/by_capability.json",
		"/channels/" + ch.ID + "/metadata.json", "/channels/" + ch.ID + "/wal.jsonl",
	} {
		if !store.Exists(path) {
			t.Errorf("missing %s", path)
		}
	}
	raw, _ := store.Read("/agents/" + b.AgentID + "/inbox.cursors.json")
	var cur map[string]string
	if err := json.Unmarshal(raw, &cur); err != nil || cur[ch.ID] != env.ID {
		t.Fatalf("cursor file %s: %v", raw, err)
	}
}

// R2/R3: a deregistered agent and its cursor stay gone.
func TestHubStoreUnregisterDoesNotResurrect(t *testing.T) {
	store := knowledge.NewMemoryStore()
	clock := testClock()
	h1 := openStoreHub(t, store, clock)
	a := mustRegister(t, h1, "alice", nil)
	b := mustRegister(t, h1, "bob", nil)
	_ = h1.SetResume(Resume{AgentID: b.AgentID, Capabilities: []string{"x"}})
	ch := openConversation(t, h1, a.AgentID, b.AgentID)
	env, _ := h1.Send(NewTextEnvelope(ch.ID, a.AgentID, "", "hi"))
	_ = h1.AdvanceInboxCursor(b.AgentID, ch.ID, env.ID)
	_ = h1.RecordNack(b.AgentID, ch.ID, env.ID, "r")
	if err := h1.Unregister(b.AgentID); err != nil {
		t.Fatal(err)
	}
	if kids, _ := store.List("/agents/" + b.AgentID); len(kids) != 0 {
		t.Fatalf("agent directory not deleted: %v", kids)
	}
	h2 := openStoreHub(t, store, clock)
	if _, err := h2.Passport(b.AgentID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted passport resurrected: %v", err)
	}
	if h2.InboxCursor(b.AgentID, ch.ID) != "" || len(h2.Nacks(b.AgentID)) != 0 {
		t.Fatalf("deleted agent's cursor/nacks resurrected")
	}
	if got := h2.AgentsWithCapability("x"); len(got) != 0 {
		t.Fatalf("capability index keeps the deleted agent: %v", got)
	}
	// Its name is free again.
	if _, err := h2.Register(Passport{Name: "bob"}, nil); err != nil {
		t.Fatalf("name not released: %v", err)
	}
}

// R2/R3 (crash flavours): the passport delete is the commit point; a crash
// after it leaves directory leftovers that hydrate collects, a crash before
// it leaves the agent fully registered.
func TestHubStoreUnregisterCrashPoints(t *testing.T) {
	for _, tc := range []struct {
		name       string
		budget     int
		wantGone   bool
		wantErrOut bool
	}{
		{"crash before the passport delete", 0, false, true},
		{"crash after the passport delete", 1, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := knowledge.NewMemoryStore()
			cs := newCrashStore(inner)
			clock := testClock()
			h1 := openStoreHub(t, cs, clock)
			a := mustRegister(t, h1, "alice", nil)
			b := mustRegister(t, h1, "bob", nil)
			ch := openConversation(t, h1, a.AgentID, b.AgentID)
			env, _ := h1.Send(NewTextEnvelope(ch.ID, a.AgentID, "", "hi"))
			_ = h1.AdvanceInboxCursor(b.AgentID, ch.ID, env.ID)
			cs.arm(tc.budget, false)
			err := h1.Unregister(b.AgentID)
			if (err != nil) != tc.wantErrOut {
				t.Fatalf("unregister err = %v", err)
			}
			cs.disarm()
			h2 := openStoreHub(t, inner, clock)
			_, perr := h2.Passport(b.AgentID)
			if gone := errors.Is(perr, ErrNotFound); gone != tc.wantGone {
				t.Fatalf("gone=%v want %v", gone, tc.wantGone)
			}
			if tc.wantGone {
				if kids, _ := inner.List("/agents/" + b.AgentID); len(kids) != 0 {
					t.Fatalf("leftovers not collected by hydrate: %v", kids)
				}
				if h2.InboxCursor(b.AgentID, ch.ID) != "" {
					t.Fatal("orphan cursor resurrected")
				}
			} else if h2.InboxCursor(b.AgentID, ch.ID) != env.ID {
				t.Fatalf("cursor of a still-registered agent lost")
			}
		})
	}
}

// A crash between the rule write and the passport write: no half registration.
func TestHubStoreRegisterCrashPoints(t *testing.T) {
	for budget := 0; budget <= 2; budget++ {
		inner := knowledge.NewMemoryStore()
		cs := newCrashStore(inner)
		h1 := openStoreHub(t, cs, testClock())
		cs.arm(budget, false)
		_, err := h1.Register(Passport{Name: "alice"}, nil)
		cs.disarm()
		h2 := openStoreHub(t, inner, testClock())
		regs := h2.Registrations()
		switch {
		case err != nil && len(regs) != 0:
			t.Fatalf("budget %d: failed register left %d registrations", budget, len(regs))
		case err == nil && len(regs) != 1:
			t.Fatalf("budget %d: acknowledged register lost", budget)
		}
		if err != nil {
			if kids, _ := inner.List("/agents"); len(kids) != 0 {
				t.Fatalf("budget %d: orphan files after hydrate: %v", budget, kids)
			}
		}
	}
}

// D17/R6: a cursor write that fails leaves memory where it was, and a
// persisted cursor survives a restart.
func TestHubStoreCursorDurableAndMemoryNeverAhead(t *testing.T) {
	inner := knowledge.NewMemoryStore()
	cs := newCrashStore(inner)
	clock := testClock()
	h1 := openStoreHub(t, cs, clock)
	a := mustRegister(t, h1, "alice", nil)
	b := mustRegister(t, h1, "bob", nil)
	ch := openConversation(t, h1, a.AgentID, b.AgentID)
	e1, _ := h1.Send(NewTextEnvelope(ch.ID, a.AgentID, "", "one"))
	e2, _ := h1.Send(NewTextEnvelope(ch.ID, a.AgentID, "", "two"))
	if err := h1.AdvanceInboxCursor(b.AgentID, ch.ID, e1.ID); err != nil {
		t.Fatal(err)
	}
	cs.arm(0, true) // the write lands, the caller sees a failure
	if err := h1.AdvanceInboxCursor(b.AgentID, ch.ID, e2.ID); err == nil {
		t.Fatal("expected the crash error")
	}
	if got := h1.InboxCursor(b.AgentID, ch.ID); got != e1.ID {
		t.Fatalf("memory ahead of the failed write: %q", got)
	}
	cs.disarm()
	h2 := openStoreHub(t, cs, clock)
	if got := h2.InboxCursor(b.AgentID, ch.ID); got != e2.ID {
		t.Fatalf("persisted cursor lost: %q want %q", got, e2.ID)
	}
	// Monotonic: an older id never moves it back, an ack in channel A leaves B.
	if err := h2.AdvanceInboxCursor(b.AgentID, ch.ID, e1.ID); err != nil {
		t.Fatal(err)
	}
	if h2.InboxCursor(b.AgentID, ch.ID) != e2.ID {
		t.Fatal("cursor moved backwards")
	}
	if err := h2.AdvanceInboxCursor(b.AgentID, "other-channel", "zzz"); err != nil {
		t.Fatal(err)
	}
	if h2.InboxCursor(b.AgentID, ch.ID) != e2.ID || h2.InboxCursor(b.AgentID, "other-channel") != "zzz" {
		t.Fatal("cursors are not per channel")
	}
	// An empty channel or envelope id is a no-op, and so is acking the id the
	// cursor already holds: neither touches the store (a dead store proves it).
	cs.arm(0, false)
	for _, args := range [][2]string{{"", "zzz"}, {"other-channel", ""}, {ch.ID, e2.ID}, {"other-channel", "zzz"}} {
		if err := h2.AdvanceInboxCursor(b.AgentID, args[0], args[1]); err != nil {
			t.Fatalf("AdvanceInboxCursor(%q, %q) must not write: %v", args[0], args[1], err)
		}
	}
	cs.disarm()
	if got := h2.InboxCursor(b.AgentID, ""); got != "" {
		t.Fatalf("cursor stored for an empty channel id: %q", got)
	}
	// A nack leaves the cursor in place.
	if err := h2.RecordNack(b.AgentID, ch.ID, e2.ID, "no"); err != nil {
		t.Fatal(err)
	}
	if h2.InboxCursor(b.AgentID, ch.ID) != e2.ID {
		t.Fatal("nack moved the cursor")
	}
	if err := h2.AdvanceInboxCursor("ghost", ch.ID, e2.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown agent: %v", err)
	}
}

// R4/R5: hydrate folds the whole WAL, and running it again changes nothing.
func TestHubHydrateIsIdempotent(t *testing.T) {
	store := knowledge.NewMemoryStore()
	clock := testClock()
	h1 := openStoreHub(t, store, clock)
	a := mustRegister(t, h1, "alice", nil)
	b := mustRegister(t, h1, "bob", nil)
	_ = h1.SetResume(Resume{AgentID: a.AgentID, Capabilities: []string{"x"}})
	ch := openConversation(t, h1, a.AgentID, b.AgentID)
	for i := 0; i < 3; i++ {
		env := NewTextEnvelope(ch.ID, a.AgentID, "", fmt.Sprint("m", i))
		env.CausationID = fmt.Sprint("c", i)
		if _, err := h1.Send(env); err != nil {
			t.Fatal(err)
		}
	}
	if err := h1.CheckpointTask(a.AgentID, "task-1", []byte(`{"n":1}`)); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	_ = h1.AdvanceInboxCursor(b.AgentID, ch.ID, "zz")
	want := stateFingerprint(h1)

	h2 := openStoreHub(t, store, clock)
	first := stateFingerprint(h2)
	if first != want {
		t.Fatalf("hydrate lost state\n got %s\nwant %s", first, want)
	}
	if err := h2.Hydrate(); err != nil {
		t.Fatal(err)
	}
	if second := stateFingerprint(h2); second != first {
		t.Fatalf("second hydrate changed state\n got %s\nwant %s", second, first)
	}
	// R7: the WAL only ever grows by appending.
	before, _ := store.Read("/channels/" + ch.ID + "/wal.jsonl")
	_ = h2.Hydrate()
	after, _ := store.Read("/channels/" + ch.ID + "/wal.jsonl")
	if string(before) != string(after) {
		t.Fatal("hydrate rewrote the WAL")
	}
}

// Replay on a file/memory WAL (no store) is idempotent too.
func TestHubReplayTwiceSameState(t *testing.T) {
	wal := NewMemoryWAL()
	h1 := newTestHub(t, WithHubWAL(wal))
	a, b := registerTwo(t, h1)
	rec := newRecorder()
	rec.attach(h1, a, true)
	rec.attach(h1, b, true)
	ch, err := h1.CreateChannel(ChannelManifest{Type: TypeConversation}, []string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = h1.Send(NewTextEnvelope(ch.ID, a, "", "x"))

	h2 := newTestHub(t, WithHubWAL(wal))
	if err := h2.Replay(); err != nil {
		t.Fatal(err)
	}
	once := stateFingerprint(h2)
	if err := h2.Replay(); err != nil {
		t.Fatal(err)
	}
	if twice := stateFingerprint(h2); twice != once {
		t.Fatalf("replay twice differs\n%s\n%s", once, twice)
	}
}

// --- idempotent post ---------------------------------------------------------------------------

func causedText(ch, sender, cause, text string) Envelope {
	env := NewTextEnvelope(ch, sender, "", text)
	env.CausationID = cause
	return env
}

// D4: a post retried under the same (channel, sender, causation) is accepted
// once, returns the first envelope and is not dispatched again.
func TestHubPostIdempotentByCausation(t *testing.T) {
	store := knowledge.NewMemoryStore()
	h := openStoreHub(t, store, testClock())
	a := mustRegister(t, h, "alice", nil)
	b := mustRegister(t, h, "bob", nil)
	got := newRecorder()
	_ = h.Attach(b.AgentID, func(env Envelope) {
		got.mu.Lock()
		got.got[b.AgentID] = append(got.got[b.AgentID], env)
		got.mu.Unlock()
		if env.EventType == EnvChannelInvite {
			_, _ = h.Send(NewChannelAck(env, b.AgentID, false, ""))
		}
	})
	ch := openConversation(t, h, a.AgentID, b.AgentID)
	delivered := len(got.forAgent(b.AgentID))

	first, err := h.Send(causedText(ch.ID, a.AgentID, "c1", "first"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := h.Send(causedText(ch.ID, a.AgentID, "c1", "retry with other text"))
	if err != nil {
		t.Fatalf("retry must succeed: %v", err)
	}
	if again.ID != first.ID || string(again.Payload) != string(first.Payload) {
		t.Fatalf("retry returned %s/%s, want the accepted %s/%s", again.ID, again.Payload, first.ID, first.Payload)
	}
	if n := len(got.forAgent(b.AgentID)) - delivered; n != 1 {
		t.Fatalf("recipient got %d copies", n)
	}
	log, _ := h.ReadWAL(ch.ID)
	count := 0
	for _, e := range log {
		if e.CausationID == "c1" && e.SenderID == a.AgentID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("WAL holds %d entries for the key", count)
	}
	// A different sender or key is a different post.
	if other, err := h.Send(causedText(ch.ID, b.AgentID, "c1", "bob")); err != nil || other.ID == first.ID {
		t.Fatalf("other sender deduped: %v", err)
	}
	if other, err := h.Send(causedText(ch.ID, a.AgentID, "c2", "second")); err != nil || other.ID == first.ID {
		t.Fatalf("other key deduped: %v", err)
	}
	// No causation id: never deduped.
	x, _ := h.Send(NewTextEnvelope(ch.ID, a.AgentID, "", "p"))
	y, _ := h.Send(NewTextEnvelope(ch.ID, a.AgentID, "", "p"))
	if x.ID == y.ID {
		t.Fatal("posts without a causation id were merged")
	}
}

// D7: the index comes back from the WAL after a restart, so a retry that
// crosses the restart is still accepted once.
func TestHubPostIdempotentAcrossRestart(t *testing.T) {
	store := knowledge.NewMemoryStore()
	clock := testClock()
	h1 := openStoreHub(t, store, clock)
	a := mustRegister(t, h1, "alice", nil)
	b := mustRegister(t, h1, "bob", nil)
	ch := openConversation(t, h1, a.AgentID, b.AgentID)
	first, err := h1.Send(causedText(ch.ID, a.AgentID, "c1", "first"))
	if err != nil {
		t.Fatal(err)
	}
	h2 := openStoreHub(t, store, clock)
	again, err := h2.Send(causedText(ch.ID, a.AgentID, "c1", "first"))
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID {
		t.Fatalf("retry after restart accepted again: %s vs %s", again.ID, first.ID)
	}
	if log, _ := h2.ReadWAL(ch.ID); countCause(log, a.AgentID, "c1") != 1 {
		t.Fatalf("WAL has %d entries", countCause(log, a.AgentID, "c1"))
	}
	if body, _ := store.Read("/channels/" + ch.ID + "/wal.jsonl"); strings.Count(string(body), `"causation_id":"c1"`) != 1 {
		t.Fatalf("store WAL has duplicates:\n%s", body)
	}
}

// The index is pruned when the channel ends: a retry then fails with the
// channel error, not with a stale envelope; a restart keeps it pruned.
func TestHubPostIdempotencyPrunedWithChannel(t *testing.T) {
	store := knowledge.NewMemoryStore()
	clock := testClock()
	h1 := openStoreHub(t, store, clock)
	a := mustRegister(t, h1, "alice", nil)
	b := mustRegister(t, h1, "bob", nil)
	ch := openConversation(t, h1, a.AgentID, b.AgentID)
	if _, err := h1.Send(causedText(ch.ID, a.AgentID, "c1", "x")); err != nil {
		t.Fatal(err)
	}
	if err := h1.CloseChannel(ch.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if _, ok := h1.FindEnvelopeByCausation(ch.ID, a.AgentID, "c1"); ok {
		t.Fatal("index kept after close")
	}
	if _, err := h1.Send(causedText(ch.ID, a.AgentID, "c1", "x")); !errors.Is(err, ErrChannelClosed) {
		t.Fatalf("retry on a closed channel: %v", err)
	}
	h2 := openStoreHub(t, store, clock)
	if _, ok := h2.FindEnvelopeByCausation(ch.ID, a.AgentID, "c1"); ok {
		t.Fatal("index resurrected for a closed channel")
	}
	if got := h2.channels[ch.ID].State; got != ChannelClosed {
		t.Fatalf("restored state %s", got)
	}
}

// Crash between "WAL appended" and the reply: the retry after restart finds
// the appended envelope instead of appending it a second time.
func TestHubPostRetryAfterCrashBetweenWALAndReply(t *testing.T) {
	inner := knowledge.NewMemoryStore()
	cs := newCrashStore(inner)
	clock := testClock()
	h1 := openStoreHub(t, cs, clock)
	a := mustRegister(t, h1, "alice", nil)
	b := mustRegister(t, h1, "bob", nil)
	ch := openConversation(t, h1, a.AgentID, b.AgentID)
	cs.arm(0, true) // the append lands, the sender sees an error
	if _, err := h1.Send(causedText(ch.ID, a.AgentID, "c1", "x")); err == nil {
		t.Fatal("expected the crash error")
	}
	cs.disarm()
	h2 := openStoreHub(t, inner, clock)
	again, err := h2.Send(causedText(ch.ID, a.AgentID, "c1", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if log, _ := h2.ReadWAL(ch.ID); countCause(log, a.AgentID, "c1") != 1 {
		t.Fatalf("retry appended a second copy (%d entries), id %s", countCause(log, a.AgentID, "c1"), again.ID)
	}
}

func countCause(log []Envelope, sender, cause string) int {
	n := 0
	for _, e := range log {
		if e.SenderID == sender && e.CausationID == cause {
			n++
		}
	}
	return n
}

// A failed store write fails the registry operation and leaves memory alone.
func TestHubStoreFailureLeavesMemoryUntouched(t *testing.T) {
	inner := knowledge.NewMemoryStore()
	cs := newCrashStore(inner)
	h := openStoreHub(t, cs, testClock())
	a := mustRegister(t, h, "alice", nil)
	before := stateFingerprint(h)

	cs.arm(0, false)
	if err := h.SetRule(a.AgentID, Rule{InboundGlobs: []string{"x"}, OutboundGlobs: []string{"y"}}); !errors.Is(err, errCrash) {
		t.Fatalf("SetRule: %v", err)
	}
	if err := h.SetResume(Resume{AgentID: a.AgentID, Capabilities: []string{"c"}}); !errors.Is(err, errCrash) {
		t.Fatalf("SetResume: %v", err)
	}
	if err := h.RecordObservation(a.AgentID, "c", task.StateCompleted, 0, "t"); !errors.Is(err, errCrash) {
		t.Fatalf("RecordObservation: %v", err)
	}
	if err := h.AddResumeExample(a.AgentID, ResumeExample{Title: "x"}); !errors.Is(err, errCrash) {
		t.Fatalf("AddResumeExample: %v", err)
	}
	if err := h.Unregister(a.AgentID); !errors.Is(err, errCrash) {
		t.Fatalf("Unregister: %v", err)
	}
	if _, err := h.Register(Passport{Name: "carol"}, nil); !errors.Is(err, errCrash) {
		t.Fatalf("Register: %v", err)
	}
	if after := stateFingerprint(h); after != before {
		t.Fatalf("memory changed although every write failed\n%s\n%s", before, after)
	}
}

// openProdHub starts a hub through the production entry point.
func openProdHub(t testing.TB, store knowledge.Store, clock func() time.Time) *Hub {
	t.Helper()
	h, err := OpenHub(WithHubStore(store), WithAckTimeout(2*time.Second), WithHubClock(clock),
		WithTTLSweepInterval(0), WithExpectationSweepInterval(0))
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	for _, p := range h.Registrations() {
		ackInvites(h, p.AgentID)
	}
	return h
}

// OpenHub (the production entry point) restores the registry, not only the
// channels: after a crash the same identities are back and a reconnecting
// client that registers its name again is refused instead of creating a
// second passport.
func TestOpenHubRestoresRegistryAfterCrash(t *testing.T) {
	inner := knowledge.NewMemoryStore()
	cs := newCrashStore(inner)
	clock := testClock()
	h1 := openProdHub(t, cs, clock)
	alice := mustRegister(t, h1, "alice", nil)
	bob := mustRegister(t, h1, "bob", nil)
	if err := h1.SetResume(Resume{AgentID: alice.AgentID, Capabilities: []string{"search"}}); err != nil {
		t.Fatal(err)
	}
	ch := openConversation(t, h1, alice.AgentID, bob.AgentID)
	env, err := h1.Send(NewTextEnvelope(ch.ID, alice.AgentID, "", "hi"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h1.AdvanceInboxCursor(bob.AgentID, ch.ID, env.ID); err != nil {
		t.Fatal(err)
	}
	cs.arm(0, false) // the process dies: nothing more reaches the disk

	h2 := openProdHub(t, inner, clock)
	if got := len(h2.Registrations()); got != 2 {
		t.Fatalf("OpenHub restored %d registrations, want 2", got)
	}
	if p, err := h2.Passport("alice"); err != nil || p.AgentID != alice.AgentID {
		t.Fatalf("alice after OpenHub: %+v %v", p, err)
	}
	if h2.InboxCursor(bob.AgentID, ch.ID) != env.ID {
		t.Fatal("cursor not restored by OpenHub")
	}
	if got := h2.AgentsWithCapability("search"); len(got) != 1 {
		t.Fatalf("capability index: %v", got)
	}
	if _, err := h2.Register(Passport{Name: "alice"}, nil); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("re-registering alice after a restart: %v", err)
	}
	kids, _ := inner.List("/agents")
	if len(kids) != 2 {
		t.Fatalf("store holds %d agents, want 2: %v", len(kids), kids)
	}
	// The restored channel is usable with the restored identities.
	if _, err := h2.Send(NewTextEnvelope(ch.ID, bob.AgentID, "", "back")); err != nil {
		t.Fatalf("send after OpenHub: %v", err)
	}
}

// A task checkpoint written through a store-backed hub survives a restart
// with its claimed writer; a failed store write fails the call and keeps the
// previous checkpoint (the StoreWAL checkpoint branch).
func TestHubStoreCheckpointDurable(t *testing.T) {
	inner := knowledge.NewMemoryStore()
	cs := newCrashStore(inner)
	clock := testClock()
	h1 := openStoreHub(t, cs, clock)
	if err := h1.CheckpointTask("alice", "task-1", []byte("v1")); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if !inner.Exists("/tasks/task-1/checkpoint.json") {
		t.Fatal("checkpoint not stored in the task layout")
	}
	cs.arm(0, false)
	if err := h1.CheckpointTask("alice", "task-1", []byte("v2")); !errors.Is(err, errCrash) {
		t.Fatalf("failed write must fail the call: %v", err)
	}
	if got, _ := h1.ReadTaskCheckpoint("alice", "task-1"); string(got) != "v1" {
		t.Fatalf("memory moved ahead of the store: %q", got)
	}
	cs.disarm()

	h2 := openStoreHub(t, inner, clock)
	if got, err := h2.ReadTaskCheckpoint("alice", "task-1"); err != nil || string(got) != "v1" {
		t.Fatalf("restored checkpoint %q, %v", got, err)
	}
	if err := h2.CheckpointTask("mallory", "task-1", []byte("evil")); !errors.Is(err, ErrCheckpointDenied) {
		t.Fatalf("writer claim lost across the restart: %v", err)
	}
}

// A cursor file with empty channel or envelope ids (Python's loader drops
// such entries) restores only the well-formed ones.
func TestHubHydrateDropsEmptyCursorEntries(t *testing.T) {
	store := knowledge.NewMemoryStore()
	clock := testClock()
	h1 := openStoreHub(t, store, clock)
	a := mustRegister(t, h1, "alice", nil)
	if err := store.Write("/agents/"+a.AgentID+"/inbox.cursors.json", []byte(`{"c1":"e1","":"e2","c3":""}`)); err != nil {
		t.Fatal(err)
	}
	h2 := openStoreHub(t, store, clock)
	h2.mu.Lock()
	got := mustJSONString(h2.ext.cursors[a.AgentID])
	h2.mu.Unlock()
	if got != `{"c1":"e1"}` {
		t.Fatalf("restored cursors %s", got)
	}
}

// Hydrate restores the registration order (creation time, not id order).
func TestHubHydrateKeepsRegistrationOrder(t *testing.T) {
	store := knowledge.NewMemoryStore()
	clock := testClock()
	h1 := openStoreHub(t, store, clock)
	for i := 0; i < 8; i++ {
		mustRegister(t, h1, fmt.Sprintf("agent%d", i), nil)
	}
	h1.mu.Lock()
	want := map[string]int{}
	for id, n := range h1.ext.regSeq {
		want[id] = n
	}
	h1.mu.Unlock()
	h2 := openStoreHub(t, store, clock)
	h2.mu.Lock()
	defer h2.mu.Unlock()
	if mustJSONString(h2.ext.regSeq) != mustJSONString(want) || h2.ext.nextSeq != 8 {
		t.Fatalf("registration order after hydrate\n got %v (next %d)\nwant %v", h2.ext.regSeq, h2.ext.nextSeq, want)
	}
}
