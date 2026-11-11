package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/luckystrker/ago/internal/uuid"
	"github.com/luckystrker/ago/network/transport"
	"github.com/luckystrker/ago/task"
)

// wireState is the hub's endpoint registry: one wireEndpoint per served
// connection, and the connection each registered agent is currently held by.
// Guarded by its own mutex; no hub method is called while it is held.
type wireState struct {
	mu        sync.Mutex
	endpoints map[string]*wireEndpoint // endpoint id → endpoint
	agentEP   map[string]*wireEndpoint // agent id → the endpoint holding it
	nextGen   uint64
	warnOnce  sync.Once // the NoAuth warning is logged once per hub
}

// wireEndpoint is the hub-side handle to one connection (Python's
// LinkEndpoint). The hub creates it when it starts serving a link and gives it
// its id: identity belongs to the connection object, never to anything a
// frame says about itself, so one connection cannot act through another's
// bindings. It lives exactly as long as ServeLink serves its link; a
// reconnect is a new link and therefore a new endpoint.
type wireEndpoint struct {
	id    string // hub-minted; not derived from any frame
	gen   uint64 // generation: increases with every endpoint the hub creates
	link  transport.Link
	bound map[string]bool // agents this connection acts as (guarded by wireState.mu)
	since map[string]HelloSince
	// senders holds one ordered sender per bound agent (guarded by wireState.mu;
	// see delivery.go).
	senders map[string]*agentSender
}

// wireCtx is what request handling needs besides the frame.
type wireCtx struct {
	ctx context.Context
	ep  *wireEndpoint
}

var taskIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

var mirrorFailureKeys = map[string]bool{"op": true, "owner_id": true, "channel_id": true, "exc_type": true, "exc_message": true}

// Acceptor yields the server end of each new connection (a transport.MemListener,
// a WebSocket server, ...).
type Acceptor interface {
	Accept(ctx context.Context) (transport.Link, error)
}

// Serve accepts connections from l and serves each on its own goroutine with
// ServeLink until ctx is done (nil) or Accept fails (that error).
func (h *Hub) Serve(ctx context.Context, l Acceptor) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		link, err := l.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, transport.ErrNoConnection) {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = h.ServeLink(ctx, link)
		}()
	}
}

// ServeLink serves ONE connection: l is the hub's end of a link that carries a
// single client. The endpoint — and with it the set of agents the client may
// act as — belongs to this call; nothing in a frame (Frame.From, Frame.To, ids
// in payloads) selects or changes it. Requests are executed concurrently (a
// create_channel blocks on invite acks that arrive as further requests of the
// same connection); notifies for agents bound to the connection are pushed
// through l. When the link ends the endpoint is dropped and its agents are
// released.
//
// A connection acts only as the agents bound to it — by a register op or an
// authenticated hello. Operations for any other agent are refused with
// access_denied. ServeLink returns nil when ctx ends or the link closes,
// otherwise the Recv error.
func (h *Hub) ServeLink(ctx context.Context, l transport.Link) error {
	ep := h.newEndpoint(l)
	defer h.dropEndpoint(ep)
	defer l.Close() // the connection is over when this call returns: the client sees it
	var wg sync.WaitGroup
	defer wg.Wait()
	wc := wireCtx{ctx: ctx, ep: ep}
	for {
		f, err := l.Recv(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, transport.ErrLinkClosed) {
				return nil
			}
			return err
		}
		switch f.Kind {
		case transport.FrameRequest:
			wg.Add(1)
			go func() {
				defer wg.Done()
				h.handleRequest(wc, f)
			}()
		case transport.FrameHello:
			h.handleHello(wc, f)
		case transport.FrameReceipt:
			h.handleReceipt(wc, f)
		case transport.FramePing:
			_ = l.Send(transport.Frame{Kind: transport.FramePong})
		}
	}
}

