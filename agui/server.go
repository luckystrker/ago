package agui

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/hitl"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
)

// Server serves one agent over AG-UI with the held-turn machinery: an agent
// that asks a human a question pauses its run on the wire — a RUN_FINISHED
// carrying an interrupt outcome, never an error — and the answer arrives as the
// resume entries of a later POST on the same thread. A held turn lives in this
// process only: resumes must be routed back to it, and it does not survive a
// restart (Python parity: AGUIStream over interrupts.py's serve_exchange).
//
// Build the served agent with Options, so its questions reach the client:
//
//	srv, err := agui.NewServer()
//	a := agent.New("bot", "", cfg, srv.Options()...)
//	h := srv.Handler(a)
//
// An agent served without Options keeps its own arrangements: a question with
// no hook to answer it fails the run.
type Server struct {
	cfg   serverConfig
	turns *servedTurns
	// fallbackHook is installed as the agent's human-input hook: Context.Ask
	// refuses a question with no hook before any subscriber sees it, so the
	// middleware below needs one behind it. On a served run the middleware
	// intercepts first; anywhere else the refusal is the honest answer.
	fallbackHook hitl.HumanHook
}

type serverConfig struct {
	retention      Retention
	requireProof   bool
	stateSnapshots bool
	guard          *Guard
	now            func() time.Time
}

// ServerOption configures a Server at construction.
type ServerOption func(*serverConfig)

// WithRetention sets how long an unanswered question is kept and how many are
// kept at once.
func WithRetention(r Retention) ServerOption { return func(c *serverConfig) { c.retention = r } }

// WithResumeProof refuses any resume that carries no proof: every interrupt is
// issued with a proof in its metadata, and a resume that carries one must carry
// the one issued. Off by default, since a standard client copies nothing from
// an interrupt's metadata (Python parity: require_resume_proof).
func WithResumeProof() ServerOption { return func(c *serverConfig) { c.requireProof = true } }

// WithStateSnapshots emits a STATE_SNAPSHOT of the conversation's variables at
// the start of every run (Python parity: AGUIStream serving with state
// snapshots).
func WithStateSnapshots() ServerOption { return func(c *serverConfig) { c.stateSnapshots = true } }

// WithGuard sets the access control and limits the served endpoint applies
// (see NewGuard). Without one the default limits still apply: a 16 MiB body and
// eight concurrent POST exchanges, no token, no origin check.
func WithGuard(g *Guard) ServerOption { return func(c *serverConfig) { c.guard = g } }

// WithClock sets the clock retention deadlines are read off, for tests that
// would otherwise have to outlast a retention bound to reach one (Python
// parity: the now parameter).
func WithClock(now func() time.Time) ServerOption {
	return func(c *serverConfig) {
		if now != nil {
			c.now = now
		}
	}
}

// NewServer builds a Server with the default retention. An invalid retention is
// an error here rather than a surprise one held turn later (Python parity:
// Retention.__post_init__).
func NewServer(opts ...ServerOption) (*Server, error) {
	cfg := serverConfig{
		retention: DefaultRetention,
		now:       time.Now,
	}
	for _, o := range opts {
		o(&cfg)
	}
	if err := cfg.retention.validate(); err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, turns: newServedTurns(cfg)}
	s.fallbackHook = hitl.DefaultHook
	return s, nil
}

// Options wires an agent for serving: the hook Context.Ask needs behind the
// question middleware, and the middleware that puts each question to the
// client. Spread them into agent.New. A WithHitlHook of the application's own
// passed after these replaces the fallback — the served questions still reach
// the client through the middleware, and the application hook answers only
// what runs outside this server.
func (s *Server) Options() []agent.Option {
	return []agent.Option{
		agent.WithHitlHook(s.fallbackHook),
		agent.WithMiddleware(func() middleware.Middleware { return interrupter{srv: s} }),
	}
}

// ServeAgent builds the served agent — name and prompt as agent.New takes them,
// the application's options applied first, the server's wiring applied after —
// and returns the handler serving it. A WithHitlHook among opts is overridden:
// questions on a served agent go to the AG-UI client.
func (s *Server) ServeAgent(name, prompt string, opts ...agent.Option) http.Handler {
	base := make([]agent.Option, 0, len(opts)+2)
	base = append(base, opts...)
	base = append(base, agent.WithHitlHook(s.fallbackHook))
	base = append(base, agent.WithMiddleware(func() middleware.Middleware { return interrupter{srv: s} }))
	return s.Handler(agent.New(name, prompt, base...))
}

