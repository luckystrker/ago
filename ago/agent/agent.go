// Package agent is the heart of AGo: the Agent turn loop (Python parity:
// ag2/agent.py — Agent, AgentRun, AgentReply, _prepare_turn, _drive_scope,
// _execute_turn). See docs/specs/core.md §1–3 for the behavioral reference.
package agent

import (
	stdcontext "context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"ago/config"
	"ago/context"
	"ago/events"
	"ago/hitl"
	"ago/middleware"
	"ago/response"
	"ago/stream"
	"ago/tools"
)

// ErrConfigNotProvided matches Python ConfigNotProvidedError: Ask/Run without a model config.
var ErrConfigNotProvided = errors.New("agent: no model config provided (pass WithConfig)")

// PromptHook produces dynamic system-prompt fragments per turn (Python parity:
// the Agent.prompt() decorator on async hooks).
type PromptHook func(ctx *context.Context) ([]string, error)

// Sendable is a raw user message: string, events.Input, or any JSON-decodable value.
type Sendable = any

type options struct {
	cfg          config.ModelConfig
	hitl         hitl.HumanHook
	tools        []tools.Tool
	middleware   []middleware.Factory
	dependencies map[string]any
	variables    map[string]any
	schema       response.Proto
	streamBase   stream.Stream
	promptHooks  []PromptHook
	fragments    []string
	tasks        *TaskConfig
}

// Option configures an Agent at construction.
type Option func(*options)

func WithConfig(cfg config.ModelConfig) Option { return func(o *options) { o.cfg = cfg } }
func WithHitlHook(h hitl.HumanHook) Option     { return func(o *options) { o.hitl = h } }
func WithTools(ts ...tools.Tool) Option        { return func(o *options) { o.tools = append(o.tools, ts...) } }
func WithMiddleware(fs ...middleware.Factory) Option {
	return func(o *options) { o.middleware = append(o.middleware, fs...) }
}
func WithDependencies(deps map[string]any) Option { return func(o *options) { o.dependencies = deps } }
func WithVariables(vars map[string]any) Option    { return func(o *options) { o.variables = vars } }
func WithResponseSchema(p response.Proto) Option  { return func(o *options) { o.schema = p } }
func WithStreamBase(s stream.Stream) Option       { return func(o *options) { o.streamBase = s } }
func WithPromptHook(h PromptHook) Option {
	return func(o *options) { o.promptHooks = append(o.promptHooks, h) }
}
func WithPromptFragments(fr ...string) Option {
	return func(o *options) { o.fragments = append(o.fragments, fr...) }
}
func WithTasks(cfg TaskConfig) Option { return func(o *options) { t := cfg; o.tasks = &t } }

// Agent is a conversable LLM agent. A bare New(name, prompt, WithConfig(...)) is a
// plain LLM loop; every harness feature is opt-in (Python parity: agent.py docstring).
type Agent struct {
	name  string
	mu    sync.Mutex
	opts  options
	locks sync.Map // stream id -> *sync.Mutex: serializes concurrent asks on one stream
}

// New creates an agent. prompt may be empty when fragments/hooks supply the prompt.
func New(name string, prompt string, opts ...Option) *Agent {
	a := &Agent{name: name}
	a.opts.fragments = append(a.opts.fragments, prompt)
	for _, o := range opts {
		o(&a.opts)
	}
	return a
}

// Name returns the agent name.
func (a *Agent) Name() string { return a.name }

// Tools returns a copy of the agent's static tools (spec parity: AgentSpec.from_agent).
func (a *Agent) Tools() []tools.Tool {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]tools.Tool, len(a.opts.tools))
	copy(out, a.opts.tools)
	return out
}

// PromptFragments returns the static prompt fragments (spec parity).
func (a *Agent) PromptFragments() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.opts.fragments))
	copy(out, a.opts.fragments)
	return out
}

// turnLock returns the per-stream mutex serializing concurrent asks.
func (a *Agent) turnLock(streamID string) *sync.Mutex {
	l, _ := a.locks.LoadOrStore(streamID, &sync.Mutex{})
	return l.(*sync.Mutex)
}

// Ask runs one blocking turn and returns the reply (Python parity: agent.ask).
func (a *Agent) Ask(ctx stdcontext.Context, msgs ...Sendable) (*AgentReply, error) {
	run, err := a.Run(ctx, msgs...)
	if err != nil {
		return nil, err
	}
	defer run.Close()
	if err := run.Start(ctx); err != nil {
		return nil, err
	}
	return run.Result(ctx)
}

// Run prepares an observable turn (Python parity: agent.run): Start launches the
// turn, Result awaits the reply, Close ends the turn scope.
func (a *Agent) Run(_ stdcontext.Context, msgs ...Sendable) (*AgentRun, error) {
	s := a.opts.streamBase
	if s == nil {
		s = stream.NewMemoryStream()
	}
	return a.newRun(s, msgs...)
}

