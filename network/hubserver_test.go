package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luckystrker/ago/network/transport"
)

// wireRig is a hub served over an in-memory listener: Hub.Serve accepts each
// dialed connection and serves it on its own endpoint.
type wireRig struct {
	t    *testing.T
	hub  *Hub
	ln   *transport.MemListener
	ctx  context.Context
	stop context.CancelFunc
	done chan struct{}
}

func newWireRig(t *testing.T, opts ...HubOption) *wireRig {
	t.Helper()
	hub := newTestHub(t, opts...)
	ln := transport.NewMemListener(1024)
	ctx, stop := context.WithCancel(context.Background())
	r := &wireRig{t: t, hub: hub, ln: ln, ctx: ctx, stop: stop, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		_ = hub.Serve(ctx, ln)
	}()
	t.Cleanup(func() {
		stop()
		_ = ln.Close()
		<-r.done
	})
	return r
}

func (r *wireRig) connect() *HubClient {
	r.t.Helper()
	hc, err := ConnectHub(r.ctx, RemoteConfig{Dial: ListenerDialer(r.ln), RPCTimeout: 5 * time.Second})
	if err != nil {
		r.t.Fatalf("ConnectHub: %v", err)
	}
	r.t.Cleanup(func() { _ = hc.Close() })
	return hc
}

// rawConn is a hand-driven connection: frames go out and come in untouched.
type rawConn struct {
	t    *testing.T
	link transport.Link
}

func (r *wireRig) raw() *rawConn {
	r.t.Helper()
	l, err := r.ln.Dial()
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = l.Close() })
	return &rawConn{t: r.t, link: l}
}

func (c *rawConn) send(f transport.Frame) {
	c.t.Helper()
	if err := c.link.Send(f); err != nil {
		c.t.Fatal(err)
	}
}

func (c *rawConn) recv() transport.Frame {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	f, err := c.link.Recv(ctx)
	if err != nil {
		c.t.Fatalf("recv: %v", err)
	}
	return f
}

// rpc sends one request and returns the response, skipping notifies.
func (c *rawConn) rpc(op, id, params string) transport.Frame {
	c.t.Helper()
	c.send(transport.Frame{Kind: transport.FrameRequest, RequestID: id, Op: op, Payload: []byte(params)})
	for {
		f := c.recv()
		if f.Kind == transport.FrameResponse && f.RequestID == id {
			return f
		}
	}
}

// End to end: two agents on a remote hub, one human, each on its own
// connection — open a channel, exchange messages, close it.
func TestRemoteHubEndToEnd(t *testing.T) {
	rig := newWireRig(t)
	ctx := context.Background()

	hcA, hcB, hcH := rig.connect(), rig.connect(), rig.connect()
	if hcA.Hub() != nil {
		t.Fatal("a remote client has no in-process hub")
	}
	bob, err := hcB.Register("bob", WithEnvironment(func(env Envelope) (string, error) {
		if env.EventType != EnvText {
			return "", nil
		}
		return "pong:" + textOf(env), nil
	}))
	if err != nil {
		t.Fatalf("register bob: %v", err)
	}
	alice, err := hcA.Register("alice", WithEnvironment(func(env Envelope) (string, error) { return "", nil }))
	if err != nil {
		t.Fatalf("register alice: %v", err)
	}
	carol, err := hcH.RegisterHuman("carol")
	if err != nil {
		t.Fatalf("register human: %v", err)
	}
	defer carol.Close()

	// alice → bob: the handshake (invite, remote ack) completes through the link.
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if ch.State != ChannelActive {
		t.Fatalf("channel state %s", ch.State)
	}
	if err := alice.Say(ch.ID, "ping"); err != nil {
		t.Fatalf("say: %v", err)
	}
	waitFor(t, func() bool {
		log, _ := rig.hub.ReadWAL(ch.ID)
		for _, e := range log {
			if e.SenderID == bob.AgentID() && textOf(e) == "pong:ping" {
				return true
			}
		}
		return false
	})
	if err := bob.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}

	// human ↔ agent over the same hub, then close.
	hch, err := carol.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())
	if err != nil {
		t.Fatalf("human create channel: %v", err)
	}
	if _, err := carol.Send(NewTextEnvelope(hch.ID, carol.AgentID(), "", "hello all")); err != nil {
		t.Fatal(err)
	}
	var sawReply bool
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for !sawReply {
		env, err := carol.Recv(rctx)
		if err != nil {
			t.Fatalf("human never saw bob's reply: %v", err)
		}
		sawReply = env.EventType == EnvText && env.SenderID == bob.AgentID() && textOf(env) == "pong:hello all"
	}
	if err := carol.CloseChannel(hch.ID, "done"); err != nil {
		t.Fatalf("close channel: %v", err)
	}
	waitForChannel(t, rig.hub, hch.ID, ChannelClosed)
	got, err := hcH.api.Channel(hch.ID)
	if err != nil || got.State != ChannelClosed || got.CloseReason != "done" {
		t.Fatalf("remote view of the closed channel: %+v %v", got, err)
	}
	if _, err := alice.Send(NewTextEnvelope(hch.ID, alice.AgentID(), "", "late")); !errors.Is(err, ErrChannelClosed) {
		t.Fatalf("send on a closed channel must fail with a matchable sentinel: %v", err)
	}
	// Names are unique hub-wide: the typed error survives the wire.
	if _, err := hcA.Register("alice"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate name over the wire: %v", err)
	}
}

func textOf(env Envelope) string {
	var p struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(env.Payload, &p)
	return p.Text
}

// registerRaw registers an agent over a hand-driven connection.
func (c *rawConn) register(name string) Passport {
	c.t.Helper()
	f := c.rpc(OpRegister, "reg-"+name, `{"passport":{"name":"`+name+`"}}`)
	if f.Code != "" {
		c.t.Fatalf("register %s: %s %s", name, f.Code, f.Message)
	}
	var p Passport
	if err := json.Unmarshal(f.Payload, &p); err != nil {
		c.t.Fatal(err)
	}
	return p
}

func (c *rawConn) hello(id, body string) transport.Frame {
	c.t.Helper()
	c.send(transport.Frame{Kind: transport.FrameHello, RequestID: id, Payload: []byte(body)})
	for {
		f := c.recv()
		if f.RequestID == id && (f.Kind == transport.FrameWelcome || f.Kind == transport.FrameError) {
			return f
		}
	}
}

// --- hello ------------------------------------------------------------------------------------

