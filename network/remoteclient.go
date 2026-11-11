package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/luckystrker/ago/network/transport"
	"github.com/luckystrker/ago/task"
)

// Remote-mode errors. Match with errors.Is.
var (
	// ErrConnectionLost fails every request that was waiting on a link when it
	// dropped, and every request issued on a connection that is already down.
	// It also wraps the underlying transport error.
	ErrConnectionLost = errors.New("network: connection to the hub lost")
	// ErrRPCTimeout is returned when the hub does not answer within the
	// client's request timeout.
	ErrRPCTimeout = errors.New("network: hub request timed out")
	// ErrNotRemote is returned by remote-only HubClient methods on an
	// in-process client.
	ErrNotRemote = errors.New("network: client is not connected to a remote hub")
	// ErrClientClosed is returned by a closed remote HubClient.
	ErrClientClosed = errors.New("network: hub client closed")
)

// Dialer opens one connection to the hub: a fresh link per call. The link is
// one connection (transport.NewPipe, a WebSocket); the hub tells connections
// apart by the link they arrive on, never by anything in a frame.
type Dialer func(ctx context.Context) (transport.Link, error)

// ListenerDialer dials the in-memory listener l (the hub serves it with
// Hub.Serve).
func ListenerDialer(l *transport.MemListener) Dialer {
	return func(ctx context.Context) (transport.Link, error) { return l.Dial() }
}

// RemoteConfig connects a HubClient to a hub over a transport.Link.
type RemoteConfig struct {
	Dial Dialer
	// RPCTimeout bounds one request (default 45s: longer than the hub's invite
	// ack wait, which a create_channel request spans).
	RPCTimeout time.Duration
	// Reconnect, when set, makes the client re-dial by itself after the link
	// drops: with the policy's backoff and attempt limit it opens a new
	// connection and re-attaches every agent with a hello carrying its
	// per-channel high-water marks, then re-acknowledges them, so delivery
	// continues where it stopped. nil: the caller drives HubClient.Reconnect
	// (Python parity: ws.py leaves reconnect to the tenant).
	Reconnect *ReconnectPolicy
	// OnEvent, when set, is told about link loss, a completed reconnect and
	// giving up. It runs on the client's goroutines and must not block.
	OnEvent func(ConnectionEvent)
}

// ReconnectPolicy is the backoff of an automatic reconnect: attempt n waits
// Initial·Factor^(n-1), capped at Max, spread by ±Jitter (a fraction), and the
// client gives up after MaxAttempts failed attempts (0 = never).
type ReconnectPolicy struct {
	Initial     time.Duration
	Max         time.Duration
	Factor      float64
	Jitter      float64
	MaxAttempts int
}

// DefaultReconnectPolicy retries after 250ms, doubling up to 30s, ±20%, and
// gives up after 10 failed attempts (about two minutes).
func DefaultReconnectPolicy() ReconnectPolicy {
	return ReconnectPolicy{Initial: 250 * time.Millisecond, Max: 30 * time.Second, Factor: 2, Jitter: 0.2, MaxAttempts: 10}
}

// delay is the wait before attempt n (1-based); rnd yields a value in [0,1).
func (p ReconnectPolicy) delay(attempt int, rnd func() float64) time.Duration {
	d := float64(p.Initial)
	if p.Factor > 1 {
		for i := 1; i < attempt; i++ {
			d *= p.Factor
			if p.Max > 0 && d >= float64(p.Max) {
				d = float64(p.Max)
				break
			}
		}
	}
	if p.Max > 0 && d > float64(p.Max) {
		d = float64(p.Max)
	}
	if p.Jitter > 0 && rnd != nil {
		d *= 1 + p.Jitter*(2*rnd()-1)
	}
	if d < 0 {
		d = 0
	}
	return time.Duration(d)
}

// ConnectionEvent reports a change of the link of a remote client.
type ConnectionEvent struct {
	Kind    string // ConnLost | ConnReconnected | ConnGaveUp
	Attempt int    // the attempt that succeeded or was the last one
	Err     error  // why the link was lost / why the client gave up
}

// Connection event kinds.
const (
	ConnLost        = "lost"
	ConnReconnected = "reconnected"
	ConnGaveUp      = "gave_up"
)