// Close cancels every turn this server is still running, including any held on
// an unanswered question. Call on shutdown.
func (s *Server) Close() { s.turns.releaseAll() }

// Handler serves the agent over AG-UI (Python parity: build_asgi): GET answers
// with the capabilities JSON, POST parses the body as RunAgentInput — 400 when
// it does not parse — and streams the run as SSE, beginning or resuming a turn.
func (s *Server) Handler(a *agent.Agent) http.Handler {
	caps := s.capabilitiesFor(a)
	guard := s.cfg.guard
	if guard == nil {
		guard = NewGuard()
	}
	return handlerFor(caps, s.exchange(a), guard)
}

// capabilitiesFor is what the served agent tells a client it can do (Python
// parity: served_capabilities with state_snapshots=True).
//
// AGO-DEVIATION: Python reads the sections off agent internals AGo does not
// expose, so the served answer is CapabilitiesFor plus what this Server itself
// guarantees: interrupts are held here, and snapshots are emitted when
// configured. Approvals are not declared — the served question cannot name its
// tool call (see answerFrom's deviation note).
func (s *Server) capabilitiesFor(a *agent.Agent) func() AgentCapabilities {
	caps := CapabilitiesFor(a)
	caps.HumanInTheLoop = &HumanInTheLoopCapabilities{
		Supported:  boolPtr(true),
		Interrupts: boolPtr(true),
	}
	if s.cfg.stateSnapshots {
		caps.State = &StateCapabilities{Snapshots: boolPtr(true)}
	}
	return func() AgentCapabilities { return caps }
}

// exchange is the RunFunc the handler drives: it begins or resumes the turn,
// carries it to the run's terminating event, and leaves a held turn running
// (Python parity: serve_exchange).
func (s *Server) exchange(a *agent.Agent) exchangeFunc {
	return func(ctx context.Context, input RunAgentInput, ex *exchange) error {
		if refused := refuseProtocolVersion(input); refused != nil {
			// Before RUN_STARTED: no run starts that this server cannot speak
			// to, and nothing the thread holds is abandoned by it.
			ex.emit(*refused)
			return nil
		}

		if len(input.Resume) > 0 {
			t := s.turns.take(input.ThreadID)
			if t != nil {
				if carried := s.resume(ctx, input, ex, t); carried {
					return nil
				}
				// A resume that answers nothing — an unknown or expired turn:
				// the entries are dropped and the run starts as an ordinary one.
			} else {
				// Answers to nothing this server holds: a restart, or another
				// worker, is no reason to fail the run — the entries are dropped
				// with a warning and the run starts as an ordinary one (Python
				// parity: begin_turn).
				for _, entry := range input.Resume {
					slog.Warn("agui: ignoring an AG-UI resume entry: the thread holds no interrupt",
						"interrupt", entry.InterruptID, "thread", input.ThreadID)
				}
			}
		} else if s.turns.holds(input.ThreadID) {
			// The thread waits on an interrupt this run does not answer; the
			// question stays held (Python parity: begin_turn's NOT_COVERED
			// refusal, before RUN_STARTED).
			ex.emit(refusedRunError(CodeInterruptNotCovered, uncoveredMessage(input.ThreadID)))
			return nil
		}
		s.begin(ctx, a, input, ex)
		return nil
	}
}