func (h *Hub) newEndpoint(l transport.Link) *wireEndpoint {
	w := &h.ext.wire
	w.warnOnce.Do(func() {
		for _, scheme := range h.ext.auth.Schemes() {
			if scheme == AuthSchemeNone {
				slog.Warn("network: this hub is served over a transport and accepts the \"none\" auth scheme: " +
					"any client that knows an agent's name can take that agent over with a hello " +
					"(serve with a registry without NoAuth, or only on a trusted transport)")
			}
		}
	})
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.endpoints == nil {
		w.endpoints = make(map[string]*wireEndpoint)
		w.agentEP = make(map[string]*wireEndpoint)
	}
	w.nextGen++
	ep := &wireEndpoint{id: uuid.New(), gen: w.nextGen, link: l, bound: make(map[string]bool), since: make(map[string]HelloSince), senders: make(map[string]*agentSender)}
	w.endpoints[ep.id] = ep
	return ep
}

// bindEndpoint makes ep the connection that holds agentID: delivery routes
// through it and an earlier holder loses the binding (Python bind_endpoint:
// the old connection stays up for its other agents).
func (h *Hub) bindEndpoint(ep *wireEndpoint, agentID string, since HelloSince) error {
	w := &h.ext.wire
	w.mu.Lock()
	if w.endpoints[ep.id] != ep {
		w.mu.Unlock()
		return fmt.Errorf("%w: endpoint %s is gone", ErrNotFound, ep.id)
	}
	if prior := w.agentEP[agentID]; prior != nil && prior != ep {
		delete(prior.bound, agentID)
		delete(prior.since, agentID)
		delete(prior.senders, agentID)
	}
	ep.bound[agentID] = true
	ep.senders[agentID] = newAgentSender(since) // a (re)bind starts a fresh walk: replay from the mark, no stall
	w.agentEP[agentID] = ep
	w.mu.Unlock()
	return h.Attach(agentID, h.wireNotifier(ep, agentID))
}

// unbindAgent forgets agentID's binding (after an unregister).
func (h *Hub) unbindAgent(agentID string) {
	w := &h.ext.wire
	w.mu.Lock()
	if ep := w.agentEP[agentID]; ep != nil {
		delete(ep.bound, agentID)
		delete(ep.since, agentID)
		delete(ep.senders, agentID)
	}
	delete(w.agentEP, agentID)
	w.mu.Unlock()
}

// dropEndpoint removes a dead connection and releases its agents (they are
// re-bound by their next hello or register).
func (h *Hub) dropEndpoint(ep *wireEndpoint) {
	w := &h.ext.wire
	w.mu.Lock()
	if w.endpoints[ep.id] != ep {
		w.mu.Unlock()
		return
	}
	delete(w.endpoints, ep.id)
	var released []string
	for id := range ep.bound {
		if w.agentEP[id] == ep {
			delete(w.agentEP, id)
			released = append(released, id)
		}
	}
	ep.bound = make(map[string]bool)
	ep.senders = make(map[string]*agentSender)
	w.mu.Unlock()
	for _, id := range released {
		_ = h.Attach(id, nil)
	}
}

// isBound reports whether the connection acts as agentID.
func (h *Hub) isBound(ep *wireEndpoint, agentID string) bool {
	w := &h.ext.wire
	w.mu.Lock()
	defer w.mu.Unlock()
	return ep.bound[agentID]
}