// ConnectHub opens the first connection and returns a HubClient whose
// registrations, requests and deliveries travel over the link: the same
// AgentClient and HumanClient API as in-process, against a hub served by
// Hub.ServeLink. Hub() is nil on such a client.
func ConnectHub(ctx context.Context, cfg RemoteConfig) (*HubClient, error) {
	if cfg.Dial == nil {
		return nil, fmt.Errorf("%w: RemoteConfig needs Dial", ErrProtocol)
	}
	if cfg.RPCTimeout <= 0 {
		cfg.RPCTimeout = 45 * time.Second
	}
	rc := &remoteClient{cfg: cfg, agents: make(map[string]*remoteAgent), early: make(map[string][]transport.Frame)}
	if err := rc.connect(ctx); err != nil {
		return nil, err
	}
	hc := NewHubClient(nil)
	hc.api = &remoteHub{rc: rc}
	hc.remote = rc
	return hc, nil
}

// remoteClient is the connection state of a remote HubClient: the current
// connection (replaced on Reconnect) and the agents attached through it.
type remoteClient struct {
	cfg RemoteConfig

	mu     sync.Mutex
	conn   *remoteConn
	epoch  uint64
	closed bool
	stop   chan struct{} // closed by close(); lazily created (stopChan)
	// reconnecting is set while an automatic reconnect runs (one at a time).
	reconnecting bool
	reconnMu     sync.Mutex // serialises reconnect (manual and automatic)
	agents       map[string]*remoteAgent
	// early holds notifies that arrive for an agent not attached yet (the hub
	// binds a connection at register/hello, before the reply is processed), in
	// order, until its Attach.
	early map[string][]transport.Frame
}

// remoteAgent is one attached identity: its delivery callback, an ordered
// delivery queue and the data a reconnect needs.
type remoteAgent struct {
	rc *remoteClient
	id string

	mu      sync.Mutex
	notify  Notifier
	queue   []transport.Frame
	wake    chan struct{}
	done    chan struct{}
	high    map[string]string // channel id → highest envelope id delivered
	nacked  map[string]bool   // channel id → this connection nacked it: nothing more is delivered or acked there until a reconnect
	hello   HelloBody         // name and credentials for re-attaching
	stopped bool
}

// remoteConn is one connection generation: requests issued on it are
// answered only by frames read from its link, so a frame that arrives late
// from an older connection cannot satisfy a newer request.
type remoteConn struct {
	rc    *remoteClient
	link  transport.Link
	epoch uint64

	mu      sync.Mutex
	pending map[string]*rpcCall
	closed  bool
	downErr error // why the connection is down (set with closed)
}

type rpcCall struct {
	done chan struct{}
	resp transport.Frame
	err  error
}

// connect opens a new connection generation and starts its receive loop.
func (rc *remoteClient) connect(ctx context.Context) error {
	link, err := rc.cfg.Dial(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrConnectionLost, err)
	}
	rc.mu.Lock()
	rc.epoch++
	cn := &remoteConn{rc: rc, link: link, epoch: rc.epoch, pending: make(map[string]*rpcCall)}
	old := rc.conn
	rc.conn = cn
	rc.mu.Unlock()
	if old != nil {
		old.shutdown(fmt.Errorf("%w: replaced by a new connection", ErrConnectionLost))
	}
	go cn.loop()
	return nil
}

func (rc *remoteClient) current() (*remoteConn, error) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.closed {
		return nil, ErrClientClosed
	}
	return rc.conn, nil
}

// loop demultiplexes the link: responses, welcomes and errors complete the
// pending request of the same id on THIS connection; notifies go to the
// attached agents.
func (cn *remoteConn) loop() {
	for {
		f, err := cn.link.Recv(context.Background())
		if err != nil {
			lost := fmt.Errorf("%w: %w", ErrConnectionLost, err)
			cn.shutdown(lost)
			cn.rc.connLost(cn, lost)
			return
		}
		switch f.Kind {
		case transport.FrameResponse, transport.FrameWelcome, transport.FrameError:
			cn.complete(f)
		case transport.FrameNotify:
			cn.rc.onNotify(cn, f)
		}
	}
}

