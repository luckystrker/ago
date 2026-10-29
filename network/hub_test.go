package network

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"ago/knowledge"
)

// fakeClock is a controllable hub clock (TTL tests).
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// recorder collects dispatched envelopes per agent (thread-safe: notifiers
// run on the sender's goroutine).
type recorder struct {
	mu  sync.Mutex
	got map[string][]Envelope
}

func newRecorder() *recorder { return &recorder{got: map[string][]Envelope{}} }

func (r *recorder) attach(h *Hub, agentID string, ackInvites bool) {
	_ = h.Attach(agentID, func(env Envelope) {
		r.mu.Lock()
		r.got[agentID] = append(r.got[agentID], env)
		r.mu.Unlock()
		if ackInvites && env.EventType == EnvChannelInvite {
			_, _ = h.Send(NewChannelAck(env, agentID, false, ""))
		}
	})
}

func (r *recorder) forAgent(agentID string) []Envelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Envelope(nil), r.got[agentID]...)
}

// newTestHub registers the four built-in adapters.
func newTestHub(t *testing.T, opts ...HubOption) *Hub {
	t.Helper()
	h := NewHub(append([]HubOption{WithAckTimeout(3 * time.Second)}, opts...)...)
	h.RegisterAdapter(ConsultingAdapter{}, "1")
	h.RegisterAdapter(ConversationAdapter{}, "1")
	h.RegisterAdapter(DiscussionAdapter{}, "1")
	h.RegisterAdapter(WorkflowAdapter{}, "1")
	return h
}

func registerTwo(t *testing.T, h *Hub) (string, string) {
	t.Helper()
	a, err := h.Register(Passport{Name: "alice"}, nil)
	if err != nil {
		t.Fatalf("register alice: %v", err)
	}
	b, err := h.Register(Passport{Name: "bob"}, nil)
	if err != nil {
		t.Fatalf("register bob: %v", err)
	}
	return a.AgentID, b.AgentID
}

func waitForChannel(t *testing.T, h *Hub, id string, want ChannelState) *ChannelHandle {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ch, err := h.Channel(id)
		if err == nil && ch.State == want {
			return ch
		}
		time.Sleep(2 * time.Millisecond)
	}
	ch, _ := h.Channel(id)
	t.Fatalf("channel %s never reached %s (now %s, reason %q)", id, want, ch.State, ch.CloseReason)
	return nil
}

func TestHubRegisterValidation(t *testing.T) {
	h := newTestHub(t)
	if _, err := h.Register(Passport{Name: ""}, nil); !errors.Is(err, ErrBadIdentity) {
		t.Fatalf("empty name: %v", err)
	}
	if _, err := h.Register(Passport{Name: "x", Kind: "robot"}, nil); !errors.Is(err, ErrBadIdentity) {
		t.Fatalf("bad kind: %v", err)
	}
	if _, err := h.Register(Passport{Name: "alice"}, nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := h.Register(Passport{Name: "alice"}, nil); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate name: %v", err)
	}
	p, err := h.Passport("alice")
	if err != nil || p.AgentID == "" {
		t.Fatalf("lookup by name: %+v %v", p, err)
	}
	if p.EffectiveKind() != KindAgent {
		t.Fatalf("default kind: %q", p.Kind)
	}
	if _, err := h.Passport("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown passport: %v", err)
	}
}