func TestParseHelloSinceFormats(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantErr    bool
		channel    string
		wantID     string
		wantOK     bool
	}{
		{"legacy single value", `{"name":"a","since_envelope_id":"e5"}`, false, "ch1", "e5", true},
		{"legacy empty string replays everything", `{"name":"a","since_envelope_id":""}`, false, "ch1", "", true},
		{"legacy null skips replay", `{"name":"a","since_envelope_id":null}`, false, "ch1", "", false},
		{"absent skips replay", `{"name":"a"}`, false, "ch1", "", false},
		{"per channel", `{"name":"a","since":{"ch1":"e7","ch2":"e1"}}`, false, "ch2", "e1", true},
		{"per channel misses another channel", `{"name":"a","since":{"ch1":"e7"}}`, false, "ch9", "", false},
		{"per channel wins over legacy, legacy covers the rest", `{"name":"a","since_envelope_id":"e1","since":{"ch1":"e9"}}`, false, "ch1", "e9", true},
		{"legacy covers channels the map omits", `{"name":"a","since_envelope_id":"e1","since":{"ch1":"e9"}}`, false, "ch2", "e1", true},
		{"since_envelope_id of the wrong type", `{"name":"a","since_envelope_id":5}`, true, "", "", false},
		{"since of the wrong type", `{"name":"a","since":["x"]}`, true, "", "", false},
		{"since value of the wrong type", `{"name":"a","since":{"ch1":5}}`, true, "", "", false},
		{"missing name", `{"since_envelope_id":"e1"}`, true, "", "", false},
		{"not an object", `[1]`, true, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := parseHello([]byte(tc.body))
			if tc.wantErr {
				if !errors.Is(err, ErrProtocol) {
					t.Fatalf("want a protocol error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			id, ok := b.SinceMarks().Lookup(tc.channel)
			if id != tc.wantID || ok != tc.wantOK {
				t.Fatalf("Lookup(%q) = %q,%v want %q,%v", tc.channel, id, ok, tc.wantID, tc.wantOK)
			}
		})
	}
}

func TestHubWireHello(t *testing.T) {
	rig := newWireRig(t, WithHubAuth(NewAuthRegistry(NoAuth{}, NewApiKeyAuth(map[string]string{"keyed": "s3cret"}))))
	c1, c2 := rig.raw(), rig.raw()
	alice := c1.register("alice")

	// Unknown name, malformed frame, scheme mismatch, bad claim.
	if f := c2.hello("h1", `{"name":"ghost"}`); f.Kind != transport.FrameError || f.Code != CodeNotFound {
		t.Fatalf("unknown name: %+v", f)
	}
	if f := c2.hello("h2", `{"name":"alice","since":{"ch":7}}`); f.Kind != transport.FrameError || f.Code != CodeProtocol {
		t.Fatalf("malformed hello: %+v", f)
	}
	if f := c2.hello("h3", `{"name":"alice","auth_scheme":"api_key","auth_claim":{"token":"x"}}`); f.Kind != transport.FrameError || f.Code != CodeAuthFailed {
		t.Fatalf("scheme mismatch: %+v", f)
	}
	if f := c2.hello("h4", `{"name":"alice","auth_claim":"not an object"}`); f.Kind != transport.FrameError || f.Code != CodeAuthFailed {
		t.Fatalf("claim of the wrong type: %+v", f)
	}
	// A credentialed identity: right and wrong token.
	reg := c1.rpc(OpRegister, "reg-keyed", `{"passport":{"name":"keyed","auth":{"scheme":"api_key","claim":{"token":"s3cret"}}}}`)
	if reg.Code != "" {
		t.Fatalf("register keyed: %+v", reg)
	}
	if bytesContain(reg.Payload, "s3cret") {
		t.Fatalf("the register response leaks the auth claim: %s", reg.Payload)
	}
	if f := c2.hello("h5", `{"name":"keyed","auth_scheme":"api_key","auth_claim":{"token":"wrong"}}`); f.Kind != transport.FrameError || f.Code != CodeAuthFailed {
		t.Fatalf("wrong token: %+v", f)
	}
	if f := c2.hello("h6", `{"name":"keyed","auth_scheme":"api_key","auth_claim":{"token":"s3cret"}}`); f.Kind != transport.FrameWelcome {
		t.Fatalf("right token: %+v", f)
	}

	// An authenticated hello re-binds the identity: the welcome echoes the
	// request id and the since marks reach the endpoint.
	w := c2.hello("h7", `{"name":"alice","since_envelope_id":"e1","since":{"chX":"e9"}}`)
	if w.Kind != transport.FrameWelcome || w.RequestID != "h7" {
		t.Fatalf("welcome: %+v", w)
	}
	var wb WelcomeBody
	if err := json.Unmarshal(w.Payload, &wb); err != nil || wb.EndpointID == "" || wb.HubTime == "" {
		t.Fatalf("welcome body: %+v %v", wb, err)
	}
	ws := &rig.hub.ext.wire
	ws.mu.Lock()
	marks := ws.agentEP[alice.AgentID].since[alice.AgentID]
	ws.mu.Unlock()
	if id, ok := marks.Lookup("chX"); !ok || id != "e9" {
		t.Fatalf("per-channel mark lost: %q %v", id, ok)
	}
	if id, ok := marks.Lookup("other"); !ok || id != "e1" {
		t.Fatalf("legacy mark lost: %q %v", id, ok)
	}

	// Connection-bound identity: alice now belongs to c2; the connection that
	// held her before can no longer act as her.
	if f := c1.rpc(OpSetRule, "r1", `{"agent_id":"`+alice.AgentID+`","rule":{"inbound_globs":["*"],"outbound_globs":["*"]}}`); f.Code != CodeAccessDenied {
		t.Fatalf("zombie connection acted as alice: %+v", f)
	}
	if f := c2.rpc(OpSetRule, "r2", `{"agent_id":"`+alice.AgentID+`","rule":{"inbound_globs":["*"],"outbound_globs":["*"]}}`); f.Code != "" {
		t.Fatalf("the new holder must act as alice: %+v", f)
	}
}

func bytesContain(b []byte, s string) bool { return strings.Contains(string(b), s) }

// --- connection-bound identity ----------------------------------------------------------------

func TestHubWireConnectionBoundIdentity(t *testing.T) {
	rig := newWireRig(t)
	ctx := context.Background()
	hcA, hcX := rig.connect(), rig.connect()
	alice, err := hcA.Register("alice", WithEnvironment(func(Envelope) (string, error) { return "", nil }))
	if err != nil {
		t.Fatal(err)
	}
	bob, err := hcA.Register("bob", WithEnvironment(func(Envelope) (string, error) { return "", nil }))
	if err != nil {
		t.Fatal(err)
	}
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	env, err := alice.Send(NewTextEnvelope(ch.ID, alice.AgentID(), "", "secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hcX.Register("mallory"); err != nil {
		t.Fatal(err)
	}
	denied := func(what, op string, params any) {
		t.Helper()
		if err := hcX.Call(ctx, op, params, nil); !errors.Is(err, ErrAccessDenied) {
			t.Errorf("%s: want access_denied, got %v", what, err)
		}
	}
	// Agent-scoped ops name an agent the connection does not hold.
	denied("set_resume", OpSetResume, setResumeParams{AgentID: alice.AgentID(), Resume: Resume{Summary: "x"}})
	denied("set_rule", OpSetRule, setRuleParams{AgentID: alice.AgentID(), Rule: DefaultRule()})
	denied("unregister", OpUnregister, agentIDParams{AgentID: alice.AgentID()})
	denied("create_channel as alice", OpCreateChannel, createChannelParams{Manifest: ChannelManifest{Type: TypeConversation}, Participants: []string{alice.AgentID(), bob.AgentID()}})
	denied("can_send", OpCanSend, canSendParams{ChannelID: ch.ID, SenderID: alice.AgentID()})
	denied("pending_turns_for", OpPendingTurnsFor, agentIDParams{AgentID: alice.AgentID()})
	denied("report_turn_failure", OpReportTurnFailure, turnFailureParams{ChannelID: ch.ID, AgentID: alice.AgentID(), EnvelopeID: env.ID})
	denied("record_observation", OpRecordObservation, recordObservationParams{OwnerID: alice.AgentID(), Capability: "c", Outcome: "completed"})
	denied("post as alice", OpPostEnvelope, postEnvelopeParams{Envelope: NewTextEnvelope(ch.ID, alice.AgentID(), "", "forged")})
	denied("list_channels of alice", OpListChannels, listChannelsParams{AgentID: alice.AgentID()})
	// Channel-scoped ops need a held participant.
	denied("get_channel", OpGetChannel, channelIDParams{ChannelID: ch.ID})
	denied("close_channel", OpCloseChannel, closeChannelParams{ChannelID: ch.ID})
	denied("read_wal", OpReadWAL, readWALParams{ChannelID: ch.ID})
	denied("find_envelope_by_causation", OpFindEnvelopeByCausation, causationParams{ChannelID: ch.ID, SenderID: alice.AgentID(), CausationID: "c"})
	// Task ops without a held agent / owner.
	if err := hcA.Call(ctx, OpObserveTask, map[string]any{"metadata": map[string]any{"task_id": "t-1", "owner_id": alice.AgentID(), "channel_id": ch.ID}}, nil); err != nil {
		t.Fatal(err)
	}
	denied("observe_task as alice", OpObserveTask, map[string]any{"metadata": map[string]any{"task_id": "t-2", "owner_id": alice.AgentID()}})
	denied("update_task of alice's task", OpUpdateTask, updateTaskParams{TaskID: "t-1", State: "working"})
	denied("checkpoint_task of alice's task", OpCheckpointTask, checkpointParams{TaskID: "t-1", State: []byte("x")})
	if err := hcX.Call(ctx, OpGetTask, taskIDParams{TaskID: "t-1"}, nil); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("a task the connection may not see must read as unknown: %v", err)
	}
	var visible []json.RawMessage
	if err := hcX.Call(ctx, OpListTasks, listTasksParams{}, &visible); err != nil || len(visible) != 0 {
		t.Errorf("list_tasks leaks: %v %v", visible, err)
	}
	var chans []ChannelSnapshot
	if err := hcX.Call(ctx, OpListChannels, listChannelsParams{}, &chans); err != nil || len(chans) != 0 {
		t.Errorf("list_channels leaks: %v %v", chans, err)
	}
	// Discovery is open to any connection.
	var p Passport
	if err := hcX.Call(ctx, OpGetAgent, nameOrIDParams{NameOrID: "alice"}, &p); err != nil || p.AgentID != alice.AgentID() {
		t.Errorf("get_agent: %+v %v", p, err)
	}
	// Nothing leaked into the hub: no forged envelope landed.
	if log, _ := rig.hub.ReadWAL(ch.ID); countText(log, "forged") != 0 {
		t.Fatal("a forged envelope was accepted")
	}
}

func countText(log []Envelope, text string) int {
	n := 0
	for _, e := range log {
		if textOf(e) == text {
			n++
		}
	}
	return n
}

// Hub-only protocol events and foreign audiences are refused; unknown and
// malformed requests answer with typed errors.
func TestHubWireRequestValidation(t *testing.T) {
	rig := newWireRig(t)
	ctx := context.Background()
	hc := rig.connect()
	alice, _ := hc.Register("alice", WithEnvironment(func(Envelope) (string, error) { return "", nil }))
	bob, _ := hc.Register("bob", WithEnvironment(func(Envelope) (string, error) { return "", nil }))
	carol, _ := hc.Register("carol", WithEnvironment(func(Envelope) (string, error) { return "", nil }))
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	opened := NewTextEnvelope(ch.ID, alice.AgentID(), "", "x")
	opened.EventType = EnvChannelOpened
	if err := hc.Call(ctx, OpPostEnvelope, postEnvelopeParams{Envelope: opened}, nil); !errors.Is(err, ErrProtocol) {
		t.Errorf("hub-only event posted over the wire: %v", err)
	}
	foreign := NewTextEnvelope(ch.ID, alice.AgentID(), carol.AgentID(), "psst")
	if err := hc.Call(ctx, OpPostEnvelope, postEnvelopeParams{Envelope: foreign}, nil); !errors.Is(err, ErrProtocol) {
		t.Errorf("audience outside the channel: %v", err)
	}
	if err := hc.Call(ctx, "nonsense", map[string]any{}, nil); !errors.Is(err, ErrProtocol) {
		t.Errorf("unknown op: %v", err)
	}
	if err := hc.Call(ctx, OpGetTask, taskIDParams{TaskID: "../etc/passwd"}, nil); !errors.Is(err, ErrProtocol) {
		t.Errorf("task id outside the allowed shape: %v", err)
	}
	if err := hc.Call(ctx, OpGetAgent, "not an object", nil); !errors.Is(err, ErrProtocol) {
		t.Errorf("params of the wrong type: %v", err)
	}
	if err := hc.Call(ctx, OpRegister, registerParams{Passport: Passport{Name: "rem", Kind: KindRemoteAgent}}, nil); !errors.Is(err, ErrAccessDenied) {
		t.Errorf("remote_agent registration over the wire: %v", err)
	}
	if err := hc.Call(ctx, OpFireTaskEvent, fireTaskEventParams{TaskID: "t-9", Kind: "task_started"}, nil); !errors.Is(err, ErrAccessDenied) {
		t.Errorf("only mirror_failed may be fired over the wire: %v", err)
	}
	// A request frame with no op cannot be correlated: the hub answers with an error frame.
	raw := rig.raw()
	raw.send(transport.Frame{Kind: transport.FrameRequest, RequestID: "x"})
	if f := raw.recv(); f.Kind != transport.FrameError || f.Code != CodeProtocol {
		t.Errorf("request without op: %+v", f)
	}
}

// Every op of Python's wire surface is served and works for a connection that
// holds the right agents.
func TestHubWireServesEveryOp(t *testing.T) {
	rig := newWireRig(t)
	ctx := context.Background()
	hc := rig.connect()
	alice, err := hc.Register("alice", WithEnvironment(func(Envelope) (string, error) { return "", nil }), WithAgentResume(Resume{Capabilities: []string{"search"}, Summary: "finds things"}))
	if err != nil {
		t.Fatal(err)
	}
	bob, _ := hc.Register("bob", WithEnvironment(func(Envelope) (string, error) { return "", nil }))
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	cause := NewTextEnvelope(ch.ID, alice.AgentID(), "", "q")
	cause.CausationID = "c1"
	sent, err := alice.Send(cause)
	if err != nil {
		t.Fatal(err)
	}
	aid, bid := alice.AgentID(), bob.AgentID()
	type op struct {
		name   string
		params any
		check  func(t *testing.T, raw json.RawMessage)
	}
	var raw = func(p any) json.RawMessage { b, _ := json.Marshal(p); return b }
	_ = raw
	ops := []op{
		{OpGetAgent, nameOrIDParams{NameOrID: "alice"}, func(t *testing.T, r json.RawMessage) { wantContains(t, r, aid) }},
		{OpGetResume, agentIDParams{AgentID: aid}, func(t *testing.T, r json.RawMessage) { wantContains(t, r, "search") }},
		{OpGetSkill, agentIDParams{AgentID: aid}, nil},
		{OpGetRule, agentIDParams{AgentID: aid}, func(t *testing.T, r json.RawMessage) { wantContains(t, r, "inbound_globs") }},
		{OpFindAgentID, nameParams{Name: "alice"}, func(t *testing.T, r json.RawMessage) { wantContains(t, r, aid) }},
		{OpNamesFor, agentIDsParams{AgentIDs: []string{aid, bid}}, func(t *testing.T, r json.RawMessage) { wantContains(t, r, `"alice"`) }},
		{OpListAgents, listAgentsParams{Capability: "search"}, func(t *testing.T, r json.RawMessage) { wantContains(t, r, aid) }},
		{OpSetResume, setResumeParams{AgentID: aid, Resume: Resume{Capabilities: []string{"search", "plan"}}}, nil},
		{OpSetSkill, setSkillParams{AgentID: aid}, nil},
		{OpSetRule, setRuleParams{AgentID: aid, Rule: DefaultRule()}, nil},
		{OpAddResumeExample, addResumeExampleParams{AgentID: aid, Example: ResumeExample{Title: "e1"}}, nil},
		{OpGetChannel, channelIDParams{ChannelID: ch.ID}, func(t *testing.T, r json.RawMessage) { wantContains(t, r, ch.ID) }},
		{OpListChannels, listChannelsParams{}, func(t *testing.T, r json.RawMessage) { wantContains(t, r, ch.ID) }},
		{OpPostEnvelope, postEnvelopeParams{Envelope: NewTextEnvelope(ch.ID, bid, "", "r")}, func(t *testing.T, r json.RawMessage) { wantContains(t, r, `"id"`) }},
		{OpReadWAL, readWALParams{ChannelID: ch.ID}, func(t *testing.T, r json.RawMessage) { wantContains(t, r, sent.ID) }},
		{OpFindEnvelopeByCausation, causationParams{ChannelID: ch.ID, SenderID: aid, CausationID: "c1"}, func(t *testing.T, r json.RawMessage) { wantContains(t, r, sent.ID) }},
		{OpCanSend, canSendParams{ChannelID: ch.ID, SenderID: aid}, func(t *testing.T, r json.RawMessage) { wantContains(t, r, "true") }},
		{OpPendingTurnsFor, agentIDParams{AgentID: aid}, nil},
		{OpReportTurnFailure, turnFailureParams{ChannelID: ch.ID, AgentID: aid, EnvelopeID: sent.ID, Error: "boom"}, nil},
		{OpObserveTask, map[string]any{"metadata": map[string]any{"task_id": "t-1", "owner_id": aid, "channel_id": ch.ID}}, nil},
		{OpGetTask, taskIDParams{TaskID: "t-1"}, func(t *testing.T, r json.RawMessage) { wantContains(t, r, "t-1") }},
		{OpListTasks, listTasksParams{}, func(t *testing.T, r json.RawMessage) { wantContains(t, r, "t-1") }},
		{OpUpdateTask, updateTaskParams{TaskID: "t-1", State: "working", Progress: map[string]any{"pct": 50}}, nil},
		{OpFireTaskEvent, fireTaskEventParams{TaskID: "t-1", Kind: TaskEventMirrorFailed, Payload: map[string]any{"owner_id": aid, "channel_id": ch.ID, "exc_message": strings.Repeat("x", 900)}}, nil},
		{OpCheckpointTask, checkpointParams{TaskID: "t-1", State: []byte("cp"), WriterID: aid}, nil},
		{OpReadTaskCheckpoint, checkpointParams{TaskID: "t-1", WriterID: aid}, func(t *testing.T, r json.RawMessage) { wantContains(t, r, `"Y3A="`) }},
		{OpRecordObservation, recordObservationParams{OwnerID: aid, Capability: "search", Outcome: "completed", TaskID: "t-1"}, nil},
		{OpCloseChannel, closeChannelParams{ChannelID: ch.ID, Reason: "done"}, func(t *testing.T, r json.RawMessage) { wantContains(t, r, "closed") }},
		{OpUnregister, agentIDParams{AgentID: bid}, nil},
	}
	covered := map[string]bool{OpRegister: true, OpCreateChannel: true} // exercised through Register / CreateChannel above
	for _, o := range ops {
		covered[o.name] = true
		var out json.RawMessage
		if err := hc.Call(ctx, o.name, o.params, &out); err != nil {
			t.Errorf("%s: %v", o.name, err)
			continue
		}
		if o.check != nil {
			o.check(t, out)
		}
	}
	for _, name := range WireOps {
		if !covered[name] {
			t.Errorf("op %s is not exercised", name)
		}
	}
	// Side effects landed in the hub itself.
	if st, _ := rig.hub.Task("t-1"); st.Progress["pct"] != float64(50) && st.Progress["pct"] != 50 {
		t.Errorf("update_task did not reach the hub: %+v", st)
	}
	if r, _ := rig.hub.ResumeOf(aid); r.Observed["search"].Completed != 1 || len(r.Capabilities) != 2 {
		t.Errorf("resume ops did not reach the hub: %+v", r)
	}
	if _, err := rig.hub.Passport(bid); !errors.Is(err, ErrNotFound) {
		t.Errorf("unregister did not reach the hub: %v", err)
	}
	if ws := &rig.hub.ext.wire; true {
		ws.mu.Lock()
		_, still := ws.agentEP[bid]
		ws.mu.Unlock()
		if still {
			t.Error("unregistered agent is still bound to a connection")
		}
	}
}

func wantContains(t *testing.T, raw json.RawMessage, sub string) {
	t.Helper()
	if !strings.Contains(string(raw), sub) {
		t.Errorf("result %s does not contain %q", raw, sub)
	}
}

// --- receipts --------------------------------------------------------------------------------------

// notifyRecorder is a raw connection's reader: it acks channel invites for
// agentID and records every notify.
type notifyRecorder struct {
	mu   sync.Mutex
	envs []Envelope
}

func (c *rawConn) recordNotifies(agentID string) *notifyRecorder {
	rec := &notifyRecorder{}
	go func() {
		for n := 0; ; n++ {
			f, err := c.link.Recv(context.Background())
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
			rec.mu.Lock()
			rec.envs = append(rec.envs, env)
			rec.mu.Unlock()
			if env.EventType == EnvChannelInvite {
				ack, _ := json.Marshal(postEnvelopeParams{Envelope: NewChannelAck(env, agentID, false, "")})
				_ = c.link.Send(transport.Frame{Kind: transport.FrameRequest, RequestID: "ack-" + env.ID, Op: OpPostEnvelope, Payload: ack})
			}
		}
	}()
	return rec
}

func (r *notifyRecorder) texts() []Envelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Envelope
	for _, e := range r.envs {
		if e.EventType == EnvText {
			out = append(out, e)
		}
	}
	return out
}

