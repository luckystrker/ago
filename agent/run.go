package agent

import (
	stdcontext "context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"ago/config"
	"ago/context"
	"ago/events"
	"ago/middleware"
	"ago/response"
	"ago/stream"
	"ago/tools"
	"ago/usage"
)

// AgentRun is one observable turn (Python parity: agent.py AgentRun). Start opens
// the turn scope (per-stream lock, config resolution, middleware instantiation,
// stream subscriptions) and drives the turn on a goroutine; Result awaits the
// reply; Close ends the scope.
type AgentRun struct {
	agent  *Agent
	stream stream.Stream
	cx     *context.Context
	done   chan struct{}

	startOnce sync.Once
	closeOnce sync.Once
	startErr  error

	// reentry holds a non-ModelRequest resume trigger sent after subscriptions
	// are in place (Resume parity).
	reentry events.Event

	reply *AgentReply
	err   error

	// scope state (set by Start, used by the turn goroutine)
	lock      *sync.Mutex
	subs      []stream.SubID
	instances []middleware.Middleware
	llm       config.LLMClient
	resolved  *tools.ResolvedTools
	schema    response.Proto
	turnCtx   stdcontext.Context
	wake      chan struct{}
	// scanStart is the transcript length recorded before the resume trigger was
	// sent, so the turn loop starts scanning at the right position.
	scanStart  int
	haltMu     sync.Mutex
	halted     bool
	haltReason string
}

// Start opens the turn scope and launches the turn goroutine. Idempotent.
func (r *AgentRun) Start(ctx stdcontext.Context) error {
	r.startOnce.Do(func() {
		r.startErr = r.open(ctx)
	})
	return r.startErr
}

func (r *AgentRun) open(ctx stdcontext.Context) error {
	a := r.agent
	a.mu.Lock()
	opts := a.opts
	a.mu.Unlock()

	r.turnCtx = ctx

	// Per-stream turn lock: concurrent asks on one stream serialize (Python parity:
	// the per-stream turn lock in _turn_scope).
	r.lock = a.turnLock(r.stream.ID())
	r.lock.Lock()

	// Model config → client (Python parity: _prepare_turn).
	cfg := opts.cfg
	if cfg == nil {
		return ErrConfigNotProvided
	}
	client, err := cfg.Create()
	if err != nil {
		return fmt.Errorf("agent: create model client: %w", err)
	}
	r.llm = client
	r.cx.SetDependency("ago:config", cfg)

	// System prompt: static fragments + dynamic hooks (Python parity: _prepare_turn).
	r.cx.AddPrompt(opts.fragments...)
	for _, h := range opts.promptHooks {
		fr, err := h(r.cx)
		if err != nil {
			return fmt.Errorf("agent: prompt hook: %w", err)
		}
		r.cx.AddPrompt(fr...)
	}

	// Tools: static + auto-injected subtask tools (Python parity: resolve_tools).
	allTools := append([]tools.Tool{}, opts.tools...)
	allTools = append(allTools, r.subtaskTools(opts)...)
	resolved, err := tools.ResolveTools(allTools, r.cx)
	if err != nil {
		r.lock.Unlock()
		return fmt.Errorf("agent: resolve tools: %w", err)
	}
	r.resolved = resolved
	r.schema = opts.schema

	// Middleware instances, one per turn (Python parity: MiddlewareFactory).
	r.instances = make([]middleware.Middleware, len(opts.middleware))
	for i, f := range opts.middleware {
		r.instances[i] = f()
	}

	// HITL: wrap the user hook with on_human_input middleware; the question is
	// transcribed before the hook answers (Python parity: wrap_hitl + interrupt
	// subscriber; AGo invokes the hook directly — see D-013). The user hook
	// receives the turn's context; per-request cancellation is the hook's own
	// responsibility (AGO-DEVIATION: Go cannot interrupt a blocking hook).
	if opts.hitl != nil {
		userHook := opts.hitl
		base := func(ev *events.HumanInputRequest, cx *context.Context) (*events.HumanMessage, error) {
			if err := cx.Send(ev); err != nil {
				return nil, err
			}
			return userHook(ctx, ev)
		}
		call := base
		for i := len(r.instances) - 1; i >= 0; i-- {
			mw, next := r.instances[i], call
			call = func(ev *events.HumanInputRequest, cx *context.Context) (*events.HumanMessage, error) {
				return mw.OnHumanInput(next, ev, cx)
			}
		}
		hook := call
		r.cx.SetHitlHook(func(_ stdcontext.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
			return hook(req, r.cx)
		})
	}

	// Subscribers (Python parity: _drive_scope).
	// 1. LLM trigger on ModelRequest | ToolResultsEvent, skipping DrainedModelRequest.
	llmCond := events.AnyOf(
		events.TypeOf[events.ModelRequest](),
		events.TypeOf[events.DrainedModelRequest](),
		events.TypeOf[events.ToolResultsEvent](),
	)
	r.subs = append(r.subs, r.stream.Subscribe(func(ev events.Event) error {
		if _, drained := ev.(*events.DrainedModelRequest); drained {
			return nil
		}
		return r.llmTrigger(ev)
	}, stream.SubscribeOptions{Condition: llmCond}))

	// 2. Halt check: records the halt; the LLM trigger turns it into a synthetic
	// response (Python parity: _HaltCheckMiddleware + AlertPolicy's HaltEvent).
	r.subs = append(r.subs, r.stream.Subscribe(func(ev events.Event) error {
		if he, ok := ev.(*events.HaltEvent); ok {
			r.haltMu.Lock()
			r.halted, r.haltReason = true, he.Reason
			r.haltMu.Unlock()
		}
		return nil
	}, stream.SubscribeOptions{Interrupt: true, Condition: events.TypeOf[events.HaltEvent]()}))

	// 3. Tool executor on ToolCallsEvent + per-tool subscriptions.
	exec := tools.NewExecutor()
	if err := exec.Register(r.cx, resolved.Tools, resolved.Known, r.instances); err != nil {
		return fmt.Errorf("agent: register tool executor: %w", err)
	}

	// Re-entry trigger for Resume (after subscriptions are live). The scan
	// position is recorded before the send so the turn loop does not miss the
	// response the trigger produces.
	if r.reentry != nil {
		n, herr := r.historyLen()
		if herr != nil {
			return herr
		}
		r.scanStart = n
		if err := r.stream.Send(r.reentry); err != nil {
			return fmt.Errorf("agent: resume trigger: %w", err)
		}
	}

	go r.executeTurn()
	return nil
}

