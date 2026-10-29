// Command mcp_http demonstrates the MCP (Model Context Protocol) client over
// streamable HTTP: the tools of a remote MCP server become ordinary local
// tools of an agent — after discovery, nothing in the agent code knows they
// are remote. It ports ag2/examples/mcp/client_http.py, whose client agent
// reaches a server built with MCPServer + uvicorn
// (ag2/examples/mcp/server_http.py) exposing one tool, calc_add.
//
// To run offline, the remote server is replaced by a minimal MCP server on
// net/http embedded in this same process: JSON-RPC 2.0, one POST per message,
// an initialize handshake that assigns Mcp-Session-Id, tools/list, tools/call.
// The client below is the real ago/mcp client, pointed at 127.0.0.1 like any
// remote server — the only thing shared with it is the TCP port. The live LLM
// is replaced by a scripted test config (the examples/ask convention): round 1
// requests the remote tool, round 2 answers from its result — the same two
// rounds a live model would play.
//
// Run: go run ./examples/mcp_http
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"

	"ago/agent"
	"ago/events"
	"ago/mcp"
	agotest "ago/testing"
	"ago/tools"
)

func main() {
	// 1. The "remote" MCP server: a minimal streamable-HTTP implementation on
	// net/http, listening on 127.0.0.1 like a real one. In the Python original
	// this is a separate uvicorn process; embedding it keeps the example offline.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("listen:", err)
		return
	}
	defer ln.Close()
	go func() { _ = http.Serve(ln, newMCPServer()) }()
	endpoint := "http://" + ln.Addr().String() + "/mcp"
	fmt.Println("mcp server on", endpoint, "(in-process, reached over HTTP)")

	// 2. The real MCP client over streamable HTTP.
	cl, err := mcp.New(&mcp.MCPServerConfig{URL: endpoint, ServerLabel: "calc-server"})
	if err != nil {
		fmt.Println("mcp client:", err)
		return
	}
	defer cl.Close()

	info, err := cl.Initialize()
	if err != nil {
		fmt.Println("handshake failed:", err)
		return
	}
	fmt.Printf("handshake: server=%s v%s, protocol=%s\n", info.Name, info.Version, info.ProtocolVersion)

	// 3. Discovery: the server's tools come back wrapped as ordinary tools.Tool
	// values (DeclaredInCode()==false, so a code-declared tool would still win
	// the name). From here on the agent treats them exactly like local tools.
	remote, err := mcp.Tools(cl)
	if err != nil {
		fmt.Println("tool discovery failed:", err)
		return
	}
	fmt.Println("discovered remote tools (now ordinary local tools for the agent):")
	for _, t := range remote {
		desc := ""
		if ss, err := t.Schemas(nil); err == nil && len(ss) > 0 {
			if sc, ok := ss[0].(tools.Schema); ok {
				desc = sc.Description
			}
		}
		fmt.Printf("  - %s (%s)\n", t.Name(), desc)
	}

	// 4. The agent. The scripted config replaces the live LLM: the first
	// response requests the remote tool, the second turns the tool result into
	// the final answer.
	cfg := agotest.NewTestConfig(
		&events.ModelResponse{
			ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
				{ID: "call-1", Name: "calc_add", Arguments: `{"a":17,"b":25}`},
			}},
			Usage: events.Usage{PromptTokens: events.I64(24), CompletionTokens: events.I64(8), TotalTokens: events.I64(32)},
		},
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: "42"},
			Usage:   events.Usage{PromptTokens: events.I64(56), CompletionTokens: events.I64(2), TotalTokens: events.I64(58)},
		},
	)

	a := agent.New("client", "You are a concise assistant. Use tools when they help.",
		agent.WithConfig(cfg), agent.WithTools(remote...))

	// 5. The ask, verbatim from the original. The loose "ask tool" wording is
	// resolved to the server's calc_add by the model; the scripted first round
	// pins exactly that resolution.
	reply, err := a.Ask(context.Background(), "Use the ask tool to add 17 and 25. Reply with just the number.")
	if err != nil {
		fmt.Println("ask failed:", err)
		return
	}

	fmt.Println("\nfinal answer:", reply.Body())

	fmt.Println("\n== transcript (one ask: request, remote tool round, answer) ==")
	evs, err := reply.History().GetEvents()
	if err != nil {
		fmt.Println("history failed:", err)
		return
	}
	printTranscript(evs)
}

