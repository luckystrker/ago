package network

import (
	"errors"
	"strings"
	"testing"

	"github.com/luckystrker/ago/task"
)

// peerCancelFixture: alice owns task t-1 on a conversation with bob; carol
// and dave are registered but not members of the channel.
type peerCancelFixture struct {
	h                       *Hub
	alice, bob, carol, dave string
	ch                      string
	rec                     *recorder
}

func newPeerCancelFixture(t *testing.T, opts ...HubOption) *peerCancelFixture {
	t.Helper()
	h := newTestHub(t, opts...)
	ids := registerAgents(t, h, "alice", "bob", "carol", "dave")
	ch, err := h.CreateChannel(ChannelManifest{Type: TypeConversation}, ids[:2])
	if err != nil {
		t.Fatal(err)
	}
	if err := h.ObserveTask(task.Metadata{TaskID: "t-1", OwnerID: ids[0], ChannelID: ch.ID, State: task.StateRunning}); err != nil {
		t.Fatal(err)
	}
	return &peerCancelFixture{h: h, alice: ids[0], bob: ids[1], carol: ids[2], dave: ids[3], ch: ch.ID}
}

func (f *peerCancelFixture) cancel(sender, audience, taskID string, payload map[string]any) error {
	env := Envelope{
		ChannelID: f.ch, SenderID: sender, Audience: audience, EventType: EnvTaskCancelRequest,
		TaskID: taskID, Payload: mustJSON(payload),
	}
	_, err := f.h.Send(env)
	return err
}

func okPayload() map[string]any { return map[string]any{"task_id": "t-1", "reason": "obsolete"} }

func TestPeerCancelRequestFromNonParticipantAccepted(t *testing.T) {
	f := newPeerCancelFixture(t)
	if err := f.cancel(f.carol, f.alice, "t-1", okPayload()); err != nil {
		t.Fatalf("well-formed peer cancel: %v", err)
	}
	wal, _ := f.h.ReadWAL(f.ch)
	last := wal[len(wal)-1]
	if last.EventType != EnvTaskCancelRequest || last.SenderID != f.carol || last.TaskID != "t-1" {
		t.Fatalf("not recorded: %+v", last)
	}
}

func TestPeerCancelOnePerSender(t *testing.T) {
	f := newPeerCancelFixture(t)
	if err := f.cancel(f.carol, f.alice, "t-1", okPayload()); err != nil {
		t.Fatal(err)
	}
	if err := f.cancel(f.carol, f.alice, "t-1", okPayload()); !errors.Is(err, ErrProtocol) {
		t.Fatalf("second request from the same peer: %v", err)
	}
	if err := f.cancel(f.dave, f.alice, "t-1", okPayload()); err != nil {
		t.Fatalf("another peer still gets its one request: %v", err)
	}
	// The per-task bookkeeping is released when the task ends.
	f.h.mu.Lock()
	tracked := len(f.h.ext.peerCancel["t-1"])
	f.h.mu.Unlock()
	if tracked != 2 {
		t.Fatalf("tracked senders: %d", tracked)
	}
	_ = f.h.UpdateTask("t-1", task.StateCompleted, nil, "done", "")
	f.h.mu.Lock()
	_, left := f.h.ext.peerCancel["t-1"]
	f.h.mu.Unlock()
	if left {
		t.Fatal("peer-cancel state outlived its task")
	}
}

func TestPeerCancelRejectsMalformedRequests(t *testing.T) {
	long := strings.Repeat("x", maxWireText+1)
	cases := map[string]struct {
		audience string
		taskID   string
		payload  map[string]any
	}{
		"audience is not the owner": {"bob", "t-1", okPayload()},
		"broadcast":                 {"", "t-1", okPayload()},
		"unknown task":              {"alice", "t-404", map[string]any{"task_id": "t-404", "reason": "x"}},
		"missing task id":           {"alice", "", okPayload()},
		"payload names another":     {"alice", "t-1", map[string]any{"task_id": "t-2", "reason": "x"}},
		"extra payload key":         {"alice", "t-1", map[string]any{"task_id": "t-1", "reason": "x", "evil": 1}},
		"missing reason":            {"alice", "t-1", map[string]any{"task_id": "t-1"}},
		"non-string reason":         {"alice", "t-1", map[string]any{"task_id": "t-1", "reason": 7}},
		"oversized reason":          {"alice", "t-1", map[string]any{"task_id": "t-1", "reason": long}},
	}
	for name, tc := range cases {
		f := newPeerCancelFixture(t)
		audience := map[string]string{"alice": f.alice, "bob": f.bob, "": ""}[tc.audience]
		before, _ := f.h.ReadWAL(f.ch)
		if err := f.cancel(f.carol, audience, tc.taskID, tc.payload); !errors.Is(err, ErrProtocol) {
			t.Errorf("%s: want ErrProtocol, got %v", name, err)
		}
		if after, _ := f.h.ReadWAL(f.ch); len(after) != len(before) {
			t.Errorf("%s: rejected request leaked into the WAL", name)
		}
	}
}

