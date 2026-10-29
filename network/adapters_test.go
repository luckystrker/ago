package network

import (
	"errors"
	"testing"
	"time"
)

// The adapter protocol-shape gates, exercised through the hub state machine
// with raw notifiers (no client plumbing).

func TestConsultingAdapterGates(t *testing.T) {
	h := newTestHub(t)
	a, b := registerTwo(t, h)
	rec := newRecorder()
	rec.attach(h, a, true)
	rec.attach(h, b, true)
	ch, err := h.CreateChannel(ChannelManifest{Type: TypeConsulting}, []string{a, b})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Respondent cannot speak first.
	if _, err := h.Send(NewTextEnvelope(ch.ID, b, "", "early")); !errors.Is(err, ErrProtocol) {
		t.Fatalf("respondent first: %v", err)
	}
	// Unknown event types ride as informational data (no gate).
	if _, err := h.Send(Envelope{ChannelID: ch.ID, SenderID: a, EventType: "ag2.custom"}); err != nil {
		t.Fatalf("informational type: %v", err)
	}
	// Task envelopes bypass the 1Q1R gate.
	if _, err := h.Send(Envelope{ChannelID: ch.ID, SenderID: a, EventType: EnvTaskCancelled,
		Payload: mustJSON(map[string]any{"task_id": "t1", "reason": "x"})}); err != nil {
		t.Fatalf("task envelope: %v", err)
	}
	// The 1Q1R exchange still completes.
	if _, err := h.Send(NewTextEnvelope(ch.ID, a, "", "q")); err != nil {
		t.Fatalf("initiator text: %v", err)
	}
	// Now the initiator cannot speak twice.
	if _, err := h.Send(NewTextEnvelope(ch.ID, a, "", "q2")); !errors.Is(err, ErrProtocol) {
		t.Fatalf("initiator twice: %v", err)
	}
	if _, err := h.Send(NewTextEnvelope(ch.ID, b, "", "a")); err != nil {
		t.Fatalf("respondent text: %v", err)
	}
	done := waitForChannel(t, h, ch.ID, ChannelClosed)
	if done.CloseReason != "consulting_complete" {
		t.Fatalf("close reason: %q", done.CloseReason)
	}
}

func TestConversationAdapter(t *testing.T) {
	h := newTestHub(t)
	a, b := registerTwo(t, h)
	rec := newRecorder()
	rec.attach(h, a, true)
	rec.attach(h, b, true)
	ch, err := h.CreateChannel(ChannelManifest{Type: TypeConversation}, []string{a, b})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Either side, any number of times; the channel never auto-closes.
	for i := 0; i < 3; i++ {
		if _, err := h.Send(NewTextEnvelope(ch.ID, a, "", "ping")); err != nil {
			t.Fatalf("alice round %d: %v", i, err)
		}
		if _, err := h.Send(NewTextEnvelope(ch.ID, b, "", "pong")); err != nil {
			t.Fatalf("bob round %d: %v", i, err)
		}
	}
	got, _ := h.Channel(ch.ID)
	if got.State != ChannelActive {
		t.Fatalf("conversation auto-closed: %s %q", got.State, got.CloseReason)
	}
	st, ok := got.AdapterState.(*conversationState)
	if !ok || st.TurnCount != 6 || st.LastSpeakerID != b {
		t.Fatalf("folded state: %+v", got.AdapterState)
	}
	// Non-participants are rejected.
	c, err := h.Register(Passport{Name: "carol"}, nil)
	if err != nil {
		t.Fatalf("register carol: %v", err)
	}
	if _, err := h.Send(NewTextEnvelope(ch.ID, c.AgentID, "", "intrude")); !errors.Is(err, ErrProtocol) {
		t.Fatalf("non-participant: %v", err)
	}
}

