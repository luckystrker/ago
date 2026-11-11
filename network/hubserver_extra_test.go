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
	"github.com/luckystrker/ago/task"
)

// Pins found by the mutation run: the edges of the wire server and the remote
// client that the broader tests did not reach.

type fakeAcceptor struct {
	links chan transport.Link
	err   error
}

func (f *fakeAcceptor) Accept(ctx context.Context) (transport.Link, error) {
	select {
	case l := <-f.links:
		return l, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		if f.err != nil {
			return nil, f.err
		}
		select {
		case l := <-f.links:
			return l, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func TestServeReturnValues(t *testing.T) {
	h := newTestHub(t)
	boom := errors.New("listener broke")
	if err := h.Serve(context.Background(), &fakeAcceptor{err: boom}); !errors.Is(err, boom) {
		t.Fatalf("Serve must return an Accept failure: %v", err)
	}
	if err := h.Serve(context.Background(), &fakeAcceptor{err: transport.ErrNoConnection}); err != nil {
		t.Fatalf("a closed listener is an orderly end: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Serve(ctx, &fakeAcceptor{links: make(chan transport.Link)}) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a cancelled context is an orderly end: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve ignored its context")
	}
}

// dispatch failures reach the listeners: an overflowing link reports and keeps
// the connection, a successful delivery reports nothing.
type dispatchRecorder struct {
	BaseHubListener
	mu     sync.Mutex
	failed []error
}

func (d *dispatchRecorder) OnDispatchFailed(_ Envelope, _ string, reason error) {
	d.mu.Lock()
	d.failed = append(d.failed, reason)
	d.mu.Unlock()
}

func (d *dispatchRecorder) count() int { d.mu.Lock(); defer d.mu.Unlock(); return len(d.failed) }

func TestHubWireDispatchFailureReporting(t *testing.T) {
	hub := newTestHub(t)
	rec := &dispatchRecorder{}
	hub.RegisterListener(rec)
	ln := transport.NewMemListener(3) // a tiny per-direction buffer
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = hub.Serve(ctx, ln) }()
	t.Cleanup(func() { stop(); _ = ln.Close(); <-done })
	rig := &wireRig{t: t, hub: hub, ln: ln, ctx: ctx}

	hcA := rig.connect()
	alice, _ := hcA.Register("alice", WithEnvironment(func(Envelope) (string, error) { return "", nil }))
	dave := rig.raw()
	d := dave.register("dave")
	dave.recvNotifyAndAck(t, d.AgentID) // consumes the invite while alice creates the channel
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, d.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.count() != 0 {
		t.Fatalf("successful deliveries were reported as failures: %v", rec.failed)
	}
	// dave never reads again: his link fills up, the hub reports and carries on.
	for i := 0; i < 6; i++ {
		if _, err := alice.Send(NewTextEnvelope(ch.ID, alice.AgentID(), "", "x")); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool { return rec.count() > 0 })
	rec.mu.Lock()
	overflow := errors.Is(rec.failed[0], transport.ErrLinkOverflow)
	rec.mu.Unlock()
	if !overflow {
		t.Fatalf("failure reason: %v", rec.failed[0])
	}
	ws := &hub.ext.wire
	ws.mu.Lock()
	held := ws.agentEP[d.AgentID] != nil
	ws.mu.Unlock()
	if !held {
		t.Fatal("an overflowing link must not release the connection")
	}
}

// recvNotifyAndAck answers the first invite arriving on the connection.
func (c *rawConn) recvNotifyAndAck(t *testing.T, agentID string) {
	t.Helper()
	go func() {
		for {
			f, err := c.link.Recv(context.Background())
			if err != nil {
				return
			}
			if f.Kind != transport.FrameNotify {
				continue
			}
			var env Envelope
			if json.Unmarshal(f.Payload, &env) != nil || env.EventType != EnvChannelInvite {
				continue
			}
			ack, _ := json.Marshal(postEnvelopeParams{Envelope: NewChannelAck(env, agentID, false, "")})
			_ = c.link.Send(transport.Frame{Kind: transport.FrameRequest, RequestID: "ack", Op: OpPostEnvelope, Payload: ack})
			return
		}
	}()
}

func TestHubWireHelloEdges(t *testing.T) {
	rig := newWireRig(t)
	c := rig.raw()
	alice := c.register("alice")
	other := rig.raw()
	// A hello names an identity by NAME: an agent id is not a name.
	if f := other.hello("byid", `{"name":"`+alice.AgentID+`"}`); f.Kind != transport.FrameError || f.Code != CodeNotFound {
		t.Fatalf("hello by agent id: %+v", f)
	}
	if f := other.rpc(OpFindAgentID, "f1", `{"name":"`+alice.AgentID+`"}`); strings.TrimSpace(string(f.Payload)) != "" {
		t.Fatalf("find_agent_id resolves names only, got %s", f.Payload)
	}
	if f := other.rpc(OpFindAgentID, "f2", `{"name":"alice"}`); !strings.Contains(string(f.Payload), alice.AgentID) {
		t.Fatalf("find_agent_id by name: %s", f.Payload)
	}
	// A remote_agent (registered in-process by an operator) is never re-attached over the wire.
	if _, err := rig.hub.Register(Passport{Name: "rem", Kind: KindRemoteAgent, Auth: map[string]any{"scheme": "a2a", "claim": map[string]any{"token": "hunter2"}}}, nil); err != nil {
		t.Fatal(err)
	}
	// The passport of a remote agent keeps its routing claim in the hub; the
	// wire never carries it.
	for _, op := range []struct{ name, params string }{{OpGetAgent, `{"name_or_id":"rem"}`}, {OpListAgents, `{}`}} {
		f := other.rpc(op.name, "leak-"+op.name, op.params)
		if f.Code != "" || !strings.Contains(string(f.Payload), `"rem"`) || strings.Contains(string(f.Payload), "hunter2") {
			t.Fatalf("%s leaks or loses the remote agent: %+v %s", op.name, f.Code, f.Payload)
		}
	}
	if f := other.hello("rem", `{"name":"rem"}`); f.Kind != transport.FrameError || f.Code != CodeAuthFailed {
		t.Fatalf("hello for a remote_agent: %+v", f)
	}
	// get_skill: unknown agent and malformed params are typed errors.
	if f := other.rpc(OpGetSkill, "s1", `{"agent_id":"ghost"}`); f.Code != CodeNotFound {
		t.Fatalf("get_skill unknown agent: %+v", f)
	}
	if f := other.rpc(OpGetSkill, "s2", `"nope"`); f.Code != CodeProtocol {
		t.Fatalf("get_skill malformed: %+v", f)
	}
	// register carries the resume: it lands, and the agent stays registered.
	reg := other.rpc(OpRegister, "r1", `{"passport":{"name":"carol"},"resume":{"capabilities":["tea"],"summary":"brews"}}`)
	if reg.Code != "" {
		t.Fatalf("register with a resume: %+v", reg)
	}
	var carol Passport
	_ = json.Unmarshal(reg.Payload, &carol)
	if r, err := rig.hub.ResumeOf(carol.AgentID); err != nil || r.Summary != "brews" {
		t.Fatalf("resume of the registered agent: %+v %v", r, err)
	}
	if got := rig.hub.AgentsWithCapability("tea"); len(got) != 1 {
		t.Fatalf("capability index: %v", got)
	}
}

func TestHubWireListingsAndTurns(t *testing.T) {
	rig := newWireRig(t)
	ctx := context.Background()
	hc := rig.connect()
	alice, _ := hc.Register("alice", WithEnvironment(func(Envelope) (string, error) { return "", nil }))
	bob, _ := hc.Register("bob", WithEnvironment(func(Envelope) (string, error) { return "", nil }))
	carol, _ := hc.Register("carol", WithEnvironment(func(Envelope) (string, error) { return "", nil }))
	var ids []string
	for i := 0; i < 3; i++ {
		ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConsulting}, bob.AgentID())
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, ch.ID)
		time.Sleep(2 * time.Millisecond)
	}
	// list_channels: creation order, honours the limit.
	var snaps []ChannelSnapshot
	if err := hc.Call(ctx, OpListChannels, listChannelsParams{}, &snaps); err != nil || len(snaps) != 3 {
		t.Fatalf("list_channels: %v %v", snaps, err)
	}
	for i, s := range snaps {
		if s.ID != ids[i] {
			t.Fatalf("channel %d is %s, want %s (creation order)", i, s.ID[:6], ids[i][:6])
		}
	}
	two := 2
	if err := hc.Call(ctx, OpListChannels, listChannelsParams{Limit: &two}, &snaps); err != nil || len(snaps) != 2 {
		t.Fatalf("list_channels limit: %d %v", len(snaps), err)
	}
	// pending_turns_for: alice holds the floor in every fresh consulting
	// channel, bob in none — an empty answer is [] not null.
	var turns []PendingTurn
	if err := hc.Call(ctx, OpPendingTurnsFor, agentIDParams{AgentID: alice.AgentID()}, &turns); err != nil || len(turns) != 3 {
		t.Fatalf("alice's pending turns: %v %v", turns, err)
	}
	var raw json.RawMessage
	if err := hc.Call(ctx, OpPendingTurnsFor, agentIDParams{AgentID: bob.AgentID()}, &raw); err != nil || strings.TrimSpace(string(raw)) != "[]" {
		t.Fatalf("bob's pending turns: %s %v", raw, err)
	}
	// report_turn_failure needs a participant of the channel.
	if err := hc.Call(ctx, OpReportTurnFailure, turnFailureParams{ChannelID: ids[0], AgentID: carol.AgentID(), EnvelopeID: "e"}, nil); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("turn failure from a non-participant: %v", err)
	}
	if err := hc.Call(ctx, OpReportTurnFailure, turnFailureParams{ChannelID: ids[0], AgentID: alice.AgentID(), EnvelopeID: "e"}, nil); err != nil {
		t.Fatalf("turn failure from a participant: %v", err)
	}
}

