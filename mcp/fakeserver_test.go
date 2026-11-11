package mcp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The scenario fake is one MCP server "brain" served over two transports — an
// httptest streamable-HTTP endpoint (below) and the stdio helper subprocess
// (stdio_test.go) — so every server-initiated-request scenario runs against both
// through the same assertions. Each tool is a scenario; its result echoes what
// the server received, so a test reads the server's view through the tool result
// (the stdio server lives in another process).

// fakeAsk sends one server-to-client request and waits for the client's answer.
type fakeAsk func(method string, params any) (json.RawMessage, *rpcError)

// notifyPrefix turns an ask into a one-way notification: the scenario tools reach
// both transports' fakes through the one ask function.
const notifyPrefix = "notify:"

type brain struct {
	// modern makes the server speak the 2026-07-28 era: requests are validated
	// like mcp.shared.inbound.classify_inbound_request (envelope in _meta, then
	// the headers, then the version) and server/discover answers a real
	// DiscoverResult. Otherwise discover is method-not-found, like a
	// handshake-era server.
	modern bool
	// unsupported, when set, makes server/discover fail with -32022 naming
	// these revisions instead of answering.
	unsupported []string
	// initCaps is the capability object received at initialize.
	initCaps json.RawMessage
}

type brainCall struct {
	Name           string                     `json:"name"`
	Arguments      map[string]any             `json:"arguments"`
	InputResponses map[string]json.RawMessage `json:"inputResponses"`
	RequestState   string                     `json:"requestState"`
	Meta           map[string]json.RawMessage `json:"_meta"`
}

var brainTools = []string{"rst", "huge", "primed", "notify", "cut", "caps", "elicit", "sample", "roots", "ping", "needs_input", "resource", "state_only", "sample_media", "idle_ping", "idle_report", "echo"}

func textResult(text string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": false}
}

func rpcFail(code int, msg string) *rpcError { return &rpcError{Code: code, Message: msg} }

const (
	metaVersionKey = "io.modelcontextprotocol/protocolVersion"
	metaCapsKey    = "io.modelcontextprotocol/clientCapabilities"
	metaInfoKey    = "io.modelcontextprotocol/clientInfo"
	metaServerKey  = "io.modelcontextprotocol/serverInfo"
)

// decodeHeaderValue is the inverse of encodeHeaderValue
// (mcp.shared.inbound.decode_header_value).
func decodeHeaderValue(v string) string {
	if strings.HasPrefix(v, "=?base64?") && strings.HasSuffix(v, "?=") {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(strings.TrimPrefix(v, "=?base64?"), "?="))
		if err != nil {
			return "\x00undecodable"
		}
		return string(raw)
	}
	return v
}

// classify is the modern validation ladder: the envelope pair in params._meta,
// then (over HTTP, headers != nil) MCP-Protocol-Version / Mcp-Method / Mcp-Name
// against the body, then the version itself. First failure wins.
func (b *brain) classify(method string, params json.RawMessage, headers map[string]string) *rpcError {
	var p map[string]json.RawMessage
	_ = json.Unmarshal(params, &p)
	var meta map[string]json.RawMessage
	if raw, ok := p["_meta"]; !ok || json.Unmarshal(raw, &meta) != nil || meta == nil {
		return rpcFail(codeInvalidParams, "params._meta must be an object carrying the required envelope keys")
	}
	for _, key := range []string{metaVersionKey, metaCapsKey} {
		if _, ok := meta[key]; !ok {
			return rpcFail(codeInvalidParams, "params._meta is missing the required envelope key(s): "+key)
		}
	}
	var version string
	versionIsString := json.Unmarshal(meta[metaVersionKey], &version) == nil
	if headers != nil {
		if !versionIsString || headers["mcp-protocol-version"] != version {
			return rpcFail(codeHeaderMismatch, "mcp-protocol-version header does not match the request envelope's protocol version")
		}
		if headers["mcp-method"] != method {
			return rpcFail(codeHeaderMismatch, "mcp-method header does not match the request body's method")
		}
		if method == "tools/call" {
			var name string
			if json.Unmarshal(p["name"], &name) == nil && decodeHeaderValue(headers["mcp-name"]) != name {
				return rpcFail(codeHeaderMismatch, "mcp-name header does not match the request body's 'name' parameter")
			}
		}
	}
	if !versionIsString {
		return rpcFail(codeInvalidParams, "protocol version must be a string")
	}
	if !containsString(modernProtocolVersions, version) {
		e := rpcFail(codeUnsupportedProtocolVersion, "Unsupported protocol version")
		e.Data, _ = json.Marshal(map[string]any{"supported": modernProtocolVersions, "requested": version})
		return e
	}
	return nil
}