// complete hands a reply to the waiting request. A reply with no waiter — a
// duplicate, a late answer to a request that timed out, an error frame that
// answers no request — is dropped: each request is satisfied at most once.
func (cn *remoteConn) complete(f transport.Frame) {
	if f.RequestID == "" {
		return
	}
	cn.mu.Lock()
	call, ok := cn.pending[f.RequestID]
	if ok {
		delete(cn.pending, f.RequestID)
	}
	cn.mu.Unlock()
	if ok {
		call.resp = f
		close(call.done)
	}
}

// shutdown marks the connection down and fails every request waiting on it
// with err. Idempotent.
func (cn *remoteConn) shutdown(err error) {
	cn.mu.Lock()
	if cn.closed {
		cn.mu.Unlock()
		return
	}
	cn.closed = true
	cn.downErr = err
	pending := cn.pending
	cn.pending = make(map[string]*rpcCall)
	cn.mu.Unlock()
	_ = cn.link.Close()
	for _, call := range pending {
		call.err = err
		close(call.done)
	}
}

// do sends f (a request or hello) under a fresh request id and waits for the
// reply carrying that id.
func (cn *remoteConn) do(ctx context.Context, f transport.Frame) (transport.Frame, error) {
	f.RequestID = newRequestID()
	call := &rpcCall{done: make(chan struct{})}
	cn.mu.Lock()
	if cn.closed {
		err := cn.downErr
		cn.mu.Unlock()
		return transport.Frame{}, err
	}
	cn.pending[f.RequestID] = call
	cn.mu.Unlock()
	forget := func() {
		cn.mu.Lock()
		delete(cn.pending, f.RequestID)
		cn.mu.Unlock()
	}
	if err := cn.link.Send(f); err != nil {
		forget()
		return transport.Frame{}, fmt.Errorf("%w: %w", ErrConnectionLost, err)
	}
	timer := time.NewTimer(cn.rc.cfg.RPCTimeout)
	defer timer.Stop()
	select {
	case <-call.done:
		if call.err != nil {
			return transport.Frame{}, call.err
		}
		return call.resp, nil
	case <-ctx.Done():
		forget()
		return transport.Frame{}, ctx.Err()
	case <-timer.C:
		forget()
		return transport.Frame{}, fmt.Errorf("%w: %s", ErrRPCTimeout, f.Op)
	}
}

// call runs one control-plane op and decodes its result into out (nil to
// discard it).
func (rc *remoteClient) call(ctx context.Context, op string, params, out any) error {
	cn, err := rc.current()
	if err != nil {
		return err
	}
	var payload []byte
	if params != nil {
		if payload, err = json.Marshal(params); err != nil {
			return err
		}
	}
	resp, err := cn.do(ctx, transport.Frame{Kind: transport.FrameRequest, Op: op, Payload: payload})
	if err != nil {
		return err
	}
	if resp.Code != "" {
		return errorFromWire(resp.Code, resp.Message)
	}
	if out != nil && len(resp.Payload) > 0 {
		if err := json.Unmarshal(resp.Payload, out); err != nil {
			return fmt.Errorf("%w: decode result of %s: %v", ErrProtocol, op, err)
		}
	}
	return nil
}

func (rc *remoteClient) callBG(op string, params, out any) error {
	return rc.call(context.Background(), op, params, out)
}

// sendHello re-binds the connection to an identity (handshake) and waits for
// the welcome.
func (rc *remoteClient) sendHello(ctx context.Context, body HelloBody) error {
	cn, err := rc.current()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := cn.do(ctx, transport.Frame{Kind: transport.FrameHello, Payload: payload})
	if err != nil {
		return err
	}
	if resp.Kind == transport.FrameError {
		return errorFromWire(resp.Code, resp.Message)
	}
	return nil
}

// --- delivery -------------------------------------------------------------------------------------

