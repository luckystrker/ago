package agui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/luckystrker/ago/agent"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/hitl"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
)

// What an interrupt says it is. The protocol leaves Reason a free string; a tool
// call held for approval says so separately, because a client renders the two
// differently — a text box against a pair of buttons — and should not have to
// read the prose to tell (Python parity: interrupts.py INPUT_REQUIRED_REASON /
// TOOL_CALL_REASON).
const (
	ReasonInputRequired = "input_required"
	ReasonToolCall      = "tool_call"
)

// Where this server's own envelope data sits inside an interrupt's metadata.
// Not "ag-ui": the protocol reserves that key for itself, and every other key
// is user space.
//
// AGO-DEVIATION: Python uses the key "ag2"; AGo names its own envelope "ago".
const (
	metadataKey = "ago"
	proofKey    = "proof"
)

// proofBytes of randomness behind one proof. Beyond guessing, and short enough
// to sit in a request body without comment (Python parity: _PROOF_BYTES).
const proofBytes = 32

// The answer schemas an interrupt carries: context.input is one string in, one
// string out; an approval is still answered as a string — the approval
// middleware reads words like "y" — but a client that drew two buttons has a
// boolean in hand and should not have to know which words this server accepts
// (Python parity: ANSWER_SCHEMA / APPROVAL_SCHEMA).
//
// approvalSchema is what a tool-call-reason interrupt would carry. It is kept
// for the protocol vocabulary, but nothing refers to it: AGo's Context.Ask
// strips the ToolApprovalRequest wrapper before this layer sees the question
// (see answerFrom), so every interrupt this server issues reads
// input_required and carries answerSchema.
var (
	answerSchema = map[string]any{
		"type":        "string",
		"title":       "Answer",
		"description": "Your answer to the agent's question.",
	}
	approvalSchema = map[string]any{
		"type":        []string{"string", "boolean"},
		"title":       "Approval",
		"description": "true to let the tool call go ahead, false to refuse it.",
	}
)

// errTurnAbandoned is what a released turn's waiting question surfaces as: the
// client gave up on the answer, so the turn stops (Python parity: release()
// cancelling the suspended task — a failure, not a timeout).
var errTurnAbandoned = errors.New("agui: the client gave up on this question")

// The refusals a resume that cannot be honoured produces, reaching the client
// as the code on a lone RUN_ERROR (Python parity: ResumeRefusedError and its
// codes). Sentinel errors so callers can branch with errors.Is.
var (
	errNotCovered     = errors.New("agui: interrupt not covered")
	errPayloadRefused = errors.New("agui: interrupt payload refused")
	errNotProven      = errors.New("agui: interrupt not proven")
)

