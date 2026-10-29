package mcp

import (
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

	agocontext "ago/context"
	"ago/events"
	"ago/middleware"
	"ago/stream"
	"ago/tools"
)

// --- fake streamable-HTTP MCP server ------------------------------------------

type fakeRequest struct {
	Method  string          `json:"method"`
	ID      *int64          `json:"id"`
	Params  json.RawMessage `json:"params"`
	Headers http.Header     `json:"-"`
}

type fakeServer struct {
	mu       sync.Mutex
	requests []fakeRequest
	inits    int // initialize requests seen
	calls    int // tools/call requests seen

	pngB64 string
	wavB64 string
}

func newFakeServer() *fakeServer {
	return &fakeServer{
		pngB64: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==",
		wavB64: base64.StdEncoding.EncodeToString([]byte("fake-wav-bytes")),
	}
}

func (s *fakeServer) recorded() []fakeRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]fakeRequest, len(s.requests))
	copy(out, s.requests)
	return out
}

func (s *fakeServer) countByMethod(method string) int {
	n := 0
	for _, r := range s.recorded() {
		if r.Method == method {
			n++
		}
	}
	return n
}

func (s *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var req fakeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	req.Headers = r.Header.Clone()
	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.mu.Unlock()

	switch req.Method {
	case "initialize":
		s.mu.Lock()
		s.inits++
		s.mu.Unlock()
		w.Header().Set("Mcp-Session-Id", "sess-7")
		writeRPCResult(w, req.ID, map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "fake-mcp", "version": "1.2"},
		})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		s.listTools(w, req)
	case "tools/call":
		s.mu.Lock()
		s.calls++
		firstCall := s.calls == 1
		s.mu.Unlock()
		s.callTool(w, req, firstCall)
	default:
		writeRPCError(w, req.ID, -32601, "method not found")
	}
}

