package network

import (
	"errors"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luckystrker/ago/task"
)

func TestSweeperRegistryRunsAndStops(t *testing.T) {
	h := NewHub()
	var ticks atomic.Int64
	if err := h.RegisterSweeper("custom", 5*time.Millisecond, func() { ticks.Add(1) }); err != nil {
		t.Fatal(err)
	}
	if err := h.RegisterSweeper("custom", time.Second, func() {}); !errors.Is(err, ErrSweeperExists) {
		t.Fatalf("duplicate name: %v", err)
	}
	if err := h.RegisterSweeper("bad", 0, func() {}); !errors.Is(err, ErrBadInterval) {
		t.Fatalf("zero interval: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if ticks.Load() != 0 {
		t.Fatal("a sweeper registered before Start must not run yet")
	}
	h.Start()
	h.Start() // idempotent
	waitFor(t, func() bool { return ticks.Load() >= 3 })

	// A sweeper registered after Start begins immediately; a panicking one
	// keeps ticking.
	var panics atomic.Int64
	if err := h.RegisterSweeper("late", 5*time.Millisecond, func() { panics.Add(1); panic("tick bug") }); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return panics.Load() >= 2 })
	if got := h.Sweepers(); !reflect.DeepEqual(got, []string{"custom", SweeperTTL, SweeperExpectations, "late"}) {
		t.Fatalf("sweepers: %v", got)
	}

	h.UnregisterSweeper("late")
	frozen := panics.Load()
	time.Sleep(30 * time.Millisecond)
	if panics.Load() != frozen {
		t.Fatal("unregistered sweeper kept running")
	}

	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil { // idempotent
		t.Fatal(err)
	}
	stopped := ticks.Load()
	time.Sleep(30 * time.Millisecond)
	if ticks.Load() != stopped {
		t.Fatal("Close left a sweeper running")
	}
	if err := h.RegisterSweeper("after", time.Second, func() {}); !errors.Is(err, ErrHubClosed) {
		t.Fatalf("register after Close: %v", err)
	}
}

// Unregistering a sweeper that never started must not wait for a goroutine
// that does not exist, and Start must not resurrect it.
func TestUnregisterSweeperBeforeStart(t *testing.T) {
	h := NewHub(WithTTLSweepInterval(0), WithExpectationSweepInterval(0))
	var ran atomic.Int64
	if err := h.RegisterSweeper("queued", 5*time.Millisecond, func() { ran.Add(1) }); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { h.UnregisterSweeper("queued"); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("UnregisterSweeper hung on a sweeper that was never started")
	}
	h.UnregisterSweeper("never-registered") // no-op
	h.Start()
	defer h.Close()
	time.Sleep(30 * time.Millisecond)
	if ran.Load() != 0 || len(h.Sweepers()) != 0 {
		t.Fatalf("an unregistered sweeper ran or lingered: %d %v", ran.Load(), h.Sweepers())
	}
}

func TestStartHonoursBuiltinSweeperIntervals(t *testing.T) {
	h := NewHub(WithTTLSweepInterval(0), WithExpectationSweepInterval(0))
	h.Start()
	defer h.Close()
	if got := h.Sweepers(); len(got) != 0 {
		t.Fatalf("zero intervals must disable the built-ins: %v", got)
	}
	h2 := NewHub()
	h2.Start()
	defer h2.Close()
	if got := h2.Sweepers(); !reflect.DeepEqual(got, []string{SweeperTTL, SweeperExpectations}) {
		t.Fatalf("default built-ins: %v", got)
	}
}