// newRun builds a turn over an explicit stream. Continuations (AgentReply.ask)
// reuse the reply's stream; fresh asks use the stream base or a new stream.
func (a *Agent) newRun(s stream.Stream, msgs ...Sendable) (*AgentRun, error) {
	run := &AgentRun{
		agent:  a,
		stream: s,
		cx:     context.NewWithDeps(s, map[string]any{}, map[string]any{}),
		done:   make(chan struct{}),
		wake:   make(chan struct{}, 1),
	}
	a.mu.Lock()
	for k, v := range a.opts.dependencies {
		run.cx.Dependencies[k] = v
	}
	for k, v := range a.opts.variables {
		run.cx.Variables[k] = v
	}
	a.mu.Unlock()
	// Per-turn input goes through the inbox so mid-turn enqueues merge uniformly.
	if len(msgs) > 0 {
		req := events.EnsureRequest(msgs...)
		if err := s.Enqueue(req.Parts...); err != nil {
			return nil, fmt.Errorf("agent: enqueue input: %w", err)
		}
	}
	return run, nil
}

// newRunOn continues a conversation on an existing stream, cloning the previous
// turn's context so variables and dependencies carry over (Python parity:
// AgentReply.ask / AgentReply.run).
func (a *Agent) newRunOn(prev *context.Context, msgs ...Sendable) (*AgentRun, error) {
	run := &AgentRun{
		agent:  a,
		stream: prev.Stream,
		cx:     prev.Clone(),
		done:   make(chan struct{}),
		wake:   make(chan struct{}, 1),
	}
	if len(msgs) > 0 {
		req := events.EnsureRequest(msgs...)
		if err := run.stream.Enqueue(req.Parts...); err != nil {
			return nil, fmt.Errorf("agent: enqueue input: %w", err)
		}
	}
	return run, nil
}

// Resume replays a trajectory: all but the last event seed the history, the last
// event is the re-entry trigger (Python parity: agent.resume). A ModelRequest
// trigger re-enters through the inbox; any other event (e.g. ToolResultsEvent) is
// sent directly and the turn continues from the model's next response.
func (a *Agent) Resume(ctx stdcontext.Context, evs ...events.Event) (*AgentReply, error) {
	if len(evs) == 0 {
		return nil, errors.New("agent: resume needs at least one event")
	}
	s := stream.NewMemoryStream()
	if err := s.History().Replace(evs[:len(evs)-1]); err != nil {
		return nil, fmt.Errorf("agent: seed history: %w", err)
	}
	run, err := a.newRun(s)
	if err != nil {
		return nil, err
	}
	defer run.Close()
	switch last := events.AsValue(evs[len(evs)-1]).(type) {
	case events.ModelRequest:
		if err := s.Enqueue(last.Parts...); err != nil {
			return nil, err
		}
	default:
		run.reentry = evs[len(evs)-1]
	}
	if err := run.Start(ctx); err != nil {
		return nil, err
	}
	return run.Result(ctx)
}

// AsTool exposes this agent as a delegation tool for another agent (Python
// parity: agent.as_tool / subagent_tool.subagent_tool). Each call runs this
// agent as a sub-task through the shared delegation machinery (runTaskOn):
// TaskStarted/TaskCompleted/TaskFailed and one rolled-up subtask UsageEvent on
// the caller's stream, a copy of the caller's dependencies and variables, and
// the HITL bridge when this agent has no hook of its own. The agent's own
// stream base (WithStreamBase) stays the delegation's stream — every
// delegation opens a fresh stream otherwise.
func (a *Agent) AsTool(opts ...AsToolOption) tools.Tool {
	var o asToolOptions
	for _, f := range opts {
		f(&o)
	}
	name := o.name
	if name == "" {
		// Python parity: subagent_tool.py `name or f"task_{agent.name}"`.
		name = "task_" + sanitizeToolName(a.name)
	}
	desc := o.description
	if desc == "" {
		desc = "Delegate to the agent " + a.name + "."
	}
	type subArgs struct {
		Objective string `json:"objective" agschema:"description=The objective for the subtask agent,required"`
		Context   string `json:"context" agschema:"description=Optional additional context for the subtask agent"`
	}
	fn := func(args subArgs, cx *context.Context) (string, error) {
		// AGO-DEVIATION: no stdlib context flows through tool execution, so the
		// delegation cannot be cancelled through the caller's tool call.
		res, err := runTaskOn(stdcontext.Background(), cx, a, args.Objective, args.Context)
		if err != nil {
			// hitl: not a tool result — it ends the calling turn (Python parity:
			// run_task re-raises HumanInputError past the delegating tool).
			return "", err
		}
		if !res.completed {
			// Python parity: subagent_tool.py
			// f"Sub-task '{agent.name}' failed: {result.error}".
			return fmt.Sprintf("Sub-task '%s' failed: %s", a.name, res.err.Error()), nil
		}
		return res.result, nil
	}
	ft, err := tools.NewFunctionTool(name, desc, fn)
	if err != nil {
		return failingTool{err: fmt.Errorf("agent: as_tool: %w", err)}
	}
	return ft
}

type failingTool struct{ err error }

func (f failingTool) Name() string                              { return "broken_tool" }
func (f failingTool) DeclaredInCode() bool                      { return true }
func (f failingTool) Schemas(_ *context.Context) ([]any, error) { return nil, f.err }
func (f failingTool) Register(_ *context.Context, _ []middleware.Middleware) ([]stream.SubID, error) {
	return nil, f.err
}

func sanitizeToolName(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// AsToolOption configures Agent.AsTool.
type AsToolOption func(*asToolOptions)

type asToolOptions struct {
	description string
	name        string
}

// WithToolDescription overrides the delegation tool's description.
func WithToolDescription(d string) AsToolOption { return func(o *asToolOptions) { o.description = d } }

// WithToolName overrides the delegation tool's name.
func WithToolName(n string) AsToolOption { return func(o *asToolOptions) { o.name = n } }
