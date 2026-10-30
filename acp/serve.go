package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/internal/uuid"
	"github.com/luckystrker/ago/stream"
)

// ServeConfig configures Serve.
type ServeConfig struct {
	// AgentName and AgentVersion are advertised in the initialize handshake
	// (defaults "ago-agent" / "0.0.0").
	AgentName    string
	AgentVersion string

	// Handle runs one prompt on sessionID. ctx is the turn's context: it is
	// cancelled when the client sends session/cancel mid-turn (a handler that
	// ignores it gets the cancelled stop reason anyway, but a cooperative one
	// stops its own work early). userText is the concatenated text of the
	// prompt's text blocks. emit pushes session/update notifications to the
	// client — Message for the reply text (several calls stream), Thought,
	// Plan, ToolCall / ToolCallUpdate and Usage for the rest of the
	// vocabulary. finalText is the turn's answer; when it differs from the
	// Message text already emitted it goes out as one more chunk, so a handler
	// that never emits still delivers (Python parity: executor._send_final_text
	// — re-sending streamed text would show the reply twice).
	//
	// An error fails the session/prompt request as a JSON-RPC internal error
	// carrying the reason (the client is in another process; an error without
	// its reason leaves whoever is debugging with nothing at all) — unless the
	// turn context was cancelled, in which case the answer is the cancelled
	// stop reason, never an error (Python parity: the prompt handler reports
	// CancelledError as PromptResponse(stop_reason="cancelled")).
	Handle func(ctx context.Context, sessionID string, userText string, emit *Emitter) (finalText string, err error)

	// Permission decides the session/request_permission requests raised via
	// Session.RequestPermission. A nil policy — or a false answer — denies
	// locally: the reject decision is synthesized and the client is never
	// asked. A true answer sends the request with the standard allow/reject
	// options and relays the client's chosen outcome.
	Permission func(toolCall, input string) bool

	// Elicit decides whether a question raised via Session.Elicit reaches the
	// client's user. A nil policy — or a false answer — declines locally:
	// ("", false), no wire traffic, and never a fabricated answer. An accepted
	// question goes to the client; its accept action carries the answer.
	Elicit func(message string) bool

	// Auth authenticates the connection. nil (the default) advertises no auth
	// methods and refuses authenticate — appropriate for local stdio, where
	// the client launched the process (Python parity: ACPAgent(auth=None)).
	// A provider gates every session method on a successful authenticate
	// first. See auth.go for the seam's limits.
	Auth Authenticator

	// History, when set, returns the stored events of a session for the
	// session/load replay. ServeAgent installs one over its per-session
	// conversations; a plain Serve holds no history, so load then replays
	// nothing (Python parity: the session store's history — the cross-process
	// storage backend stays deferred, docs/DECISIONS.md D-066).
	History func(sessionID string) ([]events.Event, error)

	// StreamThoughts projects the agent's reasoning as agent_thought_chunk
	// updates. Off by default: reasoning is internal, and an ACP client may be
	// an external audience (Python parity: ACPAgent stream_thoughts=False).
	StreamThoughts bool

	// ContextWindow is the agent's context-window size in tokens. When set,
	// every UsageEvent of a turn becomes a usage_update{used, size} — the
	// token counts the turn reported, against this window. Without it the
	// usage_update's required "size" would be a guess, so no usage update is
	// emitted from events (Python parity gap: the executor does not emit
	// usage updates at all; docs/DECISIONS.md D-056).
	ContextWindow int

	// OnSession, when set, is called with the session handle during
	// session/new, before the response (Python parity: the ACPAgent on_session
	// observer). A handler that needs the outbound permission/elicitation
	// requests — or the client-declared MCP gateway — captures the session here.
	OnSession func(s *Session)
}

// Session is the serve-side handle for one conversation: the id the client
// holds, the working directory it declared, the MCP servers it announced, and
// the outbound agent→client requests a handler may raise mid-turn.
type Session struct {
	ID  string
	CWD string
	// MCPServers are the MCP servers the client declared in session/new
	// (Python parity: recorded session context). They are not connected
	// eagerly — a server named by a client is context, not authorization
	// (agent.py new_session) — the gateway dials one only when a handler
	// first uses it.
	MCPServers []MCPServer

	conn        *serveConn
	gatewayOnce sync.Once
	gateway     *ToolGateway
}

