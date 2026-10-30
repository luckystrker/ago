package network

import (
	"errors"
	"testing"
	"time"
)

// sweepHub wires a hub with a controllable clock and a slice audit sink.
func sweepHub(t *testing.T, clock *fakeClock) (*Hub, *SliceAuditLog) {
	t.Helper()
	audit := NewSliceAuditLog()
	h := newTestHub(t, WithHubClock(clock.Now), WithAuditSink(audit))
	return h, audit
}

func sweepCount(h *Hub, channelID string, eventType string) int {
	envs, err := h.ReadWAL(channelID)
	if err != nil {
		return 0
	}
	n := 0
	for _, env := range envs {
		if env.EventType == eventType {
			n++
		}
	}
	return n
}

func TestAcksWithinViolation(t *testing.T) {
	clock := newFakeClock()
	h, audit := sweepHub(t, clock)
	hc := NewHubClient(h)

	alice, _ := hc.Register("alice")
	defer alice.Close()
	// The invitee never acks (auto-ack off): the create blocks in the
	// handshake while the channel stays pending.
	slow, _ := hc.Register("slow", WithAutoAck(false))
	defer slow.Close()

	done := make(chan error, 1)
	go func() {
		_, err := alice.CreateChannel(ChannelManifest{
			Type: TypeConversation,
			Expectations: []Expectation{{
				Kind:        "acks_within",
				OnViolation: "notify_channel",
				Params:      map[string]any{"seconds": float64(30)},
			}},
		}, slow.AgentID())
		done <- err
	}()

	deadline := time.Now().Add(2 * time.Second)
	var channelID string
	for time.Now().Before(deadline) {
		h.mu.Lock()
		for id, ch := range h.channels {
			if ch.State == ChannelPending {
				channelID = id
			}
		}
		h.mu.Unlock()
		if channelID != "" {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if channelID == "" {
		t.Fatal("pending channel never appeared")
	}

	// Before the threshold: no fire.
	h.SweepExpectations()
	if entries := audit.Entries(); len(entries) != 0 {
		t.Fatalf("no fire expected below threshold, got %v", entries)
	}

	// Past the threshold: one fire naming the pending invitee. Python
	// parity: the notify_channel handler cannot post a substantive
	// envelope to a pending channel (post_envelope requires ACTIVE and the
	// failure is suppressed), so the fire is visible only through audit.
	clock.Advance(31 * time.Second)
	h.SweepExpectations()
	if n := sweepCount(h, channelID, EnvExpectationViolated); n != 0 {
		t.Fatalf("violated envelope cannot ride a pending channel, got %d", n)
	}
	entries := audit.Entries()
	if len(entries) != 1 || entries[0].Kind != AuditExpectationViolated {
		t.Fatalf("audit entries %v", entries)
	}
	if entries[0].Expectation != "acks_within" ||
		len(entries[0].Violators) != 1 || entries[0].Violators[0] != slow.AgentID() {
		t.Fatalf("audit entry %+v, want acks_within/[%s]", entries[0], slow.AgentID())
	}

	// Dedup: a second tick must not re-fire.
	h.SweepExpectations()
	if entries := audit.Entries(); len(entries) != 1 {
		t.Fatalf("audit re-recorded the fire (%d)", len(entries))
	}

	// Let the handshake time out so the create goroutine finishes.
	<-waitChannelTerminal(t, h, channelID)
	<-done
}

// waitChannelTerminal blocks until the channel reaches a terminal state or
// the deadline, then closes (unblocking waiters is the caller's `done`).
func waitChannelTerminal(t *testing.T, h *Hub, channelID string) chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(6 * time.Second)
		for time.Now().Before(deadline) {
			ch, err := h.Channel(channelID)
			if err == nil && ch.State.Terminal() {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	return done
}

func TestReplyWithinViolation(t *testing.T) {
	clock := newFakeClock()
	h, audit := sweepHub(t, clock)
	hc := NewHubClient(h)
	alice, _ := hc.Register("alice")
	defer alice.Close()
	bob, _ := hc.Register("bob")
	defer bob.Close()

	ch, err := alice.CreateChannel(ChannelManifest{
		Type: TypeConversation,
		Expectations: []Expectation{{
			Kind:        "reply_within",
			OnViolation: "notify_channel",
			Params:      map[string]any{"seconds": float64(600)},
		}},
	}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}

	// Alice addresses Bob; Bob never replies.
	if _, err := h.Send(NewTextEnvelope(ch.ID, alice.AgentID(), "", "bob, please respond")); err != nil {
		t.Fatal(err)
	}

	// Below the threshold nothing fires.
	h.SweepExpectations()
	if n := sweepCount(h, ch.ID, EnvExpectationViolated); n != 0 {
		t.Fatalf("no violation expected below threshold, got %d", n)
	}

	clock.Advance(601 * time.Second)
	h.SweepExpectations()

	envs, _ := h.ReadWAL(ch.ID)
	var viol Envelope
	for _, env := range envs {
		if env.EventType == EnvExpectationViolated {
			viol = env
		}
	}
	if viol.ID == "" {
		t.Fatal("reply_within never fired")
	}
	var payload struct {
		Expectation string   `json:"expectation"`
		Violators   []string `json:"violators"`
	}
	if err := viol.Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Expectation != "reply_within" {
		t.Fatalf("expectation %q", payload.Expectation)
	}
	if len(payload.Violators) != 1 || payload.Violators[0] != bob.AgentID() {
		t.Fatalf("violators %v, want [bob]", payload.Violators)
	}
	if entries := audit.Entries(); len(entries) != 1 {
		t.Fatalf("audit entries %v, want 1", entries)
	}

	// Dedup on the second tick.
	h.SweepExpectations()
	if entries := audit.Entries(); len(entries) != 1 {
		t.Fatalf("audit re-recorded the fire (%d)", len(entries))
	}
}

func TestReplyWithinSatisfiedByReply(t *testing.T) {
	clock := newFakeClock()
	h, audit := sweepHub(t, clock)
	hc := NewHubClient(h)
	alice, _ := hc.Register("alice")
	defer alice.Close()
	bob, _ := hc.Register("bob")
	defer bob.Close()

	ch, err := alice.CreateChannel(ChannelManifest{
		Type: TypeConversation,
		Expectations: []Expectation{{
			Kind:        "reply_within",
			OnViolation: "notify_channel",
			Params:      map[string]any{"seconds": float64(600)},
		}},
	}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = h.Send(NewTextEnvelope(ch.ID, alice.AgentID(), "", "question"))
	_, _ = h.Send(NewTextEnvelope(ch.ID, bob.AgentID(), "", "answer"))

	clock.Advance(2 * time.Hour)
	h.SweepExpectations()
	if n := sweepCount(h, ch.ID, EnvExpectationViolated); n != 0 {
		t.Fatalf("a satisfied reply expectation must not fire, got %d", n)
	}
	if entries := audit.Entries(); len(entries) != 0 {
		t.Fatalf("audit saw phantom fires: %v", entries)
	}
}

func TestMaxSilenceAutoClose(t *testing.T) {
	clock := newFakeClock()
	h, audit := sweepHub(t, clock)
	hc := NewHubClient(h)
	alice, _ := hc.Register("alice")
	defer alice.Close()
	bob, _ := hc.Register("bob")
	defer bob.Close()

	ch, err := alice.CreateChannel(ChannelManifest{
		Type: TypeConversation,
		Expectations: []Expectation{{
			Kind:        "max_silence",
			OnViolation: "auto_close",
			Params:      map[string]any{"seconds": float64(3600)},
		}},
	}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}

	clock.Advance(2 * time.Hour)
	h.SweepExpectations()

	got, err := h.Channel(ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != ChannelClosed {
		t.Fatalf("channel state %s, want closed", got.State)
	}
	if got.CloseReason != "expectation_violated:max_silence" {
		t.Fatalf("close reason %q", got.CloseReason)
	}
	// The violation itself is not on the WAL (auto_close sends no envelope),
	// but the audit sink saw the fire.
	if entries := audit.Entries(); len(entries) != 1 || entries[0].Expectation != "max_silence" {
		t.Fatalf("audit entries %v", entries)
	}
	// Closed channels leave the sweep set.
	h.SweepExpectations()
	if entries := audit.Entries(); len(entries) != 1 {
		t.Fatalf("terminal channel was swept again: %v", entries)
	}
}

func TestUnknownOnViolationIgnored(t *testing.T) {
	clock := newFakeClock()
	h, audit := sweepHub(t, clock)
	hc := NewHubClient(h)
	alice, _ := hc.Register("alice")
	defer alice.Close()
	bob, _ := hc.Register("bob")
	defer bob.Close()

	ch, err := alice.CreateChannel(ChannelManifest{
		Type: TypeConversation,
		Expectations: []Expectation{{
			Kind:        "max_silence",
			OnViolation: "shout_into_the_void", // not a built-in handler
			Params:      map[string]any{"seconds": float64(1)},
		}},
	}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Hour)
	h.SweepExpectations()
	if n := sweepCount(h, ch.ID, EnvExpectationViolated); n != 0 {
		t.Fatalf("unknown on_violation must be silently ignored, got %d envelopes", n)
	}
	if got, _ := h.Channel(ch.ID); got.State != ChannelActive {
		t.Fatalf("unknown on_violation must not close the channel (state=%s)", got.State)
	}
	if entries := audit.Entries(); len(entries) != 0 {
		// Python parity: the handler lookup happens before the listener
		// fan-out, so an unknown on_violation value produces no audit fire.
		t.Fatalf("unknown on_violation must not reach audit: %v", entries)
	}
}

func TestSweeperIntervalLoop(t *testing.T) {
	clock := newFakeClock()
	h := newTestHub(t, WithHubClock(clock.Now))
	hc := NewHubClient(h)
	alice, _ := hc.Register("alice")
	defer alice.Close()
	bob, _ := hc.Register("bob")
	defer bob.Close()

	ch, err := alice.CreateChannel(ChannelManifest{
		Type: TypeConversation,
		Expectations: []Expectation{{
			Kind:        "max_silence",
			OnViolation: "auto_close",
			Params:      map[string]any{"seconds": float64(0.05)}, // 50ms
		}},
	}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}

	// A real-time sweeper ticking every 20ms evaluates with the fake clock;
	// advance it once and wait for the loop to notice.
	s := NewExpectationSweeper(h, 20*time.Millisecond)
	s.Start()
	defer s.Stop()
	clock.Advance(time.Hour)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got, err := h.Channel(ch.ID); err == nil && got.State == ChannelClosed {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("sweeper loop never closed the silent channel")
}

func TestAuditSinkFailureSwallowed(t *testing.T) {
	clock := newFakeClock()
	h := newTestHub(t, WithHubClock(clock.Now), WithAuditSink(failAudit{}))
	hc := NewHubClient(h)
	alice, _ := hc.Register("alice")
	defer alice.Close()
	bob, _ := hc.Register("bob")
	defer bob.Close()

	ch, err := alice.CreateChannel(ChannelManifest{
		Type: TypeConversation,
		Expectations: []Expectation{{
			Kind:        "max_silence",
			OnViolation: "auto_close",
		}},
	}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Hour)
	h.SweepExpectations()
	if got, err := h.Channel(ch.ID); err != nil || got.State != ChannelClosed {
		t.Fatalf("a failing audit sink must not break the sweeper (state=%v err=%v)", got.State, err)
	}
}

type failAudit struct{}

func (failAudit) Append(entry AuditEntry) error { return errAuditFailed }

var errAuditFailed = errors.New("audit backend down")
