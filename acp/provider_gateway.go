package acp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/hitl"
	"github.com/luckystrker/ago/internal/uuid"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
)

// The provider's tool host: an in-process streamable-HTTP MCP server that
// serves the run's function tools to the external agent (Python parity:
// tool_gateway.py ToolGateway — the HOST direction, the opposite of
// ToolGateway in tool_gateway.go which proxies servers a client declared).
// tools/call is executed through the run's event stream — a ToolCallEvent is
// sent and the matching result awaited — exactly as the agent's own executor
// does, so tool middleware and observers apply.

// gatewayServerName is the name the host is announced under in session/new;
// agents namespace the tools it serves by it (mcp__ag2__<tool>).
const gatewayServerName = "ag2"

// gatewayHumanInputError is what the external agent reads when a gateway tool
// had to ask a person and could not reach one.
const gatewayHumanInputError = "This tool had to ask a person, and there is no way to reach one. " +
	"The turn is ending; this is not a tool that failed, so do not look for another way to do the same thing."

// maxGatewayBody caps one JSON-RPC body the host accepts.
const maxGatewayBody = 8 << 20

// ErrMCPCapability reports tools that cannot be exposed to the agent over
// HTTP MCP: the agent does not advertise mcpCapabilities.http, so it could
// never reach the host (Python parity: MCPCapabilityError).
var ErrMCPCapability = errors.New("acp: agent cannot use HTTP MCP servers")

// partitionTools splits one call's tool schemas into function tools the host
// serves itself and external MCP servers handed to the agent directly
// (Python parity: partition_tools). Provider-hosted tools execute only inside
// that provider's API call, so they are refused; an MCP server with a tool
// filter is refused too, because ACP has no per-server filter and dropping it
// would widen access.
func partitionTools(opts []any) (functions []tools.Schema, external []MCPServer, err error) {
	for _, t := range opts {
		switch s := t.(type) {
		case tools.Schema:
			functions = append(functions, s)
		case *tools.Schema:
			if s == nil {
				return nil, nil, fmt.Errorf("%w: nil function schema", config.ErrUnsupportedTool)
			}
			functions = append(functions, *s)
		case tools.MCPServerToolSchema:
			srv, err := externalServer(s)
			if err != nil {
				return nil, nil, err
			}
			external = append(external, srv)
		case *tools.MCPServerToolSchema:
			if s == nil {
				return nil, nil, fmt.Errorf("%w: nil MCP server schema", config.ErrUnsupportedTool)
			}
			srv, err := externalServer(*s)
			if err != nil {
				return nil, nil, err
			}
			external = append(external, srv)
		case tools.ToolSchema:
			return nil, nil, fmt.Errorf("%w: hosted tool %q has nothing to execute locally; "+
				"ACP agents ship their own native equivalents", config.ErrUnsupportedTool, s.SchemaType())
		default:
			return nil, nil, fmt.Errorf("%w: unsupported schema type %T", config.ErrUnsupportedTool, t)
		}
	}
	return functions, external, nil
}

// externalServer translates an MCPServerTool schema into an ACP MCP server
// entry. The bearer token becomes an Authorization header unless the headers
// already carry one (Python parity: http_headers).
func externalServer(s tools.MCPServerToolSchema) (MCPServer, error) {
	if len(s.AllowedTools) > 0 || len(s.BlockedTools) > 0 {
		return MCPServer{}, fmt.Errorf("%w: MCP server %q has allowed/blocked tools, which ACP cannot enforce; "+
			"remove the filter or connect the server as an MCP toolkit so AGo executes its tools",
			config.ErrUnsupportedTool, s.ServerLabel)
	}
	headers := make(map[string]string, len(s.Headers)+1)
	hasAuth := false
	for k, v := range s.Headers {
		headers[k] = v
		if strings.EqualFold(k, "authorization") {
			hasAuth = true
		}
	}
	if s.AuthorizationToken != "" && !hasAuth {
		headers["Authorization"] = "Bearer " + s.AuthorizationToken
	}
	if len(headers) == 0 {
		headers = nil
	}
	return MCPServer{Name: s.ServerLabel, URL: s.ServerURL, Headers: headers}, nil
}

// toolHost is the HTTP MCP server. tools is read live on every request, so
// the owner may replace it between turns; the session/new declaration that
// points at the host is fixed for the session's lifetime.
type toolHost struct {
	name string
	path string // secret path component: the host admits no other URL
	// onChannelFailure is told when a tool needed a human and none was
	// reachable; the turn must then end instead of reporting success.
	onChannelFailure func(error)
	// shutdownGrace bounds how long close waits for in-flight requests before
	// cutting them off; 0 means 5s.
	shutdownGrace time.Duration

	mu      sync.Mutex
	schemas []tools.Schema
	conv    *agocontext.Context
	turnCtx context.Context

	ln  net.Listener
	srv *http.Server
	url string
}