func hasEnvelope(params json.RawMessage) bool {
	var p struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	_ = json.Unmarshal(params, &p)
	_, ok := p.Meta[metaVersionKey]
	return ok
}

// handle answers one client request. Methods it does not know are
// method-not-found. headers are the lowercased transport headers (nil over stdio).
func (b *brain) handle(method string, params json.RawMessage, headers map[string]string, ask fakeAsk) (any, *rpcError) {
	if b.modern && method != "initialize" {
		if !hasEnvelope(params) {
			// No envelope: the request is routed to the legacy path, where
			// discover does not exist.
			if method == "server/discover" {
				return nil, rpcFail(codeMethodNotFound, "method not found: server/discover")
			}
			return nil, rpcFail(codeInvalidParams, "params._meta must be an object carrying the required envelope keys")
		}
		if rpcErr := b.classify(method, params, headers); rpcErr != nil {
			return nil, rpcErr
		}
	}
	switch method {
	case "initialize":
		var p struct {
			Capabilities json.RawMessage `json:"capabilities"`
		}
		_ = json.Unmarshal(params, &p)
		b.initCaps = p.Capabilities
		return map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "scenario", "version": "1"},
		}, nil
	case "server/discover":
		if !b.modern {
			return nil, rpcFail(codeMethodNotFound, "method not found: server/discover")
		}
		if b.unsupported != nil {
			e := rpcFail(codeUnsupportedProtocolVersion, "Unsupported protocol version")
			e.Data, _ = json.Marshal(map[string]any{"supported": b.unsupported, "requested": "2026-07-28"})
			return nil, e
		}
		return map[string]any{
			"resultType":        "complete",
			"supportedVersions": []string{"2026-07-28"},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"_meta":             map[string]any{metaServerKey: map[string]any{"name": "scenario-modern", "version": "2"}},
		}, nil
	case "tools/list":
		list := make([]map[string]any, 0, len(brainTools))
		for _, n := range brainTools {
			list = append(list, map[string]any{"name": n, "description": n + " scenario", "inputSchema": map[string]any{"type": "object"}})
		}
		return map[string]any{"tools": list}, nil
	case "tools/call":
		var c brainCall
		if err := json.Unmarshal(params, &c); err != nil {
			return nil, rpcFail(codeInvalidParams, err.Error())
		}
		return b.call(c, ask)
	default:
		return nil, rpcFail(codeMethodNotFound, "method not found: "+method)
	}
}

func echoJSON(prefix string, raw json.RawMessage, rpcErr *rpcError) (any, *rpcError) {
	if rpcErr != nil {
		return textResult(prefix + "error:" + rpcErr.Message), nil
	}
	return textResult(prefix + string(raw)), nil
}

func elicitationParams(shape string) map[string]any {
	props := map[string]any{"answer": map[string]any{"type": "string"}}
	switch shape {
	case "multi":
		props["extra"] = map[string]any{"type": "string"}
	}
	p := map[string]any{
		"message":         "Pick a color",
		"requestedSchema": map[string]any{"type": "object", "properties": props},
	}
	if shape == "url" {
		p["mode"] = "url"
		p["url"] = "https://example.test/auth"
	}
	return p
}

