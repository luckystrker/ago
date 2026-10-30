package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"ago/agent"
	"ago/config"
	agocontext "ago/context"
	"ago/events"
	agotest "ago/testing"
)

// The serve-role tests run Serve against a fake client in-process: the serve
// loop reads one end of an os.Pipe pair, the test writes requests to the other
// and reads the responses (the same exchange a re-exec'd fake client would
// make, minus a process boundary).
type fakeClient struct {
	t  *testing.T
	w  *bufio.Writer // to the agent
	sc *bufio.Scanner
	id int64

	// updates collects the session/update notifications seen while awaiting
	// the reply of the call in flight (reset per call).
	updates []updateParams

	// requests records every agent→client request the client saw, answered or
	// not, with its decoded params raw for per-test assertions.
	requests []recordedRequest

	// requestHook answers an agent→client request; the returned value becomes
	// the response result. nil answers with an empty result.
	requestHook func(method string, params json.RawMessage) any
}

type recordedRequest struct {
	Method string
	Params json.RawMessage
}

func startServe(t *testing.T, serve func(in io.Reader, out io.Writer) error) *fakeClient {
	t.Helper()
	agentIn, agentInW, err := os.Pipe() // the client's writes are the agent's stdin
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	agentOutR, agentOut, err := os.Pipe() // the agent's stdout is the client's reads
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- serve(agentIn, agentOut) }()

	fc := &fakeClient{t: t, w: bufio.NewWriter(agentInW), sc: bufio.NewScanner(agentOutR)}
	fc.sc.Buffer(make([]byte, 0, 64*1024), scanBuffer)
	t.Cleanup(func() {
		agentInW.Close() // stdin EOF ends the serve loop
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("serve did not return after stdin closed")
		}
		agentIn.Close()
		agentOutR.Close()
		agentOut.Close()
	})
	return fc
}

func (c *fakeClient) call(method string, params any) (json.RawMessage, *rpcError) {
	c.t.Helper()
	c.id++
	c.updates = nil
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": c.id, "method": method, "params": params,
	})
	if err != nil {
		c.t.Fatalf("marshal request: %v", err)
	}
	fmt.Fprintln(c.w, string(payload))
	c.w.Flush()
	return c.await(c.id)
}

// await scans until the reply for wantID: session/update notifications
// accumulate, agent→client requests are recorded, answered via the hook and
// replied inline.
func (c *fakeClient) await(wantID int64) (json.RawMessage, *rpcError) {
	for c.sc.Scan() {
		line := strings.TrimSpace(c.sc.Text())
		if line == "" {
			continue
		}
		var msg incoming
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if msg.Method != "" {
			if msg.ID != nil {
				c.requests = append(c.requests, recordedRequest{Method: msg.Method, Params: msg.Params})
				result := any(map[string]any{})
				if c.requestHook != nil {
					result = c.requestHook(msg.Method, msg.Params)
				}
				payload, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: *msg.ID, Result: result})
				fmt.Fprintln(c.w, string(payload))
				c.w.Flush()
			} else if msg.Method == "session/update" {
				if u := decodeUpdate(msg.Params); u != nil {
					c.updates = append(c.updates, *u)
				}
			}
			continue
		}
		if msg.ID == nil || *msg.ID != wantID {
			continue
		}
		return msg.Result, msg.Error
	}
	c.t.Fatalf("agent stream ended before reply %d", wantID)
	return nil, nil
}

// updateTexts returns the text of the collected agent_message_chunk updates.
func (c *fakeClient) updateTexts() []string {
	out := make([]string, 0, len(c.updates))
	for _, u := range c.updates {
		out = append(out, u.Update.text())
	}
	return out
}

// sessionBag hands the Session handle captured by OnSession to the test's
// Handle closure (a handler reaches its outbound requests through it).
type sessionBag struct {
	mu sync.Mutex
	m  map[string]*Session
}

func newSessionBag() *sessionBag { return &sessionBag{m: map[string]*Session{}} }

func (b *sessionBag) capture(s *Session) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.m[s.ID] = s
}

func (b *sessionBag) get(id string) *Session {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.m[id]
}

// --- initialize / session/new / session/prompt -----------------------------------

