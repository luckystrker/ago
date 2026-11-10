package network

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/task"
	"github.com/luckystrker/ago/tools"
)

// --- ExpectedNext ---------------------------------------------------------------------

func TestExpectedNextPerAdapter(t *testing.T) {
	h := newTestHub(t)
	ids := registerAgents(t, h, "alice", "bob", "carol")
	a, b, c := ids[0], ids[1], ids[2]
	expect := func(ch string) *ExpectedTurn {
		t.Helper()
		e, err := h.ExpectedNext(ch)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}

	cons, _ := h.CreateChannel(ChannelManifest{Type: TypeConsulting}, []string{a, b})
	if e := expect(cons.ID); e == nil || e.AgentID != a || e.TriggeringEnvelopeID != "" {
		t.Fatalf("consulting fresh: initiator, no trigger: %+v", e)
	}
	q, _ := h.Send(NewTextEnvelope(cons.ID, a, "", "q"))
	if e := expect(cons.ID); e == nil || e.AgentID != b || e.TriggeringEnvelopeID != q.ID {
		t.Fatalf("consulting after the question: respondent triggered by %s: %+v", q.ID, e)
	}
	_, _ = h.Send(NewTextEnvelope(cons.ID, b, "", "a"))
	if e := expect(cons.ID); e != nil { // the cycle completed and the channel closed
		t.Fatalf("consulting complete: %+v", e)
	}

	disc, _ := h.CreateChannel(ChannelManifest{Type: TypeDiscussion}, ids)
	first, _ := h.Send(NewTextEnvelope(disc.ID, a, "", "x"))
	if e := expect(disc.ID); e == nil || e.AgentID != b || e.TriggeringEnvelopeID != first.ID {
		t.Fatalf("discussion: %+v", e)
	}

	conv, _ := h.CreateChannel(ChannelManifest{Type: TypeConversation}, []string{b, c})
	if e := expect(conv.ID); e != nil {
		t.Fatalf("conversation expects nobody: %+v", e)
	}

	graph, _ := NewRoundRobinGraph([]string{a, b}, 0)
	wf, _ := h.CreateChannel(ChannelManifest{Type: TypeWorkflow, Knobs: map[string]any{"graph": graph.ToDict()}}, []string{a, b})
	if e := expect(wf.ID); e == nil || e.AgentID != a {
		t.Fatalf("workflow initial: %+v", e)
	}
	w1, _ := h.Send(NewTextEnvelope(wf.ID, a, "", "turn"))
	if e := expect(wf.ID); e == nil || e.AgentID != b || e.TriggeringEnvelopeID != w1.ID {
		t.Fatalf("workflow next: %+v", e)
	}

	_ = h.CloseChannel(conv.ID, "x")
	if e := expect(conv.ID); e != nil {
		t.Fatalf("closed channel: %+v", e)
	}
	if _, err := h.ExpectedNext("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown channel: %v", err)
	}

	pending := h.PendingTurnsFor(b)
	if len(pending) != 2 { // discussion (after a) and workflow (after a)
		t.Fatalf("pending turns for bob: %+v", pending)
	}
	if got := h.PendingTurnsFor(c); len(got) != 0 {
		t.Fatalf("carol has no turn: %+v", got)
	}
	if got := h.PendingTurnsFor("ghost"); got != nil {
		t.Fatalf("unknown agent: %+v", got)
	}
}

// --- ResumePendingTurns ---------------------------------------------------------------

