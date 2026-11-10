package network

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Client-layer errors.
var (
	ErrClosed    = errors.New("network: client closed")
	ErrNoHandler = errors.New("network: no envelope handler registered")
)

// EnvelopeFunc is the full-control environment: given one inbound envelope,
// produce the reply text ("" = silent round). Runs on the client's pump
// goroutine.
type EnvelopeFunc func(env Envelope) (string, error)

// MessageFunc is the simple agent bridge: rendered envelope text in, reply
// text out. AGO-DEVIATION vs the spec: Python's AgentClient calls
// Agent.ask directly; AGo's network package must not depend on the agent
// package (the ARCHITECTURE graph allows it, the brief forbids it), so the
// caller bridges the agent through this callback — typically
// func(msg) { return agent.Ask(ctx, msg).Body() } after converting msg into
// a ModelRequest.
type MessageFunc func(msg string) (string, error)

// RoundFunc builds a full routing round (text + tool-driven intent) from an
// inbound envelope — the workflow-channel handler.
type RoundFunc func(env Envelope) (Round, error)

// HubClient is the tenant-side handle to one hub: it registers agents and
// humans and owns their delivery plumbing (one connection per process,
// in-process dispatch).
type HubClient struct {
	hub *Hub

	mu     sync.Mutex
	agents map[string]*AgentClient
	humans map[string]*HumanClient
}

// NewHubClient binds a client facade to a hub.
func NewHubClient(hub *Hub) *HubClient {
	return &HubClient{
		hub:    hub,
		agents: make(map[string]*AgentClient),
		humans: make(map[string]*HumanClient),
	}
}

// Hub exposes the underlying hub (for channel/WAL access).
func (c *HubClient) Hub() *Hub { return c.hub }

// AgentOption configures Register.
type AgentOption func(*agentOptions)

type agentOptions struct {
	rule    *Rule
	resume  *Resume
	kind    PassportKind
	env     EnvelopeFunc
	msg     MessageFunc
	round   RoundFunc
	inbox   int
	autoAck bool
}

// WithAgentRule sets the registration rule (nil → DefaultRule).
func WithAgentRule(r Rule) AgentOption { return func(o *agentOptions) { o.rule = &r } }

// WithAgentResume attaches a capability claim.
func WithAgentResume(r Resume) AgentOption { return func(o *agentOptions) { o.resume = &r } }

// WithPassportKind overrides the passport kind (default agent).
func WithPassportKind(k PassportKind) AgentOption { return func(o *agentOptions) { o.kind = k } }

// WithEnvironment installs the full-control environment handler.
func WithEnvironment(fn EnvelopeFunc) AgentOption { return func(o *agentOptions) { o.env = fn } }

// WithAgentFunc installs the simple text bridge.
func WithAgentFunc(fn MessageFunc) AgentOption { return func(o *agentOptions) { o.msg = fn } }

// WithRoundFunc installs the workflow round handler (takes precedence over
// the other handlers).
func WithRoundFunc(fn RoundFunc) AgentOption { return func(o *agentOptions) { o.round = fn } }

// WithAgentInbox sets the delivery buffer (default 64). A full buffer applies
// the rule's inbox overflow policy (drop_oldest evicts, anything else drops
// the incoming envelope); every drop is counted (Dropped). Only a rule with an
// Inbox block (explicit opt-in) also reports drops through Err as
// ErrInboxFull; without one an overflow stays silent in Err, as before. A
// rule with Inbox.MaxPending > 0 sizes the buffer itself and overrides n.
func WithAgentInbox(n int) AgentOption { return func(o *agentOptions) { o.inbox = n } }

// WithAutoAck toggles invite auto-ack (default on).
func WithAutoAck(on bool) AgentOption { return func(o *agentOptions) { o.autoAck = on } }

// AgentClient is one registered agent: it auto-acks invites and runs the
// configured handler for substantive envelopes, posting the round result
// back into the channel.
type AgentClient struct {
	hub      *Hub
	Passport Passport
	ruleMu   sync.RWMutex
	rule     Rule
	opts     agentOptions
	feeds    channelFeeds

	inbox     chan Envelope
	done      chan struct{}
	closeOnce sync.Once
	// pending counts envelopes the pump has taken responsibility for and not
	// finished: incremented before the envelope is queued (so a delivery in
	// flight is visible), decremented after the handler returns or the
	// envelope is discarded. It is WaitIdle's signal: the queue length alone
	// leaves a window in which a pump that has just taken an envelope off the
	// queue looks idle.
	pending int64
	dropped int64 // atomic: envelopes discarded by the inbox overflow policy

	errMu   sync.Mutex
	lastErr error

	suppressMu sync.Mutex
	suppress   map[string]bool // channel id → the pump skips its envelopes
}

