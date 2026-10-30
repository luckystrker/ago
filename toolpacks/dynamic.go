package toolpacks

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/internal/uuid"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/response"
	"github.com/luckystrker/ago/spec"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
)

// dynamicBaseDescription is the tool description prefix (Python parity:
// dynamic/factory.py _BASE_DESCRIPTION).
const dynamicBaseDescription = "Create an ephemeral dynamic agent from a spec (name, system prompt, " +
	"subset of available tools, optional response schema) and immediately " +
	"run it on the given objective. Returns the agent's final reply as a " +
	"string. The spawned agent has a fresh conversation history and cannot " +
	"recursively spawn other dynamic agents. Call this when a focused " +
	"sub-task benefits from a tailored system prompt and a narrow tool set."

// DynamicAgentFactory is the create_and_run_agent tool (Python parity:
// ag2/tools/dynamic/factory.py dynamic_agent): the model builds an AgentSpec at
// runtime — name, system prompt, a tool subset picked from the pool by name, an
// optional response schema — and the tool spawns an ephemeral agent for it and
// runs one objective. The delegation machinery is Agent.AsTool: the child turn
// reports TaskStarted/TaskCompleted/TaskFailed and one rolled-up subtask
// UsageEvent on the caller's stream, starts from a copy of the caller's
// dependencies and variables, and bridges HITL to the caller's hook (see
// docs/DECISIONS.md for the wording deviations this composition carries).
//
// A DynamicAgentFactory is safe for concurrent use after construction; every
// call spawns its own child agent.
type DynamicAgentFactory struct {
	cfg  config.ModelConfig
	pool map[string]tools.Tool
	tool *tools.FunctionTool
}

var _ tools.Tool = (*DynamicAgentFactory)(nil)

// DynamicOption configures NewDynamicAgentFactory.
type DynamicOption func(*dynamicOptions) error

type dynamicOptions struct {
	name        string
	description string
}

// WithDynamicName overrides the tool name (default create_and_run_agent; Python
// parity: the tool's fixed name).
func WithDynamicName(name string) DynamicOption {
	return func(o *dynamicOptions) error {
		if name == "" {
			return errors.New("toolpacks: dynamic factory name must not be empty")
		}
		o.name = name
		return nil
	}
}

// WithDynamicDescription replaces the whole tool description, dropping the
// pool menu (Python parity: the description is fixed; this override exists for
// callers that need one).
func WithDynamicDescription(description string) DynamicOption {
	return func(o *dynamicOptions) error { o.description = description; return nil }
}

// NewDynamicAgentFactory builds the create_and_run_agent tool. cfg is the model
// config every spawned child runs on; pool is the tool set the spec may pick
// from by name. The pool's names and descriptions are appended to the tool
// description so the parent model learns them from the schema (Python parity:
// the "Available tools:" menu).
func NewDynamicAgentFactory(cfg config.ModelConfig, pool []tools.Tool, opts ...DynamicOption) (*DynamicAgentFactory, error) {
	if cfg == nil {
		return nil, errors.New("toolpacks: dynamic factory needs a model config")
	}
	o := dynamicOptions{name: "create_and_run_agent"}
	for _, opt := range opts {
		if err := opt(&o); err != nil {
			return nil, err
		}
	}
	f := &DynamicAgentFactory{cfg: cfg, pool: map[string]tools.Tool{}}
	for _, t := range pool {
		if t == nil {
			continue
		}
		f.pool[t.Name()] = t
	}

	description := o.description
	if description == "" {
		description = dynamicBaseDescription
		if len(f.pool) > 0 {
			var lines []string
			for name, t := range f.pool {
				line := "- " + name
				if desc := toolDescription(t); desc != "" {
					line += ": " + desc
				}
				lines = append(lines, line)
			}
			sort.Strings(lines)
			description += "\n\nAvailable tools:\n" + strings.Join(lines, "\n")
		}
	}

	type dynamicArgs struct {
		Spec      spec.AgentSpec `json:"spec" agschema:"description=The agent spec carrying name and prompt and tool_names plus an optional response_schema,required"`
		Objective string         `json:"objective" agschema:"description=The objective to run the dynamic agent on,required"`
	}
	fn := func(args dynamicArgs, cx *agocontext.Context) (string, error) {
		return f.run(args.Spec, args.Objective, cx)
	}
	ft, err := tools.NewFunctionTool(o.name, description, fn)
	if err != nil {
		return nil, fmt.Errorf("toolpacks: dynamic factory: %w", err)
	}
	f.tool = ft
	return f, nil
}