// recordingBob is a client whose handler logs every inbound text and replies.
func recordingAgent(t *testing.T, hc *HubClient, name string) (*AgentClient, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	a, err := hc.Register(name, WithAgentFunc(func(msg string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, msg)
		return "re:" + msg, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return a, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func TestResumePendingTurnsRefiresTheExpectedTurn(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)
	alice, _ := hc.Register("alice")
	bob, bobSeen := recordingAgent(t, hc, "bob")
	carol, _ := hc.Register("carol")
	defer func() { _ = alice.Close(); _ = bob.Close(); _ = carol.Close() }()

	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeDiscussion}, bob.AgentID(), carol.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	// The turn envelope reaches bob while his handler is not running (as
	// after a crash): it is delivered and dropped.
	bob.SuppressHandler(ch.ID)
	if err := alice.Say(ch.ID, "your move bob"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return h.InboxPending(bob.AgentID()) == 0 })
	if got := bobSeen(); len(got) != 0 {
		t.Fatalf("handler ran while suppressed: %v", got)
	}

	bob.ResumeHandler(ch.ID)
	n, err := bob.ResumePendingTurns()
	if err != nil || n != 1 {
		t.Fatalf("resumed %d turns (%v)", n, err)
	}
	waitFor(t, func() bool { return len(bobSeen()) == 1 })
	if got := bobSeen(); got[0] != "your move bob" {
		t.Fatalf("handler input: %v", got)
	}
	// The re-fired turn produced the reply: it is carol's turn now, so bob has
	// nothing left to resume — and carol has exactly one.
	waitFor(t, func() bool { return len(h.PendingTurnsFor(bob.AgentID())) == 0 })
	if n, _ := bob.ResumePendingTurns(); n != 0 {
		t.Fatalf("nothing pending for bob any more, resumed %d", n)
	}
	if got := h.PendingTurnsFor(carol.AgentID()); len(got) != 1 {
		t.Fatalf("carol pending: %+v", got)
	}
	// The creator's own fresh turn has no trigger and is skipped.
	fresh, _ := alice.CreateChannel(ChannelManifest{Type: TypeDiscussion}, bob.AgentID())
	if n, _ := alice.ResumePendingTurns(); n != 0 {
		t.Fatalf("a turn without a trigger must be skipped, resumed %d (channel %s)", n, fresh.ID)
	}
}

func TestHandlerSkipsEnvelopeWhoseReplyAlreadyLanded(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)
	alice, _ := hc.Register("alice")
	bob, bobSeen := recordingAgent(t, hc, "bob")
	defer func() { _ = alice.Close(); _ = bob.Close() }()
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	if err := alice.Say(ch.ID, "once"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(bobSeen()) == 1 })
	waitFor(t, func() bool { return countReplies(h, ch.ID, bob.AgentID()) == 1 })

	// At-least-once redelivery of the same envelope: the reply exists, so the
	// handler must not run (nor post) a second time.
	wal, _ := h.ReadWAL(ch.ID)
	var trigger Envelope
	for _, e := range wal {
		if e.EventType == EnvText && e.SenderID == alice.AgentID() {
			trigger = e
		}
	}
	bob.deliver(trigger)
	bob.deliver(trigger)
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = bob.WaitIdle(ctx)
	if got := bobSeen(); len(got) != 1 {
		t.Fatalf("handler re-ran on a redelivery: %v", got)
	}
	if n := countReplies(h, ch.ID, bob.AgentID()); n != 1 {
		t.Fatalf("duplicate replies posted: %d", n)
	}
}

func countReplies(h *Hub, ch, sender string) int {
	wal, _ := h.ReadWAL(ch)
	n := 0
	for _, e := range wal {
		if e.EventType == EnvText && e.SenderID == sender {
			n++
		}
	}
	return n
}

func TestHandlerFailureIsReportedAsTurnFailure(t *testing.T) {
	h := newTestHub(t)
	rec := newRecListener()
	h.RegisterListener(rec)
	hc := NewHubClient(h)
	alice, _ := hc.Register("alice")
	bob, _ := hc.Register("bob", WithAgentFunc(func(string) (string, error) { return "", errors.New("llm down") }))
	defer func() { _ = alice.Close(); _ = bob.Close() }()
	ch, _ := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())
	_ = alice.Say(ch.ID, "hi")
	waitFor(t, func() bool { return len(rec.seen("turn_failed")) == 1 })
	if got := rec.seen("turn_failed"); got[0] != "llm down" {
		t.Fatalf("turn failure: %v", got)
	}
	if bob.Err() == nil {
		t.Fatal("Err() keeps the last failure")
	}
	// The channel stays alive.
	if got, _ := h.Channel(ch.ID); got.State != ChannelActive {
		t.Fatalf("channel state: %s", got.State)
	}
}

// --- WaitForChannelEvent --------------------------------------------------------------

