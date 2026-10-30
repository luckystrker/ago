package agent

import (
	"errors"
	"fmt"
	"strings"

	"github.com/luckystrker/ago/aggregate"
	"github.com/luckystrker/ago/compact"
	"github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/knowledge"
	"github.com/luckystrker/ago/tools"
)

// WithKnowledgeConfig attaches a knowledge store to the agent (Python parity:
// the knowledge=KnowledgeConfig(...) parameter). At construction the option:
//
//   - registers the store in the conversation dependencies under
//     knowledge.DepKey, so DI consumers can resolve it like Python's
//     dependencies[KnowledgeStore];
//   - adds the auto-injected "knowledge" tool when cfg.ExposeTool is on
//     (Python parity: _make_knowledge_tool joining _additional_tools);
//   - arms the end-of-turn event log when cfg.WriteEventLog is on and the
//     once-per-store bootstrap (sentinel knowledge.SentinelPath), both of which
//     run at turn scope (Python parity: _KnowledgeContext.enter).
//
// The cfg.Compact/Aggregate middleware wiring happens in applyKnowledgeMiddleware
// after every option has applied — the option funcs see no agent name.
//
// A Config with a nil Store contributes nothing (WithPlugin parity for a nil
// plugin). A zero Config has both flags off — knowledge.NewConfig applies the
// Python defaults.
func WithKnowledgeConfig(cfg knowledge.Config) Option {
	return func(o *options) {
		if cfg.Store == nil {
			return
		}
		c := cfg
		o.knowledge = &c
		if o.dependencies == nil {
			o.dependencies = map[string]any{}
		}
		o.dependencies[knowledge.DepKey] = cfg.Store
		if cfg.ExposeTool {
			t, err := newKnowledgeTool(cfg.Store)
			if err != nil {
				// Unreachable for the static argument struct; surfaced through
				// the tool contract instead of panicking in an option.
				o.tools = append(o.tools, failingTool{err: fmt.Errorf("agent: knowledge tool: %w", err)})
				return
			}
			o.tools = append(o.tools, t)
		}
	}
}

// applyKnowledgeMiddleware appends the compaction and aggregation middlewares
// configured on the knowledge Config (Python parity: the agent constructor's
// add_middleware calls, agent.py's knowledge block). It runs after every option
// has applied, for two reasons: the option funcs see no agent name, and the
// post-pass pins the position Python gives these middlewares — appended after
// the user middleware list, compaction before aggregation.
//
//   - compaction wires when a strategy is set (a nil trigger leaves the
//     middleware a pass-through no-op; Python crashes in that shape);
//   - aggregation wires when both strategy and trigger are set (Python checks
//     the trigger's numeric conditions and fails a nil strategy at fire time;
//     a Go Trigger carries no inspectable conditions).
func (o *options) applyKnowledgeMiddleware(name string) {
	cfg := o.knowledge
	if cfg == nil || cfg.Store == nil {
		return
	}
	if cfg.Compact != nil {
		o.middleware = append(o.middleware, compact.New(cfg.CompactTrigger, cfg.Compact, compact.WithAgent(name)))
	}
	if cfg.Aggregate != nil && cfg.AggregateTrigger != nil {
		o.middleware = append(o.middleware, aggregate.New(cfg.AggregateTrigger, cfg.Aggregate))
	}
}

// knowledgeArgs are the model-facing arguments of the knowledge tool (Python
// parity: knowledge(action, path="/", content="")). Descriptions avoid commas —
// the agschema tag splits on them.
type knowledgeArgs struct {
	Action  string `json:"action" agschema:"description=The action to perform: read | write | list | delete,required"`
	Path    string `json:"path" agschema:"description=Store path to operate on"`
	Content string `json:"content" agschema:"description=Content for the write action"`
}