func newToolHost(schemas []tools.Schema, onChannelFailure func(error)) (*toolHost, error) {
	var token [24]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, fmt.Errorf("acp: tool host token: %w", err)
	}
	return &toolHost{
		name:             gatewayServerName,
		path:             "/mcp/" + hex.EncodeToString(token[:]),
		schemas:          append([]tools.Schema(nil), schemas...),
		onChannelFailure: onChannelFailure,
	}, nil
}

// start binds loopback with an OS-assigned port and serves; the host is
// reachable only from this machine, which is all a locally launched agent
// needs.
func (h *toolHost) start() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("acp: tool host listen: %w", err)
	}
	h.ln = ln
	h.url = "http://" + ln.Addr().String() + h.path
	h.srv = &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = h.srv.Serve(ln) }()
	return nil
}

// declaration is the session/new entry announcing the host.
func (h *toolHost) declaration() MCPServer { return MCPServer{Name: h.name, URL: h.url} }

// setTools replaces the served tool list (the next tools/list sees it).
func (h *toolHost) setTools(schemas []tools.Schema) {
	h.mu.Lock()
	h.schemas = append([]tools.Schema(nil), schemas...)
	h.mu.Unlock()
}

// setTurn points the host at the live conversation: tool calls arriving while
// the turn runs execute in it.
func (h *toolHost) setTurn(ctx context.Context, conv *agocontext.Context) {
	h.mu.Lock()
	h.turnCtx, h.conv = ctx, conv
	h.mu.Unlock()
}

// close stops the HTTP server, bounded; idempotent.
func (h *toolHost) close() {
	if h.srv == nil {
		return
	}
	grace := h.shutdownGrace
	if grace <= 0 {
		grace = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := h.srv.Shutdown(ctx); err != nil {
		// A request that will not finish must not outlive the session.
		_ = h.srv.Close()
	}
	h.srv = nil
}

// --- HTTP / JSON-RPC ---------------------------------------------------------

type gwRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type gwResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// loopbackHost reports whether a Host header names this machine's loopback:
// the DNS-rebinding fence (a page on another origin can reach the port, but
// only under its own Host).
func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	switch strings.ToLower(strings.Trim(host, "[]")) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// ServeHTTP admits only POSTs to the secret path from a loopback Host.
func (h *toolHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != h.path || !loopbackHost(r.Host) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if o := r.Header.Get("Origin"); o != "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxGatewayBody))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var req gwRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Method == "" {
		writeGW(w, gwResponse{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: rpcErr(codeInvalidRequest, "not a JSON-RPC request")})
		return
	}
	if len(req.ID) == 0 { // a notification: accepted, never answered
		w.WriteHeader(http.StatusAccepted)
		return
	}
	resp := gwResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = h.initializeResult(req.Params)
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = h.listTools()
	case "tools/call":
		resp.Result, resp.Error = h.callTool(r.Context(), req.Params)
	default:
		resp.Error = rpcErr(codeMethodNotFound, "method %q is not supported", req.Method)
	}
	writeGW(w, resp)
}

func writeGW(w http.ResponseWriter, resp gwResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, "encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// mcpProtocolVersion is offered when the client names none.
const mcpProtocolVersion = "2025-03-26"

func (h *toolHost) initializeResult(params json.RawMessage) map[string]any {
	version := mcpProtocolVersion
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(params, &p) == nil && p.ProtocolVersion != "" {
		version = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": h.name, "version": clientVersion},
	}
}

func (h *toolHost) listTools() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]map[string]any, 0, len(h.schemas))
	for _, s := range h.schemas {
		input := s.Parameters
		if len(input) == 0 {
			input = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		t := map[string]any{"name": s.Name, "inputSchema": input}
		if s.Description != "" {
			t["description"] = s.Description
		}
		out = append(out, t)
	}
	return map[string]any{"tools": out}
}

type gwBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

func textBlockGW(s string) gwBlock { return gwBlock{Type: "text", Text: s} }

func toolResult(blocks []gwBlock, isError bool) map[string]any {
	if blocks == nil {
		blocks = []gwBlock{}
	}
	return map[string]any{"content": blocks, "isError": isError}
}

func (h *toolHost) callTool(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Name == "" {
		return nil, rpcErr(codeInvalidParams, "tools/call params: need a tool name")
	}
	h.mu.Lock()
	known := false
	for _, s := range h.schemas {
		if s.Name == p.Name {
			known = true
		}
	}
	h.mu.Unlock()
	if !known {
		return toolResult([]gwBlock{textBlockGW(fmt.Sprintf("unknown tool %q", p.Name))}, true), nil
	}
	blocks, isErr := h.execute(ctx, p.Name, p.Arguments)
	return toolResult(blocks, isErr), nil
}

