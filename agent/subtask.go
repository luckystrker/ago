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
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/hitl"
	"github.com/luckystrker/ago/internal/uuid"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
	"github.com/luckystrker/ago/usage"
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

// childStreamUsage reports the cumulative spend recorded on the delegation's
// own stream plus source records the forwarder retained while publishing
// upward. It is a snapshot for inspection (TaskCompleted.Usage), never an
// additive record: the source UsageEvents have already been forwarded to the
// parent, so a report that summed this field as well would double count.
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

func childStreamUsage(childStream stream.Stream, f *forwarder) events.Usage {
	if f != nil {
		return f.usage(childStream)
	}
	evs, err := childStream.History().GetEvents()
	if err != nil {
		return events.Usage{}
	}
	return usage.FromEvents(evs).Total
}

// runTaskOn runs child as a sub-task of the conversation parentCX and reports
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
// A hitl failure comes back as the error result — it must end the calling turn
// (Python parity: run_task re-raises HumanInputError). Any other failure is
// reported in the returned taskResult with completed=false and becomes the
// delegating tool's "Error: ..." output; the terminal task event is emitted on
// both paths.
func runTaskOn(ctx stdcontext.Context, parentCX *context.Context, child *Agent, objective, contextStr string) (taskResult, error) {
	taskID := strings.ReplaceAll(uuid.New(), "-", "") // Python parity: uuid4().hex
	prompt := objective
	if contextStr != "" {
		prompt = objective + "\n\n## Context\n" + contextStr
	}

	// The child transcript persists where the parent's does, under the child's
	// own stream id (Python parity: MemoryStream(storage=parent ...storage)).
	// A stream base set on the child agent keeps the persistent-stream behavior
	// of AsTool users.
	child.mu.Lock()
	base := child.opts.streamBase
	child.mu.Unlock()
	childStream := base
	if childStream == nil {
		childStream = stream.NewMemoryStream(stream.WithStorage(parentCX.Stream.History().Storage()))
	}

	// A stream already owned by an enclosing turn in this call chain cannot be
	// entered again: the process-wide turn lock is not reentrant (upstream's
	// asyncio lock would queue forever). Reject the delegation instead of
	// deadlocking; nested delegations normally use a fresh child stream.
	heldStreams := append(heldStreamsOf(parentCX), parentCX.Stream.ID())
	for _, id := range heldStreams {
		if id != "" && id == childStream.ID() {
			return taskResult{
				taskID:      taskID,
				objective:   objective,
				childStream: childStream,
				err:         fmt.Errorf("%w: stream %s is held by an enclosing turn", ErrNestedSameStream, id),
			}, nil
		}
	}

	// The relay is installed by the child turn scope, after the per-stream lock
	// is held: a delegation queued behind another one on a shared stream must not
	// capture that invocation's events (upstream Forwarder.execution inside
	// _turn_scope). TaskStarted is the first event the scope publishes.
	forwarder := newForwarder(parentCX, childStream, taskID, child.Name())
	startedEvent := &events.TaskStarted{
		TaskID:           taskID,
		AgentName:        child.Name(),
		Objective:        objective,
		ParentToolCallID: parentCX.ToolCallID,
	}
	scopeOpen, startedPublished := false, false
	var run *AgentRun

	// publish authors an event on the child stream. While the turn scope is
	// open the ordinary stream path persists and the relay forwards it; before
	// the scope opened (or after a failed Start) there is no subscription, so the
	// event is relayed directly to the parent.
	publish := func(ev events.Event) error {
		return forwarder.publish(childStream, scopeOpen, ev)
	}

	// publishAfterTurn persists an event on the child stream (best effort) and
	// then relays it to the parent. It is used only after the child turn scope
	// has been closed, so the terminal is the last event the parent observes for
	// this delegation.
	publishAfterTurn := func(ev events.Event) error {
		if childStream.ID() == parentCX.Stream.ID() {
			return childStream.Send(ev)
		}
		if serr := childStream.Send(ev); serr != nil {
			slog.Error("agent: delegation terminal persist failed", "agent", child.Name(), "err", serr)
		}
		return forwarder.forward(ev)
	}

	// fail reports a failed delegation the way Python's except branch does:
	// terminal task event on every path, hitl failures re-raised so they end the
	// calling turn. A cancelled run reports TaskCancelled and is a normal
	// (non-hitl) result for the delegating tool. Terminal publication is
	// best-effort but no longer silent: a broken parent stream is logged.
	fail := func(err error) (taskResult, error) {
		// A turn goroutine that outlived Result must finish and its relay must be
		// gone before the terminal: the terminal is the last event the parent
		// observes (upstream publishes it in Forwarder.execution finally).
		if run != nil {
			run.Close()
		}
		scopeOpen = false
		if !startedPublished {
			if serr := publishAfterTurn(startedEvent); serr != nil {
				slog.Error("agent: delegation TaskStarted send failed", "agent", child.Name(), "err", serr)
			}
			startedPublished = true
		}
		if errors.Is(err, stdcontext.Canceled) {
			terminal := &events.TaskCancelled{
				TaskID:    taskID,
				AgentName: child.Name(),
				Objective: objective,
				Reason:    err.Error(),
			}
			if serr := publishAfterTurn(terminal); serr != nil {
				slog.Error("agent: delegation TaskCancelled send failed", "agent", child.Name(), "err", serr)
			}
			return taskResult{
				taskID:      taskID,
				objective:   objective,
				cancelled:   true,
				childStream: childStream,
				err:         err,
			}, nil
		}
		terminal := &events.TaskFailed{
			TaskID:    taskID,
			AgentName: child.Name(),
			Objective: objective,
			Error:     err.Error(),
		}
		if serr := publishAfterTurn(terminal); serr != nil {
			slog.Error("agent: delegation TaskFailed send failed", "agent", child.Name(), "err", serr)
		}
		if hitl.IsError(err) {
			return taskResult{}, err
		}
		return taskResult{taskID: taskID, objective: objective, childStream: childStream, err: err}, nil
	}

	var err error
	run, err = child.newRunWith(childStream, context.DeriveChildVariables(parentCX.VariableStore()), prompt)
	if err != nil {
		return fail(err)
	}
	defer run.Close()
	run.delegation = forwarder
	run.delegationStart = startedEvent

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

	// AGO-DEVIATION: no stdlib context flows through tool execution, so the
	// delegation cannot be cancelled through the caller's tool call.
	if err := run.Start(ctx); err != nil {
		startedPublished = run.delegationStarted
		return fail(err)
	}
	scopeOpen, startedPublished = true, true
	reply, err := run.Result(ctx)
	if err != nil {
		return fail(err)
	}

	// The completed event carries the child stream's cumulative spend for
	// inspection; the additive accounting already travelled as source
	// UsageEvents, so a reader must never sum this snapshot.
	total := childStreamUsage(childStream, forwarder)
	terminal := &events.TaskCompleted{
		TaskID:     taskID,
		AgentName:  child.Name(),
		Objective:  objective,
		Result:     reply.Body(),
		TaskStream: childStream.ID(),
		Usage:      total,
	}
	if serr := publish(terminal); serr != nil {
		slog.Error("agent: delegation TaskCompleted send failed", "agent", child.Name(), "err", serr)
	}
	return taskResult{
		taskID:      taskID,
		objective:   objective,
		result:      reply.Body(),
		completed:   true,
		childStream: childStream,
	}, nil
}

