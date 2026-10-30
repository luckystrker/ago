package network

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Expectation sweepers port hub/expectations.py + hub/sweepers.py:
// protocol-shape contracts declared on ChannelManifest.Expectations. A
// send-time validation rejects bad sends; expectations react to bad
// silence (or bad pacing) — the hub evaluates them on a periodic tick,
// independently of the per-envelope send path.
//
// Handlers are passive: the hub records, signals, or closes; it never
// re-tries, substitutes content, or makes outcome decisions for the agent.

// Default sweep interval (Python hub default expectation_sweep_interval=10s).
const DefaultExpectationSweepInterval = 10 * time.Second

// ViolationHandler names (Expectation.OnViolation selects by them).
const (
	HandlerAudit         = "audit"
	HandlerNotifyChannel = "notify_channel"
	HandlerAutoClose     = "auto_close"
)

// Violation is an evaluator's verdict: which expectation fired, whom it
// applies to (an empty list = channel-wide, e.g. max_silence) and the
// free-form detail payload riding the violation envelope / audit record.
type Violation struct {
	Expectation Expectation
	ViolatorIDs []string
	Detail      map[string]any
}

// ExpectationContext bundles the per-tick inputs handed to every
// evaluator: pure functions over (channel, wal, clock).
type ExpectationContext struct {
	Channel *ChannelHandle
	WAL     []Envelope
	Now     time.Time
}

// ExpectationEvaluator is a pure predicate over the context. It must be
// deterministic — the sweeper runs it on a periodic tick, so a
// non-deterministic evaluator would flap. A nil result means "no violation
// right now".
type ExpectationEvaluator interface {
	Name() string
	Evaluate(exp Expectation, ctx ExpectationContext) *Violation
}

// ViolationHandler reacts to a fired violation. It must tolerate duplicate
// calls across hub restarts (the fired-violation dedup is in-memory only).
type ViolationHandler interface {
	Name() string
	Handle(h *Hub, channelID string, violation Violation)
}

// expectationParamSeconds reads the params "seconds" number with a
// default. JSON decoding lands it as float64; manifest authors may also
// pass a plain int.
func expectationParamSeconds(exp Expectation, def float64) float64 {
	switch n := exp.Params["seconds"].(type) {
	case float64:
		if n > 0 {
			return n
		}
	case int:
		if n > 0 {
			return float64(n)
		}
	case int64:
		if n > 0 {
			return float64(n)
		}
	}
	return def
}

// --- evaluators ---------------------------------------------------------------

// AcksWithinEvaluator fires while a pending channel's invitees haven't
// acked within params["seconds"] (default 30).
type AcksWithinEvaluator struct{}

// Name implements ExpectationEvaluator.
func (AcksWithinEvaluator) Name() string { return "acks_within" }

// Evaluate implements ExpectationEvaluator.
func (AcksWithinEvaluator) Evaluate(exp Expectation, ctx ExpectationContext) *Violation {
	if ctx.Channel.State != ChannelPending {
		return nil
	}
	seconds := expectationParamSeconds(exp, 30)
	elapsed := ctx.Now.Sub(ctx.Channel.CreatedAt).Seconds()
	if elapsed < seconds || len(ctx.Channel.PendingAcks) == 0 {
		return nil
	}
	return &Violation{
		Expectation: exp,
		ViolatorIDs: append([]string(nil), ctx.Channel.PendingAcks...),
		Detail: map[string]any{
			"elapsed_seconds":   elapsed,
			"threshold_seconds": seconds,
		},
	}
}

// ReplyWithinEvaluator fires for participants addressed by a text envelope
// who haven't sent any text of their own since, after params["seconds"]
// (default 600). Active channels only.
type ReplyWithinEvaluator struct{}

// Name implements ExpectationEvaluator.
func (ReplyWithinEvaluator) Name() string { return "reply_within" }