// onNotify routes a delivery to the recipient's ordered queue. Frames of a
// connection that is no longer current are dropped: the hub re-delivers
// whatever the new connection has not acknowledged.
func (rc *remoteClient) onNotify(cn *remoteConn, f transport.Frame) {
	rc.mu.Lock()
	if rc.closed || rc.conn != cn {
		rc.mu.Unlock()
		return
	}
	ag := rc.agents[f.RecipientID]
	if ag == nil {
		const maxEarly = 256
		if len(rc.early[f.RecipientID]) < maxEarly {
			rc.early[f.RecipientID] = append(rc.early[f.RecipientID], f)
		}
		rc.mu.Unlock()
		return
	}
	rc.mu.Unlock()
	ag.enqueue(f)
}

func (a *remoteAgent) enqueue(f transport.Frame) {
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return
	}
	a.queue = append(a.queue, f)
	a.mu.Unlock()
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// run delivers queued notifies one at a time, in arrival order. It runs apart
// from the receive loop: a delivery may issue requests (acking an invite)
// whose responses are read by that loop.
func (a *remoteAgent) run() {
	for {
		a.mu.Lock()
		if a.stopped {
			a.mu.Unlock()
			return
		}
		if len(a.queue) == 0 {
			a.mu.Unlock()
			select {
			case <-a.wake:
			case <-a.done:
				return
			}
			continue
		}
		f := a.queue[0]
		a.queue = a.queue[1:]
		fn := a.notify
		a.mu.Unlock()
		a.deliver(fn, f)
	}
}

func (a *remoteAgent) deliver(fn Notifier, f transport.Frame) {
	// After a nack the hub stops sending that channel, but envelopes already in
	// flight still arrive: delivering or acking one would move the cursor past
	// the nacked envelope (model D19). They are dropped; the reconnect's replay
	// brings them back in order.
	a.mu.Lock()
	stalled := a.nacked[f.ChannelID]
	a.mu.Unlock()
	if stalled {
		return
	}
	var env Envelope
	if err := json.Unmarshal(f.Payload, &env); err != nil {
		a.nack(f, "undecodable envelope: "+err.Error())
		return
	}
	// The hub never repeats an envelope on a live connection; a repeat is a
	// redelivery after a lost acknowledgement. Within this process it is not
	// handed to the agent again, only acknowledged again (invariant 3: at most
	// duplicates until the ack is persisted — and not even those here).
	a.mu.Lock()
	seen := env.ID != "" && env.ID <= a.high[env.ChannelID]
	a.mu.Unlock()
	if seen {
		a.receipt(f, ReceiptAck, "")
		return
	}
	status, reason := ReceiptAck, ""
	func() {
		defer func() {
			if r := recover(); r != nil {
				status, reason = ReceiptNack, fmt.Sprint("notify handler panicked: ", r)
			}
		}()
		if fn != nil {
			fn(env)
		}
	}()
	if status == ReceiptNack {
		a.nack(f, reason)
		return
	}
	a.mu.Lock()
	if env.ID > a.high[env.ChannelID] {
		a.high[env.ChannelID] = env.ID
	}
	a.mu.Unlock()
	a.receipt(f, status, reason)
}

// nack reports an envelope the agent could not process and stops the channel
// on this connection. The high-water mark does not pass a nacked envelope, or
// a reconnect would ask the hub to skip it.
func (a *remoteAgent) nack(f transport.Frame, reason string) {
	a.mu.Lock()
	if a.nacked == nil {
		a.nacked = map[string]bool{}
	}
	a.nacked[f.ChannelID] = true
	a.mu.Unlock()
	a.receipt(f, ReceiptNack, reason)
}

// receipt acknowledges one delivery on the current connection.
func (a *remoteAgent) receipt(f transport.Frame, status, reason string) {
	if f.EnvelopeID == "" {
		return
	}
	cn, err := a.rc.current()
	if err != nil || cn == nil {
		return
	}
	_ = cn.link.Send(transport.Frame{
		Kind:        transport.FrameReceipt,
		RecipientID: a.id, ChannelID: f.ChannelID, EnvelopeID: f.EnvelopeID, Code: status, Message: reason,
	})
}

