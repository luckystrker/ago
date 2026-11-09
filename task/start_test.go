package task

import (
	"errors"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
)

// TestNewLeavesTheTaskCreated pins the constructor/start split: New builds the
// handle, Start performs the Python __aenter__ side (metadata, injection,
// TaskStarted).
func TestNewLeavesTheTaskCreated(t *testing.T) {
	_, c := newTestContext(t)
	tsk := New(c, "agent", Spec{Title: "t"})
	if tsk.State() != StateCreated || tsk.TaskID() != "" {
		t.Fatalf("state/id before Start = %q/%q", tsk.State(), tsk.TaskID())
	}
	if _, ok := c.Dependency(DepKey); ok {
		t.Fatal("New stamped the task dependency before Start")
	}
	if err := tsk.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if tsk.State() != StateRunning || tsk.TaskID() == "" {
		t.Fatalf("state/id after Start = %q/%q", tsk.State(), tsk.TaskID())
	}
	if got, ok := c.Dependency(DepKey); !ok || got != any(tsk) {
		t.Fatal("Start did not stamp the task dependency")
	}
	if err := tsk.Complete("done"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, ok := c.Dependency(DepKey); ok {
		// Nil is the documented "absent" representation after rollback.
		if v, _ := c.Dependency(DepKey); v != nil {
			t.Fatalf("dependency after terminal = %#v, want nil", v)
		}
	}
	if got := tsk.State(); got != StateCompleted {
		t.Fatalf("state = %q", got)
	}
}

// TestStartWithPublisherBypassesTheStream pins the delegation reject path: a
// lifecycle Task can publish through a relay without touching the bound stream.
func TestStartWithPublisherBypassesTheStream(t *testing.T) {
	s, c := newTestContext(t)
	pubStream := stream.NewMemoryStream()
	tsk := New(c, "agent", Spec{Title: "t"}, WithPublisher(func(ev events.Event) error {
		return pubStream.Send(ev)
	}))
	if err := tsk.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if evs, _ := s.History().GetEvents(); len(evs) != 0 {
		t.Fatalf("bound stream saw %d events, want none", len(evs))
	}
	if evs, _ := pubStream.History().GetEvents(); len(evs) != 1 {
		t.Fatalf("publisher stream saw %d events, want TaskStarted", len(evs))
	}
	if err := tsk.Complete("done"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if evs, _ := pubStream.History().GetEvents(); len(evs) != 2 {
		t.Fatalf("publisher stream saw %d events, want start+terminal", len(evs))
	}
}

// TestStartCarriesExternalIdentity pins the delegation identity: the caller
// supplies the task id and parent tool-call id; both reach the wire shape and
// the terminal keeps the same id.
func TestStartCarriesExternalIdentity(t *testing.T) {
	s, c := newTestContext(t)
	tsk := New(c, "worker", Spec{Title: "work"}, WithTaskID("t-1"), WithParentToolCallID("call-9"))
	rec := subscribe(s)
	if err := tsk.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if tsk.TaskID() != "t-1" {
		t.Fatalf("task id = %q", tsk.TaskID())
	}
	started, ok := rec.find("TaskStarted").(*events.TaskStarted)
	if !ok {
		t.Fatal("TaskStarted not emitted")
	}
	if started.TaskID != "t-1" || started.ParentToolCallID != "call-9" {
		t.Fatalf("started identity = %q/%q", started.TaskID, started.ParentToolCallID)
	}
	if err := tsk.Fail(errors.New("boom")); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if got := tsk.State(); got != StateFailed {
		t.Fatalf("state = %q", got)
	}
}

// TestWithoutInjectionLeavesTheSlotAlone pins the run-lifecycle contract: it
// does not inject itself as the active Task, so an enclosing TaskInject keeps
// resolving to the owner's task.
func TestWithoutInjectionLeavesTheSlotAlone(t *testing.T) {
	_, c := newTestContext(t)
	outer, err := Start(c, "owner", Spec{Title: "outer"})
	if err != nil {
		t.Fatalf("outer start: %v", err)
	}
	inner := New(c, "agent", Spec{Title: "inner"}, WithoutInjection())
	if err := inner.Start(); err != nil {
		t.Fatalf("inner start: %v", err)
	}
	if got, ok := c.Dependency(DepKey); !ok || got != any(outer) {
		t.Fatal("non-injecting task replaced the enclosing task slot")
	}
	if err := inner.Complete(nil); err != nil {
		t.Fatalf("inner complete: %v", err)
	}
	if got, _ := c.Dependency(DepKey); got != any(outer) {
		t.Fatal("non-injecting terminal rolled back a foreign slot")
	}
	if err := outer.Complete(nil); err != nil {
		t.Fatalf("outer complete: %v", err)
	}
}

// TestExternalTerminalIsAdopted pins upstream _observe_terminal: an external
// terminal for the same task id moves the handle to that state without a second
// terminal, and the handle is then immutable.
func TestExternalTerminalIsAdopted(t *testing.T) {
	s, c := newTestContext(t)
	tsk := New(c, "agent", Spec{Title: "t"}, WithTaskID("t-adopt"))
	if err := tsk.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	rec := subscribe(s)
	if err := s.Send(events.TaskExpired{TaskID: "t-adopt", AgentName: "ticker", Objective: "t"}); err != nil {
		t.Fatalf("external terminal: %v", err)
	}
	if got := tsk.State(); got != StateExpired {
		t.Fatalf("state = %q, want expired", got)
	}
	if err := tsk.Complete("late"); err != nil {
		t.Fatalf("complete after adoption: %v", err)
	}
	if got := tsk.State(); got != StateExpired {
		t.Fatalf("state after late complete = %q", got)
	}
	if got := rec.count("TaskCompleted"); got != 0 {
		t.Fatalf("TaskCompleted emitted %d times after adoption, want 0", got)
	}
}

// TestStartSendFailureLeavesAHandle pins the always-terminal contract: when the
// TaskStarted send fails, the handle still exists and can publish a terminal
// (the lifecycle finishes through it).
func TestStartSendFailureLeavesAHandle(t *testing.T) {
	storage := failStartedStorage{Storage: stream.NewMemoryStorage()}
	s := stream.NewMemoryStream(stream.WithStorage(storage))
	c := agocontext.New(s)
	tsk := New(c, "agent", Spec{Title: "t"})
	if err := tsk.Start(); err == nil {
		t.Fatal("start send failure swallowed")
	}
	if tsk == nil || tsk.State() != StateRunning {
		t.Fatalf("handle = %v state %q", tsk, tsk.State())
	}
	if err := tsk.Fail(errors.New("storage refused the opening event")); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if got := tsk.State(); got != StateFailed {
		t.Fatalf("state = %q", got)
	}
}

// TestTerminalClearsTheStreamSubscription pins the exactly-once terminal
// wiring in white box: reaching a terminal releases the observation
// subscription (sub reset), so a late owner transition finds no listener to
// contradict and no subscription leaks.
func TestTerminalClearsTheStreamSubscription(t *testing.T) {
	_, c := newTestContext(t)
	tsk := New(c, "agent", Spec{Title: "t"})
	if err := tsk.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	tsk.mu.Lock()
	sub := tsk.sub
	tsk.mu.Unlock()
	if sub == 0 {
		t.Fatal("started task holds no observation subscription")
	}
	if err := tsk.Complete("done"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	tsk.mu.Lock()
	after := tsk.sub
	tsk.mu.Unlock()
	if after != 0 {
		t.Fatalf("subscription id = %d after terminal, want released", after)
	}
}

// failStartedStorage rejects any transcript write that contains a TaskStarted.
type failStartedStorage struct{ stream.Storage }

func (f failStartedStorage) SetHistory(id string, evs []events.Event) error {
	for _, ev := range evs {
		if _, ok := events.AsValue(ev).(events.TaskStarted); ok {
			return errors.New("storage down on TaskStarted")
		}
	}
	return f.Storage.SetHistory(id, evs)
}
