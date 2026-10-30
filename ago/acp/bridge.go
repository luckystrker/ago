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
	"time"
)

// AgentInfo is what an agent advertised in the initialize handshake.
type AgentInfo struct {
	ProtocolVersion int    `json:"protocolVersion"`
	Name            string `json:"name"`
	Title           string `json:"title"`
	Version         string `json:"version"`
}

// Bridge drives an external ACP agent running as a subprocess (the consume
// half of the protocol: a CLI coding agent in ACP mode). NewBridge launches
// the process and performs the initialize handshake; SessionNew and Prompt
// then drive one session each call. agent_message_chunk updates streamed while
// a prompt runs accumulate into that prompt's return value.
//
// Operations serialize under the bridge's lock — at most one exchange in
// flight — so a Bridge is safe for concurrent use. The agent→client requests
// that arrive while a reply is awaited (session/request_permission,
// elicitation/create) are answered inline from the Permission / Elicit
// policies; a client that sends anything else gets method-not-found rather
// than a hang.
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

	// Info is what the agent advertised in the initialize handshake.
	Info AgentInfo

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	closed bool
	nextID int64
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

	// Handshake: version offer, client capabilities, client identity. The
	// agent's answer is recorded, not negotiated further (Python parity:
	// session.ensure — the serve side owns version negotiation).
	b.nextID++
	res, err := b.exchange(b.nextID, "initialize", map[string]any{
		"protocolVersion":    protocolVersion,
		"clientCapabilities": map[string]any{},
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

// SessionNew creates a session in the agent's cwd and returns its id. The
// session's mcp_servers and additional_directories are part of the full
// protocol; the subset sends only cwd.
func (b *Bridge) SessionNew(cwd string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return "", ErrClosed
	}
	b.nextID++
	res, err := b.exchange(b.nextID, "session/new", map[string]any{"cwd": cwd}, nil)
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
// nothing returns "".
func (b *Bridge) Prompt(sessionID, text string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return "", ErrClosed
	}
	b.nextID++
	var chunks []string
	if _, err := b.exchange(b.nextID, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []contentBlock{textBlock(text)},
	}, &chunks); err != nil {
		return "", err
	}
	return strings.Join(chunks, ""), nil
}

// Close terminates the subprocess: closing stdin lets a well-behaved agent
// exit; past killAfter it is killed. Idempotent; operations afterwards report
// ErrClosed.
func (b *Bridge) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
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
	if err := writeLine(b.stdin, rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		return nil, err
	}
	return b.readReply(id, chunks)
}

// readReply scans until the reply with wantID arrives. Everything else the
// agent sends while the reply is awaited is handled here: session/update
// notifications accumulate into chunks (during a prompt) or are dropped, and
// an agent→client request is answered from the policies so the agent does not
// wait forever.
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
			} else if chunks != nil && msg.Method == "session/update" {
				if u := decodeUpdate(msg.Params); u != nil && u.Update.SessionUpdate == "agent_message_chunk" {
					*chunks = append(*chunks, u.Update.text())
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

// answerRequest responds to one agent→client request. The two the subset
// implements are session/request_permission and elicitation/create; anything
// else is refused method-not-found (mediated fs, terminal — deferred, see
// docs/DECISIONS.md D-052).
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
	default:
		resp.Error = rpcErr(codeMethodNotFound, "method %q is not implemented by the client", method)
	}
	// A write failure here cannot be returned anywhere; the exchange in flight
	// fails on its own read the moment the pipe is truly gone.
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