func (s *fakeServer) listTools(w http.ResponseWriter, req fakeRequest) {
	var p struct {
		Cursor string `json:"cursor"`
	}
	_ = json.Unmarshal(req.Params, &p)
	if p.Cursor == "" {
		writeRPCResult(w, req.ID, map[string]any{
			"tools": []map[string]any{
				{"name": "alpha", "description": "Alpha tool",
					"inputSchema": map[string]any{"type": "object",
						"properties": map[string]any{"x": map[string]any{"type": "string"}}}},
				{"name": "beta", "description": "Beta tool",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
				{"name": "echo", "description": "Echo tool",
					"inputSchema": map[string]any{"type": "object",
						"properties": map[string]any{"msg": map[string]any{"type": "string"}}}},
				{"name": "boom", "description": "Boom tool",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
			},
			"nextCursor": "page-2",
		})
		return
	}
	if p.Cursor == "page-2" {
		writeRPCResult(w, req.ID, map[string]any{
			"tools": []map[string]any{
				{"name": "gamma", "description": "Gamma tool"},
				// A misbehaving server may list a name twice; the first wins.
				{"name": "alpha", "description": "Alpha duplicate"},
			},
		})
		return
	}
	writeRPCError(w, req.ID, -32602, "bad cursor")
}

func (s *fakeServer) callTool(w http.ResponseWriter, req fakeRequest, firstCall bool) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	_ = json.Unmarshal(req.Params, &p)
	switch p.Name {
	case "echo":
		if firstCall {
			// One expiring session: the first exchange is answered 404 and the
			// client must re-initialize and retry.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeRPCResult(w, req.ID, map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "hello"},
				{"type": "image", "data": s.pngB64, "mimeType": "image/png"},
				{"type": "audio", "data": s.wavB64, "mimeType": "audio/wav"},
				{"type": "resource_link", "uri": "file:///srv/report.pdf", "mimeType": "application/pdf"},
				{"type": "resource", "resource": map[string]any{
					"uri": "file:///srv/data.json", "mimeType": "application/json", "text": "{}",
				}},
			},
			"isError": false,
		})
	case "empty":
		writeRPCResult(w, req.ID, map[string]any{"content": []any{}, "isError": false})
	case "boom":
		writeRPCResult(w, req.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": "kaput"}},
			"isError": true,
		})
	case "rpc_err":
		writeRPCError(w, req.ID, -32602, "bad params")
	case "sse_echo":
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", `{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info"}}`)
		res, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": "via sse"}}, "isError": false,
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", res)
	default:
		writeRPCError(w, req.ID, -32601, "unknown tool")
	}
}

func writeRPCResult(w http.ResponseWriter, id *int64, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func writeRPCError(w http.ResponseWriter, id *int64, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": msg},
	})
}

// newHTTPClient points a Client at a fresh fake server.
func newHTTPClient(t *testing.T, cfg func(*MCPServerConfig)) (*Client, *fakeServer) {
	t.Helper()
	srv := newFakeServer()
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	c := &MCPServerConfig{URL: hs.URL}
	if cfg != nil {
		cfg(c)
	}
	cl, err := New(c)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { cl.Close() })
	return cl, srv
}

// --- New() validation ----------------------------------------------------------

func TestNewConfigValidation(t *testing.T) {
	t.Run("unsupported", func(t *testing.T) {
		for _, cfg := range []any{nil, 42, (*MCPServerConfig)(nil), (*MCPStdioConfig)(nil)} {
			if _, err := New(cfg); !errors.Is(err, ErrUnsupportedConfig) {
				t.Errorf("New(%v) = %v, want ErrUnsupportedConfig", cfg, err)
			}
		}
	})
	t.Run("missing url", func(t *testing.T) {
		if _, err := New(&MCPServerConfig{}); !errors.Is(err, ErrNotProvided) {
			t.Errorf("empty URL = %v, want ErrNotProvided", err)
		}
	})
	t.Run("non-http url", func(t *testing.T) {
		if _, err := New(&MCPServerConfig{URL: "ftp://example.com/mcp"}); !errors.Is(err, ErrNotProvided) {
			t.Errorf("ftp URL = %v, want ErrNotProvided", err)
		}
		if _, err := New(&MCPServerConfig{URL: "not a url"}); !errors.Is(err, ErrNotProvided) {
			t.Errorf("garbage URL = %v, want ErrNotProvided", err)
		}
	})
	t.Run("missing command", func(t *testing.T) {
		if _, err := New(&MCPStdioConfig{}); !errors.Is(err, ErrNotProvided) {
			t.Errorf("empty command = %v, want ErrNotProvided", err)
		}
	})
	t.Run("value forms accepted", func(t *testing.T) {
		if _, err := New(MCPStdioConfig{Command: "whatever"}); err != nil {
			t.Errorf("value stdio config: %v", err)
		}
	})
}

// --- handshake -----------------------------------------------------------------

func TestHTTPInitialize(t *testing.T) {
	cl, srv := newHTTPClient(t, func(c *MCPServerConfig) {
		c.AuthorizationToken = "tok1"
		c.Headers = map[string]string{"X-Custom": "v1"}
	})
	info, err := cl.Initialize()
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if info.Name != "fake-mcp" || info.Version != "1.2" {
		t.Errorf("server info = %+v, want name fake-mcp version 1.2", info)
	}
	if info.ProtocolVersion != "2024-11-05" {
		t.Errorf("protocol version = %q", info.ProtocolVersion)
	}
	if _, ok := info.Capabilities["tools"]; !ok {
		t.Errorf("capabilities = %v, want tools entry", info.Capabilities)
	}

	// Idempotent: no second handshake.
	again, err := cl.Initialize()
	if err != nil {
		t.Fatalf("second Initialize: %v", err)
	}
	if again.Name != info.Name || again.Version != info.Version || again.ProtocolVersion != info.ProtocolVersion {
		t.Errorf("second Initialize = %+v, want %+v", again, info)
	}
	if got := srv.countByMethod("initialize"); got != 1 {
		t.Fatalf("initialize requests = %d, want 1", got)
	}

	// The handshake request shape and headers.
	var initReq *fakeRequest
	for _, r := range srv.recorded() {
		if r.Method == "initialize" {
			initReq = &r
		}
	}
	if initReq == nil {
		t.Fatal("no initialize request recorded")
	}
	var params struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ClientInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"clientInfo"`
	}
	if err := json.Unmarshal(initReq.Params, &params); err != nil {
		t.Fatalf("initialize params: %v", err)
	}
	if params.ProtocolVersion != "2024-11-05" {
		t.Errorf("offered protocolVersion = %q", params.ProtocolVersion)
	}
	if params.ClientInfo.Name == "" {
		t.Error("clientInfo.name is empty")
	}
	if params.Capabilities == nil {
		t.Error("capabilities must be an object, got null")
	}
	if got := initReq.Headers.Get("Authorization"); got != "Bearer tok1" {
		t.Errorf("Authorization = %q, want Bearer tok1", got)
	}
	if got := initReq.Headers.Get("X-Custom"); got != "v1" {
		t.Errorf("X-Custom = %q, want v1", got)
	}
	if got := initReq.Headers.Get("Accept"); !strings.Contains(got, "application/json") || !strings.Contains(got, "text/event-stream") {
		t.Errorf("Accept = %q, want both application/json and text/event-stream", got)
	}
	if got := initReq.Headers.Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("Content-Type = %q", got)
	}
	if initReq.Headers.Get("Mcp-Session-Id") != "" {
		t.Error("initialize must not carry a session id")
	}

	// The confirmation notification went out, and the session id follows it.
	if got := srv.countByMethod("notifications/initialized"); got != 1 {
		t.Errorf("initialized notifications = %d, want 1", got)
	}
	if _, err := cl.CallTool("empty", nil); err != nil {
		t.Fatalf("CallTool after initialize: %v", err)
	}
	var lastCall fakeRequest
	found := false
	for _, r := range srv.recorded() {
		if r.Method == "tools/call" {
			lastCall = r
			found = true
		}
	}
	if !found {
		t.Fatal("no tools/call request recorded")
	}
	if lastCall.Headers.Get("Mcp-Session-Id") != "sess-7" {
		t.Errorf("tools/call Mcp-Session-Id = %q, want sess-7", lastCall.Headers.Get("Mcp-Session-Id"))
	}
}