// printTranscript renders the event kinds that make up one MCP-backed ask.
func printTranscript(evs []events.Event) {
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.ModelRequest:
			fmt.Printf("[user]      %s\n", events.RenderForPrompt(e))
		case events.ToolCallsEvent:
			for _, c := range e.Calls {
				fmt.Printf("[model]     tool call %s(%s)\n", c.Name, c.Arguments)
			}
		case events.ToolResultsEvent:
			for _, r := range e.Results {
				out := ""
				for i, p := range r.Result.Parts {
					if i > 0 {
						out += "; "
					}
					out += events.RenderForPrompt(p)
				}
				fmt.Printf("[remote]    %s -> %s\n", r.Name, out)
			}
		case events.ModelResponse:
			if e.Content() != "" {
				fmt.Printf("[assistant] %s\n", e.Content())
			}
		}
	}
}

// --- the embedded MCP server -----------------------------------------------------
// A minimal stand-in for ag2/examples/mcp/server_http.py: one tool, calc_add,
// exposed over streamable HTTP. The protocol subset is exactly what the
// ago/mcp client speaks: initialize, notifications/initialized, tools/list,
// tools/call, plus DELETE (the spec's session-teardown hint).

// rpcRequest is the JSON-RPC 2.0 envelope of one POSTed message; a nil ID
// marks a notification.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// mcpServer holds the HTTP handler and the set of live session ids.
type mcpServer struct {
	mu       sync.Mutex
	sessions map[string]bool
	next     int
}

func newMCPServer() *mcpServer { return &mcpServer{sessions: map[string]bool{}} }

// calcAddTool is the advertisement tools/list returns for the one tool.
var calcAddTool = map[string]any{
	"name":        "calc_add",
	"description": "Add two integers.",
	"inputSchema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"a": map[string]any{"type": "integer", "description": "First addend"},
			"b": map[string]any{"type": "integer", "description": "Second addend"},
		},
		"required": []string{"a", "b"},
	},
}

func (s *mcpServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		// Session teardown: forget the id the request names.
		sid := r.Header.Get("Mcp-Session-Id")
		s.mu.Lock()
		delete(s.sessions, sid)
		s.mu.Unlock()
		fmt.Printf("[server]   session %s closed (DELETE)\n", sid)
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Every message after initialize must carry the session id the server
	// assigned; a 404 is the streamable-HTTP session-loss signal — the AGo
	// client answers it by re-initializing and retrying once.
	sid := r.Header.Get("Mcp-Session-Id")
	if req.Method != "initialize" && !s.knowsSession(sid) {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// A message without an id is a notification: 202 Accepted, no body.
	if req.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch req.Method {
	case "initialize":
		assigned := s.newSession()
		w.Header().Set("Mcp-Session-Id", assigned)
		fmt.Printf("[server]   initialize -> session %s assigned\n", assigned)
		s.writeResult(w, req.ID, map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "calc-mcp", "version": "1.0"},
		})
	case "tools/list":
		fmt.Println("[server]   tools/list -> calc_add")
		s.writeResult(w, req.ID, map[string]any{"tools": []map[string]any{calcAddTool}})
	case "tools/call":
		s.callTool(w, req)
	default:
		s.writeError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

// callTool runs the one server-side tool and answers with a text content block.
func (s *mcpServer) callTool(w http.ResponseWriter, req rpcRequest) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.writeError(w, req.ID, -32602, "bad params")
		return
	}
	arg := func(k string) (int, error) {
		v, ok := p.Arguments[k].(float64) // encoding/json: numbers arrive as float64
		if !ok {
			return 0, fmt.Errorf("calc_add: argument %q must be a number", k)
		}
		return int(v), nil
	}
	a, errA := arg("a")
	b, errB := arg("b")
	if errA != nil || errB != nil {
		if errA == nil {
			errA = errB
		}
		s.writeError(w, req.ID, -32602, errA.Error())
		return
	}
	sum := a + b
	fmt.Printf("[server]   tools/call calc_add(%d, %d) = %d  (computed here, returned over HTTP)\n", a, b, sum)
	s.writeResult(w, req.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": fmt.Sprintf("%d", sum)}},
		"isError": false,
	})
}

func (s *mcpServer) newSession() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	id := fmt.Sprintf("sess-%d", s.next)
	s.sessions[id] = true
	return id
}

func (s *mcpServer) knowsSession(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

func (s *mcpServer) writeResult(w http.ResponseWriter, id *int64, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *mcpServer) writeError(w http.ResponseWriter, id *int64, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": msg},
	})
}