// issueProof is a fresh secret, left with one interrupt and asked for on its
// answer. A capability, not a signature: the question it proves is suspended in
// this process, so the value to compare against is in memory beside it — no
// key, nothing to rotate. Per interrupt, so a leaked proof does not carry over
// (Python parity: issue_proof).
func issueProof() string {
	b := make([]byte, proofBytes)
	if _, err := rand.Read(b); err != nil {
		// The process entropy pool is the only source; a failure here is beyond
		// serving. An absent proof reads as missing, which a required check
		// refuses — the run fails closed rather than running on a secret that
		// never existed.
		slog.Error("agui: issuing an interrupt proof", "err", err)
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// withProof stamps the server's envelope, carrying a fresh proof, onto an
// interrupt's metadata.
func withProof(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	out[metadataKey] = map[string]any{proofKey: issueProof()}
	return out
}

// checkProof verifies the proof a resume entry carries against the one its
// interrupt was issued with. A proof that is present must be the one issued;
// one that is absent from the resume is refused only when required — a standard
// client copies nothing from an interrupt's metadata (Python parity:
// check_proof).
//
// AGO-DEVIATION: Python's check_proof also accepts a required resume when
// neither side carries a proof, so an interrupt issued through a failed
// entropy source is answered by a resume that merely repeats the empty
// envelope. AGo fails closed: a required proof must exist on both sides and
// match — nothing was proved when there was no secret to present (security
// audit L-16).
func checkProof(entry ResumeEntry, interrupt Interrupt, required bool) error {
	entryProof, entryHas := proofIn(entry.Metadata)
	interruptProof, interruptHas := proofIn(interrupt.Metadata)
	if !required && !entryHas {
		return nil
	}
	if !entryHas || !interruptHas || subtle.ConstantTimeCompare([]byte(entryProof), []byte(interruptProof)) != 1 {
		return fmt.Errorf("%w: the resume for interrupt %s does not carry the proof it was issued with", errNotProven, interrupt.ID)
	}
	return nil
}

// proofIn reads the proof out of a metadata envelope. A malformed proof and a
// missing one are one case when a proof is required: both mean nothing was
// proved (Python parity: _proof_in). An empty value is a missing one too — a
// real proof is 32 random bytes, and the empty envelope is what a failed
// issueProof leaves behind.
func proofIn(metadata map[string]any) (string, bool) {
	envelope, ok := metadata[metadataKey].(map[string]any)
	if !ok {
		return "", false
	}
	p, ok := envelope[proofKey].(string)
	if !ok || p == "" {
		return "", false
	}
	return p, true
}

// answerFrom reads the answer a resume entry carries, as the string the waiting
// call will read (Python parity: answer_from).
//
// AGO-DEVIATION: Python refuses a boolean payload for every interrupt that is
// not a tool approval. AGo's Context.Ask hands middleware a request stripped of
// its ToolApprovalRequest wrapper (middleware/approval.go), so this layer
// cannot tell an approval from a plain question; a boolean is translated for
// any interrupt — the words a human typing "true" would send. See
// docs/DECISIONS.md D-065.
func answerFrom(entry ResumeEntry, interrupt Interrupt) (string, error) {
	switch p := entry.Payload.(type) {
	case string:
		return p, nil
	case bool:
		if p {
			return "y", nil
		}
		return "n", nil
	default:
		kind := fmt.Sprintf("%T", entry.Payload)
		if entry.Payload == nil {
			kind = "null"
		}
		return "", fmt.Errorf("%w: interrupt %s asked for a string answer, got %s", errPayloadRefused, interrupt.ID, kind)
	}
}

// uncoveredMessage is the refusal text for a run that does not answer what its
// thread is waiting on (Python parity: _uncovered).
func uncoveredMessage(threadID string) string {
	return fmt.Sprintf("thread %s is waiting on an interrupt this run does not answer: "+
		`resume it, or give it up with a "cancelled" entry`, threadID)
}

// Retention is how long an unanswered question is kept, and how many at once
// (Python parity: Retention).
type Retention struct {
	// TTL is how long a held turn survives unanswered, after which it is
	// cancelled. Also sent to the client as the interrupt's ExpiresAt.
	TTL time.Duration
	// MaxHeld is how many held turns are allowed at once. Holding past this one
	// cancels the oldest — the one nearest its deadline.
	MaxHeld int
}

// DefaultRetention is the retention a Server gets when none is configured
// (Python parity: DEFAULT_RETENTION — 900 seconds, 128 held turns).
var DefaultRetention = Retention{TTL: 900 * time.Second, MaxHeld: 128}

func (r Retention) validate() error {
	if r.TTL <= 0 {
		return fmt.Errorf("agui: retention TTL must be positive, got %s", r.TTL)
	}
	if r.MaxHeld < 1 {
		return fmt.Errorf("agui: retention MaxHeld must be at least 1, got %d", r.MaxHeld)
	}
	return nil
}

// question is one outstanding interrupt: the wire representation, and the
// channel its answer travels back on.
type question struct {
	interrupt Interrupt
	answer    chan string
	abandon   chan struct{}
}

// servedTurn is one AG-UI-served agent turn, owned by the Server rather than by
// a request: a turn paused on a question survives the exchange that started it
// and is carried by the exchange that resumes it (Python parity: ServedTurn +
// TurnOutput, minus the subagent suspension ledger — see docs/DECISIONS.md
// D-065 for what is deferred).
type servedTurn struct {
	out  *turnOutput
	proj *Stream
	run  *agent.AgentRun
	subs []stream.SubID

	threadID string
	streamID string

	// clientState is the state object the opening run input carried, decoded
	// when it was a JSON object; nil otherwise. Written before the turn starts
	// and read-only afterwards (Python parity: a dict incoming.state).
	clientState map[string]any

	// driverDone closes when the turn's driver has handled the run's end.
	driverDone chan struct{}

	askMu sync.Mutex // one question outstanding per turn at a time
	mu    sync.Mutex // guards question, deadline
	// question is non-nil while the turn is parked waiting for an answer;
	// deadline is when that answer stops being acceptable.
	question *question
	deadline time.Time

	releaseOnce sync.Once
}

func (t *servedTurn) outstanding() *question {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.question
}

func (t *servedTurn) setQuestion(q *question, deadline time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.question, t.deadline = q, deadline
}

func (t *servedTurn) clearQuestion(q *question) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.question == q {
		t.question, t.deadline = nil, time.Time{}
	}
}

// deliver hands the answer to the waiting call (Python parity: deliver — the
// question is forgotten synchronously, so a resume landing the next instant
// reads the turn as no longer waiting).
func (t *servedTurn) deliver(q *question, answer string) {
	t.clearQuestion(q)
	q.answer <- answer
}

// unpark forgets the question and stops holding the turn, however the wait
// ended (Python parity: wake + discard in _put's finally).
func (t *servedTurn) unpark(s *Server, q *question) {
	t.clearQuestion(q)
	s.turns.discard(t)
}

// release cancels the turn, wherever it is parked: the waiting question
// surfaces its abandonment and the run scope winds down (Python parity:
// release/aclose).
func (t *servedTurn) release() {
	t.releaseOnce.Do(func() {
		if q := t.outstanding(); q != nil {
			close(q.abandon)
		}
		if t.run != nil {
			t.run.Close()
		}
	})
}

// turnOutput routes a turn's events to the exchange currently carrying it, and
// keeps what a paused turn produces for the exchange that resumes it (Python
// parity: TurnOutput's send/pause/rebind, minus the subagent suspension
// machinery). Safe for concurrent use: the turn's driver, its stream dispatch
// and the question middleware all send through one output.
type turnOutput struct {
	mu       sync.Mutex
	threadID string
	runID    string
	emit     func(Event) // the exchange now carrying the turn; nil when none is
	paused   bool
	kept     []Event
}

func (o *turnOutput) send(e Event) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.paused {
		o.kept = append(o.kept, e)
		return
	}
	o.deliver(e)
}