// --- tools/list pagination -----------------------------------------------------

func TestHTTPListToolsPagination(t *testing.T) {
	cl, srv := newHTTPClient(t, nil)
	got, err := cl.ListTools()
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	for _, tl := range got {
		names = append(names, tl.Name)
	}
	want := []string{"alpha", "beta", "echo", "boom", "gamma", "alpha"}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Errorf("tools = %v, want %v", names, want)
	}
	if got[0].Description != "Alpha tool" {
		t.Errorf("alpha description = %q", got[0].Description)
	}
	if got[0].InputSchema["type"] != "object" {
		t.Errorf("alpha inputSchema = %v", got[0].InputSchema)
	}

	// The second page was fetched with the server's cursor.
	var cursors []string
	for _, r := range srv.recorded() {
		if r.Method != "tools/list" {
			continue
		}
		var p struct {
			Cursor string `json:"cursor"`
		}
		_ = json.Unmarshal(r.Params, &p)
		cursors = append(cursors, p.Cursor)
	}
	if fmt.Sprint(cursors) != fmt.Sprint([]string{"", "page-2"}) {
		t.Errorf("cursors = %v, want [ page-2]", cursors)
	}
}

// --- tools/call result mapping --------------------------------------------------

func TestHTTPCallToolContentMapping(t *testing.T) {
	cl, _ := newHTTPClient(t, nil)
	if _, err := cl.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	out, err := cl.CallTool("echo", map[string]any{"msg": "hi"})
	if err != nil {
		t.Fatalf("CallTool echo: %v", err)
	}
	if len(out.Raw) == 0 {
		t.Error("Raw result is empty")
	}
	if len(out.Parts) != 5 {
		t.Fatalf("parts = %d, want 5", len(out.Parts))
	}

	text, ok := events.AsValue(out.Parts[0]).(events.TextInput)
	if !ok || text.Content != "hello" {
		t.Errorf("part 0 = %#v, want TextInput hello", out.Parts[0])
	}
	img, ok := events.AsValue(out.Parts[1]).(events.BinaryInput)
	if !ok || img.Kind != events.BinaryImage || img.MediaType != "image/png" || len(img.Data) == 0 {
		t.Errorf("part 1 = %#v, want png BinaryInput", out.Parts[1])
	}
	if want, _ := base64.StdEncoding.DecodeString(newFakeServer().pngB64); string(img.Data) != string(want) {
		t.Error("image data does not decode to the server's bytes")
	}
	aud, ok := events.AsValue(out.Parts[2]).(events.BinaryInput)
	if !ok || aud.Kind != events.BinaryAudio || aud.MediaType != "audio/wav" || string(aud.Data) != "fake-wav-bytes" {
		t.Errorf("part 2 = %#v, want wav BinaryInput", out.Parts[2])
	}
	link, ok := events.AsValue(out.Parts[3]).(events.UrlInput)
	if !ok || link.URL != "file:///srv/report.pdf" || link.Kind != events.BinaryDocument {
		t.Errorf("part 3 = %#v, want UrlInput document", out.Parts[3])
	}
	embedded, ok := events.AsValue(out.Parts[4]).(events.TextInput)
	if !ok {
		t.Fatalf("part 4 = %#v, want TextInput with JSON", out.Parts[4])
	}
	var block map[string]any
	if err := json.Unmarshal([]byte(embedded.Content), &block); err != nil {
		t.Fatalf("embedded part is not JSON: %v (%q)", err, embedded.Content)
	}
	if block["type"] != "resource" {
		t.Errorf("embedded block type = %v", block["type"])
	}
	res, _ := block["resource"].(map[string]any)
	if res == nil || res["uri"] != "file:///srv/data.json" {
		t.Errorf("embedded resource = %v", block["resource"])
	}
}

