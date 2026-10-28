//go:build agowave2

// Package agent is the heart of AGo: the Agent turn loop (Python parity:
// ag2/agent.py — Agent, AgentRun, AgentReply, _prepare_turn, _drive_scope,
// _execute_turn). See docs/specs/core.md.
package agent

import (
	stdcontext "context"
	"errors"
	"fmt"
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
var ErrConfigNotProvided = errors.New("agent: no model config provided (pass WithConfig or a config dependency)")

// PromptHook produces dynamic system-prompt fragments per turn (Python parity:
// Agent.prompt() decorator on async hooks).
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

func WithConfig(cfg config.ModelConfig) Option        { return func(o *options) { o.cfg = cfg } }
func WithHitlHook(h hitl.HumanHook) Option            { return func(o *options) { o.hitl = h } }
func WithTools(ts ...tools.Tool) Option               { return func(o *options) { o.tools = append(o.tools, ts...) } }
func WithMiddleware(fs ...middleware.Factory) Option  { return func(o *options) { o.middleware = append(o.middleware, fs...) } }
func WithDependencies(deps map[string]any) Option     { return func(o *options) { o.dependencies = deps } }
func WithVariables(vars map[string]any) Option        { return func(o *options) { o.variables = vars } }
func WithResponseSchema(p response.Proto) Option      { return func(o *options) { o.schema = p } }
func WithStreamBase(s stream.Stream) Option           { return func(o *options) { o.streamBase = s } }
func WithPromptHook(h PromptHook) Option              { return func(o *options) { o.promptHooks = append(o.promptHooks, h) } }
func WithPromptFragments(fr ...string) Option         { return func(o *options) { o.fragments = append(o.fragments, fr...) } }
func WithTasks(cfg TaskConfig) Option                 { return func(o *options) { t := cfg; o.tasks = &t } }

// Agent is a conversable LLM agent. A bare New(name, prompt, WithConfig(...)) is a
// plain LLM loop; every harness feature is opt-in (Python parity: agent.py docstring).
type Agent struct {
	name  string
	mu    sync.Mutex
	opts  options
	locks sync.Map // stream id -> *sync.Mutex, serializes concurrent asks on one stream
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

func (a *Agent) Name() string { return a.name }

// Tools returns the agent's static tools (spec/serialization parity).
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

// Run prepares an observable turn (Python parity: agent.run). Start launches it,
// Result awaits the reply, Close ends the turn scope.
func (a *Agent) Run(_ stdcontext.Context, msgs ...Sendable) (*AgentRun, error) {
	s := a.opts.streamBase
	if s == nil {
		s = stream.NewMemoryStream()
	}
	run := &AgentRun{
		agent:  a,
		stream: s,
		cx:     context.NewWithDeps(s, map[string]any{}, map[string]any{}),
		done:   make(chan struct{}),
	}
	for k, v := range a.opts.dependencies {
		run.cx.Dependencies[k] = v
	}
	for k, v := range a.opts.variables {
		run.cx.Variables[k] = v
	}
	// Per-turn input goes through the inbox so mid-turn enqueues merge uniformly.
	if len(msgs) > 0 {
		req := events.EnsureRequest(msgs...)
		if err := s.Enqueue(req.Parts...); err != nil {
			return nil, fmt.Errorf("agent: enqueue input: %w", err)
		}
	}
	return run, nil
}

// Resume replays a trajectory: all but the last event seed the history, the last
// event is the re-entry trigger (Python parity: agent.resume). Returns the reply of
// the continued turn.
func (a *Agent) Resume(ctx stdcontext.Context, evs ...events.Event) (*AgentReply, error) {
	if len(evs) == 0 {
		return nil, errors.New("agent: resume needs at least one event")
	}
	s := stream.NewMemoryStream()
	if err := s.History().Replace(evs[:len(evs)-1]); err != nil {
		return nil, fmt.Errorf("agent: seed history: %w", err)
	}
	run, err := a.Run(ctx)
	if err != nil {
		return nil, err
	}
	run.stream = s
	run.cx.Stream = s
	defer run.Close()
	switch last := events.AsValue(evs[len(evs)-1]).(type) {
	case events.ModelRequest:
		if err := s.Enqueue(last.Parts...); err != nil {
			return nil, err
		}
	default:
		le := evs[len(evs)-1]
		if err := s.Send(le); err != nil {
			return nil, err
		}
		run.reentry = true
	}
	if err := run.Start(ctx); err != nil {
		return nil, err
	}
	return run.Result(ctx)
}

// AsTool exposes this agent as a tool for another agent (Python parity: agent.as_tool).
func (a *Agent) AsTool(opts ...AsToolOption) tools.Tool {
	var o asToolOptions
	for _, f := range opts {
		f(&o)
	}
	name := o.name
	if name == "" {
		name = "ask_" + sanitizeToolName(a.name)
	}
	desc := o.description
	if desc == "" {
		desc = "Delegate to the agent " + a.name + "."
	}
	type subArgs struct {
		Message string `json:"message" agschema:"description=The message to send to the agent,required"`
	}
	fn := func(args subArgs, ctx *context.Context) (string, error) {
		// The child shares the parent's stream state when asked on a continuation.
		reply, err := a.Ask(ctx.Stream.Context(), args.Message)
		if err != nil {
			return "", err
		}
		return reply.Body(), nil
	}
	_ = mw_unused
	ft, err := tools.NewFunctionTool(name, desc, fn)
	if err != nil {
		// A tool with one string argument cannot fail to bind; panic would be wrong
		// for library code, so degrade to a name-only tool is unnecessary — surface it.
		return failingTool{err: fmt.Errorf("agent: as_tool: %w", err)}
	}
	return ft
}

type mw_unused = struct{}

type failingTool struct{ err error }

func (f failingTool) Name() string          { return "broken_tool" }
func (f failingTool) DeclaredInCode() bool  { return true }
func (f failingTool) Schemas(_ *context.Context) ([]any, error) { return nil, f.err }
func (f failingTool) Register(_ *context.Context, _ []middleware.Middleware) error { return f.err }

func sanitizeToolName(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			out = append(out, c)
		} else {
			out = append(out, '_')
		}
	}
	return string(out)
}

// AsToolOption configures Agent.AsTool.
type AsToolOption func(*asToolOptions)

type asToolOptions struct {
	description string
	name        string
}

func WithToolDescription(d string) AsToolOption { return func(o *asToolOptions) { o.description = d } }
func WithToolName(n string) AsToolOption        { return func(o *asToolOptions) { o.name = n } }
