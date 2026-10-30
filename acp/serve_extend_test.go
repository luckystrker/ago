package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// notify writes one JSON-RPC notification to the agent (the session/cancel
// path — a notification has no reply to await).
func (c *fakeClient) notify(method string, params any) {
	c.t.Helper()
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		c.t.Fatalf("marshal notification: %v", err)
	}
	c.writeMu.Lock()
	fmt.Fprintln(c.w, string(payload))
	c.w.Flush()
	c.writeMu.Unlock()
}

// updateKinds lists the kinds of the collected updates, for compact assertions.
func (c *fakeClient) updateKinds() []string {
	out := make([]string, 0, len(c.updates))
	for _, u := range c.updates {
		out = append(out, u.Update.Kind)
	}
	return out
}

// --- session/cancel -----------------------------------------------------------------

// TestServeCancelStopsTheTurn: a session/cancel notification mid-turn
// cancels the turn's context, and the prompt is answered with the cancelled
// stop reason rather than an error (Python parity: agent.py cancel + the
// prompt handler's CancelledError branch).
func TestServeCancelStopsTheTurn(t *testing.T) {
	bag := newSessionBag()
	started := make(chan struct{})
	var once sync.Once
	var turns atomic.Int32
	var observedCtx context.Context
	cfg := ServeConfig{
		OnSession: bag.capture,
		Handle: func(ctx context.Context, sessionID, userText string, emit *Emitter) (string, error) {
			if turns.Add(1) == 1 {
				// The first turn blocks until cancelled, the way a long turn
				// would; later turns answer normally.
				once.Do(func() {
					observedCtx = ctx
					close(started)
				})
				<-ctx.Done()
				return "", ctx.Err()
			}
			return "recovered", nil
		},
	}
	fc := startServe(t, func(in io.Reader, out io.Writer) error { return serveIO(in, out, cfg) })
	_, _ = fc.call("initialize", map[string]any{"protocolVersion": protocolVersion})
	res, _ := fc.call("session/new", map[string]any{"cwd": t.TempDir()})
	var created struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(res, &created)

	go func() {
		<-started // the turn's context is registered by now
		fc.notify("session/cancel", map[string]any{"sessionId": created.SessionID})
	}()
	res, rpcErr := fc.call("session/prompt", map[string]any{
		"sessionId": created.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "long work"}},
	})
	if rpcErr != nil {
		t.Fatalf("cancelled prompt = %v, want a normal response", rpcErr)
	}
	var prompt struct {
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(res, &prompt); err != nil {
		t.Fatalf("decode prompt result: %v", err)
	}
	if prompt.StopReason != StopCancelled {
		t.Errorf("stopReason = %q, want %q", prompt.StopReason, StopCancelled)
	}
	if observedCtx.Err() != context.Canceled {
		t.Errorf("handler context = %v, want context.Canceled", observedCtx.Err())
	}
	// The post-cancel prompt must not inherit the cancellation.
	if _, rpcErr = fc.call("session/prompt", map[string]any{
		"sessionId": created.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "next"}},
	}); rpcErr != nil {
		t.Errorf("prompt after cancel: %v", rpcErr)
	}

}

// TestServeCancelUnknownSessionIgnored: a cancel naming a session that does
// not exist — or no turn running — is a no-op, never an error (Python parity:
// agent.py cancel logs and ignores).
func TestServeCancelUnknownSessionIgnored(t *testing.T) {
	var done int
	cfg := ServeConfig{
		Handle: func(ctx context.Context, sessionID, userText string, emit *Emitter) (string, error) {
			done++
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

	fc.notify("session/cancel", map[string]any{"sessionId": "does-not-exist"})
	fc.notify("session/cancel", nil) // malformed params, still not a crash
	res, rpcErr := fc.call("session/prompt", map[string]any{
		"sessionId": created.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "go"}},
	})
	if rpcErr != nil {
		t.Fatalf("prompt after stray cancels: %v", rpcErr)
	}
	var prompt struct {
		StopReason string `json:"stopReason"`
	}
	_ = json.Unmarshal(res, &prompt)
	if prompt.StopReason != StopEndTurn {
		t.Errorf("stopReason = %q, want %q (the stray cancels must not leak)", prompt.StopReason, StopEndTurn)
	}
}