// currentRunID is the run id the turn's events name right now (Python parity:
// TurnOutput.run_id, rebound per resuming exchange).
func (o *turnOutput) currentRunID() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.runID
}

// hold pauses the turn: what it sends from now on is kept for the exchange
// that resumes it (Python parity: the head of TurnOutput.pause). pause then
// flushes what was kept and delivers the interrupt that ends the run.
func (o *turnOutput) hold() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.paused = true
}

// pause ends the current exchange on the interrupt, keeping what follows for
// the next (Python parity: pause — paused before anything is delivered, so
// events the turn still sends belong to the run that resumes it).
func (o *turnOutput) pause(interrupt RunFinishedEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.paused = true
	kept := o.kept
	o.kept = nil
	for _, e := range kept {
		o.deliver(e)
	}
	o.deliver(interrupt)
}

// rebind points the turn at the exchange now carrying it. Kept events are not
// flushed here: the resuming exchange emits RUN_STARTED first and calls flush
// after (Python parity: rebind plus serve_exchange's ordering).
func (o *turnOutput) rebind(ex *exchange, runID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.runID = runID
	o.emit = ex.emit
	o.paused = false
}

// flush delivers what the turn kept while paused.
func (o *turnOutput) flush() {
	o.mu.Lock()
	defer o.mu.Unlock()
	kept := o.kept
	o.kept = nil
	for _, e := range kept {
		o.deliver(e)
	}
}

// abandon hands back everything owed to the exchange that gives up on a paused
// turn — what it kept, then the closing events for entities still open — and
// unbinds the turn, so whatever the dying run still produces is dropped
// (Python parity: TurnOutput.abandon).
func (o *turnOutput) abandon(closings []Event) []Event {
	o.mu.Lock()
	defer o.mu.Unlock()
	owed := make([]Event, 0, len(o.kept)+len(closings))
	owed = append(owed, o.kept...)
	owed = append(owed, closings...)
	o.kept, o.emit = nil, nil
	return owed
}

// unbind drops the exchange between a paused turn's death and its driver's
// return, so the driver's terminal handling writes nowhere.
func (o *turnOutput) unbind() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.emit = nil
}