// Receipts move the cursor and the nack log only when they come from the
// connection that holds the named agent; the delivery that went over the link
// carries ids the receipts can name.
func TestHubWireReceipts(t *testing.T) {
	rig := newWireRig(t)
	hcA := rig.connect()
	alice, _ := hcA.Register("alice", WithEnvironment(func(Envelope) (string, error) { return "", nil }))
	dave, mallory := rig.raw(), rig.raw()
	d := dave.register("dave")
	mallory.register("mallory")
	rec := dave.recordNotifies(d.AgentID)

	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, d.AgentID)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	e1, _ := alice.Send(NewTextEnvelope(ch.ID, alice.AgentID(), "", "one"))
	e2, _ := alice.Send(NewTextEnvelope(ch.ID, alice.AgentID(), "", "two"))
	waitFor(t, func() bool { return len(rec.texts()) == 2 })
	if got := rec.texts(); got[0].ID != e1.ID || got[1].ID != e2.ID {
		t.Fatalf("delivery order over the link: %s %s", got[0].ID, got[1].ID)
	}

	receipt := func(c *rawConn, code, env string) {
		c.send(transport.Frame{Kind: transport.FrameReceipt, RecipientID: d.AgentID, ChannelID: ch.ID, EnvelopeID: env, Code: code, Message: "why"})
	}
	// Forged by a connection that does not hold dave: nothing changes.
	receipt(mallory, ReceiptAck, e2.ID)
	receipt(mallory, ReceiptNack, e2.ID)
	// A round trip on the same connection orders the check after the receipts.
	mallory.rpc(OpGetAgent, "sync", `{"name_or_id":"dave"}`)
	if got := rig.hub.InboxCursor(d.AgentID, ch.ID); got != "" {
		t.Fatalf("a forged ack moved the cursor to %q", got)
	}
	if n := len(rig.hub.Nacks(d.AgentID)); n != 0 {
		t.Fatalf("a forged nack was recorded (%d)", n)
	}
	// The holder acks e1, nacks e2: the cursor stays at e1 (a nack never moves
	// it). An ack with no envelope goes first and must change nothing.
	receipt(dave, ReceiptAck, "")
	receipt(dave, ReceiptAck, e1.ID)
	receipt(dave, ReceiptNack, e2.ID)
	waitFor(t, func() bool { return len(rig.hub.Nacks(d.AgentID)) == 1 })
	if got := rig.hub.InboxCursor(d.AgentID, ch.ID); got != e1.ID {
		t.Fatalf("cursor %q want %q", got, e1.ID)
	}
	if n := rig.hub.Nacks(d.AgentID); n[0].EnvelopeID != e2.ID || n[0].Reason != "why" {
		t.Fatalf("nack: %+v", n)
	}
}