// run_subtask tool -----------------------------------------------------------

func (r *AgentRun) newRunSubtaskTool(tc *TaskConfig) tools.Tool {
	type subtaskArgs struct {
		Task string `json:"task" agschema:"description=The objective for the subtask agent,required"`
	}
	fn := func(args subtaskArgs, cx *context.Context) (string, error) {
		// AGO-DEVIATION: no stdlib context flows through tool execution, so the
		// delegation cannot be cancelled through the caller's tool call.
		res, err := runTaskOn(stdcontext.Background(), cx, r.childAgent(tc, "subtask"), args.Task, "")
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
			sem     chan struct{}
		)
		// runOne renders one sub-task: failures become "Error: ..." text inside
		// the combined output (Python parity: `_spawn_subtask` returns
		// f"Error: {result.error}"), hitl failures end the whole call.
		runOne := func(i int, task string) error {
			// AGO-DEVIATION: no stdlib context flows through tool execution.
			res, err := runTaskOn(stdcontext.Background(), cx, r.childAgent(tc, fmt.Sprintf("subtask-%d", i)), task, "")
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
			sem = make(chan struct{}, maxConcurrency(tc.MaxConcurrency))
			for i, task := range args.Tasks {
				wg.Add(1)
				sem <- struct{}{}
				go func(i int, task string) {
					defer wg.Done()
					defer func() { <-sem }()
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