// Gateway returns a proxy for the session's declared MCP servers, or nil when
// the client declared none. Tool calls made through it travel over the
// ago/mcp client (one connection per declared server, dialed lazily on the
// first call). The returned gateway belongs to the session; Serve tears it
// down with the connection.
func (s *Session) Gateway() *ToolGateway {
	if s == nil || len(s.MCPServers) == 0 {
		return nil
	}
	s.gatewayOnce.Do(func() { s.gateway = NewToolGateway(s.MCPServers) })
	return s.gateway
}

// RequestPermission raises the protocol's session/request_permission request
// for toolCall (a title describing the action) and input (its raw input).
//
// The Permission policy is consulted first: a nil policy or a false answer
// denies locally — the reject decision is returned and nothing goes out on the
// wire. A true answer sends the request with the standard allow/reject options
// and relays the client's answer; a cancelled outcome denies.
//
// It must be called from inside ServeConfig.Handle (the serve loop owns the
// connection while a prompt runs). ctx is checked before the request is
// written; the reply read itself cannot be cancelled.
func (s *Session) RequestPermission(ctx context.Context, toolCall, input string) (Decision, error) {
	if s == nil || s.conn == nil {
		return Decision{}, errors.New("acp: session is not connected")
	}
	c := s.conn
	if c.cfg.Permission == nil || !c.cfg.Permission(toolCall, input) {
		return rejectDecision(), nil
	}
	blk, _ := json.Marshal(input)
	res, err := c.exchange(ctx, "session/request_permission", requestPermissionParams{
		SessionID: s.ID,
		ToolCall:  permToolCall{Title: toolCall, RawInput: blk},
		Options: []permissionOption{
			{OptionID: "allow", Name: "Allow", Kind: kindAllowOnce},
			{OptionID: "reject", Name: "Reject", Kind: kindRejectOnce},
		},
	})
	if err != nil {
		return Decision{}, err
	}
	var d Decision
	if err := json.Unmarshal(res, &d); err != nil {
		return Decision{}, fmt.Errorf("%w: session/request_permission result: %v", ErrProtocol, err)
	}
	return d, nil
}

// Elicit asks the client's user the question (elicitation/create, text form).
//
// The Elicit policy is consulted first: a nil policy or false declines
// locally — ("", false, nil), no wire traffic, and never a fabricated answer.
// An accepted question goes to the client; its accept action carries the
// answer, decline/cancel map to ("", false, nil).
func (s *Session) Elicit(ctx context.Context, message string) (string, bool, error) {
	if s == nil || s.conn == nil {
		return "", false, errors.New("acp: session is not connected")
	}
	c := s.conn
	if c.cfg.Elicit == nil || !c.cfg.Elicit(message) {
		return "", false, nil
	}
	res, err := c.exchange(ctx, "elicitation/create", elicitationParams{Message: message})
	if err != nil {
		return "", false, err
	}
	var r elicitationResult
	if err := json.Unmarshal(res, &r); err != nil {
		return "", false, fmt.Errorf("%w: elicitation/create result: %v", ErrProtocol, err)
	}
	if r.Action != ElicitAccept {
		return "", false, nil
	}
	text, _ := r.Content.(string)
	return text, true, nil
}

// Emitter streams session/update notifications to the client while a turn
// runs. Handle receives one bound to the session; Message is the reply text,
// the rest the vocabulary this subset speaks. Every Message is recorded so
// the turn's final text is only sent when it was not already streamed. A
// write failure is dropped: the turn's answer still goes out with the
// prompt's response, and a dead pipe fails the next write loudly.
type Emitter struct {
	c         *serveConn
	sessionID string

	mu        sync.Mutex
	delivered []string
}

func (e *Emitter) push(u Update) {
	_ = e.c.write(rpcNotification{JSONRPC: "2.0", Method: "session/update",
		Params: updateParams{SessionID: e.sessionID, Update: u}})
}

// Message streams one reply-text chunk (Python parity:
// acp.update_agent_message_text).
func (e *Emitter) Message(chunk string) {
	e.push(agentMessageChunk(chunk))
	e.mu.Lock()
	e.delivered = append(e.delivered, chunk)
	e.mu.Unlock()
}

// messageText is the reply text already on the wire this turn.
func (e *Emitter) messageText() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.delivered, "")
}