// A dead link releases its agents: the next dispatch finds the connection
// gone and the identity is free to be bound again.
func TestHubWireDeadConnectionIsDropped(t *testing.T) {
	rig := newWireRig(t)
	hcA := rig.connect()
	alice, _ := hcA.Register("alice", WithEnvironment(func(Envelope) (string, error) { return "", nil }))
	dave := rig.raw()
	d := dave.register("dave")
	_ = dave.recordNotifies(d.AgentID)
	if _, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, d.AgentID); err != nil {
		t.Fatal(err)
	}
	ws := &rig.hub.ext.wire
	_ = dave.link.Close()
	ch, _ := hcA.api.Channel(mustOnlyChannel(t, rig.hub))
	if _, err := alice.Send(NewTextEnvelope(ch.ID, alice.AgentID(), "", "anyone?")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		ws.mu.Lock()
		defer ws.mu.Unlock()
		return ws.agentEP[d.AgentID] == nil && len(ws.endpoints) == 1 // only hcA's connection is left
	})
	// dave re-attaches from a new connection.
	again := rig.raw()
	if f := again.hello("again", `{"name":"dave"}`); f.Kind != transport.FrameWelcome {
		t.Fatalf("re-attach after a dead connection: %+v", f)
	}
}

func mustOnlyChannel(t *testing.T, h *Hub) string {
	t.Helper()
	chans := h.ListChannels()
	if len(chans) != 1 {
		t.Fatalf("want one channel, have %d", len(chans))
	}
	return chans[0].ID
}

