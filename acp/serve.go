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

	// Handle runs one prompt on sessionID. userText is the concatenated text of
	// the prompt's text blocks. emit pushes one agent_message_chunk to the
	// client — call it several times to stream. finalText is the turn's answer;
	// when it differs from what was already emitted it goes out as one more
	// chunk, so a handler that never emits still delivers (Python parity:
	// executor._send_final_text — re-sending streamed text would show the
	// reply twice).
	//
	// An error fails the session/prompt request as a JSON-RPC internal error
	// carrying the reason (the client is in another process; an error without
	// its reason leaves whoever is debugging with nothing at all).
	Handle func(sessionID string, userText string, emit func(chunk string)) (finalText string, err error)

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

	// OnSession, when set, is called with the session handle during
	// session/new, before the response (Python parity: the ACPAgent on_session
	// observer). A handler that needs the outbound permission/elicitation
	// requests captures the session here.
	OnSession func(s *Session)
}

// Session is the serve-side handle for one conversation: the id the client
// holds, the working directory it declared, and the outbound agent→client
// requests a handler may raise mid-turn.
type Session struct {
	ID  string
	CWD string

	conn *serveConn
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

// serveConn is one serve-role connection: the config, the input scanner and
// the sessions minted on it.
type serveConn struct {
	cfg ServeConfig
	in  *bufio.Scanner
	out io.Writer

	// writeMu serializes every write to out. The serve loop writes responses,
	// and emit closures run on stream-dispatch goroutines while the loop sits
	// inside Handle — both funnel through here.
	writeMu sync.Mutex

	nextID      atomic.Int64
	sessions    map[string]*Session
	initialized bool
}

// Serve reads ACP JSON-RPC from stdin and writes to stdout until the client
// disconnects (stdin EOF returns nil). One request runs at a time: a slow
// Handle blocks the connection, which is the subset's whole concurrency story
// (per-session queues, cancel and the multi-session store are deferred — see
// docs/DECISIONS.md D-052).
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
	c := &serveConn{cfg: cfg, out: out, sessions: map[string]*Session{}}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), scanBuffer)
	c.in = sc

	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
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
			continue // a reply with nothing awaiting it
		}
		if msg.ID == nil {
			// Notifications (session/cancel and friends) are outside the
			// subset; ignored (docs/DECISIONS.md D-052).
			continue
		}
		result, rpcErr := c.dispatch(msg.Method, msg.Params)
		resp := rpcResponse{JSONRPC: "2.0", ID: *msg.ID}
		if rpcErr != nil {
			resp.Error = rpcErr
		} else {
			resp.Result = result
		}
		if err := c.write(resp); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("acp: read stdin: %w", err)
	}
	return nil
}

// write pushes one message out under the write lock.
func (c *serveConn) write(msg any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeLine(c.out, msg)
}

