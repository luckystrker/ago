package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/luckystrker/ago/internal/uuid"
)

// TransportConfig tunes the streamable-HTTP transport (Python parity:
// ag2.mcp.TransportConfig plus the MCPServer stateless/json_response switches).
// The zero value is the stateless, JSON-answering transport this package has
// always served.
//
// AGO-DEVIATION: Python's defaults are stateful sessions and SSE answers
// (stateless=False, json_response=False). AGo keeps the stateless JSON default
// so an existing deployment's behaviour does not change; Stateful and
// StreamResponses switch to Python's mode.
type TransportConfig struct {
	// Stateful issues an Mcp-Session-Id at initialize and requires it on every
	// later request: an unknown or expired id answers 404, a request without
	// one 400, DELETE ends the session. Per-session state — the client's
	// advertised capabilities, the conversation continuing across calls, the
	// back-channel to the client — hangs off it.
	Stateful bool
	// StreamResponses answers each request POST as an SSE stream when the client
	// accepts one: notifications (progress, log) and server-to-client requests
	// (elicitation, sampling) travel on it ahead of the final response. Without
	// it replies are application/json and those messages have no wire.
	StreamResponses bool
	// SessionIdleTimeout is how long a session may sit with no request in flight
	// and no stream open before it is reaped (its next request answers 404).
	// Zero means 30 minutes; a negative value never reaps. Stateful only.
	SessionIdleTimeout time.Duration
	// MaxSessions caps concurrent sessions; an initialize beyond the cap answers
	// 503. Zero means 10,000; a negative value removes the cap.
	MaxSessions int
	// MaxRequestBodySize bounds one request body in bytes; a larger body answers
	// 413 on a Stateful or StreamResponses transport. Zero means the package
	// default (8 MiB).
	MaxRequestBodySize int
	// EventStore makes streams resumable: a client reconnecting with
	// Last-Event-ID receives what it missed. Nil leaves streams non-resumable.
	// A request's turn keeps running after its client disconnects when a store is
	// configured, so the reconnecting client collects the result.
	EventStore EventStore
	// SSERetryInterval is the reconnect delay suggested to clients on the
	// priming event (only sent on a resumable stream, and only to clients on
	// 2025-11-25 or later). Zero suggests nothing.
	SSERetryInterval time.Duration
}

const (
	defaultSessionIdleTimeout = 30 * time.Minute
	defaultMaxSessions        = 10000
)

// Protocol revisions this server speaks over the initialize handshake
// (mcp_types.version.HANDSHAKE_PROTOCOL_VERSIONS), oldest first. The modern
// per-request-envelope era (2026-07-28) is not served; a client probing it with
// server/discover is told which revisions are.
var handshakeVersions = []string{"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25"}

// latestHandshakeVersion answers a client asking for a revision the server does
// not speak (the SDK's negotiation: echo a supported request, else its latest).
const latestHandshakeVersion = "2025-11-25"

// defaultNegotiatedVersion is the revision assumed for a request that names none
// (mcp_types.DEFAULT_NEGOTIATED_VERSION).
const defaultNegotiatedVersion = "2025-03-26"

func supportedHandshake(v string) bool {
	for _, s := range handshakeVersions {
		if s == v {
			return true
		}
	}
	return false
}

// atLeast2025_11_25 reports whether v can resume a stream: only clients on
// 2025-11-25 or later parse the empty priming event. The revisions are dates, so
// string order is chronological.
func atLeast2025_11_25(v string) bool { return v >= "2025-11-25" }

// clientCaps is what a client declared at initialize.
type clientCaps struct {
	sampling    bool
	elicitation bool // form elicitation: a bare `elicitation: {}` counts, a url-only declaration does not
}

func parseClientCaps(raw json.RawMessage) clientCaps {
	var c struct {
		Sampling    json.RawMessage `json:"sampling"`
		Elicitation *struct {
			Form json.RawMessage `json:"form"`
			URL  json.RawMessage `json:"url"`
		} `json:"elicitation"`
	}
	_ = json.Unmarshal(raw, &c)
	out := clientCaps{sampling: len(c.Sampling) > 0 && string(c.Sampling) != "null"}
	if c.Elicitation != nil {
		hasForm := len(c.Elicitation.Form) > 0 && string(c.Elicitation.Form) != "null"
		hasURL := len(c.Elicitation.URL) > 0 && string(c.Elicitation.URL) != "null"
		out.elicitation = hasForm || !hasURL
	}
	return out
}

// mcpSession is one MCP session of a Stateful transport.
type mcpSession struct {
	id     string
	ctx    context.Context // ends with the session: DELETE, reaping, server close
	cancel context.CancelFunc

	mu              sync.Mutex
	initialized     bool
	protocolVersion string
	caps            clientCaps
	last            time.Time
	inflight        int
	streams         int // open GET streams (the standalone one and resumed ones)
	standalone      bool
	exchanges       map[string]*exchange // live request streams by key
	pending         map[string]chan wireReply
	nextReq         int64
	terminated      bool
	onEnd           func() // runs once when the session ends
}