// --- session/load --------------------------------------------------------------------

// TestServeAgentLoadReplaysHistory: session/load reattaches to the session
// this connection issued and replays its stored history as the updates of a
// live turn — user turns as user_message_chunk, replies as
// agent_message_chunk — before the response (Python parity: agent.py
// load_session + mappers.history_to_session_updates).
func TestServeAgentLoadReplaysHistory(t *testing.T) {
	double := &streamingDouble{}
	a := agent.New("served", "you echo.", agent.WithConfig(double))
	bag := newSessionBag()
	fc := startServe(t, func(in io.Reader, out io.Writer) error {
		return serveAgentIO(in, out, a, ServeConfig{OnSession: bag.capture})
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
		"prompt":    []map[string]any{{"type": "text", "text": "hello"}},
	}); rpcErr != nil {
		t.Fatalf("session/prompt: %v", rpcErr)
	}

	res, rpcErr = fc.call("session/load", map[string]any{"sessionId": created.SessionID, "cwd": "/elsewhere"})
	if rpcErr != nil {
		t.Fatalf("session/load: %v", rpcErr)
	}
	if len(res) > 2 && string(res) != "{}" && string(res) != "null" {
		t.Logf("session/load result = %s", res) // an empty object: nothing to negotiate
	}
	// The replay: the user turn, then the reply — nothing after the response.
	var kinds []string
	var texts []string
	for _, u := range fc.updates {
		kinds = append(kinds, u.Update.Kind)
		texts = append(texts, u.Update.text())
		if u.SessionID != created.SessionID {
			t.Errorf("replay update session = %q, want %q", u.SessionID, created.SessionID)
		}
	}
	if fmt.Sprint(kinds) != fmt.Sprint([]string{"user_message_chunk", "agent_message_chunk"}) {
		t.Fatalf("replay kinds = %v, want [user_message_chunk agent_message_chunk]", kinds)
	}
	if fmt.Sprint(texts) != fmt.Sprint([]string{"hello", "Hey"}) {
		t.Errorf("replay texts = %v, want [hello Hey]", texts)
	}
	for _, u := range fc.updates {
		if u.Update.MessageID == "" {
			t.Errorf("%s chunk carries no messageId; a client cannot group chunks", u.Update.Kind)
		}
	}

	// The replayed session's cwd was refreshed by the load.
	if s := bag.get(created.SessionID); s != nil && s.CWD != "/elsewhere" {
		t.Errorf("session cwd after load = %q, want /elsewhere", s.CWD)
	}

	// Loading a session this connection never issued is resource_not_found
	// (Python parity: an id with no history behind it).
	if _, rpcErr = fc.call("session/load", map[string]any{"sessionId": "00000000-0000-0000-0000-000000000000", "cwd": "."}); rpcErr == nil || rpcErr.Code != codeResourceNotFound {
		t.Errorf("unknown session/load = %v, want code %d", rpcErr, codeResourceNotFound)
	}
}

// --- session/update vocabulary --------------------------------------------------------

// vocabDouble is a provider double scripting one tool round: reasoning and a
// tool request on the first call, token telemetry and the final answer on the
// second. It exercises the full update vocabulary the serve role maps.
type vocabDouble struct {
	calls int
}

