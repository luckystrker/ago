package agent

import (
	stdcontext "context"
	"testing"

	"github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/task"
)

// TestTaskWithSpecCarriesSpecFields pins that the deprecated Agent.Task entry
// point can carry description, payload and capability (Python parity:
// agent.task(title, description=, payload=, capability=)): they reach the
// TaskStarted event that opens the task.
func TestTaskWithSpecCarriesSpecFields(t *testing.T) {
	var streams []stream.Stream
	taskStreamProbe = func(s stream.Stream, _ *context.Context) {
		streams = append(streams, s)
	}
	t.Cleanup(func() { taskStreamProbe = nil })

	a := New("owner", "")
	handle, err := a.TaskWithSpec(stdcontext.Background(), task.Spec{
		Title:       "write the report",
		Description: "quarterly numbers",
		Payload:     map[string]any{"quarter": "Q3"},
		Capability:  "reporting",
	})
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	if len(streams) != 1 {
		t.Fatalf("probed streams = %d, want 1", len(streams))
	}
	defer handle.Cancel("test done")
	evs, err := streams[0].History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var started *events.TaskStarted
	for _, ev := range evs {
		if ts, ok := events.AsValue(ev).(events.TaskStarted); ok {
			s := ts
			started = &s
			break
		}
	}
	if started == nil || started.Spec == nil {
		t.Fatal("no TaskStarted with a spec on the task stream")
	}
	if started.Spec.Description != "quarterly numbers" {
		t.Fatalf("description = %q", started.Spec.Description)
	}
	if got := started.Spec.Payload["quarter"]; got != "Q3" {
		t.Fatalf("payload quarter = %v, want Q3", got)
	}
	if started.Spec.Capability != "reporting" {
		t.Fatalf("capability = %q", started.Spec.Capability)
	}
	if started.Objective != "write the report" {
		t.Fatalf("objective = %q, want the title", started.Objective)
	}
}