// Thought streams one reasoning delta. The ServeConfig.StreamThoughts flag
// gates it — ServeAgent emits thoughts only when the deployment opted in
// (Python parity: executor event mapping, stream_thoughts).
func (e *Emitter) Thought(chunk string) {
	e.push(Update{Kind: "agent_thought_chunk", Text: chunk})
}

// Plan replaces the client's view of the agent's plan wholesale (Python
// parity: acp.update_plan).
func (e *Emitter) Plan(entries []PlanEntry) {
	e.push(Update{Kind: "plan", Entries: entries})
}

// ToolCall announces a tool call: id, a title, the kind (KindOther unless the
// deployment knows better) and the in_progress status, with rawInput as the
// call's raw JSON arguments (Python parity: mappers._tool_call_start).
func (e *Emitter) ToolCall(id, title, kind, status string, rawInput string) {
	e.push(Update{Kind: "tool_call", ToolCallID: id, Title: title, ToolKind: kind,
		Status: status, RawInput: json.RawMessage(rawInput)})
}

// ToolCallUpdate settles a tool call: the id, the settled status (failed or
// completed) and the result text (Python parity: mappers._tool_result_update).
func (e *Emitter) ToolCallUpdate(id, title, status, text string) {
	e.push(Update{Kind: "tool_call_update", ToolCallID: id, Title: title,
		Status: status, ContentText: text})
}

// Usage reports session token state: used tokens in context against the
// context-window size (Python parity: acp.UsageUpdate — used/size required,
// cost optional).
func (e *Emitter) Usage(used, size int64, cost *UsageCost) {
	e.push(Update{Kind: "usage_update", Used: used, Size: size, Cost: cost})
}

// serveConn is one serve-role connection: the config, the input scanner, the
// sessions minted on it, and the turn contexts session/cancel can stop.
type serveConn struct {
	cfg ServeConfig
	in  *bufio.Scanner
	out io.Writer

	// writeMu serializes every write to out. The serve loop writes responses,
	// and emit closures run on stream-dispatch goroutines while the loop sits
	// inside Handle — both funnel through here.
	writeMu sync.Mutex

	nextID   atomic.Int64
	sessions map[string]*Session
	// initialized/authed gate the session methods (Python parity:
	// _require_session_scope). authed starts true when no provider is
	// configured — local stdio needs none.
	initialized bool
	authed      bool

	// cancelMu guards turnCancels: the read goroutine cancels a turn while
	// the serve loop registers/unregisters it.
	cancelMu    sync.Mutex
	turnCancels map[string]context.CancelFunc

	// replies carries agent→client request answers from the read goroutine to
	// the exchange waiting inside Handle (one exchange in flight at a time).
	// cmds carries client→agent requests to the serve loop, which runs them
	// one at a time.
	replies  chan *incoming
	cmds     chan *incoming
	readDone chan struct{}
	readErr  error
}

// Serve reads ACP JSON-RPC from stdin and writes to stdout until the client
// disconnects (stdin EOF returns nil). Requests run one at a time on the
// serve loop; a dedicated read goroutine keeps the wire live while a turn
// runs, which is what makes session/cancel deliverable mid-turn (the
// multi-session queue of the Python store stays deferred — see
// docs/DECISIONS.md D-066).
func Serve(cfg ServeConfig) error {
	return serveIO(os.Stdin, os.Stdout, cfg)
}

// serveIO is Serve over explicit streams; the tests drive it through pipes.
func serveIO(in io.Reader, out io.Writer, cfg ServeConfig) error {
	if cfg.Handle == nil {
		return errors.New("acp: ServeConfig.Handle is not set")
	}
	if cfg.AgentName == "" {
		cfg.AgentName = defaultAgentName
	}
	if cfg.AgentVersion == "" {
		cfg.AgentVersion = defaultAgentVersion
	}
	c := &serveConn{
		cfg:         cfg,
		out:         out,
		sessions:    map[string]*Session{},
		authed:      cfg.Auth == nil,
		turnCancels: map[string]context.CancelFunc{},
		replies:     make(chan *incoming, 8),
		cmds:        make(chan *incoming, 64),
		readDone:    make(chan struct{}),
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), scanBuffer)
	c.in = sc

	go c.readLoop()
	for req := range c.cmds {
		result, rpcErr := c.dispatch(req.Method, req.Params)
		resp := rpcResponse{JSONRPC: "2.0", ID: *req.ID}
		if rpcErr != nil {
			resp.Error = rpcErr
		} else {
			resp.Result = result
		}
		if err := c.write(resp); err != nil {
			return err
		}
	}
	<-c.readDone
	return c.readErr
}