// Evaluate implements ExpectationEvaluator.
func (ReplyWithinEvaluator) Evaluate(exp Expectation, ctx ExpectationContext) *Violation {
	if ctx.Channel.State != ChannelActive {
		return nil
	}
	seconds := expectationParamSeconds(exp, 600)

	// Latest text addressed to each participant + each participant's
	// latest sent text (single pass, WAL order; later envelopes win).
	latestIn := map[string]Envelope{}
	latestOut := map[string]Envelope{}
	for _, env := range ctx.WAL {
		if env.EventType != EnvText {
			continue
		}
		latestOut[env.SenderID] = env
		for _, p := range ctx.Channel.Participants {
			if p.AgentID == env.SenderID {
				continue
			}
			// Audience "" = broadcast within the channel (Python: audience
			// None hits everyone; an addressed envelope hits the addressee).
			if env.Audience != "" && env.Audience != p.AgentID {
				continue
			}
			latestIn[p.AgentID] = env
		}
	}

	var violators []string
	for _, p := range ctx.Channel.Participants {
		inbound, ok := latestIn[p.AgentID]
		if !ok {
			continue
		}
		if out, sent := latestOut[p.AgentID]; sent && !out.CreatedAt.Before(inbound.CreatedAt) {
			continue
		}
		if ctx.Now.Sub(inbound.CreatedAt).Seconds() >= seconds {
			violators = append(violators, p.AgentID)
		}
	}
	if len(violators) == 0 {
		return nil
	}
	return &Violation{
		Expectation: exp,
		ViolatorIDs: violators,
		Detail:      map[string]any{"threshold_seconds": seconds},
	}
}

// MaxSilenceEvaluator fires when the channel has had no content envelope
// for params["seconds"] (default 3600). Channel-wide: ViolatorIDs is empty.
// Anchors on the latest content envelope, falling back to channel creation.
type MaxSilenceEvaluator struct{}

// Name implements ExpectationEvaluator.
func (MaxSilenceEvaluator) Name() string { return "max_silence" }

// Evaluate implements ExpectationEvaluator.
func (MaxSilenceEvaluator) Evaluate(exp Expectation, ctx ExpectationContext) *Violation {
	if ctx.Channel.State != ChannelActive {
		return nil
	}
	seconds := expectationParamSeconds(exp, 3600)
	anchor := ctx.Channel.CreatedAt
	for i := len(ctx.WAL) - 1; i >= 0; i-- {
		if isContentEvent(ctx.WAL[i].EventType) {
			anchor = ctx.WAL[i].CreatedAt
			break
		}
	}
	elapsed := ctx.Now.Sub(anchor).Seconds()
	if elapsed < seconds {
		return nil
	}
	return &Violation{
		Expectation: exp,
		ViolatorIDs: nil,
		Detail: map[string]any{
			"elapsed_seconds":   elapsed,
			"threshold_seconds": seconds,
		},
	}
}

// isContentEvent reports a substantive (non-protocol, non-task, non-
// violation) envelope — the silence anchor set.
func isContentEvent(eventType string) bool {
	if strings.HasPrefix(eventType, "ag2.channel.") || strings.HasPrefix(eventType, "ag2.task.") {
		return false
	}
	return eventType != EnvExpectationViolated
}

// --- handlers -----------------------------------------------------------------

// AuditViolationHandler records the fire through the hub's audit sink and
// sends nothing (Python parity: the audit handler is a no-op wrapper — the
// hub itself records every fire to the audit sink before invoking the
// handler, so this handler exists so manifests authored against the
// "audit" on_violation value resolve).
type AuditViolationHandler struct{}

// Name implements ViolationHandler.
func (AuditViolationHandler) Name() string { return HandlerAudit }

// Handle implements ViolationHandler.
func (AuditViolationHandler) Handle(*Hub, string, Violation) {}

// NotifyChannelHandler broadcasts EnvExpectationViolated to every channel
// participant (sender = creator). Posting is best-effort: a closing
// channel must not crash the sweeper.
type NotifyChannelHandler struct{}

