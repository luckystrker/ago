package agent

import (
	stdcontext "context"
	"time"

	"github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/task"
)

// Task opens a tracked unit of work owned by this agent and returns its
// lifecycle handle (Python parity: agent.task — an unentered Task the caller
// drives through Progress/Complete/Fail/Cancel/Expire; Agent.Task only creates
// and binds it). The TaskStarted event and the terminal events fired by the
// handle flow on the task's private conversation stream.
//
// AGO-DEVIATION: Python's agent.task accepts an explicit Context and only falls
// back to a private stream when none is supplied; the AGo signature has no
// context parameter, so Task always builds a private *context.Context over a
// fresh MemoryStream, seeded with the agent's dependencies (WithDependencies)
// so injected values resolve the same way they do inside a turn.
func (a *Agent) Task(ctx stdcontext.Context, title string, opts ...task.Option) (*task.Task, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	s := stream.NewMemoryStream()
	deps := make(map[string]any, len(a.opts.dependencies))
	a.mu.Lock()
	for k, v := range a.opts.dependencies {
		deps[k] = v
	}
	a.mu.Unlock()
	cx := context.NewWithDeps(s, deps, map[string]any{})
	if taskStreamProbe != nil {
		taskStreamProbe(s, cx)
	}
	return task.Start(cx, a.name, task.Spec{Title: title}, opts...)
}

// taskStreamProbe is a test seam: when non-nil it receives the private stream
// and context a task is about to live on, before TaskStarted fires — the same
// window Python observers get by passing context to agent.task. Production
// leaves it nil, so the cost of a task is one nil check.
var taskStreamProbe func(s stream.Stream, cx *context.Context)

// TaskWithTTL proxies task.WithTTL (Python parity: the ttl_seconds parameter of
// agent.task).
func TaskWithTTL(d time.Duration) task.Option { return task.WithTTL(d) }

// TaskWithCheckpointStore proxies task.WithCheckpointStore (Python parity: the
// checkpoint_store parameter of agent.task). A nil store is ignored, matching
// task.WithCheckpointStore.
func TaskWithCheckpointStore(store task.CheckpointStore) task.Option {
	return task.WithCheckpointStore(store)
}