func (d *vocabDouble) Provider() config.Provider { return "test" }
func (d *vocabDouble) Model() string             { return "vocab-model" }
func (d *vocabDouble) Create() (config.LLMClient, error) {
	return d, nil
}
func (d *vocabDouble) Clone(map[string]any) (config.ModelConfig, error) { return d, nil }
func (d *vocabDouble) Call(ctx context.Context, messages []events.Event, conv *agocontext.Context, opts config.CallOptions) (*events.ModelResponse, error) {
	d.calls++
	if d.calls == 1 {
		if err := conv.Send(&events.ModelReasoning{Content: "pondering"}); err != nil {
			return nil, err
		}
		return &events.ModelResponse{
			ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{{
				ID: "call-1", Name: "search", Arguments: `{"q":"ago"}`,
			}}},
		}, nil
	}
	if err := conv.Send(&events.UsageEvent{Usage: events.Usage{
		PromptTokens: events.I64(11), CompletionTokens: events.I64(7),
	}}); err != nil {
		return nil, err
	}
	return &events.ModelResponse{Message: &events.ModelMessage{Content: "done"}}, nil
}

// assertToolCallUpdate checks one collected tool_call/tool_call_update.
func assertUpdate(t *testing.T, u updateParams, want Update) {
	t.Helper()
	// Round-trip through the wire shape: what the serve role emits must
	// decode back to what a client would see.
	data, err := json.Marshal(u.Update)
	if err != nil {
		t.Fatalf("marshal %s update: %v", want.Kind, err)
	}
	var got Update
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode %s update %s: %v", want.Kind, data, err)
	}
	if got.Kind != want.Kind {
		t.Errorf("kind = %q, want %q (wire %s)", got.Kind, want.Kind, data)
	}
	if want.Text != "" && got.Text != want.Text {
		t.Errorf("text = %q, want %q (wire %s)", got.Text, want.Text, data)
	}
	if want.ToolCallID != "" && got.ToolCallID != want.ToolCallID {
		t.Errorf("toolCallId = %q, want %q (wire %s)", got.ToolCallID, want.ToolCallID, data)
	}
	if want.Title != "" && got.Title != want.Title {
		t.Errorf("title = %q, want %q (wire %s)", got.Title, want.Title, data)
	}
	if want.ToolKind != "" && got.ToolKind != want.ToolKind {
		t.Errorf("kind field = %q, want %q (wire %s)", got.ToolKind, want.ToolKind, data)
	}
	if want.Status != "" && got.Status != want.Status {
		t.Errorf("status = %q, want %q (wire %s)", got.Status, want.Status, data)
	}
	if len(want.RawInput) > 0 && string(got.RawInput) != string(want.RawInput) {
		t.Errorf("rawInput = %s, want %s (wire %s)", got.RawInput, want.RawInput, data)
	}
	if want.ContentText != "" && got.ContentText != want.ContentText {
		t.Errorf("content text = %q, want %q (wire %s)", got.ContentText, want.ContentText, data)
	}
	if want.Used != 0 && got.Used != want.Used {
		t.Errorf("used = %d, want %d (wire %s)", got.Used, want.Used, data)
	}
	if want.Size != 0 && got.Size != want.Size {
		t.Errorf("size = %d, want %d (wire %s)", got.Size, want.Size, data)
	}
}

