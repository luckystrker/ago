package network

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// msgLog records the messages one agent's callback received.
type msgLog struct {
	mu   sync.Mutex
	msgs []string
}

func (l *msgLog) add(msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, msg)
}

func (l *msgLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.msgs...)
}

func waitIdle(t *testing.T, clients ...*AgentClient) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, c := range clients {
		if err := c.WaitIdle(ctx); err != nil {
			t.Fatalf("client %s never went idle: %v", c.AgentID(), err)
		}
	}
}

// TestWorkflowRoundRobinMaxTurns drives two agents through a workflow
// channel via the message callbacks; the MaxTurns cap terminates.
func TestWorkflowRoundRobinMaxTurns(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)

	aliceLog := &msgLog{}
	bobLog := &msgLog{}
	alice, err := hc.Register("alice", WithAgentFunc(func(msg string) (string, error) {
		aliceLog.add(msg)
		return "alice round", nil
	}))
	if err != nil {
		t.Fatalf("register alice: %v", err)
	}
	defer alice.Close()
	bob, err := hc.Register("bob", WithAgentFunc(func(msg string) (string, error) {
		bobLog.add(msg)
		return "bob round", nil
	}))
	if err != nil {
		t.Fatalf("register bob: %v", err)
	}
	defer bob.Close()

	graph := TransitionGraph{
		Initial:     alice.AgentID(),
		Transitions: []Transition{{When: Always{}, To: &RoundRobinTarget{}}},
		MaxTurns:    2,
	}
	ch, err := alice.CreateChannel(ChannelManifest{
		Type:  TypeWorkflow,
		Knobs: map[string]any{"graph": graph.ToDict()},
	}, bob.AgentID())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// The creator kicks off: her local round is captured as a packet.
	text, err := alice.Ask(ch.ID, "kickoff")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if text != "alice round" {
		t.Fatalf("ask text: %q", text)
	}

	waitIdle(t, alice, bob)
	done := waitForChannel(t, h, ch.ID, ChannelClosed)
	if done.CloseReason != "max_turns" {
		t.Fatalf("close reason: %q", done.CloseReason)
	}

	// alice saw the kickoff echo and bob's packet; bob saw alice's packet.
	if got := aliceLog.all(); len(got) != 2 || got[0] != "kickoff" || got[1] != "bob round" {
		t.Fatalf("alice log: %v", got)
	}
	if got := bobLog.all(); len(got) != 1 || got[0] != "alice round" {
		t.Fatalf("bob log: %v", got)
	}
}

// TestWorkflowHandoff: a tool-driven Handoff redirects the round to the
// named participant (Passport-name resolution) and the next speaker reads
// the handoff signal line.
func TestWorkflowHandoff(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)

	aliceLog := &msgLog{}
	bobLog := &msgLog{}
	alice, _ := hc.Register("alice", WithRoundFunc(func(Envelope) (Round, error) {
		return Round{Text: "redirecting", Handoff: &Handoff{Target: "bob", Reason: "load"}}, nil
	}))
	defer alice.Close()
	bob, _ := hc.Register("bob", WithAgentFunc(func(msg string) (string, error) {
		bobLog.add(msg)
		return "bob done", nil
	}))
	defer bob.Close()

	graph := TransitionGraph{
		Initial:     alice.AgentID(),
		Transitions: []Transition{{When: Always{}, To: &RoundRobinTarget{}}},
		MaxTurns:    2,
	}
	ch, err := alice.CreateChannel(ChannelManifest{
		Type:  TypeWorkflow,
		Knobs: map[string]any{"graph": graph.ToDict()},
	}, bob.AgentID())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// The seed round itself carries the handoff: alice's local round is
	// captured as a packet whose routing redirects to bob.
	if _, err := alice.Ask(ch.ID, "seed"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	waitIdle(t, alice, bob)
	waitForChannel(t, h, ch.ID, ChannelClosed)

	// alice's trailing reply (to bob's last packet) legitimately races the
	// MaxTurns close; only closure-class errors may surface.
	if err := alice.Err(); err != nil && !errors.Is(err, ErrChannelClosed) && !errors.Is(err, ErrProtocol) {
		t.Fatalf("alice handler error: %v", err)
	}
	if err := bob.Err(); err != nil {
		t.Fatalf("bob handler error: %v", err)
	}
	// bob's inbound carries the handoff signal line + the body.
	got := bobLog.all()
	if len(got) != 1 {
		t.Fatalf("bob log: %v", got)
	}
	if !strings.Contains(got[0], "[Handed off via handoff] load") || !strings.Contains(got[0], "redirecting") {
		t.Fatalf("bob inbound: %q", got[0])
	}
	_ = aliceLog
}

// TestWorkflowFinish: a tool-driven Finish terminates the channel with the
// tool's reason.
func TestWorkflowFinish(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)

	alice, _ := hc.Register("alice", WithRoundFunc(func(Envelope) (Round, error) {
		return Round{Text: "wrapping up", Finish: &Finish{Summary: "complete", Reason: "wrapped"}}, nil
	}))
	defer alice.Close()
	bob, _ := hc.Register("bob", WithAgentFunc(func(msg string) (string, error) {
		return "bob should never run", nil
	}))
	defer bob.Close()

	graph := TransitionGraph{
		Initial:     alice.AgentID(),
		Transitions: []Transition{{When: Always{}, To: &RoundRobinTarget{}}},
	}
	ch, err := alice.CreateChannel(ChannelManifest{
		Type:  TypeWorkflow,
		Knobs: map[string]any{"graph": graph.ToDict()},
	}, bob.AgentID())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := alice.Ask(ch.ID, "go"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	waitIdle(t, alice, bob)
	done := waitForChannel(t, h, ch.ID, ChannelClosed)
	if done.CloseReason != "wrapped" {
		t.Fatalf("close reason: %q", done.CloseReason)
	}
	// bob never got a round in: the finish packet was the last substantive
	// envelope in the WAL (his reply attempt, if any raced the close, is
	// rejected by the terminated-workflow gate / closed channel).
	wal, err := h.ReadWAL(ch.ID)
	if err != nil {
		t.Fatalf("read wal: %v", err)
	}
	for _, env := range wal {
		if env.EventType == EnvPacket && env.SenderID == bob.AgentID() {
			t.Fatalf("bob posted a round after finish: %+v", env)
		}
	}
	if err := bob.Err(); err != nil && !errors.Is(err, ErrProtocol) && !errors.Is(err, ErrChannelClosed) {
		t.Fatalf("bob error: %v", err)
	}
}