func (b *brain) call(c brainCall, ask fakeAsk) (any, *rpcError) {
	switch c.Name {
	case "caps":
		if b.modern {
			if raw, ok := c.Meta[metaCapsKey]; ok {
				return textResult(string(raw)), nil
			}
			return textResult("no-meta"), nil
		}
		return textResult(string(b.initCaps)), nil
	case "echo":
		return textResult("echo"), nil
	case "notify":
		// Two notifications ahead of the result, as a server streaming progress does.
		ask(notifyPrefix+"notifications/progress", map[string]any{"progressToken": "t", "progress": 1, "message": "first"})
		ask(notifyPrefix+"notifications/message", map[string]any{"level": "info", "data": "second", "logger": "scenario"})
		return textResult("notified"), nil
	case "elicit":
		shape, _ := c.Arguments["shape"].(string)
		raw, rpcErr := ask("elicitation/create", elicitationParams(shape))
		return echoJSON("elicited:", raw, rpcErr)
	case "sample":
		raw, rpcErr := ask("sampling/createMessage", map[string]any{
			"messages": []map[string]any{
				{"role": "user", "content": map[string]any{"type": "text", "text": "what is 2+2?"}},
			},
			"systemPrompt": "be brief",
			"maxTokens":    16,
		})
		return echoJSON("sampled:", raw, rpcErr)
	case "sample_media":
		// A conversation carrying an array of blocks: text, image and audio —
		// or, with arguments.bad set, an image whose payload is not base64.
		imageData := "cG5nLWJ5dGVz"
		if bad, _ := c.Arguments["bad"].(bool); bad {
			imageData = "***"
		}
		raw, rpcErr := ask("sampling/createMessage", map[string]any{
			"messages": []map[string]any{
				{"role": "user", "content": []map[string]any{
					{"type": "text", "text": "look and listen"},
					{"type": "image", "data": imageData, "mimeType": "image/png"},
					{"type": "audio", "data": "d2F2LWJ5dGVz", "mimeType": "audio/wav"},
				}},
				{"role": "assistant", "content": map[string]any{"type": "text", "text": "noted"}},
			},
			"maxTokens": 8,
		})
		return echoJSON("sampled:", raw, rpcErr)
	case "state_only":
		// A round that carries only a state token: nothing to answer, the client
		// backs off and retries without inputResponses.
		if c.RequestState == "" {
			return map[string]any{"resultType": "input_required", "requestState": "s1"}, nil
		}
		return textResult(fmt.Sprintf("state_only done state=%s responses=%d", c.RequestState, len(c.InputResponses))), nil
	case "roots":
		raw, rpcErr := ask("roots/list", nil)
		return echoJSON("roots:", raw, rpcErr)
	case "ping":
		raw, rpcErr := ask("ping", nil)
		return echoJSON("pong:", raw, rpcErr)
	case "needs_input":
		return b.needsInput(c)
	case "resource":
		return map[string]any{"content": []map[string]any{
			{"type": "resource", "resource": map[string]any{"uri": "file:///a.txt", "mimeType": "text/plain", "text": "embedded text"}},
			{"type": "resource", "resource": map[string]any{"uri": "file:///b.png", "mimeType": "image/png", "blob": "cG5nLWJ5dGVz"}},
			{"type": "resource", "resource": map[string]any{"uri": "file:///c.bin", "blob": "Ymlu"}},
			{"type": "resource", "resource": map[string]any{"uri": "file:///d"}},
		}}, nil
	default:
		return nil, rpcFail(codeInvalidParams, "unknown tool "+c.Name)
	}
}

// needsInput is the modern-era scenario: instead of a result the server returns
// input_required (a question plus a state token) until the client has answered
// "rounds" times, then completes with an echo of what the last round carried.
func (b *brain) needsInput(c brainCall) (any, *rpcError) {
	rounds := 1
	if v, ok := c.Arguments["rounds"].(float64); ok {
		rounds = int(v)
	}
	done := 0
	if c.RequestState != "" {
		fmt.Sscanf(c.RequestState, "state-%d", &done)
	}
	if done >= rounds {
		answers := map[string]json.RawMessage{}
		for k, v := range c.InputResponses {
			answers[k] = v
		}
		enc, _ := json.Marshal(answers)
		return textResult(fmt.Sprintf("completed after %d rounds state=%s answers=%s", done, c.RequestState, enc)), nil
	}
	return map[string]any{
		"resultType": "input_required",
		"inputRequests": map[string]any{
			"q1": map[string]any{"method": "elicitation/create", "params": elicitationParams("")},
		},
		"requestState": fmt.Sprintf("state-%d", done+1),
	}, nil
}

// --- streamable HTTP over httptest ----------------------------------------------

type loggedRequest struct {
	Method       string
	Params       json.RawMessage
	Session      string
	ProtoVersion string
	Host         string
	// Reply marks a POST that carried the client's reply to a server request.
	Reply bool
	// Headers are the request's headers, lowercased.
	Headers map[string]string
}

type scenarioHTTP struct {
	brain brain

	mu      sync.Mutex
	log     []loggedRequest
	pending map[string]chan wireMessage
	nextReq atomic.Int64

	// getStatus, when non-zero, answers every GET with that status (a server
	// without a standalone stream: 405).
	getStatus int
	// push feeds the standalone GET stream: each value is sent as one event.
	push chan map[string]any
	// cutReq is the request id of a "cut" call whose stream was dropped before its
	// response; resumes counts the GETs that carried a Last-Event-ID.
	cutReq  json.RawMessage
	resumes int
	// resumeEnds makes a resume answer with a stream that ends without the
	// response (exhausting the client's reconnect attempts).
	resumeEnds bool
}

func newScenarioHTTP(modern bool) *scenarioHTTP {
	return &scenarioHTTP{brain: brain{modern: modern}, pending: map[string]chan wireMessage{}, push: make(chan map[string]any, 16)}
}

