package network

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/task"
)

func TestTaskMirrorLifecycle(t *testing.T) {
	h := newTestHub(t)
	s := stream.NewMemoryStream()

	m := NewTaskMirror(h, "owner-1", "ch-9").Attach(s)
	defer m.Detach(s)

	now := time.Now()
	s.Send(events.TaskStarted{
		TaskID:    "t-1",
		AgentName: "worker",
		Spec:      &events.TaskSpec{Title: "grind", Capability: "grinding", Payload: map[string]any{"k": "v"}},
		ExpiresAt: now.Add(time.Minute).UTC().Format(time.RFC3339),
	})

	got, err := h.Task("t-1")
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if got.OwnerID != "owner-1" || got.ChannelID != "ch-9" || got.State != task.StateRunning {
		t.Fatalf("observed %+v", got)
	}
	if got.Spec.Title != "grind" || got.Spec.Capability != "grinding" {
		t.Fatalf("spec %+v", got.Spec)
	}
	if got.ExpiresAt.IsZero() {
		t.Fatal("agent-side deadline must carry through")
	}

	s.Send(events.TaskProgress{TaskID: "t-1", Payload: map[string]any{"step": 2}})
	got, _ = h.Task("t-1")
	if got.Progress["step"] != 2 || got.LastProgressAt.IsZero() {
		t.Fatalf("progress not merged: %+v", got)
	}

	s.Send(events.TaskCompleted{TaskID: "t-1", Result: "done"})
	got, _ = h.Task("t-1")
	if got.State != task.StateCompleted || got.Result != "done" || got.CompletedAt.IsZero() {
		t.Fatalf("completion not recorded: %+v", got)
	}

	// Active-only listing drops the terminal task; owner filter works.
	s.Send(events.TaskStarted{TaskID: "t-2", AgentName: "worker"})
	all := h.Tasks("owner-1", false)
	if len(all) != 2 {
		t.Fatalf("Tasks() = %d entries, want 2", len(all))
	}
	active := h.Tasks("owner-1", true)
	if len(active) != 1 || active[0].TaskID != "t-2" {
		t.Fatalf("active tasks %v", active)
	}
	if none := h.Tasks("stranger", false); len(none) != 0 {
		t.Fatalf("owner filter leaked: %v", none)
	}
}

func TestTaskMirrorFailureAndCancel(t *testing.T) {
	h := newTestHub(t)
	s := stream.NewMemoryStream()
	m := NewTaskMirror(h, "owner-1", "").Attach(s)
	defer m.Detach(s)

	s.Send(events.TaskStarted{TaskID: "t-3", Objective: "objective only"})
	got, _ := h.Task("t-3")
	if got.Spec.Title != "objective only" {
		t.Fatalf("spec-less start must fall back to the objective, got %+v", got.Spec)
	}

	s.Send(events.TaskFailed{TaskID: "t-3", Error: "boom"})
	got, _ = h.Task("t-3")
	if got.State != task.StateFailed || got.Error != "boom" {
		t.Fatalf("failure not recorded: %+v", got)
	}

	s.Send(events.TaskStarted{TaskID: "t-4"})
	s.Send(events.TaskCancelled{TaskID: "t-4", Reason: "user asked"})
	got, _ = h.Task("t-4")
	if got.State != task.StateCancelled || got.Error != "user asked" {
		t.Fatalf("cancellation not recorded: %+v", got)
	}

	s.Send(events.TaskExpired{TaskID: "t-5"}) // never observed
	if _, err := h.Task("t-5"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("expired unknown task must stay unknown, got %v", err)
	}
}

func TestTaskMirrorProgressForUnknownTaskDropped(t *testing.T) {
	h := newTestHub(t)
	s := stream.NewMemoryStream()
	m := NewTaskMirror(h, "owner-1", "").Attach(s)
	defer m.Detach(s)

	// Python parity: progress/completion for a task the hub never observed
	// is a NotFound the mirror swallows — no task appears, no crash.
	s.Send(events.TaskProgress{TaskID: "ghost", Payload: map[string]any{"x": 1}})
	if _, err := h.Task("ghost"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("ghost task must stay unknown, got %v", err)
	}
}