// attach registers (or with nil fn removes) the delivery callback of an
// identity and releases the notifies buffered for it.
func (rc *remoteClient) attach(agentID string, fn Notifier) error {
	rc.mu.Lock()
	if rc.closed {
		rc.mu.Unlock()
		return ErrClientClosed
	}
	if fn == nil {
		ag := rc.agents[agentID]
		delete(rc.agents, agentID)
		delete(rc.early, agentID)
		rc.mu.Unlock()
		if ag != nil {
			ag.stop()
		}
		return nil
	}
	ag := rc.agents[agentID]
	if ag == nil {
		ag = &remoteAgent{rc: rc, id: agentID, wake: make(chan struct{}, 1), done: make(chan struct{}), high: make(map[string]string)}
		rc.agents[agentID] = ag
		go ag.run()
	}
	early := rc.early[agentID]
	delete(rc.early, agentID)
	ag.mu.Lock()
	ag.notify = fn
	ag.mu.Unlock()
	// The buffered notifies are queued before rc.mu is released: a frame that
	// arrives for the agent right after must not overtake them.
	for _, f := range early {
		ag.enqueue(f)
	}
	rc.mu.Unlock()
	return nil
}

func (a *remoteAgent) stop() {
	a.mu.Lock()
	if !a.stopped {
		a.stopped = true
		close(a.done)
	}
	a.mu.Unlock()
}

// setHello remembers how to re-attach an identity after a reconnect.
func (rc *remoteClient) setHello(agentID string, body HelloBody) {
	rc.mu.Lock()
	ag := rc.agents[agentID]
	rc.mu.Unlock()
	if ag != nil {
		ag.mu.Lock()
		ag.hello = body
		ag.mu.Unlock()
	}
}

// reconnect drops the current connection (its waiting requests fail with
// ErrConnectionLost), dials a new one and re-binds every attached agent with a
// hello that carries its per-channel high-water marks.
func (rc *remoteClient) reconnect(ctx context.Context) error {
	rc.reconnMu.Lock()
	defer rc.reconnMu.Unlock()
	if _, err := rc.current(); err != nil {
		return err
	}
	if err := rc.connect(ctx); err != nil {
		return err
	}
	rc.mu.Lock()
	agents := make([]*remoteAgent, 0, len(rc.agents))
	for _, ag := range rc.agents {
		agents = append(agents, ag)
	}
	rc.mu.Unlock()
	for _, ag := range agents {
		ag.mu.Lock()
		body := ag.hello
		since := make(map[string]string, len(ag.high))
		for c, id := range ag.high {
			since[c] = id
		}
		ag.nacked = nil // a new connection starts with every channel delivered again
		ag.mu.Unlock()
		if body.Name == "" {
			continue
		}
		body.Since = since
		if err := rc.sendHello(ctx, body); err != nil {
			return fmt.Errorf("network: re-attach %s: %w", body.Name, err)
		}
		// AGO-DEVIATION: Python's replay uses the hello's since only to bound the
		// replay and never moves the cursor from it, so an ack lost with the old
		// connection leaves the cursor behind for good. The AGo client re-sends
		// an ack for its high-water mark of every channel right after the welcome
		// (model D13).
		ag.reack(since)
	}
	return nil
}

// reack re-acknowledges the high-water marks on the current connection.
func (a *remoteAgent) reack(marks map[string]string) {
	cn, err := a.rc.current()
	if err != nil || cn == nil {
		return
	}
	for ch, id := range marks {
		_ = cn.link.Send(transport.Frame{
			Kind: transport.FrameReceipt, RecipientID: a.id, ChannelID: ch, EnvelopeID: id, Code: ReceiptAck,
		})
	}
}

func (rc *remoteClient) stopChan() chan struct{} {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.stop == nil {
		rc.stop = make(chan struct{})
	}
	return rc.stop
}

func (rc *remoteClient) emit(ev ConnectionEvent) {
	if rc.cfg.OnEvent != nil {
		rc.cfg.OnEvent(ev)
	}
}

// connLost reacts to the end of a connection's read loop: when it is still the
// current connection of a client that reconnects by itself, a backoff loop
// starts.
func (rc *remoteClient) connLost(cn *remoteConn, err error) {
	rc.mu.Lock()
	if rc.closed || rc.conn != cn {
		rc.mu.Unlock()
		return
	}
	start := rc.cfg.Reconnect != nil && !rc.reconnecting
	if start {
		rc.reconnecting = true
	}
	rc.mu.Unlock()
	rc.emit(ConnectionEvent{Kind: ConnLost, Err: err})
	if start {
		go rc.autoReconnect()
	}
}