// run resolves the spec against the pool, spawns the child and runs the
// objective (Python parity: dynamic/handler.py resolve_and_run — a spec
// resolution failure is a recoverable "Error: …" result the parent model can
// retry with a corrected spec; a HITL failure ends the calling turn).
func (f *DynamicAgentFactory) run(spec spec.AgentSpec, objective string, cx *agocontext.Context) (string, error) {
	// Resolution is checked here (not via spec.ToAgent's error) so the text is
	// the Python handler's recoverable message with the missing/available lists.
	var missing []string
	for _, name := range spec.ToolNames {
		if _, ok := f.pool[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Sprintf("Error: unknown tools %s. Available: %s",
			pyList(missing), pyList(sortedPoolNames(f.pool))), nil
	}

	opts := []agent.Option{agent.WithConfig(f.cfg)}
	if spec.ResponseSchema != nil && len(spec.ResponseSchema.Schema) > 0 {
		// Python parity: spec.to_agent attaches the spec's response schema.
		opts = append(opts, agent.WithResponseSchema(response.RawSchema(
			spec.ResponseSchema.Schema,
			response.WithRawName(spec.ResponseSchema.Name),
			response.WithRawDescription(spec.ResponseSchema.Description),
		)))
	}
	child, err := spec.ToAgent(f.pool, opts...)
	if err != nil {
		return "Error: " + err.Error(), nil
	}

	// The delegation goes through Agent.AsTool so the child turn runs exactly
	// the shared sub-task machinery (lifecycle events, usage rollup, HITL
	// bridge, dependency copy). The delegation tool's stream name is made
	// unique per call: the framework routes calls to tools by name, so two
	// concurrent create_and_run_agent calls spawning same-named children must
	// not answer each other's calls.
	delegationName := uniqueToolName("task_" + sanitizeToolName(childName(spec)))
	delegation := child.AsTool(agent.WithToolName(delegationName))

	s := cx.Stream
	call := events.NewToolCallEvent(delegationName)
	args, err := json.Marshal(map[string]string{"objective": objective})
	if err != nil {
		return "", fmt.Errorf("toolpacks: dynamic factory: %w", err)
	}
	call.Arguments = string(args)

	settled := make(chan events.Event, 1)
	resSub := s.Subscribe(func(ev events.Event) error {
		select {
		case settled <- ev:
		default:
		}
		return nil
	}, stream.SubscribeOptions{Condition: delegationSettled(call.ID)})
	defer s.Unsubscribe(resSub)

	subs, err := delegation.Register(cx, nil)
	if err != nil {
		return "", fmt.Errorf("toolpacks: dynamic factory: register delegation: %w", err)
	}
	for _, id := range subs {
		defer s.Unsubscribe(id)
	}

	if err := s.Send(call); err != nil {
		// A hitl failure from the child must end the calling turn (Python
		// parity: run_task re-raises HumanInputError); dispatch is synchronous,
		// so the child has already settled by the time Send returns.
		return "", err
	}
	select {
	case ev := <-settled:
		switch res := events.AsValue(ev).(type) {
		case events.ToolResultEvent:
			return resultText(res.Result), nil
		case events.ToolErrorEvent:
			return "Error: " + res.Error, nil
		default:
			return "", fmt.Errorf("toolpacks: dynamic factory: unexpected delegation outcome %T", ev)
		}
	case <-streamClosed(s):
		return "", errors.New("toolpacks: dynamic factory: stream closed before the child answered")
	}
}

// --- helpers ------------------------------------------------------------------

// delegationSettled matches the events that can settle the synthetic delegation
// call (tools.Executor resultCondition parity).
func delegationSettled(callID string) events.Condition {
	return func(ev events.Event) bool {
		switch e := events.AsValue(ev).(type) {
		case events.ToolResultEvent:
			return e.ParentID == callID
		case events.ToolErrorEvent:
			return e.ParentID == callID
		}
		return false
	}
}

// childName picks the child agent's display name (Python parity: spec.name;
// an empty name stays empty — Agent allows it).
func childName(spec spec.AgentSpec) string { return spec.Name }

// uniqueToolName appends a short suffix so same-named siblings cannot collide
// on the stream's name routing.
func uniqueToolName(base string) string {
	return base + "_" + strings.ReplaceAll(uuid.New(), "-", "")[:8]
}

// sanitizeToolName keeps [A-Za-z0-9_] (agent.sanitizeToolName parity).
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

// resultText renders a tool result's text parts (Python parity: run_task
// result.result — the child reply body).
func resultText(r events.ToolResult) string {
	var parts []string
	for _, p := range r.Parts {
		if t, ok := events.AsValue(p).(events.TextInput); ok {
			parts = append(parts, t.Content)
		}
	}
	return strings.Join(parts, "\n")
}

// toolDescription reads the first schema's description for the pool menu.
func toolDescription(t tools.Tool) string {
	cx := agocontext.New(stream.NewMemoryStream())
	ss, err := t.Schemas(cx)
	if err != nil {
		return ""
	}
	for _, s := range ss {
		if sch, ok := s.(tools.Schema); ok {
			return strings.TrimSpace(sch.Description)
		}
	}
	return ""
}

// sortedPoolNames lists the pool keys sorted (Python parity: sorted(available)).
func sortedPoolNames(pool map[string]tools.Tool) []string {
	out := make([]string, 0, len(pool))
	for n := range pool {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// pyList renders a Python list literal (the dynamic handler's error text is
// model-facing; Python formats missing/available with sorted(...) list reprs).
func pyList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "'" + strings.ReplaceAll(n, "'", "\\'") + "'"
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// streamClosed reports the stream's shutdown, so the child wait cannot pin a
// tool after the conversation ended. A stream without a done signal yields nil
// (a nil channel never fires — the wait settles through the result only).
func streamClosed(s stream.Stream) <-chan struct{} {
	if c, ok := s.(interface{ Done() <-chan struct{} }); ok {
		return c.Done()
	}
	return nil
}

// Name returns the tool name.
func (f *DynamicAgentFactory) Name() string { return f.tool.Name() }

// DeclaredInCode reports a program-declared tool (framework contract).
func (f *DynamicAgentFactory) DeclaredInCode() bool { return f.tool.DeclaredInCode() }

// Schemas declares the create_and_run_agent function.
func (f *DynamicAgentFactory) Schemas(cx *agocontext.Context) ([]any, error) {
	return f.tool.Schemas(cx)
}

// Register subscribes the factory on the conversation stream.
func (f *DynamicAgentFactory) Register(cx *agocontext.Context, mw []middleware.Middleware) ([]stream.SubID, error) {
	return f.tool.Register(cx, mw)
}