// Result waits for the turn to finish and returns the reply (idempotent; the same
// failure is re-raised, Python parity). Starts the turn when Start was not called.
func (r *AgentRun) Result(ctx stdcontext.Context) (*AgentReply, error) {
	if err := r.Start(ctx); err != nil {
		return nil, err
	}
	<-r.done
	if r.err != nil {
		return nil, r.err
	}
	return r.reply, nil
}

// Enqueue adds input to the mid-turn inbox (Python parity: AgentRun.enqueue).
func (r *AgentRun) Enqueue(parts ...events.Input) error { return r.stream.Enqueue(parts...) }

// Stream exposes the conversation stream.
func (r *AgentRun) Stream() stream.Stream { return r.stream }

// Close ends the turn scope: unsubscribes and releases the per-stream lock.
// Safe to call multiple times; safe before Start (no-op).
func (r *AgentRun) Close() {
	r.closeOnce.Do(func() {
		for _, id := range r.subs {
			r.stream.Unsubscribe(id)
		}
		if r.lock != nil {
			r.lock.Unlock()
		}
	})
}

// AgentReply is the result of one turn (Python parity: agent.py AgentReply).
type AgentReply struct {
	agent  *Agent
	resp   *events.ModelResponse
	cx     *context.Context
	schema response.Proto
}

// Body returns the text of the final answer ("" when the response carries none).
func (r *AgentReply) Body() string { return r.resp.Content() }

// Response exposes the final ModelResponse event.
func (r *AgentReply) Response() *events.ModelResponse { return r.resp }

// Files returns binary content produced by the model call.
func (r *AgentReply) Files() []events.BinaryResult { return r.resp.Files }

// History exposes the conversation transcript.
func (r *AgentReply) History() *stream.History { return r.cx.Stream.History() }

// Content returns the schema-validated raw JSON of the answer. Without a response
// schema it returns the body as a JSON string value.
func (r *AgentReply) Content() (json.RawMessage, error) {
	body := r.Body()
	if r.schema != nil {
		if _, err := r.schema.Validate(body, r.cx); err != nil {
			return nil, err
		}
		return json.RawMessage(body), nil
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// ContentAs validates the answer against the response schema and decodes it into T.
// On a validation failure the model is re-asked with the error appended, up to
// retries times (Python parity: AgentReply.content(retries=N)).
func ContentAs[T any](ctx stdcontext.Context, r *AgentReply, retries int) (T, error) {
	var out T
	if r.schema == nil {
		return out, errors.New("agent: ContentAs needs a response schema (WithResponseSchema)")
	}
	reply := r
	for attempt := 0; ; attempt++ {
		v, err := reply.schema.Validate(reply.Body(), reply.cx)
		if err == nil {
			b, merr := json.Marshal(v)
			if merr != nil {
				return out, fmt.Errorf("agent: encode validated response: %w", merr)
			}
			if uerr := json.Unmarshal(b, &out); uerr != nil {
				return out, fmt.Errorf("agent: decode response into %T: %w", out, uerr)
			}
			return out, nil
		}
		if attempt >= retries {
			return out, err
		}
		// Re-ask with the validation error appended (same stream/client).
		next, aerr := reply.continueAsk(ctx, "Your previous response failed validation: "+
			err.Error()+". Answer again, valid this time.")
		if aerr != nil {
			return out, aerr
		}
		reply = next
	}
}

// Usage reports the token accounting of the whole conversation so far.
func (r *AgentReply) Usage() *usage.Report {
	evs, err := r.cx.History()
	if err != nil {
		return &usage.Report{}
	}
	return usage.FromEvents(evs)
}

// Ask continues the conversation on the same stream and returns the next reply
// (Python parity: AgentReply.ask).
func (r *AgentReply) Ask(ctx stdcontext.Context, msgs ...Sendable) (*AgentReply, error) {
	return r.continueAsk(ctx, msgs...)
}

// Run continues the conversation on the same stream and returns the run handle
// (Python parity: AgentReply.run).
func (r *AgentReply) Run(ctx stdcontext.Context, msgs ...Sendable) (*AgentRun, error) {
	run, err := r.agent.newRunOn(r.cx, msgs...)
	if err != nil {
		return nil, err
	}
	if err := run.Start(ctx); err != nil {
		run.Close()
		return nil, err
	}
	return run, nil
}

// continueAsk opens the next turn on the same stream, cloning this turn's context
// so variables and dependencies carry over.
func (r *AgentReply) continueAsk(ctx stdcontext.Context, msgs ...Sendable) (*AgentReply, error) {
	run, err := r.agent.newRunOn(r.cx, msgs...)
	if err != nil {
		return nil, err
	}
	defer run.Close()
	if err := run.Start(ctx); err != nil {
		return nil, err
	}
	return run.Result(ctx)
}
