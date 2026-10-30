package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/luckystrker/ago/agent"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/mcp"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
)

// --- shared test fixtures -------------------------------------------------------

// recordingAsk is the fake Ask bridge: it records every userText it saw and
// answers "reply: <userText>", so both the text mapping and the context fold
// are observable from the reply.
type recordingAsk struct {
	mu  sync.Mutex
	got []string
}

func (r *recordingAsk) ask(_ context.Context, userText string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, userText)
	return "reply: " + userText, nil
}

func (r *recordingAsk) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}

// testTools returns the deterministic pair every round trip uses: echo (a
// plain function tool answering through the stream machinery) and fail (a
// function tool whose error must surface as a tool-level error result). The
// pair lives in mustTestTools (stdio_test.go) so the child process can build
// it without a *testing.T.
func testTools(t *testing.T) []tools.Tool {
	t.Helper()
	return mustTestTools()
}

// newTestServer builds the round-trip server: fake ask plus echo and fail.
func newTestServer(t *testing.T, ask func(ctx context.Context, userText string) (string, error)) (*Server, *recordingAsk) {
	t.Helper()
	rec := &recordingAsk{}
	if ask == nil {
		ask = rec.ask
	}
	s, err := NewServer(ServerConfig{
		Name:         "test-server",
		Version:      "1.2.3",
		Instructions: "Talk to it.",
		Ask:          ask,
		Tools:        testTools(t),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s, rec
}

// httpClient connects the real ago/mcp client to the server over httptest —
// the same two roles a production deployment runs.
func httpClient(t *testing.T, s *Server) *mcp.Client {
	t.Helper()
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	cl, err := mcp.New(&mcp.MCPServerConfig{URL: ts.URL})
	if err != nil {
		t.Fatalf("mcp.New: %v", err)
	}
	t.Cleanup(func() { cl.Close() })
	return cl
}

// textPart asserts the part is a TextInput and returns its content.
func textPart(t *testing.T, part events.Input) string {
	t.Helper()
	if ti, ok := events.AsValue(part).(events.TextInput); ok {
		return ti.Content
	}
	t.Fatalf("part %#v is not TextInput", part)
	return ""
}

// rawPost sends one raw JSON-RPC message to the handler and decodes the reply.
func rawPost(t *testing.T, s *Server, msg string) rpcResponse {
	t.Helper()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(msg))
	if err != nil {
		t.Fatalf("post %s: %v", msg, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var out rpcResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return out
}

// --- round trips ----------------------------------------------------------------

func TestHTTPRoundTrip(t *testing.T) {
	s, rec := newTestServer(t, nil)
	cl := httpClient(t, s)

	info, err := cl.Initialize()
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if info.Name != "test-server" || info.Version != "1.2.3" {
		t.Errorf("server info = %+v", info)
	}
	if info.ProtocolVersion != protocolVersion {
		t.Errorf("protocolVersion = %q, want %q", info.ProtocolVersion, protocolVersion)
	}

	got, err := cl.ListTools()
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	for _, tl := range got {
		names = append(names, tl.Name)
	}
	if fmt.Sprint(names) != fmt.Sprint([]string{"ask", "echo", "fail"}) {
		t.Errorf("tools = %v (ask first, then the config's entries)", names)
	}
	// The conversational tool's schema carries the required message argument.
	if len(got[0].InputSchema["required"].([]any)) != 1 {
		t.Errorf("ask inputSchema = %v", got[0].InputSchema)
	}

	// The ask tool: text argument in, the bridge's reply as text content out.
	out, err := cl.CallTool("ask", map[string]any{"message": "hi"})
	if err != nil {
		t.Fatalf("CallTool ask: %v", err)
	}
	if len(out.Parts) != 1 || textPart(t, out.Parts[0]) != "reply: hi" {
		t.Errorf("ask result = %#v", out.Parts)
	}

	// The context argument is folded in front of the message.
	if _, err := cl.CallTool("ask", map[string]any{"message": "q", "context": "facts"}); err != nil {
		t.Fatalf("CallTool ask+context: %v", err)
	}
	if seen := rec.seen(); len(seen) != 2 || seen[1] != "Context:\nfacts\nq" {
		t.Errorf("ask userText = %v, want the context folded in front", seen)
	}

	// A deterministic tool answers through its own machinery.
	out, err = cl.CallTool("echo", map[string]any{"text": "round"})
	if err != nil {
		t.Fatalf("CallTool echo: %v", err)
	}
	if textPart(t, out.Parts[0]) != "echo: round" {
		t.Errorf("echo = %q", textPart(t, out.Parts[0]))
	}

	// A tool failure is a tool-level error result (isError), not a JSON-RPC
	// error: the client surfaces ErrToolCall carrying the message.
	if _, err := cl.CallTool("fail", nil); !errors.Is(err, mcp.ErrToolCall) || !strings.Contains(err.Error(), "boom") {
		t.Errorf("CallTool fail = %v, want ErrToolCall naming boom", err)
	}

	// An unknown name is a JSON-RPC invalid-params refusal (client parity:
	// ErrProtocol), matching the executor's MCPError arm.
	if _, err := cl.CallTool("nope", nil); !errors.Is(err, mcp.ErrProtocol) || !strings.Contains(err.Error(), "Unknown tool") {
		t.Errorf("CallTool nope = %v, want ErrProtocol naming Unknown tool", err)
	}

	// The empty-message guard.
	if _, err := cl.CallTool("ask", nil); !errors.Is(err, mcp.ErrToolCall) ||
		!strings.Contains(err.Error(), "Missing required 'message' argument.") {
		t.Errorf("CallTool ask/empty = %v, want the message guard", err)
	}

	// Conversations are deferred: a handle argument is refused, not dropped.
	if _, err := cl.CallTool("ask", map[string]any{"message": "x", "conversation": "c1"}); !errors.Is(err, mcp.ErrToolCall) ||
		!strings.Contains(err.Error(), "does not maintain conversations") {
		t.Errorf("CallTool ask/conversation = %v, want the conversations refusal", err)
	}
}

// TestNewServerForAgent drives the real agent bridge end to end: tools/call
// ask → agent turn → scripted reply body.
func TestNewServerForAgent(t *testing.T) {
	cfg := agotest.NewTestConfig(&events.ModelResponse{Message: &events.ModelMessage{Content: "agent says hi"}})
	a := agent.New("served", "You are served.", agent.WithConfig(cfg))
	s, err := NewServerForAgent(a, WithServerVersion("9.9.9"), WithTools(testTools(t)...))
	if err != nil {
		t.Fatalf("NewServerForAgent: %v", err)
	}
	cl := httpClient(t, s)

	info, err := cl.Initialize()
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if info.Name != "served" || info.Version != "9.9.9" {
		t.Errorf("server info = %+v (name defaults to the agent's)", info)
	}
	out, err := cl.CallTool("ask", map[string]any{"message": "hello"})
	if err != nil {
		t.Fatalf("CallTool ask: %v", err)
	}
	if textPart(t, out.Parts[0]) != "agent says hi" {
		t.Errorf("ask = %q, want the scripted agent reply body", textPart(t, out.Parts[0]))
	}
}

// --- handshake details ----------------------------------------------------------

func TestInitializeNegotiation(t *testing.T) {
	s, _ := newTestServer(t, nil)
	// A revision this subset does not speak is answered with its own latest.
	resp := rawPost(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize",
		"params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`)
	if resp.Error != nil {
		t.Fatalf("initialize newer: %v", resp.Error)
	}
	var got struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(mustRaw(t, resp.Result), &got); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if got.ProtocolVersion != protocolVersion {
		t.Errorf("negotiated to %q, want %q", got.ProtocolVersion, protocolVersion)
	}

	// The supported revision is echoed verbatim.
	resp = rawPost(t, s, `{"jsonrpc":"2.0","id":2,"method":"initialize",
		"params":{"protocolVersion":"`+protocolVersion+`","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`)
	if err := json.Unmarshal(mustRaw(t, resp.Result), &got); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if got.ProtocolVersion != protocolVersion {
		t.Errorf("echoed %q, want %q", got.ProtocolVersion, protocolVersion)
	}
}

func TestPingAndEnvelopeErrors(t *testing.T) {
	s, _ := newTestServer(t, nil)

	// ping answers an empty result.
	resp := rawPost(t, s, `{"jsonrpc":"2.0","id":7,"method":"ping"}`)
	if resp.Error != nil || resp.Result == nil {
		t.Errorf("ping = %+v, want an empty result", resp)
	}

	// notifications/initialized sent as a request is confirmed.
	resp = rawPost(t, s, `{"jsonrpc":"2.0","id":8,"method":"notifications/initialized"}`)
	if resp.Error != nil || resp.Result == nil {
		t.Errorf("initialized-as-request = %+v, want a confirmation result", resp)
	}

	// An unknown method is method-not-found.
	resp = rawPost(t, s, `{"jsonrpc":"2.0","id":9,"method":"resources/list"}`)
	if resp.Error == nil || resp.Error.Code != codeMethodNotFound {
		t.Errorf("resources/list = %+v, want code %d", resp, codeMethodNotFound)
	}

	// A batch is refused as invalid request (it parses, so it is not a parse
	// error).
	resp = rawPost(t, s, `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`)
	if resp.Error == nil || resp.Error.Code != codeInvalidRequest {
		t.Errorf("batch = %+v, want code %d", resp, codeInvalidRequest)
	}

	// Garbage is a parse error.
	resp = rawPost(t, s, `not json`)
	if resp.Error == nil || resp.Error.Code != codeParseError {
		t.Errorf("garbage = %+v, want code %d", resp, codeParseError)
	}

	// A notification over HTTP is accepted with 202 and no body.
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	httpResp, err := http.Post(ts.URL, "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if err != nil {
		t.Fatalf("notification post: %v", err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusAccepted {
		t.Errorf("notification status = %d, want 202", httpResp.StatusCode)
	}
	body, _ := io.ReadAll(httpResp.Body)
	if len(strings.TrimSpace(string(body))) != 0 {
		t.Errorf("notification body = %q, want empty", body)
	}
}

// TestBeforeInitializeGate pins the ordering rule: the tools methods refuse
// until initialize ran on this server process; ping and initialize themselves
// do not.
func TestBeforeInitializeGate(t *testing.T) {
	s, _ := newTestServer(t, nil)
	for _, method := range []string{"tools/list", "tools/call"} {
		resp := rawPost(t, s, fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q}`, method))
		if resp.Error == nil || resp.Error.Code != codeInvalidRequest {
			t.Errorf("%s before initialize = %+v, want code %d", method, resp, codeInvalidRequest)
		}
	}
	// ping works any time.
	resp := rawPost(t, s, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if resp.Error != nil {
		t.Errorf("ping before initialize = %+v", resp)
	}
	// initialize unlocks the methods.
	resp = rawPost(t, s, `{"jsonrpc":"2.0","id":3,"method":"initialize","params":{"protocolVersion":"`+protocolVersion+`"}}`)
	if resp.Error != nil {
		t.Fatalf("initialize: %v", resp.Error)
	}
	resp = rawPost(t, s, `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`)
	if resp.Error != nil {
		t.Errorf("tools/list after initialize = %+v", resp)
	}
}

// TestGETListenStream checks the server-to-client SSE surface: open, held
// silent, torn down when the client goes away.
func TestGETListenStream(t *testing.T) {
	s, _ := newTestServer(t, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content type = %q", ct)
	}
	// The stream stays open (no EOF while the request lives) and carries only
	// the keepalive comment.
	buf := make([]byte, 64)
	n, err := resp.Body.Read(buf)
	if err != nil {
		t.Fatalf("read keepalive: %v", err)
	}
	if got := string(buf[:n]); !strings.HasPrefix(got, ":") {
		t.Errorf("first frame = %q, want an SSE comment", got)
	}
	cancel()
	// The read drains after cancellation.
	_, _ = io.ReadAll(resp.Body)
}

// --- construction refusals ------------------------------------------------------

// stubTool is a test double for the tools.Tool edges the function tools do
// not reach: a hosted schema (refused at construction) and a registration
// that never answers (the no-result guard).
type stubTool struct {
	name    string
	hosted  bool
	schemas []tools.Schema
}

func (s *stubTool) Name() string         { return s.name }
func (s *stubTool) DeclaredInCode() bool { return true }
func (s *stubTool) Schemas(_ *agocontext.Context) ([]any, error) {
	if s.hosted {
		return []any{hostedSchema{}}, nil
	}
	out := make([]any, len(s.schemas))
	for i, sc := range s.schemas {
		out[i] = sc
	}
	return out, nil
}
func (s *stubTool) Register(_ *agocontext.Context, _ []middleware.Middleware) ([]stream.SubID, error) {
	return nil, nil
}

type hostedSchema struct{}

func (hostedSchema) SchemaType() string { return "web_search" }

func TestConstructionRefusals(t *testing.T) {
	// No Ask bridge.
	if _, err := NewServer(ServerConfig{}); !errors.Is(err, ErrNotProvided) {
		t.Errorf("NewServer without Ask = %v, want ErrNotProvided", err)
	}
	// A hosted (non-function) schema cannot be served.
	s, err := NewServer(ServerConfig{Ask: func(context.Context, string) (string, error) { return "", nil },
		Tools: []tools.Tool{&stubTool{name: "hosted", hosted: true}}})
	if err == nil || strings.Contains(errText(err), "tool name conflict") {
		t.Errorf("NewServer with a hosted schema = %v, want the function-schema refusal", err)
	}
	_ = s
	// A name colliding with the conversational tool's reserved name.
	_, err = NewServer(ServerConfig{Ask: func(context.Context, string) (string, error) { return "", nil },
		Tools: []tools.Tool{&stubTool{name: "ask", schemas: []tools.Schema{{
			Name: "ask", Description: "", Parameters: map[string]any{"type": "object"},
		}}}}})
	if !errors.Is(err, ErrConflict) {
		t.Errorf("ask-name collision = %v, want ErrConflict", err)
	}
	// Two entries sharing one name.
	_, err = NewServer(ServerConfig{Ask: func(context.Context, string) (string, error) { return "", nil },
		Tools: []tools.Tool{
			&stubTool{name: "a", schemas: []tools.Schema{{Name: "dup", Parameters: map[string]any{"type": "object"}}}},
			&stubTool{name: "b", schemas: []tools.Schema{{Name: "dup", Parameters: map[string]any{"type": "object"}}}},
		}})
	if !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate names = %v, want ErrConflict", err)
	}
	// A nil agent.
	if _, err := NewServerForAgent(nil); !errors.Is(err, ErrNotProvided) {
		t.Errorf("NewServerForAgent(nil) = %v, want ErrNotProvided", err)
	}
}

// TestNoResultGuard exercises the misbehaving-tool arm: a tool that registers
// but never answers must fail the call, not hang it.
func TestNoResultGuard(t *testing.T) {
	s, err := NewServer(ServerConfig{Ask: func(context.Context, string) (string, error) { return "", nil },
		Tools: []tools.Tool{&stubTool{name: "silent", schemas: []tools.Schema{{
			Name: "silent", Description: "", Parameters: map[string]any{"type": "object"},
		}}}}})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	resp := rawPost(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+protocolVersion+`"}}`)
	if resp.Error != nil {
		t.Fatalf("initialize: %v", resp.Error)
	}
	resp = rawPost(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/call",
		"params":{"name":"silent","arguments":{}}}`)
	if resp.Error != nil {
		t.Fatalf("tools/call silent: %v", resp.Error)
	}
	var result struct {
		Content []contentBlock `json:"content"`
		IsError bool           `json:"isError"`
	}
	if err := json.Unmarshal(mustRaw(t, resp.Result), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !result.IsError || len(result.Content) != 1 || !strings.Contains(result.Content[0].Text, "returned no result") {
		t.Errorf("silent tool result = %+v, want the no-result guard", result)
	}
}

// --- result content mapping (unit) ----------------------------------------------

func TestResultContentMapping(t *testing.T) {
	png := []byte("png-bytes")
	blocks := resultContent([]events.Input{
		events.TextInput{Content: "plain"},
		events.BinaryInput{Data: png, MediaType: "image/png", Kind: events.BinaryImage},
		events.BinaryInput{Data: []byte("wav"), MediaType: "audio/wav", Kind: events.BinaryAudio},
		events.BinaryInput{Data: []byte("pdf"), MediaType: "application/pdf", Kind: events.BinaryDocument},
		events.UrlInput{URL: "https://example.com/x", Kind: events.BinaryDocument},
		events.DataInput{Data: map[string]any{"a": 1}},
	}, "tool")
	if len(blocks) != 6 {
		t.Fatalf("blocks = %d, want 6", len(blocks))
	}
	if blocks[0].Type != "text" || blocks[0].Text != "plain" {
		t.Errorf("text block = %+v", blocks[0])
	}
	if blocks[1].Type != "image" || blocks[1].Data != base64.StdEncoding.EncodeToString(png) || blocks[1].MIMEType != "image/png" {
		t.Errorf("image block = %+v", blocks[1])
	}
	if blocks[2].Type != "audio" || blocks[2].MIMEType != "audio/wav" {
		t.Errorf("audio block = %+v", blocks[2])
	}
	if blocks[3].Type != "resource" || blocks[3].Resource == nil ||
		blocks[3].Resource.URI != "resource://tool" || blocks[3].Resource.MIMEType != "application/pdf" {
		t.Errorf("resource block = %+v", blocks[3])
	}
	if blocks[4].Type != "resource_link" || blocks[4].URI != "https://example.com/x" || blocks[4].Name != "tool" {
		t.Errorf("resource_link block = %+v", blocks[4])
	}
	if blocks[5].Type != "text" || blocks[5].Text != `{"a":1}` {
		t.Errorf("data block = %+v", blocks[5])
	}
	if got := resultContent(nil, "t"); len(got) != 1 || got[0].Text != "" {
		t.Errorf("empty result = %+v, want one empty text block", got)
	}
}

// --- helpers --------------------------------------------------------------------

func mustRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	return raw
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