// serveGet is the standalone stream of streamable HTTP, held to the SDK's rules:
// it needs text/event-stream in Accept and the session id, answers anything else
// with an error status, and streams what the test pushes.
func (s *scenarioHTTP) serveGet(w http.ResponseWriter, r *http.Request) {
	entry := loggedRequest{Method: "GET", Host: r.Host, Session: r.Header.Get("Mcp-Session-Id"), Headers: map[string]string{
		"accept": r.Header.Get("Accept"), "last-event-id": r.Header.Get("Last-Event-ID"), "mcp-protocol-version": r.Header.Get("MCP-Protocol-Version"),
	}}
	s.record(entry)
	if s.getStatus != 0 {
		w.WriteHeader(s.getStatus)
		return
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		w.WriteHeader(http.StatusNotAcceptable)
		return
	}
	if r.Header.Get("Mcp-Session-Id") != "sess-1" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl := w.(http.Flusher)
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		s.mu.Lock()
		s.resumes++
		reqID, ends := s.cutReq, s.resumeEnds
		s.mu.Unlock()
		if ends {
			fmt.Fprint(w, "id: e-again\nretry: 5\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{\"data\":\"still going\"}}\n\n")
			fl.Flush()
			return
		}
		if last != "e1" || reqID == nil {
			return
		}
		res, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": reqID, "result": textResult("resumed")})
		fmt.Fprintf(w, "id: e2\nevent: message\ndata: %s\n\n", res)
		fl.Flush()
		return
	}
	fmt.Fprint(w, ": open\n\n")
	fl.Flush()
	for {
		select {
		case msg := <-s.push:
			b, _ := json.Marshal(msg)
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *scenarioHTTP) record(r loggedRequest) {
	s.mu.Lock()
	s.log = append(s.log, r)
	s.mu.Unlock()
}

// requests returns the logged client requests (not replies) for one method.
func (s *scenarioHTTP) requests(method string) []loggedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []loggedRequest
	for _, r := range s.log {
		if r.Method == method {
			out = append(out, r)
		}
	}
	return out
}

func (s *scenarioHTTP) methods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, r := range s.log {
		if !r.Reply {
			out = append(out, r.Method)
		}
	}
	return out
}

type httpExchange struct {
	s   *scenarioHTTP
	w   http.ResponseWriter
	sse bool
}

func (x *httpExchange) writeEvent(v any) {
	if !x.sse {
		x.w.Header().Set("Content-Type", "text/event-stream")
		x.sse = true
	}
	b, _ := json.Marshal(v)
	fmt.Fprintf(x.w, "data: %s\n\n", b)
	x.w.(http.Flusher).Flush()
}

func (x *httpExchange) ask(method string, params any) (json.RawMessage, *rpcError) {
	if strings.HasPrefix(method, notifyPrefix) {
		msg := map[string]any{"jsonrpc": "2.0", "method": strings.TrimPrefix(method, notifyPrefix)}
		if params != nil {
			msg["params"] = params
		}
		x.writeEvent(msg)
		return json.RawMessage(`{}`), nil
	}
	id := fmt.Sprintf("srv-%d", x.s.nextReq.Add(1))
	ch := make(chan wireMessage, 1)
	x.s.mu.Lock()
	x.s.pending[`"`+id+`"`] = ch
	x.s.mu.Unlock()
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	x.writeEvent(msg)
	select {
	case reply := <-ch:
		if reply.Error != nil {
			return nil, reply.Error
		}
		return reply.Result, nil
	case <-time.After(10 * time.Second):
		return nil, rpcFail(-1, "client never answered "+method)
	}
}