func TestObserveTaskUpdateInPlace(t *testing.T) {
	h := newTestHub(t)
	if err := h.ObserveTask(task.Metadata{TaskID: "t-9", OwnerID: "o", State: task.StateRunning}); err != nil {
		t.Fatal(err)
	}
	// Re-observe with fewer fields: existing values must survive.
	if err := h.ObserveTask(task.Metadata{TaskID: "t-9", State: task.StateRunning}); err != nil {
		t.Fatal(err)
	}
	got, _ := h.Task("t-9")
	if got.OwnerID != "o" {
		t.Fatalf("re-observe wiped the owner: %+v", got)
	}
	if err := h.ObserveTask(task.Metadata{}); err == nil {
		t.Fatal("empty task id must be rejected")
	}
	if err := h.UpdateTask("nope", task.StateCompleted, nil, nil, ""); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("update of unknown task: %v", err)
	}
}

func TestDetachStopsForwarding(t *testing.T) {
	h := newTestHub(t)
	s := stream.NewMemoryStream()
	m := NewTaskMirror(h, "owner-1", "").Attach(s)
	m.Detach(s)
	s.Send(events.TaskStarted{TaskID: "t-99"})
	if _, err := h.Task("t-99"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatal("detached mirror must not forward")
	}
}

// observingMirror registers a real agent so the mirror's owner has a resume
// to update, with a fake hub clock for latency.
func observingMirror(t *testing.T) (*Hub, *fakeClock, stream.Stream, string) {
	t.Helper()
	clock := newFakeClock()
	h := newTestHub(t, WithHubClock(clock.Now))
	p, err := h.Register(Passport{Name: "worker"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := stream.NewMemoryStream()
	NewTaskMirror(h, p.AgentID, "ch-1").Attach(s)
	return h, clock, s, p.AgentID
}

func TestTaskMirrorRecordsCapabilityObservation(t *testing.T) {
	h, clock, s, owner := observingMirror(t)

	s.Send(events.TaskStarted{TaskID: "t-1", Spec: &events.TaskSpec{Title: "g", Capability: "grinding"}})
	clock.Advance(2500 * time.Millisecond)
	s.Send(events.TaskCompleted{TaskID: "t-1", Result: "ok"})

	r, _ := h.ResumeOf(owner)
	stat := r.Observed["grinding"]
	if stat.N != 1 || stat.Completed != 1 || stat.P50LatencyMS == nil || *stat.P50LatencyMS != 2500 {
		t.Fatalf("completed task: %+v", stat)
	}
	if got := h.AgentsWithCapability("grinding"); !reflect.DeepEqual(got, []string{owner}) {
		t.Fatalf("capability index: %v", got)
	}

	s.Send(events.TaskStarted{TaskID: "t-2", Spec: &events.TaskSpec{Capability: "grinding"}})
	s.Send(events.TaskFailed{TaskID: "t-2", Error: "boom"})
	s.Send(events.TaskStarted{TaskID: "t-3", Spec: &events.TaskSpec{Capability: "grinding"}})
	s.Send(events.TaskExpired{TaskID: "t-3"})
	s.Send(events.TaskStarted{TaskID: "t-4", Spec: &events.TaskSpec{Capability: "grinding"}})
	s.Send(events.TaskCancelled{TaskID: "t-4", Reason: "no longer needed"})
	r, _ = h.ResumeOf(owner)
	stat = r.Observed["grinding"]
	if stat.N != 4 || stat.Completed != 1 || stat.Failed != 1 || stat.Expired != 1 {
		t.Fatalf("all four terminal states: %+v", stat)
	}
}

func TestTaskMirrorIgnoresUntaggedAndNonTerminalTasks(t *testing.T) {
	h, _, s, owner := observingMirror(t)

	s.Send(events.TaskStarted{TaskID: "plain", Spec: &events.TaskSpec{Title: "no capability"}})
	s.Send(events.TaskCompleted{TaskID: "plain"})
	s.Send(events.TaskStarted{TaskID: "tagged", Spec: &events.TaskSpec{Capability: "grinding"}})
	s.Send(events.TaskProgress{TaskID: "tagged", Payload: map[string]any{"step": 1}})

	r, _ := h.ResumeOf(owner)
	if len(r.Observed) != 0 {
		t.Fatalf("observations without a terminal capability task: %+v", r.Observed)
	}
	// A task completed twice (cascade) is counted once.
	s.Send(events.TaskCompleted{TaskID: "tagged"})
	s.Send(events.TaskCompleted{TaskID: "tagged"})
	r, _ = h.ResumeOf(owner)
	if r.Observed["grinding"].N != 1 {
		t.Fatalf("duplicate terminal events: %+v", r.Observed["grinding"])
	}
}

func TestTaskMirrorUnregisteredOwnerIsQuiet(t *testing.T) {
	fired := 0
	h := newTestHub(t, WithTaskEventHook(func(string, string, map[string]any) { fired++ }))
	s := stream.NewMemoryStream()
	NewTaskMirror(h, "not-registered", "").Attach(s)
	s.Send(events.TaskStarted{TaskID: "t", Spec: &events.TaskSpec{Capability: "x"}})
	s.Send(events.TaskCompleted{TaskID: "t"})
	if fired != 0 {
		t.Fatalf("an owner without a registration is not a mirror failure: %d events", fired)
	}
	if got, err := h.Task("t"); err != nil || got.State != task.StateCompleted {
		t.Fatalf("task store must still update: %+v %v", got, err)
	}
}

func TestTaskMirrorEscalatesForwardingFailure(t *testing.T) {
	type fire struct {
		taskID, kind string
		payload      map[string]any
	}
	var fires []fire
	h := newTestHub(t, WithTaskEventHook(func(taskID, kind string, payload map[string]any) {
		fires = append(fires, fire{taskID, kind, payload})
	}))
	s := stream.NewMemoryStream()
	NewTaskMirror(h, "owner-1", "ch-9").Attach(s)

	// A TaskStarted with no id cannot be observed: the hub refuses it.
	s.Send(events.TaskStarted{TaskID: "", Spec: &events.TaskSpec{Title: "x"}})
	if len(fires) != 1 {
		t.Fatalf("want one mirror_failed event, got %d", len(fires))
	}
	got := fires[0]
	if got.kind != TaskEventMirrorFailed || got.payload["op"] != "started" ||
		got.payload["owner_id"] != "owner-1" || got.payload["channel_id"] != "ch-9" {
		t.Fatalf("payload: %+v", got)
	}
	if msg, _ := got.payload["exc_message"].(string); msg == "" {
		t.Fatalf("exc_message missing: %+v", got.payload)
	}

	// Progress for an unknown task is the expected swallowed case, not a failure.
	s.Send(events.TaskProgress{TaskID: "never-started", Payload: map[string]any{"a": 1}})
	if len(fires) != 1 {
		t.Fatalf("ErrTaskNotFound must not escalate: %d events", len(fires))
	}
}

func TestFireTaskEventWithoutHookIsNoop(t *testing.T) {
	h := newTestHub(t)
	h.FireTaskEvent("t", TaskEventMirrorFailed, nil) // must not panic
}

// The owner's max_concurrent_tasks cap refuses a new task once that many are
// non-terminal, and is never applied to a re-observation of a known task.
func TestObserveTaskMaxConcurrentTasks(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)
	alice, err := hc.Register("alice", WithAgentRule(Rule{
		InboundGlobs:       []string{"*"},
		OutboundGlobs:      []string{"*"},
		MaxConcurrentTasks: 1,
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()
	owner := alice.AgentID()

	if err := h.ObserveTask(task.Metadata{TaskID: "t-1", OwnerID: owner, State: task.StateRunning}); err != nil {
		t.Fatalf("first task: %v", err)
	}
	if err := h.ObserveTask(task.Metadata{TaskID: "t-2", OwnerID: owner, State: task.StateRunning}); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("second task over the cap: %v, want ErrAccessDenied", err)
	}
	// Re-observing the task that is already counted is not a new task.
	if err := h.ObserveTask(task.Metadata{TaskID: "t-1", Progress: map[string]any{"pct": 50}}); err != nil {
		t.Fatalf("re-observe: %v", err)
	}
	// A terminal task frees its slot.
	if err := h.UpdateTask("t-1", task.StateCompleted, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.ObserveTask(task.Metadata{TaskID: "t-2", OwnerID: owner, State: task.StateRunning}); err != nil {
		t.Fatalf("after the slot freed: %v", err)
	}
	// A different owner has its own budget (bob has no cap at all).
	if err := h.ObserveTask(task.Metadata{TaskID: "t-bob", OwnerID: "someone-else", State: task.StateRunning}); err != nil {
		t.Fatalf("uncapped owner: %v", err)
	}
}