func TestWaitForChannelEventAgent(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)
	alice, _ := hc.Register("alice")
	bob, _ := hc.Register("bob")
	defer func() { _ = alice.Close(); _ = bob.Close() }()
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	other, _ := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())

	// Ensure before the send, as documented: even an instant reply is seen.
	alice.EnsureChannelInbox(ch.ID)
	if err := bob.Say(ch.ID, "noise"); err != nil {
		t.Fatal(err)
	}
	if err := bob.Say(other.ID, "elsewhere"); err != nil {
		t.Fatal(err)
	}
	if err := bob.Say(ch.ID, "the answer"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := alice.WaitForChannelEvent(ctx, ch.ID, func(e Envelope) bool { return e.EventType == EnvText && e.Text() == "the answer" })
	if err != nil || got.Text() != "the answer" || got.ChannelID != ch.ID {
		t.Fatalf("wait: %+v %v", got, err)
	}

	// Blocks until a later envelope arrives.
	done := make(chan Envelope, 1)
	go func() {
		e, _ := alice.WaitForChannelEvent(ctx, ch.ID, func(e Envelope) bool { return e.Text() == "later" })
		done <- e
	}()
	time.Sleep(30 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("returned before the envelope existed")
	default:
	}
	_ = bob.Say(ch.ID, "later")
	select {
	case e := <-done:
		if e.Text() != "later" {
			t.Fatalf("got %q", e.Text())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("never woke up")
	}

	// Timeout and cancellation surface the context error.
	short, cancelShort := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelShort()
	if _, err := alice.WaitForChannelEvent(short, ch.ID, func(Envelope) bool { return false }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
	// A channel nothing was ever delivered on has an empty queue: the wait
	// blocks until its context ends.
	late, cancelLate := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelLate()
	if _, err := alice.WaitForChannelEvent(late, "never-seen-channel", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("empty queue: %v", err)
	}
	// Discarding forgets the queue.
	alice.DiscardChannelInbox(ch.ID)

	// Closing the client wakes a blocked waiter with ErrClosed.
	errc := make(chan error, 1)
	go func() {
		_, err := alice.WaitForChannelEvent(context.Background(), ch.ID, func(Envelope) bool { return false })
		errc <- err
	}()
	time.Sleep(20 * time.Millisecond)
	_ = alice.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not wake the waiter")
	}
}

func TestWaitForChannelEventHuman(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)
	alice, _ := hc.Register("alice")
	hana, _ := hc.RegisterHuman("hana")
	defer func() { _ = alice.Close(); hana.Close() }()
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, hana.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	hana.EnsureChannelInbox(ch.ID)
	_ = alice.Say(ch.ID, "ping")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := hana.WaitForChannelEvent(ctx, ch.ID, func(e Envelope) bool { return e.EventType == EnvText })
	if err != nil || got.Text() != "ping" {
		t.Fatalf("human wait: %+v %v", got, err)
	}
	hana.DiscardChannelInbox(ch.ID)
}

// --- human callbacks and filtered pull ---------------------------------------------------

func TestHumanCallbacksAndFilteredPull(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)
	alice, _ := hc.Register("alice")
	hana, _ := hc.RegisterHuman("hana")
	defer func() { _ = alice.Close(); hana.Close() }()

	var mu sync.Mutex
	var order []string
	rec := func(tag string) func(Envelope) {
		return func(e Envelope) {
			if e.EventType == EnvText {
				mu.Lock()
				order = append(order, tag+":"+e.Text())
				mu.Unlock()
			}
		}
	}
	removeFirst := hana.OnEnvelope(rec("first"))
	hana.OnEnvelope(func(Envelope) { panic("ui bug") })
	hana.OnEnvelope(rec("third"))

	ch1, _ := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, hana.AgentID())
	ch2, _ := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, hana.AgentID())
	_ = alice.Say(ch1.ID, "one")
	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()
	if !reflect.DeepEqual(got, []string{"first:one", "third:one"}) {
		t.Fatalf("callbacks run in registration order and survive a panicking one: %v", got)
	}
	removeFirst()
	removeFirst() // idempotent
	_ = alice.Say(ch1.ID, "two")
	mu.Lock()
	got = append([]string(nil), order...)
	mu.Unlock()
	if !reflect.DeepEqual(got, []string{"first:one", "third:one", "third:two"}) {
		t.Fatalf("removed callback still ran: %v", got)
	}

	// Pull: envelopes of other channels (and non-matching ones) are discarded.
	_ = alice.Say(ch2.ID, "on two")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	env, err := hana.NextEnvelope(ctx, func(e Envelope) bool { return ForChannel(ch2.ID)(e) && e.EventType == EnvText })
	if err != nil || env.Text() != "on two" {
		t.Fatalf("filtered pull: %+v %v", env, err)
	}
	// Unfiltered: the very next envelope.
	_ = alice.Say(ch1.ID, "three")
	env, err = hana.NextEnvelope(ctx, nil)
	if err != nil || env.Text() != "three" {
		t.Fatalf("unfiltered pull: %+v %v", env, err)
	}
	short, cancelShort := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelShort()
	if _, err := hana.NextEnvelope(short, ForChannel("nope")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("no match: %v", err)
	}
	hana.Close()
	if _, err := hana.NextEnvelope(context.Background(), nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed: %v", err)
	}
}

