package agent

import (
	stdcontext "context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/hitl"
	"github.com/luckystrker/ago/internal/uuid"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/task"
	"github.com/luckystrker/ago/tools"
)

// TaskConfig configures the auto-injected subtask tools (Python parity:
// agent.py TaskConfig). WithTasks(opts) gives the agent two tools — run_subtask
// and run_subtasks — that spawn bare child Agents; the subtask tools never reach
// the spawned children, so delegation cannot recurse by construction.
//
// Deprecated: TaskConfig and the auto-injected run_subtask/run_subtasks tools
// are deprecated, following upstream AG2 (removal in 1.4.0). Construct worker
// Agents explicitly and expose them via Agent.AsTool instead. The runtime
// behavior is unchanged and stays supported until that removal.
//
// AGO-DEVIATION: Python flags TaskConfig with @deprecated (static analyzers
// only, no runtime warning); AGo flags it with this doc comment — godoc and
// staticcheck surface it, and no runtime warning is emitted either. The
// behavior stays frozen at the v1.2.2 baseline until the removal decision.
type TaskConfig struct {
	// Config is the child model config; nil falls back to the parent's config.
	Config config.ModelConfig
	// Prompt is the child system prompt.
	Prompt string
	// IncludeTools filters the parent tools the child inherits: nil (default)
	// means the child inherits all parent user tools (Python include_tools=None);
	// non-nil is a name allowlist (an empty slice = no parent tools). ExcludeTools
	// is applied after the allowlist.
	IncludeTools []string
	// ExcludeTools removes parent tools by name.
	ExcludeTools []string
	// ExtraTools are added to the child in both modes.
	ExtraTools []tools.Tool
	// MaxConcurrency caps run_subtasks parallelism; 0 = unlimited.
	MaxConcurrency int
}

// bypassKey is the variables key Python's approval middleware stores "always"
// grants under (middleware/builtin/tools/approval.py BYPASS_KEY). It lives under
// the reserved "ag:" prefix, so derive_child_variables never hands it to a
// sub-task: the child asks again. The constant remains the contract used by the
// delegation tests and documents the key the reserved-prefix rule covers.
const bypassKey = "ag:approval_required:always"

// Delegation tool descriptions (Python parity: _RUN_SUBTASK_DESCRIPTION /
// _RUN_SUBTASKS_DESCRIPTION verbatim).
const (
	runSubtaskDescription = "Spawn an isolated subtask agent for self-contained focused work. " +
		"The subtask runs with a fresh conversation history and inherits this " +
		"agent's tools, then returns its result as a string. " +
		"You can call this tool multiple times in parallel within a single " +
		"response — each call runs concurrently in its own context. For " +
		"deliberate fan-out with one tool call, use run_subtasks instead."
	runSubtasksDescription = "Run multiple subtasks bundled into one tool call. Each task runs in " +
		"its own isolated subtask agent (fresh history, parent's tools). " +
		"Set parallel=True (default) to dispatch them concurrently — preferred " +
		"when the tasks are independent. parallel=False runs them sequentially " +
		"and is useful only when later tasks depend on earlier results."
)

const defaultTaskPrompt = "You are a task agent. Complete the given objective and report the result."

// subtaskTools builds the auto-injected delegation tools for this turn
// (Python parity: _build_subtask_toolkit). Returns nil when tasks are off.
func (r *AgentRun) subtaskTools(parentOpts options) []tools.Tool {
	if parentOpts.tasks == nil {
		return nil
	}
	tc := parentOpts.tasks
	return []tools.Tool{r.newRunSubtaskTool(tc), r.newRunSubtasksTool(tc)}
}