// wireReply is a client's reply to a server-to-client request.
type wireReply struct {
	result json.RawMessage
	err    *rpcError
}

func newSession(parent context.Context) *mcpSession {
	ctx, cancel := context.WithCancel(parent)
	return &mcpSession{
		id: uuid.New(), ctx: ctx, cancel: cancel, last: time.Now(),
		exchanges: map[string]*exchange{}, pending: map[string]chan wireReply{},
		protocolVersion: defaultNegotiatedVersion,
	}
}

func (s *mcpSession) touch() {
	s.mu.Lock()
	s.last = time.Now()
	s.mu.Unlock()
}

// begin marks a request in flight (it holds the session open); the returned func
// ends it and restarts the idle window.
func (s *mcpSession) begin() func() {
	s.mu.Lock()
	s.inflight++
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.inflight--
		s.last = time.Now()
		s.mu.Unlock()
	}
}

func (s *mcpSession) idle(now time.Time, timeout time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return timeout > 0 && s.inflight == 0 && s.streams == 0 && now.Sub(s.last) > timeout
}

func (s *mcpSession) isInitialized() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initialized
}

func (s *mcpSession) clientCapabilities() clientCaps {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.caps
}

// terminate ends the session: its context is cancelled (turns in flight stop),
// open streams close and waiting back-channel calls fail.
func (s *mcpSession) terminate() {
	s.mu.Lock()
	if s.terminated {
		s.mu.Unlock()
		return
	}
	s.terminated = true
	pending := s.pending
	s.pending = map[string]chan wireReply{}
	exs := make([]*exchange, 0, len(s.exchanges))
	for _, e := range s.exchanges {
		exs = append(exs, e)
	}
	onEnd := s.onEnd
	s.mu.Unlock()
	s.cancel()
	if onEnd != nil {
		onEnd()
	}
	for _, ch := range pending {
		close(ch)
	}
	for _, e := range exs {
		e.close()
	}
}

// deliver routes a client's reply to the server request waiting for it; it
// reports whether anyone was waiting.
func (s *mcpSession) deliver(id string, r wireReply) bool {
	s.mu.Lock()
	ch, ok := s.pending[id]
	delete(s.pending, id)
	s.mu.Unlock()
	if ok {
		ch <- r
	}
	return ok
}

// --- the registry ----------------------------------------------------------------

// sessions is the Stateful transport's session table.
type sessions struct {
	idle time.Duration // <= 0: never reap
	max  int           // <= 0: unlimited
	// onEnd, when set, builds the cleanup run as a session ends.
	onEnd func(sessionID string) func()

	mu  sync.Mutex
	by  map[string]*mcpSession
	ctx context.Context
}

func newSessions(idle time.Duration, max int) *sessions {
	return &sessions{idle: idle, max: max, by: map[string]*mcpSession{}, ctx: context.Background()}
}

var (
	errTooManySessions = errors.New("mcpserver: too many open sessions")
	errNoSession       = errors.New("mcpserver: session not found")
)

// reapLocked drops sessions idle past the timeout. It runs on every lookup and
// admission: sessions need no background goroutine, and an expired id answers
// 404 from the first request that notices.
func (r *sessions) reapLocked(now time.Time) {
	for id, s := range r.by {
		if s.idle(now, r.idle) {
			delete(r.by, id)
			go s.terminate()
		}
	}
}

// open admits a new session or reports errTooManySessions.
func (r *sessions) open() (*mcpSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reapLocked(time.Now())
	if r.max > 0 && len(r.by) >= r.max {
		return nil, errTooManySessions
	}
	s := newSession(r.ctx)
	if r.onEnd != nil {
		s.onEnd = r.onEnd(s.id)
	}
	r.by[s.id] = s
	return s, nil
}

// get returns the live session, or errNoSession.
func (r *sessions) get(id string) (*mcpSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reapLocked(time.Now())
	s, ok := r.by[id]
	if !ok {
		return nil, errNoSession
	}
	s.touch()
	return s, nil
}

func (r *sessions) drop(id string) {
	r.mu.Lock()
	s := r.by[id]
	delete(r.by, id)
	r.mu.Unlock()
	if s != nil {
		s.terminate()
	}
}

func (r *sessions) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.by)
}

// --- the per-request channel ------------------------------------------------------

// ErrNoBackChannel reports a server-to-client request attempted where the
// request has no channel to the client: a JSON-answering or stateless
// transport, stdio, or a request already finished.
var ErrNoBackChannel = errors.New("mcpserver: this request has no back-channel to the client")

// ClientError is a JSON-RPC error object the client answered a server request
// with.
type ClientError struct {
	Code    int
	Message string
}

func (e *ClientError) Error() string { return fmt.Sprintf("client error %d: %s", e.Code, e.Message) }