func TestServeInitializeSessionPrompt(t *testing.T) {
	handleRuns := 0
	var capturedID string
	bag := newSessionBag()
	cfg := ServeConfig{
		OnSession: func(s *Session) {
			bag.capture(s)
			capturedID = s.ID
		},
		Handle: func(sessionID, userText string, emit func(chunk string)) (string, error) {
			handleRuns++
			_ = bag.get(sessionID)
			emit("Hel")
			emit("lo")
			return "Hello", nil // already on the wire: no extra chunk
		},
	}
	fc := startServe(t, func(in io.Reader, out io.Writer) error { return serveIO(in, out, cfg) })

	// A session method before initialize is refused (Python parity:
	// _require_session_scope).
	_, rpcErr := fc.call("session/new", map[string]any{"cwd": t.TempDir()})
	if rpcErr == nil || rpcErr.Code != codeInvalidRequest {
		t.Fatalf("session/new before initialize = %v, want code %d", rpcErr, codeInvalidRequest)
	}

	res, rpcErr := fc.call("initialize", map[string]any{"protocolVersion": protocolVersion})
	if rpcErr != nil {
		t.Fatalf("initialize: %v", rpcErr)
	}
	var init struct {
		ProtocolVersion   int            `json:"protocolVersion"`
		AgentCapabilities map[string]any `json:"agentCapabilities"`
		AgentInfo         struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"agentInfo"`
	}
	if err := json.Unmarshal(res, &init); err != nil {
		t.Fatalf("decode initialize result: %v", err)
	}
	if init.ProtocolVersion != protocolVersion {
		t.Errorf("protocolVersion = %d, want %d", init.ProtocolVersion, protocolVersion)
	}
	if init.AgentInfo.Name != defaultAgentName || init.AgentInfo.Version != defaultAgentVersion {
		t.Errorf("agentInfo = %+v, want %s/%s", init.AgentInfo, defaultAgentName, defaultAgentVersion)
	}
	promptCaps, _ := init.AgentCapabilities["promptCapabilities"].(map[string]any)
	if promptCaps == nil || promptCaps["image"] != false || promptCaps["audio"] != false {
		t.Errorf("promptCapabilities = %v, want image/audio/embeddedContext all false (text-only)", promptCaps)
	}
	if len(fc.updates) != 0 || len(fc.requests) != 0 {
		t.Errorf("initialize produced %d updates / %d requests, want none",
			len(fc.updates), len(fc.requests))
	}

	res, rpcErr = fc.call("session/new", map[string]any{"cwd": t.TempDir()})
	if rpcErr != nil {
		t.Fatalf("session/new: %v", rpcErr)
	}
	var created struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(res, &created); err != nil || created.SessionID == "" {
		t.Fatalf("session/new result = %s (%v), want a sessionId", res, err)
	}
	if capturedID != created.SessionID {
		t.Errorf("OnSession saw %q, want %q", capturedID, created.SessionID)
	}
	if s := bag.get(created.SessionID); s == nil || s.CWD == "" {
		t.Errorf("session bag = %+v, want the session with a cwd", bag.get(created.SessionID))
	}

	res, rpcErr = fc.call("session/prompt", map[string]any{
		"sessionId": created.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "hi"}},
	})
	if rpcErr != nil {
		t.Fatalf("session/prompt: %v", rpcErr)
	}
	var prompt struct {
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(res, &prompt); err != nil {
		t.Fatalf("decode prompt result: %v", err)
	}
	if prompt.StopReason != "end_turn" {
		t.Errorf("stopReason = %q, want end_turn", prompt.StopReason)
	}
	if texts := fc.updateTexts(); fmt.Sprint(texts) != fmt.Sprint([]string{"Hel", "lo"}) {
		t.Errorf("chunks = %v, want [Hel lo]", texts)
	}
	for _, u := range fc.updates {
		if u.SessionID != created.SessionID {
			t.Errorf("update session = %q, want %q", u.SessionID, created.SessionID)
		}
		if u.Update.SessionUpdate != "agent_message_chunk" {
			t.Errorf("update kind = %q, want agent_message_chunk", u.Update.SessionUpdate)
		}
	}

	// A prompt naming an unknown session is a server error, not a crash
	// (Python parity: resource_not_found "acp-session:<id>").
	_, rpcErr = fc.call("session/prompt", map[string]any{
		"sessionId": "nope",
		"prompt":    []map[string]any{{"type": "text", "text": "x"}},
	})
	if rpcErr == nil || rpcErr.Code != codeSessionNotFound || !strings.Contains(rpcErr.Message, "nope") {
		t.Errorf("unknown session = %v, want code %d naming nope", rpcErr, codeSessionNotFound)
	}

	// A method outside the subset is method-not-found.
	_, rpcErr = fc.call("session/load", map[string]any{})
	if rpcErr == nil || rpcErr.Code != codeMethodNotFound {
		t.Errorf("session/load = %v, want code %d", rpcErr, codeMethodNotFound)
	}

	// A second prompt on the same session runs Handle again.
	if _, rpcErr = fc.call("session/prompt", map[string]any{
		"sessionId": created.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "hi"}},
	}); rpcErr != nil {
		t.Fatalf("second session/prompt: %v", rpcErr)
	}
	if handleRuns != 2 {
		t.Errorf("Handle ran %d times, want 2", handleRuns)
	}
}

// TestServeFinalTextEmittedOnce: a handler that never streams still delivers —
// the final text goes out as one chunk; a handler whose final text equals what
// it streamed is not sent twice (executor parity: _send_final_text).
func TestServeFinalTextEmittedOnce(t *testing.T) {
	cfg := ServeConfig{
		Handle: func(sessionID, userText string, emit func(chunk string)) (string, error) {
			if userText == "stream" {
				emit("par")
				emit("tial")
				return "partial", nil
			}
			return "done", nil // no streaming: Serve must emit it
		},
	}
	fc := startServe(t, func(in io.Reader, out io.Writer) error { return serveIO(in, out, cfg) })
	var rpcErr *rpcError
	if _, rpcErr = fc.call("initialize", map[string]any{"protocolVersion": protocolVersion}); rpcErr != nil {
		t.Fatalf("initialize: %v", rpcErr)
	}
	res, _ := fc.call("session/new", map[string]any{"cwd": t.TempDir()})
	var created struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(res, &created)

	if _, rpcErr = fc.call("session/prompt", map[string]any{
		"sessionId": created.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "quiet"}},
	}); rpcErr != nil {
		t.Fatalf("quiet prompt: %v", rpcErr)
	}
	if texts := fc.updateTexts(); fmt.Sprint(texts) != fmt.Sprint([]string{"done"}) {
		t.Errorf("quiet turn chunks = %v, want [done]", texts)
	}

	if _, rpcErr = fc.call("session/prompt", map[string]any{
		"sessionId": created.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "stream"}},
	}); rpcErr != nil {
		t.Fatalf("stream prompt: %v", rpcErr)
	}
	if texts := fc.updateTexts(); fmt.Sprint(texts) != fmt.Sprint([]string{"par", "tial"}) {
		t.Errorf("streaming turn chunks = %v, want [par tial] exactly once", texts)
	}
}

// TestServeHandleError: a failing handler becomes an internal error carrying
// the reason (Python parity: prompt → internal_error with the cause), and the
// connection keeps serving.
func TestServeHandleError(t *testing.T) {
	cfg := ServeConfig{
		Handle: func(sessionID, userText string, emit func(chunk string)) (string, error) {
			return "", fmt.Errorf("model exploded")
		},
	}
	fc := startServe(t, func(in io.Reader, out io.Writer) error { return serveIO(in, out, cfg) })
	_, _ = fc.call("initialize", map[string]any{"protocolVersion": protocolVersion})
	res, _ := fc.call("session/new", map[string]any{"cwd": t.TempDir()})
	var created struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(res, &created)

	_, rpcErr := fc.call("session/prompt", map[string]any{
		"sessionId": created.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "go"}},
	})
	if rpcErr == nil || rpcErr.Code != codeInternalError || !strings.Contains(rpcErr.Message, "model exploded") {
		t.Errorf("failing prompt = %v, want internal error naming the cause", rpcErr)
	}
}

// --- permissions -----------------------------------------------------------------

// TestServePermissionPolicyDeny: no Permission policy — the reject decision is
// synthesized locally and the client is never asked (no session/request_permission
// on the wire).
func TestServePermissionPolicyDeny(t *testing.T) {
	bag := newSessionBag()
	var got Decision
	var gotErr error
	cfg := ServeConfig{
		OnSession: bag.capture,
		Handle: func(sessionID, userText string, emit func(chunk string)) (string, error) {
			s := bag.get(sessionID)
			got, gotErr = s.RequestPermission(context.Background(), "write_file", "README.md")
			return "ok", nil
		},
	}
	fc := startServe(t, func(in io.Reader, out io.Writer) error { return serveIO(in, out, cfg) })
	_, _ = fc.call("initialize", map[string]any{"protocolVersion": protocolVersion})
	res, _ := fc.call("session/new", map[string]any{"cwd": t.TempDir()})
	var created struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(res, &created)

	if _, rpcErr := fc.call("session/prompt", map[string]any{
		"sessionId": created.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "go"}},
	}); rpcErr != nil {
		t.Fatalf("prompt: %v", rpcErr)
	}
	if gotErr != nil {
		t.Fatalf("RequestPermission: %v", gotErr)
	}
	if got != (Decision{Outcome: OutcomeSelected, OptionID: "reject"}) {
		t.Errorf("decision = %+v, want the synthesized reject", got)
	}
	for _, r := range fc.requests {
		if r.Method == "session/request_permission" {
			t.Errorf("deny must not reach the wire; client saw %s", r.Method)
		}
	}
}

// TestServePermissionClientDecides: with an allowing policy the request goes
// out with the allow/reject options and the client's chosen option decides —
// allow on the first prompt, reject on the second.
func TestServePermissionClientDecides(t *testing.T) {
	bag := newSessionBag()
	var decisions []Decision
	var prompt int
	cfg := ServeConfig{
		OnSession: bag.capture,
		Permission: func(toolCall, input string) bool {
			return toolCall == "write_file" && input == "README.md"
		},
		Handle: func(sessionID, userText string, emit func(chunk string)) (string, error) {
			prompt++
			d, err := bag.get(sessionID).RequestPermission(context.Background(), "write_file", "README.md")
			if err != nil {
				return "", err
			}
			decisions = append(decisions, d)
			return "ok", nil
		},
	}
	fc := startServe(t, func(in io.Reader, out io.Writer) error { return serveIO(in, out, cfg) })
	_, _ = fc.call("initialize", map[string]any{"protocolVersion": protocolVersion})
	res, _ := fc.call("session/new", map[string]any{"cwd": t.TempDir()})
	var created struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(res, &created)

	var seen int
	fc.requestHook = func(method string, params json.RawMessage) any {
		if method != "session/request_permission" {
			return map[string]any{}
		}
		seen++
		var p requestPermissionParams
		if err := json.Unmarshal(params, &p); err != nil {
			t.Errorf("decode permission params: %v", err)
			return Decision{Outcome: OutcomeCancelled}
		}
		if p.SessionID != created.SessionID {
			t.Errorf("request session = %q, want %q", p.SessionID, created.SessionID)
		}
		if p.ToolCall.Title != "write_file" || p.ToolCall.inputString() != "README.md" {
			t.Errorf("toolCall = %+v, want write_file/README.md", p.ToolCall)
		}
		if len(p.Options) != 2 ||
			p.Options[0].OptionID != "allow" || p.Options[0].Kind != kindAllowOnce ||
			p.Options[1].OptionID != "reject" || p.Options[1].Kind != kindRejectOnce {
			t.Errorf("options = %+v, want allow(allow_once)+reject(reject_once)", p.Options)
		}
		if seen == 1 {
			return Decision{Outcome: OutcomeSelected, OptionID: "allow"}
		}
		return Decision{Outcome: OutcomeSelected, OptionID: "reject"}
	}

	for range 2 {
		if _, rpcErr := fc.call("session/prompt", map[string]any{
			"sessionId": created.SessionID,
			"prompt":    []map[string]any{{"type": "text", "text": "go"}},
		}); rpcErr != nil {
			t.Fatalf("prompt: %v", rpcErr)
		}
	}
	if seen != 2 {
		t.Fatalf("client saw %d permission requests, want 2", seen)
	}
	if !decisions[0].Allowed() || decisions[0].OptionID != "allow" {
		t.Errorf("first decision = %+v, want selected allow", decisions[0])
	}
	if decisions[1].Allowed() {
		t.Errorf("second decision = %+v, want denied", decisions[1])
	}
}

// TestServePermissionCancelled: a client that answers cancelled denies.
func TestServePermissionCancelled(t *testing.T) {
	bag := newSessionBag()
	var got Decision
	cfg := ServeConfig{
		OnSession: bag.capture,
		Permission: func(toolCall, input string) bool {
			return true
		},
		Handle: func(sessionID, userText string, emit func(chunk string)) (string, error) {
			d, err := bag.get(sessionID).RequestPermission(context.Background(), "write_file", "README.md")
			if err != nil {
				return "", err
			}
			got = d
			return "ok", nil
		},
	}
	fc := startServe(t, func(in io.Reader, out io.Writer) error { return serveIO(in, out, cfg) })
	_, _ = fc.call("initialize", map[string]any{"protocolVersion": protocolVersion})
	res, _ := fc.call("session/new", map[string]any{"cwd": t.TempDir()})
	var created struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(res, &created)
	fc.requestHook = func(method string, params json.RawMessage) any {
		return Decision{Outcome: OutcomeCancelled}
	}
	if _, rpcErr := fc.call("session/prompt", map[string]any{
		"sessionId": created.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "go"}},
	}); rpcErr != nil {
		t.Fatalf("prompt: %v", rpcErr)
	}
	if got.Allowed() || got.Outcome != OutcomeCancelled {
		t.Errorf("decision = %+v, want cancelled", got)
	}
}

// --- elicitation -----------------------------------------------------------------

func TestServeElicit(t *testing.T) {
	bag := newSessionBag()
	type result struct {
		answer string
		ok     bool
	}
	var results []result
	var gotErr error

	// Prompt 1: policy refuses — declined locally, nothing on the wire.
	// Prompt 2: policy takes it, client accepts with content.
	// Prompt 3: policy takes it, client declines.
	prompt := 0
	var asked string
	cfg := ServeConfig{
		OnSession: bag.capture,
		Elicit: func(message string) bool {
			prompt++
			asked = message
			return prompt > 1
		},
		Handle: func(sessionID, userText string, emit func(chunk string)) (string, error) {
			answer, ok, err := bag.get(sessionID).Elicit(context.Background(), "Deploy to prod?")
			if err != nil {
				return "", err
			}
			results = append(results, result{answer, ok})
			return "p" + fmt.Sprint(prompt), nil
		},
	}
	fc := startServe(t, func(in io.Reader, out io.Writer) error { return serveIO(in, out, cfg) })
	_, _ = fc.call("initialize", map[string]any{"protocolVersion": protocolVersion})
	res, _ := fc.call("session/new", map[string]any{"cwd": t.TempDir()})
	var created struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(res, &created)

	var elicitCalls int
	fc.requestHook = func(method string, params json.RawMessage) any {
		if method != "elicitation/create" {
			return map[string]any{}
		}
		elicitCalls++
		var p elicitationParams
		if err := json.Unmarshal(params, &p); err != nil {
			t.Errorf("decode elicitation params: %v", err)
			return elicitationResult{Action: ElicitDecline}
		}
		if p.Message != "Deploy to prod?" {
			t.Errorf("elicitation message = %q, want %q", p.Message, "Deploy to prod?")
		}
		if elicitCalls == 1 {
			return elicitationResult{Action: ElicitAccept, Content: "42"}
		}
		return elicitationResult{Action: ElicitDecline}
	}

	for range 3 {
		if _, rpcErr := fc.call("session/prompt", map[string]any{
			"sessionId": created.SessionID,
			"prompt":    []map[string]any{{"type": "text", "text": "go"}},
		}); rpcErr != nil {
			t.Fatalf("prompt %d: %v", prompt, rpcErr)
		}
	}
	if gotErr != nil {
		t.Fatalf("Elicit (policy refuses): %v", gotErr)
	}
	if results[0] != (result{"", false}) {
		t.Errorf("refused Elicit = %+v, want declined without wire traffic", results[0])
	}
	if elicitCalls != 2 {
		t.Errorf("client saw %d elicitation requests, want 2", elicitCalls)
	}
	if asked != "Deploy to prod?" {
		t.Errorf("policy was asked %q, want %q", asked, "Deploy to prod?")
	}
	if results[1] != (result{"42", true}) {
		t.Errorf("accepted Elicit = %+v, want (42, true)", results[1])
	}
	if results[2] != (result{"", false}) {
		t.Errorf("declined Elicit = %+v, want declined", results[2])
	}
}

// --- ServeAgent ------------------------------------------------------------------

// streamingDouble publishes two ModelMessageChunk deltas before its response,
// exercising the ServeAgent chunk path (a real provider client streams the
// same way — conv.Send of ModelMessageChunk).
type streamingDouble struct{ calls int }

func (d *streamingDouble) Provider() config.Provider { return "test" }
func (d *streamingDouble) Model() string             { return "double-model" }
func (d *streamingDouble) Create() (config.LLMClient, error) {
	return d, nil
}
func (d *streamingDouble) Clone(map[string]any) (config.ModelConfig, error) { return d, nil }
func (d *streamingDouble) Call(ctx context.Context, messages []events.Event, conv *agocontext.Context, opts config.CallOptions) (*events.ModelResponse, error) {
	d.calls++
	for _, part := range []string{"He", "y"} {
		if err := conv.Send(&events.ModelMessageChunk{Content: part}); err != nil {
			return nil, err
		}
	}
	return &events.ModelResponse{Message: &events.ModelMessage{Content: "Hey"}}, nil
}

// TestServeAgentStreamsAndContinues: chunks stream out as they are produced,
// the final text is not re-sent, and a second prompt on the session continues
// the conversation (fresh sessions would not share the double's call count
// through the replies map — here both prompts hit the same stream).
func TestServeAgentStreamsAndContinues(t *testing.T) {
	double := &streamingDouble{}
	a := agent.New("served", "you echo.", agent.WithConfig(double))
	bag := newSessionBag()
	fc := startServe(t, func(in io.Reader, out io.Writer) error {
		return serveAgentIO(in, out, a, ServeConfig{OnSession: bag.capture})
	})

	if _, rpcErr := fc.call("initialize", map[string]any{"protocolVersion": protocolVersion}); rpcErr != nil {
		t.Fatalf("initialize: %v", rpcErr)
	}
	res, rpcErr := fc.call("session/new", map[string]any{"cwd": t.TempDir()})
	if rpcErr != nil {
		t.Fatalf("session/new: %v", rpcErr)
	}
	var created struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(res, &created)

	for range 2 {
		if _, rpcErr = fc.call("session/prompt", map[string]any{
			"sessionId": created.SessionID,
			"prompt":    []map[string]any{{"type": "text", "text": "hello"}},
		}); rpcErr != nil {
			t.Fatalf("session/prompt: %v", rpcErr)
		}
		if texts := fc.updateTexts(); fmt.Sprint(texts) != fmt.Sprint([]string{"He", "y"}) {
			t.Errorf("chunks = %v, want [He y] with no duplicate final text", texts)
		}
	}
	if double.calls != 2 {
		t.Errorf("the agent saw %d model calls, want 2 (the session continued)", double.calls)
	}
}

// TestServeAgentNonStreaming: a provider that emits no chunks gets its final
// body delivered as one agent_message_chunk.
func TestServeAgentNonStreaming(t *testing.T) {
	scripted := agotest.NewTestConfig(&events.ModelResponse{
		Message: &events.ModelMessage{Content: "done"},
	})
	a := agent.New("served", "", agent.WithConfig(scripted))
	fc := startServe(t, func(in io.Reader, out io.Writer) error {
		return serveAgentIO(in, out, a, ServeConfig{})
	})
	_, _ = fc.call("initialize", map[string]any{"protocolVersion": protocolVersion})
	res, rpcErr := fc.call("session/new", map[string]any{"cwd": t.TempDir()})
	if rpcErr != nil {
		t.Fatalf("session/new: %v", rpcErr)
	}
	var created struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(res, &created)
	if _, rpcErr = fc.call("session/prompt", map[string]any{
		"sessionId": created.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "go"}},
	}); rpcErr != nil {
		t.Fatalf("session/prompt: %v", rpcErr)
	}
	if texts := fc.updateTexts(); fmt.Sprint(texts) != fmt.Sprint([]string{"done"}) {
		t.Errorf("chunks = %v, want [done]", texts)
	}
}