// boundAgents lists the agents the connection holds, sorted.
func (h *Hub) boundAgents(ep *wireEndpoint) []string {
	w := &h.ext.wire
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, 0, len(ep.bound))
	for id := range ep.bound {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// wireNotifier is the dispatch hook of a wire-bound agent. The envelope that
// triggered it is already in the channel log, so the hook only runs the agent's
// ordered sender (pump), which delivers it — and anything older still pending —
// in log order.
func (h *Hub) wireNotifier(ep *wireEndpoint, agentID string) Notifier {
	return func(env Envelope) {
		if s := h.senderOf(ep, agentID); s != nil {
			s.markLive(env)
		}
		h.pump(ep, agentID)
	}
}

func (wc wireCtx) reply(f transport.Frame) {
	_ = wc.ep.link.Send(f)
}

func (wc wireCtx) fail(requestID string, err error) {
	wc.reply(transport.Frame{Kind: transport.FrameResponse, RequestID: requestID, Code: wireErrorCode(err), Message: err.Error()})
}

// --- hello ------------------------------------------------------------------------------------

func (h *Hub) handleHello(wc wireCtx, f transport.Frame) {
	reject := func(err error) {
		wc.reply(transport.Frame{Kind: transport.FrameError, RequestID: f.RequestID, Code: wireErrorCode(err), Message: err.Error()})
	}
	body, err := parseHello(f.Payload)
	if err != nil {
		reject(err)
		return
	}
	stored, err := h.Passport(body.Name)
	if err != nil || stored.Name != body.Name {
		reject(fmt.Errorf("%w: unknown name: %s", ErrNotFound, body.Name))
		return
	}
	// AGO-DEVIATION: Python lets a remote_agent re-attach on a connection that
	// holds one of its owners; AGo never registers a remote_agent over the wire
	// (no owner bookkeeping), so a hello for one is always refused.
	if stored.Kind == KindRemoteAgent {
		reject(fmt.Errorf("%w: remote agent %q re-attaches only alongside its owner", ErrAuth, body.Name))
		return
	}
	scheme, _ := authSchemeAndClaim(stored)
	// WARNING: for a NoAuth identity ("none" scheme) this check is the whole
	// authentication: anyone who names it here takes it over — the connection is
	// bound to it and the previous holder loses it (Python parity). Only
	// credentialed identities (WithHubAuth) are protected against that.
	//
	// AGO-DEVIATION: Python's HelloFrame defaults auth_scheme to "none"; the
	// AGo hello omits an empty field, so "" is read as "none" (NoAuth).
	want := body.AuthScheme
	if want == "" {
		want = AuthSchemeNone
	}
	if want != scheme {
		reject(fmt.Errorf("%w: auth scheme %q does not match %q's registered scheme %q", ErrAuth, want, body.Name, scheme))
		return
	}
	if err := h.Authenticate(wc.ctx, stored, body.AuthClaim); err != nil {
		reject(err)
		return
	}
	// AGO-DEVIATION: Python replays only when the hello carries
	// since_envelope_id (None skips replay). AGo always replays from
	// max(persisted cursor, since mark): a hello without marks means "from my
	// cursor", otherwise an agent that reconnects without remembering anything
	// would silently lose what arrived while it was offline (invariant 1).
	since := body.SinceMarks()
	if err := h.bindEndpoint(wc.ep, stored.AgentID, since); err != nil {
		reject(err)
		return
	}
	w := &h.ext.wire
	w.mu.Lock()
	if w.agentEP[stored.AgentID] == wc.ep {
		wc.ep.since[stored.AgentID] = since
	}
	w.mu.Unlock()
	payload, _ := json.Marshal(WelcomeBody{EndpointID: wc.ep.id, HubTime: h.now().UTC().Format(time.RFC3339Nano)})
	wc.reply(transport.Frame{Kind: transport.FrameWelcome, RequestID: f.RequestID, Payload: payload})
	h.pump(wc.ep, stored.AgentID) // replay: strictly after the mark, in log order
}

// --- receipts ----------------------------------------------------------------------------------

// handleReceipt processes a delivery receipt. Receipts for an agent this
// connection does not hold, or with missing fields, are dropped silently
// (Python _handle_receipt): an ack the hub cannot attribute has nothing to
// advance, and a connection only acks deliveries for the agents it holds.
func (h *Hub) handleReceipt(wc wireCtx, f transport.Frame) {
	if f.RecipientID == "" || !h.isBound(wc.ep, f.RecipientID) {
		return
	}
	switch f.Code {
	case ReceiptAck:
		if f.EnvelopeID == "" || f.ChannelID == "" {
			return
		}
		// The cursor only ever names an envelope the channel holds and the
		// recipient was sent (wave-4 model: CursorBacked); an ack for anything
		// else has nothing to advance.
		env, ok := h.envelopeByID(f.ChannelID, f.EnvelopeID)
		// Python moves the cursor for any envelope the recipient names. AGo also
		// refuses an ack for the recipient's own envelope — it never received
		// it — except lifecycle broadcasts, which address the creator too (so a
		// creator that acked `opened` is not sent it again after a reconnect).
		if !ok || (env.SenderID == f.RecipientID && !isLifecycleEvent(env.EventType)) {
			return
		}
		if ch, err := h.Channel(f.ChannelID); err != nil || !ch.hasParticipant(f.RecipientID) {
			return
		}
		_ = h.AdvanceInboxCursor(f.RecipientID, f.ChannelID, f.EnvelopeID)
		// A delivered substantive envelope no longer counts against the
		// recipient's inbox capacity.
		if substantive(env) {
			h.AckInbox(f.RecipientID, 1)
		}
		h.pump(wc.ep, f.RecipientID) // flow control: a drained link takes the next entries
	case ReceiptNack:
		_ = h.RecordNack(f.RecipientID, f.ChannelID, f.EnvelopeID, f.Message)
		// AGO-DEVIATION: after a nack AGo stops delivering that channel to the
		// connection until it re-binds (model D19). Python leaves the cursor
		// but keeps delivering, so a later ack of a newer envelope would move
		// the cursor past the nacked one and it would never be replayed.
		h.stallChannel(wc.ep, f.RecipientID, f.ChannelID)
		if env, ok := h.envelopeByID(f.ChannelID, f.EnvelopeID); ok {
			reason := fmt.Errorf("network: recipient %s nacked envelope %s: %s", f.RecipientID, f.EnvelopeID, f.Message)
			h.fanOut(func(l HubListener) { l.OnDispatchFailed(env, f.RecipientID, reason) })
		}
	}
}

// --- requests -----------------------------------------------------------------------------------

func (h *Hub) handleRequest(wc wireCtx, f transport.Frame) {
	if f.RequestID == "" || f.Op == "" {
		wc.reply(transport.Frame{Kind: transport.FrameError, Code: CodeProtocol, Message: "malformed request frame: request_id and op are required"})
		return
	}
	var params json.RawMessage = f.Payload
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	result, err := h.dispatchOp(wc, f.Op, params)
	if err != nil {
		wc.fail(f.RequestID, err)
		return
	}
	var payload []byte
	if result != nil {
		payload, err = json.Marshal(result)
		if err != nil {
			wc.fail(f.RequestID, fmt.Errorf("network: encode result of %s: %w", f.Op, err))
			return
		}
	}
	wc.reply(transport.Frame{Kind: transport.FrameResponse, RequestID: f.RequestID, Payload: payload})
}

func decodeParams(raw json.RawMessage, dst any) error {
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("%w: malformed params: %v", ErrProtocol, err)
	}
	return nil
}