// newKnowledgeTool builds the "knowledge" tool bound to store (Python parity:
// _make_knowledge_tool — built once at agent construction so the model-visible
// definition is stable). Action semantics and result strings follow the Python
// tool verbatim.
func newKnowledgeTool(store knowledge.Store) (tools.Tool, error) {
	fn := func(args knowledgeArgs, _ *context.Context) (string, error) {
		path := args.Path
		if path == "" {
			path = "/"
		}
		switch args.Action {
		case "read":
			data, err := store.Read(path)
			if err != nil {
				if errors.Is(err, knowledge.ErrNotFound) {
					return fmt.Sprintf("Not found: %s", path), nil
				}
				return "", err
			}
			return string(data), nil

		case "write":
			if args.Content == "" {
				return "Error: content is required for write action.", nil
			}
			if err := store.Write(path, []byte(args.Content)); err != nil {
				return "", err
			}
			return fmt.Sprintf("Written to %s", path), nil

		case "list":
			entries, err := store.List(path)
			if err != nil {
				return "", err
			}
			if len(entries) == 0 {
				return fmt.Sprintf("Empty: %s", path), nil
			}
			listing := ""
			// A directory's SKILL.md heads the listing (Python parity:
			// f"{path.rstrip('/')}/SKILL.md").
			if data, rerr := store.Read(strings.TrimSuffix(path, "/") + "/SKILL.md"); rerr == nil && len(data) > 0 {
				listing = string(data) + "\n---\n"
			}
			for i, e := range entries {
				if i > 0 {
					listing += "\n"
				}
				listing += e
			}
			return listing, nil

		case "delete":
			if err := store.Delete(path); err != nil {
				return "", err
			}
			return fmt.Sprintf("Deleted: %s", path), nil

		default:
			return fmt.Sprintf("Unknown action: %s. Available: read, write, list, delete.", args.Action), nil
		}
	}
	return tools.NewFunctionTool("knowledge", "Manage your knowledge store.\n\n"+
		"Actions:\n"+
		"    read   - Read file at path.\n"+
		"    write  - Write content to path.\n"+
		"    list   - List entries at path.\n"+
		"    delete - Delete file at path.", fn)
}

// enterKnowledge runs the knowledge turn-scope entry (Python parity:
// _KnowledgeContext.enter, called at _drive_scope): it bootstraps the store
// once per agent, guarded by the in-memory flag and the store sentinel.
func (a *Agent) enterKnowledge() error {
	a.mu.Lock()
	cfg := a.opts.knowledge
	a.mu.Unlock()
	if cfg == nil || cfg.Store == nil {
		return nil
	}

	a.knowledgeMu.Lock()
	defer a.knowledgeMu.Unlock()
	if a.knowledgeBootstrapped {
		return nil
	}
	b := cfg.Bootstrap
	if b == nil {
		b = knowledge.DefaultBootstrap{MentionTool: cfg.ExposeTool}
	}
	if err := knowledge.BootstrapOnce(cfg.Store, a.name, b); err != nil {
		return fmt.Errorf("agent: knowledge bootstrap: %w", err)
	}
	a.knowledgeBootstrapped = true
	return nil
}

// persistEventLog writes the stream history to the store's log directory at
// the end of a turn (Python parity: the post-yield half of
// _KnowledgeContext.enter). A failure emits EventLogFailed on the stream and is
// otherwise swallowed — the reply is still delivered (EventLogFailed contract).
func (r *AgentRun) persistEventLog() {
	r.agent.mu.Lock()
	cfg := r.agent.opts.knowledge
	r.agent.mu.Unlock()
	if cfg == nil || !cfg.WriteEventLog || cfg.Store == nil {
		return
	}
	evs, err := r.cx.History()
	if err == nil {
		err = knowledge.NewEventLogWriter(cfg.Store).Persist(r.stream.ID(), evs)
	}
	if err != nil {
		_ = r.cx.Send(&events.EventLogFailed{
			Agent:     r.agent.name,
			ErrorType: fmt.Sprintf("%T", err),
			Error:     err.Error(),
		})
	}
}
