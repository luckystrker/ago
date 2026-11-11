package acp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/tools"
)

// --- sessions ----------------------------------------------------------------

// providerSession is one live ACP connection and session bound to one
// conversation stream.
type providerSession struct {
	cfg      *Config
	b        *Bridge
	id       string
	model    string
	host     *toolHost
	external []MCPServer
	sent     int // ModelRequests already forwarded (high-water mark)

	closeOnce sync.Once
	closeErr  error

	mu   sync.Mutex
	turn *turnState
}

// turnState is the per-turn side channel between the Bridge callbacks (which
// run on the goroutine reading the agent's stdout) and the Call waiting on
// the turn.
type turnState struct {
	ctx  context.Context
	conv *agocontext.Context

	mu       sync.Mutex
	text     strings.Builder
	files    []events.BinaryResult
	worked   bool
	chanErr  error
	failed   chan struct{}
	failOnce sync.Once
}

func newTurnState(ctx context.Context, conv *agocontext.Context) *turnState {
	return &turnState{ctx: ctx, conv: conv, failed: make(chan struct{})}
}

// fail records that the turn's human-input channel failed and wakes the
// waiter. The first failure wins: it is the one that describes what happened.
func (t *turnState) fail(err error) {
	t.failOnce.Do(func() {
		t.mu.Lock()
		t.chanErr = err
		t.mu.Unlock()
		close(t.failed)
	})
}

func (t *turnState) failure() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.chanErr
}

func (t *turnState) snapshot() (text string, files []events.BinaryResult, worked bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.text.String(), append([]events.BinaryResult(nil), t.files...), t.worked
}

func (s *providerSession) current() *turnState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turn
}

func (s *providerSession) setTurn(t *turnState) {
	s.mu.Lock()
	s.turn = t
	s.mu.Unlock()
}

// close ends the session: the tool host, then the subprocess. Idempotent.
func (s *providerSession) close() error {
	s.closeOnce.Do(func() {
		if s.host != nil {
			s.host.close()
		}
		if s.b != nil {
			s.closeErr = s.b.Close()
		}
	})
	return s.closeErr
}

// kill stops the subprocess without waiting for the exchange parked on it,
// then ends the session; used when a turn cannot be ended cooperatively.
func (s *providerSession) kill() {
	s.b.kill()
	_ = s.close()
}

// onUpdate maps one session/update onto the run's event stream (Python
// parity: mappers.map_session_update via BridgeState.handle_update).
func (s *providerSession) onUpdate(u Update) {
	t := s.current()
	if t == nil {
		return
	}
	var ev events.Event
	switch u.Kind {
	case "agent_message_chunk":
		t.mu.Lock()
		t.text.WriteString(u.Text)
		if len(u.Data) > 0 {
			// An image or audio block is a file of the reply (Python parity:
			// block_to_files → ModelResponse.files), not message text.
			t.files = append(t.files, events.BinaryResult{Data: u.Data, Metadata: map[string]any{"mimeType": u.MimeType}})
		}
		t.mu.Unlock()
		if u.Text == "" {
			return
		}
		ev = events.ModelMessageChunk{Content: u.Text}
	case "agent_thought_chunk":
		t.markWorked()
		ev = events.ModelReasoning{Content: u.Text}
	case "tool_call":
		t.markWorked()
		args := "{}"
		if len(u.RawInput) > 0 && string(u.RawInput) != "null" {
			args = string(u.RawInput)
		}
		name := u.Title
		if name == "" {
			name = "tool"
		}
		ev = events.BuiltinToolCallEvent{ToolCallEvent: events.ToolCallEvent{ID: u.ToolCallID, Name: name, Arguments: args}}
	case "tool_call_update":
		t.markWorked()
		text := u.ContentText
		if text == "" {
			text = u.Status
		}
		ev = events.BuiltinToolResultEvent{ToolResultEvent: events.ToolResultEvent{
			ParentID: u.ToolCallID, Name: u.Title,
			Result: events.ToolResult{Parts: []events.Input{events.TextInput{Content: text}}},
		}}
	case "plan":
		t.markWorked()
		ev = PlanEvent{Entries: u.Entries}
	default:
		// Mode and command updates have typed events; usage_update and
		// anything newer have no AGo representation and are dropped (Python
		// parity: map_session_update returns None).
		if e, ok := EventOf(u); ok {
			ev = e
		}
	}
	if ev == nil {
		return
	}
	// A failing subscriber must not break the agent's turn.
	_ = t.conv.Send(ev)
}