func TestHTTPCallToolEmptyContent(t *testing.T) {
	cl, _ := newHTTPClient(t, nil)
	out, err := cl.CallTool("empty", nil)
	if err != nil {
		t.Fatalf("CallTool empty: %v", err)
	}
	if len(out.Parts) != 1 {
		t.Fatalf("parts = %d, want 1", len(out.Parts))
	}
	text, ok := events.AsValue(out.Parts[0]).(events.TextInput)
	if !ok || text.Content != `{"content":[],"isError":false}` {
		t.Errorf("empty content part = %#v", out.Parts[0])
	}
}

func TestHTTPCallToolIsError(t *testing.T) {
	cl, _ := newHTTPClient(t, nil)
	_, err := cl.CallTool("boom", nil)
	if !errors.Is(err, ErrToolCall) {
		t.Fatalf("err = %v, want ErrToolCall", err)
	}
	if !strings.Contains(err.Error(), "kaput") {
		t.Errorf("error text %q does not carry the server's message", err.Error())
	}
}

func TestHTTPCallToolRPCError(t *testing.T) {
	cl, _ := newHTTPClient(t, nil)
	_, err := cl.CallTool("rpc_err", nil)
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("err = %v, want ErrProtocol", err)
	}
	if !strings.Contains(err.Error(), "bad params") {
		t.Errorf("error text %q does not name the server's message", err.Error())
	}
}