func limitOr(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

// wirePassport is the wire form of a passport: never carries an auth claim.
func wirePassport(p Passport) Passport {
	if p.Kind == KindRemoteAgent {
		return withoutClaim(p)
	}
	return p
}

// wireChannel is the wire form of a channel: its persisted snapshot.
func wireChannel(ch *ChannelHandle) ChannelSnapshot {
	return *channelSnapshot(ch).Channel
}

// dispatchOp authorizes and executes one control-plane op. Every op of
// Python's _dispatch_request_op is served here on the matching Hub method.
func (h *Hub) dispatchOp(wc wireCtx, op string, raw json.RawMessage) (any, error) {
	ep := wc.ep
	switch op {
	// ── registration / identity ────────────────────────────────────────
	case OpRegister:
		var p registerParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if p.Passport.Kind == KindRemoteAgent {
			return nil, fmt.Errorf("%w: this hub does not accept remote_agent registration over the wire", ErrAccessDenied)
		}
		reg, err := h.Register(p.Passport, p.Rule)
		if err != nil {
			return nil, err
		}
		if p.Resume != nil {
			r := *p.Resume
			r.AgentID = reg.AgentID
			if err := h.SetResume(r); err != nil {
				_ = h.Unregister(reg.AgentID)
				return nil, err
			}
		}
		if err := h.bindEndpoint(ep, reg.AgentID, HelloSince{}); err != nil {
			_ = h.Unregister(reg.AgentID)
			return nil, err
		}
		return wirePassport(reg), nil
	case OpGetAgent:
		var p nameOrIDParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		pass, err := h.Passport(p.NameOrID)
		if err != nil {
			return nil, err
		}
		return wirePassport(pass), nil
	case OpGetResume:
		var p agentIDParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		return h.ResumeOf(p.AgentID)
	case OpGetSkill:
		var p agentIDParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if _, err := h.Passport(p.AgentID); err != nil {
			return nil, err
		}
		// AGO-DEVIATION: Python returns the agent's SKILL.md; AGo has no skill
		// registry, so get_skill answers null for every registered agent.
		return nil, nil
	case OpGetRule:
		var p agentIDParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		return h.RuleOf(p.AgentID)
	case OpFindAgentID:
		var p nameParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		pass, err := h.Passport(p.Name)
		if err != nil || pass.Name != p.Name {
			return nil, nil
		}
		return pass.AgentID, nil
	case OpNamesFor:
		var p agentIDsParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		out := make(map[string]string, len(p.AgentIDs))
		for _, id := range p.AgentIDs {
			out[id] = h.NameFor(id)
		}
		return out, nil
	case OpListAgents:
		var p listAgentsParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		agents := h.QueryAgents(AgentQuery{Capability: p.Capability, Query: p.Query, Kind: PassportKind(p.Kind), SortBy: p.SortBy, Limit: limitOr(p.Limit, 50)})
		out := make([]Passport, len(agents))
		for i, a := range agents {
			out[i] = wirePassport(a)
		}
		return out, nil
	case OpSetResume:
		var p setResumeParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if err := h.requireBound(ep, p.AgentID); err != nil {
			return nil, err
		}
		p.Resume.AgentID = p.AgentID
		return nil, h.SetResume(p.Resume)
	case OpSetSkill:
		var p setSkillParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if err := h.requireBound(ep, p.AgentID); err != nil {
			return nil, err
		}
		// AGO-DEVIATION: Python stores or clears the agent's SKILL.md; AGo has
		// no skill registry, so only the clearing form (null) is accepted — a
		// no-op — and setting a skill is a protocol error.
		if p.SkillMD != nil {
			return nil, fmt.Errorf("%w: this hub has no skill registry", ErrProtocol)
		}
		return nil, nil
	case OpSetRule:
		var p setRuleParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if err := h.requireBound(ep, p.AgentID); err != nil {
			return nil, err
		}
		return nil, h.SetRule(p.AgentID, p.Rule)
	case OpAddResumeExample:
		var p addResumeExampleParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if err := h.requireBound(ep, p.AgentID); err != nil {
			return nil, err
		}
		return nil, h.AddResumeExample(p.AgentID, p.Example)
	case OpUnregister:
		var p agentIDParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if err := h.requireBound(ep, p.AgentID); err != nil {
			return nil, err
		}
		if err := h.Unregister(p.AgentID); err != nil {
			return nil, err
		}
		h.unbindAgent(p.AgentID)
		return nil, nil

	// ── channels ───────────────────────────────────────────────────────
	case OpCreateChannel:
		var p createChannelParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if len(p.Participants) == 0 {
			return nil, fmt.Errorf("%w: channel requires at least one participant", ErrProtocol)
		}
		if err := h.requireBound(ep, p.Participants[0]); err != nil {
			return nil, err
		}
		ch, err := h.CreateChannel(p.Manifest, p.Participants)
		if err != nil {
			return nil, err
		}
		return wireChannel(ch), nil
	case OpGetChannel:
		var p channelIDParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		ch, err := h.requireParticipant(ep, p.ChannelID)
		if err != nil {
			return nil, err
		}
		return wireChannel(ch), nil
	case OpListChannels:
		var p listChannelsParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		holders := h.boundAgents(ep)
		if p.AgentID != "" {
			if err := h.requireBound(ep, p.AgentID); err != nil {
				return nil, err
			}
			holders = []string{p.AgentID}
		}
		out := []ChannelSnapshot{}
		for _, ch := range h.ListChannels() {
			for _, id := range holders {
				if ch.hasParticipant(id) {
					out = append(out, wireChannel(ch))
					break
				}
			}
		}
		sort.Slice(out, func(i, j int) bool {
			if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
				return out[i].CreatedAt.Before(out[j].CreatedAt)
			}
			return out[i].ID < out[j].ID
		})
		if lim := limitOr(p.Limit, 50); len(out) > lim {
			out = out[:lim]
		}
		return out, nil
	case OpCloseChannel:
		var p closeChannelParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if _, err := h.requireParticipant(ep, p.ChannelID); err != nil {
			return nil, err
		}
		if err := h.CloseChannel(p.ChannelID, p.Reason); err != nil {
			return nil, err
		}
		ch, err := h.Channel(p.ChannelID)
		if err != nil {
			return nil, err
		}
		return wireChannel(ch), nil
	case OpPostEnvelope:
		var p postEnvelopeParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if err := h.authorizePost(ep, p.Envelope); err != nil {
			return nil, err
		}
		return h.Send(p.Envelope)
	case OpReadWAL:
		var p readWALParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if _, err := h.requireParticipant(ep, p.ChannelID); err != nil {
			return nil, err
		}
		return h.ReadWALRange(p.ChannelID, p.Since, limitOr(p.Until, -1))
	case OpFindEnvelopeByCausation:
		var p causationParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if _, err := h.requireParticipant(ep, p.ChannelID); err != nil {
			return nil, err
		}
		if env, ok := h.FindEnvelopeByCausation(p.ChannelID, p.SenderID, p.CausationID); ok {
			return env, nil
		}
		return nil, nil
	case OpCanSend:
		var p canSendParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if err := h.requireBound(ep, p.SenderID); err != nil {
			return nil, err
		}
		return h.CanSend(p.ChannelID, p.SenderID, p.EventType), nil
	case OpPendingTurnsFor:
		var p agentIDParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if err := h.requireBound(ep, p.AgentID); err != nil {
			return nil, err
		}
		turns := h.PendingTurnsFor(p.AgentID)
		if turns == nil {
			turns = []PendingTurn{}
		}
		return turns, nil
	case OpReportTurnFailure:
		var p turnFailureParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if err := h.requireBound(ep, p.AgentID); err != nil {
			return nil, err
		}
		ch, err := h.Channel(p.ChannelID)
		if err != nil || !ch.hasParticipant(p.AgentID) {
			return nil, fmt.Errorf("%w: agent is not a participant of channel %q", ErrAccessDenied, p.ChannelID)
		}
		h.ReportTurnFailure(p.ChannelID, p.AgentID, p.EnvelopeID, errors.New(p.Error))
		return nil, nil
	}
	return h.dispatchTaskOp(wc, op, raw)
}