// readLoop parses incoming lines and routes them: replies to the exchange
// waiting inside Handle, session/cancel straight to the turn's context, and
// everything else onto the serve loop's queue. A queue overflow is refused
// with an internal error rather than parking the client's request forever.
func (c *serveConn) readLoop() {
	// Close order matters: replies first — an exchange parked inside Handle
	// must learn the client is gone — then the serve loop's queue, then the
	// done signal the serve loop waits on.
	defer close(c.readDone)
	defer close(c.cmds)
	defer close(c.replies)
	for c.in.Scan() {
		line := bytes.TrimSpace(c.in.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg incoming
		if err := json.Unmarshal(line, &msg); err != nil {
			// A garbled line is skipped: a chatty banner must not kill the
			// session (same policy as the mcp stdio client).
			continue
		}
		if msg.Method == "" {
			if msg.ID == nil {
				continue
			}
			// A reply to an agent→client request; the single exchange in
			// flight picks it up, an unsolicited one is dropped.
			select {
			case c.replies <- &msg:
			default:
			}
			continue
		}
		if msg.ID == nil {
			if msg.Method == "session/cancel" {
				c.cancelTurn(msg.Params)
			}
			continue // other notifications are outside the subset
		}
		select {
		case c.cmds <- &msg:
		default:
			_ = c.write(rpcResponse{JSONRPC: "2.0", ID: *msg.ID,
				Error: rpcErr(codeInternalError, "the serve loop is busy; request %s dropped", msg.Method)})
		}
	}
	if err := c.in.Err(); err != nil {
		c.readErr = fmt.Errorf("acp: read stdin: %w", err)
	}
}

// cancelTurn stops the running turn of the named session. A notification has
// no response channel, so an unknown session — or one with nothing running —
// is ignored rather than raised (Python parity: agent.py cancel).
func (c *serveConn) cancelTurn(params json.RawMessage) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.SessionID == "" {
		return
	}
	c.cancelMu.Lock()
	cancel := c.turnCancels[p.SessionID]
	c.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// write pushes one message out under the write lock.
func (c *serveConn) write(msg any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeLine(c.out, msg)
}

// dispatch runs one request to completion. It is called on the serve loop
// goroutine; Handle runs inline, so the scanner is exclusively the read
// goroutine's while a nested exchange (permission/elicitation) waits for the
// client on the replies channel.
func (c *serveConn) dispatch(method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		var p struct {
			ProtocolVersion *int `json:"protocolVersion"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, rpcErr(codeInvalidParams, "initialize params: %v", err)
		}
		// Answer with the lower of the two versions (Python parity:
		// agent.py initialize — min(protocol_version, acp.PROTOCOL_VERSION)).
		version := protocolVersion
		if p.ProtocolVersion != nil && *p.ProtocolVersion < version {
			version = *p.ProtocolVersion
		}
		c.initialized = true
		caps := map[string]any{
			// Truthful advertisement: the subset takes text prompts and
			// nothing else; anything advertised here must actually work.
			// loadSession is advertised unconditionally, like the Python
			// agent does: an id with no replayable history gets a clean
			// resource_not_found (agent.py _capabilities).
			"loadSession": true,
			"promptCapabilities": map[string]any{
				"image":           false,
				"audio":           false,
				"embeddedContext": false,
			},
		}
		resp := map[string]any{
			"protocolVersion":   version,
			"agentCapabilities": caps,
			"agentInfo": map[string]any{
				"name":    c.cfg.AgentName,
				"version": c.cfg.AgentVersion,
			},
		}
		if c.cfg.Auth != nil {
			resp["authMethods"] = c.cfg.Auth.Methods()
		}
		return resp, nil

	case "authenticate":
		if !c.initialized {
			return nil, rpcErr(codeInvalidRequest, "Call initialize before authenticate.")
		}
		if c.cfg.Auth == nil {
			return nil, rpcErr(codeInvalidRequest, "This ACP agent does not require or support authentication.")
		}
		// The SDK spreads the request's _meta into the provider's kwargs
		// (agent.py authenticate); the token arrives there, or nested under a
		// "meta" entry of the metadata (auth.py StaticTokenAuth).
		var p struct {
			MethodID string         `json:"methodId"`
			Meta     map[string]any `json:"_meta"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, rpcErr(codeInvalidParams, "authenticate params: %v", err)
		}
		if p.Meta == nil {
			p.Meta = map[string]any{}
		}
		if err := c.cfg.Auth.Authenticate(p.MethodID, p.Meta); err != nil {
			return nil, rpcErr(codeAuthRequired, "%s", err.Error())
		}
		c.authed = true
		return map[string]any{}, nil

	case "session/new":
		if rpcErr := c.requireSessionScope(); rpcErr != nil {
			return nil, rpcErr
		}
		var p struct {
			CWD        string      `json:"cwd"`
			MCPServers []MCPServer `json:"mcpServers"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, rpcErr(codeInvalidParams, "session/new params: %v", err)
		}
		// cwd is recorded as context, never acted on: a path named by a
		// client is context, not authorization (Python parity: new_session).
		// The declared MCP servers are likewise recorded, connected only when
		// a handler asks the session's Gateway to dial one.
		s := &Session{ID: uuid.New(), CWD: p.CWD, MCPServers: p.MCPServers, conn: c}
		c.sessions[s.ID] = s
		if c.cfg.OnSession != nil {
			c.cfg.OnSession(s)
		}
		return map[string]any{"sessionId": s.ID}, nil

	case "session/load":
		if rpcErr := c.requireSessionScope(); rpcErr != nil {
			return nil, rpcErr
		}
		return c.loadSession(params)

	case "session/prompt":
		if rpcErr := c.requireSessionScope(); rpcErr != nil {
			return nil, rpcErr
		}
		var p struct {
			SessionID string         `json:"sessionId"`
			Prompt    []contentBlock `json:"prompt"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, rpcErr(codeInvalidParams, "session/prompt params: %v", err)
		}
		session, ok := c.sessions[p.SessionID]
		if !ok {
			// Python parity: resource_not_found "acp-session:<id>" (agent.py
			// _get_session) — the ACP resource-not-found code.
			return nil, rpcErr(codeResourceNotFound, "acp-session:%s", p.SessionID)
		}
		return c.runPrompt(session, blocksText(p.Prompt))

	default:
		return nil, rpcErr(codeMethodNotFound, "method %q is not implemented", method)
	}
}