func (t *turnState) markWorked() {
	t.mu.Lock()
	t.worked = true
	t.mu.Unlock()
}

var affirmative = map[string]bool{
	"y": true, "yes": true, "allow": true, "allow_once": true, "allow_always": true, "ok": true, "approve": true,
}

// permission answers a session/request_permission (and the consent for the
// mediated fs writes and terminal launches) per PermissionPolicy.
func (s *providerSession) permission(toolCall, input string) bool {
	switch s.cfg.PermissionPolicy {
	case PermissionAuto:
		return true
	case PermissionDeny:
		return false
	}
	t := s.current()
	if t == nil {
		return false
	}
	title := toolCall
	if title == "" {
		title = "an action"
	}
	answer, err := t.conv.Input(t.ctx, "Agent requests permission for: "+title+"\nAllow? (yes/no)", 0)
	if err != nil {
		t.fail(err)
		return false
	}
	return affirmative[strings.ToLower(strings.TrimSpace(answer))]
}

// elicit answers an elicitation/create per ElicitationPolicy.
func (s *providerSession) elicit(message string) (string, bool) {
	if s.cfg.ElicitationPolicy == ElicitationDecline {
		return "", false
	}
	t := s.current()
	if t == nil {
		return "", false
	}
	answer, err := t.conv.Input(t.ctx, "Agent asks: "+message, 0)
	if err != nil {
		t.fail(err)
		return "", false
	}
	return answer, true
}

// onChannelFailure is the tool host's report of a tool that needed a person.
func (s *providerSession) onChannelFailure(err error) {
	if t := s.current(); t != nil {
		t.fail(err)
	}
}

// sessionFor returns the live session for the conversation, starting one on
// the first turn and re-validating the tools against it afterwards.
func (c *Config) sessionFor(ctx context.Context, conv *agocontext.Context, functions []tools.Schema, external []MCPServer) (*providerSession, error) {
	key := conv.Stream.ID()
	c.mu.Lock()
	s := c.sessions[key]
	c.mu.Unlock()
	if s != nil {
		if err := s.refreshTools(functions, external); err != nil {
			return nil, err
		}
		return s, nil
	}

	s, err := c.startSession(ctx, functions, external)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.sessions == nil {
		c.sessions = map[string]*providerSession{}
	}
	if prior := c.sessions[key]; prior != nil {
		// Two first turns raced on one stream; keep the earlier session.
		c.mu.Unlock()
		_ = s.close()
		return prior, nil
	}
	c.sessions[key] = s
	c.mu.Unlock()
	return s, nil
}

// dropSession forgets a session the connection to which is gone.
func (c *Config) dropSession(conv *agocontext.Context, s *providerSession) {
	c.mu.Lock()
	if c.sessions[conv.Stream.ID()] == s {
		delete(c.sessions, conv.Stream.ID())
	}
	c.mu.Unlock()
}

// refreshTools re-validates one turn's tools against the session created on
// turn one: function tools hot-update the host's served list; anything ACP
// cannot change mid-session (the external server set, the host's existence)
// is an error (Python parity: _refresh_tools).
func (s *providerSession) refreshTools(functions []tools.Schema, external []MCPServer) error {
	if s.cfg.DisableTools {
		return nil
	}
	if !sameServers(external, s.external) {
		return errors.New("acp: the run's MCPServerTool set changed after the ACP session was created; " +
			"ACP fixes mcp_servers at session/new — keep the set stable for the whole run")
	}
	switch {
	case s.host != nil:
		s.host.setTools(functions)
	case len(functions) > 0:
		return errors.New("acp: function tools appeared after the ACP session was created without a tool host; " +
			"ACP fixes mcp_servers at session/new — provide the tools on the first turn")
	}
	return nil
}

func sameServers(a, b []MCPServer) bool {
	if len(a) != len(b) {
		return false
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}