// SuppressHandler stops the pump from running the configured handler on
// envelopes of channelID (the delegate tool owns the channel's lifecycle
// for the duration of its consult). ResumeHandler undoes it.
func (a *AgentClient) SuppressHandler(channelID string) {
	a.suppressMu.Lock()
	defer a.suppressMu.Unlock()
	if a.suppress == nil {
		a.suppress = make(map[string]bool)
	}
	a.suppress[channelID] = true
}

// ResumeHandler lifts a SuppressHandler. Unknown ids are a no-op.
func (a *AgentClient) ResumeHandler(channelID string) {
	a.suppressMu.Lock()
	defer a.suppressMu.Unlock()
	delete(a.suppress, channelID)
}

func (a *AgentClient) suppressed(channelID string) bool {
	a.suppressMu.Lock()
	defer a.suppressMu.Unlock()
	return a.suppress[channelID]
}

// Register stamps a passport, binds delivery and starts the client pump.
func (c *HubClient) Register(name string, opts ...AgentOption) (*AgentClient, error) {
	o := agentOptions{autoAck: true, inbox: 64}
	for _, opt := range opts {
		opt(&o)
	}
	passport, err := c.hub.Register(Passport{Name: name, Kind: o.kind}, o.rule)
	if err != nil {
		return nil, err
	}
	rule := DefaultRule()
	if o.rule != nil {
		rule = o.rule.Clone()
	}
	a := &AgentClient{
		hub:      c.hub,
		Passport: passport,
		rule:     rule,
		opts:     o,
		inbox:    make(chan Envelope, inboxCapacity(rule, o.inbox)),
		done:     make(chan struct{}),
	}
	if o.resume != nil {
		r := *o.resume
		r.AgentID = passport.AgentID
		if err := c.hub.SetResume(r); err != nil {
			return nil, err
		}
	}
	if err := c.hub.Attach(passport.AgentID, a.deliver); err != nil {
		return nil, err
	}
	go a.pump()
	c.mu.Lock()
	c.agents[passport.AgentID] = a
	c.mu.Unlock()
	return a, nil
}

// AgentID is the hub-stamped registration id.
func (a *AgentClient) AgentID() string { return a.Passport.AgentID }

// Rule is the agent's stored rule.
func (a *AgentClient) Rule() Rule {
	a.ruleMu.RLock()
	defer a.ruleMu.RUnlock()
	return a.rule.Clone()
}

// Dropped is the number of delivered envelopes a full inbox buffer has
// discarded. With an Inbox rule each drop is also surfaced through Err as
// ErrInboxFull; without one Dropped is the only signal.
func (a *AgentClient) Dropped() int64 { return atomic.LoadInt64(&a.dropped) }

// Err returns the last handler error, if any (the pump never abandons the
// client on a failed round) or, for an agent whose rule has an Inbox block,
// the last inbox overflow (ErrInboxFull).
func (a *AgentClient) Err() error {
	a.errMu.Lock()
	defer a.errMu.Unlock()
	return a.lastErr
}

// Send posts an envelope through the hub.
func (a *AgentClient) Send(env Envelope) (Envelope, error) {
	return a.hub.Send(env)
}

// CreateChannel opens a channel with this agent as creator.
func (a *AgentClient) CreateChannel(manifest ChannelManifest, invitees ...string) (*ChannelHandle, error) {
	// The queue for ch exists once the opened envelope reaches us (every
	// delivery creates it), so open → say → wait loses no fast reply.
	return a.hub.CreateChannel(manifest, append([]string{a.AgentID()}, invitees...))
}

// Say posts a raw text envelope — the participant's own words (the Python
// say-tool / Channel.send path), as opposed to Ask which runs the local
// agent round first.
func (a *AgentClient) Say(channelID string, text string) error {
	_, err := a.hub.Send(NewTextEnvelope(channelID, a.AgentID(), "", text))
	return err
}

// Ask runs the local round pipeline on msg and posts the round envelope
// (the kickoff path: the creator speaks first). Returns the reply text.
func (a *AgentClient) Ask(channelID string, msg string) (string, error) {
	round, err := a.runHandlers(Envelope{ChannelID: channelID, EventType: EnvText, Payload: mustJSON(map[string]string{"text": msg})})
	if err != nil {
		return "", err
	}
	if round.Empty() {
		return "", nil
	}
	if err := a.postRound(channelID, round, Envelope{}); err != nil {
		return "", err
	}
	return round.Text, nil
}