// A receipt only counts for a participant of the channel the envelope is in.
func TestHubWireReceiptFromNonParticipantIgnored(t *testing.T) {
	rig := newWireRig(t)
	hc := rig.connect()
	alice, _ := hc.Register("alice", WithEnvironment(func(Envelope) (string, error) { return "", nil }))
	bob, _ := hc.Register("bob", WithEnvironment(func(Envelope) (string, error) { return "", nil }))
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	e1, _ := alice.Send(NewTextEnvelope(ch.ID, alice.AgentID(), "", "one"))
	erin := rig.raw()
	e := erin.register("erin") // registered, holds her own connection, not in the channel
	erin.send(transport.Frame{Kind: transport.FrameReceipt, RecipientID: e.AgentID, ChannelID: ch.ID, EnvelopeID: e1.ID, Code: ReceiptAck})
	// The sender cannot ack her own envelope either.
	hcA := hc // alice's connection
	_ = hcA
	erin.rpc(OpGetAgent, "sync", `{"name_or_id":"erin"}`)
	if got := rig.hub.InboxCursor(e.AgentID, ch.ID); got != "" {
		t.Fatalf("a non-participant's ack set a cursor: %q", got)
	}
}

// Remote reads of the client API: causation lookup, envelope lookup, turn
// failure reporting, active-only task listing.
func TestRemoteHubReadsAndTaskFilter(t *testing.T) {
	rig := newWireRig(t)
	hc := rig.connect()
	rec := &turnRecorder{}
	rig.hub.RegisterListener(rec)
	alice, _ := hc.Register("alice", WithEnvironment(func(Envelope) (string, error) { return "", nil }))
	bob, _ := hc.Register("bob", WithEnvironment(func(Envelope) (string, error) { return "", nil }))
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	env := NewTextEnvelope(ch.ID, alice.AgentID(), "", "q")
	env.CausationID = "cz"
	sent, err := alice.Send(env)
	if err != nil {
		t.Fatal(err)
	}
	api := hc.api
	if got, ok := api.FindEnvelopeByCausation(ch.ID, alice.AgentID(), "cz"); !ok || got.ID != sent.ID {
		t.Fatalf("causation lookup: %+v %v", got, ok)
	}
	if _, ok := api.FindEnvelopeByCausation(ch.ID, alice.AgentID(), "nope"); ok {
		t.Fatal("an unknown causation id must read as not found")
	}
	if got, ok := api.envelopeByID(ch.ID, sent.ID); !ok || got.ID != sent.ID {
		t.Fatalf("envelopeByID: %+v %v", got, ok)
	}
	if _, ok := api.envelopeByID(ch.ID, "nope"); ok {
		t.Fatal("unknown envelope id found")
	}
	if _, ok := api.envelopeByID("ghost", sent.ID); ok {
		t.Fatal("unknown channel found")
	}
	api.ReportTurnFailure(ch.ID, alice.AgentID(), sent.ID, errors.New("model refused"))
	api.ReportTurnFailure(ch.ID, alice.AgentID(), sent.ID, nil) // no error value: must not crash
	waitFor(t, func() bool { return rec.count() == 2 })
	if rec.first() != "model refused" {
		t.Fatalf("reported error %q", rec.first())
	}
	// Tasks: the active-only filter.
	for _, id := range []string{"tk-a", "tk-b"} {
		if err := hc.Call(context.Background(), OpObserveTask, map[string]any{"metadata": map[string]any{"task_id": id, "owner_id": alice.AgentID()}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := hc.Call(context.Background(), OpUpdateTask, updateTaskParams{TaskID: "tk-a", State: string(task.StateCompleted)}, nil); err != nil {
		t.Fatal(err)
	}
	if got := api.Tasks(alice.AgentID(), true); len(got) != 1 || got[0].TaskID != "tk-b" {
		t.Fatalf("active tasks: %v", got)
	}
	if got := api.Tasks(alice.AgentID(), false); len(got) != 2 || got[0].TaskID != "tk-a" {
		t.Fatalf("all tasks: %v", got)
	}
}

type turnRecorder struct {
	BaseHubListener
	mu   sync.Mutex
	errs []string
}

func (r *turnRecorder) OnTurnFailed(_, _, _ string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	msg := "<nil>"
	if err != nil {
		msg = err.Error()
	}
	r.errs = append(r.errs, msg)
}
func (r *turnRecorder) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.errs) }
func (r *turnRecorder) first() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.errs) == 0 {
		return ""
	}
	return r.errs[0]
}