// isPaused reports whether the turn is parked on a question (Python parity:
// TurnOutput.paused).
func (o *turnOutput) isPaused() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.paused
}

func (o *turnOutput) deliver(e Event) {
	if o.emit != nil {
		o.emit(e)
	}
}

// servedTurns is every turn this server is running, and which of them are held.
// Held turns are keyed by thread, not by run: a resume carries a new run id
// under the same thread (Python parity: ServedTurns). State lives in this
// process only: resumes must be routed back to it, and nothing survives a
// restart.
type servedTurns struct {
	mu        sync.Mutex
	retention Retention
	now       func() time.Time
	held      map[string]*servedTurn
	byStream  map[string]*servedTurn
}

func newServedTurns(cfg serverConfig) *servedTurns {
	return &servedTurns{
		retention: cfg.retention,
		now:       cfg.now,
		held:      map[string]*servedTurn{},
		byStream:  map[string]*servedTurn{},
	}
}

// bind records which turn a run's stream belongs to: the question middleware
// finds its turn through the asking context's stream id.
func (st *servedTurns) bind(streamID string, t *servedTurn) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.byStream[streamID] = t
}

// turnForStream is the turn whose run owns this stream, if there is one.
func (st *servedTurns) turnForStream(streamID string) *servedTurn {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.byStream[streamID]
}

// hold holds the turn for its thread, evicting whatever that thread held before
// and stamping the retention clock, so each new question buys a full bound
// (Python parity: hold).
func (st *servedTurns) hold(t *servedTurn, deadline time.Time) {
	t.mu.Lock()
	t.deadline = deadline
	t.mu.Unlock()

	st.mu.Lock()
	defer st.mu.Unlock()
	if previous := st.held[t.threadID]; previous != nil && previous != t {
		slog.Info("agui: releasing a held AG-UI turn whose thread has been superseded", "thread", t.threadID)
		previous.release()
	}
	st.held[t.threadID] = t
	st.evictExpiredLocked()
	for len(st.held) > st.retention.MaxHeld {
		// Nearest its deadline, rather than longest held: under one TTL the two
		// are the same turn (Python parity: hold's eviction loop).
		soonest := st.soonestLocked()
		if soonest == nil {
			break
		}
		slog.Warn("agui: releasing the held AG-UI turn nearest its deadline", "held", len(st.held))
		delete(st.held, soonest.threadID)
		soonest.release()
	}
}

// soonestLocked is the held turn nearest its deadline; nil when every held turn
// waits on an interrupt that never expires (Python parity: _deadline_of's
// infinity).
func (st *servedTurns) soonestLocked() *servedTurn {
	var (
		soonest *servedTurn
		at      time.Time
	)
	for _, t := range st.held {
		t.mu.Lock()
		d := t.deadline
		t.mu.Unlock()
		if d.IsZero() {
			continue
		}
		if soonest == nil || d.Before(at) {
			soonest, at = t, d
		}
	}
	return soonest
}

// take removes and returns the turn held for thread, or nil. Removed rather
// than looked up, so two resumes racing one thread cannot both drive it. An
// expired turn is still returned — the caller decides what to tell a late
// answer (Python parity: take).
func (st *servedTurns) take(threadID string) *servedTurn {
	st.mu.Lock()
	defer st.mu.Unlock()
	t := st.held[threadID]
	delete(st.held, threadID)
	st.evictExpiredLocked()
	return t
}

// restore puts back a turn taken for a resume that was refused. Not hold: the
// turn keeps the deadline it was holding on, so a stream of bad resumes can
// neither extend its life nor move it down the eviction queue (Python parity:
// restore).
func (st *servedTurns) restore(t *servedTurn) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.held[t.threadID] = t
}

// discard stops holding the turn and forgets its stream binding, if this is
// still the turn its thread holds (Python parity: discard).
func (st *servedTurns) discard(t *servedTurn) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.held[t.threadID] == t {
		delete(st.held, t.threadID)
	}
	if st.byStream[t.streamID] == t {
		delete(st.byStream, t.streamID)
	}
}

// holds reports whether the thread holds a question that can still be answered.
func (st *servedTurns) holds(threadID string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.evictExpiredLocked()
	_, ok := st.held[threadID]
	return ok
}