// resume hands a resume to the turn it addresses and drives the continuation on
// this exchange. The returned bool reports whether this exchange carried the
// run — honoured, refused, or abandoned; false falls through to an ordinary new
// run (Python parity: resume_held_turn + begin_turn).
func (s *Server) resume(ctx context.Context, input RunAgentInput, ex *exchange, t *servedTurn) bool {
	q := t.outstanding()
	if q == nil || s.turns.expired(t) {
		// Unknown, already answered, or expired: which of them is timing or
		// routing, and none is the client's error. What it sent answers nothing
		// this server asked (Python parity: resume_held_turn's None path).
		t.release()
		s.turns.discard(t)
		return false
	}

	var entry *ResumeEntry
	for i := range input.Resume {
		switch c := input.Resume[i]; {
		case c.InterruptID == q.interrupt.ID && entry == nil:
			entry = &input.Resume[i]
		default:
			// An answer to something this thread is not asking: the protocol
			// says to carry on without it, and to say so.
			slog.Warn("agui: ignoring an AG-UI resume entry for an interrupt the thread is not waiting on",
				"interrupt", c.InterruptID, "thread", input.ThreadID)
		}
	}
	if entry == nil {
		s.turns.restore(t)
		ex.emit(refusedRunError(CodeInterruptNotCovered, uncoveredMessage(input.ThreadID)))
		return true
	}

	// Before the payload is so much as looked at, and before "cancelled" is
	// honoured: ending someone else's turn is not a lesser act than answering
	// it (Python parity: check_proof's placement).
	if err := checkProof(*entry, q.interrupt, s.cfg.requireProof); err != nil {
		s.turns.restore(t)
		ex.emit(refusedRunError(CodeInterruptNotProven, err.Error()))
		return true
	}

	switch entry.Status {
	case "cancelled":
		// The client gave up on the question: the turn stops, and its run
		// closes as cancelled — a stopped run, not a success (Python parity:
		// _cancel, under the RUN_STARTED the exchange already emitted).
		ex.emit(NewRunStartedEvent(input.ThreadID, input.RunID))
		s.abandon(input, ex, t)
		return true
	case "resolved":
		answer, err := answerFrom(*entry, q.interrupt)
		if err != nil {
			s.turns.restore(t)
			ex.emit(refusedRunError(CodeInterruptPayloadRefused, err.Error()))
			return true
		}
		// The resumed run continues the turn's own state: the incoming history
		// is not re-mapped (Python parity: the resume path never reaches
		// run_stream's mapping).
		t.out.rebind(ex, input.RunID)
		ex.emit(NewRunStartedEvent(input.ThreadID, input.RunID))
		t.out.flush()
		t.deliver(q, answer)
		ex.wait()
		return true
	default:
		// run_input strips an entry whose status is not one of the protocol's,
		// so this is unreachable through the HTTP door; refused rather than
		// silently dropped, for a caller that built the input by hand.
		s.turns.restore(t)
		ex.emit(refusedRunError(CodeInterruptNotCovered, uncoveredMessage(input.ThreadID)))
		return true
	}
}

// abandon stops a turn whose question the client gave up on, and closes its run
// as cancelled: first what the turn kept while paused, then an end for each
// entity still open on the wire (Python parity: _cancel).
func (s *Server) abandon(input RunAgentInput, ex *exchange, t *servedTurn) {
	t.release()
	owed := t.out.abandon(t.proj.Closings())
	for _, e := range owed {
		ex.emit(e)
	}
	ex.emit(RunFinishedEvent{
		baseEvent: baseEvent{Type: EventRunFinished, Timestamp: TimestampMs()},
		ThreadID:  input.ThreadID,
		RunID:     input.RunID,
		Usage:     t.proj.TakeUsage(),
		Outcome:   CancelledOutcome{},
	})
	s.turns.discard(t)
}

// begin starts a fresh turn for this exchange and drives it: the mapped history
// seeds the run's transcript, the trailing user messages are its first request,
// and the exchange ends on whatever terminates the run — a question pausing it
// included (Python parity: begin_turn's start path + run_stream + drive_run).
func (s *Server) begin(ctx context.Context, a *agent.Agent, input RunAgentInput, ex *exchange) {
	t := &servedTurn{
		out:         &turnOutput{threadID: input.ThreadID, runID: input.RunID, emit: ex.emit},
		threadID:    input.ThreadID,
		clientState: clientStateOf(input.State),
		driverDone:  make(chan struct{}),
	}
	t.proj = NewStream(t.out.send)

	// The turn is owned by the server, not by this request: its run outlives
	// the exchange that started it, so it must not inherit the request
	// context's cancellation — a paused turn would die with the response it
	// paused on (Python parity: the task ServedTurns owns, outside the request
	// scope). Cancellation travels through release/Close instead, which is why
	// the run scope is opened on the detached context too.
	turnCtx := context.WithoutCancel(ctx)
	run, err := a.Run(turnCtx)
	if err != nil {
		ex.emit(runErrorEvent(err))
		return
	}
	t.run = run
	t.streamID = run.Stream().ID()
	s.turns.bind(t.streamID, t)

	mapped := mapRunInputMessages(input)
	if err := run.Stream().History().Replace(mapped.history); err != nil {
		ex.emit(runErrorEvent(err))
		run.Close()
		s.turns.discard(t)
		return
	}
	t.subs = append(t.subs, run.Stream().Subscribe(func(ev events.Event) error {
		t.proj.Handle(ev)
		return nil
	}, stream.SubscribeOptions{}))

	if len(mapped.current) > 0 {
		if err := run.Enqueue(mapped.current...); err != nil {
			ex.emit(runErrorEvent(err))
			s.endTurn(t)
			run.Close()
			return
		}
	}

	// RUN_STARTED is the exchange's, not the turn's, and names this server's
	// own protocol version (Python parity: serve_exchange).
	ex.emit(NewRunStartedEvent(input.ThreadID, input.RunID))

	if err := run.Start(turnCtx); err != nil {
		ex.emit(runErrorEvent(err))
		s.endTurn(t)
		run.Close()
		return
	}

	// The turn outlives this exchange when it pauses; its driver owns the run
	// from here (Python parity: start's ensure_future + drive_run).
	go s.driveTurn(t)
	ex.wait()
}