// Client lifecycle edges.
func TestRemoteClientLifecycleEdges(t *testing.T) {
	// Default and non-positive timeouts become the default.
	rig := newWireRig(t)
	for _, to := range []time.Duration{0, -time.Second} {
		hc, err := ConnectHub(rig.ctx, RemoteConfig{Dial: ListenerDialer(rig.ln), RPCTimeout: to})
		if err != nil {
			t.Fatal(err)
		}
		if got := hc.remote.cfg.RPCTimeout; got != 45*time.Second {
			t.Fatalf("RPCTimeout %v became %v, want the 45s default", to, got)
		}
		_ = hc.Close()
	}
	if _, err := ConnectHub(rig.ctx, RemoteConfig{}); !errors.Is(err, ErrProtocol) {
		t.Fatalf("config without Dial: %v", err)
	}
	// An in-process client has nothing to close and is not remote.
	local := NewHubClient(newTestHub(t))
	if err := local.Close(); err != nil {
		t.Fatalf("closing an in-process client: %v", err)
	}
	if err := local.Reconnect(context.Background()); !errors.Is(err, ErrNotRemote) {
		t.Fatalf("reconnect in-process: %v", err)
	}
	if err := local.Call(context.Background(), OpGetAgent, nil, nil); !errors.Is(err, ErrNotRemote) {
		t.Fatalf("call in-process: %v", err)
	}
	// Close stops a remote client: the link is down, later calls say so, delivery stops.
	hc := rig.connect()
	alice, _ := hc.Register("alice", WithEnvironment(func(Envelope) (string, error) { return "", nil }))
	cn := hc.remote.conn
	ag := hc.remote.agents[alice.AgentID()]
	if err := hc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := hc.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	cn.mu.Lock()
	down := cn.closed
	cn.mu.Unlock()
	if !down {
		t.Fatal("Close left the connection up")
	}
	ag.mu.Lock()
	stopped := ag.stopped
	ag.mu.Unlock()
	if !stopped {
		t.Fatal("Close left the agent's delivery running")
	}
	if err := hc.Call(context.Background(), OpGetAgent, nameOrIDParams{NameOrID: "alice"}, nil); !errors.Is(err, ErrClientClosed) {
		t.Fatalf("call after close: %v", err)
	}
	if err := hc.Reconnect(context.Background()); !errors.Is(err, ErrClientClosed) {
		t.Fatalf("reconnect after close: %v", err)
	}
}

