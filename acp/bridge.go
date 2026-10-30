package acp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// AgentInfo is what an agent advertised in the initialize handshake.
type AgentInfo struct {
	ProtocolVersion int    `json:"protocolVersion"`
	Name            string `json:"name"`
	Title           string `json:"title"`
	Version         string `json:"version"`
}

// TurnResult is one prompt turn's outcome: the agent_message_chunk text that
// streamed while it ran, and the stop reason the agent answered with
// ("end_turn", or "cancelled" after a session/cancel — Python parity:
// PromptResponse.stop_reason accumulated by bridge.handle_update).
type TurnResult struct {
	Text       string
	StopReason string
}

// Bridge drives an external ACP agent running as a subprocess (the consume
// half of the protocol: a CLI coding agent in ACP mode). NewBridge launches
// the process and performs the initialize handshake; SessionNew and Prompt
// then drive one session each call. agent_message_chunk updates streamed while
// a prompt run accumulate into that prompt's return value; every other
// session/update the agent streams reaches OnUpdate.
//
// Operations serialize under the bridge's lock — at most one exchange in
// flight — so a Bridge is safe for concurrent use, with one deliberate
// exception: Cancel writes a notification without taking the lock, so it can
// reach the agent while Prompt is parked on the reply. The agent→client
// requests that arrive while a reply is awaited (session/request_permission,
// elicitation/create, and the mediated fs/* and terminal/* when FS is set)
// are answered inline from the Permission / Elicit policies and the
// filesystem root; a client that sends anything else gets method-not-found
// rather than a hang.
//
// AGO-DEVIATION: the exported operations carry no context.Context (the porting
// brief pins the signatures); they run on context.Background().
type Bridge struct {
	// Permission decides the agent's session/request_permission requests.
	// A nil policy — or a false answer — denies. An allowed request selects
	// the agent's first allow_once/allow_always option (Python parity:
	// permissions.resolve_permission_option_id).
	Permission func(toolCall, input string) bool

	// Elicit decides the agent's elicitation/create questions: the answer text
	// and whether the question is taken. A nil policy — or ok=false — declines;
	// nothing is ever fabricated on the user's behalf.
	Elicit func(message string) (answer string, ok bool)

	// OnUpdate, when set, receives every session/update the agent streams —
	// thought chunks, tool calls, plans, usage — while exchanges run. It is
	// called under the bridge's lock: it must not call back into the Bridge.
	OnUpdate func(u Update)

	// FS enables the mediated filesystem and terminals the agent may reach
	// fs/* and terminal/* for, confined under Root (empty: this process's
	// working directory — Python parity: config.fs_root or config.cwd). nil,
	// the default, refuses every fs/* and terminal/* request with
	// method-not-found: nothing is ever advertised that does not work.
	FS *FSConfig

	// Info is what the agent advertised in the initialize handshake.
	Info AgentInfo

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	// closed is atomic so Cancel can check it without b.mu: the whole point
	// of Cancel is to write while Prompt holds b.mu parked on the reply.
	closed atomic.Bool
	nextID int64

	// writeMu serializes writes to the agent's stdin. The exchanges write
	// under b.mu; Cancel writes without it, which is what lets a cancel land
	// while a prompt is in flight.
	writeMu sync.Mutex

	termsOnce sync.Once
	terms     *terminalManager
}

// NewBridge launches cmd with args and env as a subprocess and runs the
// initialize handshake. env is the child's complete environment (os.Environ()
// is inherited when nil). An agent that fails to launch, exits or answers the
// handshake with an error fails NewBridge with the process already torn down.
func NewBridge(cmd string, args []string, env []string) (*Bridge, error) {
	if cmd == "" {
		return nil, fmt.Errorf("%w: cmd is empty", ErrNotProvided)
	}
	b := &Bridge{}
	if err := b.start(cmd, args, env); err != nil {
		return nil, err
	}
	return b, nil
}

