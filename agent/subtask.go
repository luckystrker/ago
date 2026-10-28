package agent

import (
	stdcontext "context"
	"errors"
	"fmt"
	"sync"

	"ago/config"
	"ago/context"
	"ago/events"
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
	// IncludeTools gives the child the parent's tool set (minus ExcludeTools).
	IncludeTools bool
	// ExcludeTools removes parent tools by name when IncludeTools is set.
	ExcludeTools []string
	// ExtraTools are added to the child in both modes.
	ExtraTools []tools.Tool
	// MaxConcurrency caps run_subtasks parallelism; 0 = unlimited.
	MaxConcurrency int
}

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

// childAgent builds one child agent for a subtask call.
func (r *AgentRun) childAgent(tc *TaskConfig, name string) (*Agent, error) {
	childCfg := tc.Config
	if childCfg == nil {
		childCfg = r.agent.opts.cfg
		if childCfg == nil {
			return nil, ErrConfigNotProvided
		}
	}
	prompt := tc.Prompt
	if prompt == "" {
		prompt = defaultTaskPrompt
	}
	var childTools []tools.Tool
	if tc.IncludeTools {
		a := r.agent
		a.mu.Lock()
		for _, t := range a.opts.tools {
			if !excluded(tc.ExcludeTools, t.Name()) {
				childTools = append(childTools, t)
			}
		}
		a.mu.Unlock()
	}
	childTools = append(childTools, tc.ExtraTools...)
	opts := []Option{WithConfig(childCfg), WithPromptFragments(prompt)}
	if len(childTools) > 0 {
		opts = append(opts, WithTools(childTools...))
	}
	return New(name, "", opts...), nil
}

func excluded(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// rollupUsage aggregates the child transcript into one subtask UsageEvent on the
// parent stream (Python parity: the rolled-up "subtask" UsageEvent; per-call
// events stay on the child stream — no double counting).
func (r *AgentRun) rollupUsage(label string, childStream stream.Stream) {
	history, err := childStream.History().GetEvents()
	if err != nil {
		return
	}
	var parts []events.UsageEvent
	for _, ev := range history {
		if ue, ok := events.AsValue(ev).(events.UsageEvent); ok && usage.Reported(ue.Usage) {
			parts = append(parts, ue)
		}
	}
	rep := usage.FromEvents(history)
	if !usage.Reported(rep.Total) {
		return
	}
	ue := events.UsageEvent{Usage: rep.Total, Kind: events.UsageKindSubtask, Label: label, Parts: parts}
	_ = r.cx.Send(&ue)
}

// run_subtask tool -----------------------------------------------------------

func (r *AgentRun) newRunSubtaskTool(tc *TaskConfig) tools.Tool {
	type subtaskArgs struct {
		Objective string `json:"objective" agschema:"description=The objective for the subtask agent,required"`
	}
	fn := func(args subtaskArgs, _ *context.Context) (string, error) {
		child, err := r.childAgent(tc, "subtask")
		if err != nil {
			return "", err
		}
		// AGO-DEVIATION: no stdlib context flows through tool execution, so a
		// subtask cannot be cancelled through the parent's tool call.
		reply, err := child.Ask(stdcontext.Background(), args.Objective)
		if err != nil {
			return "", err
		}
		r.rollupUsage("subtask", reply.cx.Stream)
		return reply.Body(), nil
	}
	t, err := tools.NewFunctionTool("run_subtask",
		"Run one subtask with a dedicated agent and return its final answer.", fn)
	if err != nil {
		return failingTool{err: err}
	}
	return t
}

// run_subtasks tool ----------------------------------------------------------

func (r *AgentRun) newRunSubtasksTool(tc *TaskConfig) tools.Tool {
	type subtasksArgs struct {
		Objectives []string `json:"objectives" agschema:"description=The objectives to run in parallel,required"`
	}
	fn := func(args subtasksArgs, _ *context.Context) (string, error) {
		if len(args.Objectives) == 0 {
			return "", errors.New("run_subtasks: no objectives given")
		}
		sem := make(chan struct{}, maxConcurrency(tc.MaxConcurrency))
		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			results = make([]string, len(args.Objectives))
			failure error
		)
		for i, objective := range args.Objectives {
			wg.Add(1)
			sem <- struct{}{}
			go func(i int, objective string) {
				defer wg.Done()
				defer func() { <-sem }()
				child, err := r.childAgent(tc, fmt.Sprintf("subtask-%d", i))
				if err != nil {
					mu.Lock()
					if failure == nil {
						failure = err
					}
					mu.Unlock()
					return
				}
				reply, err := child.Ask(stdcontext.Background(), objective)
				if err != nil {
					mu.Lock()
					if failure == nil {
						failure = err
					}
					mu.Unlock()
					return
				}
				r.rollupUsage(fmt.Sprintf("subtask-%d", i), reply.cx.Stream)
				mu.Lock()
				results[i] = reply.Body()
				mu.Unlock()
			}(i, objective)
		}
		wg.Wait()
		if failure != nil {
			return "", failure
		}
		out := ""
		for i, res := range results {
			if i > 0 {
				out += "\n\n"
			}
			out += fmt.Sprintf("[subtask %d]\n%s", i, res)
		}
		return out, nil
	}
	t, err := tools.NewFunctionTool("run_subtasks",
		"Run several subtasks in parallel with dedicated agents and return their answers.", fn)
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
