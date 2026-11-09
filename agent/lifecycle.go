package agent

import (
	stdcontext "context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/hitl"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/observers"
	"github.com/luckystrker/ago/plugin"
	"github.com/luckystrker/ago/response"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/task"
	"github.com/luckystrker/ago/tools"
	"github.com/luckystrker/ago/usage"
)

// ParentCancelledReason is the cancellation reason of a delegation stopped by
// its caller (upstream tools/subagents/_forwarding.py PARENT_CANCELLED). It is
// the delegation lifecycle's cancel reason; an ordinary run reports the default
// "cancelled".
const ParentCancelledReason = "parent cancelled"

// RunOptions are the per-invocation options of one ask/run/resume (Python
// parity: the keyword arguments those methods accept). Capability and TTL drive
// the task lifecycle: either one enables it, zero options keep an ordinary call
// emitting no Task events (upstream ADR 0035). Every other field is an override
// that applies to this invocation only; its zero value keeps the agent's
// configuration, so the struct is backward compatible.
//
// AGO-DEVIATION: Go has no keyword arguments, so the options travel as this
// value through the AskWith/RunWith/ResumeWith family; Ask/Run/Resume stay the
// zero-options spelling. See docs/DECISIONS.md D-093a.
type RunOptions struct {
	// Capability tags the task with a capability name from the owner's track
	// record; observers record terminal stats for tagged tasks.
	Capability string
	// TTL is the task's observation deadline. It only populates metadata and
	// TaskStarted.expires_at — an external observer calls Expire (Python parity).
	TTL time.Duration

	// Stream runs the invocation on an existing stream instead of the agent's
	// stream base or a fresh one (Python parity: stream= of ask/run/resume). For
	// a resume the seeded trajectory replaces that stream's history. Continuations
	// (AgentReply.Ask/Run) keep the reply's stream and ignore it.
	Stream stream.Stream
	// Config replaces the agent's model config for this invocation (Python
	// parity: config=). nil keeps the agent's config.
	Config config.ModelConfig
	// Tools are appended to the agent's tools for this invocation (Python
	// parity: tools=). They are not visible to delegated children, exactly like
	// tools attached at construction.
	Tools []tools.Tool
	// Middleware is appended after the agent's middleware, so it lands innermost
	// in the turn onion (Python parity: middleware=).
	Middleware []middleware.Factory
	// Observers are appended after the agent's observers; they register on the
	// turn's stream and are bracketed by ObserverStarted/ObserverCompleted
	// (Python parity: observers=).
	Observers []observers.Observer
	// Plugins apply their contributions for this invocation only (Python
	// parity: plugins=). Unlike construction-time plugins (WithPlugin), the
	// attachments do not persist past the turn.
	//
	// AGO-DEVIATION: a plugin's dynamic prompt hooks run only on a turn that
	// seeds its prompt (a fresh conversation); a continuation keeps the prompt it
	// cloned, so re-running the hooks would duplicate their fragments.
	Plugins []plugin.Plugin
	// HitlHook replaces the agent's human-input hook for this invocation
	// (Python parity: hitl_hook=). nil keeps the agent's hook; a non-nil hook
	// takes precedence over it for this call only.
	HitlHook hitl.HumanHook
	// Dependencies overlay the agent's dependencies for this invocation (Python
	// parity: dependencies=).
	Dependencies map[string]any
	// Variables overlay the conversation variables for this invocation (Python
	// parity: variables=); scoped declarations are honored.
	Variables map[string]any
	// Prompt replaces the system-prompt fragments for this invocation (Python
	// parity: prompt=). A non-empty slice suppresses the agent's own static
	// fragments for the call; nil keeps them.
	Prompt []string
	// ResponseSchema replaces the agent's response schema for this invocation
	// (Python parity: the response_schema= argument). nil keeps the agent's.
	ResponseSchema response.Proto
	// FreeText asks for free-text replies on this invocation even when the agent
	// has a response schema (Python parity: response_schema=None, which its
	// `omit` sentinel distinguishes from "not passed"). FreeText wins over
	// ResponseSchema.
	FreeText bool
}

// enabled reports whether the options ask for a task lifecycle.
func (o RunOptions) enabled() bool { return o.Capability != "" || o.TTL > 0 }

// runLifecycle is the task lifecycle of one model run or delegation (upstream
// ag2/_run_lifecycle.py RunLifecycle). It wraps a task.Task without injecting
// it: the task is created when the invocation is driven, a terminal is chosen
// exactly once from failure/result, and every path (turn error, cancellation,
// panic recovery, usage-read failure) publishes it.
//
// It is used under the owning run's per-stream turn lock. failure/result are
// written on the turn's goroutine, or after awaitTurn froze it, never
// concurrently.
type runLifecycle struct {
	owner string
	spec  task.Spec
	cx    *context.Context

	ttl              time.Duration
	taskID           string
	parentToolCallID string
	// accounting returns source UsageEvents the forwarder retained outside the
	// lifecycle stream; they are persisted before the terminal and included in
	// its snapshot (upstream persist_accounting/snapshot_usage).
	accounting func() []events.Event
	// publisher routes events away from the bound stream (the delegation reject
	// path). nil means author on cx.Stream.
	publisher func(events.Event) error
	// cancelReason is reported by TaskCancelled; delegations use
	// ParentCancelledReason, ordinary runs "cancelled".
	cancelReason string

	failure error
	result  any
	t       *task.Task
}