// --- SetRule ---------------------------------------------------------------------------

func TestAgentSetRuleTakesEffect(t *testing.T) {
	h := newTestHub(t)
	rec := newRecListener()
	h.RegisterListener(rec)
	hc := NewHubClient(h)
	alice, _ := hc.Register("alice")
	bob, _ := hc.Register("bob")
	defer func() { _ = alice.Close(); _ = bob.Close() }()
	ch, _ := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())
	if err := alice.Say(ch.ID, "allowed"); err != nil {
		t.Fatal(err)
	}

	tight := DefaultRule()
	tight.OutboundGlobs = []string{"nobody"}
	if err := alice.SetRule(tight); err != nil {
		t.Fatal(err)
	}
	// The conversation broadcast skips the outbound check; an addressed send
	// to bob is what the rule now forbids.
	_, err := alice.Send(NewTextEnvelope(ch.ID, alice.AgentID(), bob.AgentID(), "now forbidden"))
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("send under the new rule: %v", err)
	}
	if got := alice.Rule().OutboundGlobs; !reflect.DeepEqual(got, []string{"nobody"}) {
		t.Fatalf("client's copy: %v", got)
	}
	stored, _ := h.RuleOf(alice.AgentID())
	if !reflect.DeepEqual(stored.OutboundGlobs, []string{"nobody"}) {
		t.Fatalf("hub's copy: %v", stored.OutboundGlobs)
	}
	if got := rec.seen("agent"); got[len(got)-1] != AgentEventRuleSet {
		t.Fatalf("rule_set event missing: %v", got)
	}

	// Rate: a new limit starts with a full bucket and applies at once.
	limited := DefaultRule()
	limited.Rate = RateBlock{PerMinute: 60, Burst: 1}
	_ = alice.SetRule(limited)
	if err := alice.Say(ch.ID, "1"); err != nil {
		t.Fatal(err)
	}
	if err := alice.Say(ch.ID, "2"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("new rate: %v", err)
	}
	// Setting a rule again hands out a fresh, full bucket.
	if err := alice.SetRule(limited); err != nil {
		t.Fatal(err)
	}
	if err := alice.Say(ch.ID, "3"); err != nil {
		t.Fatalf("SetRule must reset the rate bucket: %v", err)
	}

	// Invalid rules are refused and change nothing.
	bad := DefaultRule()
	bad.Inbox = InboxBlock{Overflow: "spill"}
	if err := alice.SetRule(bad); !errors.Is(err, ErrBadIdentity) {
		t.Fatalf("invalid rule: %v", err)
	}
	if err := h.SetRule("ghost", DefaultRule()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown agent: %v", err)
	}
	if alice.Rule().Rate != limited.Rate {
		t.Fatal("a refused SetRule must leave the rule alone")
	}
}

// --- NetworkPlugin ----------------------------------------------------------------------

func TestNetworkPluginBundlesToolsAndIdentityLine(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)
	alice, _ := hc.Register("alice")
	defer alice.Close()
	p, err := NewNetworkPlugin(hc, alice)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != NetworkPluginName {
		t.Fatalf("name: %q", p.Name())
	}
	var names []string
	for _, tl := range p.Tools() {
		names = append(names, tl.Name())
	}
	if !reflect.DeepEqual(names, []string{"peers", "channels", "tasks", "context", "delegate"}) {
		t.Fatalf("tools: %v", names)
	}
	want := "You are alice (agent_id: " + alice.AgentID() + ")."
	if got := p.PromptFragments(); !reflect.DeepEqual(got, []string{want}) {
		t.Fatalf("prompt fragments: %v", got)
	}
	if alice.IdentityLine() != want {
		t.Fatalf("identity line: %q", alice.IdentityLine())
	}
	if _, err := NewNetworkPlugin(hc, nil); err == nil {
		t.Fatal("a plugin needs the calling agent")
	}
}