// releaseAll cancels every turn this server is running (Python parity:
// release_all). Call it on shutdown, so a turn still waiting is cancelled.
func (st *servedTurns) releaseAll() {
	st.mu.Lock()
	seen := map[*servedTurn]bool{}
	all := make([]*servedTurn, 0, len(st.held)+len(st.byStream))
	for _, t := range st.held {
		if !seen[t] {
			seen[t] = true
			all = append(all, t)
		}
	}
	for _, t := range st.byStream {
		if !seen[t] {
			seen[t] = true
			all = append(all, t)
		}
	}
	st.held = map[string]*servedTurn{}
	st.byStream = map[string]*servedTurn{}
	st.mu.Unlock()
	for _, t := range all {
		t.release()
	}
}

// evictExpiredLocked releases the turns whose questions can no longer be
// answered (Python parity: _evict_expired). st.mu is held; t.mu covers the
// deadlines.
func (st *servedTurns) evictExpiredLocked() {
	now := st.now()
	for threadID, t := range st.held {
		t.mu.Lock()
		d := t.deadline
		t.mu.Unlock()
		if !d.IsZero() && !d.After(now) {
			slog.Info("agui: releasing an expired held AG-UI turn", "thread", threadID)
			delete(st.held, threadID)
			t.release()
		}
	}
}

// expired reports whether the question the turn is held on can still be
// answered (Python parity: expired).
func (st *servedTurns) expired(t *servedTurn) bool {
	t.mu.Lock()
	d := t.deadline
	t.mu.Unlock()
	return !d.IsZero() && !d.After(st.now())
}

// interrupter is the middleware that answers a served run's questions from the
// AG-UI client: a question parks the turn — its run pauses on the wire with an
// interrupt outcome, never an error — and waits for the resume that answers it
// (Python parity: ClientInterrupter, registered where Python would install a
// human-input hook).
//
// A run this server did not start has no turn bound to its stream; its
// questions fall through to the agent's own hook.
type interrupter struct {
	middleware.Base
	srv *Server
}

// OnTurn emits the snapshot a run opens with, when the server is configured to
// emit state snapshots (Python parity: _serve_turn's opening
// StateSnapshotEvent).
func (in interrupter) OnTurn(next middleware.TurnFunc, ev *events.ModelRequest, cx *agocontext.Context) (*events.ModelResponse, error) {
	if in.srv.cfg.stateSnapshots {
		if t := in.srv.turns.turnForStream(cx.Stream.ID()); t != nil {
			t.out.send(NewStateSnapshotEvent(openingSnapshot(in.srv, t, cx)))
		}
	}
	return next(ev, cx)
}

// OnHumanInput parks the turn on the question and waits for the resume that
// answers it. The returned message is what the asking call reads; Context.Ask
// transcribes it on the transcript (Python parity: ClientInterrupter.__call__).
func (in interrupter) OnHumanInput(next middleware.HumanInputFunc, ev *events.HumanInputRequest, cx *agocontext.Context) (*events.HumanMessage, error) {
	t := in.srv.turns.turnForStream(cx.Stream.ID())
	if t == nil {
		// Not this server's run: an in-process use of the same agent keeps its
		// own arrangements (Python parity: a caller who passed a hook answers in
		// process).
		return next(ev, cx)
	}
	answer, err := in.srv.askClient(t, ev)
	if err != nil {
		return nil, err
	}
	return &events.HumanMessage{ParentID: ev.ID, Content: answer}, nil
}