func (h *Hub) dispatchTaskOp(wc wireCtx, op string, raw json.RawMessage) (any, error) {
	ep := wc.ep
	switch op {
	case OpGetTask:
		var p taskIDParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if err := checkTaskID(p.TaskID); err != nil {
			return nil, err
		}
		md, err := h.Task(p.TaskID)
		if err != nil {
			return nil, err
		}
		if !h.taskVisible(ep, md) {
			// Same answer as an unknown id, so ids cannot be probed.
			return nil, fmt.Errorf("%w: %s", ErrTaskNotFound, p.TaskID)
		}
		return md, nil
	case OpListTasks:
		var p listTasksParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		out := []task.Metadata{}
		for _, md := range h.Tasks(p.AgentID, false) {
			if p.ChannelID != "" && md.ChannelID != p.ChannelID {
				continue
			}
			if p.State != "" && string(md.State) != p.State {
				continue
			}
			if h.taskVisible(ep, md) {
				out = append(out, md)
			}
		}
		if lim := limitOr(p.Limit, 50); len(out) > lim {
			out = out[:lim]
		}
		return out, nil
	case OpObserveTask:
		var md task.Metadata
		if err := decodeParams(raw, &struct {
			Metadata *task.Metadata `json:"metadata"`
		}{&md}); err != nil {
			return nil, err
		}
		if err := checkTaskID(md.TaskID); err != nil {
			return nil, err
		}
		if err := h.requireBound(ep, md.OwnerID); err != nil {
			return nil, err
		}
		if md.ChannelID != "" {
			ch, err := h.Channel(md.ChannelID)
			if err != nil || !ch.hasParticipant(md.OwnerID) {
				return nil, fmt.Errorf("%w: task owner is not a participant of channel %q", ErrAccessDenied, md.ChannelID)
			}
		}
		if existing, err := h.Task(md.TaskID); err == nil {
			if err := h.requireBound(ep, existing.OwnerID); err != nil {
				return nil, err
			}
		}
		return nil, h.ObserveTask(md)
	case OpUpdateTask:
		var p updateTaskParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if err := checkTaskID(p.TaskID); err != nil {
			return nil, err
		}
		if len(h.boundAgents(ep)) == 0 {
			return nil, fmt.Errorf("%w: connection has no bound agent", ErrAccessDenied)
		}
		if err := h.requireTaskOwner(ep, p.TaskID); err != nil {
			return nil, err
		}
		return nil, h.UpdateTask(p.TaskID, task.State(p.State), p.Progress, p.Result, p.Error)
	case OpRecordObservation:
		var p recordObservationParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if p.TaskID != "" {
			if err := checkTaskID(p.TaskID); err != nil {
				return nil, err
			}
		}
		if err := h.requireBound(ep, p.OwnerID); err != nil {
			return nil, err
		}
		if p.TaskID != "" {
			if err := h.requireTaskOwner(ep, p.TaskID); err != nil {
				return nil, err
			}
		}
		latency := time.Duration(-1)
		if p.LatencyMS != nil {
			latency = time.Duration(*p.LatencyMS) * time.Millisecond
		}
		return nil, h.RecordObservation(p.OwnerID, p.Capability, task.State(p.Outcome), latency, p.TaskID)
	case OpFireTaskEvent:
		var p fireTaskEventParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if err := checkTaskID(p.TaskID); err != nil {
			return nil, err
		}
		if len(h.boundAgents(ep)) == 0 {
			return nil, fmt.Errorf("%w: connection has no bound agent", ErrAccessDenied)
		}
		if err := h.authorizeMirrorFailure(ep, p.TaskID, p.Kind, p.Payload); err != nil {
			return nil, err
		}
		payload := make(map[string]any, len(p.Payload))
		for k, v := range p.Payload {
			if s, ok := v.(string); ok && len(s) > maxWireText {
				v = s[:maxWireText]
			}
			payload[k] = v
		}
		h.FireTaskEvent(p.TaskID, p.Kind, payload)
		return nil, nil
	case OpCheckpointTask, OpReadTaskCheckpoint:
		var p checkpointParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if err := checkTaskID(p.TaskID); err != nil {
			return nil, err
		}
		if len(h.boundAgents(ep)) == 0 {
			return nil, fmt.Errorf("%w: connection has no bound agent", ErrAccessDenied)
		}
		if _, err := h.Task(p.TaskID); err == nil {
			if err := h.requireTaskOwner(ep, p.TaskID); err != nil {
				return nil, err
			}
		}
		writer, err := h.checkpointWriter(ep, p.WriterID)
		if err != nil {
			return nil, err
		}
		if op == OpCheckpointTask {
			return nil, h.CheckpointTask(writer, p.TaskID, p.State)
		}
		return h.ReadTaskCheckpoint(writer, p.TaskID)
	}
	return nil, fmt.Errorf("%w: unknown control-plane op: %q", ErrProtocol, op)
}