// Name implements ViolationHandler.
func (NotifyChannelHandler) Name() string { return HandlerNotifyChannel }

// Handle implements ViolationHandler.
func (NotifyChannelHandler) Handle(h *Hub, channelID string, violation Violation) {
	h.mu.Lock()
	ch, ok := h.channels[channelID]
	var creator string
	if ok {
		creator = ch.CreatorID
	}
	h.mu.Unlock()
	if !ok || ch.State.Terminal() {
		return
	}
	payload := mustJSON(map[string]any{
		"expectation": violation.Expectation.Kind,
		"violators":   violation.ViolatorIDs,
		"detail":      violation.Detail,
	})
	env := Envelope{
		ChannelID: channelID,
		SenderID:  creator,
		EventType: EnvExpectationViolated,
		Payload:   payload,
	}
	_, _ = h.Send(env)
}

// AutoCloseViolationHandler transitions the channel to closed with reason
// "expectation_violated:<name>".
type AutoCloseViolationHandler struct{}

// Name implements ViolationHandler.
func (AutoCloseViolationHandler) Name() string { return HandlerAutoClose }

// Handle implements ViolationHandler.
func (AutoCloseViolationHandler) Handle(h *Hub, channelID string, violation Violation) {
	h.closeChannel(channelID, fmt.Sprintf("expectation_violated:%s", violation.Expectation.Kind), nil)
}

// DefaultExpectationEvaluators returns the built-in evaluator set.
func DefaultExpectationEvaluators() []ExpectationEvaluator {
	return []ExpectationEvaluator{
		AcksWithinEvaluator{},
		ReplyWithinEvaluator{},
		MaxSilenceEvaluator{},
	}
}

// DefaultViolationHandlers returns the built-in handler set.
func DefaultViolationHandlers() []ViolationHandler {
	return []ViolationHandler{
		AuditViolationHandler{},
		NotifyChannelHandler{},
		AutoCloseViolationHandler{},
	}
}

// --- hub-side sweeper ---------------------------------------------------------

// ExpectationSweeper evaluates every channel's expectations on a fixed
// interval and fires handlers on new violations. The tick is available
// standalone (Hub.SweepExpectations) so tests and custom loops run it
// deterministically; Start/Stop wrap it in a background goroutine.
type ExpectationSweeper struct {
	hub      *Hub
	interval time.Duration

	mu      sync.Mutex
	stop    chan struct{}
	stopped chan struct{}
	once    sync.Once
}

// NewExpectationSweeper builds a sweeper over hub with one tick per
// interval.
func NewExpectationSweeper(hub *Hub, interval time.Duration) *ExpectationSweeper {
	return &ExpectationSweeper{hub: hub, interval: interval, stop: make(chan struct{}), stopped: make(chan struct{})}
}

// Start spawns the loop. Idempotent.
func (s *ExpectationSweeper) Start() {
	s.once.Do(func() {
		go func() {
			defer close(s.stopped)
			t := time.NewTicker(s.interval)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					s.hub.SweepExpectations()
				case <-s.stop:
					return
				}
			}
		}()
	})
}

// Stop cancels the loop and waits for the goroutine to exit. Idempotent.
func (s *ExpectationSweeper) Stop() {
	s.mu.Lock()
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	s.mu.Unlock()
	<-s.stopped
}

// SweepExpectations runs one tick: evaluate every non-terminal channel's
// manifest expectations, fire the per-expectation handler on violations
// not seen before, and record every fire through the audit sink (the
// AuditLog parity — the sink sees every fire regardless of which handler
// the expectation selects). A tick failure on one channel never aborts
// the others. Safe for concurrent use.
func (h *Hub) SweepExpectations() {
	// Snapshot the channels to evaluate, then work outside the lock: a
	// handler posts envelopes / closes channels (it takes the lock itself).
	h.mu.Lock()
	if !h.expectationsArmed() {
		h.mu.Unlock()
		return
	}
	var ids []string
	for id, ch := range h.channels {
		if !ch.State.Terminal() && len(ch.Manifest.Expectations) > 0 {
			ids = append(ids, id)
		}
	}
	now := h.now()
	h.mu.Unlock()

	for _, id := range ids {
		h.sweepChannel(id, now)
	}
}

