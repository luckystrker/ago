package agent

import (
	stdcontext "context"
	"errors"
	"testing"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/task"
)

// taskRecorder captures every event flowing on a task's private stream,
// transient or not. Delivery is synchronous in the sender's goroutine and the
// tests below drive the task from a single goroutine, so no locking is needed.
type taskRecorder struct {
	seen []events.Event
}

func subscribeTaskRecorder(s stream.Stream) *taskRecorder {
	rec := &taskRecorder{}
	s.Subscribe(func(ev events.Event) error {
		rec.seen = append(rec.seen, ev)
		return nil
	}, stream.SubscribeOptions{})
	return rec
}

func (r *taskRecorder) count(eventType string) int {
	n := 0
	for _, ev := range r.seen {
		if ev.EventType() == eventType {
			n++
		}
	}
	return n
}

func (r *taskRecorder) find(eventType string) events.Event {
	for _, ev := range r.seen {
		if ev.EventType() == eventType {
			return ev
		}
	}
	return nil
}

// probeTaskStream installs the taskStreamProbe seam so the test subscribes to
// the private stream before TaskStarted fires, and restores the seam on cleanup.
func probeTaskStream(t *testing.T, fn func(s stream.Stream, cx *agocontext.Context)) {
	t.Helper()
	taskStreamProbe = fn
	t.Cleanup(func() { taskStreamProbe = nil })
}

