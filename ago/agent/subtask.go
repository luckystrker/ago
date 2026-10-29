package agent

import (
	stdcontext "context"
	"fmt"
	"strings"
	"sync"

	"ago/config"
	"ago/context"
	"ago/events"
	"ago/hitl"
	"ago/internal/uuid"
	"ago/stream"
	"ago/tools"
	"ago/usage"
)

// TaskConfig configures the auto-injected subtask tools (Python parity:
// agent.py TaskConfig). WithTasks(opts) gives the agent two tools — run_subtask
// and run_subtasks — that spawn bare child Agents; the subtask tools never reach
// the spawned children, so delegation cannot recurse by construction.
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
// grants under (middleware/builtin/tools/approval.py BYPASS_KEY). Tool approvals
// are not inherited by a sub-task: exactly this key is dropped from the copied
// variables so the child asks again (Python parity: run_task.py
// `if k != BYPASS_KEY` — filter by key, not by prefix).
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
	childStream stream.Stream
	err         error
}

// usagePair is one (Provider, Model) attribution candidate behind a spend.
type usagePair struct{ provider, model string }

// rollupUsage aggregates the child transcript into one subtask UsageEvent on
// the parent stream (Python parity: run_task._emit_rollup — the child's per-call
// events stay on the child stream, so the additive report sees the delegation
// once). Only events appended at index start and later count toward the rollup:
// Python scopes the rollup with a usage subscription registered per call, so a
// stream reused across sequential delegations (the persistent_stream behavior
// of AsTool) reports only this invocation's spend, never an earlier one's.
// Attribution is stamped only when exactly one (Provider, Model) pair is
// behind the spend, and only events with reported tokens vote (Python parity:
// _sole_pair — a nested delegation's unlabelled rollup counts as a pair and
// correctly leaves the parent unlabelled). A nested rollup is flattened into
// Parts in place of itself (Python parity: `event.parts or [event]`). The
// rollup's total_tokens survives only when every counted call reported one
// (Python parity: _rollup_usage); the returned Usage is the stream's cumulative
// total (Python parity: reply.usage().total — the running total on a reused
// stream) and the bool reports whether this invocation spent anything.
func rollupUsage(parentCX *context.Context, label string, childStream stream.Stream, start int) (events.Usage, bool) {
	history, err := childStream.History().GetEvents()
	if err != nil {
		return events.Usage{}, false
	}
	var (
		pairs map[usagePair]struct{}
		parts []events.UsageEvent
		spent events.Usage
		total events.Usage
		// totalsReported stays true while every counted call carried
		// total_tokens (Python parity: _rollup_usage).
		totalsReported = true
	)
	for i, ev := range history {
		ue, ok := events.AsValue(ev).(events.UsageEvent)
		if !ok || !usage.Reported(ue.Usage) {
			continue
		}
		total = total.Add(ue.Usage)
		if i < start {
			continue // an earlier delegation's spend: its own rollup already reported it
		}
		if pairs == nil {
			pairs = make(map[usagePair]struct{})
		}
		pairs[usagePair{ue.Provider, ue.Model}] = struct{}{}
		if len(ue.Parts) > 0 {
			parts = append(parts, ue.Parts...)
		} else {
			parts = append(parts, ue)
		}
		if ue.Usage.TotalTokens == nil {
			totalsReported = false
		}
		spent = spent.Add(ue.Usage)
	}
	if !usage.Reported(spent) {
		return total, false
	}
	out := &events.UsageEvent{Usage: spent, Kind: events.UsageKindSubtask, Label: label, Parts: parts}
	// total_tokens survives only when every counted call reported one: a partial
	// sum falls below the counts it is meant to cover (Python parity:
	// _rollup_usage). The returned Usage keeps the plain sum — TaskCompleted
	// carries it, like AgentReply.usage in Python.
	if !totalsReported {
		out.Usage.TotalTokens = nil
	}
	if len(pairs) == 1 {
		for p := range pairs {
			out.Provider, out.Model = p.provider, p.model
		}
	}
	_ = parentCX.Send(out)
	return total, true
}