// exchange is the channel of one in-flight request: the SSE stream its POST was
// answered on (when it was), through which the server sends notifications and
// requests ahead of the final response, and from which a resuming client can
// pick up what it missed.
type exchange struct {
	sess  *mcpSession // nil on a stateless transport
	key   string      // the stream id the event store files its messages under
	store EventStore
	// canRequest is whether a server-to-client request can be delivered and
	// answered: SSE answers on a stateful transport.
	canRequest bool
	// sse is whether the POST is being answered as a stream at all.
	sse bool

	done chan struct{} // closed when the request's response has been sent

	mu     sync.Mutex
	w      http.ResponseWriter
	fl     http.Flusher
	closed bool
}

func (e *exchange) attach(w http.ResponseWriter) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.w = w
	e.fl, _ = w.(http.Flusher)
}

func (e *exchange) detach() {
	e.mu.Lock()
	e.w, e.fl = nil, nil
	e.mu.Unlock()
}

func (e *exchange) close() {
	e.mu.Lock()
	e.closed = true
	e.w, e.fl = nil, nil
	e.mu.Unlock()
}

// emit stores one message (when a store is configured) and writes it to the
// attached connection. The store comes first, so a message produced while the
// client is disconnected is still there for the reconnecting client.
func (e *exchange) emit(ctx context.Context, msg any) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	id := ""
	if e.store != nil {
		id, _ = e.store.StoreEvent(ctx, e.key, data)
	}
	e.writeLocked(id, data)
}

func (e *exchange) writeLocked(id string, data []byte) {
	if e.w == nil {
		return
	}
	// A ResponseWriter that fails loudly (a wrapper over a connection that was
	// hijacked or torn down) must not take the agent's turn with it: the client is
	// gone, and what it missed stays in the event store.
	defer func() {
		if recover() != nil {
			e.w, e.fl = nil, nil
		}
	}()
	var buf []byte
	if id != "" {
		buf = append(buf, "id: "+id+"\n"...)
	}
	buf = append(buf, "event: message\ndata: "...)
	buf = append(buf, data...)
	buf = append(buf, "\n\n"...)
	if _, err := e.w.Write(buf); err != nil {
		e.w, e.fl = nil, nil // the client went away; later messages are only stored
		return
	}
	if e.fl != nil {
		e.fl.Flush()
	}
}

// notify sends a notification on the request's stream; without one it is dropped
// (Python parity: a JSON response carries only the response).
func (e *exchange) notify(ctx context.Context, method string, params any) {
	if e == nil || !e.sse {
		return
	}
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	e.emit(ctx, msg)
}

// request sends a server-to-client request on the stream and waits for the
// client's reply, which arrives as a separate POST routed here by id.
func (e *exchange) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if e == nil || !e.canRequest || e.sess == nil {
		return nil, ErrNoBackChannel
	}
	s := e.sess
	s.mu.Lock()
	if s.terminated {
		s.mu.Unlock()
		return nil, ErrNoBackChannel
	}
	s.nextReq++
	id := s.nextReq
	idKey := fmt.Sprint(id)
	ch := make(chan wireReply, 1)
	s.pending[idKey] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, idKey)
		s.mu.Unlock()
	}()

	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	e.emit(ctx, msg)
	select {
	case r, ok := <-ch:
		if !ok {
			return nil, ErrNoBackChannel
		}
		if r.err != nil {
			return nil, &ClientError{Code: r.err.Code, Message: r.err.Message}
		}
		return r.result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.ctx.Done():
		return nil, ErrNoBackChannel
	}
}

type exchangeKey struct{}

func withExchange(ctx context.Context, e *exchange) context.Context {
	return context.WithValue(ctx, exchangeKey{}, e)
}

func exchangeFrom(ctx context.Context) *exchange {
	e, _ := ctx.Value(exchangeKey{}).(*exchange)
	return e
}

type sessionKey struct{}

func withSession(ctx context.Context, s *mcpSession) context.Context {
	return context.WithValue(ctx, sessionKey{}, s)
}

func sessionFrom(ctx context.Context) *mcpSession {
	s, _ := ctx.Value(sessionKey{}).(*mcpSession)
	return s
}

type stdioKey struct{}

// withStdio marks a dispatch context as coming from the stdio transport, whose
// single client shares one conversation.
func withStdio(ctx context.Context) context.Context { return context.WithValue(ctx, stdioKey{}, true) }

func isStdio(ctx context.Context) bool { v, _ := ctx.Value(stdioKey{}).(bool); return v }

// Close ends every MCP session of a Stateful transport: turns in flight are
// cancelled, open streams close, and the sessions' ids answer 404 afterwards. It
// is safe to call more than once and a no-op on a stateless transport.
func (s *Server) Close() error {
	if s.sessions == nil {
		return nil
	}
	s.sessions.mu.Lock()
	all := make([]*mcpSession, 0, len(s.sessions.by))
	for id, sess := range s.sessions.by {
		all = append(all, sess)
		delete(s.sessions.by, id)
	}
	s.sessions.mu.Unlock()
	for _, sess := range all {
		sess.terminate()
	}
	return nil
}
