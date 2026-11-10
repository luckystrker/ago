package network

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luckystrker/ago/task"
)

// recListener records every hook it receives, by hook name and detail.
type recListener struct {
	BaseHubListener
	mu     sync.Mutex
	hooks  []string
	detail map[string][]string
}

func newRecListener() *recListener { return &recListener{detail: map[string][]string{}} }

func (r *recListener) note(hook, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hooks = append(r.hooks, hook)
	r.detail[hook] = append(r.detail[hook], detail)
}

func (r *recListener) seen(hook string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.detail[hook]...)
}

func (r *recListener) OnEnvelopePosted(env Envelope, ch *ChannelHandle) {
	r.note("posted", env.EventType+"@"+string(ch.State))
}
func (r *recListener) OnEnvelopeRejected(env Envelope, reason error) {
	r.note("rejected", reason.Error())
}
func (r *recListener) OnDispatchFailed(env Envelope, recipient string, reason error) {
	r.note("dispatch_failed", recipient)
}
func (r *recListener) OnChannelEvent(id, kind string, p map[string]any) { r.note("channel", kind) }
func (r *recListener) OnAgentEvent(id, kind string, p map[string]any)   { r.note("agent", kind) }
func (r *recListener) OnExpectationFired(ch string, e Expectation, v Violation) {
	r.note("expectation", e.Kind)
}
func (r *recListener) OnTurnFailed(ch, agent, env string, err error) {
	r.note("turn_failed", err.Error())
}
func (r *recListener) OnTaskEvent(id, kind string, p map[string]any) { r.note("task", kind) }
func (r *recListener) OnInboxPressure(agent string, pending, max int) {
	r.note("pressure", agent)
}