// requireSessionScope gates every session operation on an initialized — and,
// when an auth provider is configured, authenticated — connection (Python
// parity: agent.py _require_session_scope).
func (c *serveConn) requireSessionScope() *rpcError {
	if !c.initialized {
		return rpcErr(codeInvalidRequest, "Call initialize before any session method.")
	}
	if !c.authed {
		return rpcErr(codeAuthRequired, "Call authenticate first.")
	}
	return nil
}

// runPrompt runs one turn. The turn runs under its own context, registered so
// a session/cancel notification can stop it; when the context was cancelled
// the answer is the cancelled stop reason, whatever the handler returned.
func (c *serveConn) runPrompt(session *Session, userText string) (any, *rpcError) {
	turnCtx, cancel := context.WithCancel(context.Background())
	c.cancelMu.Lock()
	c.turnCancels[session.ID] = cancel
	c.cancelMu.Unlock()
	defer func() {
		c.cancelMu.Lock()
		delete(c.turnCancels, session.ID)
		c.cancelMu.Unlock()
		cancel()
	}()

	// One turn, one tracker: what the emitter put on the wire as reply text,
	// so the final text is only sent when it was not (executor parity:
	// _forward_updates + _send_final_text).
	emit := &Emitter{c: c, sessionID: session.ID}

	final, err := c.cfg.Handle(turnCtx, session.ID, userText, emit)
	if turnCtx.Err() != nil {
		// Cancellation is a normal ACP outcome, not a failure: it is reported
		// as a stop reason, and whatever the handler returned is discarded
		// (Python parity: the prompt handler's CancelledError branch).
		return map[string]any{"stopReason": StopCancelled}, nil
	}
	if err != nil {
		return nil, rpcErr(codeInternalError, "%s", err.Error())
	}
	if final != "" && final != emit.messageText() {
		emit.Message(final)
	}
	return map[string]any{"stopReason": StopEndTurn}, nil
}

