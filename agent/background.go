package agent

import (
	stdcontext "context"
	"fmt"
	"strings"

	"github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/internal/uuid"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
)

// AsBackgroundTool exposes this agent as a fire-and-forget delegation tool
// (Python parity: tools/subagents/background.py background_agent_tool). A call
// starts the sub-agent on a stream-tracked background goroutine and returns
// "Background task started: <task id>" at once; the parent turn keeps running.
// When the child finishes, its outcome is enqueued on the caller's stream as a
// follow-up message, so the next model call sees it. The child's lifecycle
// (TaskStarted/TaskCompleted or TaskFailed) and its operational events reach the
// caller's stream through the ordinary delegation relay.
//
// The background run is detached from the calling turn: it is spawned in the
// stream's scope, so it survives the turn that started it (Python parity:
// Context.spawn_background). Only the process bounds its lifetime.
//
// Options are the AsTool ones: name (default "background_task_<agent>"),
// description, WithToolMiddleware and the stream options.
func (a *Agent) AsBackgroundTool(opts ...AsToolOption) tools.Tool {
	var o asToolOptions
	for _, f := range opts {
		f(&o)
	}
	name := o.name
	if name == "" {
		// Python parity: background.py `name or f"background_task_{agent.name}"`.
		name = "background_task_" + sanitizeToolName(a.name)
	}
	desc := o.description
	if desc == "" {
		desc = "Start the agent " + a.name + " on a background task and return its task id."
	}
	type subArgs struct {
		Objective string `json:"objective" agschema:"description=The objective for the background agent,required"`
		Context   string `json:"context" agschema:"description=Optional additional context for the background agent"`
	}
	fn := func(args subArgs, cx *context.Context) (string, error) {
		taskID := strings.ReplaceAll(uuid.New(), "-", "") // Python parity: uuid4().hex
		// The detached run gets its own stream up front so the tool can report
		// the task id before the child has even started.
		childStream := stream.Stream(nil)
		if o.streamFactory != nil {
			childStream = o.streamFactory(a, cx)
		}
		if childStream == nil {
			childStream = stream.NewMemoryStream(stream.WithStorage(cx.Stream.History().Storage()))
		}
		parent := cx
		objective, contextStr := args.Objective, args.Context
		cx.SpawnBackground(stdcontext.Background(), func(_ stdcontext.Context) {
			deliverBackgroundResult(parent, a, childStream, objective, contextStr, taskID)
		})
		return fmt.Sprintf("Background task started: %s", taskID), nil
	}
	ft, err := tools.NewFunctionTool(name, desc, fn, tools.WithMiddleware(o.middleware...))
	if err != nil {
		return failingTool{err: fmt.Errorf("agent: background tool: %w", err)}
	}
	return ft
}

// deliverBackgroundResult runs one background delegation on its own stream and
// enqueues the outcome as a follow-up message on the caller's stream (Python
// parity: _run_and_deliver). A hitl failure is reported as a failed task, not
// re-raised: there is no calling turn left to end.
func deliverBackgroundResult(parent *context.Context, child *Agent, childStream stream.Stream, objective, contextStr, taskID string) {
	res, err := runTaskWith(stdcontext.Background(), parent, child, objective, contextStr,
		func(*Agent, *context.Context) stream.Stream { return childStream }, taskID)
	var message string
	switch {
	case err != nil:
		message = fmt.Sprintf("Background task %s (%s) failed:\n%s", taskID, objective, err)
	case res.cancelled:
		message = fmt.Sprintf("Background task %s (%s) failed:\n%s", taskID, objective, res.err)
	case !res.completed:
		message = fmt.Sprintf("Background task %s (%s) failed:\n%s", taskID, objective, res.err)
	default:
		message = fmt.Sprintf("Background task %s (%s) completed:\n%s", taskID, objective, res.result)
	}
	// A service-origin enqueue: the result must not open a new user interval
	// (Python parity: context.enqueue's default origin).
	_ = parent.EnqueueOrigin(events.OriginService, events.EnsureInput(message))
}