// childAgent builds one child agent for a subtask call. A config falling back to
// nil is not an error here: Python constructs the bare Agent fine and the failure
// surfaces at ask time — inside runTaskOn's lifecycle, so the delegation reports
// TaskFailed and an "Error: ..." output instead of failing the tool call.
func (r *AgentRun) childAgent(tc *TaskConfig, name string) *Agent {
	childCfg := tc.Config
	if childCfg == nil {
		childCfg = r.agent.opts.cfg
	}
	prompt := tc.Prompt
	if prompt == "" {
		prompt = defaultTaskPrompt
	}
	var childTools []tools.Tool
	{
		// Exact _filter_subtask_tools shape: the allowlist is the parent tool
		// filter — nil (default) passes every tool, an empty one passes nothing;
		// the exclude list applies after. nil vs empty is the Python None vs []
		// distinction, which is why the field is a slice, not a bool.
		allow := map[string]struct{}{}
		if tc.IncludeTools != nil {
			for _, n := range tc.IncludeTools {
				allow[n] = struct{}{}
			}
		}
		a := r.agent
		a.mu.Lock()
		for _, t := range a.opts.tools {
			if tc.IncludeTools != nil {
				if _, ok := allow[t.Name()]; !ok {
					continue
				}
			}
			if excluded(tc.ExcludeTools, t.Name()) {
				continue
			}
			childTools = append(childTools, t)
		}
		a.mu.Unlock()
	}
	childTools = append(childTools, tc.ExtraTools...)
	opts := []Option{WithConfig(childCfg), WithPromptFragments(prompt)}
	if len(childTools) > 0 {
		opts = append(opts, WithTools(childTools...))
	}
	return New(name, "", opts...)
}