// loadSession reattaches the client to a session by id and replays its stored
// history as session/update notifications before the response (Python parity:
// agent.py load_session). The subset keeps sessions per connection, so only a
// session this connection issued can load; an id with no session is answered
// resource_not_found the way Python answers an id with no history.
func (c *serveConn) loadSession(params json.RawMessage) (any, *rpcError) {
	var p struct {
		SessionID  string      `json:"sessionId"`
		CWD        string      `json:"cwd"`
		MCPServers []MCPServer `json:"mcpServers"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, rpcErr(codeInvalidParams, "session/load params: %v", err)
	}
	session, ok := c.sessions[p.SessionID]
	if !ok {
		return nil, rpcErr(codeResourceNotFound, "acp-session:%s", p.SessionID)
	}
	// Refresh what the request carries (Python parity: the live-session
	// branch of load_session).
	session.CWD = p.CWD
	session.MCPServers = p.MCPServers
	session.gatewayOnce = sync.Once{}
	session.gateway = nil

	var evs []events.Event
	if c.cfg.History != nil {
		var err error
		if evs, err = c.cfg.History(session.ID); err != nil {
			return nil, rpcErr(codeInternalError, "read session history: %s", err.Error())
		}
	}
	// Every stored entry is replayed before this returns, and nothing is sent
	// after: the response is the line between history and whatever comes next
	// (agent.py load_session). Sequential on purpose.
	for _, u := range historyToUpdates(evs, session.ID) {
		if err := c.write(rpcNotification{JSONRPC: "2.0", Method: "session/update",
			Params: updateParams{SessionID: session.ID, Update: u}}); err != nil {
			return nil, rpcErr(codeInternalError, "deliver replay: %s", err.Error())
		}
	}
	return map[string]any{}, nil
}

// exchange writes one agent→client request and reads its reply. Called from
// inside Handle, so the reply arrives on the replies channel the read
// goroutine feeds.
func (c *serveConn) exchange(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := c.nextID.Add(1)
	if err := c.write(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		return nil, err
	}
	return c.readReply(id)
}

// readReply waits for the reply with wantID on the replies channel. While it
// waits, a client→agent request is refused method-not-found so the client
// does not wait forever. The reply read itself cannot be cancelled (Python
// parity: the executor's delivery); a closed channel — the read goroutine
// saw end-of-file — reports the client gone.
func (c *serveConn) readReply(wantID int64) (json.RawMessage, error) {
	for msg := range c.replies {
		if msg.Method != "" {
			if msg.ID != nil {
				_ = c.write(rpcResponse{JSONRPC: "2.0", ID: *msg.ID,
					Error: rpcErr(codeMethodNotFound, "method %q is not implemented", msg.Method)})
			}
			continue
		}
		if msg.ID == nil || *msg.ID != wantID {
			continue
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("%w: %s", ErrProtocol, msg.Error.Error())
		}
		return msg.Result, nil
	}
	if err := c.in.Err(); err != nil {
		return nil, fmt.Errorf("%w: client stdin: %v", ErrProtocol, err)
	}
	return nil, fmt.Errorf("%w: client exited before answering request %d", ErrProtocol, wantID)
}

// partsText flattens input parts into one text string (Python parity:
// mappers.tool_result_text): text parts verbatim, data parts as JSON, binary
// parts named — their bytes have no faithful text form and must not be
// smuggled into a text block.
func partsText(parts []events.Input) string {
	var b strings.Builder
	for _, part := range parts {
		switch p := events.AsValue(part).(type) {
		case events.TextInput:
			b.WriteString(p.Content)
		case events.DataInput:
			if data, err := json.Marshal(p.Data); err == nil {
				b.Write(data)
			} else {
				fmt.Fprintf(&b, "%v", p.Data)
			}
		default:
			fmt.Fprintf(&b, "[%s]", part.EventType())
		}
	}
	return b.String()
}

// resultText flattens a tool result's parts into the text ACP carries in a
// tool_call_update's content.
func resultText(r events.ToolResult) string { return partsText(r.Parts) }

// usageTotal is the best available stand-in for a usage_update's "used": the
// event's total, or the input+output sum when the total was not reported.
func usageTotal(u events.Usage) int64 {
	if u.TotalTokens != nil {
		return *u.TotalTokens
	}
	var sum int64
	if u.PromptTokens != nil {
		sum += *u.PromptTokens
	}
	if u.CompletionTokens != nil {
		sum += *u.CompletionTokens
	}
	return sum
}

// historyToUpdates retells a session's stored history as the session/updates
// of a session/load (Python parity: mappers.history_to_session_updates).
// Storage holds what the agent loop persisted, not what a live turn streamed,
// and the two differ in the ways the Python mapper bridges:
//
//   - text goes out live as transient ModelMessageChunks that are never
//     stored; the stored ModelResponse carries the whole message, so replay
//     emits one agent_message_chunk per response that has one;
//   - a user turn is never sent live at all — the client typed it — so
//     replay emits it as user_message_chunks, one per input part;
//   - a tool call is persisted inside the ModelResponse, again as the batch
//     ToolCallsEvent, and its result once in the batch ToolResultsEvent;
//     replay starts each call once and settles it once, at whichever record
//     it meets first, and never settles a call it did not start.
//
// Every chunk carries a messageId so a client can group chunks into messages;
// ids count per role from the start of the history, so replaying the same
// history twice yields the same ids. Not replayed: UsageEvent (telemetry),
// and anything else with no ACP counterpart.
func historyToUpdates(evs []events.Event, sessionID string) []Update {
	var updates []Update
	started := map[string]bool{}
	settled := map[string]bool{}
	userTurns, agentTurns := 0, 0

	start := func(call events.ToolCallEvent) {
		if started[call.ID] {
			return
		}
		started[call.ID] = true
		updates = append(updates, Update{Kind: "tool_call", ToolCallID: call.ID,
			Title: call.Name, ToolKind: KindOther, Status: StatusInProgress,
			RawInput: json.RawMessage(call.Arguments)})
	}
	settle := func(res events.ToolResultEvent) {
		if !started[res.ParentID] || settled[res.ParentID] {
			return
		}
		settled[res.ParentID] = true
		updates = append(updates, Update{Kind: "tool_call_update", ToolCallID: res.ParentID,
			Title: res.Name, Status: StatusCompleted, ContentText: resultText(res.Result)})
	}
	settleFailed := func(res events.ToolErrorEvent) {
		if !started[res.ParentID] || settled[res.ParentID] {
			return
		}
		settled[res.ParentID] = true
		updates = append(updates, Update{Kind: "tool_call_update", ToolCallID: res.ParentID,
			Title: res.Name, Status: StatusFailed, ContentText: res.Error})
	}

	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.ModelRequest:
			userTurns++
			for _, part := range e.Parts {
				updates = append(updates, Update{Kind: "user_message_chunk",
					MessageID: fmt.Sprintf("%s:u%d", sessionID, userTurns), Text: inputText(part)})
			}
		case events.ModelResponse:
			if e.Message != nil && e.Message.Content != "" {
				agentTurns++
				updates = append(updates, Update{Kind: "agent_message_chunk",
					MessageID: fmt.Sprintf("%s:a%d", sessionID, agentTurns), Text: e.Message.Content})
			}
			for _, call := range e.ToolCalls.Calls {
				start(call)
			}
		case events.ToolCallsEvent:
			for _, call := range e.Calls {
				start(call)
			}
		case events.ToolCallEvent:
			start(e)
		case events.ToolResultsEvent:
			for _, res := range e.Results {
				settle(res)
			}
		case events.ToolNotFoundEvent:
			settleFailed(e.ToolErrorEvent)
		case events.ToolErrorEvent:
			settleFailed(e)
		case events.ToolResultEvent:
			settle(e)
		}
	}
	return updates
}

// inputText renders one stored input part as the text of a replayed
// user_message_chunk (Python parity: mappers.input_to_block, reduced to the
// subset's text-only blocks): text verbatim, data as JSON, everything else
// named rather than reproduced.
func inputText(part events.Input) string {
	switch p := events.AsValue(part).(type) {
	case events.TextInput:
		return p.Content
	case events.DataInput:
		if data, err := json.Marshal(p.Data); err == nil {
			return string(data)
		}
		return fmt.Sprintf("%v", p.Data)
	case events.BinaryInput:
		return "[" + string(p.Kind) + "]"
	case events.UrlInput:
		return "[" + string(p.Kind) + "] " + p.URL
	case events.FileIdInput:
		if p.Filename != "" {
			return "[file] " + p.Filename
		}
		return "[file] " + p.FileID
	default:
		return "[" + part.EventType() + "]"
	}
}

// ServeAgent serves one *agent.Agent (convenience over Serve): every
// session/prompt runs one agent turn — the first prompt on a session starts a
// fresh conversation, later prompts continue it — and the turn's events
// project onto the session/update vocabulary: ModelMessageChunk deltas as
// agent_message_chunk while the turn runs, tool calls and their results as
// tool_call / tool_call_update, reasoning as agent_thought_chunk when
// StreamThoughts is on, token telemetry as usage_update when ContextWindow is
// set. cfg.Handle is built here; set AgentName/AgentVersion/OnSession on cfg
// before calling.
//
// AGO-DEVIATION: the Python executor drives Agent._execute on the session's
// own stream with per-session variables isolation and history repair after a
// failed turn; the subset runs plain Ask turns and keeps only per-session
// conversation continuity (docs/DECISIONS.md D-052). The per-session usage
// total for usage_update sums the session's reported events; "tokens in
// context" and a per-call "failed" status are not distinguishable in the
// batch result events the AGo loop emits (docs/DECISIONS.md D-066).
func ServeAgent(a *agent.Agent, cfg ServeConfig) error {
	if a == nil {
		return errors.New("acp: ServeAgent needs an agent")
	}
	return serveAgentIO(os.Stdin, os.Stdout, a, cfg)
}

// serveAgentIO is ServeAgent over explicit streams; the tests drive it through
// pipes.
func serveAgentIO(in io.Reader, out io.Writer, a *agent.Agent, cfg ServeConfig) error {
	// The serve loop is the only accessor: Handle and History both run inside
	// dispatch, one request at a time.
	replies := map[string]*agent.AgentReply{}
	// Per-session running token total, the "used" of usage_update.
	usageTotals := map[string]events.Usage{}

	cfg.Handle = func(ctx context.Context, sessionID, userText string, emit *Emitter) (string, error) {
		var run *agent.AgentRun
		var err error
		if prev := replies[sessionID]; prev != nil {
			run, err = prev.Run(ctx, userText)
		} else {
			run, err = a.Run(ctx, userText)
		}
		if err != nil {
			return "", fmt.Errorf("acp: start turn: %w", err)
		}
		defer run.Close()

		sub := run.Stream().Subscribe(func(ev events.Event) error {
			// The live projection maps what the turn loop sends as it runs:
			// per-call (loose) tool events, not the batch records — the batch
			// ToolCallsEvent/ToolResultsEvent are the loop's own bookkeeping
			// and would double every call on the wire (Python parity: the
			// executor maps ToolCallEvent/ToolResultEvent only).
			switch e := events.AsValue(ev).(type) {
			case events.ModelMessageChunk:
				emit.Message(e.Content)
			case events.ModelReasoning:
				if cfg.StreamThoughts {
					emit.Thought(e.Content)
				}
			case events.ToolCallEvent:
				emit.ToolCall(e.ID, e.Name, KindOther, StatusInProgress, e.Arguments)
			case events.ToolNotFoundEvent:
				text := e.Error
				if text == "" {
					text = resultText(e.ToolResultEvent.Result)
				}
				emit.ToolCallUpdate(e.ParentID, e.Name, StatusFailed, text)
			case events.ToolErrorEvent:
				emit.ToolCallUpdate(e.ParentID, e.Name, StatusFailed, e.Error)
			case events.ToolResultEvent:
				emit.ToolCallUpdate(e.ParentID, e.Name, StatusCompleted, resultText(e.Result))
			case events.UsageEvent:
				if cfg.ContextWindow > 0 {
					total := usageTotals[sessionID].Add(e.Usage)
					usageTotals[sessionID] = total
					emit.Usage(usageTotal(total), int64(cfg.ContextWindow), nil)
				}
			}
			return nil
		}, stream.SubscribeOptions{})
		defer run.Stream().Unsubscribe(sub)

		// The final text — streamed chunk by chunk when the provider streams,
		// sent whole by runPrompt when it does not — is deduped by runPrompt.
		reply, err := run.Result(ctx)
		if err != nil {
			return "", err
		}
		replies[sessionID] = reply
		return reply.Body(), nil
	}
	cfg.History = func(sessionID string) ([]events.Event, error) {
		reply := replies[sessionID]
		if reply == nil {
			return nil, nil
		}
		return reply.History().GetEvents()
	}
	return serveIO(in, out, cfg)
}