// Reconnect: the client replaces its link, re-binds its agents with a hello
// and keeps receiving; requests waiting on the dropped link fail typed.
func TestRemoteHubReconnectRebindsAgents(t *testing.T) {
	rig := newWireRig(t)
	ctx := context.Background()
	hcA, hcB := rig.connect(), rig.connect()
	var mu sync.Mutex
	var got []string
	bob, err := hcB.Register("bob", WithEnvironment(func(env Envelope) (string, error) {
		mu.Lock()
		got = append(got, textOf(env))
		mu.Unlock()
		return "", nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	alice, _ := hcA.Register("alice", WithEnvironment(func(Envelope) (string, error) { return "", nil }))
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	if err := alice.Say(ch.ID, "before"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 1 })

	oldEpoch := hcB.remote.conn.epoch
	if err := hcB.Reconnect(ctx); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if hcB.remote.conn.epoch == oldEpoch {
		t.Fatal("a reconnect must open a new connection generation")
	}
	if err := alice.Say(ch.ID, "after"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 2 })
	if got[1] != "after" {
		t.Fatalf("delivery after the reconnect: %v", got)
	}
	if err := hcB.Call(ctx, OpCanSend, canSendParams{ChannelID: ch.ID, SenderID: bob.AgentID()}, nil); err != nil {
		t.Fatalf("the new connection must act as bob: %v", err)
	}
	// Detach stops delivery without unregistering.
	if err := hcB.Detach(bob.AgentID()); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.hub.Passport("bob"); err != nil {
		t.Fatalf("detach unregistered the agent: %v", err)
	}
	// Attach picks the identity up again on a fresh client, through a hello.
	hcC := rig.connect()
	bob2, err := hcC.Attach("bob", WithEnvironment(func(env Envelope) (string, error) {
		mu.Lock()
		got = append(got, "C:"+textOf(env))
		mu.Unlock()
		return "", nil
	}))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if bob2.AgentID() != bob.AgentID() {
		t.Fatal("attach must keep the identity")
	}
	if err := alice.Say(ch.ID, "third"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 3 })
	if got[2] != "C:third" {
		t.Fatalf("delivery after attach: %v", got)
	}
	if err := hcC.Call(ctx, OpGetAgent, nameOrIDParams{NameOrID: "ghost"}, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("typed error across the wire: %v", err)
	}
	if _, err := hcC.Attach("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("attach to an unknown name: %v", err)
	}
}