func TestPeerCancelOnlyForLiveTaskOnItsOwnChannel(t *testing.T) {
	f := newPeerCancelFixture(t)
	// Another channel: the task does not ride it.
	other, err := f.h.CreateChannel(ChannelManifest{Type: TypeConversation}, []string{f.alice, f.bob})
	if err != nil {
		t.Fatal(err)
	}
	env := Envelope{ChannelID: other.ID, SenderID: f.carol, Audience: f.alice, EventType: EnvTaskCancelRequest,
		TaskID: "t-1", Payload: mustJSON(okPayload())}
	if _, err := f.h.Send(env); !errors.Is(err, ErrProtocol) {
		t.Fatalf("task on another channel: %v", err)
	}
	// A terminal task cannot be cancelled by anyone.
	_ = f.h.UpdateTask("t-1", task.StateCompleted, nil, "done", "")
	if err := f.cancel(f.carol, f.alice, "t-1", okPayload()); !errors.Is(err, ErrProtocol) {
		t.Fatalf("terminal task: %v", err)
	}
	// Only cancel requests get the exemption: any other event from a stranger
	// is still refused.
	if _, err := f.h.Send(NewTextEnvelope(f.ch, f.carol, "", "hi")); !errors.Is(err, ErrProtocol) {
		t.Fatalf("text from a non-participant: %v", err)
	}
}

func TestPeerCancelParticipantsAreNotLimited(t *testing.T) {
	f := newPeerCancelFixture(t)
	// Participants are not limited: bob (a member) may send as many cancel
	// requests as he likes, shaped or not.
	for i := 0; i < 3; i++ {
		if err := f.cancel(f.bob, f.alice, "t-1", okPayload()); err != nil {
			t.Fatalf("participant request %d: %v", i, err)
		}
	}
}

// The tasks tool's cancel posts the shape the validator expects, so the two
// ends agree.
func TestTasksToolCancelCarriesTaskID(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)
	owner, _ := hc.Register("owner")
	asker, _ := hc.Register("asker")
	defer func() { _ = owner.Close(); _ = asker.Close() }()
	ch, err := asker.CreateChannel(ChannelManifest{Type: TypeConversation}, owner.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	_ = h.ObserveTask(task.Metadata{TaskID: "t-9", OwnerID: owner.AgentID(), ChannelID: ch.ID, State: task.StateRunning})
	set, err := ClientTools(hc, asker)
	if err != nil {
		t.Fatal(err)
	}
	callTool(t, set, "tasks", `{"action":"cancel","task_id":"t-9","reason":"stop"}`)
	wal, _ := h.ReadWAL(ch.ID)
	var found bool
	for _, e := range wal {
		if e.EventType == EnvTaskCancelRequest && e.TaskID == "t-9" {
			found = true
		}
	}
	if !found {
		t.Fatal("the tasks tool must stamp Envelope.TaskID on the cancel request")
	}
}

// A hub-driven expiry (channel closed) releases the bookkeeping too.
func TestPeerCancelStateReleasedWhenChannelCascadesTask(t *testing.T) {
	f := newPeerCancelFixture(t)
	if err := f.cancel(f.carol, f.alice, "t-1", okPayload()); err != nil {
		t.Fatal(err)
	}
	if err := f.h.CloseChannel(f.ch, "done"); err != nil {
		t.Fatal(err)
	}
	if md, _ := f.h.Task("t-1"); md.State != task.StateExpired {
		t.Fatalf("cascade: %s", md.State)
	}
	f.h.mu.Lock()
	_, left := f.h.ext.peerCancel["t-1"]
	f.h.mu.Unlock()
	if left {
		t.Fatal("peer-cancel state outlived the cascaded task")
	}
}