func newRunLifecycle(owner string, spec task.Spec, cx *context.Context) *runLifecycle {
	return &runLifecycle{owner: owner, spec: spec, cx: cx, cancelReason: "cancelled"}
}

// started reports whether the task has been driven (created or terminal).
func (l *runLifecycle) started() bool {
	return l != nil && l.t != nil && l.t.State() != task.StateCreated
}

// start opens the task and publishes TaskStarted. Idempotent: a second call
// (for example finish after a rejected delegation) does not restart it. A
// publication failure keeps the handle so finish can still publish a terminal.
func (l *runLifecycle) start() error {
	if l == nil || l.t != nil {
		return nil
	}
	opts := []task.Option{
		task.WithTaskID(l.taskID),
		task.WithParentToolCallID(l.parentToolCallID),
		task.WithoutInjection(),
		task.WithTTL(l.ttl),
	}
	if l.publisher != nil {
		opts = append(opts, task.WithPublisher(l.publisher))
	}
	l.t = task.New(l.cx, l.owner, l.spec, opts...)
	if err := l.t.Start(); err != nil {
		if l.failure == nil {
			l.failure = err
		}
		return err
	}
	return nil
}

// reject records a failure for a delegation that never reached its first step
// and publishes the start+terminal pair through the configured publisher
// (upstream RunLifecycle.reject). It never touches cx.Stream.
func (l *runLifecycle) reject(failure error) error {
	if l == nil {
		return failure
	}
	if l.failure == nil {
		l.failure = failure
	}
	if err := l.start(); err != nil {
		// The original failure stays the terminal's cause.
		slog.Error("agent: task lifecycle start failed on a rejected invocation", "agent", l.owner, "err", err)
	}
	return l.finish()
}

// finish publishes the terminal exactly once. It persists retained accounting
// first and snapshots usage for TaskCompleted; a retention failure decides the
// terminal when nothing failed before, and is logged and suppressed behind an
// earlier failure (upstream RunLifecycle.finish). A cleanup error that decided
// the terminal is returned after the terminal is published.
func (l *runLifecycle) finish() error {
	if l == nil || l.t == nil {
		return nil
	}
	state := l.t.State()
	if state == task.StateCreated {
		return nil
	}

	var (
		accounting []events.Event
		cleanupErr error
	)
	if l.accounting != nil {
		accounting = l.accounting()
		if err := persistUsageAccounting(l.cx, accounting); err != nil {
			if l.failure == nil {
				l.failure = err
				cleanupErr = err
			} else {
				slog.Warn("agent: retaining task usage failed after the run failed",
					"agent", l.owner, "err", err, "failure", l.failure)
			}
		}
	}

	// An adopted (already terminal) task still retains captured accounting, but
	// publishes no second terminal.
	driven := !state.Terminal()
	if !driven {
		return cleanupErr
	}

	var snapshot events.Usage
	if l.failure == nil {
		snapshot = snapshotUsage(l.cx.Stream, accounting)
	}

	var err error
	switch {
	case l.failure == nil:
		err = l.t.CompleteWithUsage(l.result, snapshot)
	case errors.Is(l.failure, stdcontext.Canceled):
		err = l.t.Cancel(l.cancelReason)
	default:
		err = l.t.Fail(l.failure)
	}
	if cleanupErr != nil {
		return cleanupErr
	}
	return err
}

// snapshotUsage reads the stream's cumulative spend, falling back to retained
// accounting when the transcript cannot be read (upstream snapshot_usage: a
// storage failure must not replace the task's outcome). Records are deduplicated
// by event_id, so the snapshot is never additive.
func snapshotUsage(s stream.Stream, accounting []events.Event) events.Usage {
	var evs []events.Event
	if s != nil {
		evs, _ = s.History().GetEvents()
	}
	if len(accounting) == 0 {
		return usage.FromEvents(evs).Total
	}
	all := make([]events.Event, 0, len(evs)+len(accounting))
	all = append(all, evs...)
	all = append(all, accounting...)
	return usage.FromEvents(all).Total
}

// persistUsageAccounting appends source records the relay captured but the
// lifecycle stream never persisted (a delivery failure between the relay and the
// transcript). It is an append, not a replace: siblings can publish
// concurrently, and readers deduplicate by event_id (upstream
// persist_accounting). A read failure is left to the storage layer: upstream
// preserves the original failure, so nothing is appended.
func persistUsageAccounting(cx *context.Context, evs []events.Event) error {
	if cx == nil || cx.Stream == nil || len(evs) == 0 {
		return nil
	}
	history, err := cx.Stream.History().GetEvents()
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{}, len(history))
	for _, ev := range history {
		if record, ok := usage.EventFrom(ev); ok && record.EventID != "" {
			seen[record.EventID] = struct{}{}
		}
	}
	for _, ev := range evs {
		record, ok := usage.EventFrom(ev)
		if !ok {
			continue
		}
		if record.EventID != "" {
			if _, exists := seen[record.EventID]; exists {
				continue
			}
		}
		if err := cx.Stream.History().Append(ev); err != nil {
			return fmt.Errorf("agent: retain task usage: %w", err)
		}
		if record.EventID != "" {
			seen[record.EventID] = struct{}{}
		}
	}
	return nil
}
