package network

import (
	"encoding/json"
	"errors"
	"testing"
)

// jsonMap round-trips a dict through JSON so the test sees what a real
// knobs payload looks like (numbers as float64, arrays as []any).
func jsonMap(t *testing.T, d map[string]any) map[string]any {
	t.Helper()
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func ctxFor(env Envelope, order []string, last string) *TurnContext {
	creator := ""
	if len(order) > 0 {
		creator = order[0]
	}
	return &TurnContext{
		Envelope:         env,
		ParticipantOrder: order,
		LastSpeakerID:    last,
		CreatorID:        creator,
		ContextVars:      map[string]any{},
	}
}

func TestAgentTarget(t *testing.T) {
	next, err := AgentTarget{AgentID: "bob"}.Next(ctxFor(Envelope{}, []string{"alice"}, "alice"))
	if err != nil || next != "bob" {
		t.Fatalf("AgentTarget: %q %v", next, err)
	}
}

func TestRoundRobinTarget(t *testing.T) {
	order := []string{"a", "b", "c"}
	ctx := ctxFor(Envelope{SenderID: "a"}, order, "")
	rr := &RoundRobinTarget{}
	// last speaker unknown → falls back to the envelope sender
	next, _ := rr.Next(ctx)
	if next != "b" {
		t.Fatalf("after a: want b, got %q", next)
	}
	ctx.LastSpeakerID = "c"
	next, _ = rr.Next(ctx)
	if next != "a" {
		t.Fatalf("after c: wrap to a, got %q", next)
	}
	// unknown anchor → start from the head
	ctx.LastSpeakerID = "zzz"
	next, _ = rr.Next(ctx)
	if next != "a" {
		t.Fatalf("unknown anchor: want a, got %q", next)
	}
	// explicit Agents list overrides participant order
	rr2 := &RoundRobinTarget{Agents: []string{"x", "y"}}
	ctx2 := ctxFor(Envelope{SenderID: "x"}, order, "")
	next, _ = rr2.Next(ctx2)
	if next != "y" {
		t.Fatalf("explicit list: want y, got %q", next)
	}
	// empty order terminates
	ctx3 := ctxFor(Envelope{}, nil, "")
	ctx3.CloseReason = ""
	next, _ = (&RoundRobinTarget{}).Next(ctx3)
	if next != "" || ctx3.CloseReason != "no_participants" {
		t.Fatalf("empty order: %q %q", next, ctx3.CloseReason)
	}
}

func TestStayAndRevertTargets(t *testing.T) {
	ctx := ctxFor(Envelope{SenderID: "b"}, []string{"a", "b"}, "b")
	next, _ := (StayTarget{}).Next(ctx)
	if next != "b" {
		t.Fatalf("stay: %q", next)
	}
	next, _ = (RevertToInitiatorTarget{}).Next(ctx)
	if next != "a" {
		t.Fatalf("revert: %q", next)
	}
}

func TestTerminateTarget(t *testing.T) {
	ctx := ctxFor(Envelope{}, []string{"a"}, "")
	next, _ := (TerminateTarget{Reason: "after_work"}).Next(ctx)
	if next != "" || ctx.CloseReason != "after_work" {
		t.Fatalf("terminate: %q %q", next, ctx.CloseReason)
	}
}

func TestConditions(t *testing.T) {
	ctx := ctxFor(Envelope{SenderID: "a", EventType: EnvText}, []string{"a"}, "")
	var always Always
	if !always.Match(ctx) {
		t.Error("always must fire")
	}
	fromA := FromSpeaker{AgentID: "a"}
	fromB := FromSpeaker{AgentID: "b"}
	if !fromA.Match(ctx) || fromB.Match(ctx) {
		t.Error("from_speaker mismatch")
	}
	route := ToolCalled{Name: "route"}
	other := ToolCalled{Name: "other"}
	if route.Match(ctx) {
		t.Error("tool_called must not fire on text")
	}
	packet := NewPacketEnvelope("ch", "a", "", "body", WithPacketRouting(map[string]any{"kind": "text", "tool": "route"}))
	ctxP := ctxFor(packet, []string{"a"}, "")
	if !route.Match(ctxP) || other.Match(ctxP) {
		t.Error("tool_called packet match broken")
	}
}

func TestGraphSelect(t *testing.T) {
	g := TransitionGraph{
		Initial: "a",
		Transitions: []Transition{
			{When: FromSpeaker{AgentID: "b"}, To: AgentTarget{AgentID: "a"}},
			{When: Always{}, To: &RoundRobinTarget{}},
		},
	}
	ctx := ctxFor(Envelope{SenderID: "a"}, []string{"a", "b"}, "")
	if r := g.Select(ctx); r.NextSender != "b" {
		t.Fatalf("first match: %+v", r)
	}
	// no match → terminate with the fall-through reason
	g2 := TransitionGraph{Initial: "a", Transitions: []Transition{
		{When: FromSpeaker{AgentID: "zzz"}, To: AgentTarget{AgentID: "b"}},
	}}
	ctx2 := ctxFor(Envelope{SenderID: "a"}, []string{"a"}, "")
	r := g2.Select(ctx2)
	if !r.Close || r.CloseReason != "no_transition" {
		t.Fatalf("fall-through: %+v", r)
	}
	// terminate target wins over later transitions
	g3 := TransitionGraph{Initial: "a", Transitions: []Transition{
		{When: Always{}, To: TerminateTarget{Reason: "done"}},
		{When: Always{}, To: AgentTarget{AgentID: "b"}},
	}}
	r = g3.Select(ctxFor(Envelope{SenderID: "a"}, []string{"a"}, ""))
	if !r.Close || r.CloseReason != "done" {
		t.Fatalf("terminate: %+v", r)
	}
}

func TestGraphDictRoundTrip(t *testing.T) {
	g := TransitionGraph{
		Initial: "a",
		Transitions: []Transition{
			{When: FromSpeaker{AgentID: "a"}, To: AgentTarget{AgentID: "b"}},
			{When: ToolCalled{Name: "route"}, To: &RoundRobinTarget{Agents: []string{"a", "b"}}},
			{When: Always{}, To: RevertToInitiatorTarget{}},
		},
		MaxTurns: 5,
	}
	d := g.ToDict()
	back, err := GraphFromDict(d)
	if err != nil {
		t.Fatalf("GraphFromDict: %v", err)
	}
	if back.Initial != "a" || back.MaxTurns != 5 || len(back.Transitions) != 3 {
		t.Fatalf("round trip mismatch: %+v", back)
	}
	if back.Transitions[0].When.(FromSpeaker).AgentID != "a" {
		t.Fatal("condition 0 broken")
	}
	if back.Transitions[1].When.(ToolCalled).Name != "route" {
		t.Fatal("condition 1 broken")
	}
	if back.Transitions[1].To.(*RoundRobinTarget).Agents[1] != "b" {
		t.Fatal("target 1 broken")
	}
	// The dict survives a JSON round trip (knobs arrive as generic maps).
	repaired, err := GraphFromDict(jsonMap(t, d))
	if err != nil {
		t.Fatalf("GraphFromDict(json): %v", err)
	}
	if repaired.MaxTurns != 5 || len(repaired.Transitions) != 3 {
		t.Fatalf("json round trip: %+v", repaired)
	}
}

func TestGraphFromDictErrors(t *testing.T) {
	if _, err := GraphFromDict(nil); err == nil {
		t.Fatal("nil dict must error")
	}
	if _, err := GraphFromDict(map[string]any{}); err == nil {
		t.Fatal("missing initial must error")
	}
	if _, err := GraphFromDict(map[string]any{
		"initial":     "a",
		"transitions": []any{map[string]any{"when": map[string]any{"name": "wat"}}},
	}); err == nil {
		t.Fatal("unknown condition must error")
	}
	if _, err := GraphFromDict(map[string]any{
		"initial":     "a",
		"transitions": []any{map[string]any{"when": map[string]any{}, "to": map[string]any{"name": "wat"}}},
	}); err == nil {
		t.Fatal("unknown target must error")
	}
}

func TestNewRoundRobinGraph(t *testing.T) {
	if _, err := NewRoundRobinGraph(nil, 0); !errors.Is(err, ErrProtocol) {
		t.Fatalf("empty participants: %v", err)
	}
	h := newTestHub(t)
	ids := registerAgents(t, h, "alice", "bob", "carol")
	graph, err := NewRoundRobinGraph(ids, 5)
	if err != nil {
		t.Fatal(err)
	}
	if graph.Initial != ids[0] || graph.MaxTurns != 5 {
		t.Fatalf("graph: %+v", graph)
	}
	// The builder output must survive the knob round trip.
	if _, err := GraphFromDict(graph.ToDict()); err != nil {
		t.Fatalf("ToDict/GraphFromDict: %v", err)
	}

	ch := openWorkflow(t, h, graph, ids)
	// Speaking order follows the join order, wrapping around, until MaxTurns.
	for turn, speaker := range []string{ids[0], ids[1], ids[2], ids[0], ids[1]} {
		if _, err := h.Send(NewTextEnvelope(ch, speaker, "", "turn")); err != nil {
			t.Fatalf("turn %d by %s: %v", turn, speaker, err)
		}
	}
	got, _ := h.Channel(ch)
	if got.State != ChannelClosed || got.CloseReason != "max_turns" {
		t.Fatalf("after 5 turns: %s %q", got.State, got.CloseReason)
	}
}

func TestNewRoundRobinGraphRejectsOutOfTurn(t *testing.T) {
	h := newTestHub(t)
	ids := registerAgents(t, h, "alice", "bob")
	graph, _ := NewRoundRobinGraph(ids, 0)
	ch := openWorkflow(t, h, graph, ids)
	if _, err := h.Send(NewTextEnvelope(ch, ids[1], "", "jumping the queue")); !errors.Is(err, ErrProtocol) {
		t.Fatalf("only the initial speaker may open: %v", err)
	}
}

func TestNewSequenceGraph(t *testing.T) {
	if _, err := NewSequenceGraph(nil); !errors.Is(err, ErrProtocol) {
		t.Fatalf("empty steps: %v", err)
	}
	h := newTestHub(t)
	ids := registerAgents(t, h, "alice", "bob", "carol")
	graph, err := NewSequenceGraph(ids)
	if err != nil {
		t.Fatal(err)
	}
	if graph.Initial != ids[0] || graph.MaxTurns != 3 {
		t.Fatalf("graph: %+v", graph)
	}
	if _, err := GraphFromDict(graph.ToDict()); err != nil {
		t.Fatalf("ToDict/GraphFromDict: %v", err)
	}

	ch := openWorkflow(t, h, graph, ids)
	// The pipeline runs strictly in step order...
	if _, err := h.Send(NewTextEnvelope(ch, ids[2], "", "skip ahead")); !errors.Is(err, ErrProtocol) {
		t.Fatalf("step 3 before step 1: %v", err)
	}
	for _, speaker := range ids {
		if got, _ := h.Channel(ch); got.State != ChannelActive {
			t.Fatalf("closed before %s spoke: %s", speaker, got.State)
		}
		if _, err := h.Send(NewTextEnvelope(ch, speaker, "", "done")); err != nil {
			t.Fatalf("%s: %v", speaker, err)
		}
	}
	// ...and ends with its own close reason after the last step.
	got, _ := h.Channel(ch)
	if got.State != ChannelClosed || got.CloseReason != "sequence_complete" {
		t.Fatalf("after the last step: %s %q", got.State, got.CloseReason)
	}
}

func TestNewSequenceGraphSingleStep(t *testing.T) {
	h := newTestHub(t)
	ids := registerAgents(t, h, "alice", "bob")
	graph, _ := NewSequenceGraph(ids[:1])
	ch := openWorkflow(t, h, graph, ids)
	if _, err := h.Send(NewTextEnvelope(ch, ids[0], "", "only step")); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.Channel(ch); got.State != ChannelClosed || got.CloseReason != "sequence_complete" {
		t.Fatalf("single step: %s %q", got.State, got.CloseReason)
	}
}

// registerAgents registers one auto-acking raw-notifier agent per name and
// returns their ids in order.
func registerAgents(t *testing.T, h *Hub, names ...string) []string {
	t.Helper()
	rec := newRecorder()
	ids := make([]string, len(names))
	for i, name := range names {
		p, err := h.Register(Passport{Name: name}, nil)
		if err != nil {
			t.Fatal(err)
		}
		rec.attach(h, p.AgentID, true)
		ids[i] = p.AgentID
	}
	return ids
}

func openWorkflow(t *testing.T, h *Hub, graph TransitionGraph, ids []string) string {
	t.Helper()
	ch, err := h.CreateChannel(ChannelManifest{Type: TypeWorkflow, Knobs: map[string]any{"graph": graph.ToDict()}}, ids)
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	return ch.ID
}
