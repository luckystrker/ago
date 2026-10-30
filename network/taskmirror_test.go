package network

import (
	"errors"
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