// rpcAs sends a request on c that claims to come from somewhere else: From and
// To are filled with whatever the attacker likes. The hub must not care.
func (c *rawConn) rpcAs(from, op, id, params string) transport.Frame {
	c.t.Helper()
	if err := c.link.Send(transport.Frame{Kind: transport.FrameRequest, From: from, To: from, RequestID: id, Op: op, Payload: []byte(params)}); err != nil {
		c.t.Fatal(err)
	}
	for {
		f := c.recv()
		if f.Kind == transport.FrameResponse && f.RequestID == id {
			return f
		}
	}
}

func mustJSON2(v any) string { b, _ := json.Marshal(v); return string(b) }

// The identity of a request is the connection it arrives on. A foreign
// connection that writes the victim's address, endpoint id or agent id into
// Frame.From/To still holds nothing: every op that acts as, or reads the
// private state of, the victim is refused and the victim is untouched.
func TestHubWireForeignConnectionCannotActAsAnother(t *testing.T) {
	rig := newWireRig(t)
	ctx := context.Background()
	hcV := rig.connect() // the victim's connection
	alice, err := hcV.Register("alice", WithEnvironment(func(Envelope) (string, error) { return "", nil }))
	if err != nil {
		t.Fatal(err)
	}
	bob, _ := hcV.Register("bob", WithEnvironment(func(Envelope) (string, error) { return "", nil }))
	aid, bid := alice.AgentID(), bob.AgentID()
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bid)
	if err != nil {
		t.Fatal(err)
	}
	if err := hcV.Call(ctx, OpObserveTask, map[string]any{"metadata": map[string]any{"task_id": "t-v", "owner_id": aid, "channel_id": ch.ID}}, nil); err != nil {
		t.Fatal(err)
	}
	sent, _ := alice.Send(NewTextEnvelope(ch.ID, aid, "", "hello"))

	ws := &rig.hub.ext.wire
	ws.mu.Lock()
	victimEndpoint := ws.agentEP[aid].id
	ws.mu.Unlock()

	mal := rig.raw()
	mal.register("mallory")
	before := mustJSON2(map[string]any{"passports": rig.hub.Registrations(), "rule": mustRule(rig.hub, aid), "resume": mustResume(rig.hub, aid)})

	denied := map[string]string{ // op → params naming the victim's resources
		OpSetResume:               mustJSON2(setResumeParams{AgentID: aid, Resume: Resume{Summary: "pwned"}}),
		OpSetSkill:                mustJSON2(setSkillParams{AgentID: aid}),
		OpSetRule:                 mustJSON2(setRuleParams{AgentID: aid, Rule: Rule{InboundGlobs: []string{"evil"}, OutboundGlobs: []string{"evil"}}}),
		OpAddResumeExample:        mustJSON2(addResumeExampleParams{AgentID: aid, Example: ResumeExample{Title: "pwned"}}),
		OpUnregister:              mustJSON2(agentIDParams{AgentID: aid}),
		OpCreateChannel:           mustJSON2(createChannelParams{Manifest: ChannelManifest{Type: TypeConversation}, Participants: []string{aid, bid}}),
		OpGetChannel:              mustJSON2(channelIDParams{ChannelID: ch.ID}),
		OpCloseChannel:            mustJSON2(closeChannelParams{ChannelID: ch.ID, Reason: "pwned"}),
		OpPostEnvelope:            mustJSON2(postEnvelopeParams{Envelope: NewTextEnvelope(ch.ID, aid, "", "forged")}),
		OpReadWAL:                 mustJSON2(readWALParams{ChannelID: ch.ID}),
		OpFindEnvelopeByCausation: mustJSON2(causationParams{ChannelID: ch.ID, SenderID: aid, CausationID: "c"}),
		OpCanSend:                 mustJSON2(canSendParams{ChannelID: ch.ID, SenderID: aid}),
		OpPendingTurnsFor:         mustJSON2(agentIDParams{AgentID: aid}),
		OpReportTurnFailure:       mustJSON2(turnFailureParams{ChannelID: ch.ID, AgentID: aid, EnvelopeID: sent.ID}),
		OpObserveTask:             mustJSON2(map[string]any{"metadata": map[string]any{"task_id": "t-x", "owner_id": aid}}),
		OpUpdateTask:              mustJSON2(updateTaskParams{TaskID: "t-v", State: "completed"}),
		OpRecordObservation:       mustJSON2(recordObservationParams{OwnerID: aid, Capability: "c", Outcome: "completed"}),
		OpFireTaskEvent:           mustJSON2(fireTaskEventParams{TaskID: "t-v", Kind: TaskEventMirrorFailed, Payload: map[string]any{"owner_id": aid, "channel_id": ch.ID}}),
		OpCheckpointTask:          mustJSON2(checkpointParams{TaskID: "t-v", State: []byte("x")}),
		OpReadTaskCheckpoint:      mustJSON2(checkpointParams{TaskID: "t-v"}),
	}
	open := map[string]string{ // discovery and registration: open to any connection, harmless
		OpRegister:     `{"passport":{"name":"mallory2"}}`,
		OpGetAgent:     `{"name_or_id":"alice"}`,
		OpGetResume:    mustJSON2(agentIDParams{AgentID: aid}),
		OpGetSkill:     mustJSON2(agentIDParams{AgentID: aid}),
		OpGetRule:      mustJSON2(agentIDParams{AgentID: aid}),
		OpFindAgentID:  `{"name":"alice"}`,
		OpNamesFor:     mustJSON2(agentIDsParams{AgentIDs: []string{aid}}),
		OpListAgents:   `{}`,
		OpListChannels: `{}`,
		OpListTasks:    `{}`,
	}
	spoofs := []string{victimEndpoint, aid, "hub", ""}
	for i, op := range WireOps {
		params, isDenied := denied[op]
		if !isDenied {
			if _, isOpen := open[op]; !isOpen && op != OpGetTask {
				t.Errorf("op %s is not classified by the test", op)
				continue
			}
		}
		for j, from := range spoofs {
			id := fmt.Sprintf("s-%d-%d", i, j)
			switch {
			case isDenied:
				if f := mal.rpcAs(from, op, id, params); f.Code != CodeAccessDenied {
					t.Errorf("%s with From=%q: want access_denied, got code %q (%s)", op, from, f.Code, f.Message)
				}
			case op == OpGetTask:
				if f := mal.rpcAs(from, op, id, mustJSON2(taskIDParams{TaskID: "t-v"})); f.Code != CodeTaskNotFound {
					t.Errorf("get_task with From=%q: the task must read as unknown, got %q", from, f.Code)
				}
			default:
				p := open[op]
				if op == OpRegister {
					p = fmt.Sprintf(`{"passport":{"name":"mallory-%d"}}`, j)
				}
				f := mal.rpcAs(from, op, id, p)
				if f.Code != "" {
					t.Errorf("%s with From=%q: open op failed: %s %s", op, from, f.Code, f.Message)
				}
				if op == OpListChannels || op == OpListTasks {
					if got := strings.TrimSpace(string(f.Payload)); got != "[]" {
						t.Errorf("%s leaks the victim's data to a foreign connection: %s", op, got)
					}
				}
			}
		}
	}
	// The victim is untouched.
	after := mustJSON2(map[string]any{"passports": filterOut(rig.hub.Registrations(), "mallory"), "rule": mustRule(rig.hub, aid), "resume": mustResume(rig.hub, aid)})
	if strings.Contains(after, "pwned") || strings.Contains(after, "evil") {
		t.Fatalf("the victim's state was modified: %s", after)
	}
	_ = before
	if got, err := rig.hub.Channel(ch.ID); err != nil || got.State != ChannelActive {
		t.Fatalf("channel disturbed: %+v %v", got, err)
	}
	if log, _ := rig.hub.ReadWAL(ch.ID); countText(log, "forged") != 0 {
		t.Fatal("a forged envelope was accepted")
	}
	if md, err := rig.hub.Task("t-v"); err != nil || md.State == "completed" {
		t.Fatalf("task disturbed: %+v %v", md, err)
	}
	if _, err := rig.hub.Passport(aid); err != nil {
		t.Fatalf("alice was unregistered: %v", err)
	}
	// ... and still holds her own connection: the victim keeps acting as herself.
	if err := hcV.Call(ctx, OpCanSend, canSendParams{ChannelID: ch.ID, SenderID: aid}, nil); err != nil {
		t.Fatalf("the victim lost her binding: %v", err)
	}
	// A receipt that names the victim is ignored whatever its From says.
	mal.send(transport.Frame{Kind: transport.FrameReceipt, From: victimEndpoint, RecipientID: bid, ChannelID: ch.ID, EnvelopeID: sent.ID, Code: ReceiptNack, Message: "forged"})
	mal.rpc(OpGetAgent, "sync", `{"name_or_id":"alice"}`)
	if len(rig.hub.Nacks(bid)) != 0 {
		t.Fatal("a foreign connection recorded a nack for the victim")
	}
}