// expectationsArmed reports whether evaluator/handler registries are
// populated (a hub with no expectations wired skips ticks entirely).
func (h *Hub) expectationsArmed() bool {
	return len(h.expectEvaluators) > 0 && len(h.expectHandlers) > 0
}

// sweepChannel evaluates one channel's expectations at instant now.
func (h *Hub) sweepChannel(channelID string, now time.Time) {
	h.mu.Lock()
	ch, ok := h.channels[channelID]
	if !ok || ch.State.Terminal() {
		h.mu.Unlock()
		return
	}
	// Snapshot what evaluators may read: the handle fields they touch are
	// plain values, the WAL is copied.
	snap := *ch
	snap.Participants = append([]Participant(nil), ch.Participants...)
	snap.PendingAcks = append([]string(nil), ch.PendingAcks...)
	wal := append([]Envelope(nil), h.logs[channelID]...)
	expectations := append([]Expectation(nil), ch.Manifest.Expectations...)
	ctx := ExpectationContext{Channel: &snap, WAL: wal, Now: now}

	type pendingFire struct {
		idx    int
		key    string
		v      *Violation
		action ViolationHandler
	}
	var fires []pendingFire
	for idx, exp := range expectations {
		evaluator, ok := h.expectEvaluators[exp.Kind]
		if !ok {
			continue
		}
		v := evaluator.Evaluate(exp, ctx)
		if v == nil {
			continue
		}
		handler, ok := h.expectHandlers[exp.OnViolation]
		if !ok {
			continue // unknown on_violation: silently ignored (Python parity)
		}
		fired := h.firedViolations[channelID]
		for _, vid := range orEmpty(v.ViolatorIDs) {
			key := fmt.Sprintf("%d|%s|%s", idx, exp.Kind, vid)
			if fired[key] {
				continue
			}
			if fired == nil {
				fired = map[string]bool{}
				h.firedViolations[channelID] = fired
			}
			fired[key] = true
			// Every fire reaches the audit sink before the handler —
			// Python fans on_expectation_fired out to AuditLog the same
			// way. A sink failure is swallowed (auditing never breaks the
			// flow it observes).
			violators := append([]string(nil), v.ViolatorIDs...)
			h.auditEntry(AuditExpectationViolated, channelID, func(e *AuditEntry) {
				e.Expectation = exp.Kind
				e.Violators = violators
				e.Detail = v.Detail
			})
			fires = append(fires, pendingFire{idx: idx, key: key, v: v, action: handler})
		}
	}
	h.mu.Unlock()

	for _, f := range fires {
		func() {
			// A panicking handler must not kill the sweeper (Python
			// suppresses handler exceptions; the violation stays marked
			// fired so a bad handler cannot spin).
			defer func() { _ = recover() }()
			f.action.Handle(h, channelID, *f.v)
		}()
	}
}

// forgetFiredViolations drops the dedup cache of a terminal channel.
func (h *Hub) forgetFiredViolations(channelID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.firedViolations, channelID)
}

// orEmpty maps a channel-wide violation (no violators) to a single empty
// key so the dedup still tracks the fire.
func orEmpty(ids []string) []string {
	if len(ids) == 0 {
		return []string{""}
	}
	return ids
}

// RegisterExpectationEvaluator installs an evaluator by name (replacing an
// existing one). Register before creating channels whose manifests declare
// expectations.
func (h *Hub) RegisterExpectationEvaluator(e ExpectationEvaluator) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expectEvaluators[e.Name()] = e
}

// RegisterViolationHandler installs a violation handler by name.
func (h *Hub) RegisterViolationHandler(vh ViolationHandler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expectHandlers[vh.Name()] = vh
}
