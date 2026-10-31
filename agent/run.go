package agent

import (
	stdcontext "context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/response"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
	"github.com/luckystrker/ago/usage"
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
	// pendingInput is the turn's own input, enqueued by Start once the
	// per-stream lock is held (audit M-25).
	pendingInput []events.Input

	reply *AgentReply
	err   error

	// scope state (set by Start, used by the turn goroutine)
	lock          *turnLock
	subs          []stream.SubID
	instances     []middleware.Middleware
	llm           config.LLMClient
	resolved      *tools.ResolvedTools
	schema        response.Proto
	turnCtx       stdcontext.Context
	cancel        stdcontext.CancelFunc
	wake          chan struct{}
	maxToolRounds int
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

	// The turn runs on a cancellable context so Close can stop an in-flight
	// turn (Python parity: AgentRun.__aexit__ cancels an unfinished task). The
	// same context is installed on the conversation context, so hooks that see
	// no stdlib parameter (middleware, tools asking a human) still follow turn
	// cancellation (audit M-26).
	r.turnCtx, r.cancel = stdcontext.WithCancel(ctx)
	r.cx.SetTurnContext(r.turnCtx)
	r.maxToolRounds = opts.maxToolRounds

	// Per-stream turn lock: concurrent asks on one stream serialize (Python parity:
	// the per-stream turn lock in _turn_scope).
	r.lock = a.acquireTurnLock(r.stream.ID())
	r.lock.mu.Lock()
	// A failed scope entry releases everything the scope took, so a forgotten
	// Close after a Start error cannot strand the stream for later asks
	// (Python parity: the _turn_scope async-with releases the lock when the
	// body fails).
	opened := false
	defer func() {
		if !opened {
			r.Close()
		}
	}()

	// The turn's input enters the inbox only now, under the lock: a sibling
	// delegation blocked on this stream must not have its objective drained by
	// the turn that owns the lock (audit M-25).
	if len(r.pendingInput) > 0 {
		if err := r.stream.Enqueue(r.pendingInput...); err != nil {
			return fmt.Errorf("agent: enqueue input: %w", err)
		}
		r.pendingInput = nil
	}

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

	// Knowledge scope entry: bootstrap the store once per agent before anything
	// else consumes it (Python parity: _drive_scope's outermost
	// `async with self._knowledge_context.enter(context)`).
	if err := a.enterKnowledge(); err != nil {
		return err
	}

	// System prompt: static fragments + dynamic hooks, seeded only when the
	// context carries no prompt yet — continuations clone the previous turn's
	// context and re-seeding would duplicate every fragment on the wire
	// (Python parity: _prepare_turn's `if not context.prompt`).
	if len(r.cx.PromptLines()) == 0 {
		r.cx.AddPrompt(opts.fragments...)
		for _, h := range opts.promptHooks {
			fr, err := h(r.cx)
			if err != nil {
				return fmt.Errorf("agent: prompt hook: %w", err)
			}
			r.cx.AddPrompt(fr...)
		}
	}

	// Tools: static + auto-injected subtask tools (Python parity: resolve_tools).
	allTools := append([]tools.Tool{}, opts.tools...)
	allTools = append(allTools, r.subtaskTools(opts)...)
	resolved, err := tools.ResolveTools(allTools, r.cx)
	if err != nil {
		return fmt.Errorf("agent: resolve tools: %w", err)
	}
	r.resolved = resolved
	r.schema = opts.schema

	// Middleware instances, one per turn (Python parity: MiddlewareFactory).
	r.instances = make([]middleware.Middleware, len(opts.middleware))
	for i, f := range opts.middleware {
		r.instances[i] = f()
	}

	// HITL: wrap the user hook with on_human_input middleware and install it on
	// the context. Context.Ask routes each question through the stream, passes
	// its per-request deadline context down to the user hook, and transcribes
	// the answer; the question itself is not transcribed (Python parity: the
	// wrapped hook is an interrupt subscriber whose wrapper returning None stops
	// dispatch before persistence). A hook that ignores its context outlives the
	// deadline — Go cannot cancel a goroutine, so Ask abandons it (AGO-DEVIATION:
	// Python cancels the in-flight hook; see docs/DECISIONS.md D-032).
	if opts.hitl != nil {
		userHook := opts.hitl
		r.cx.SetHitlHook(func(hctx stdcontext.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
			// The middleware chain carries no stdlib context, so the per-request
			// context is bound into the head of the chain here, per invocation.
			call := func(ev *events.HumanInputRequest, cx *context.Context) (*events.HumanMessage, error) {
				return userHook(hctx, ev)
			}
			for i := len(r.instances) - 1; i >= 0; i-- {
				mw, next := r.instances[i], call
				call = func(ev *events.HumanInputRequest, cx *context.Context) (*events.HumanMessage, error) {
					return mw.OnHumanInput(next, ev, cx)
				}
			}
			return call(req, r.cx)
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

	// 3. Tool executor on ToolCallsEvent + per-tool subscriptions. Every
	// subscription the executor and the tools created joins r.subs, so Close
	// tears the whole turn's wiring down; the per-stream turn lock serializes
	// turns, which makes per-turn registration idempotent (docs/DECISIONS.md
	// D-032; Python keeps the same lifetime with a per-turn ExitStack).
	exec := tools.NewExecutor()
	execSubs, err := exec.Register(r.cx, resolved.Tools, resolved.Known, r.instances)
	if err != nil {
		return fmt.Errorf("agent: register tool executor: %w", err)
	}
	r.subs = append(r.subs, execSubs...)

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

	opened = true
	go r.executeTurn()
	return nil
}

// Result waits for the turn to finish and returns the reply (idempotent; the same
// failure is re-raised, Python parity). Starts the turn when Start was not called.
// The wait honors ctx: a caller that gives up gets ctx.Err() instead of parking
// forever on a turn whose goroutine ignores cancellation (audit M-26).
func (r *AgentRun) Result(ctx stdcontext.Context) (*AgentReply, error) {
	if err := r.Start(ctx); err != nil {
		return nil, err
	}
	select {
	case <-r.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if r.err != nil {
		return nil, r.err
	}
	return r.reply, nil
}

// Enqueue adds input to the mid-turn inbox (Python parity: AgentRun.enqueue).
func (r *AgentRun) Enqueue(parts ...events.Input) error { return r.stream.Enqueue(parts...) }

// Stream exposes the conversation stream.
func (r *AgentRun) Stream() stream.Stream { return r.stream }

// Close ends the turn scope: cancels an in-flight turn, unsubscribes and
// releases the per-stream lock. Safe to call multiple times; safe before Start
// (no-op).
func (r *AgentRun) Close() {
	r.closeOnce.Do(func() {
		// Stop a turn still in flight before tearing the scope down: otherwise its
		// goroutine parks forever, Result never returns, and the lock is freed
		// under a live turn (Python parity: AgentRun.__aexit__ cancels an
		// unfinished task).
		if r.cancel != nil {
			r.cancel()
		}
		for _, id := range r.subs {
			r.stream.Unsubscribe(id)
		}
		if r.lock != nil {
			// Releases the mutex and the run's map slot; the entry survives
			// while other runs still serialize on this stream (audit L-19).
			r.agent.releaseTurnLock(r.stream.ID(), r.lock)
			r.lock = nil
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