func TestHubLifecycleConsulting(t *testing.T) {
	h := newTestHub(t)
	a, b := registerTwo(t, h)
	rec := newRecorder()
	rec.attach(h, a, true)
	rec.attach(h, b, true)

	ch, err := h.CreateChannel(ChannelManifest{Type: TypeConsulting}, []string{a, b})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if ch.State != ChannelActive {
		t.Fatalf("after handshake: %s", ch.State)
	}
	if len(ch.PendingAcks) != 0 {
		t.Fatalf("pending acks not drained: %v", ch.PendingAcks)
	}
	// Roles: creator = initiator, invitee = respondent.
	if ch.Participants[0].Role != RoleInitiator || ch.Participants[1].Role != RoleRespondent {
		t.Fatalf("roles: %+v", ch.Participants)
	}

	// Lifecycle 1: bob receives the invite + opened; alice only opened.
	gotB := rec.forAgent(b)
	if len(gotB) < 2 || gotB[0].EventType != EnvChannelInvite || gotB[1].EventType != EnvChannelOpened {
		t.Fatalf("bob lifecycle envelopes: %v", eventTypes(gotB))
	}
	gotA := rec.forAgent(a)
	if len(gotA) != 1 || gotA[0].EventType != EnvChannelOpened {
		t.Fatalf("alice lifecycle envelopes: %v", eventTypes(gotA))
	}

	// Send: question → answer → auto-close.
	if _, err := h.Send(NewTextEnvelope(ch.ID, a, "", "question?")); err != nil {
		t.Fatalf("send question: %v", err)
	}
	if _, err := h.Send(NewTextEnvelope(ch.ID, b, "", "answer.")); err != nil {
		t.Fatalf("send answer: %v", err)
	}
	done := waitForChannel(t, h, ch.ID, ChannelClosed)
	if done.CloseReason != "consulting_complete" {
		t.Fatalf("close reason: %q", done.CloseReason)
	}

	// Both sides see the lifecycle broadcasts; only bob sees the question.
	gotA = rec.forAgent(a)
	kinds := eventTypes(gotA)
	if kinds[len(kinds)-1] != EnvChannelClosed {
		t.Fatalf("alice last envelope: %v", kinds)
	}
	gotB = rec.forAgent(b)
	found := false
	for _, env := range gotB {
		if env.EventType == EnvText && env.SenderID == a {
			found = true
		}
	}
	if !found {
		t.Fatalf("bob never saw the question: %v", eventTypes(gotB))
	}

	// Terminal channels reject further sends.
	if _, err := h.Send(NewTextEnvelope(ch.ID, a, "", "again")); !errors.Is(err, ErrChannelClosed) {
		t.Fatalf("send on closed: %v", err)
	}

	// The in-memory WAL holds the full accepted sequence.
	wal, err := h.ReadWAL(ch.ID)
	if err != nil {
		t.Fatalf("read wal: %v", err)
	}
	want := []string{EnvChannelInvite, EnvChannelAck, EnvChannelOpened, EnvText, EnvText, EnvChannelClosed}
	if len(wal) != len(want) {
		t.Fatalf("wal length %d, want %d: %v", len(wal), len(want), eventTypes(wal))
	}
	for i, e := range want {
		if wal[i].EventType != e {
			t.Fatalf("wal[%d] = %s, want %s", i, wal[i].EventType, e)
		}
	}
	// Hub-stamped fields are present.
	for _, env := range wal {
		if env.ID == "" || env.CreatedAt.IsZero() {
			t.Fatalf("envelope not hub-stamped: %+v", env)
		}
	}
	if len(h.ListChannels()) != 1 {
		t.Fatalf("list channels: %d", len(h.ListChannels()))
	}
}

func eventTypes(envs []Envelope) []string {
	out := make([]string, len(envs))
	for i, e := range envs {
		out[i] = e.EventType
	}
	return out
}

func TestHubAckTimeout(t *testing.T) {
	h := NewHub(WithAckTimeout(30 * time.Millisecond))
	h.RegisterAdapter(ConsultingAdapter{}, "1")
	a, b := registerTwo(t, h)
	rec := newRecorder()
	rec.attach(h, a, true) // b never acks

	_, err := h.CreateChannel(ChannelManifest{Type: TypeConsulting}, []string{a, b})
	if !errors.Is(err, ErrAckTimeout) {
		t.Fatalf("want ErrAckTimeout, got %v", err)
	}
	chans := h.ListChannels()
	if len(chans) != 1 || chans[0].State != ChannelClosed || chans[0].CloseReason != "invite_timeout" {
		t.Fatalf("channel after timeout: %+v", chans)
	}
}

func TestHubInviteReject(t *testing.T) {
	h := newTestHub(t)
	a, b := registerTwo(t, h)
	_ = h.Attach(b, func(env Envelope) {
		if env.EventType == EnvChannelInvite {
			_, _ = h.Send(NewChannelAck(env, b, true, "busy"))
		}
	})

	_, err := h.CreateChannel(ChannelManifest{Type: TypeConsulting}, []string{a, b})
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("want protocol error on reject, got %v", err)
	}
	chans := h.ListChannels()
	if len(chans) != 1 || chans[0].State != ChannelClosed || chans[0].CloseReason != "invite_rejected" {
		t.Fatalf("channel after reject: %+v", chans)
	}
	if len(chans[0].RejectedBy) != 1 || chans[0].RejectedBy[0] != b {
		t.Fatalf("rejected_by: %v", chans[0].RejectedBy)
	}
}