// start launches the process and handshakes. The encoding is UTF-8 by fiat
// (os/exec pipes bytes).
func (b *Bridge) start(cmdName string, args []string, env []string) error {
	cmd := exec.Command(cmdName, args...)
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = env
	// Agent stderr is diagnostics the bridge discards; keeping it silent
	// avoids interleaving it into a conversation's log stream.
	cmd.Stderr = io.Discard

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("acp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("acp: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("acp: launch %s: %w", cmdName, err)
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), scanBuffer)
	b.cmd, b.stdin, b.stdout = cmd, stdin, sc
	if b.FS != nil {
		root, rerr := b.FS.root()
		if rerr != nil {
			b.stop()
			return fmt.Errorf("acp: resolve fs root: %w", rerr)
		}
		b.terms = newTerminalManager(root)
	}

	// Handshake: version offer, client capabilities, client identity. The
	// agent's answer is recorded, not negotiated further (Python parity:
	// session.ensure — the serve side owns version negotiation).
	b.nextID++
	res, err := b.exchange(b.nextID, "initialize", map[string]any{
		"protocolVersion":    protocolVersion,
		"clientCapabilities": clientCapabilities(b),
		"clientInfo":         map[string]any{"name": clientName, "version": clientVersion},
	}, nil)
	if err != nil {
		b.stop()
		return err
	}
	var parsed struct {
		ProtocolVersion *int `json:"protocolVersion"`
		AgentInfo       struct {
			Name    string `json:"name"`
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"agentInfo"`
	}
	if err := json.Unmarshal(res, &parsed); err != nil {
		b.stop()
		return fmt.Errorf("%w: initialize result: %v", ErrProtocol, err)
	}
	version := protocolVersion
	if parsed.ProtocolVersion != nil {
		version = *parsed.ProtocolVersion
	}
	b.Info = AgentInfo{
		ProtocolVersion: version,
		Name:            parsed.AgentInfo.Name,
		Title:           parsed.AgentInfo.Title,
		Version:         parsed.AgentInfo.Version,
	}
	return nil
}

// clientCapabilities is the client role's honest advertisement: fs
// read/write only when the mediated filesystem is enabled (Python parity:
// client.py FileSystemCapabilities(read_text_file=True, write_text_file=True)
// for an agent that wires it). Truthful on the deny side too — the mediated
// methods answer method-not-found when not enabled, so they must not be
// advertised.
func clientCapabilities(b *Bridge) map[string]any {
	caps := map[string]any{}
	if b != nil && b.FS != nil {
		caps["fs"] = map[string]any{"readTextFile": true, "writeTextFile": true}
	}
	return caps
}

// SessionNew creates a session in the agent's cwd and returns its id. The
// session's additional_directories are part of the full protocol; the subset
// sends only cwd. To also announce MCP servers the agent may reach, use
// SessionNewWithServers.
func (b *Bridge) SessionNew(cwd string) (string, error) {
	return b.sessionNew(declaredServersParams{CWD: cwd})
}

// SessionNewWithServers creates a session and announces MCP servers to the
// agent (Python parity: new_session's mcp_servers — HTTP entries; the SDK's
// session.ensure takes acp.schema.HttpMcpServer only). A stdio entry names a
// command only the declarer can run — a subprocess agent cannot dial it — so
// it is refused with ErrUndeclarableServer rather than silently useless.
func (b *Bridge) SessionNewWithServers(cwd string, servers []MCPServer) (string, error) {
	for _, s := range servers {
		if err := declarable(s); err != nil {
			return "", err
		}
	}
	return b.sessionNew(declaredServersParams{CWD: cwd, MCPServers: servers})
}

// sessionNew sends one session/new and decodes the id. Caller holds b.mu.
func (b *Bridge) sessionNew(params declaredServersParams) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed.Load() {
		return "", ErrClosed
	}
	b.nextID++
	res, err := b.exchange(b.nextID, "session/new", params, nil)
	if err != nil {
		return "", err
	}
	var parsed struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(res, &parsed); err != nil {
		return "", fmt.Errorf("%w: session/new result: %v", ErrProtocol, err)
	}
	if parsed.SessionID == "" {
		return "", fmt.Errorf("%w: session/new returned no sessionId", ErrProtocol)
	}
	return parsed.SessionID, nil
}

// Prompt sends one user turn (a single text block) and returns the agent's
// answer: the agent_message_chunk text streamed while the turn ran (Python
// parity: bridge.handle_update accumulating _turn_parts). A turn that streams
// nothing returns "". The stop reason is discarded; use PromptTurn to see it.
func (b *Bridge) Prompt(sessionID, text string) (string, error) {
	tr, err := b.PromptTurn(sessionID, text)
	if err != nil {
		return "", err
	}
	return tr.Text, nil
}

// PromptTurn sends one user turn and returns its text and stop reason. After
// a Cancel the stop reason is whatever the agent answered — a conforming
// agent reports "cancelled" (Python parity: agent.py prompt returns
// PromptResponse(stop_reason="cancelled") for a cancelled turn).
func (b *Bridge) PromptTurn(sessionID, text string) (TurnResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed.Load() {
		return TurnResult{}, ErrClosed
	}
	b.nextID++
	var chunks []string
	res, err := b.exchange(b.nextID, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []contentBlock{textBlock(text)},
	}, &chunks)
	if err != nil {
		return TurnResult{}, err
	}
	var parsed struct {
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(res, &parsed); err != nil {
		return TurnResult{}, fmt.Errorf("%w: session/prompt result: %v", ErrProtocol, err)
	}
	if parsed.StopReason == "" {
		parsed.StopReason = StopEndTurn
	}
	return TurnResult{Text: strings.Join(chunks, ""), StopReason: parsed.StopReason}, nil
}

// Cancel asks the agent to stop sessionID's running turn and everything
// queued behind it (Python parity: the client side of session/cancel — a
// notification, no reply channel). The prompt in flight returns on its own
// with the agent's answer; a conforming agent reports the cancelled stop
// reason and keeps the already-streamed events in its history. A cancel for
// an unknown session, or with no turn running, is a no-op on the agent side.
//
// Cancel does not take the bridge's operation lock — that lock is held by
// Prompt while the turn runs, and the whole point is to reach the agent while
// it holds the floor.
func (b *Bridge) Cancel(sessionID string) error {
	if b.closed.Load() {
		return ErrClosed
	}
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	return writeLine(b.stdin, rpcNotification{JSONRPC: "2.0", Method: "session/cancel",
		Params: map[string]any{"sessionId": sessionID}})
}

// Load reattaches to a session by id and lets the agent replay its stored
// history as session/update notifications before the response (Python
// parity: load_session — "a Client may treat the response as the line between
// history and whatever comes next"). The replay reaches OnUpdate; this
// returns once the agent has answered. A session the agent does not know —
// or still has a turn running on — is refused by the agent with a protocol
// error.
func (b *Bridge) Load(sessionID, cwd string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed.Load() {
		return ErrClosed
	}
	b.nextID++
	_, err := b.exchange(b.nextID, "session/load", map[string]any{
		"sessionId": sessionID,
		"cwd":       cwd,
	}, nil)
	return err
}

// Close terminates the subprocess: closing stdin lets a well-behaved agent
// exit; past killAfter it is killed. Idempotent; operations afterwards report
// ErrClosed.
func (b *Bridge) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed.Load() {
		return nil
	}
	b.closed.Store(true)
	b.stop()
	return nil
}

// stop tears the process down without touching the closed flag (also used for
// a failed handshake, before the Bridge was ever handed out).
func (b *Bridge) stop() {
	if b.stdin != nil {
		b.stdin.Close()
	}
	if b.cmd != nil && b.cmd.Process != nil {
		done := make(chan struct{})
		go func() { _ = b.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(killAfter):
			_ = b.cmd.Process.Kill()
			<-done
		}
	}
}

// exchange writes one request and reads its reply. Caller holds b.mu.
func (b *Bridge) exchange(id int64, method string, params any, chunks *[]string) (json.RawMessage, error) {
	b.writeMu.Lock()
	err := writeLine(b.stdin, rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	b.writeMu.Unlock()
	if err != nil {
		return nil, err
	}
	return b.readReply(id, chunks)
}

// readReply scans until the reply with wantID arrives. Everything else the
// agent sends while the reply is awaited is handled here: session/update
// notifications reach OnUpdate (and accumulate into chunks during a prompt),
// and an agent→client request is answered from the policies and the mediated
// filesystem so the agent does not wait forever.
func (b *Bridge) readReply(wantID int64, chunks *[]string) (json.RawMessage, error) {
	for b.stdout.Scan() {
		line := bytes.TrimSpace(b.stdout.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg incoming
		if err := json.Unmarshal(line, &msg); err != nil {
			// A garbled line is skipped: a chatty banner must not kill the
			// session, and the awaited reply may still follow.
			continue
		}
		if msg.Method != "" {
			if msg.ID != nil {
				b.answerRequest(*msg.ID, msg.Method, msg.Params)
			} else if msg.Method == "session/update" {
				if u := decodeUpdate(msg.Params); u != nil {
					if b.OnUpdate != nil {
						b.OnUpdate(u.Update)
					}
					if chunks != nil && u.Update.Kind == "agent_message_chunk" {
						*chunks = append(*chunks, u.Update.text())
					}
				}
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
	if err := b.stdout.Err(); err != nil {
		return nil, fmt.Errorf("%w: agent stdout: %v", ErrProtocol, err)
	}
	return nil, fmt.Errorf("%w: agent exited before answering request %d", ErrProtocol, wantID)
}

// answerRequest responds to one agent→client request: the permission and
// elicitation policies, and — when FS is configured — the mediated
// filesystem and terminals. Anything else is refused method-not-found, the
// answer a client owes for a method it never advertised (ACP error doc).
func (b *Bridge) answerRequest(id int64, method string, params json.RawMessage) {
	resp := rpcResponse{JSONRPC: "2.0", ID: id}
	switch method {
	case "session/request_permission":
		var p requestPermissionParams
		if err := json.Unmarshal(params, &p); err != nil {
			resp.Error = rpcErr(codeInvalidParams, "session/request_permission params: %v", err)
			break
		}
		resp.Result = b.permissionAnswer(p)
	case "elicitation/create":
		var p elicitationParams
		if err := json.Unmarshal(params, &p); err != nil {
			resp.Error = rpcErr(codeInvalidParams, "elicitation/create params: %v", err)
			break
		}
		resp.Result = b.elicitationAnswer(p)
	case "fs/read_text_file":
		var p readTextFileParams
		if err := json.Unmarshal(params, &p); err != nil {
			resp.Error = rpcErr(codeInvalidParams, "fs/read_text_file params: %v", err)
			break
		}
		if res, rpcErr := b.fsRead(p); rpcErr != nil {
			resp.Error = rpcErr
		} else {
			resp.Result = res
		}
	case "fs/write_text_file":
		var p writeTextFileParams
		if err := json.Unmarshal(params, &p); err != nil {
			resp.Error = rpcErr(codeInvalidParams, "fs/write_text_file params: %v", err)
			break
		}
		if rpcErr := b.fsWrite(p); rpcErr != nil {
			resp.Error = rpcErr
		} else {
			resp.Result = map[string]any{}
		}
	case "terminal/create":
		var p createTerminalParams
		if err := json.Unmarshal(params, &p); err != nil {
			resp.Error = rpcErr(codeInvalidParams, "terminal/create params: %v", err)
			break
		}
		if res, rpcErr := b.fsCreateTerminal(p); rpcErr != nil {
			resp.Error = rpcErr
		} else {
			resp.Result = res
		}
	case "terminal/output":
		var p terminalIDParams
		if err := json.Unmarshal(params, &p); err != nil {
			resp.Error = rpcErr(codeInvalidParams, "terminal/output params: %v", err)
			break
		}
		if res, rpcErr := b.terminalOutput(p); rpcErr != nil {
			resp.Error = rpcErr
		} else {
			resp.Result = res
		}
	case "terminal/wait_for_exit":
		var p terminalIDParams
		if err := json.Unmarshal(params, &p); err != nil {
			resp.Error = rpcErr(codeInvalidParams, "terminal/wait_for_exit params: %v", err)
			break
		}
		if res, rpcErr := b.terminalWait(p); rpcErr != nil {
			resp.Error = rpcErr
		} else {
			resp.Result = res
		}
	case "terminal/kill":
		var p terminalIDParams
		if err := json.Unmarshal(params, &p); err != nil {
			resp.Error = rpcErr(codeInvalidParams, "terminal/kill params: %v", err)
			break
		}
		if rpcErr := b.terminalKill(p); rpcErr != nil {
			resp.Error = rpcErr
		} else {
			resp.Result = map[string]any{}
		}
	case "terminal/release":
		var p terminalIDParams
		if err := json.Unmarshal(params, &p); err != nil {
			resp.Error = rpcErr(codeInvalidParams, "terminal/release params: %v", err)
			break
		}
		if rpcErr := b.terminalRelease(p); rpcErr != nil {
			resp.Error = rpcErr
		} else {
			resp.Result = map[string]any{}
		}
	default:
		resp.Error = rpcErr(codeMethodNotFound, "method %q is not implemented by the client", method)
	}
	// A write failure here cannot be returned anywhere; the exchange in flight
	// fails on its own read the moment the pipe is truly gone.
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	_ = writeLine(b.stdin, resp)
}

// permissionAnswer resolves the agent's request (Python parity:
// permissions.resolve_permission_option_id wrapped by bridge.py into an
// AllowedOutcome/DeniedOutcome — allow picks the first allow_once/allow_always
// option, deny picks a reject_once/reject_always one the agent actually
// offered, no fitting option cancels). The answer goes on the wire nested
// (Decision.MarshalJSON), so a standard ACP agent accepts it.
func (b *Bridge) permissionAnswer(p requestPermissionParams) Decision {
	if b.Permission != nil && b.Permission(p.ToolCall.Title, p.ToolCall.inputString()) {
		for _, o := range p.Options {
			if o.Kind == kindAllowOnce || o.Kind == kindAllowAlways {
				return Decision{Outcome: OutcomeSelected, OptionID: o.OptionID}
			}
		}
		// Allowed by policy but the agent offered nothing selectable.
		return Decision{Outcome: OutcomeCancelled}
	}
	for _, o := range p.Options {
		if o.Kind == kindRejectOnce || o.Kind == kindRejectAlways {
			return Decision{Outcome: OutcomeSelected, OptionID: o.OptionID}
		}
	}
	return Decision{Outcome: OutcomeCancelled}
}

// elicitationAnswer resolves the agent's question (Python parity:
// elicitation.resolve_elicitation_response with policy "ask"): a policy that
// takes the question accepts with the answer as text content, everything else
// declines. The form/url modes and per-property rendering are deferred
// (docs/DECISIONS.md D-052).
func (b *Bridge) elicitationAnswer(p elicitationParams) elicitationResult {
	if b.Elicit != nil {
		if answer, ok := b.Elicit(p.Message); ok {
			return elicitationResult{Action: ElicitAccept, Content: answer}
		}
	}
	return elicitationResult{Action: ElicitDecline}
}