// --- authorization ---------------------------------------------------------------------------------

func checkTaskID(id string) error {
	if !taskIDRe.MatchString(id) {
		return fmt.Errorf("%w: invalid task_id: %q", ErrProtocol, id)
	}
	return nil
}

func (h *Hub) requireBound(ep *wireEndpoint, agentID string) error {
	if !h.isBound(ep, agentID) {
		return fmt.Errorf("%w: connection is not bound to agent %q", ErrAccessDenied, agentID)
	}
	return nil
}

// requireParticipant returns the channel when an agent the connection holds
// participates in it.
func (h *Hub) requireParticipant(ep *wireEndpoint, channelID string) (*ChannelHandle, error) {
	ch, err := h.Channel(channelID)
	if err != nil {
		return nil, err
	}
	for _, id := range h.boundAgents(ep) {
		if ch.hasParticipant(id) {
			return ch, nil
		}
	}
	return nil, fmt.Errorf("%w: connection has no participant in channel %q", ErrAccessDenied, channelID)
}

// authorizePost admits a posted envelope: the sender is held by the
// connection, hub-only protocol events are refused, and the audience is a
// participant of the channel.
func (h *Hub) authorizePost(ep *wireEndpoint, env Envelope) error {
	if err := checkTaskIDOptional(env.TaskID); err != nil {
		return err
	}
	if err := h.requireBound(ep, env.SenderID); err != nil {
		return err
	}
	if strings.HasPrefix(env.EventType, "ag2.channel.") && env.EventType != EnvChannelAck && env.EventType != EnvChannelReject {
		return fmt.Errorf("%w: %q is emitted only by the hub", ErrProtocol, env.EventType)
	}
	if env.Audience != "" {
		if ch, err := h.Channel(env.ChannelID); err == nil && !ch.hasParticipant(env.Audience) {
			return fmt.Errorf("%w: audience must list only participants of the channel", ErrProtocol)
		}
	}
	return nil
}