func mustRule(h *Hub, id string) Rule     { r, _ := h.RuleOf(id); return r }
func mustResume(h *Hub, id string) Resume { r, _ := h.ResumeOf(id); return r }

func filterOut(ps []Passport, prefix string) []Passport {
	var out []Passport
	for _, p := range ps {
		if !strings.HasPrefix(p.Name, prefix) {
			out = append(out, p)
		}
	}
	return out
}

// A second hello does not hand an identity to a stranger: the credentials of
// its scheme decide. (A NoAuth identity has no credentials — as in Python,
// anyone who names it may re-attach; the hello then moves the binding.)
func TestHubWireStrangerHelloNeedsCredentials(t *testing.T) {
	rig := newWireRig(t, WithHubAuth(NewAuthRegistry(NoAuth{}, NewApiKeyAuth(map[string]string{"keyed": "s3cret"}))))
	owner, stranger := rig.raw(), rig.raw()
	reg := owner.rpc(OpRegister, "r", `{"passport":{"name":"keyed","auth":{"scheme":"api_key","claim":{"token":"s3cret"}}}}`)
	if reg.Code != "" {
		t.Fatal(reg.Message)
	}
	var keyed Passport
	_ = json.Unmarshal(reg.Payload, &keyed)
	setRule := func(c *rawConn, id string) transport.Frame {
		return c.rpc(OpSetRule, id, `{"agent_id":"`+keyed.AgentID+`","rule":{"inbound_globs":["*"],"outbound_globs":["*"]}}`)
	}
	for i, body := range []string{
		`{"name":"keyed"}`, // no credentials: scheme "none" ≠ api_key
		`{"name":"keyed","auth_scheme":"api_key"}`,
		`{"name":"keyed","auth_scheme":"api_key","auth_claim":{"token":"guess"}}`,
		`{"name":"keyed","auth_scheme":"none","auth_claim":{"token":"s3cret"}}`,
	} {
		if f := stranger.hello(fmt.Sprintf("h%d", i), body); f.Kind != transport.FrameError || f.Code != CodeAuthFailed {
			t.Fatalf("stranger hello %s: %+v", body, f)
		}
		if f := setRule(stranger, fmt.Sprintf("x%d", i)); f.Code != CodeAccessDenied {
			t.Fatalf("a refused hello must not bind: %+v", f)
		}
		if f := setRule(owner, fmt.Sprintf("o%d", i)); f.Code != "" {
			t.Fatalf("a refused hello displaced the holder: %+v", f)
		}
	}
	// The right credentials do move the binding.
	if f := stranger.hello("ok", `{"name":"keyed","auth_scheme":"api_key","auth_claim":{"token":"s3cret"}}`); f.Kind != transport.FrameWelcome {
		t.Fatalf("right credentials: %+v", f)
	}
	if f := setRule(owner, "late"); f.Code != CodeAccessDenied {
		t.Fatalf("the displaced holder still acts: %+v", f)
	}
	// NoAuth identity: Python parity — a hello that names it re-binds it.
	open := owner.register("open")
	if f := stranger.hello("noauth", `{"name":"open"}`); f.Kind != transport.FrameWelcome {
		t.Fatalf("NoAuth re-attach: %+v", f)
	}
	if f := owner.rpc(OpSetRule, "np", `{"agent_id":"`+open.AgentID+`","rule":{}}`); f.Code != CodeAccessDenied {
		t.Fatalf("NoAuth displacement: %+v", f)
	}
}

// ServeLink returns the error that ended the link (and nil for an orderly end).
type brokenLink struct {
	err error
}

func (b brokenLink) Send(transport.Frame) error                    { return nil }
func (b brokenLink) Recv(context.Context) (transport.Frame, error) { return transport.Frame{}, b.err }
func (b brokenLink) Close() error                                  { return nil }