// --- ClientTask -------------------------------------------------------------------------

func TestClientTaskHandleRefreshes(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)
	owner, _ := hc.Register("owner")
	watcher, _ := hc.Register("watcher")
	defer func() { _ = owner.Close(); _ = watcher.Close() }()
	s := stream.NewMemoryStream()
	NewTaskMirror(h, owner.AgentID(), "").Attach(s)
	s.Send(events.TaskStarted{TaskID: "t-1", Spec: &events.TaskSpec{Title: "grind"}})

	ct, err := watcher.Task("t-1")
	if err != nil {
		t.Fatal(err)
	}
	if ct.TaskID() != "t-1" || ct.OwnerID() != owner.AgentID() || ct.State() != task.StateRunning {
		t.Fatalf("handle: %s %s %s", ct.TaskID(), ct.OwnerID(), ct.State())
	}
	s.Send(events.TaskCompleted{TaskID: "t-1", Result: "ok"})
	if ct.State() != task.StateRunning {
		t.Fatal("the handle is a snapshot until Info refreshes it")
	}
	md, err := ct.Info()
	if err != nil || md.State != task.StateCompleted || ct.State() != task.StateCompleted || ct.Metadata().Result != "ok" {
		t.Fatalf("after Info: %+v %v / %s", md, err, ct.State())
	}
	if _, err := watcher.Task("ghost"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("unknown task: %v", err)
	}
}

// --- say tool and ToolsFor ---------------------------------------------------------------

func channelToolNames(t *testing.T, a *AgentClient, ch string) []string {
	t.Helper()
	ts, err := a.ChannelTools(ch)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, tl := range ts {
		names = append(names, tl.Name())
	}
	return names
}

func TestToolsForPerAdapter(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)
	alice, _ := hc.Register("alice", WithAutoAck(true))
	bob, _ := hc.Register("bob")
	carol, _ := hc.Register("carol")
	defer func() { _ = alice.Close(); _ = bob.Close(); _ = carol.Close() }()

	// Consulting: say follows the floor of the 1Q1R cycle.
	cons, _ := alice.CreateChannel(ChannelManifest{Type: TypeConsulting}, bob.AgentID())
	if got := channelToolNames(t, alice, cons.ID); !reflect.DeepEqual(got, []string{"say"}) {
		t.Fatalf("consulting initiator: %v", got)
	}
	if got := channelToolNames(t, bob, cons.ID); len(got) != 0 {
		t.Fatalf("consulting respondent before the question: %v", got)
	}
	alice.SuppressHandler(cons.ID)
	bob.SuppressHandler(cons.ID)
	_ = alice.Say(cons.ID, "q")
	if got := channelToolNames(t, alice, cons.ID); len(got) != 0 {
		t.Fatalf("consulting initiator after asking: %v", got)
	}
	if got := channelToolNames(t, bob, cons.ID); !reflect.DeepEqual(got, []string{"say"}) {
		t.Fatalf("consulting respondent after the question: %v", got)
	}

	// Conversation: always. Discussion and workflow: never.
	conv, _ := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())
	if a, b := channelToolNames(t, alice, conv.ID), channelToolNames(t, bob, conv.ID); !reflect.DeepEqual(a, []string{"say"}) || !reflect.DeepEqual(b, []string{"say"}) {
		t.Fatalf("conversation: %v %v", a, b)
	}
	disc, _ := alice.CreateChannel(ChannelManifest{Type: TypeDiscussion}, bob.AgentID(), carol.AgentID())
	if got := channelToolNames(t, alice, disc.ID); len(got) != 0 {
		t.Fatalf("discussion: %v", got)
	}
	graph, _ := NewRoundRobinGraph([]string{alice.AgentID(), bob.AgentID()}, 0)
	wf, _ := alice.CreateChannel(ChannelManifest{Type: TypeWorkflow, Knobs: map[string]any{"graph": graph.ToDict()}}, bob.AgentID())
	if got := channelToolNames(t, alice, wf.ID); len(got) != 0 {
		t.Fatalf("workflow hides say behind handoff tools: %v", got)
	}
	// Closed channels offer nothing; unknown channels fail.
	_ = h.CloseChannel(conv.ID, "x")
	if got := channelToolNames(t, alice, conv.ID); len(got) != 0 {
		t.Fatalf("closed channel: %v", got)
	}
	if _, err := alice.ChannelTools("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown channel: %v", err)
	}
}