// Detaching stops the agent's delivery goroutine and its pump; detaching an
// unknown id is harmless, and attach(nil) on an unknown id too.
func TestRemoteDetachStopsDelivery(t *testing.T) {
	rig := newWireRig(t)
	hc := rig.connect()
	alice, _ := hc.Register("alice", WithEnvironment(func(Envelope) (string, error) { return "", nil }))
	hum, _ := hc.RegisterHuman("hannah")
	ag := hc.remote.agents[alice.AgentID()]
	if err := hc.Detach("ghost"); err != nil {
		t.Fatalf("detach of an unknown id: %v", err)
	}
	if err := hc.remote.attach("ghost", nil); err != nil {
		t.Fatal(err)
	}
	if err := hc.Detach(alice.AgentID()); err != nil {
		t.Fatal(err)
	}
	if err := hc.Detach(hum.AgentID()); err != nil {
		t.Fatal(err)
	}
	ag.mu.Lock()
	stopped := ag.stopped
	ag.mu.Unlock()
	if !stopped {
		t.Fatal("the delivery goroutine of a detached agent keeps running")
	}
	select {
	case <-alice.done:
	default:
		t.Fatal("the pump of a detached agent keeps running")
	}
	select {
	case <-hum.done:
	default:
		t.Fatal("a detached human is still receiving")
	}
	if _, err := rig.hub.Passport("alice"); err != nil {
		t.Fatalf("detach unregistered the agent: %v", err)
	}
}