func TestServeLinkReturnsTransportErrors(t *testing.T) {
	h := newTestHub(t)
	boom := errors.New("socket exploded")
	if err := h.ServeLink(context.Background(), brokenLink{err: boom}); !errors.Is(err, boom) {
		t.Fatalf("ServeLink swallowed the transport error: %v", err)
	}
	if err := h.ServeLink(context.Background(), brokenLink{err: fmt.Errorf("x: %w", transport.ErrLinkClosed)}); err != nil {
		t.Fatalf("an orderly close is not an error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a, _ := transport.NewPipe(4)
	if err := h.ServeLink(ctx, a); err != nil {
		t.Fatalf("a cancelled context is not an error: %v", err)
	}
	if n := len(h.ext.wire.endpoints); n != 0 {
		t.Fatalf("%d endpoints leaked after ServeLink returned", n)
	}
}

// Unregistering an agent releases the connection's claim on it: the id stays
// refused (access_denied, not "not found" from the hub), and the registry
// forgets it.
func TestHubWireUnregisterReleasesTheBinding(t *testing.T) {
	rig := newWireRig(t)
	c := rig.raw()
	alice := c.register("alice")
	if f := c.rpc(OpUnregister, "u", `{"agent_id":"`+alice.AgentID+`"}`); f.Code != "" {
		t.Fatal(f.Message)
	}
	for op, params := range map[string]string{
		OpSetRule:  `{"agent_id":"` + alice.AgentID + `","rule":{}}`,
		OpCanSend:  `{"channel_id":"x","sender_id":"` + alice.AgentID + `"}`,
		OpGetAgent: `{"name_or_id":"` + alice.AgentID + `"}`,
	} {
		f := c.rpc(op, "p-"+op, params)
		want := CodeAccessDenied
		if op == OpGetAgent {
			want = CodeNotFound // open op: the hub simply does not know her any more
		}
		if f.Code != want {
			t.Errorf("%s after unregister: code %q want %q", op, f.Code, want)
		}
	}
	ws := &rig.hub.ext.wire
	ws.mu.Lock()
	defer ws.mu.Unlock()
	for _, ep := range ws.endpoints {
		if ep.bound[alice.AgentID] {
			t.Fatal("the connection still holds the unregistered agent")
		}
	}
	if ws.agentEP[alice.AgentID] != nil {
		t.Fatal("the registry still maps the unregistered agent to a connection")
	}
}

// Receipts free a slot of the recipient's inbox only for substantive
// envelopes it was sent; protocol envelopes never counted, and receipts with
// no channel or envelope attribute nothing.
func TestHubWireReceiptInboxAccounting(t *testing.T) {
	rig := newWireRig(t)
	hcA := rig.connect()
	alice, _ := hcA.Register("alice", WithEnvironment(func(Envelope) (string, error) { return "", nil }))
	dave := rig.raw()
	reg := dave.rpc(OpRegister, "r", `{"passport":{"name":"dave"},"rule":{"inbound_globs":["*"],"outbound_globs":["*"],"inbox":{"max_pending":2}}}`)
	var d Passport
	_ = json.Unmarshal(reg.Payload, &d)
	rec := dave.recordNotifies(d.AgentID)
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, d.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	var invite Envelope
	rec.mu.Lock()
	for _, e := range rec.envs {
		if e.EventType == EnvChannelInvite {
			invite = e
		}
	}
	rec.mu.Unlock()
	e1, _ := alice.Send(NewTextEnvelope(ch.ID, alice.AgentID(), "", "one"))
	e2, _ := alice.Send(NewTextEnvelope(ch.ID, alice.AgentID(), "", "two"))
	if _, err := alice.Send(NewTextEnvelope(ch.ID, alice.AgentID(), "", "three")); !errors.Is(err, ErrInboxFull) {
		t.Fatalf("a full inbox must refuse: %v", err)
	}
	if got := rig.hub.InboxPending(d.AgentID); got != 2 {
		t.Fatalf("pending %d", got)
	}
	ack := func(env string) {
		dave.send(transport.Frame{Kind: transport.FrameReceipt, RecipientID: d.AgentID, ChannelID: ch.ID, EnvelopeID: env, Code: ReceiptAck})
	}
	// Receipts of one connection are processed in order: everything that is
	// meant to have no effect goes first, then the real ack of e1 — once its
	// effect shows, the earlier ones have been processed.
	ack(invite.ID)                                                                                                                                    // a protocol envelope: frees nothing
	ack("ffffffffffffffffffffffffffffffff")                                                                                                           // an envelope the channel never had
	dave.send(transport.Frame{Kind: transport.FrameReceipt, RecipientID: d.AgentID, EnvelopeID: e1.ID, Code: ReceiptAck})                             // no channel
	dave.send(transport.Frame{Kind: transport.FrameReceipt, RecipientID: d.AgentID, ChannelID: ch.ID, Code: ReceiptAck})                              // no envelope
	dave.send(transport.Frame{Kind: transport.FrameReceipt, RecipientID: d.AgentID, ChannelID: "other-channel", EnvelopeID: e1.ID, Code: ReceiptAck}) // wrong channel
	ack(e1.ID)
	waitFor(t, func() bool { return rig.hub.InboxCursor(d.AgentID, ch.ID) == e1.ID })
	if got := rig.hub.InboxPending(d.AgentID); got != 1 {
		t.Fatalf("pending %d: exactly e1's slot must have been freed (cursor %q)", got, rig.hub.InboxCursor(d.AgentID, ch.ID))
	}
	ack(e2.ID)
	waitFor(t, func() bool { return rig.hub.InboxCursor(d.AgentID, ch.ID) == e2.ID })
	if got := rig.hub.InboxPending(d.AgentID); got != 0 {
		t.Fatalf("pending %d after both acks", got)
	}
	if _, err := alice.Send(NewTextEnvelope(ch.ID, alice.AgentID(), "", "three")); err != nil {
		t.Fatalf("a freed inbox accepts again: %v", err)
	}
}

// A reconnect whose re-attach is refused reports it: the client does not
// pretend its agents are bound again.
func TestRemoteReconnectReportsRefusedReattach(t *testing.T) {
	rig := newWireRig(t)
	hc := rig.connect()
	alice, err := hc.Register("alice", WithEnvironment(func(Envelope) (string, error) { return "", nil }))
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.hub.Unregister(alice.AgentID()); err != nil { // gone behind the client's back
		t.Fatal(err)
	}
	err = hc.Reconnect(context.Background())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("reconnect after the identity vanished: %v", err)
	}
	if !strings.Contains(err.Error(), "alice") {
		t.Fatalf("the error should name the agent: %v", err)
	}
}

// Attach to a name whose hello the hub refuses fails and binds nothing.
func TestRemoteAttachRefusedHello(t *testing.T) {
	rig := newWireRig(t, WithHubAuth(NewAuthRegistry(NoAuth{}, NewApiKeyAuth(map[string]string{"keyed": "s3cret"}))))
	owner := rig.connect()
	if _, err := owner.Register("keyed", WithAgentAuth("api_key", map[string]any{"token": "s3cret"})); err != nil {
		t.Fatalf("register keyed: %v", err)
	}
	other := rig.connect()
	if _, err := other.Attach("keyed", WithAgentAuth("api_key", map[string]any{"token": "nope"})); !errors.Is(err, ErrAuth) {
		t.Fatalf("attach with a wrong token: %v", err)
	}
	if _, err := other.Attach("keyed"); !errors.Is(err, ErrAuth) {
		t.Fatalf("attach without credentials: %v", err)
	}
	if err := other.Call(context.Background(), OpSetRule, setRuleParams{AgentID: mustPassportID(t, rig.hub, "keyed"), Rule: DefaultRule()}, nil); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("a refused attach must not bind: %v", err)
	}
	if a, err := other.Attach("keyed", WithAgentAuth("api_key", map[string]any{"token": "s3cret"})); err != nil || a == nil {
		t.Fatalf("attach with the token: %v", err)
	}
}

func mustPassportID(t *testing.T, h *Hub, name string) string {
	t.Helper()
	p, err := h.Passport(name)
	if err != nil {
		t.Fatal(err)
	}
	return p.AgentID
}