func TestHubRules(t *testing.T) {
	t.Run("outbound_glob_denies", func(t *testing.T) {
		h := newTestHub(t)
		rule := DefaultRule()
		rule.OutboundGlobs = []string{"bob"} // alice may only address bob
		a, _ := h.Register(Passport{Name: "alice"}, &rule)
		b, _ := h.Register(Passport{Name: "bob"}, nil)
		c, _ := h.Register(Passport{Name: "carol"}, nil)
		rec := newRecorder()
		rec.attach(h, a.AgentID, true)
		rec.attach(h, b.AgentID, true)
		ch, err := h.CreateChannel(ChannelManifest{Type: TypeConversation}, []string{a.AgentID, b.AgentID})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		// Addressed send to the allowed recipient passes.
		if _, err := h.Send(NewTextEnvelope(ch.ID, a.AgentID, b.AgentID, "hi")); err != nil {
			t.Fatalf("allowed send: %v", err)
		}
		// Addressing a registered non-participant violates the glob.
		env := NewTextEnvelope(ch.ID, a.AgentID, c.AgentID, "hi carol")
		if _, err := h.Send(env); !errors.Is(err, ErrAccessDenied) {
			t.Fatalf("outbound violation: %v", err)
		}
		// Broadcast skips the outbound gate (Python parity).
		if _, err := h.Send(NewTextEnvelope(ch.ID, a.AgentID, "", "broadcast")); err != nil {
			t.Fatalf("broadcast: %v", err)
		}
		// The gate fires on invites too: a creator restricted to "bob"
		// cannot open a channel to carol.
		_, err = h.CreateChannel(ChannelManifest{Type: TypeConversation}, []string{a.AgentID, c.AgentID})
		if !errors.Is(err, ErrAccessDenied) {
			t.Fatalf("invite past outbound glob: %v", err)
		}
	})

	t.Run("inbound_glob_skips_silently", func(t *testing.T) {
		h := newTestHub(t)
		carolRule := DefaultRule()
		carolRule.InboundGlobs = []string{"alice"} // carol only accepts alice
		a, _ := h.Register(Passport{Name: "alice"}, nil)
		b, _ := h.Register(Passport{Name: "bob"}, nil)
		c, _ := h.Register(Passport{Name: "carol"}, &carolRule)
		rec := newRecorder()
		rec.attach(h, a.AgentID, true)
		rec.attach(h, b.AgentID, true)
		rec.attach(h, c.AgentID, true)
		ch, err := h.CreateChannel(ChannelManifest{Type: TypeDiscussion},
			[]string{a.AgentID, b.AgentID, c.AgentID})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		// alice (creator, expected first) speaks → bob and carol both accept.
		if _, err := h.Send(NewTextEnvelope(ch.ID, a.AgentID, "", "from alice")); err != nil {
			t.Fatalf("send alice: %v", err)
		}
		// bob's turn → carol's inbound rule blocks "bob": silently skipped.
		if _, err := h.Send(NewTextEnvelope(ch.ID, b.AgentID, "", "from bob")); err != nil {
			t.Fatalf("send bob: %v", err)
		}
		for _, env := range rec.forAgent(c.AgentID) {
			if env.SenderID == b.AgentID {
				t.Fatalf("carol received blocked envelope from bob: %+v", env)
			}
		}
		found := false
		for _, env := range rec.forAgent(c.AgentID) {
			if env.SenderID == a.AgentID && env.EventType == EnvText {
				found = true
			}
		}
		if !found {
			t.Fatal("carol never received alice's allowed text")
		}
		foundBobAtAlice := false
		for _, env := range rec.forAgent(a.AgentID) {
			if env.SenderID == b.AgentID && env.EventType == EnvText {
				foundBobAtAlice = true
			}
		}
		if !foundBobAtAlice {
			t.Fatal("alice never received bob's text")
		}
		_ = h.CloseChannel(ch.ID, "done")
	})

	t.Run("delegation_depth", func(t *testing.T) {
		h := newTestHub(t)
		rule := DefaultRule()
		rule.MaxDelegationDepth = 2
		a, _ := h.Register(Passport{Name: "alice"}, &rule)
		b, _ := h.Register(Passport{Name: "bob"}, nil)
		rec := newRecorder()
		rec.attach(h, a.AgentID, true)
		rec.attach(h, b.AgentID, true)
		ch, err := h.CreateChannel(ChannelManifest{Type: TypeConversation}, []string{a.AgentID, b.AgentID})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		env := NewTextEnvelope(ch.ID, a.AgentID, "", "deep")
		env.Depth = 3
		if _, err := h.Send(env); !errors.Is(err, ErrDepthExceeded) {
			t.Fatalf("depth violation: %v", err)
		}
		env.Depth = 2
		if _, err := h.Send(env); err != nil {
			t.Fatalf("depth 2 within cap: %v", err)
		}
	})

	t.Run("max_concurrent_channels", func(t *testing.T) {
		h := newTestHub(t)
		rule := DefaultRule()
		rule.MaxConcurrentChannels = 1
		a, _ := h.Register(Passport{Name: "alice"}, &rule)
		b, _ := h.Register(Passport{Name: "bob"}, nil)
		rec := newRecorder()
		rec.attach(h, a.AgentID, true)
		rec.attach(h, b.AgentID, true)
		if _, err := h.CreateChannel(ChannelManifest{Type: TypeConversation}, []string{a.AgentID, b.AgentID}); err != nil {
			t.Fatalf("first create: %v", err)
		}
		_, err := h.CreateChannel(ChannelManifest{Type: TypeConversation}, []string{a.AgentID, b.AgentID})
		if !errors.Is(err, ErrAccessDenied) {
			t.Fatalf("second create: %v", err)
		}
	})
}