func (rc *remoteClient) autoReconnect() {
	defer func() {
		rc.mu.Lock()
		rc.reconnecting = false
		rc.mu.Unlock()
	}()
	pol := *rc.cfg.Reconnect
	stop := rc.stopChan()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	var last error
	for attempt := 1; ; attempt++ {
		if pol.MaxAttempts > 0 && attempt > pol.MaxAttempts {
			rc.emit(ConnectionEvent{Kind: ConnGaveUp, Attempt: attempt - 1, Err: last})
			return
		}
		select {
		case <-time.After(pol.delay(attempt, rand.Float64)):
		case <-ctx.Done():
			return
		}
		err := rc.reconnect(ctx)
		if err == nil {
			rc.emit(ConnectionEvent{Kind: ConnReconnected, Attempt: attempt})
			return
		}
		last = err
		switch {
		case errors.Is(err, ErrClientClosed) || ctx.Err() != nil:
			return
		case errors.Is(err, ErrAuth) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrAccessDenied):
			// The hub refuses the identity itself: retrying cannot help.
			rc.emit(ConnectionEvent{Kind: ConnGaveUp, Attempt: attempt, Err: err})
			return
		}
	}
}

func (rc *remoteClient) close() error {
	rc.mu.Lock()
	if rc.closed {
		rc.mu.Unlock()
		return nil
	}
	rc.closed = true
	if rc.stop == nil {
		rc.stop = make(chan struct{})
	}
	close(rc.stop)
	cn := rc.conn
	agents := rc.agents
	rc.agents = make(map[string]*remoteAgent)
	rc.mu.Unlock()
	for _, ag := range agents {
		ag.stop()
	}
	if cn != nil {
		cn.shutdown(fmt.Errorf("%w: client closed", ErrConnectionLost))
	}
	return nil
}

// --- HubClient remote surface ---------------------------------------------------------------------------

// Call runs one control-plane op against a remote hub and decodes the result
// into out (nil to discard it). The ops are the Op* constants.
func (c *HubClient) Call(ctx context.Context, op string, params, out any) error {
	if c.remote == nil {
		return ErrNotRemote
	}
	return c.remote.call(ctx, op, params, out)
}

// Reconnect replaces the link of a remote client: requests waiting on the old
// connection fail with ErrConnectionLost, a new connection is dialed and
// every attached agent is re-bound with a hello carrying its per-channel
// high-water marks.
func (c *HubClient) Reconnect(ctx context.Context) error {
	if c.remote == nil {
		return ErrNotRemote
	}
	return c.remote.reconnect(ctx)
}

// Close stops every agent and human this client built (their pumps and
// delivery queues end) and, for a remote client, tears the link down: waiting
// requests fail with ErrConnectionLost. The identities stay registered at the
// hub.
func (c *HubClient) Close() error {
	// Stop every agent and human this client built: their pumps and delivery
	// queues end (the identities stay registered at the hub).
	c.mu.Lock()
	var ids []string
	for id := range c.agents {
		ids = append(ids, id)
	}
	for id := range c.humans {
		ids = append(ids, id)
	}
	c.mu.Unlock()
	for _, id := range ids {
		_ = c.Detach(id)
	}
	if c.remote == nil {
		return nil
	}
	return c.remote.close()
}

// Detach stops delivery to an attached agent without unregistering it; a
// later Attach (or a reconnect elsewhere) can pick the identity up again.
func (c *HubClient) Detach(agentID string) error {
	c.mu.Lock()
	a := c.agents[agentID]
	h := c.humans[agentID]
	delete(c.agents, agentID)
	delete(c.humans, agentID)
	c.mu.Unlock()
	if a != nil {
		a.closeOnce.Do(func() { close(a.done) })
	}
	if h != nil {
		h.closeOnce.Do(func() { close(h.done) })
	}
	return c.api.Attach(agentID, nil)
}