func TestHTTPCallToolOverSSE(t *testing.T) {
	cl, _ := newHTTPClient(t, nil)
	out, err := cl.CallTool("sse_echo", nil)
	if err != nil {
		t.Fatalf("CallTool sse_echo: %v", err)
	}
	if len(out.Parts) != 1 {
		t.Fatalf("parts = %d, want 1", len(out.Parts))
	}
	if text, ok := events.AsValue(out.Parts[0]).(events.TextInput); !ok || text.Content != "via sse" {
		t.Errorf("part 0 = %#v, want TextInput via sse", out.Parts[0])
	}
}

func TestHTTPSessionExpiredRetry(t *testing.T) {
	cl, srv := newHTTPClient(t, nil)
	// The fake answers the first tools/call with 404; the client must
	// re-initialize and succeed on the retry.
	out, err := cl.CallTool("echo", nil)
	if err != nil {
		t.Fatalf("CallTool with expired session: %v", err)
	}
	if len(out.Parts) != 5 {
		t.Errorf("parts = %d, want 5", len(out.Parts))
	}
	if got := srv.countByMethod("initialize"); got != 2 {
		t.Errorf("initialize requests = %d, want 2 (initial + rebuild)", got)
	}
}

// --- Tools(): filters, prefix, wrapping -----------------------------------------

func TestHTTPToolsFiltersAndPrefix(t *testing.T) {
	cl, _ := newHTTPClient(t, func(c *MCPServerConfig) {
		c.AllowedTools = []string{"alpha"}
		c.BlockedTools = []string{"gamma"}
		c.ToolNamePrefix = "srv_"
	})
	got, err := Tools(cl)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("tools = %d, want 1 (alpha allowed, gamma blocked, beta not allowed)", len(got))
	}
	rt, ok := got[0].(*remoteTool)
	if !ok {
		t.Fatalf("tool type = %T", got[0])
	}
	if got[0].Name() != "srv_alpha" {
		t.Errorf("name = %q, want srv_alpha", got[0].Name())
	}
	if got[0].DeclaredInCode() {
		t.Error("remote tool must not be declared in code")
	}
	if rt.remoteName != "alpha" {
		t.Errorf("remote name = %q, want alpha (prefix never reaches the server)", rt.remoteName)
	}
	if !strings.HasPrefix(rt.source, "mcp:") || !strings.HasSuffix(rt.source, ":alpha") {
		t.Errorf("source = %q, want mcp:<endpoint>:alpha", rt.source)
	}
	schemas, err := got[0].Schemas(agocontext.New(stream.NewMemoryStream()))
	if err != nil || len(schemas) != 1 {
		t.Fatalf("Schemas = %v, %v", schemas, err)
	}
	schema, ok := schemas[0].(tools.Schema)
	if !ok {
		t.Fatalf("schema type = %T", schemas[0])
	}
	if schema.Name != "srv_alpha" || schema.Description != "Alpha tool" {
		t.Errorf("schema = %+v", schema)
	}
	if schema.Parameters["type"] != "object" {
		t.Errorf("parameters = %v", schema.Parameters)
	}
}

func TestHTTPToolsDedupAndSchemaNormalization(t *testing.T) {
	cl, _ := newHTTPClient(t, nil)
	got, err := Tools(cl)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	var names []string
	for _, tl := range got {
		names = append(names, tl.Name())
	}
	// The duplicated alpha keeps its first listing; gamma had no inputSchema and
	// gets the no-argument object.
	if fmt.Sprint(names) != fmt.Sprint([]string{"alpha", "beta", "echo", "boom", "gamma"}) {
		t.Errorf("names = %v", names)
	}
	var gamma tools.Tool
	for _, tl := range got {
		if tl.Name() == "gamma" {
			gamma = tl
		}
	}
	schemas, err := gamma.Schemas(nil)
	if err != nil || len(schemas) != 1 {
		t.Fatalf("gamma Schemas = %v, %v", schemas, err)
	}
	schema := schemas[0].(tools.Schema)
	if schema.Parameters["type"] != "object" {
		t.Errorf("gamma parameters = %v, want normalized object", schema.Parameters)
	}
	if _, hasProps := schema.Parameters["properties"]; !hasProps {
		t.Errorf("gamma parameters = %v, want properties entry", schema.Parameters)
	}
}