// runVocabTurn serves the vocabDouble for one prompt and returns the updates.
func runVocabTurn(t *testing.T, cfg ServeConfig) *fakeClient {
	t.Helper()
	a := agent.New("vocab", "", agent.WithConfig(&vocabDouble{}))
	fc := startServe(t, func(in io.Reader, out io.Writer) error {
		return serveAgentIO(in, out, a, cfg)
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
	return fc
}

// TestServeUpdateVocabulary: the serve role maps the AGo turn's events onto
// the full session/update vocabulary — reasoning as agent_thought_chunk (only
// when StreamThoughts is on), the model's tool calls as tool_call, their
// results as tool_call_update with the result text, token telemetry as
// usage_update when ContextWindow is set.
func TestServeUpdateVocabulary(t *testing.T) {
	fc := runVocabTurn(t, ServeConfig{
		StreamThoughts: true,
		ContextWindow:  100000,
	})
	kinds := fc.updateKinds()
	wantKinds := []string{
		"agent_thought_chunk",
		"tool_call",
		"tool_call_update",
		"usage_update",
		"agent_message_chunk", // the final text, delivered by the turn
	}
	if fmt.Sprint(kinds) != fmt.Sprint(wantKinds) {
		t.Fatalf("update kinds = %v, want %v", kinds, wantKinds)
	}
	assertUpdate(t, fc.updates[0], Update{Kind: "agent_thought_chunk", Text: "pondering"})
	assertUpdate(t, fc.updates[1], Update{Kind: "tool_call", ToolCallID: "call-1",
		Title: "search", ToolKind: KindOther, Status: StatusInProgress, RawInput: json.RawMessage(`{"q":"ago"}`)})
	// "search" has no registered tool here: the not-found outcome settles the
	// call as failed, with the reason as its content — an outcome, never a
	// protocol error.
	assertUpdate(t, fc.updates[2], Update{Kind: "tool_call_update", ToolCallID: "call-1",
		Title: "search", Status: StatusFailed})
	if fc.updates[2].Update.ContentText == "" {
		t.Errorf("failed tool_call_update carries no content text; the client learns nothing")
	}
	// usage_update: the reported tokens against the configured window.
	assertUpdate(t, fc.updates[3], Update{Kind: "usage_update", Used: 18, Size: 100000})
}

// TestServeUpdateVocabularyGated: thoughts stay off by default, and usage
// updates are not emitted without a context window to report against —
// neither required field would be filled truthfully (Python parity:
// stream_thoughts defaults off; the executor emits no usage updates).
func TestServeUpdateVocabularyGated(t *testing.T) {
	fc := runVocabTurn(t, ServeConfig{})
	for _, k := range []string{"agent_thought_chunk", "usage_update"} {
		for _, u := range fc.updates {
			if u.Update.Kind == k {
				t.Errorf("saw %s with the feature gate off; want it suppressed", k)
			}
		}
	}
	kinds := fc.updateKinds()
	if fmt.Sprint(kinds) != fmt.Sprint([]string{"tool_call", "tool_call_update", "agent_message_chunk"}) {
		t.Fatalf("update kinds = %v, want [tool_call tool_call_update agent_message_chunk]", kinds)
	}
}

// --- auth ------------------------------------------------------------------------

// TestServeStaticTokenAuth: with a provider the initialize handshake
// advertises its methods, every session method is gated on authenticate, and
// a rejected credential is refused. Without a provider authenticate is
// refused and the session methods are open (Python parity: auth.py
// StaticTokenAuth + agent.py _require_session_scope).
func TestServeStaticTokenAuth(t *testing.T) {
	auth, err := NewStaticTokenAuth("s3cret")
	if err != nil {
		t.Fatalf("NewStaticTokenAuth: %v", err)
	}
	if _, err = NewStaticTokenAuth(""); err == nil {
		t.Error("NewStaticTokenAuth(\"\") must refuse an empty token")
	}
	cfg := ServeConfig{
		Auth: auth,
		Handle: func(ctx context.Context, sessionID, userText string, emit *Emitter) (string, error) {
			return "ok", nil
		},
	}
	fc := startServe(t, func(in io.Reader, out io.Writer) error { return serveIO(in, out, cfg) })

	res, rpcErr := fc.call("initialize", map[string]any{"protocolVersion": protocolVersion})
	if rpcErr != nil {
		t.Fatalf("initialize: %v", rpcErr)
	}
	var init struct {
		AuthMethods []AuthMethod `json:"authMethods"`
	}
	if err := json.Unmarshal(res, &init); err != nil {
		t.Fatalf("decode initialize: %v", err)
	}
	if len(init.AuthMethods) != 1 || init.AuthMethods[0].ID != "token" || init.AuthMethods[0].Name != "Shared token" {
		t.Errorf("authMethods = %+v, want the shared-token method", init.AuthMethods)
	}

	// Session methods are gated until authenticate succeeds.
	_, rpcErr = fc.call("session/new", map[string]any{"cwd": t.TempDir()})
	if rpcErr == nil || rpcErr.Code != codeAuthRequired {
		t.Errorf("session/new before authenticate = %v, want code %d", rpcErr, codeAuthRequired)
	}

	// A wrong method id and a wrong token are both refused; the refusal is
	// the ACP authentication-required error.
	_, rpcErr = fc.call("authenticate", map[string]any{"methodId": "oauth"})
	if rpcErr == nil || rpcErr.Code != codeAuthRequired {
		t.Errorf("authenticate(unknown method) = %v, want code %d", rpcErr, codeAuthRequired)
	}
	_, rpcErr = fc.call("authenticate", map[string]any{
		"methodId": "token", "_meta": map[string]any{"token": "wrong"},
	})
	if rpcErr == nil || rpcErr.Code != codeAuthRequired {
		t.Errorf("authenticate(wrong token) = %v, want code %d", rpcErr, codeAuthRequired)
	}
	// The token nested under a "meta" entry is found too (Python parity:
	// kwargs.get("meta", {}).get("token")).
	_, rpcErr = fc.call("authenticate", map[string]any{
		"methodId": "token", "_meta": map[string]any{"meta": map[string]any{"token": "s3cret"}},
	})
	if rpcErr != nil {
		t.Fatalf("authenticate(nested token): %v", rpcErr)
	}

	res, rpcErr = fc.call("session/new", map[string]any{"cwd": t.TempDir()})
	if rpcErr != nil {
		t.Fatalf("session/new after authenticate: %v", rpcErr)
	}

	// Without a provider: authenticate is refused, sessions are open.
	fc2 := startServe(t, func(in io.Reader, out io.Writer) error {
		return serveIO(in, out, ServeConfig{Handle: cfg.Handle})
	})
	_, _ = fc2.call("initialize", map[string]any{"protocolVersion": protocolVersion})
	if _, rpcErr = fc2.call("authenticate", map[string]any{"methodId": "token"}); rpcErr == nil || rpcErr.Code != codeInvalidRequest {
		t.Errorf("authenticate without a provider = %v, want code %d", rpcErr, codeInvalidRequest)
	}
	if _, rpcErr = fc2.call("session/new", map[string]any{"cwd": t.TempDir()}); rpcErr != nil {
		t.Errorf("session/new without a provider: %v", rpcErr)
	}
	var res2 struct {
		AuthMethods []AuthMethod `json:"authMethods"`
	}
	res2Raw, _ := fc2.call("initialize", map[string]any{"protocolVersion": protocolVersion})
	_ = json.Unmarshal(res2Raw, &res2)
	if len(res2.AuthMethods) != 0 {
		t.Errorf("authMethods without a provider = %+v, want none", res2.AuthMethods)
	}
}

// --- tool gateway -------------------------------------------------------------------

// fakeMCP is the minimal streamable-HTTP MCP server the gateway test dials:
// initialize, then tools/call answering one text block (the mcp package's
// own tests carry the full shape; this one is the minimum the client needs).
type fakeMCP struct {
	mu    sync.Mutex
	calls int
}

func (s *fakeMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     *int64          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	write := func(result map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}
	switch req.Method {
	case "initialize":
		write(map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"serverInfo":      map[string]any{"name": "fake-mcp", "version": "1.0"},
		})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/call":
		s.mu.Lock()
		s.calls++
		s.mu.Unlock()
		write(map[string]any{
			"content": []map[string]any{{"type": "text", "text": "mcp-hello"}},
			"isError": false,
		})
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
			"error": map[string]any{"code": -32601, "message": "method not found"}})
	}
}