// The background TTL sweeper itself (real ticker) expires an overdue channel.
func TestBackgroundTTLSweeperExpiresChannel(t *testing.T) {
	clock := newFakeClock()
	h := NewHub(WithHubClock(clock.Now), WithTTLSweepInterval(5*time.Millisecond),
		WithExpectationSweepInterval(0), WithAckTimeout(3*time.Second))
	h.RegisterDefaultAdapters()
	a, b := registerTwo(t, h)
	rec := newRecorder()
	rec.attach(h, a, true)
	rec.attach(h, b, true)
	ch, err := h.CreateChannel(ChannelManifest{Type: TypeConversation}, []string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	h.Start()
	defer h.Close()
	clock.Advance(3 * time.Hour)
	waitForChannel(t, h, ch.ID, ChannelExpired)
}

func TestOpenHubRegistersDefaultsReplaysAndStarts(t *testing.T) {
	wal := NewFileWAL(filepath.Join(t.TempDir(), "wal.jsonl"))
	h, err := OpenHub(WithHubWAL(wal), WithAckTimeout(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	a, b := registerTwo(t, h)
	rec := newRecorder()
	rec.attach(h, a, true)
	rec.attach(h, b, true)
	// No RegisterAdapter calls: the four built-in types work out of the box.
	for _, typ := range []string{TypeConsulting, TypeConversation, TypeDiscussion} {
		if _, err := h.CreateChannel(ChannelManifest{Type: typ}, []string{a, b}); err != nil {
			t.Fatalf("%s on an opened hub: %v", typ, err)
		}
	}
	graph, _ := NewRoundRobinGraph([]string{a, b}, 0)
	if _, err := h.CreateChannel(ChannelManifest{Type: TypeWorkflow, Knobs: map[string]any{"graph": graph.ToDict()}}, []string{a, b}); err != nil {
		t.Fatalf("workflow on an opened hub: %v", err)
	}
	if got := h.Sweepers(); len(got) != 2 {
		t.Fatalf("OpenHub must start the built-in sweepers: %v", got)
	}
	_ = h.Close()

	// Reopening over the same WAL replays the channels (adapters are already
	// there, which Replay needs).
	h2, err := OpenHub(WithHubWAL(wal))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer h2.Close()
	if got := len(h2.ListChannels()); got != 4 {
		t.Fatalf("replayed channels: %d", got)
	}

	// Opt out.
	bare, err := OpenHub(WithoutDefaultAdapters())
	if err != nil {
		t.Fatal(err)
	}
	defer bare.Close()
	if got := bare.Health().AdaptersLoaded; got != 0 {
		t.Fatalf("WithoutDefaultAdapters loaded %d adapters", got)
	}
	if NewHub().Health().AdaptersLoaded != 0 {
		t.Fatal("NewHub must keep registering nothing")
	}
}

// --- TTL sweep, defaults and the task cascade -----------------------------------------

// ttlFixture is a fake-clock hub with two agents and an open conversation.
func ttlFixture(t *testing.T, rule *Rule, manifest ChannelManifest) (*Hub, *fakeClock, string, string, *ChannelHandle, *recListener) {
	t.Helper()
	clock := newFakeClock()
	h := newTestHub(t, WithHubClock(clock.Now))
	rec := newRecListener()
	h.RegisterListener(rec)
	a, err := h.Register(Passport{Name: "alice"}, rule)
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Register(Passport{Name: "bob"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := newRecorder()
	raw.attach(h, a.AgentID, true)
	raw.attach(h, b.AgentID, true)
	if manifest.Type == "" {
		manifest.Type = TypeConversation
	}
	ch, err := h.CreateChannel(manifest, []string{a.AgentID, b.AgentID})
	if err != nil {
		t.Fatal(err)
	}
	return h, clock, a.AgentID, b.AgentID, ch, rec
}

func TestDefaultChannelTTLAndSweepCascadesTasks(t *testing.T) {
	h, clock, alice, _, ch, rec := ttlFixture(t, nil, ChannelManifest{})
	if want := clock.Now().Add(2 * time.Hour); !ch.ExpiresAt.Equal(want) {
		t.Fatalf("creator's default TTL: expires %v, want %v", ch.ExpiresAt, want)
	}
	// Two live tasks ride the channel, one finished task and one unrelated.
	for _, id := range []string{"t-live-1", "t-live-2", "t-done"} {
		if err := h.ObserveTask(task.Metadata{TaskID: id, OwnerID: alice, ChannelID: ch.ID, State: task.StateRunning,
			ExpiresAt: clock.Now().Add(10 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	_ = h.UpdateTask("t-done", task.StateCompleted, nil, "ok", "")
	_ = h.ObserveTask(task.Metadata{TaskID: "t-elsewhere", OwnerID: alice, State: task.StateRunning,
		ExpiresAt: clock.Now().Add(10 * time.Hour)})

	clock.Advance(2*time.Hour - time.Second)
	h.SweepTTL()
	if got, _ := h.Channel(ch.ID); got.State != ChannelActive {
		t.Fatalf("expired before its TTL: %s", got.State)
	}
	clock.Advance(2 * time.Second)
	h.SweepTTL()

	got, _ := h.Channel(ch.ID)
	if got.State != ChannelExpired || got.CloseReason != "ttl_expired" {
		t.Fatalf("channel after the sweep: %s %q", got.State, got.CloseReason)
	}
	for _, id := range []string{"t-live-1", "t-live-2"} {
		md, _ := h.Task(id)
		if md.State != task.StateExpired || md.Error != "channel_closed" || md.CompletedAt.IsZero() {
			t.Errorf("%s did not cascade: %+v", id, md)
		}
	}
	if md, _ := h.Task("t-done"); md.State != task.StateCompleted {
		t.Errorf("a finished task must keep its state: %s", md.State)
	}
	if md, _ := h.Task("t-elsewhere"); md.State != task.StateRunning {
		t.Errorf("a task on no channel must not cascade: %s", md.State)
	}
	// Observers see ag2 task expiry before the channel event (Python order).
	var order []string
	rec.mu.Lock()
	for _, hook := range rec.hooks {
		if hook == "task" || hook == "channel" {
			order = append(order, hook)
		}
	}
	rec.mu.Unlock()
	if !reflect.DeepEqual(order, []string{"channel", "channel", "task", "task", "task", "channel"}) {
		// created, opened, [t-done completed], t-live-1, t-live-2, expired
		t.Fatalf("event order: %v", order)
	}
	if kinds := rec.seen("channel"); kinds[len(kinds)-1] != "expired" {
		t.Fatalf("channel events: %v", kinds)
	}
	// A second sweep is a no-op.
	h.SweepTTL()
}

// A task expires only from an explicit deadline its owner stamped. The rule's
// task_ttl_default is carried for parity (Python never applies it either), so
// a task observed without a deadline is never swept.
func TestSweepTTLExpiresOnlyTasksWithAnExplicitDeadline(t *testing.T) {
	h, clock, alice, _, _, rec := ttlFixture(t, nil, ChannelManifest{})
	if got := DefaultRule().TaskTTLDefault; got != DefaultTaskTTL {
		t.Fatalf("the rule keeps task_ttl_default for parity: %q", got)
	}
	if err := h.ObserveTask(task.Metadata{TaskID: "t-open", OwnerID: alice, State: task.StateRunning}); err != nil {
		t.Fatal(err)
	}
	if err := h.ObserveTask(task.Metadata{TaskID: "t-own", OwnerID: alice, State: task.StateRunning,
		ExpiresAt: clock.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if md, _ := h.Task("t-open"); !md.ExpiresAt.IsZero() {
		t.Fatalf("no implicit deadline may be stamped: %v", md.ExpiresAt)
	}
	clock.Advance(16 * time.Minute) // past the unapplied 15m default
	h.SweepTTL()
	if md, _ := h.Task("t-open"); md.State != task.StateRunning {
		t.Fatalf("a task with no deadline was expired: %+v", md)
	}
	if md, _ := h.Task("t-own"); md.State != task.StateRunning {
		t.Fatalf("task inside its own deadline expired: %+v", md)
	}
	clock.Advance(time.Hour)
	h.SweepTTL()
	if md, _ := h.Task("t-own"); md.State != task.StateExpired || md.Error != "ttl_expired" {
		t.Fatalf("task past its explicit deadline: %+v", md)
	}
	if md, _ := h.Task("t-open"); md.State != task.StateRunning {
		t.Fatalf("a task with no deadline must stay running forever: %+v", md)
	}
	if got := rec.seen("task"); !reflect.DeepEqual(got, []string{"expired"}) {
		t.Fatalf("task events: %v", got)
	}
	// Terminal states are immutable: a late agent-side completion changes nothing.
	if err := h.UpdateTask("t-own", task.StateCompleted, nil, "late", ""); err != nil {
		t.Fatal(err)
	}
	if md, _ := h.Task("t-own"); md.State != task.StateExpired || md.Result != nil {
		t.Fatalf("late update overwrote a terminal task: %+v", md)
	}
}

func TestChannelTTLKnobAndRuleOverrides(t *testing.T) {
	// A knob beats the creator's default; ttl_seconds 0 means no expiry.
	h, clock, _, _, ch, _ := ttlFixture(t, nil, ChannelManifest{Knobs: map[string]any{"ttl_seconds": 60}})
	if !ch.ExpiresAt.Equal(clock.Now().Add(time.Minute)) {
		t.Fatalf("knob TTL: %v", ch.ExpiresAt)
	}
	_, _, _, _, forever, _ := ttlFixture(t, nil, ChannelManifest{Knobs: map[string]any{"ttl_seconds": 0}})
	if !forever.ExpiresAt.IsZero() {
		t.Fatalf("ttl_seconds 0 must mean no expiry: %v", forever.ExpiresAt)
	}
	// A hand-built rule has no default TTL: the old behaviour.
	_, _, _, _, bare, _ := ttlFixture(t, &Rule{InboundGlobs: []string{"*"}, OutboundGlobs: []string{"*"}}, ChannelManifest{})
	if !bare.ExpiresAt.IsZero() {
		t.Fatalf("a Rule literal sets no default TTL: %v", bare.ExpiresAt)
	}
	short := DefaultRule()
	short.ChannelTTLDefault = "30m"
	_, c3, _, _, custom, _ := ttlFixture(t, &short, ChannelManifest{})
	if !custom.ExpiresAt.Equal(c3.Now().Add(30 * time.Minute)) {
		t.Fatalf("rule TTL: %v", custom.ExpiresAt)
	}
	bad := DefaultRule()
	bad.TaskTTLDefault = "soon"
	if _, err := h.Register(Passport{Name: "carol"}, &bad); !errors.Is(err, ErrBadIdentity) {
		t.Fatalf("unparseable TTL must be refused at Register: %v", err)
	}
}

func TestSweepTTLReleasesPendingCreateWaiter(t *testing.T) {
	clock := newFakeClock()
	h := newTestHub(t, WithHubClock(clock.Now), WithAckTimeout(5*time.Second))
	hc := NewHubClient(h)
	alice, _ := hc.Register("alice")
	slow, _ := hc.Register("slow", WithAutoAck(false)) // never acks
	defer func() { _ = alice.Close(); _ = slow.Close() }()
	done := make(chan error, 1)
	go func() {
		_, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation, Knobs: map[string]any{"ttl_seconds": 30}}, slow.AgentID())
		done <- err
	}()
	waitFor(t, func() bool { return len(h.ListChannels()) == 1 })
	clock.Advance(time.Minute)
	h.SweepTTL()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("create must fail when the pending channel expires")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the TTL sweep did not release the create waiter")
	}
}

// Every field gets a different value, so a counter wired to the wrong source
// cannot pass.
func TestHubHealth(t *testing.T) {
	log := NewAuditLog(NewSliceAuditLog())
	clock := newFakeClock()
	h := newTestHub(t, WithAuditLog(log), WithHubClock(clock.Now))
	h.RegisterListener(newRecListener())
	h.RegisterListener(newRecListener())
	bobRule := DefaultRule()
	bobRule.Inbox = InboxBlock{MaxPending: 50}
	alice, bob, ch, _ := rawConversation(t, h, nil, &bobRule)
	for i := 0; i < 5; i++ {
		_ = say(h, ch, alice, "to bob")
	}
	for i := 0; i < 2; i++ {
		_ = say(h, ch, bob, "to alice")
	}
	for i := 0; i < 4; i++ { // four more registrations: six agents in all
		if _, err := h.Register(Passport{Name: string(rune('p' + i))}, nil); err != nil {
			t.Fatal(err)
		}
	}
	second, err := h.CreateChannel(ChannelManifest{Type: TypeConversation}, []string{alice, bob})
	if err != nil {
		t.Fatal(err)
	}
	closed, err := h.CreateChannel(ChannelManifest{Type: TypeConversation}, []string{alice, bob})
	if err != nil {
		t.Fatal(err)
	}
	_ = h.CloseChannel(closed.ID, "x")
	_ = second
	for i := 0; i < 8; i++ { // eight live tasks and one finished
		_ = h.ObserveTask(task.Metadata{TaskID: string(rune('a' + i)), OwnerID: alice, State: task.StateRunning})
	}
	_ = h.ObserveTask(task.Metadata{TaskID: "done", OwnerID: alice, State: task.StateRunning})
	_ = h.UpdateTask("done", task.StateCompleted, nil, nil, "")
	_ = h.RegisterSweeper("custom", time.Hour, func() {})

	got := h.Health()
	want := HubHealth{
		ActiveChannels: 2, ActiveTasks: 8, RegisteredAgents: 6,
		PendingInboxTotal: 7, MaxPendingInboxDepth: 5,
		RegisteredListeners: 3, AdaptersLoaded: 4, Sweepers: 1,
		AuditLogBytes: log.BytesWritten(),
	}
	if got != want {
		t.Fatalf("health:\n got %+v\nwant %+v", got, want)
	}
	if got.AuditLogBytes == 0 {
		t.Fatal("audit bytes must be non-zero")
	}
	if (HubHealth{}) != NewHub().Health() {
		t.Fatalf("an empty hub: %+v", NewHub().Health())
	}
}