func TestToolsNilClient(t *testing.T) {
	if _, err := Tools(nil); err == nil {
		t.Error("Tools(nil) must fail")
	}
}

// --- the tools.Tool contract on a live stream ------------------------------------

func TestRemoteToolOnStream(t *testing.T) {
	cl, srv := newHTTPClient(t, func(c *MCPServerConfig) {
		c.ToolNamePrefix = "srv_"
	})
	remote, err := Tools(cl)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	byName := map[string]tools.Tool{}
	var names []string
	for _, tl := range remote {
		byName[tl.Name()] = tl
		names = append(names, tl.Name())
	}
	if len(byName) != 5 {
		t.Fatalf("registered names = %v", names)
	}

	ms := stream.NewMemoryStream()
	cx := agocontext.New(ms)
	exec := tools.NewExecutor()
	known := map[string]bool{}
	for _, tl := range remote {
		known[tl.Name()] = true
	}
	for _, tl := range remote {
		if err := tl.Register(cx, nil); err != nil {
			t.Fatalf("Register %s: %v", tl.Name(), err)
		}
	}
	if err := exec.Register(cx, remote, known, []middleware.Middleware{}); err != nil {
		t.Fatalf("Executor.Register: %v", err)
	}

	var results []events.ToolResultsEvent
	ms.Subscribe(func(ev events.Event) error {
		if r, ok := events.AsValue(ev).(events.ToolResultsEvent); ok {
			results = append(results, r)
		}
		return nil
	}, stream.SubscribeOptions{Condition: events.TypeOf[events.ToolResultsEvent]()})

	calls := events.ToolCallsEvent{Calls: []events.ToolCallEvent{
		{ID: "t1", Name: "srv_echo", Arguments: `{"msg":"hi"}`},
		{ID: "t2", Name: "srv_boom", Arguments: `{}`},
	}}
	if err := ms.Send(calls); err != nil {
		t.Fatalf("send calls: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results batches = %d, want 1", len(results))
	}
	if len(results[0].Results) != 2 {
		t.Fatalf("results = %d, want 2", len(results[0].Results))
	}

	ok := results[0].Results[0]
	if ok.ParentID != "t1" || ok.Name != "srv_echo" {
		t.Errorf("result 0 = %+v", ok)
	}
	if len(ok.Result.Parts) != 5 {
		t.Errorf("echo parts = %d, want 5", len(ok.Result.Parts))
	}

	failed := results[0].Results[1]
	if failed.ParentID != "t2" {
		t.Errorf("result 1 parent = %q, want t2", failed.ParentID)
	}
	var texts []string
	for _, p := range failed.Result.Parts {
		if ti, isText := events.AsValue(p).(events.TextInput); isText {
			texts = append(texts, ti.Content)
		}
	}
	if len(texts) != 1 || !strings.Contains(texts[0], "kaput") {
		t.Errorf("failure parts = %v, want the server's error text", texts)
	}

	// The outbound call carried the unprefixed server name and the arguments.
	var sentName, argsJSON string
	for _, r := range srv.recorded() {
		if r.Method != "tools/call" {
			continue
		}
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(r.Params, &p)
		if p.Name == "echo" {
			sentName = p.Name
			b, _ := json.Marshal(p.Arguments)
			argsJSON = string(b)
		}
	}
	if sentName != "echo" {
		t.Errorf("outbound tool name = %q, want the server-side echo", sentName)
	}
	if argsJSON != `{"msg":"hi"}` {
		t.Errorf("outbound arguments = %s, want {\"msg\":\"hi\"}", argsJSON)
	}
}