func (s *scenarioHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.serveGet(w, r)
		return
	}
	if r.Method == http.MethodDelete {
		s.record(loggedRequest{Method: "DELETE", Session: r.Header.Get("Mcp-Session-Id"), Host: r.Host})
		return
	}
	body, _ := io.ReadAll(r.Body)
	var msg wireMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	headers := map[string]string{}
	for k := range r.Header {
		headers[strings.ToLower(k)] = r.Header.Get(k)
	}
	entry := loggedRequest{
		Method: msg.Method, Params: msg.Params, Host: r.Host, Headers: headers,
		Session: r.Header.Get("Mcp-Session-Id"), ProtoVersion: r.Header.Get("MCP-Protocol-Version"),
	}
	if msg.Method == "" && msg.hasID() {
		// The client's reply to a request the server sent on a stream.
		entry.Reply = true
		s.record(entry)
		s.mu.Lock()
		ch := s.pending[string(msg.ID)]
		delete(s.pending, string(msg.ID))
		s.mu.Unlock()
		if ch != nil {
			ch <- msg
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	s.record(entry)
	if !msg.hasID() {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if msg.Method == "tools/call" && strings.Contains(string(msg.Params), `"name":"rst"`) {
		// The connection dies mid-stream, with no clean end: a broken read, not EOF.
		s.mu.Lock()
		s.cutReq = msg.ID
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "id: e1\nretry: 5\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progressToken\":\"t\",\"progress\":1}}\n\n")
		rc := http.NewResponseController(w)
		_ = rc.Flush()
		if conn, _, err := rc.Hijack(); err == nil {
			conn.Close()
		}
		return
	}
	if msg.Method == "tools/call" && strings.Contains(string(msg.Params), `"name":"huge"`) {
		// An identified event, then one past any sane cap.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "id: e1\nretry: 5\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{}}\n\nevent: message\ndata: %s\n\n", strings.Repeat("x", 4096))
		return
	}
	if msg.Method == "tools/call" && strings.Contains(string(msg.Params), `"name":"primed"`) {
		// A resumable stream opens with a priming event: an id and an empty data
		// line, then the real messages.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		res, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": textResult("primed ok")})
		fmt.Fprintf(w, "id: p0\nretry: 5\ndata: \n\nid: p1\nevent: message\ndata: %s\n\n", res)
		return
	}
	if msg.Method == "tools/call" && strings.Contains(string(msg.Params), `"name":"cut"`) {
		// A stream that dies after one identified event, before its response: the
		// client must resume it with Last-Event-ID.
		s.mu.Lock()
		s.cutReq = msg.ID
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "id: e1\nretry: 5\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progressToken\":\"t\",\"progress\":1}}\n\n")
		w.(http.Flusher).Flush()
		return
	}
	x := &httpExchange{s: s, w: w}
	result, rpcErr := s.brain.handle(msg.Method, msg.Params, headers, x.ask)
	reply := map[string]any{"jsonrpc": "2.0", "id": msg.ID}
	if rpcErr != nil {
		reply["error"] = rpcErr
	} else {
		reply["result"] = result
	}
	if msg.Method == "initialize" {
		w.Header().Set("Mcp-Session-Id", "sess-1")
	}
	if x.sse {
		x.writeEvent(reply)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if rpcErr != nil && s.brain.modern {
		// The modern wire maps error codes to HTTP statuses
		// (mcp.shared.inbound.ERROR_CODE_HTTP_STATUS).
		switch rpcErr.Code {
		case codeInvalidParams, codeInvalidRequest, codeHeaderMismatch, codeUnsupportedProtocolVersion:
			w.WriteHeader(http.StatusBadRequest)
		case codeMethodNotFound:
			w.WriteHeader(http.StatusNotFound)
		}
	}
	_ = json.NewEncoder(w).Encode(reply)
}

// --- shared test plumbing --------------------------------------------------------

// scenarioKind names a transport a scenario runs over.
type scenarioKind string

const (
	overHTTP  scenarioKind = "http"
	overStdio scenarioKind = "stdio"
)

// newScenarioClient builds a Client over the given transport against the
// scenario server. configure may adjust the config of either transport through
// the callbacks; the HTTP server handle is returned for log assertions (nil for
// stdio, whose server is another process).
func newScenarioClient(t *testing.T, kind scenarioKind, modern bool, mode ProtocolMode, timeout time.Duration, opts ...Option) (*Client, *scenarioHTTP) {
	t.Helper()
	var (
		cl  *Client
		srv *scenarioHTTP
		err error
	)
	switch kind {
	case overHTTP:
		srv = newScenarioHTTP(modern)
		hs := httptest.NewServer(srv)
		t.Cleanup(hs.Close)
		cl, err = New(&MCPServerConfig{URL: hs.URL, ProtocolMode: mode, Timeout: timeout}, opts...)
	case overStdio:
		cfg := stdioTestConfig(t, func(c *MCPStdioConfig) {
			c.ProtocolMode = mode
			c.Timeout = timeout
			if modern {
				c.Env = append(c.Env, stdioModernEnv+"=1")
			}
		})
		cl, err = New(cfg, opts...)
	}
	if err != nil {
		t.Fatalf("New(%s): %v", kind, err)
	}
	t.Cleanup(func() { cl.Close() })
	return cl, srv
}

var bothKinds = []scenarioKind{overHTTP, overStdio}

func firstText(t *testing.T, out ToolOutcome) string {
	t.Helper()
	if len(out.Parts) == 0 {
		t.Fatal("tool result has no parts")
	}
	return textContent(t, out.Parts[0])
}

func containsAll(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("result %q does not contain %q", got, w)
		}
	}
}