// TestAgentTaskLifecycle drives the full caller-owned flow through Agent.Task:
// Start → Progress → Checkpoint → Complete, with the lifecycle events landing
// on the task's private stream.
func TestAgentTaskLifecycle(t *testing.T) {
	var (
		rec  *taskRecorder
		priv stream.Stream
	)
	probeTaskStream(t, func(s stream.Stream, _ *agocontext.Context) {
		rec = subscribeTaskRecorder(s)
		priv = s
	})

	store := task.NewMemoryCheckpointStore()
	a := New("worker", "")
	tsk, err := a.Task(stdcontext.Background(), "ship the release",
		TaskWithTTL(time.Minute), TaskWithCheckpointStore(store))
	if err != nil {
		t.Fatalf("task: %v", err)
	}

	// Start: running, owned by the agent, titled, TTL forwarded.
	if tsk.State() != task.StateRunning {
		t.Fatalf("state = %q, want running", tsk.State())
	}
	md := tsk.Metadata()
	if md.OwnerID != "worker" || md.Spec.Title != "ship the release" {
		t.Fatalf("metadata = %+v", md)
	}
	if md.ExpiresAt.IsZero() {
		t.Fatal("expires_at not set (TaskWithTTL not forwarded)")
	}
	started, ok := rec.find("TaskStarted").(*events.TaskStarted)
	if !ok {
		t.Fatal("TaskStarted not emitted on the task stream")
	}
	if started.TaskID != tsk.TaskID() || started.AgentName != "worker" || started.Objective != "ship the release" {
		t.Fatalf("TaskStarted = %+v", started)
	}
	if _, err := time.Parse(time.RFC3339, started.ExpiresAt); err != nil {
		t.Fatalf("TaskStarted.expires_at not RFC3339: %q (%v)", started.ExpiresAt, err)
	}
	select {
	case <-tsk.Done():
		t.Fatal("done closed while running")
	default:
	}

	// Progress: a live (transient) event plus merged metadata.
	if err := tsk.Progress(map[string]any{"step": "build"}); err != nil {
		t.Fatalf("progress: %v", err)
	}
	if rec.count("TaskProgress") != 1 {
		t.Fatalf("TaskProgress events = %d, want 1", rec.count("TaskProgress"))
	}
	pg, ok := rec.find("TaskProgress").(*events.TaskProgress)
	if !ok || pg.Payload["step"] != "build" || pg.AgentName != "worker" || pg.Objective != "ship the release" {
		t.Fatalf("TaskProgress = %+v", pg)
	}
	if md := tsk.Metadata(); md.Progress["step"] != "build" || md.LastProgressAt.IsZero() {
		t.Fatalf("progress metadata = %+v", md)
	}

	// Checkpoint lands in the store under the task id.
	if err := tsk.Checkpoint([]byte(`{"step":"build"}`)); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	got, err := store.Read(tsk.TaskID())
	if err != nil {
		t.Fatalf("store read: %v", err)
	}
	if string(got) != `{"step":"build"}` {
		t.Fatalf("checkpoint = %s", got)
	}

	// Complete: terminal, Done closed, event on the same stream.
	if err := tsk.Complete("shipped"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if tsk.State() != task.StateCompleted {
		t.Fatalf("state = %q, want completed", tsk.State())
	}
	if md := tsk.Metadata(); md.Result != "shipped" || md.CompletedAt.IsZero() {
		t.Fatalf("metadata after complete = %+v", md)
	}
	select {
	case <-tsk.Done():
	default:
		t.Fatal("done not closed after complete")
	}
	done, ok := rec.find("TaskCompleted").(*events.TaskCompleted)
	if !ok {
		t.Fatal("TaskCompleted not emitted")
	}
	if done.Result != "shipped" || done.TaskStream != priv.ID() {
		t.Fatalf("TaskCompleted = %+v (want task_stream %q)", done, priv.ID())
	}

	// Durability: TaskStarted/TaskCompleted persist, transient progress does not.
	history, err := priv.History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var startedN, completedN, progressN int
	for _, ev := range history {
		switch ev.EventType() {
		case "TaskStarted":
			startedN++
		case "TaskCompleted":
			completedN++
		case "TaskProgress":
			progressN++
		}
	}
	if startedN != 1 || completedN != 1 || progressN != 0 {
		t.Fatalf("history started=%d completed=%d progress=%d", startedN, completedN, progressN)
	}
}

// TestAgentTaskSeedsDependencies: the private context is seeded with the
// agent's dependencies and the active task is stamped under task.DepKey; on a
// terminal transition the stamp rolls back. Variables are not copied.
func TestAgentTaskSeedsDependencies(t *testing.T) {
	var priv *agocontext.Context
	probeTaskStream(t, func(_ stream.Stream, cx *agocontext.Context) { priv = cx })

	a := New("worker", "",
		WithDependencies(map[string]any{"db": "pg"}),
		WithVariables(map[string]any{"k": "v"}))
	tsk, err := a.Task(stdcontext.Background(), "job")
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	if v, ok := priv.Dependency("db"); !ok || v != "pg" {
		t.Fatalf("dependency db = %q (ok=%v), want pg", v, ok)
	}
	if dep, ok := priv.Dependency(task.DepKey); !ok || dep != tsk {
		t.Fatalf("task dependency not stamped: ok=%v", ok)
	}
	if _, ok := priv.Variable("k"); ok {
		t.Fatal("variables must not be copied into the task context")
	}

	if err := tsk.Complete("done"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if dep, ok := priv.Dependency(task.DepKey); !ok || dep != nil {
		t.Fatalf("dependency not rolled back after complete: ok=%v dep=%v", ok, dep)
	}
}

// TestAgentTaskOptionsForwarding: arbitrary task options pass through — a
// checkpoint written by one task is resumed by the next (WithResumeFrom), and a
// missing or nil store turns Checkpoint into a silent no-op.
func TestAgentTaskOptionsForwarding(t *testing.T) {
	a := New("worker", "")
	store := task.NewMemoryCheckpointStore()

	first, err := a.Task(stdcontext.Background(), "first", TaskWithCheckpointStore(store))
	if err != nil {
		t.Fatalf("first task: %v", err)
	}
	if err := first.Checkpoint([]byte("state-1")); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	second, err := a.Task(stdcontext.Background(), "second",
		TaskWithCheckpointStore(store), task.WithResumeFrom(first.TaskID()))
	if err != nil {
		t.Fatalf("second task: %v", err)
	}
	if string(second.ResumedState()) != "state-1" {
		t.Fatalf("resumed = %q, want state-1", second.ResumedState())
	}

	bare, err := a.Task(stdcontext.Background(), "bare")
	if err != nil {
		t.Fatalf("bare task: %v", err)
	}
	if err := bare.Checkpoint([]byte("{}")); err != nil {
		t.Fatalf("checkpoint without store: %v", err)
	}

	nilStore, err := a.Task(stdcontext.Background(), "nilstore", TaskWithCheckpointStore(nil))
	if err != nil {
		t.Fatalf("nil-store task: %v", err)
	}
	if err := nilStore.Checkpoint([]byte("{}")); err != nil {
		t.Fatalf("checkpoint with nil store: %v", err)
	}
}

// TestAgentTaskHonorsCancelledContext: a cancelled call context refuses to open
// a task.
func TestAgentTaskHonorsCancelledContext(t *testing.T) {
	a := New("worker", "")
	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	cancel()
	tsk, err := a.Task(ctx, "job")
	if !errors.Is(err, stdcontext.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if tsk != nil {
		t.Fatal("task returned on a cancelled context")
	}
}