// TestConsultingViaClients: full 1Q1R handshake driven by two message
// callbacks (the "delegate one question" path).
func TestConsultingViaClients(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)

	bobLog := &msgLog{}
	alice, _ := hc.Register("alice", WithAgentFunc(func(msg string) (string, error) {
		return "the answer is 42", nil
	}))
	defer alice.Close()
	bob, _ := hc.Register("bob", WithAgentFunc(func(msg string) (string, error) {
		bobLog.add(msg)
		return "42", nil
	}))
	defer bob.Close()

	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConsulting}, bob.AgentID())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// The initiator posts the question verbatim (say-tool path); the
	// respondent's agent answers it.
	if err := alice.Say(ch.ID, "what is the answer?"); err != nil {
		t.Fatalf("say: %v", err)
	}
	waitIdle(t, alice, bob)
	done := waitForChannel(t, h, ch.ID, ChannelClosed)
	if done.CloseReason != "consulting_complete" {
		t.Fatalf("close reason: %q", done.CloseReason)
	}
	// bob's reply was routed back to alice as text.
	if got := bobLog.all(); len(got) != 1 || got[0] != "what is the answer?" {
		t.Fatalf("bob log: %v", got)
	}
}

// TestHumanClientPushPull: a human participant acks invites, sees the
// lifecycle + replies in its pull queue, and can answer.
func TestHumanClientPushPull(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)

	agent, _ := hc.Register("agent", WithAgentFunc(func(msg string) (string, error) {
		return "ack: " + msg, nil
	}))
	defer agent.Close()
	human, err := hc.RegisterHuman("pat")
	if err != nil {
		t.Fatalf("register human: %v", err)
	}
	defer human.Close()

	ch, err := human.CreateChannel(ChannelManifest{Type: TypeConversation}, agent.AgentID())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// The human's pull queue first sees the opened broadcast.
	env, err := human.Recv(ctx)
	if err != nil {
		t.Fatalf("recv opened: %v", err)
	}
	if env.EventType != EnvChannelOpened {
		t.Fatalf("first envelope: %s", env.EventType)
	}
	// The human answers; the agent bridges through the callback.
	if _, err := human.Reply(env, "hello agent"); err != nil {
		t.Fatalf("reply: %v", err)
	}
	waitIdle(t, agent)
	// Drain until the agent's text reply lands (skipping lifecycle noise).
	for {
		env, err := human.Recv(ctx)
		if err != nil {
			t.Fatalf("recv reply: %v", err)
		}
		if env.EventType == EnvText {
			if env.Text() != "ack: hello agent" {
				t.Fatalf("reply text: %q", env.Text())
			}
			break
		}
	}
	// The human can close the channel.
	if err := human.CloseChannel(ch.ID, "done chatting"); err != nil {
		t.Fatalf("close: %v", err)
	}
	done, err := h.Channel(ch.ID)
	if err != nil || done.State != ChannelClosed || done.CloseReason != "done chatting" {
		t.Fatalf("channel after human close: %+v %v", done, err)
	}
}

// TestAgentEnvelopeEnvironment: the full-control environment sees raw
// envelopes and can decline to speak.
func TestAgentEnvelopeEnvironment(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)

	var seen []Envelope
	var mu sync.Mutex
	alice, _ := hc.Register("alice", WithEnvironment(func(env Envelope) (string, error) {
		mu.Lock()
		seen = append(seen, env)
		mu.Unlock()
		if env.Text() == "speak" {
			return "spoken", nil
		}
		return "", nil // silent round
	}))
	defer alice.Close()
	bob, _ := hc.Register("bob", WithAgentFunc(func(msg string) (string, error) { return "ok", nil }))
	defer bob.Close()

	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Silent round: nothing posted, no error.
	if _, err := alice.Ask(ch.ID, "stay quiet"); err != nil {
		t.Fatalf("silent ask: %v", err)
	}
	waitIdle(t, alice)
	if _, err := alice.Ask(ch.ID, "speak"); err != nil {
		t.Fatalf("speak ask: %v", err)
	}
	waitIdle(t, alice, bob)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 || seen[0].Text() != "stay quiet" || seen[1].Text() != "speak" {
		t.Fatalf("environment saw: %v", eventTypes(seen))
	}
	// Anything beyond the two synthetic asks is bob's round-tripped reply.
	for _, env := range seen[2:] {
		if env.SenderID != bob.AgentID() || env.Text() != "ok" {
			t.Fatalf("unexpected extra envelope: %+v", env)
		}
	}
}

// TestRegisterErrors pins the registration error surface.
func TestRegisterErrors(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)
	if _, err := hc.Register(""); err == nil {
		t.Fatal("empty name must fail")
	}
	a, err := hc.Register("dup")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer a.Close()
	if _, err := hc.Register("dup"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate: %v", err)
	}
}