func checkTaskIDOptional(id string) error {
	if id == "" {
		return nil
	}
	return checkTaskID(id)
}

func (h *Hub) taskVisible(ep *wireEndpoint, md task.Metadata) bool {
	if h.isBound(ep, md.OwnerID) {
		return true
	}
	if md.ChannelID == "" {
		return false
	}
	ch, err := h.Channel(md.ChannelID)
	if err != nil {
		return false
	}
	for _, id := range h.boundAgents(ep) {
		if ch.hasParticipant(id) {
			return true
		}
	}
	return false
}

// requireTaskOwner passes for a task the hub has not observed (it has no
// owner) and otherwise needs the owner held by the connection.
func (h *Hub) requireTaskOwner(ep *wireEndpoint, taskID string) error {
	md, err := h.Task(taskID)
	if err != nil {
		return nil
	}
	return h.requireBound(ep, md.OwnerID)
}

// checkpointWriter names the agent a checkpoint write or read acts as: the
// requested writer if the connection holds it, else the only agent held.
func (h *Hub) checkpointWriter(ep *wireEndpoint, requested string) (string, error) {
	bound := h.boundAgents(ep)
	if len(bound) == 0 {
		return "", fmt.Errorf("%w: connection has no bound agent", ErrAccessDenied)
	}
	if requested != "" {
		return requested, h.requireBound(ep, requested)
	}
	if len(bound) > 1 {
		return "", fmt.Errorf("%w: writer_id is required when the connection holds several agents", ErrProtocol)
	}
	return bound[0], nil
}