// TestServeToolGateway: the MCP servers a client declares in session/new are
// recorded on the session and proxied through the ago/mcp client when the
// handler asks — nothing is dialed before the first call (Python deviation:
// agent.py records without acting; docs/DECISIONS.md D-056).
func TestServeToolGateway(t *testing.T) {
	hs := httptest.NewServer(&fakeMCP{})
	t.Cleanup(hs.Close)

	bag := newSessionBag()
	var gatewayText string
	var gatewayErr error
	cfg := ServeConfig{
		OnSession: bag.capture,
		Handle: func(ctx context.Context, sessionID, userText string, emit *Emitter) (string, error) {
			s := bag.get(sessionID)
			gw := s.Gateway()
			if gw == nil {
				return "", fmt.Errorf("no gateway for %d declared servers", len(s.MCPServers))
			}
			gatewayText, gatewayErr = gw.Call("demo", "echo", map[string]any{"msg": "hi"})
			return "done", nil
		},
	}
	fc := startServe(t, func(in io.Reader, out io.Writer) error { return serveIO(in, out, cfg) })
	_, _ = fc.call("initialize", map[string]any{"protocolVersion": protocolVersion})
	res, rpcErr := fc.call("session/new", map[string]any{
		"cwd": t.TempDir(),
		"mcpServers": []map[string]any{
			{"name": "demo", "url": hs.URL},
			{"name": "local", "command": "whatever", "args": []string{"serve"}},
		},
	})
	if rpcErr != nil {
		t.Fatalf("session/new with mcpServers: %v", rpcErr)
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
	if gatewayErr != nil {
		t.Fatalf("gateway call: %v", gatewayErr)
	}
	if gatewayText != "mcp-hello" {
		t.Errorf("gateway tool result = %q, want %q", gatewayText, "mcp-hello")
	}

	// An unknown server is a clean error, not a dial.
	s := bag.get(created.SessionID)
	if _, err := s.Gateway().Call("nope", "echo", nil); err == nil {
		t.Error("gateway call naming an undeclared server must fail")
	}
	// Declared servers are recorded even though nothing was dialed eagerly.
	if len(s.MCPServers) != 2 || s.MCPServers[0].Name != "demo" {
		t.Errorf("declared servers = %+v, want demo+local recorded", s.MCPServers)
	}

	// A session without declarations has no gateway.
	res, _ = fc.call("session/new", map[string]any{"cwd": t.TempDir()})
	var plain struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(res, &plain)
	if bag.get(plain.SessionID).Gateway() != nil {
		t.Error("a session with no declared servers must have no gateway")
	}
}

// --- the fs guard (unit) -------------------------------------------------------------

// TestConfine: the guard refuses anything that resolves outside the root —
// traversal, absolute siblings — and passes paths inside it, including ones
// that do not exist yet (write targets) (Python parity: bridge._confine).
func TestConfine(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root+"/inner", 0o755); err != nil {
		t.Fatal(err)
	}
	inside, err := confine(root, "inner/file.txt")
	if err != nil {
		t.Fatalf("confine(inner/file.txt): %v", err)
	}
	if !strings.HasPrefix(inside, root) || !strings.HasSuffix(inside, "file.txt") {
		t.Errorf("confine(inner/file.txt) = %q, want inside the root", inside)
	}
	if got, err := confine(root, root+`/inner/other.txt`); err != nil || got == "" {
		t.Errorf("confine(absolute inside) = %q, %v", got, err)
	}
	for _, escape := range []string{"../outside.txt", `/../outside.txt`, `inner/../../outside.txt`} {
		if got, err := confine(root, escape); err == nil {
			t.Errorf("confine(%q) = %q, want refused", escape, got)
		}
	}
	// A sibling absolute path escapes even though it is well-formed.
	if _, err := confine(root, root+"-sibling"+string(os.PathSeparator)+"x.txt"); err == nil {
		t.Error("confine(root-sibling path) must be refused")
	}
}