// --- execution through the run's stream --------------------------------------

// settleCondition matches the events that settle one call: its result, its
// error, or the client hand-off.
func settleCondition(callID string) events.Condition {
	return func(ev events.Event) bool {
		switch e := events.AsValue(ev).(type) {
		case events.ToolResultEvent:
			return e.ParentID == callID
		case events.ToolErrorEvent:
			return e.ParentID == callID
		case events.ToolNotFoundEvent:
			return e.ParentID == callID
		case events.ClientToolCallEvent:
			return e.ID == callID
		}
		return false
	}
}

// execute runs one tool call in the live conversation and renders the outcome
// as MCP content. A tool that needed a human who could not be reached ends
// the turn (onChannelFailure) rather than reporting an ordinary tool failure.
func (h *toolHost) execute(reqCtx context.Context, name string, args map[string]any) ([]gwBlock, bool) {
	h.mu.Lock()
	conv, turnCtx := h.conv, h.turnCtx
	h.mu.Unlock()
	if conv == nil {
		return []gwBlock{textBlockGW("no active run to execute the tool in")}, true
	}
	if args == nil {
		args = map[string]any{}
	}
	argJSON, err := json.Marshal(args)
	if err != nil {
		return []gwBlock{textBlockGW("tool arguments: " + err.Error())}, true
	}
	call := events.ToolCallEvent{ID: uuid.New(), Name: name, Arguments: string(argJSON)}

	// Subscribe before sending: the tool's own subscriber may answer during
	// the dispatch of the call itself.
	ch := make(chan events.Event, 4)
	sub := conv.Stream.Subscribe(func(ev events.Event) error {
		ch <- ev
		return nil
	}, stream.SubscribeOptions{Condition: settleCondition(call.ID)})
	defer conv.Stream.Unsubscribe(sub)

	if err := conv.Send(call); err != nil {
		if hitl.IsError(err) {
			// Close the call off so the transcript stays replayable, then end
			// the turn.
			_ = conv.Send(events.ToolResultEvent{ParentID: call.ID, Name: name,
				Result: events.ToolResult{Parts: []events.Input{events.TextInput{Content: events.SentinelToolResult}}}})
			if h.onChannelFailure != nil {
				h.onChannelFailure(err)
			}
			return []gwBlock{textBlockGW(gatewayHumanInputError)}, true
		}
		return []gwBlock{textBlockGW(err.Error())}, true
	}

	var ev events.Event
	select {
	case ev = <-ch:
	case <-reqCtx.Done():
		return []gwBlock{textBlockGW("request cancelled")}, true
	case <-turnCtx.Done():
		return []gwBlock{textBlockGW("the turn ended before the tool finished")}, true
	}

	switch e := events.AsValue(ev).(type) {
	case events.ToolErrorEvent:
		return []gwBlock{textBlockGW(e.Error)}, true
	case events.ToolNotFoundEvent:
		return []gwBlock{textBlockGW(e.Error)}, true
	case events.ClientToolCallEvent:
		return []gwBlock{textBlockGW(fmt.Sprintf(
			"tool %q requires client-side execution and cannot be executed through the ACP tool gateway", name))}, true
	case events.ToolResultEvent:
		// A final=true result reaches the agent as ordinary tool output:
		// final-response semantics cannot be enforced over ACP.
		return resultBlocks(e.Result), false
	}
	return []gwBlock{textBlockGW("unexpected tool outcome")}, true
}

// resultBlocks renders a tool result's parts as MCP content blocks (Python
// parity: ToolGateway._execute's part mapping).
func resultBlocks(r events.ToolResult) []gwBlock {
	var blocks []gwBlock
	for _, part := range r.Parts {
		switch p := events.AsValue(part).(type) {
		case events.TextInput:
			blocks = append(blocks, textBlockGW(p.Content))
		case events.DataInput:
			b, err := json.Marshal(p.Data)
			if err != nil {
				b = []byte(fmt.Sprint(p.Data))
			}
			blocks = append(blocks, textBlockGW(string(b)))
		case events.BinaryInput:
			if strings.HasPrefix(p.MediaType, "image/") {
				blocks = append(blocks, gwBlock{Type: "image", Data: base64.StdEncoding.EncodeToString(p.Data), MimeType: p.MediaType})
			} else {
				blocks = append(blocks, textBlockGW(fmt.Sprintf("<binary %s, %d bytes>", p.MediaType, len(p.Data))))
			}
		case events.UrlInput:
			blocks = append(blocks, textBlockGW(p.URL))
		case events.FileIdInput:
			blocks = append(blocks, textBlockGW("<file "+p.FileID+">"))
		default:
			blocks = append(blocks, textBlockGW(fmt.Sprint(part)))
		}
	}
	return blocks
}
