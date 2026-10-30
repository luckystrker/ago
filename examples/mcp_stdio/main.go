// Command mcp_stdio demonstrates tools of an external MCP server becoming
// ordinary agent tools (parity with ag2/examples/mcp/client_demo.py driving
// ag2/examples/mcp/server_stdio.py). The example launches a stub MCP server as
// a stdio subprocess — newline-delimited JSON-RPC 2.0 on stdin/stdout, the wire
// dialect ago/mcp speaks — then hands its calc_add tool to the agent through
// mcp.New + mcp.Tools (the AGo equivalent of Python's MCPToolkit: discover the
// server's tools, wrap each one as a tools.Tool the model can call by name).
// The scripted model asks for the tool in round 1 and answers with the number
// the subprocess computed in round 2, so the whole story runs offline: no API
// keys, no network, and the arithmetic happens in the child process.
//
// Run: go run ./examples/mcp_stdio
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/mcp"
	agotest "github.com/luckystrker/ago/testing"
)

// serverModeArg marks the child process. The parent re-executes its own binary
// with this argument to launch the server (go run builds a temporary binary, so
// os.Executable always points at something runnable — the Go counterpart of the
// original's `python -m examples.mcp.server_stdio`).
const serverModeArg = "-mcp-server"

func main() {
	if len(os.Args) > 1 && os.Args[1] == serverModeArg {
		runStubServer(os.Stdin, os.Stdout)
		return
	}
	runClient()
}

// runClient is the MCP client side: launch the server, adopt its tools, ask.
func runClient() {
	exe, err := os.Executable()
	if err != nil {
		fmt.Println("resolve executable:", err)
		return
	}

	// The subprocess: this binary again, in server mode. Env is left nil, so the
	// child inherits this process's environment (os/exec semantics) — the
	// equivalent of client_demo.py's env=dict(os.environ).
	serverCfg := &mcp.MCPStdioConfig{
		Command: exe,
		Args:    []string{serverModeArg},
	}

	// mcp.New validates the config; the session (handshake + tool discovery)
	// opens lazily on the first operation. Initialize here to show who answered.
	cl, err := mcp.New(serverCfg)
	if err != nil {
		fmt.Println("mcp client:", err)
		return
	}
	defer cl.Close()

	info, err := cl.Initialize()
	if err != nil {
		fmt.Println("initialize:", err)
		return
	}
	fmt.Printf("server: %s v%s (protocol %s, stdio subprocess)\n", info.Name, info.Version, info.ProtocolVersion)

	// mcp.Tools is the MCPToolkit step: list the server's tools and wrap each as
	// an agent tool (DeclaredInCode()==false — code-declared tools keep name
	// precedence over remote ones).
	remote, err := mcp.Tools(cl)
	if err != nil {
		fmt.Println("list tools:", err)
		return
	}
	fmt.Printf("tools: %d discovered\n", len(remote))
	for _, t := range remote {
		fmt.Printf("  - %s\n", t.Name())
	}

	// Scripted model: round 1 calls the remote tool by name, round 2 answers
	// with the number the subprocess returned. Usage numbers are invented,
	// exactly like a real provider would report them.
	modelCfg := agotest.NewTestConfig(
		&events.ModelResponse{
			ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
				{ID: "call-1", Name: "calc_add", Arguments: `{"a":17,"b":25}`},
			}},
			Usage: events.Usage{PromptTokens: events.I64(35), CompletionTokens: events.I64(10), TotalTokens: events.I64(45)},
		},
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: "42"},
			Usage:   events.Usage{PromptTokens: events.I64(75), CompletionTokens: events.I64(2), TotalTokens: events.I64(77)},
		},
	)

	a := agent.New("client", "You are a concise assistant. Use tools when they help.",
		agent.WithConfig(modelCfg), agent.WithTools(remote...))

	reply, err := a.Ask(context.Background(), "Use the calc_add tool to add 17 and 25. Reply with just the number.")
	if err != nil {
		fmt.Println("ask failed:", err)
		return
	}

	fmt.Println("\nassistant:", reply.Body())

	fmt.Println("\n== transcript (question, remote tool round trip, answer) ==")
	evs, err := reply.History().GetEvents()
	if err != nil {
		fmt.Println("history failed:", err)
		return
	}
	printTranscript(evs)
}

// runStubServer serves one MCP session on stdin/stdout: one JSON-RPC request per
// line, one reply per line. It advertises calc_add and actually adds the
// arguments — the arithmetic happens in the subprocess, not in the client.
func runStubServer(in io.Reader, out io.Writer) {
	w := bufio.NewWriter(out)
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		switch req.Method {
		case "initialize":
			reply(w, map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"protocolVersion": "2024-11-05",
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]any{"name": "calc-stdio", "version": "1.0"},
				},
			})
		case "notifications/initialized":
			// A notification: no id, no reply expected.
		case "tools/list":
			reply(w, map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"tools": []map[string]any{{
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
				}}},
			})
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if p.Name != "calc_add" {
				reply(w, rpcError(req.ID, -32602, "unknown tool"))
				continue
			}
			// JSON numbers decode as float64; the operands are integers by schema.
			av, _ := p.Arguments["a"].(float64)
			bv, _ := p.Arguments["b"].(float64)
			reply(w, map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": fmt.Sprintf("%d", int64(av+bv))}},
					"isError": false,
				},
			})
		default:
			reply(w, rpcError(req.ID, -32601, "method not found"))
		}
	}
}

// reply writes one JSON-RPC message and flushes: the client reads line by line
// and would hang on a reply still sitting in the buffer.
func reply(w *bufio.Writer, msg map[string]any) {
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	fmt.Fprintln(w, string(b))
	w.Flush()
}

func rpcError(id *int64, code int, msg string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": msg},
	}
}

// printTranscript renders the event kinds that make up the round trip: the
// model's tool call goes to the MCP wrapper, the wrapper's result event comes
// back from the subprocess, and the model answers.
func printTranscript(evs []events.Event) {
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.ModelRequest:
			fmt.Printf("[user]      %s\n", events.RenderForPrompt(e))
		case events.ModelResponse:
			if e.Content() != "" {
				fmt.Printf("[assistant] %s\n", e.Content())
			}
		case events.ToolCallsEvent:
			for _, c := range e.Calls {
				fmt.Printf("[model]     tool call %s(%s)\n", c.Name, c.Arguments)
			}
		case events.ToolResultsEvent:
			for _, r := range e.Results {
				fmt.Printf("[tool/mcp]  %s -> %s\n", r.Name, renderParts(r.Result))
			}
		}
	}
}

func renderParts(r events.ToolResult) string {
	out := ""
	for i, p := range r.Parts {
		if i > 0 {
			out += "; "
		}
		out += events.RenderForPrompt(p)
	}
	return out
}