func TestDiscussionAdapterRotation(t *testing.T) {
	h := newTestHub(t)
	a, _ := h.Register(Passport{Name: "alice"}, nil)
	b, _ := h.Register(Passport{Name: "bob"}, nil)
	c, _ := h.Register(Passport{Name: "carol"}, nil)
	rec := newRecorder()
	for _, id := range []string{a.AgentID, b.AgentID, c.AgentID} {
		rec.attach(h, id, true)
	}
	ch, err := h.CreateChannel(ChannelManifest{Type: TypeDiscussion}, []string{a.AgentID, b.AgentID, c.AgentID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	say := func(sender, text string) error {
		_, err := h.Send(NewTextEnvelope(ch.ID, sender, "", text))
		return err
	}
	// Creator speaks first; out-of-turn rejected.
	if err := say(b.AgentID, "early"); !errors.Is(err, ErrProtocol) {
		t.Fatalf("out of turn: %v", err)
	}
	if err := say(a.AgentID, "1"); err != nil {
		t.Fatalf("alice: %v", err)
	}
	if err := say(c.AgentID, "early too"); !errors.Is(err, ErrProtocol) {
		t.Fatalf("carol early: %v", err)
	}
	if err := say(b.AgentID, "2"); err != nil {
		t.Fatalf("bob: %v", err)
	}
	if err := say(c.AgentID, "3"); err != nil {
		t.Fatalf("carol: %v", err)
	}
	// Rotation wrapped back to the creator.
	if err := say(a.AgentID, "4"); err != nil {
		t.Fatalf("wrap: %v", err)
	}
	got, _ := h.Channel(ch.ID)
	st := got.AdapterState.(*discussionState)
	if st.TurnCount != 4 || st.Expected != b.AgentID || st.LastSpeakerID != a.AgentID {
		t.Fatalf("rotation state: %+v", st)
	}

	// Unsupported ordering knobs are rejected at create time.
	if _, err := h.CreateChannel(ChannelManifest{Type: TypeDiscussion,
		Knobs: map[string]any{"ordering": "chatbag"}}, []string{a.AgentID, b.AgentID}); !errors.Is(err, ErrProtocol) {
		t.Fatalf("bad ordering: %v", err)
	}
}

func TestWorkflowAdapterStaticRouting(t *testing.T) {
	h := newTestHub(t)
	a, b := registerTwo(t, h)
	rec := newRecorder()
	rec.attach(h, a, true)
	rec.attach(h, b, true)

	// ToolCalled("route") routes to bob; anything else terminates.
	graph := TransitionGraph{
		Initial: a,
		Transitions: []Transition{
			{When: ToolCalled{Name: "route"}, To: AgentTarget{AgentID: b}},
		},
	}
	ch, err := h.CreateChannel(ChannelManifest{
		Type:  TypeWorkflow,
		Knobs: map[string]any{"graph": graph.ToDict()},
	}, []string{a, b})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Static ToolCalled routing: kind "handoff" without a target falls
	// through to the graph.
	pkt := NewPacketEnvelope(ch.ID, a, "", "body",
		WithPacketRouting(map[string]any{"kind": "handoff", "tool": "route", "reason": "static"}))
	if _, err := h.Send(pkt); err != nil {
		t.Fatalf("route packet: %v", err)
	}
	got, _ := h.Channel(ch.ID)
	st := got.AdapterState.(*workflowState)
	if st.Expected != b {
		t.Fatalf("expected speaker after routing: %q", st.Expected)
	}

	// bob's plain packet carries no routing tool → no transition matches →
	// the graph fall-through terminates the channel.
	if _, err := h.Send(NewPacketEnvelope(ch.ID, b, "", "plain",
		WithPacketRouting(map[string]any{"kind": "text"}))); err != nil {
		t.Fatalf("bob packet: %v", err)
	}
	done := waitForChannel(t, h, ch.ID, ChannelClosed)
	if done.CloseReason != "no_transition" {
		t.Fatalf("fall-through close reason: %q", done.CloseReason)
	}
}

func TestWorkflowAdapterContext(t *testing.T) {
	h := newTestHub(t)
	a, b := registerTwo(t, h)
	rec := newRecorder()
	rec.attach(h, a, true)
	rec.attach(h, b, true)

	graph := TransitionGraph{
		Initial: a,
		Transitions: []Transition{
			{When: Always{}, To: &RoundRobinTarget{}},
		},
	}
	ch, err := h.CreateChannel(ChannelManifest{
		Type: TypeWorkflow,
		Knobs: map[string]any{
			"graph":        graph.ToDict(),
			"context_vars": map[string]any{"seed": "v0"},
		},
	}, []string{a, b})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Standalone context mutation.
	if _, err := h.Send(NewContextSetEnvelope(ch.ID, a, map[string]any{"k": "v1"}, "seed")); err != nil {
		t.Fatalf("context set: %v", err)
	}
	// Packet-carried context updates apply before speaker selection.
	pkt := NewPacketEnvelope(ch.ID, a, "", "round",
		WithPacketContext(map[string]any{"k": "v2"}, "gone"))
	if _, err := h.Send(pkt); err != nil {
		t.Fatalf("packet: %v", err)
	}
	got, _ := h.Channel(ch.ID)
	st := got.AdapterState.(*workflowState)
	if st.ContextVars["k"] != "v2" || len(st.ContextVars) != 1 {
		t.Fatalf("context vars: %+v", st.ContextVars)
	}
	if st.Expected != b || st.TurnCount != 1 {
		t.Fatalf("state after packet: expected=%q turns=%d", st.Expected, st.TurnCount)
	}
	_ = rec
}

func TestWorkflowAdapterCreateValidation(t *testing.T) {
	h := newTestHub(t)
	a, b := registerTwo(t, h)
	rec := newRecorder()
	rec.attach(h, a, true)
	rec.attach(h, b, true)

	// Missing graph knob.
	if _, err := h.CreateChannel(ChannelManifest{Type: TypeWorkflow}, []string{a, b}); !errors.Is(err, ErrProtocol) {
		t.Fatalf("missing graph: %v", err)
	}
	// Initial speaker not a participant.
	graph := TransitionGraph{Initial: "stranger", Transitions: []Transition{{When: Always{}, To: TerminateTarget{}}}}
	if _, err := h.CreateChannel(ChannelManifest{
		Type:  TypeWorkflow,
		Knobs: map[string]any{"graph": graph.ToDict()},
	}, []string{a, b}); !errors.Is(err, ErrProtocol) {
		t.Fatalf("stranger initial speaker: %v", err)
	}
	// Valid graph opens.
	graph.Initial = a
	ch, err := h.CreateChannel(ChannelManifest{
		Type:  TypeWorkflow,
		Knobs: map[string]any{"graph": graph.ToDict()},
	}, []string{a, b})
	if err != nil {
		t.Fatalf("valid create: %v", err)
	}
	st := ch.AdapterState.(*workflowState)
	if st.Expected != a || st.CreatorID != a {
		t.Fatalf("initial state: %+v", st)
	}
	_ = h.CloseChannel(ch.ID, "done")
	_ = time.Second
}