// Attach binds an agent to the identity already registered under name — the
// reconnect-aware companion of Register. Remotely it performs the hello
// handshake (the hub validates the credentials of WithAgentAuth and binds this
// connection to the identity); in-process it only re-binds delivery.
func (c *HubClient) Attach(name string, opts ...AgentOption) (*AgentClient, error) {
	o := agentOptions{autoAck: true, inbox: 64}
	for _, opt := range opts {
		opt(&o)
	}
	if c.remote != nil {
		// The hub binds the connection at the hello; notifies that arrive before
		// the agent is attached are buffered.
		if err := c.remote.sendHello(context.Background(), HelloBody{Name: name, AuthScheme: o.authScheme, AuthClaim: o.authClaim}); err != nil {
			return nil, err
		}
	}
	passport, err := c.api.Passport(name)
	if err != nil {
		return nil, err
	}
	rule, err := c.api.RuleOf(passport.AgentID)
	if err != nil {
		return nil, err
	}
	a, err := c.newAgent(passport, rule, o)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// --- remoteHub: hubAPI over the wire -------------------------------------------------------------------------

// remoteHub implements hubAPI by control-plane requests. Methods whose hub
// counterpart has no error return swallow transport errors (the zero result
// reads as "not found" / "nothing to do").
type remoteHub struct{ rc *remoteClient }

var _ hubAPI = (*remoteHub)(nil)
var _ hubAPI = (*Hub)(nil)

func channelFromSnapshot(s ChannelSnapshot) *ChannelHandle {
	return &ChannelHandle{
		ID: s.ID, Manifest: s.Manifest, CreatorID: s.CreatorID, Participants: s.Participants,
		State: s.State, CreatedAt: s.CreatedAt, ExpiresAt: s.ExpiresAt, ClosedAt: s.ClosedAt,
		CloseReason: s.CloseReason, PendingAcks: s.PendingAcks, RejectedBy: s.RejectedBy,
	}
}

func (r *remoteHub) Register(p Passport, rule *Rule) (Passport, error) {
	var out Passport
	err := r.rc.callBG(OpRegister, registerParams{Passport: p, Rule: rule}, &out)
	return out, err
}

func (r *remoteHub) SetResume(res Resume) error {
	return r.rc.callBG(OpSetResume, setResumeParams{AgentID: res.AgentID, Resume: res}, nil)
}

func (r *remoteHub) Attach(agentID string, fn Notifier) error { return r.rc.attach(agentID, fn) }

func (r *remoteHub) Unregister(agentID string) error {
	return r.rc.callBG(OpUnregister, agentIDParams{AgentID: agentID}, nil)
}

func (r *remoteHub) Passport(nameOrID string) (Passport, error) {
	var out Passport
	err := r.rc.callBG(OpGetAgent, nameOrIDParams{NameOrID: nameOrID}, &out)
	return out, err
}

func (r *remoteHub) RuleOf(agentID string) (Rule, error) {
	var out Rule
	err := r.rc.callBG(OpGetRule, agentIDParams{AgentID: agentID}, &out)
	return out, err
}

func (r *remoteHub) Send(env Envelope) (Envelope, error) {
	var out Envelope
	err := r.rc.callBG(OpPostEnvelope, postEnvelopeParams{Envelope: env}, &out)
	return out, err
}

func (r *remoteHub) CreateChannel(m ChannelManifest, participants []string) (*ChannelHandle, error) {
	var out ChannelSnapshot
	if err := r.rc.callBG(OpCreateChannel, createChannelParams{Manifest: m, Participants: participants}, &out); err != nil {
		return nil, err
	}
	return channelFromSnapshot(out), nil
}

func (r *remoteHub) Channel(id string) (*ChannelHandle, error) {
	var out ChannelSnapshot
	if err := r.rc.callBG(OpGetChannel, channelIDParams{ChannelID: id}, &out); err != nil {
		return nil, err
	}
	return channelFromSnapshot(out), nil
}

func (r *remoteHub) CloseChannel(id, reason string) error {
	return r.rc.callBG(OpCloseChannel, closeChannelParams{ChannelID: id, Reason: reason}, nil)
}

// AckInbox is a no-op remotely: the hub frees a recipient's inbox slot when
// it receives the delivery receipt the client sends for every notify.
func (r *remoteHub) AckInbox(string, int) {}

// bookInbox is a no-op remotely (the hub books a redelivery itself).
func (r *remoteHub) bookInbox(string) {}

func (r *remoteHub) FindEnvelopeByCausation(channelID, senderID, causationID string) (Envelope, bool) {
	var out *Envelope
	if err := r.rc.callBG(OpFindEnvelopeByCausation, causationParams{ChannelID: channelID, SenderID: senderID, CausationID: causationID}, &out); err != nil || out == nil {
		return Envelope{}, false
	}
	return *out, true
}

func (r *remoteHub) ReportTurnFailure(channelID, agentID, envelopeID string, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	_ = r.rc.callBG(OpReportTurnFailure, turnFailureParams{ChannelID: channelID, AgentID: agentID, EnvelopeID: envelopeID, Error: msg}, nil)
}

// AddResumeExample is one op on the hub, which extends the resume under its
// lock — atomic, unlike a client-side read-modify-write.
func (r *remoteHub) AddResumeExample(agentID string, ex ResumeExample) error {
	return r.rc.callBG(OpAddResumeExample, addResumeExampleParams{AgentID: agentID, Example: ex}, nil)
}

// bigLimit asks a limited list op for everything (the hub's default is 50).
var bigLimit = func() *int { n := 1 << 20; return &n }()

// Registrations lists every passport, ordered by name (like Hub.Registrations).
func (r *remoteHub) Registrations() []Passport {
	var out []Passport
	_ = r.rc.callBG(OpListAgents, listAgentsParams{SortBy: SortByName, Limit: func() *int { n := 0; return &n }()}, &out)
	return out
}

func (r *remoteHub) ResumeOf(agentID string) (Resume, error) {
	var out Resume
	err := r.rc.callBG(OpGetResume, agentIDParams{AgentID: agentID}, &out)
	return out, err
}

// ListChannels lists the channels the connection's agents participate in (the
// hub shows a connection no other channel).
func (r *remoteHub) ListChannels() []*ChannelHandle {
	var snaps []ChannelSnapshot
	if err := r.rc.callBG(OpListChannels, listChannelsParams{Limit: bigLimit}, &snaps); err != nil {
		return nil
	}
	out := make([]*ChannelHandle, len(snaps))
	for i, s := range snaps {
		out[i] = channelFromSnapshot(s)
	}
	return out
}

// Tasks lists the observed tasks the connection may see, ordered by id; a
// non-empty owner restricts to that owner's tasks.
func (r *remoteHub) Tasks(owner string, activeOnly bool) []task.Metadata {
	var all []task.Metadata
	if err := r.rc.callBG(OpListTasks, listTasksParams{AgentID: owner, Limit: bigLimit}, &all); err != nil {
		return nil
	}
	out := all[:0]
	for _, md := range all {
		if !activeOnly || !md.State.Terminal() {
			out = append(out, md)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TaskID < out[j].TaskID })
	return out
}

func (r *remoteHub) ReadWAL(channelID string) ([]Envelope, error) {
	var out []Envelope
	err := r.rc.callBG(OpReadWAL, readWALParams{ChannelID: channelID}, &out)
	return out, err
}

func (r *remoteHub) SetRule(agentID string, rule Rule) error {
	return r.rc.callBG(OpSetRule, setRuleParams{AgentID: agentID, Rule: rule}, nil)
}

func (r *remoteHub) PendingTurnsFor(agentID string) []PendingTurn {
	var out []PendingTurn
	_ = r.rc.callBG(OpPendingTurnsFor, agentIDParams{AgentID: agentID}, &out)
	return out
}

func (r *remoteHub) envelopeByID(channelID, envelopeID string) (Envelope, bool) {
	var log []Envelope
	if err := r.rc.callBG(OpReadWAL, readWALParams{ChannelID: channelID}, &log); err != nil {
		return Envelope{}, false
	}
	for _, e := range log {
		if e.ID == envelopeID {
			return e, true
		}
	}
	return Envelope{}, false
}

func (r *remoteHub) Task(taskID string) (task.Metadata, error) {
	var out task.Metadata
	err := r.rc.callBG(OpGetTask, taskIDParams{TaskID: taskID}, &out)
	return out, err
}