// askClient puts a question to the client, holds the turn, and returns the
// answer (Python parity: ServedTurns.ask + _put). Concurrent askers on one turn
// are put one at a time: each next question is the outcome of the run that
// answers the one before.
func (s *Server) askClient(t *servedTurn, req *events.HumanInputRequest) (string, error) {
	t.askMu.Lock()
	defer t.askMu.Unlock()

	since := s.cfg.now()
	deadline := s.deadline(req.Timeout, since)
	interrupt := s.interruptFor(t, req, deadline)
	q := &question{
		interrupt: interrupt,
		answer:    make(chan string, 1),
		abandon:   make(chan struct{}),
	}
	t.setQuestion(q, deadline)
	defer t.unpark(s, q)
	// Held before the question is emitted, never after: the exchange ends on
	// that very event, so a resume can be in flight the moment it lands (Python
	// parity: _put holds before pausing).
	s.turns.hold(t, deadline)
	// Paused before the usage is read, so whatever the turn still sends — a
	// sibling tool call finishing — belongs to the run that resumes it, on the
	// wire and on the meter.
	t.out.hold()

	t.out.pause(RunFinishedEvent{
		baseEvent: baseEvent{Type: EventRunFinished, Timestamp: TimestampMs()},
		ThreadID:  t.threadID,
		RunID:     t.out.currentRunID(),
		Usage:     t.proj.TakeUsage(),
		Outcome:   InterruptOutcome{Interrupts: []Interrupt{interrupt}},
	})

	if deadline.IsZero() {
		select {
		case answer := <-q.answer:
			return answer, nil
		case <-q.abandon:
			return "", &hitl.FailedError{Cause: errTurnAbandoned}
		}
	}
	// The deadline the client was shown is kept here, by the call that
	// advertised it: a registry swept only by later traffic would hold an
	// unanswered question for as long as the process stayed quiet (Python
	// parity: wait_for on the answer). The timer reads the configured clock's
	// remaining share, so a fake clock shortens it the way it shortens the
	// registry's sweep.
	timer := time.NewTimer(deadline.Sub(since))
	defer timer.Stop()
	select {
	case answer := <-q.answer:
		return answer, nil
	case <-q.abandon:
		return "", &hitl.FailedError{Cause: errTurnAbandoned}
	case <-timer.C:
		return "", &hitl.TimeoutError{Timeout: deadline.Sub(since).Seconds()}
	}
}

// interruptFor builds the wire interrupt for one human-input request, under the
// request's own id (Python parity: ClientInterrupter.interrupt_for). A request
// gating a tool call says so — where AGo can tell; see answerFrom's deviation
// note. Tagged only with an invocation the client was told about.
func (s *Server) interruptFor(t *servedTurn, req *events.HumanInputRequest, deadline time.Time) Interrupt {
	in := Interrupt{
		ID:             req.ID,
		Reason:         ReasonInputRequired,
		Message:        req.Content,
		ResponseSchema: answerSchema,
	}
	if req.TaskID != "" && t.proj.OpenSubagent(req.TaskID) {
		in.SubagentRunID = req.TaskID
	}
	if !deadline.IsZero() {
		in.ExpiresAt = deadline.UTC().Format(time.RFC3339Nano)
	}
	in.Metadata = withProof(nil)
	return in
}

// deadline is when an interrupt raised now stops being answerable: the earlier
// of the retention TTL from now and the request's own timeout from since
// (Python parity: ServedTurns.deadline). Clients reject late answers locally
// against this figure, so it has to be the one that will in fact apply.
func (s *Server) deadline(timeoutSeconds float64, since time.Time) time.Time {
	retained := since.Add(s.cfg.retention.TTL)
	if timeoutSeconds <= 0 {
		return retained
	}
	if timeout := since.Add(time.Duration(timeoutSeconds * float64(time.Second))); timeout.Before(retained) {
		return timeout
	}
	return retained
}

// openingSnapshot is the state a run opens with: the client's keys and the
// server's variables together, the framework's reserved keys left out (Python
// parity: _serve_turn's initial_state plus _encode_context). A snapshot
// replaces the client's state wholesale, so the client's keys travel in it.
//
// AGO-DEVIATION: Python seeds the turn's variables with the client's state;
// AGo's served run cannot write the conversation variables from outside the
// agent package, so the client's state is merged into the snapshot
// representation only, and seeding is deferred (docs/DECISIONS.md D-065).
func openingSnapshot(s *Server, t *servedTurn, cx *agocontext.Context) map[string]any {
	out := map[string]any{}
	for k, v := range t.clientState {
		out[k] = v
	}
	for k, v := range cx.VariablesSnapshot() {
		out[k] = v
	}
	for k := range out {
		if reservedVariable(k) {
			delete(out, k)
		}
	}
	return out
}

// reservedVariable reports whether a variable key belongs to the framework's
// control plane (Python parity: strip_reserved_variables; the prefixes are the
// context package's contract).
func reservedVariable(key string) bool {
	const (
		agoPrefix = "ag:"
		a2aPrefix = "a2a:"
	)
	return len(key) >= len(agoPrefix) && key[:len(agoPrefix)] == agoPrefix ||
		len(key) >= len(a2aPrefix) && key[:len(a2aPrefix)] == a2aPrefix
}