// WaitIdle blocks until every envelope delivered to this client has been
// handled: nothing queued, nothing in flight (test affordance for asynchronous
// dispatch). An envelope is counted from the moment the hub hands it to the
// pump until the handler returns, so there is no window in which a delivery is
// neither queued nor running and the wait would return early.
func (a *AgentClient) WaitIdle(ctx context.Context) error {
	for {
		if atomic.LoadInt64(&a.pending) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// Close detaches the client from the hub, unregisters it and stops the
// pump. Idempotent.
func (a *AgentClient) Close() error {
	a.closeOnce.Do(func() {
		close(a.done)
		_ = a.hub.Attach(a.AgentID(), nil)
		_ = a.hub.Unregister(a.AgentID())
	})
	return nil
}

// deliver is the hub-side synchronous notifier: invites are acked inline (so
// CreateChannel's handshake completes deterministically), substantive
// envelopes land in the pump inbox (an LLM turn must not block the sender).
func (a *AgentClient) deliver(env Envelope) {
	select {
	case <-a.done:
		return
	default:
	}
	a.feeds.push(env)
	if isProtocolEvent(env.EventType) {
		if env.EventType == EnvChannelInvite && a.opts.autoAck && env.SenderID != a.AgentID() {
			// An ack failure must not crash the agent: the hub times the
			// handshake out and closes the channel.
			_, _ = a.hub.Send(NewChannelAck(env, a.AgentID(), false, ""))
		}
		return
	}
	if isTaskEvent(env.EventType) {
		return
	}
	if a.suppressed(env.ChannelID) {
		a.hub.AckInbox(a.AgentID(), 1) // counted at dispatch, never queued
		return
	}
	atomic.AddInt64(&a.pending, 1)
	enqueueWithPolicy(a.inbox, env, a.Rule().Inbox.EffectiveOverflow(), a.discard)
}

// discard records an envelope the overflow policy threw away and frees its
// inbox slot at the hub.
func (a *AgentClient) discard(env Envelope) {
	atomic.AddInt64(&a.dropped, 1)
	atomic.AddInt64(&a.pending, -1)
	if a.Rule().Inbox != (InboxBlock{}) { // explicit opt-in; no Inbox rule keeps the old silent drop
		a.errMu.Lock()
		a.lastErr = fmt.Errorf("%w: agent %s discarded envelope %s on channel %s",
			ErrInboxFull, a.AgentID(), env.ID, env.ChannelID)
		a.errMu.Unlock()
	}
	a.hub.AckInbox(a.AgentID(), 1)
}

// inboxCapacity sizes a delivery queue: the rule's MaxPending when set (the
// rule is the capacity policy), else the client option.
func inboxCapacity(rule Rule, fallback int) int {
	if rule.Inbox.MaxPending > 0 {
		return rule.Inbox.MaxPending
	}
	return fallback
}

// enqueueWithPolicy pushes env onto q. A full queue applies overflow:
// drop_oldest evicts the head and retries, every other policy drops env
// itself; discard sees each dropped envelope.
func enqueueWithPolicy(q chan Envelope, env Envelope, overflow string, discard func(Envelope)) {
	for {
		select {
		case q <- env:
			return
		default:
		}
		if overflow != OverflowDropOldest {
			discard(env)
			return
		}
		select {
		case old := <-q:
			discard(old)
		default:
		}
	}
}

func (a *AgentClient) pump() {
	for {
		select {
		case env := <-a.inbox:
			a.process(env)
			a.hub.AckInbox(a.AgentID(), 1)
			atomic.AddInt64(&a.pending, -1)
		case <-a.done:
			return
		}
	}
}

// process routes one substantive envelope through the handler chain and
// posts the resulting round envelope. No terminal-state recheck here: the
// hub's adapter gates and channel state machine reject stale replies
// deterministically (a rejected reply surfaces through Err).
func (a *AgentClient) process(env Envelope) {
	// Idempotent under at-least-once redelivery: a reply to this very
	// envelope already landed, so the turn is not run (and posted) twice.
	if env.ID != "" {
		if _, done := a.hub.FindEnvelopeByCausation(env.ChannelID, a.AgentID(), env.ID); done {
			return
		}
	}
	round, err := a.runHandlers(env)
	if err != nil {
		a.failTurn(env, err)
		return
	}
	if round.Empty() {
		return
	}
	if err := a.postRound(env.ChannelID, round, env); err != nil {
		a.failTurn(env, err)
	}
}

// failTurn records a failed turn: Err() keeps the last error and the hub
// fans OnTurnFailed out to its listeners. The channel stays alive and no
// reply is posted.
func (a *AgentClient) failTurn(env Envelope, err error) {
	a.errMu.Lock()
	a.lastErr = err
	a.errMu.Unlock()
	a.hub.ReportTurnFailure(env.ChannelID, a.AgentID(), env.ID, err)
}

// runHandlers resolves the handler precedence (round > envelope > message)
// and runs it once.
func (a *AgentClient) runHandlers(env Envelope) (Round, error) {
	switch {
	case a.opts.round != nil:
		return a.opts.round(env)
	case a.opts.env != nil:
		text, err := a.opts.env(env)
		return Round{Text: text}, err
	case a.opts.msg != nil:
		text, err := a.opts.msg(renderInbound(env))
		return Round{Text: text}, err
	default:
		return Round{}, nil
	}
}

// postRound encodes round into the channel's protocol shape (packets for
// workflow channels and intent-carrying rounds, plain text otherwise) and
// posts it with causation + delegation depth from the triggering envelope.
func (a *AgentClient) postRound(channelID string, round Round, trigger Envelope) error {
	ch, err := a.hub.Channel(channelID)
	if err != nil {
		return err
	}
	var env Envelope
	if ch.AdapterType() == TypeWorkflow || round.Handoff != nil || round.Finish != nil {
		var opts []PacketOption
		switch {
		case round.Finish != nil:
			opts = append(opts, WithFinish(round.Finish))
		case round.Handoff != nil:
			opts = append(opts, WithHandoff(&Handoff{
				Target: a.resolveTarget(round.Handoff.Target),
				Reason: round.Handoff.Reason,
			}))
		default:
			opts = append(opts, WithPacketRouting(map[string]any{"kind": "text"}))
		}
		env = NewPacketEnvelope(channelID, a.AgentID(), "", round.Text, opts...)
	} else {
		env = NewTextEnvelope(channelID, a.AgentID(), "", round.Text)
	}
	env.CausationID = trigger.ID
	env.Depth = ReplyDepth(trigger)
	_, err = a.hub.Send(env)
	return err
}

// resolveTarget maps a Handoff target (a Passport name by convention) to the
// hub-stamped agent id; exact ids and unknown names pass through unchanged
// (Python: name_to_id resolution at packet-finalize).
func (a *AgentClient) resolveTarget(nameOrID string) string {
	if p, err := a.hub.Passport(nameOrID); err == nil {
		return p.AgentID
	}
	return nameOrID
}

// renderInbound projects an envelope into the text an agent reads (the
// packet shape: handoff signal line + body).
func renderInbound(env Envelope) string {
	switch env.EventType {
	case EnvPacket:
		p := decodePacket(env)
		var lines []string
		if p.Routing["kind"] == "handoff" {
			tool := "handoff"
			if s, ok := p.Routing["tool"].(string); ok && s != "" {
				tool = s
			}
			reason, _ := p.Routing["reason"].(string)
			if reason != "" {
				lines = append(lines, fmt.Sprintf("[Handed off via %s] %s", tool, reason))
			} else {
				lines = append(lines, fmt.Sprintf("[Handed off via %s]", tool))
			}
		}
		if p.Body != "" {
			lines = append(lines, p.Body)
		}
		return strings.Join(lines, "\n")
	default:
		return env.Text()
	}
}

// --- human client -------------------------------------------------------------

// HumanOption configures RegisterHuman.
type HumanOption func(*humanOptions)

type humanOptions struct {
	rule    *Rule
	autoAck bool
	inbox   int
}

// WithHumanRule sets the registration rule (nil → DefaultRule).
func WithHumanRule(r Rule) HumanOption { return func(o *humanOptions) { o.rule = &r } }

// WithHumanAutoAck toggles invite auto-ack (default on — without it a
// channel opened to a human stalls until the UI acks).
func WithHumanAutoAck(on bool) HumanOption { return func(o *humanOptions) { o.autoAck = on } }

// WithHumanInbox sets the pull-queue capacity (default 128). The queue also
// holds protocol envelopes, which the rule's inbox cap does not count, so it
// is never smaller than Inbox.MaxPending; a full queue applies the rule's
// overflow policy.
func WithHumanInbox(n int) HumanOption { return func(o *humanOptions) { o.inbox = n } }

// HumanClient is a registered non-LLM participant: every inbound envelope is
// pushed to the inbox (pull via Recv / Inbox) and invites are auto-acked by
// default.
type HumanClient struct {
	hub      *Hub
	Passport Passport
	rule     Rule

	inbox     chan Envelope
	done      chan struct{}
	closeOnce sync.Once

	feeds     channelFeeds
	cbMu      sync.Mutex
	callbacks []*envelopeCallback
}

// RegisterHuman stamps a passport and binds human delivery.
func (c *HubClient) RegisterHuman(name string, opts ...HumanOption) (*HumanClient, error) {
	o := humanOptions{autoAck: true, inbox: 128}
	for _, opt := range opts {
		opt(&o)
	}
	passport, err := c.hub.Register(Passport{Name: name, Kind: KindHuman}, o.rule)
	if err != nil {
		return nil, err
	}
	rule := DefaultRule()
	if o.rule != nil {
		rule = o.rule.Clone()
	}
	h := &HumanClient{
		hub:      c.hub,
		Passport: passport,
		rule:     rule,
		inbox:    make(chan Envelope, max(o.inbox, rule.Inbox.MaxPending)),
		done:     make(chan struct{}),
	}
	if err := c.hub.Attach(passport.AgentID, h.deliver); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.humans[passport.AgentID] = h
	c.mu.Unlock()
	return h, nil
}

// AgentID is the hub-stamped registration id.
func (h *HumanClient) AgentID() string { return h.Passport.AgentID }

// Inbox exposes the pull queue (every inbound envelope, unfiltered). Reading
// it directly bypasses inbox accounting: when the rule sets Inbox.MaxPending,
// consume through Recv (or call Hub.AckInbox yourself) so capacity frees up.
func (h *HumanClient) Inbox() <-chan Envelope { return h.inbox }

// Recv waits for the next inbound envelope; fails with ErrClosed once the
// client is closed. A received substantive envelope frees its inbox slot.
func (h *HumanClient) Recv(ctx context.Context) (Envelope, error) {
	select {
	case env := <-h.inbox:
		if substantive(env) {
			h.hub.AckInbox(h.AgentID(), 1)
		}
		return env, nil
	case <-ctx.Done():
		return Envelope{}, ctx.Err()
	case <-h.done:
		return Envelope{}, ErrClosed
	}
}

// Send posts an envelope through the hub.
func (h *HumanClient) Send(env Envelope) (Envelope, error) { return h.hub.Send(env) }

// Reply posts a text answer causally linked to the triggering envelope.
func (h *HumanClient) Reply(trigger Envelope, text string) (Envelope, error) {
	env := NewTextEnvelope(trigger.ChannelID, h.AgentID(), "", text)
	env.CausationID = trigger.ID
	env.Depth = ReplyDepth(trigger)
	return h.hub.Send(env)
}

// CreateChannel opens a channel with this human as creator.
func (h *HumanClient) CreateChannel(manifest ChannelManifest, invitees ...string) (*ChannelHandle, error) {
	return h.hub.CreateChannel(manifest, append([]string{h.AgentID()}, invitees...))
}

// CloseChannel closes a channel the human participates in.
func (h *HumanClient) CloseChannel(channelID string, reason string) error {
	return h.hub.CloseChannel(channelID, reason)
}

// Close unregisters the human and wakes Recv. Idempotent.
func (h *HumanClient) Close() {
	h.closeOnce.Do(func() {
		close(h.done)
		_ = h.hub.Attach(h.AgentID(), nil)
		_ = h.hub.Unregister(h.AgentID())
	})
}

// deliver auto-acks invites (before fanning out, so the channel reaches
// active before the UI sees the invite) and pushes every envelope —
// protocol lifecycle included — to the pull queue (Python parity).
func (h *HumanClient) deliver(env Envelope) {
	select {
	case <-h.done:
		return
	default:
	}
	h.feeds.push(env)
	h.runCallbacks(env)
	if env.EventType == EnvChannelInvite && env.SenderID != h.AgentID() {
		_, _ = h.hub.Send(NewChannelAck(env, h.AgentID(), false, ""))
	}
	enqueueWithPolicy(h.inbox, env, h.rule.Inbox.EffectiveOverflow(), func(dropped Envelope) {
		if substantive(dropped) {
			h.hub.AckInbox(h.AgentID(), 1)
		}
	})
}

// AddExample appends an example task to this agent's resume. The hub does the
// read-modify-write atomically, so concurrent SetResume or observation
// updates are not clobbered.
func (a *AgentClient) AddExample(ex ResumeExample) error {
	return a.hub.AddResumeExample(a.AgentID(), ex)
}