// dispatch runs one request to completion. It is called on the serve loop
// goroutine; Handle runs inline, so the scanner is exclusively ours while a
// nested readReply (permission/elicitation) waits for the client.
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
		return map[string]any{
			"protocolVersion": version,
			// Truthful advertisement: the subset takes text prompts and
			// nothing else; anything advertised here must actually work.
			"agentCapabilities": map[string]any{
				"promptCapabilities": map[string]any{
					"image":           false,
					"audio":           false,
					"embeddedContext": false,
				},
			},
			"agentInfo": map[string]any{
				"name":    c.cfg.AgentName,
				"version": c.cfg.AgentVersion,
			},
		}, nil

	case "session/new":
		if !c.initialized {
			return nil, rpcErr(codeInvalidRequest, "Call initialize before any session method.")
		}
		var p struct {
			CWD string `json:"cwd"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, rpcErr(codeInvalidParams, "session/new params: %v", err)
		}
		// cwd and any mcpServers in the request are recorded as context and not
		// acted on (Python parity: new_session — a path named by a client is
		// context, not authorization). mcpServers is dropped here outright.
		s := &Session{ID: uuid.New(), CWD: p.CWD, conn: c}
		c.sessions[s.ID] = s
		if c.cfg.OnSession != nil {
			c.cfg.OnSession(s)
		}
		return map[string]any{"sessionId": s.ID}, nil

	case "session/prompt":
		if !c.initialized {
			return nil, rpcErr(codeInvalidRequest, "Call initialize before any session method.")
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
			// Python parity: resource_not_found "acp-session:<id>"
			// (agent.py:690); the code sits in the server-error range.
			return nil, rpcErr(codeSessionNotFound, "acp-session:%s", p.SessionID)
		}

		// One turn, one tracker: what emit put on the wire, so the final text
		// is only sent when it was not (executor parity: _forward_updates +
		// _send_final_text).
		var mu sync.Mutex
		var delivered []string
		emit := func(chunk string) {
			if err := c.write(rpcNotification{JSONRPC: "2.0", Method: "session/update",
				Params: updateParams{SessionID: session.ID, Update: agentMessageChunk(chunk)}}); err != nil {
				return
			}
			mu.Lock()
			delivered = append(delivered, chunk)
			mu.Unlock()
		}

		// Non-text blocks are dropped, their text siblings kept.
		final, err := c.cfg.Handle(session.ID, blocksText(p.Prompt), emit)
		if err != nil {
			return nil, rpcErr(codeInternalError, "%s", err.Error())
		}
		mu.Lock()
		joined := strings.Join(delivered, "")
		mu.Unlock()
		if final != "" && final != joined {
			_ = c.write(rpcNotification{JSONRPC: "2.0", Method: "session/update",
				Params: updateParams{SessionID: session.ID, Update: agentMessageChunk(final)}})
		}
		return map[string]any{"stopReason": "end_turn"}, nil

	default:
		return nil, rpcErr(codeMethodNotFound, "method %q is not implemented", method)
	}
}

// exchange writes one agent→client request and reads its reply. Called from
// inside Handle, so it reads the same scanner the serve loop is parked on.
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

// readReply scans until the reply with wantID arrives. While waiting, a
// client→agent request (mediated fs, terminal — all deferred) is refused
// method-not-found so the client does not wait forever, and notifications are
// dropped: the only notification this side may see mid-turn is the client
// echoing its own session/update, which the subset does not consume.
func (c *serveConn) readReply(wantID int64) (json.RawMessage, error) {
	for c.in.Scan() {
		line := bytes.TrimSpace(c.in.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg incoming
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
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

// ServeAgent serves one *agent.Agent (convenience over Serve): every
// session/prompt runs one agent turn — the first prompt on a session starts a
// fresh conversation, later prompts continue it — and ModelMessageChunk deltas
// stream out as agent_message_chunk updates while the turn runs. cfg.Handle is
// built here; set AgentName/AgentVersion/OnSession on cfg before calling.
//
// AGO-DEVIATION: the Python executor drives Agent._execute on the session's
// own stream with per-session variables isolation and history repair after a
// failed turn; the subset runs plain Ask turns and keeps only per-session
// conversation continuity (docs/DECISIONS.md D-052).
func ServeAgent(a *agent.Agent, cfg ServeConfig) error {
	if a == nil {
		return errors.New("acp: ServeAgent needs an agent")
	}
	return serveAgentIO(os.Stdin, os.Stdout, a, cfg)
}

// serveAgentIO is ServeAgent over explicit streams; the tests drive it through
// pipes.
func serveAgentIO(in io.Reader, out io.Writer, a *agent.Agent, cfg ServeConfig) error {
	replies := map[string]*agent.AgentReply{} // the serve loop is the only accessor
	cfg.Handle = func(sessionID, userText string, emit func(chunk string)) (string, error) {
		ctx := context.Background()
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

		var mu sync.Mutex
		var streamed []string
		sub := run.Stream().Subscribe(func(ev events.Event) error {
			if mc, ok := events.AsValue(ev).(events.ModelMessageChunk); ok {
				mu.Lock()
				streamed = append(streamed, mc.Content)
				mu.Unlock()
				emit(mc.Content)
			}
			return nil
		}, stream.SubscribeOptions{})
		defer run.Stream().Unsubscribe(sub)

		reply, err := run.Result(ctx)
		if err != nil {
			return "", err
		}
		replies[sessionID] = reply
		body := reply.Body()
		mu.Lock()
		joined := strings.Join(streamed, "")
		mu.Unlock()
		if body != "" && body != joined {
			emit(body)
		}
		return body, nil
	}
	return serveIO(in, out, cfg)
}