func TestHubTTL(t *testing.T) {
	clock := newFakeClock()
	h := newTestHub(t, WithHubClock(clock.Now))
	a, b := registerTwo(t, h)
	rec := newRecorder()
	rec.attach(h, a, true)
	rec.attach(h, b, true)

	t.Run("channel_expiry", func(t *testing.T) {
		ch, err := h.CreateChannel(ChannelManifest{
			Type:  TypeConversation,
			Knobs: map[string]any{"ttl_seconds": 10},
		}, []string{a, b})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := h.Send(NewTextEnvelope(ch.ID, a, "", "in time")); err != nil {
			t.Fatalf("send before expiry: %v", err)
		}
		clock.Advance(11 * time.Second)
		_, err = h.Send(NewTextEnvelope(ch.ID, a, "", "too late"))
		if !errors.Is(err, ErrChannelExpired) {
			t.Fatalf("want ErrChannelExpired, got %v", err)
		}
		done := waitForChannel(t, h, ch.ID, ChannelExpired)
		if done.CloseReason != "ttl" {
			t.Fatalf("close reason: %q", done.CloseReason)
		}
	})

	t.Run("envelope_ttl", func(t *testing.T) {
		ch, err := h.CreateChannel(ChannelManifest{Type: TypeConversation}, []string{a, b})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		env := NewTextEnvelope(ch.ID, a, "", "short lived")
		env.CreatedAt = clock.Now()
		env.TTLSecs = 5
		if _, err := h.Send(env); err != nil {
			t.Fatalf("send fresh: %v", err)
		}
		clock.Advance(6 * time.Second)
		env2 := NewTextEnvelope(ch.ID, a, "", "stale")
		env2.CreatedAt = clock.Now().Add(-6 * time.Second)
		env2.TTLSecs = 5
		if _, err := h.Send(env2); !errors.Is(err, ErrTTLExpired) {
			t.Fatalf("want ErrTTLExpired, got %v", err)
		}
	})
}

func TestHubUnknownSenderAndChannel(t *testing.T) {
	h := newTestHub(t)
	a, b := registerTwo(t, h)
	rec := newRecorder()
	rec.attach(h, a, true)
	rec.attach(h, b, true)
	ch, err := h.CreateChannel(ChannelManifest{Type: TypeConversation}, []string{a, b})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := h.Send(NewTextEnvelope(ch.ID, "ghost", "", "boo")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown sender: %v", err)
	}
	if _, err := h.Send(NewTextEnvelope("nope", a, "", "boo")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown channel: %v", err)
	}
}