// driveTurn carries the turn to its end and closes its run on the wire however
// it ended (Python parity: drive_run). A pause's RUN_FINISHED already ended the
// current run; everything else reports here — on the exchange now carrying the
// turn, or nowhere when no exchange does.
func (s *Server) driveTurn(t *servedTurn) {
	defer close(t.driverDone)
	defer s.turns.discard(t)
	defer s.endTurn(t)

	_, err := t.run.Result(context.Background())
	if t.out.isPaused() {
		// The turn died while parked — retention expired, or the client gave
		// up. Whatever paused it already ended its run on the wire; nothing is
		// carried by ending it again (Python parity: CancelledError behind a
		// paused output).
		return
	}
	switch {
	case err == nil:
		t.out.send(RunFinishedEvent{
			baseEvent: baseEvent{Type: EventRunFinished, Timestamp: TimestampMs()},
			ThreadID:  t.threadID,
			RunID:     t.out.currentRunID(),
			Outcome:   t.proj.SuccessOutcome(),
			Usage:     t.proj.TakeUsage(),
		})
	case ctxWasCancelled(err):
		// A stop the server made — shutdown, release_all — closes the run as
		// cancelled (Python parity: drive_run's CancelledError → output.stop).
		t.out.send(RunFinishedEvent{
			baseEvent: baseEvent{Type: EventRunFinished, Timestamp: TimestampMs()},
			ThreadID:  t.threadID,
			RunID:     t.out.currentRunID(),
			Outcome:   CancelledOutcome{},
			Usage:     t.proj.TakeUsage(),
		})
	default:
		slog.Error("agui: run failed", "thread", t.threadID, "err", err)
		t.out.send(runErrorEvent(err))
	}
}

// endTurn unsubscribes the projection from the run's stream (Python parity:
// drive_run's aclose of the channel — nothing more will be said).
func (s *Server) endTurn(t *servedTurn) {
	if t.run != nil {
		for _, sub := range t.subs {
			t.run.Stream().Unsubscribe(sub)
		}
	}
	t.subs = nil
	t.out.unbind()
}

// ctxWasCancelled reports a run the server stopped, as distinct from one that
// failed (Python parity: drive_run's asyncio.CancelledError branch).
func ctxWasCancelled(err error) bool {
	return errors.Is(err, context.Canceled)
}

// clientStateOf decodes a run input's state when it is a JSON object; anything
// else — absent, a non-object — seeds nothing (Python parity: shares_state).
func clientStateOf(raw json.RawMessage) map[string]any {
	if len(raw) == 0 || raw[0] != '{' {
		return nil
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		slog.Warn("agui: ignoring a run input state that does not decode as an object", "err", err)
		return nil
	}
	return state
}

// refusedRunError is the lone RUN_ERROR a refused resume or a run that never
// started gets, before any stream content (Python parity:
// ResumeRefusedError.as_event). The code, not the prose, is what a client
// branches on.
func refusedRunError(code, message string) RunErrorEvent {
	return RunErrorEvent{
		baseEvent: baseEvent{Type: EventRunError, Timestamp: TimestampMs()},
		Message:   message,
		Code:      code,
	}
}