func excluded(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// taskResult is the Go form of Python run_task.TaskResult.
type taskResult struct {
	taskID      string
	objective   string
	result      string
	completed   bool
	cancelled   bool
	childStream stream.Stream
	err         error
}

// heldStreamsDependencyKey carries the chain of stream ids owned by enclosing
// turns into a delegated run, so a grandchild reusing any ancestor's stream is
// rejected before it can block on a non-reentrant per-stream lock.
const heldStreamsDependencyKey = "ago:held-streams"

func heldStreamsOf(cx *context.Context) []string {
	if cx == nil {
		return nil
	}
	if v, ok := cx.Dependency(heldStreamsDependencyKey); ok {
		if ids, ok := v.([]string); ok {
			return append([]string(nil), ids...)
		}
	}
	return nil
}

// runTaskOn runs child as a sub-task of the conversation parentCX with the
// default stream selection (the child's stream base, else a fresh stream over
// the parent's storage).
func runTaskOn(ctx stdcontext.Context, parentCX *context.Context, child *Agent, objective, contextStr string) (taskResult, error) {
	return runTaskWith(ctx, parentCX, child, objective, contextStr, nil, "")
}

// runTaskWith runs child as a sub-task of the conversation parentCX and reports
// the delegation on the parent stream (Python parity:
// tools/subagents/run_task.py run_task): TaskStarted before the child turn,
// TaskCompleted/TaskFailed/TaskCancelled after, all authored on the child stream
// and relayed upward by a Forwarder. Operational child events arrive at the
// parent as ChildEvent observations; source UsageEvents arrive as themselves
// with delegation provenance, so there is no rollup to reconcile. The child
// transcript persists into the parent's storage under the child's own stream id;
// the child run starts from a copy of the parent's dependencies and a scoped
// variable branch (COPIED values deep-copied, LOCAL and reserved control-plane
// keys dropped, SHARED bindings reused), so its mutations stay scoped to the
// branch except for SHARED rebinding. When the child has no HITL hook of its own
// and the parent has one, questions asked inside the child are bridged: the
// child's request is forwarded as a ChildEvent and the parent's hook answers.
//
// factory builds this delegation's stream (nil falls back to the child's stream
// base and then to a fresh stream) and taskID pins the delegation's id (empty
// generates one), which is how AsTool's stream options and the background tool
// address one delegation.
//
// A hitl failure comes back as the error result — it must end the calling turn
// (Python parity: run_task re-raises HumanInputError). Any other failure is
// reported in the returned taskResult with completed=false and becomes the
// delegating tool's "Error: ..." output; the terminal task event is emitted on
// both paths.
func runTaskWith(ctx stdcontext.Context, parentCX *context.Context, child *Agent, objective, contextStr string, factory DelegationStreamFactory, taskID string) (taskResult, error) {
	if taskID == "" {
		taskID = strings.ReplaceAll(uuid.New(), "-", "") // Python parity: uuid4().hex
	}
	prompt := objective
	if contextStr != "" {
		prompt = objective + "\n\n## Context\n" + contextStr
	}

	// The child transcript persists where the parent's does, under the child's
	// own stream id (Python parity: MemoryStream(storage=parent ...storage)).
	// A per-delegation factory wins over the child's stream base, which keeps
	// the persistent-stream behavior of AsTool users.
	var childStream stream.Stream
	if factory != nil {
		childStream = factory(child, parentCX)
	}
	if childStream == nil {
		child.mu.Lock()
		childStream = child.opts.streamBase
		child.mu.Unlock()
	}
	if childStream == nil {
		childStream = stream.NewMemoryStream(stream.WithStorage(parentCX.Stream.History().Storage()))
	}

	// A stream already owned by an enclosing turn in this call chain cannot be
	// entered again: the process-wide turn lock is not reentrant (upstream's
	// asyncio lock would queue forever). Reject the delegation instead of
	// deadlocking; nested delegations normally use a fresh child stream.
	heldStreams := append(heldStreamsOf(parentCX), parentCX.Stream.ID())
	var nestedErr error
	for _, id := range heldStreams {
		if id != "" && id == childStream.ID() {
			nestedErr = fmt.Errorf("%w: stream %s is held by an enclosing turn", ErrNestedSameStream, id)
			break
		}
	}

	// The relay is installed by the child turn scope, after the per-stream lock
	// is held: a delegation queued behind another one on a shared stream must not
	// capture that invocation's events (upstream Forwarder.execution inside
	// _turn_scope). The lifecycle publishes TaskStarted as the first event.
	forwarder := newForwarder(parentCX, childStream, taskID, child.Name())
	var run *AgentRun

	// fail reports a failed delegation the way Python's except branch does:
	// exactly one terminal task event on every path, hitl failures re-raised so
	// they end the calling turn. A cancelled run reports TaskCancelled and is a
	// normal (non-hitl) result for the delegating tool.
	//
	// A failure inside an entered scope closes the run, which awaits the turn,
	// publishes the terminal through the still-installed relay under the
	// per-stream lock, and only then stops the relay and releases the lock
	// (upstream Forwarder.execution's finally). A failure before the scope was
	// entered uses lifecycle.reject: started+terminal go straight to the parent,
	// without touching the child stream.
	fail := func(err error) (taskResult, error) {
		lc := (*runLifecycle)(nil)
		if run != nil {
			lc = run.lifecycle
		}
		if lc == nil {
			lc = rejectLifecycle(child, childStream, forwarder, taskID, objective, parentCX.ToolCallID)
		}
		if lc.failure == nil {
			lc.failure = err
		}
		lc.cancelReason = ParentCancelledReason
		if lc.started() {
			if run != nil {
				run.Close()
			} else if ferr := lc.finish(); ferr != nil {
				slog.Error("agent: delegation terminal publication failed", "agent", child.Name(), "err", ferr)
			}
		} else {
			lc.publisher = forwarder.reject
			if ferr := lc.reject(err); ferr != nil {
				slog.Error("agent: rejected delegation lifecycle failed", "agent", child.Name(), "err", ferr)
			}
			if run != nil {
				run.Close()
			}
		}
		if errors.Is(err, stdcontext.Canceled) {
			return taskResult{
				taskID:      taskID,
				objective:   objective,
				cancelled:   true,
				childStream: childStream,
				err:         err,
			}, nil
		}
		if hitl.IsError(err) {
			return taskResult{}, err
		}
		return taskResult{taskID: taskID, objective: objective, childStream: childStream, err: err}, nil
	}

	if nestedErr != nil {
		return fail(nestedErr)
	}

	var err error
	run, err = child.newRunWith(childStream, context.DeriveChildVariables(parentCX.VariableStore()), prompt)
	if err != nil {
		return fail(err)
	}
	defer run.Close()

	// The delegation uses the same lifecycle as an ordinary run: same Task
	// transitions, objective title, the caller's task id and tool-call id. The
	// forwarder keeps addressing/forwarding; it constructs no terminal events of
	// its own (upstream Forwarder.lifecycle).
	lc := newRunLifecycle(child.Name(), task.Spec{Title: objective}, run.cx)
	lc.taskID = taskID
	lc.parentToolCallID = parentCX.ToolCallID
	lc.accounting = forwarder.accountingEvents
	lc.cancelReason = ParentCancelledReason
	run.lifecycle = lc
	run.delegation = forwarder

	// The child run starts from the parent's dependencies merged over the child
	// agent's own option maps (newRun already seeded those): Python passes
	// dependencies=parent_context.dependencies.copy() into agent.ask, and
	// __build_context unions them as self._agent_dependencies | overrides — the
	// delegated agent's own WithDependencies survive, the parent's snapshot wins
	// only on shared keys. Replacing the map instead would strip the child of its
	// own injected state, which its tools read at call time.
	for k, v := range parentCX.DependenciesSnapshot() {
		run.cx.Dependencies[k] = v
	}
	// Nested delegations extend this forwarder's path when stamping their own
	// provenance.
	run.cx.SetDependency(forwarderDependencyKey, forwarder)
	run.cx.SetDependency(heldStreamsDependencyKey, heldStreams)

	// HITL bridge: a child without its own hook asks through the parent's
	// (Python parity: run_task installs the bridge when the agent has no
	// _hitl_hook). The parent hook is the middleware-wrapped one installed on
	// the parent's run context. The question and answer already reach the parent
	// stream as ChildEvent observations through the forwarder; the bridge only
	// supplies the answer, so it must not publish a second copy.
	child.mu.Lock()
	childHook := child.opts.hitl
	child.mu.Unlock()
	if childHook == nil {
		if parentHook := parentCX.Hook(); parentHook != nil {
			run.cx.SetHitlHook(parentHook)
		}
		// Without a hook on either side the child's Context.Ask refuses with
		// hitl.NotProvidedError — the default_hitl_hook refusal, carried through
		// the tool layer as a turn-ending hitl error.
	}

	if err := run.Start(ctx); err != nil {
		return fail(err)
	}
	reply, err := run.Result(ctx)
	if err != nil {
		return fail(err)
	}

	// The completed event carries the child stream's cumulative spend for
	// inspection; the additive accounting already travelled as source
	// UsageEvents, so a reader must never sum this snapshot. The deferred Close
	// publishes it (with the forwarder's retained accounting merged in).
	lc.result = reply.Body()
	return taskResult{
		taskID:      taskID,
		objective:   objective,
		result:      reply.Body(),
		completed:   true,
		childStream: childStream,
	}, nil
}

// rejectLifecycle builds the lifecycle of a delegation that never entered its
// turn. Its publisher is the forwarder's reject path, so the started and
// terminal events reach the parent without touching the child stream (which may
// be owned by a busy enclosing turn).
func rejectLifecycle(child *Agent, childStream stream.Stream, f *forwarder, taskID, objective, parentToolCallID string) *runLifecycle {
	lc := newRunLifecycle(child.Name(), task.Spec{Title: objective}, context.New(childStream))
	lc.taskID = taskID
	lc.parentToolCallID = parentToolCallID
	lc.publisher = f.reject
	lc.cancelReason = ParentCancelledReason
	return lc
}

// run_subtask tool -----------------------------------------------------------

func (r *AgentRun) newRunSubtaskTool(tc *TaskConfig) tools.Tool {
	type subtaskArgs struct {
		Task string `json:"task" agschema:"description=The objective for the subtask agent,required"`
	}
	fn := func(args subtaskArgs, cx *context.Context) (string, error) {
		// The singular tool draws from the same per-agent pool as the fan-out
		// tool, so one parent turn (or several) can never exceed
		// TaskConfig.MaxConcurrency (Python parity: _spawn_subtask's
		// _task_slots_for, shared with run_subtasks).
		release := r.agent.acquireTaskSlot(maxConcurrency(tc.MaxConcurrency))
		defer release()
		res, err := runTaskOn(cx.TurnContext(), cx, r.childAgent(tc, "subtask"), args.Task, "")
		if err != nil {
			// hitl: not a tool result — it ends the calling turn (Python parity:
			// run_task re-raises HumanInputError past the delegating tool).
			return "", err
		}
		if res.cancelled {
			return "Sub-task cancelled: " + res.err.Error(), nil
		}
		if !res.completed {
			return "Error: " + res.err.Error(), nil
		}
		return res.result, nil
	}
	t, err := tools.NewFunctionTool("run_subtask", runSubtaskDescription, fn)
	if err != nil {
		return failingTool{err: err}
	}
	return t
}

// run_subtasks tool ----------------------------------------------------------

func (r *AgentRun) newRunSubtasksTool(tc *TaskConfig) tools.Tool {
	type subtasksArgs struct {
		Tasks []string `json:"tasks" agschema:"description=The tasks to delegate to subtask agents,required"`
		// Optional (pointer) field: Python `parallel: bool = True`.
		Parallel *bool `json:"parallel" agschema:"description=Run the subtasks concurrently (the default) or sequentially"`
	}
	fn := func(args subtasksArgs, cx *context.Context) (string, error) {
		parallel := args.Parallel == nil || *args.Parallel
		results := make([]string, len(args.Tasks))
		var (
			hitlErr error
			mu      sync.Mutex
			wg      sync.WaitGroup
		)
		// One pool per agent, shared with the singular tool and with other turns
		// on the same agent (Python parity: Agent._task_slots_for). Every
		// delegation, sequential or concurrent, draws a slot.
		limit := maxConcurrency(tc.MaxConcurrency)
		// runOne renders one sub-task: failures become "Error: ..." text inside
		// the combined output (Python parity: `_spawn_subtask` returns
		// f"Error: {result.error}"), hitl failures end the whole call.
		runOne := func(i int, task string) error {
			release := r.agent.acquireTaskSlot(limit)
			defer release()
			res, err := runTaskOn(cx.TurnContext(), cx, r.childAgent(tc, fmt.Sprintf("subtask-%d", i)), task, "")
			if err != nil {
				return err
			}
			if res.cancelled {
				results[i] = "Sub-task cancelled: " + res.err.Error()
				return nil
			}
			if !res.completed {
				results[i] = "Error: " + res.err.Error()
				return nil
			}
			results[i] = res.result
			return nil
		}
		if parallel {
			for i, task := range args.Tasks {
				wg.Add(1)
				go func(i int, task string) {
					defer wg.Done()
					if err := runOne(i, task); err != nil {
						mu.Lock()
						if hitlErr == nil {
							hitlErr = err
						}
						mu.Unlock()
					}
				}(i, task)
			}
			// Python gathers everything before re-raising: the siblings still run
			// and report their lifecycle events.
			wg.Wait()
		} else {
			for i, task := range args.Tasks {
				if err := runOne(i, task); err != nil {
					// One sub-task that could not reach a human sinks the whole
					// call: rendering it as text would read as a subtask that
					// failed (Python parity: the sequential HumanInputError
					// re-raise, later tasks skipped).
					return "", err
				}
			}
		}
		if hitlErr != nil {
			return "", hitlErr
		}
		parts := make([]string, len(args.Tasks))
		for i, task := range args.Tasks {
			parts[i] = fmt.Sprintf("## %s\n\n%s", task, results[i])
		}
		return strings.Join(parts, "\n\n---\n\n"), nil
	}
	t, err := tools.NewFunctionTool("run_subtasks", runSubtasksDescription, fn)
	if err != nil {
		return failingTool{err: err}
	}
	return t
}

func maxConcurrency(n int) int {
	if n <= 0 {
		return 16 // a sane default so one call cannot fork unbounded goroutines
	}
	return n
}

// acquireTaskSlot takes one slot from the agent's shared subtask pool and
// returns the release function. The pool is created on first use and reused by
// every turn of the agent, so run_subtask and run_subtasks across concurrent
// parent turns share the same cap (Python parity: Agent._task_slots_for, one
// semaphore per agent).
//
// AGO-DEVIATION: Python keys the pool by event loop and drops closed loops; Go
// has no loop object, so one process-wide pool per agent is the equivalent.
// A different limit replaces the pool (TaskConfig is fixed per agent, so this
// only happens if an agent is reconfigured).
func (a *Agent) acquireTaskSlot(limit int) func() {
	if limit <= 0 {
		return func() {}
	}
	a.taskSlotsMu.Lock()
	if a.taskSlots == nil || a.taskSlotsLimit != limit {
		a.taskSlots = make(chan struct{}, limit)
		a.taskSlotsLimit = limit
	}
	sem := a.taskSlots
	a.taskSlotsMu.Unlock()
	sem <- struct{}{}
	return func() { <-sem }
}

// PersistentStream returns a delegation stream factory that reuses one stream
// per delegated agent for the lifetime of a conversation (Python parity:
// tools/subagents/persistent_stream.py). The stream id is cached in the caller's
// dependencies under "ag:<agent>:stream"; each delegation rebuilds a stream from
// that id and the parent's storage, so the worker reads back its own accumulated
// history while each delegation still gets its own subscriber set and turn lock.
//
// The same warning as Python's applies: the sub-agent becomes stateful, and
// token cost grows with every delegation.
func PersistentStream() DelegationStreamFactory {
	return func(agent *Agent, cx *context.Context) stream.Stream {
		if agent == nil || cx == nil {
			return nil
		}
		key := "ag:" + agent.Name() + ":stream"
		id, _ := cx.Dependency(key)
		streamID, _ := id.(string)
		if streamID == "" {
			streamID = "s-" + uuid.New()
			cx.SetDependency(key, streamID)
		}
		return stream.NewMemoryStream(
			stream.WithStorage(cx.Stream.History().Storage()),
			stream.WithStreamID(streamID),
		)
	}
}