// TestHubPendingGate pins the active-state gate: a substantive envelope
// cannot travel while the handshake is still pending.
func TestHubPendingGate(t *testing.T) {
	h := NewHub(WithAckTimeout(5 * time.Second))
	h.RegisterAdapter(ConversationAdapter{}, "1")
	a, b := registerTwo(t, h)
	rec := newRecorder()
	rec.attach(h, a, true) // b attached without acking below
	_ = h.Attach(b, func(Envelope) {})

	done := make(chan error, 1)
	go func() {
		_, err := h.CreateChannel(ChannelManifest{Type: TypeConversation}, []string{a, b})
		done <- err
	}()
	// Wait until the channel exists in pending state.
	var chID string
	for i := 0; i < 500 && chID == ""; i++ {
		time.Sleep(2 * time.Millisecond)
		if chans := h.ListChannels(); len(chans) == 1 {
			chID = chans[0].ID
		}
	}
	if chID == "" {
		t.Fatal("channel never created")
	}
	if _, err := h.Send(NewTextEnvelope(chID, a, "", "too early")); !errors.Is(err, ErrProtocol) {
		t.Fatalf("send on pending channel: %v", err)
	}
	// Ack to unblock the creator (b is the invitee — participants[1]).
	pending := h.ListChannels()[0]
	invites, _ := h.ReadWAL(pending.ID)
	for _, env := range invites {
		if env.EventType == EnvChannelInvite && env.Audience == b {
			_, _ = h.Send(NewChannelAck(env, b, false, ""))
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("create after ack: %v", err)
	}
	waitForChannel(t, h, chID, ChannelActive)
}

// TestWALReplay runs the same scenario over the WAL backends.
func TestWALReplay(t *testing.T) {
	t.Run("memory", func(t *testing.T) { testWALReplay(t, NewMemoryWAL()) })
	t.Run("file", func(t *testing.T) {
		testWALReplay(t, NewFileWAL(filepath.Join(t.TempDir(), "wal.jsonl")))
	})
	t.Run("knowledge", func(t *testing.T) {
		// The brief-pinned backend: knowledge.EventLogWriter over a memory
		// store, records as DataInput events.
		w := knowledge.NewEventLogWriter(knowledge.NewMemoryStore())
		testWALReplay(t, NewKnowledgeWAL(w, "network-hub"))
	})
}

func testWALReplay(t *testing.T, wal WAL) {
	t.Helper()
	// Hub 1: run a full consulting exchange.
	h1 := newTestHub(t, WithHubWAL(wal))
	a, b := registerTwo(t, h1)
	rec := newRecorder()
	rec.attach(h1, a, true)
	rec.attach(h1, b, true)
	ch, err := h1.CreateChannel(ChannelManifest{Type: TypeConsulting}, []string{a, b})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := h1.Send(NewTextEnvelope(ch.ID, a, "", "q")); err != nil {
		t.Fatalf("send q: %v", err)
	}
	if _, err := h1.Send(NewTextEnvelope(ch.ID, b, "", "a")); err != nil {
		t.Fatalf("send a: %v", err)
	}
	waitForChannel(t, h1, ch.ID, ChannelClosed)
	original, _ := h1.ReadWAL(ch.ID)

	// Hub 2: replay the same WAL; adapters must be registered first.
	h2 := NewHub(WithHubWAL(wal))
	h2.RegisterAdapter(ConsultingAdapter{}, "1")
	if err := h2.Replay(); err != nil {
		t.Fatalf("replay: %v", err)
	}
	replayed, err := h2.Channel(ch.ID)
	if err != nil {
		t.Fatalf("replayed channel missing: %v", err)
	}
	if replayed.State != ChannelClosed || replayed.CloseReason != "consulting_complete" {
		t.Fatalf("replayed state: %s %q", replayed.State, replayed.CloseReason)
	}
	if replayed.CreatorID != a || len(replayed.Participants) != 2 {
		t.Fatalf("replayed participants: %+v", replayed.Participants)
	}
	// The adapter state was re-folded from the WAL.
	st, ok := replayed.AdapterState.(*consultingState)
	if !ok || !st.InitiatorSent || !st.RespondentReplied {
		t.Fatalf("replayed adapter state: %+v", replayed.AdapterState)
	}
	got, err := h2.ReadWAL(ch.ID)
	if err != nil {
		t.Fatalf("read replayed wal: %v", err)
	}
	if len(got) != len(original) {
		t.Fatalf("replayed log length %d, want %d", len(got), len(original))
	}
	for i := range got {
		if got[i].ID != original[i].ID {
			t.Fatalf("replayed log[%d] id mismatch: %s vs %s", i, got[i].ID, original[i].ID)
		}
	}
	// Replay restores channel state, not identities: hub 2 has no
	// passports, so a send from a hub-1 id fails at the sender lookup
	// (identities re-register with fresh ids in a real restart).
	if _, err := h2.Send(NewTextEnvelope(ch.ID, a, "", "zombie")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("send on replayed hub: %v", err)
	}
}