// authorizeMirrorFailure admits a wire fire_task_event only as a TaskMirror
// failure report (see Python _authorize_mirror_failure).
func (h *Hub) authorizeMirrorFailure(ep *wireEndpoint, taskID, kind string, payload map[string]any) error {
	if kind != TaskEventMirrorFailed {
		return fmt.Errorf("%w: only %q task events are accepted over the wire, got %q", ErrAccessDenied, TaskEventMirrorFailed, kind)
	}
	for k := range payload {
		if !mirrorFailureKeys[k] {
			return fmt.Errorf("%w: mirror_failed payload key %q is not allowed", ErrProtocol, k)
		}
	}
	owner, _ := payload["owner_id"].(string)
	if owner == "" {
		return fmt.Errorf("%w: mirror_failed payload needs the reporting agent's owner_id", ErrProtocol)
	}
	if err := h.requireBound(ep, owner); err != nil {
		return err
	}
	channelID, _ := payload["channel_id"].(string)
	if md, err := h.Task(taskID); err == nil {
		if md.OwnerID != owner || channelID != md.ChannelID {
			return fmt.Errorf("%w: mirror_failed for task %q must carry its own owner and channel", ErrAccessDenied, taskID)
		}
	} else if channelID != "" {
		ch, err := h.Channel(channelID)
		if err != nil || !ch.hasParticipant(owner) {
			return fmt.Errorf("%w: task owner is not a participant of channel %q", ErrAccessDenied, channelID)
		}
	}
	return nil
}

// newRequestID mints a request id unique for the process lifetime. Wave-4
// model LinkRPC: a per-connection counter would let a late response of an
// old connection satisfy a new request.
func newRequestID() string { return uuid.New() }