// runTaskOn runs child as a sub-task of the conversation parentCX and reports
// the delegation on the parent stream (Python parity:
// tools/subagents/run_task.py run_task): TaskStarted before the child turn, one
// rolled-up subtask UsageEvent and TaskCompleted/TaskFailed after. The child
// transcript persists into the parent's storage under the child's own stream id;
// the child run starts from a copy of the parent's dependencies and variables
// (minus the approval bypass key — tool approvals are not inherited) and its
// variable mutations stay scoped to the child run. When the child has no HITL
// hook of its own and the parent has one, questions asked inside the child are
// bridged: a copy stamped with the task id goes to the parent stream and the
// parent's hook answers.
//
// A hitl failure comes back as the error result — it must end the calling turn
// (Python parity: run_task re-raises HumanInputError). Any other failure is
// reported in the returned taskResult with completed=false and becomes the
// delegating tool's "Error: ..." output; TaskFailed and the rollup are emitted
// on both paths.
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
	// The rollup counts only what this invocation appends: a stream base reused
	// across sequential delegations already carries the earlier ones' spend, and
	// Python scopes its per-call usage subscription the same way (run_task
	// registers it after TaskStarted and removes it in the same finally).
	invocationStart := 0
	if evs, err := childStream.History().GetEvents(); err == nil {
		invocationStart = len(evs)
	}

	if err := parentCX.Send(&events.TaskStarted{
		TaskID:           taskID,
		AgentName:        child.Name(),
		Objective:        objective,
		ParentToolCallID: parentCX.ToolCallID,
	}); err != nil {
		return taskResult{}, err
	}

	// fail reports a failed delegation the way Python's except branch does:
	// rollup (nothing spent → skipped) and TaskFailed on every path, hitl
	// failures re-raised so they end the calling turn.
	fail := func(err error) (taskResult, error) {
		rollupUsage(parentCX, child.Name(), childStream, invocationStart)
		_ = parentCX.Send(&events.TaskFailed{
			TaskID:    taskID,
			AgentName: child.Name(),
			Objective: objective,
			Error:     err.Error(),
		})
		if hitl.IsError(err) {
			return taskResult{}, err
		}
		return taskResult{taskID: taskID, objective: objective, childStream: childStream, err: err}, nil
	}

	run, err := child.newRun(childStream, prompt)
	if err != nil {
		return fail(err)
	}
	defer run.Close()

	// The child run starts from the parent's state merged over the child agent's
	// own option maps (newRun already seeded those): Python passes
	// dependencies=parent_context.dependencies.copy() / variables minus the
	// bypass key into agent.ask, and __build_context unions them as
	// self._agent_dependencies | overrides — the delegated agent's own
	// WithDependencies/WithVariables survive, the parent's snapshot wins only on
	// shared keys. Replacing the maps instead would strip the child of its own
	// injected state, which its tools read at call time. The bypass key is
	// dropped from the parent's copy, so approvals are not inherited and
	// concurrent sibling tasks cannot clobber each other.
	for k, v := range parentCX.DependenciesSnapshot() {
		run.cx.Dependencies[k] = v
	}
	vars := parentCX.VariablesSnapshot()
	delete(vars, bypassKey)
	for k, v := range vars {
		run.cx.Variables[k] = v
	}

	// HITL bridge: a child without its own hook asks through the parent's
	// (Python parity: run_task installs the bridge when the agent has no
	// _hitl_hook). The parent hook is the middleware-wrapped one installed on
	// the parent's run context, so the answer follows the asking context and
	// lands on the child stream as the reply to the child's request.
	child.mu.Lock()
	childHook := child.opts.hitl
	child.mu.Unlock()
	if childHook == nil {
		if parentHook := parentCX.Hook(); parentHook != nil {
			run.cx.SetHitlHook(func(hctx stdcontext.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
				// Observers on the parent stream see this delegation asking; the
				// answer is matched on the original request id, so the copy keeps
				// it and a nested delegation's stamp is replaced.
				stamped := *req
				stamped.TaskID = taskID
				_ = parentCX.Stream.Send(&stamped)
				return parentHook(hctx, req)
			})
		}
		// AGO-DEVIATION: the forwarded question is transcribed on the parent
		// stream — Python swallows it with a per-turn interrupter, which AGo
		// subscribers (event only, no context) cannot replicate; the per-turn
		// wiring is why the bridge calls the hook directly (docs/DECISIONS.md
		// D-013, D-040). Without a hook on either side the child's Context.Ask
		// refuses with hitl.NotProvidedError — the default_hitl_hook refusal,
		// carried through the tool layer as a turn-ending hitl error.
	}

	// AGO-DEVIATION: no stdlib context flows through tool execution, so the
	// delegation cannot be cancelled through the caller's tool call.
	if err := run.Start(ctx); err != nil {
		return fail(err)
	}
	reply, err := run.Result(ctx)
	if err != nil {
		return fail(err)
	}

	total, _ := rollupUsage(parentCX, child.Name(), childStream, invocationStart)
	_ = parentCX.Send(&events.TaskCompleted{
		TaskID:     taskID,
		AgentName:  child.Name(),
		Objective:  objective,
		Result:     reply.Body(),
		TaskStream: childStream.ID(),
		Usage:      total,
	})
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