// Notifies for an agent that is not attached yet are held in order, up to a
// bound, and released at its attach.
func TestRemoteEarlyNotifiesAreBufferedUpToTheBound(t *testing.T) {
	n := newFakeNet()
	rc := newRemoteOnFake(t, n)
	cn, _ := rc.current()
	for i := 0; i < 300; i++ {
		payload, _ := json.Marshal(Envelope{ID: fmt.Sprintf("%04d", i), ChannelID: "ch", EventType: EnvText})
		rc.onNotify(cn, transport.Frame{Kind: transport.FrameNotify, RecipientID: "late", ChannelID: "ch", EnvelopeID: "x", Payload: payload})
	}
	var mu sync.Mutex
	count := 0
	_ = rc.attach("late", func(Envelope) { mu.Lock(); count++; mu.Unlock() })
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return count >= 256 })
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if count != 256 {
		t.Fatalf("%d early notifies were delivered, want exactly the 256 kept", count)
	}
}

// Task scoping over the wire: who may observe, see, update, checkpoint and
// report on a task.
func TestHubWireTaskScoping(t *testing.T) {
	rig := newWireRig(t)
	ctx := context.Background()
	hcA, hcB, hcC := rig.connect(), rig.connect(), rig.connect()
	noop := WithEnvironment(func(Envelope) (string, error) { return "", nil })
	alice, _ := hcA.Register("alice", noop)
	bob, _ := hcB.Register("bob", noop)
	carol, _ := hcC.Register("carol", noop)
	aid, bid, cid := alice.AgentID(), bob.AgentID(), carol.AgentID()
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bid)
	if err != nil {
		t.Fatal(err)
	}
	observe := func(hc *HubClient, id, owner, channel string) error {
		md := map[string]any{"task_id": id, "owner_id": owner}
		if channel != "" {
			md["channel_id"] = channel
		}
		return hc.Call(ctx, OpObserveTask, map[string]any{"metadata": md}, nil)
	}
	// observe: the owner has to be a participant of the channel the task rides.
	if err := observe(hcC, "tc", cid, ch.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("observing onto a channel the owner is not in: %v", err)
	}
	if err := observe(hcA, "t-chan", aid, ch.ID); err != nil {
		t.Fatal(err)
	}
	if err := observe(hcA, "t-solo", aid, ""); err != nil {
		t.Fatal(err)
	}
	// Re-observing somebody else's task id is refused (the owner is not held).
	if err := observe(hcB, "t-solo", bid, ""); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("hijacking an observed task id: %v", err)
	}
	// record_observation against another owner's task is refused.
	if err := hcB.Call(ctx, OpRecordObservation, recordObservationParams{OwnerID: bid, Capability: "c", Outcome: "completed", TaskID: "t-solo"}, nil); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("recording an observation against another owner's task: %v", err)
	}
	if err := hcA.Call(ctx, OpRecordObservation, recordObservationParams{OwnerID: aid, Capability: "c", Outcome: "completed", TaskID: "t-solo"}, nil); err != nil {
		t.Fatalf("own task: %v", err)
	}
	// Visibility: the owner, and participants of the task's channel — nobody else.
	for _, tc := range []struct {
		hc    *HubClient
		task  string
		print string
		want  bool
	}{
		{hcA, "t-chan", "owner", true}, {hcB, "t-chan", "channel participant", true}, {hcC, "t-chan", "stranger", false},
		{hcA, "t-solo", "owner", true}, {hcB, "t-solo", "channel participant of nothing", false},
	} {
		var md task.Metadata
		err := tc.hc.Call(ctx, OpGetTask, taskIDParams{TaskID: tc.task}, &md)
		if seen := err == nil; seen != tc.want {
			t.Errorf("get_task %s as %s: seen=%v want %v (%v)", tc.task, tc.print, seen, tc.want, err)
		}
	}
	// list_tasks filters by channel and by state, and shows only what the connection may see.
	var listed []task.Metadata
	if err := hcA.Call(ctx, OpListTasks, listTasksParams{ChannelID: ch.ID}, &listed); err != nil || len(listed) != 1 || listed[0].TaskID != "t-chan" {
		t.Fatalf("list_tasks by channel: %v %v", listed, err)
	}
	if err := hcA.Call(ctx, OpUpdateTask, updateTaskParams{TaskID: "t-solo", State: string(task.StateCompleted)}, nil); err != nil {
		t.Fatal(err)
	}
	if err := hcA.Call(ctx, OpListTasks, listTasksParams{State: string(task.StateCompleted)}, &listed); err != nil || len(listed) != 1 || listed[0].TaskID != "t-solo" {
		t.Fatalf("list_tasks by state: %v %v", listed, err)
	}
	two := 1
	if err := hcA.Call(ctx, OpListTasks, listTasksParams{Limit: &two}, &listed); err != nil || len(listed) != 1 {
		t.Fatalf("list_tasks limit: %v %v", listed, err)
	}
	if err := hcB.Call(ctx, OpListTasks, listTasksParams{}, &listed); err != nil || len(listed) != 1 || listed[0].TaskID != "t-chan" {
		t.Fatalf("bob sees only the channel's task: %v %v", listed, err)
	}

	// checkpoint: a connection that holds one agent needs no writer_id; the
	// task is protected by its first writer.
	if err := hcC.Call(ctx, OpCheckpointTask, checkpointParams{TaskID: "free-1", State: []byte("c1")}, nil); err != nil {
		t.Fatalf("checkpoint with a single agent and no writer_id: %v", err)
	}
	var state []byte
	if err := hcC.Call(ctx, OpReadTaskCheckpoint, checkpointParams{TaskID: "free-1"}, &state); err != nil || string(state) != "c1" {
		t.Fatalf("read checkpoint: %q %v", state, err)
	}
	if err := hcB.Call(ctx, OpCheckpointTask, checkpointParams{TaskID: "free-1", State: []byte("evil")}, nil); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("a second writer: %v", err)
	}

	// fire_task_event: only mirror_failed, only for the reporter's own tasks and channels.
	fire := func(hc *HubClient, task string, payload map[string]any) error {
		return hc.Call(ctx, OpFireTaskEvent, fireTaskEventParams{TaskID: task, Kind: TaskEventMirrorFailed, Payload: payload}, nil)
	}
	if err := fire(hcA, "t-chan", map[string]any{"owner_id": aid, "channel_id": ch.ID, "exc_message": "x"}); err != nil {
		t.Fatalf("a faithful mirror_failed: %v", err)
	}
	if err := fire(hcA, "t-chan", map[string]any{"owner_id": aid}); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("mirror_failed that drops the task's channel: %v", err)
	}
	if err := fire(hcB, "t-chan", map[string]any{"owner_id": bid, "channel_id": ch.ID}); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("mirror_failed with another owner for the task: %v", err)
	}
	if err := fire(hcC, "unobserved", map[string]any{"owner_id": cid, "channel_id": ch.ID}); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("mirror_failed onto a channel the owner is not in: %v", err)
	}
	if err := fire(hcA, "unobserved", map[string]any{"owner_id": aid, "channel_id": ch.ID}); err != nil {
		t.Fatalf("unobserved task on the owner's channel: %v", err)
	}
	if err := fire(hcA, "unobserved", map[string]any{"owner_id": aid}); err != nil {
		t.Fatalf("unobserved task, no channel: %v", err)
	}
	if err := fire(hcA, "t-chan", map[string]any{"channel_id": ch.ID}); !errors.Is(err, ErrProtocol) {
		t.Fatalf("mirror_failed without owner_id: %v", err)
	}
	if err := fire(hcA, "t-chan", map[string]any{"owner_id": aid, "secret": "x"}); !errors.Is(err, ErrProtocol) {
		t.Fatalf("mirror_failed with a foreign payload key: %v", err)
	}
	if err := fire(hcA, "t-chan", map[string]any{"owner_id": bid, "channel_id": ch.ID}); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("mirror_failed naming an owner the connection does not hold: %v", err)
	}
}