// The scripted scenario drives every one of the nine hooks.
func TestHubListenerSeesAllNineHooks(t *testing.T) {
	clock := newFakeClock()
	h := newTestHub(t, WithHubClock(clock.Now))
	rec := newRecListener()
	h.RegisterListener(rec)

	alice, err := h.Register(Passport{Name: "alice"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	bobRule := DefaultRule()
	bobRule.Inbox = InboxBlock{MaxPending: 4, HighWater: 1}
	bob, err := h.Register(Passport{Name: "bob"}, &bobRule)
	if err != nil {
		t.Fatal(err)
	}
	_ = h.Attach(alice.AgentID, func(env Envelope) {
		if env.EventType == EnvChannelInvite {
			_, _ = h.Send(NewChannelAck(env, alice.AgentID, false, ""))
		}
	})
	_ = h.Attach(bob.AgentID, func(env Envelope) {
		if env.EventType == EnvChannelInvite {
			_, _ = h.Send(NewChannelAck(env, bob.AgentID, false, ""))
		}
		if env.EventType == EnvText && env.Text() == "boom" {
			panic("handler exploded")
		}
	})

	ch, err := h.CreateChannel(ChannelManifest{
		Type:         TypeConversation,
		Expectations: []Expectation{{Kind: "max_silence", OnViolation: "audit", Params: map[string]any{"seconds": 10}}},
	}, []string{alice.AgentID, bob.AgentID})
	if err != nil {
		t.Fatal(err)
	}
	if err := say(h, ch.ID, alice.AgentID, "hi"); err != nil { // posted + pressure
		t.Fatal(err)
	}
	if err := say(h, ch.ID, alice.AgentID, "boom"); err != nil { // dispatch failed, send still succeeds
		t.Fatalf("a panicking recipient must not fail the sender: %v", err)
	}
	if _, err := h.Send(NewTextEnvelope("no-such-channel", alice.AgentID, "", "x")); err == nil { // rejected
		t.Fatal("send to an unknown channel must fail")
	}
	clock.Advance(11 * time.Second)
	h.SweepExpectations() // expectation fired
	h.ReportTurnFailure(ch.ID, bob.AgentID, "env-1", errors.New("model exploded"))
	if err := h.ObserveTask(task.Metadata{TaskID: "t-1", OwnerID: alice.AgentID, State: task.StateRunning}); err != nil {
		t.Fatal(err)
	}
	if err := h.UpdateTask("t-1", task.StateCompleted, nil, "ok", ""); err != nil { // task event
		t.Fatal(err)
	}
	if err := h.SetResume(Resume{AgentID: alice.AgentID}); err != nil {
		t.Fatal(err)
	}
	if err := h.CloseChannel(ch.ID, "done"); err != nil {
		t.Fatal(err)
	}

	for _, hook := range []string{"posted", "rejected", "dispatch_failed", "channel", "agent", "expectation", "turn_failed", "task", "pressure"} {
		if len(rec.seen(hook)) == 0 {
			t.Errorf("hook %q never fired (all: %v)", hook, rec.hooks)
		}
	}
	if got := rec.seen("channel"); !reflect.DeepEqual(got, []string{"created", "opened", "closed"}) {
		t.Errorf("channel events: %v", got)
	}
	agents := rec.seen("agent")
	if !reflect.DeepEqual(agents[:2], []string{"registered", "registered"}) || agents[len(agents)-1] != "resume_set" {
		t.Errorf("agent events: %v", agents)
	}
	if got := rec.seen("dispatch_failed"); !reflect.DeepEqual(got, []string{bob.AgentID}) {
		t.Errorf("dispatch failure recipient: %v", got)
	}
	if got := rec.seen("pressure"); !reflect.DeepEqual(got, []string{bob.AgentID}) {
		t.Errorf("pressure: %v", got)
	}
	if got := rec.seen("task"); !reflect.DeepEqual(got, []string{"completed"}) {
		t.Errorf("task events: %v", got)
	}
	if got := rec.seen("expectation"); !reflect.DeepEqual(got, []string{"max_silence"}) {
		t.Errorf("expectation: %v", got)
	}
	if got := rec.seen("rejected"); len(got) != 1 || !strings.Contains(got[0], "not found") {
		t.Errorf("rejection reason: %v", got)
	}
}

type panickyListener struct{ BaseHubListener }

func (panickyListener) OnAgentEvent(string, string, map[string]any) { panic("listener bug") }

func TestHubListenersAreIsolatedAndOrdered(t *testing.T) {
	h := newTestHub(t)
	first, second := newRecListener(), newRecListener()
	h.RegisterListener(first)
	h.RegisterListener(panickyListener{})
	h.RegisterListener(second)
	if h.ListenerCount() != 3 {
		t.Fatalf("listener count: %d", h.ListenerCount())
	}
	// A panicking listener neither fails Register nor starves the next one.
	if _, err := h.Register(Passport{Name: "alice"}, nil); err != nil {
		t.Fatalf("register must survive a panicking listener: %v", err)
	}
	if len(first.seen("agent")) != 1 || len(second.seen("agent")) != 1 {
		t.Fatalf("listeners after the panicking one were skipped: %v %v", first.hooks, second.hooks)
	}
	h.UnregisterListener(first)
	h.UnregisterListener(first) // absent: no-op
	if h.ListenerCount() != 2 {
		t.Fatalf("after unregister: %d", h.ListenerCount())
	}
	_, _ = h.Register(Passport{Name: "bob"}, nil)
	if len(first.seen("agent")) != 1 || len(second.seen("agent")) != 2 {
		t.Fatalf("unregistered listener kept receiving: %v %v", first.hooks, second.hooks)
	}
}

// A listener may call back into the hub: events are delivered outside its lock.
type reentrantListener struct {
	BaseHubListener
	h    *Hub
	seen int
}

func (l *reentrantListener) OnAgentEvent(id, kind string, _ map[string]any) {
	if kind == AgentEventRegistered {
		_, _ = l.h.Passport(id)
		l.h.Registrations()
		l.seen++
	}
}

func TestHubListenerMayCallBackIntoHub(t *testing.T) {
	h := newTestHub(t)
	l := &reentrantListener{h: h}
	h.RegisterListener(l)
	done := make(chan struct{})
	go func() {
		_, _ = h.Register(Passport{Name: "alice"}, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("deadlock: listener ran under the hub lock")
	}
	if l.seen != 1 {
		t.Fatalf("seen: %d", l.seen)
	}
}

// --- AuditLog as a listener -------------------------------------------------------

func TestAuditLogRecordsListenerEvents(t *testing.T) {
	clock := newFakeClock()
	sink := NewSliceAuditLog()
	log := NewAuditLog(sink)
	h := newTestHub(t, WithHubClock(clock.Now), WithAuditLog(log))
	hc := NewHubClient(h)

	var live []AuditEntry
	var liveMu sync.Mutex
	unsub := log.Subscribe(func(e AuditEntry) {
		liveMu.Lock()
		live = append(live, e)
		liveMu.Unlock()
	})

	alice, err := hc.Register("alice", WithAgentFunc(func(string) (string, error) { return "", errors.New("model down") }))
	if err != nil {
		t.Fatal(err)
	}
	bob, _ := hc.Register("bob")
	defer func() { _ = alice.Close(); _ = bob.Close() }()
	ch, err := bob.CreateChannel(ChannelManifest{Type: TypeConversation, Expectations: []Expectation{}}, alice.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	if err := bob.Say(ch.ID, "hello"); err != nil { // alice's handler fails → turn_failed
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(kindsOf(sink.Entries(), AuditTurnFailed)) == 1 })
	_ = h.ObserveTask(task.Metadata{TaskID: "t-1", OwnerID: bob.AgentID(), ChannelID: ch.ID, State: task.StateRunning, Spec: task.Spec{Capability: "grind"}})
	_ = h.UpdateTask("t-1", task.StateFailed, nil, nil, "boom")
	_ = h.RecordObservation(bob.AgentID(), "grind", task.StateFailed, -1, "t-1")
	_ = h.SetRule(bob.AgentID(), DefaultRule())
	_ = h.CloseChannel(ch.ID, "done")
	_ = alice.Close()

	entries := sink.Entries()
	byKind := map[string][]AuditEntry{}
	for _, e := range entries {
		byKind[e.Kind] = append(byKind[e.Kind], e)
	}
	for _, kind := range []string{AuditAgentRegistered, AuditAgentUnregistered, AuditChannelCreated, AuditChannelClosed,
		AuditTaskTerminated, AuditTurnFailed, AuditResumeSet, AuditRuleSet} {
		if len(byKind[kind]) == 0 {
			t.Errorf("no %q record; kinds: %v", kind, entryKinds(entries))
		}
	}
	if len(byKind[AuditAgentRegistered]) != 2 || byKind[AuditAgentRegistered][0].Name != "alice" {
		t.Errorf("registrations: %+v", byKind[AuditAgentRegistered])
	}
	created := byKind[AuditChannelCreated][0]
	if created.ChannelID != ch.ID || created.ManifestType != TypeConversation || created.CreatorID != bob.AgentID() || len(created.Participants) != 2 {
		t.Errorf("channel_created: %+v", created)
	}
	if c := byKind[AuditChannelClosed][0]; c.Reason != "done" {
		t.Errorf("channel_closed: %+v", c)
	}
	task1 := byKind[AuditTaskTerminated][0]
	if task1.TaskID != "t-1" || task1.OwnerID != bob.AgentID() || task1.Outcome != "failed" || task1.Capability != "grind" || task1.Reason != "boom" {
		t.Errorf("task_terminated: %+v", task1)
	}
	if tf := byKind[AuditTurnFailed][0]; tf.AgentID != alice.AgentID() || tf.ExcMessage != "model down" || tf.ChannelID != ch.ID {
		t.Errorf("turn_failed: %+v", tf)
	}
	var observed bool
	for _, e := range byKind[AuditResumeSet] {
		if e.Source == AuditSourceObserved && e.Capability == "grind" && e.Outcome == "failed" {
			observed = true
		}
	}
	if !observed {
		t.Errorf("observation resume_set missing: %+v", byKind[AuditResumeSet])
	}
	// Event time follows the hub clock.
	if !byKind[AuditAgentRegistered][0].At.Equal(clock.Now()) {
		t.Errorf("record time %v, hub clock %v", byKind[AuditAgentRegistered][0].At, clock.Now())
	}

	// Live subscribers saw the same stream; after unsubscribe they stop.
	liveMu.Lock()
	n := len(live)
	liveMu.Unlock()
	if n != len(entries) {
		t.Errorf("subscriber saw %d of %d entries", n, len(entries))
	}
	unsub()
	_, _ = h.Register(Passport{Name: "carol"}, nil)
	liveMu.Lock()
	after := len(live)
	liveMu.Unlock()
	if after != n {
		t.Errorf("unsubscribed callback still fired")
	}

	// The byte counter is the JSON bytes appended (newline included).
	var want int64
	for _, e := range sink.Entries() {
		line, _ := json.Marshal(e)
		want += int64(len(line)) + 1
	}
	if got := log.BytesWritten(); got != want || got == 0 {
		t.Errorf("bytes written %d, want %d", got, want)
	}
	if h.Health().AuditLogBytes != want {
		t.Errorf("health audit bytes %d, want %d", h.Health().AuditLogBytes, want)
	}
}

func entryKinds(es []AuditEntry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Kind
	}
	return out
}

func kindsOf(es []AuditEntry, kind string) []AuditEntry {
	var out []AuditEntry
	for _, e := range es {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never became true")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

type failingSink struct{}

func (failingSink) Append(AuditEntry) error { return errors.New("disk full") }

func TestAuditLogSurvivesSinkAndSubscriberFailures(t *testing.T) {
	log := NewAuditLog(failingSink{})
	h := newTestHub(t, WithAuditLog(log))
	if _, err := h.Register(Passport{Name: "alice"}, nil); err != nil {
		t.Fatalf("a failing audit sink must not break registration: %v", err)
	}
	if log.BytesWritten() != 0 {
		t.Fatal("failed appends must not count bytes")
	}

	ok := NewAuditLog(NewSliceAuditLog())
	called := 0
	ok.Subscribe(func(AuditEntry) { panic("bad tail") })
	ok.Subscribe(func(AuditEntry) { called++ })
	if err := ok.Append(AuditEntry{Kind: "custom"}); err != nil {
		t.Fatal(err)
	}
	if called != 1 {
		t.Fatalf("a panicking subscriber starved the next one: %d", called)
	}
}

func TestWithAuditSinkStaysExpectationOnly(t *testing.T) {
	sink := NewSliceAuditLog()
	h := newTestHub(t, WithAuditSink(sink))
	_, _ = registerTwo(t, h) // agent events would be audited by WithAuditLog
	if n := len(sink.Entries()); n != 0 {
		t.Fatalf("WithAuditSink must keep recording only expectation fires, got %v", sink.Entries())
	}
	if h.AuditLog() == nil || h.ListenerCount() != 1 {
		t.Fatalf("audit listener not registered")
	}
}

// Only terminal task events are audited; delivery problems and free-form
// tenant kinds are not task outcomes.
func TestAuditLogIgnoresNonTerminalTaskEvents(t *testing.T) {
	sink := NewSliceAuditLog()
	h := newTestHub(t, WithAuditLog(NewAuditLog(sink)))
	h.FireTaskEvent("t-1", TaskEventMirrorFailed, map[string]any{"op": "started"})
	h.FireTaskEvent("t-1", "progress", nil)
	if got := kindsOf(sink.Entries(), AuditTaskTerminated); len(got) != 0 {
		t.Fatalf("non-terminal task events were audited: %+v", got)
	}
	h.FireTaskEvent("t-2", "cancelled", map[string]any{"owner_id": "o", "reason": "stop"})
	got := kindsOf(sink.Entries(), AuditTaskTerminated)
	if len(got) != 1 || got[0].TaskID != "t-2" || got[0].Outcome != "cancelled" || got[0].Reason != "stop" {
		t.Fatalf("a terminal kind passed through FireTaskEvent: %+v", got)
	}
}

// Invite acks and rejects are absorbed by the hub: they never reach the
// posted fan-out (Python returns before it), every other accepted envelope does.
func TestEnvelopePostedSkipsInviteAcksAndRejects(t *testing.T) {
	h := newTestHub(t, WithAckTimeout(3*time.Second))
	rec := newRecListener()
	h.RegisterListener(rec)
	a, b := registerTwo(t, h)
	raw := newRecorder()
	raw.attach(h, a, true)
	raw.attach(h, b, true)
	ch, err := h.CreateChannel(ChannelManifest{Type: TypeConversation}, []string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	_ = say(h, ch.ID, a, "hi")
	_ = h.CloseChannel(ch.ID, "done")

	posted := rec.seen("posted")
	has := func(prefix string) bool {
		for _, p := range posted {
			if strings.HasPrefix(p, prefix) {
				return true
			}
		}
		return false
	}
	if !has(EnvChannelInvite) || !has(EnvChannelOpened) || !has(EnvText) {
		t.Fatalf("invite, opened and text must be posted: %v", posted)
	}
	if has(EnvChannelAck) {
		t.Fatalf("an invite ack reached the posted fan-out: %v", posted)
	}

	// A rejecting invitee.
	c, _ := h.Register(Passport{Name: "carol"}, nil)
	_ = h.Attach(c.AgentID, func(env Envelope) {
		if env.EventType == EnvChannelInvite {
			_, _ = h.Send(NewChannelAck(env, c.AgentID, true, "no thanks"))
		}
	})
	if _, err := h.CreateChannel(ChannelManifest{Type: TypeConversation}, []string{a, c.AgentID}); err == nil {
		t.Fatal("a rejected invite must fail the create")
	}
	for _, p := range rec.seen("posted") {
		if strings.HasPrefix(p, EnvChannelReject) {
			t.Fatalf("an invite reject reached the posted fan-out: %v", rec.seen("posted"))
		}
	}
}