func TestSayToolPostsWithAudience(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)
	alice, _ := hc.Register("alice")
	bob, _ := hc.Register("bob")
	carol, _ := hc.Register("carol")
	defer func() { _ = alice.Close(); _ = bob.Close(); _ = carol.Close() }()
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	bound, err := alice.ChannelTools(ch.ID) // bound to the channel
	if err != nil || len(bound) != 1 {
		t.Fatalf("channel tools: %v %v", bound, err)
	}
	last := func() Envelope {
		wal, _ := h.ReadWAL(ch.ID)
		return wal[len(wal)-1]
	}

	id := callTool(t, bound, "say", `{"content":"hello all"}`)
	if e := last(); e.Text() != "hello all" || e.Audience != "" || e.SenderID != alice.AgentID() || e.ID != id {
		t.Fatalf("broadcast: %+v (tool returned %v)", e, id)
	}
	callTool(t, bound, "say", `{"content":"psst","audience":["bob"]}`)
	if e := last(); e.Text() != "psst" || e.Audience != bob.AgentID() {
		t.Fatalf("addressed: %+v", e)
	}
	// Errors are data for the model, not Go errors.
	for args, want := range map[string]string{
		`{"content":"x","audience":["bob","carol"]}`: "at most one",
		`{"content":"x","audience":["ghost"]}`:       "not found",
	} {
		if got, _ := callTool(t, bound, "say", args).(string); !strings.Contains(got, want) {
			t.Errorf("%s → %q, want %q", args, got, want)
		}
	}

	// The generic tool needs a channel_id.
	generic, err := SayTool(alice, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := callTool(t, toolSet(generic), "say", `{"content":"x"}`).(string); !strings.Contains(got, "no current channel") {
		t.Fatalf("generic without channel: %q", got)
	}
	callTool(t, toolSet(generic), "say", `{"content":"via id","channel_id":"`+ch.ID+`"}`)
	if e := last(); e.Text() != "via id" {
		t.Fatalf("generic with channel_id: %+v", e)
	}
	if got, _ := callTool(t, toolSet(generic), "say", `{"content":"x","channel_id":"ghost"}`).(string); !strings.HasPrefix(got, "Error: say failed") {
		t.Fatalf("hub refusal: %q", got)
	}
	if _, err := SayTool(nil, ""); err == nil {
		t.Fatal("say needs the calling agent")
	}
}

func toolSet(ts ...tools.Tool) []tools.Tool { return ts }