// The one-request-per-sender limit survives a restart: Replay rebuilds it
// from the cancel requests in the WAL.
func TestPeerCancelLimitRebuiltOnReplay(t *testing.T) {
	wal := NewMemoryWAL()
	f := newPeerCancelFixture(t, WithHubWAL(wal))
	if err := f.cancel(f.carol, f.alice, "t-1", okPayload()); err != nil {
		t.Fatal(err)
	}

	h2 := NewHub(WithHubWAL(wal))
	h2.RegisterDefaultAdapters()
	if err := h2.Replay(); err != nil {
		t.Fatal(err)
	}
	// Registry and task persistence are separate gaps: bring the identities
	// and the live task back by hand, as a restarted deployment would.
	f.h.mu.Lock()
	h2.mu.Lock()
	for id, p := range f.h.passports {
		h2.passports[id] = p
		h2.byName[p.Name] = id
		h2.rules[id] = DefaultRule()
	}
	h2.mu.Unlock()
	f.h.mu.Unlock()
	if err := h2.ObserveTask(task.Metadata{TaskID: "t-1", OwnerID: f.alice, ChannelID: f.ch, State: task.StateRunning}); err != nil {
		t.Fatal(err)
	}
	send := func(sender string) error {
		_, err := h2.Send(Envelope{ChannelID: f.ch, SenderID: sender, Audience: f.alice, EventType: EnvTaskCancelRequest,
			TaskID: "t-1", Payload: mustJSON(okPayload())})
		return err
	}
	if err := send(f.carol); !errors.Is(err, ErrProtocol) {
		t.Fatalf("carol's second request after a restart: %v", err)
	}
	if err := send(f.dave); err != nil {
		t.Fatalf("dave's first request after a restart: %v", err)
	}
}

// A channel that ended terminal leaves no peer-cancel or causation state
// behind after a replay, just as closing it live prunes them.
func TestReplayPrunesTerminalChannelBookkeeping(t *testing.T) {
	wal := NewMemoryWAL()
	f := newPeerCancelFixture(t, WithHubWAL(wal))
	reply := NewTextEnvelope(f.ch, f.bob, "", "r")
	reply.CausationID = "some-envelope"
	if _, err := f.h.Send(reply); err != nil {
		t.Fatal(err)
	}
	if err := f.cancel(f.carol, f.alice, "t-1", okPayload()); err != nil {
		t.Fatal(err)
	}
	if err := f.h.CloseChannel(f.ch, "done"); err != nil {
		t.Fatal(err)
	}

	h2 := NewHub(WithHubWAL(wal))
	h2.RegisterDefaultAdapters()
	if err := h2.Replay(); err != nil {
		t.Fatal(err)
	}
	if _, ok := h2.FindEnvelopeByCausation(f.ch, f.bob, "some-envelope"); ok {
		t.Fatal("causation index of a closed channel came back")
	}
	h2.mu.Lock()
	leftover := len(h2.ext.peerCancel) + len(h2.ext.causation)
	h2.mu.Unlock()
	if leftover != 0 {
		t.Fatalf("bookkeeping of a terminal channel survived replay: %d", leftover)
	}
}

// Only the cancel-request event type gets the stranger exemption, even with
// every other detail well formed.
func TestPeerCancelExemptionIsForCancelRequestsOnly(t *testing.T) {
	f := newPeerCancelFixture(t)
	for _, evType := range []string{EnvTaskCancelled, "ag2.task.completed", EnvText, EnvContextSet} {
		env := Envelope{ChannelID: f.ch, SenderID: f.carol, Audience: f.alice, EventType: evType,
			TaskID: "t-1", Payload: mustJSON(okPayload())}
		if _, err := f.h.Send(env); !errors.Is(err, ErrProtocol) {
			t.Errorf("%s from a stranger: %v", evType, err)
		}
	}
}

// The reason limit is inclusive: exactly maxWireText bytes is accepted, one
// more is not.
func TestPeerCancelReasonLengthBoundary(t *testing.T) {
	f := newPeerCancelFixture(t)
	exact := map[string]any{"task_id": "t-1", "reason": strings.Repeat("x", maxWireText)}
	if err := f.cancel(f.carol, f.alice, "t-1", exact); err != nil {
		t.Fatalf("a reason of exactly %d bytes: %v", maxWireText, err)
	}
	over := map[string]any{"task_id": "t-1", "reason": strings.Repeat("x", maxWireText+1)}
	if err := f.cancel(f.dave, f.alice, "t-1", over); !errors.Is(err, ErrProtocol) {
		t.Fatalf("a reason of %d bytes: %v", maxWireText+1, err)
	}
}