// A re-fired turn is booked against the agent's inbox like any delivery, so
// the hub's pending count stays exact while the handler works on it.
func TestResumePendingTurnsBooksInboxSlot(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)
	alice, _ := hc.Register("alice")
	gate := make(chan struct{})
	started := make(chan struct{}, 2)
	bob, err := hc.Register("bob", WithAgentFunc(func(string) (string, error) {
		started <- struct{}{}
		<-gate
		return "", nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { close(gate); _ = alice.Close(); _ = bob.Close() }()
	carol, _ := hc.Register("carol")
	defer carol.Close()
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeDiscussion}, bob.AgentID(), carol.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	bob.SuppressHandler(ch.ID)
	_ = alice.Say(ch.ID, "your move")
	waitFor(t, func() bool { return h.InboxPending(bob.AgentID()) == 0 })
	bob.ResumeHandler(ch.ID)
	if n, _ := bob.ResumePendingTurns(); n != 1 {
		t.Fatalf("resumed %d", n)
	}
	<-started // the handler is working on it
	if got := h.InboxPending(bob.AgentID()); got != 1 {
		t.Fatalf("the re-fired turn must count while in flight: pending %d", got)
	}
}

// open → say → wait: a reply that lands before wait is called is not lost,
// with no manual EnsureChannelInbox anywhere.
func TestWaitForChannelEventSeesReplyThatArrivedFirst(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)
	alice, _ := hc.Register("alice")
	bob, _ := hc.Register("bob", WithAgentFunc(func(msg string) (string, error) { return "re:" + msg, nil }))
	defer func() { _ = alice.Close(); _ = bob.Close() }()
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	if err := alice.Say(ch.ID, "ping"); err != nil {
		t.Fatal(err)
	}
	// Let bob's reply land before alice starts waiting.
	waitFor(t, func() bool { return countReplies(h, ch.ID, bob.AgentID()) == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := alice.WaitForChannelEvent(ctx, ch.ID, func(e Envelope) bool { return e.EventType == EnvText && e.SenderID == bob.AgentID() })
	if err != nil || got.Text() != "re:ping" {
		t.Fatalf("reply that arrived before the wait: %+v %v", got, err)
	}

	// The same for a channel the client did not open: first delivery creates
	// the queue.
	carol, _ := hc.Register("carol")
	defer carol.Close()
	other, _ := carol.CreateChannel(ChannelManifest{Type: TypeConversation}, alice.AgentID())
	_ = carol.Say(other.ID, "hello alice")
	got, err = alice.WaitForChannelEvent(ctx, other.ID, func(e Envelope) bool { return e.Text() == "hello alice" })
	if err != nil || got.SenderID != carol.AgentID() {
		t.Fatalf("channel opened by the peer: %+v %v", got, err)
	}
}

func TestWaitForChannelEventHumanSeesReplyThatArrivedFirst(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)
	hana, _ := hc.RegisterHuman("hana")
	bob, _ := hc.Register("bob", WithAgentFunc(func(msg string) (string, error) { return "re:" + msg, nil }))
	defer func() { hana.Close(); _ = bob.Close() }()
	ch, err := hana.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hana.Send(NewTextEnvelope(ch.ID, hana.AgentID(), "", "ping")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return countReplies(h, ch.ID, bob.AgentID()) == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := hana.WaitForChannelEvent(ctx, ch.ID, func(e Envelope) bool { return e.SenderID == bob.AgentID() && e.EventType == EnvText })
	if err != nil || got.Text() != "re:ping" {
		t.Fatalf("human reply that arrived first: %+v %v", got, err)
	}
}

func TestChannelFeedIsBoundedAndCountsDrops(t *testing.T) {
	var f channelFeeds
	for i := 0; i < channelFeedCap; i++ {
		f.push(Envelope{ChannelID: "c", ID: fmt.Sprint(i)})
	}
	if got := f.dropped.Load(); got != 0 {
		t.Fatalf("a full feed is not a drop yet: %d", got)
	}
	f.push(Envelope{ChannelID: "c", ID: fmt.Sprint(channelFeedCap)}) // delivery 1025
	if got := f.dropped.Load(); got != 1 {
		t.Fatalf("dropped after 1025 deliveries: %d, want 1", got)
	}
	feed := f.ensure("c")
	feed.mu.Lock()
	n, first, last := len(feed.queue), feed.queue[0].ID, feed.queue[len(feed.queue)-1].ID
	feed.mu.Unlock()
	if n != channelFeedCap || first != "1" || last != fmt.Sprint(channelFeedCap) {
		t.Fatalf("queue %d [%s..%s]: want %d with the oldest (0) gone", n, first, last, channelFeedCap)
	}
	for i := 0; i < 9; i++ {
		f.push(Envelope{ChannelID: "c", ID: "x"})
	}
	if got := f.dropped.Load(); got != 10 {
		t.Fatalf("running total: %d", got)
	}
	// Another channel has its own bound.
	f.push(Envelope{ChannelID: "other", ID: "o"})
	if got := f.dropped.Load(); got != 10 {
		t.Fatalf("an unrelated channel dropped something: %d", got)
	}
	f.discard("c")
	if f.ensure("c") == feed {
		t.Fatal("discard must free the queue")
	}
}

// The counter is visible on both client types.
func TestChannelFeedDroppedGetters(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)
	bob, _ := hc.Register("bob")
	hana, _ := hc.RegisterHuman("hana")
	defer func() { _ = bob.Close(); hana.Close() }()
	if bob.ChannelFeedDropped() != 0 || hana.ChannelFeedDropped() != 0 {
		t.Fatal("a fresh client has dropped nothing")
	}
	// Lifecycle envelopes reach the feed without touching the handler inbox.
	for i := 0; i <= channelFeedCap; i++ {
		env := Envelope{ChannelID: "ch", EventType: EnvChannelOpened}
		bob.deliver(env)
		hana.deliver(env)
	}
	if got := bob.ChannelFeedDropped(); got != 1 {
		t.Fatalf("agent: %d, want 1", got)
	}
	if got := hana.ChannelFeedDropped(); got != 1 {
		t.Fatalf("human: %d, want 1", got)
	}
}
