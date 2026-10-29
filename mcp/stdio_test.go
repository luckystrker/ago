package mcp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agocontext "ago/context"
	"ago/events"
	"ago/stream"
	"ago/tools"
)

// The stdio tests re-exec this test binary as a stub MCP server: when the env
// flag is set, TestMain serves newline-delimited JSON-RPC on stdin/stdout and
// never runs the tests (the "TestMain process helper" pattern).
const stdioServerEnv = "AGO_MCP_TEST_STDIO_SERVER"
const stdioTokenEnv = "MCP_TEST_TOKEN"

func TestMain(m *testing.M) {
	if os.Getenv(stdioServerEnv) == "1" {
		runStdioServer(os.Stdin, os.Stdout)
		return
	}
	os.Exit(m.Run())
}

// --- the stub server (child process) -------------------------------------------

type stubRequest struct {
	ID     *int64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func runStdioServer(in io.Reader, out io.Writer) {
	w := bufio.NewWriter(out)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), stdioScanBuffer)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req stubRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		switch req.Method {
		case "initialize":
			// An unsolicited notification before the reply: the client must
			// skip it and still match the response by id.
			writeStub(w, map[string]any{"jsonrpc": "2.0", "method": "notifications/stub-hello", "params": map[string]any{}})
			writeStub(w, map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"protocolVersion": "2024-11-05",
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]any{"name": "stdio-stub", "version": "2.0"},
				},
			})
		case "notifications/initialized":
			// Nothing to confirm.
		case "tools/list":
			stubListTools(w, req)
		case "tools/call":
			stubCallTool(w, req)
		default:
			writeStub(w, map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": -32601, "message": "method not found"},
			})
		}
	}
}

func stubListTools(w io.Writer, req stubRequest) {
	var p struct {
		Cursor string `json:"cursor"`
	}
	_ = json.Unmarshal(req.Params, &p)
	if p.Cursor == "" {
		writeStub(w, map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]any{
				"tools": []map[string]any{
					{"name": "echo", "description": "Echo tool",
						"inputSchema": map[string]any{"type": "object",
							"properties": map[string]any{"msg": map[string]any{"type": "string"}}}},
					{"name": "fail", "description": "Always fails",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
				},
				"nextCursor": "m2",
			},
		})
		return
	}
	writeStub(w, map[string]any{
		"jsonrpc": "2.0", "id": req.ID,
		"result": map[string]any{
			"tools": []map[string]any{
				{"name": "probe", "description": "Environment probe",
					"inputSchema": map[string]any{"type": "object",
						"properties": map[string]any{"kind": map[string]any{"type": "string"}}}},
				{"name": "bare", "description": "No schema"},
			},
		},
	})
}

func stubCallTool(w io.Writer, req stubRequest) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	_ = json.Unmarshal(req.Params, &p)
	switch p.Name {
	case "echo":
		msg, _ := p.Arguments["msg"].(string)
		writeStub(w, stubTextResult(req.ID, "echo: "+msg, false))
	case "probe":
		kind, _ := p.Arguments["kind"].(string)
		text := ""
		switch kind {
		case "env":
			text = os.Getenv(stdioTokenEnv)
		case "cwd":
			text, _ = os.Getwd()
		}
		writeStub(w, stubTextResult(req.ID, text, false))
	case "fail":
		writeStub(w, stubTextResult(req.ID, "stub failure", true))
	default:
		writeStub(w, map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"error": map[string]any{"code": -32602, "message": "unknown tool"},
		})
	}
}

func stubTextResult(id *int64, text string, isErr bool) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0", "id": id,
		"result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
			"isError": isErr,
		},
	}
}

func writeStub(w io.Writer, msg any) {
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	fmt.Fprintln(w, string(b))
	if f, ok := w.(*bufio.Writer); ok {
		f.Flush()
	}
}

// --- the client-side tests -------------------------------------------------------

func stdioTestConfig(t *testing.T, mutate func(*MCPStdioConfig)) *MCPStdioConfig {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cfg := &MCPStdioConfig{
		Command: exe,
		CWD:     t.TempDir(),
		Env:     append(os.Environ(), stdioServerEnv+"=1", stdioTokenEnv+"=s3cret-token"),
	}
	if mutate != nil {
		mutate(cfg)
	}
	return cfg
}

// textContent asserts the part is a TextInput and returns its content.
func textContent(t *testing.T, part events.Input) string {
	t.Helper()
	if ti, ok := events.AsValue(part).(events.TextInput); ok {
		return ti.Content
	}
	t.Fatalf("part %#v is not TextInput", part)
	return ""
}

func TestStdioRoundTrip(t *testing.T) {
	cfg := stdioTestConfig(t, nil)
	cl, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { cl.Close() })

	info, err := cl.Initialize()
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if info.Name != "stdio-stub" || info.Version != "2.0" {
		t.Errorf("server info = %+v", info)
	}

	got, err := cl.ListTools()
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	for _, tl := range got {
		names = append(names, tl.Name)
	}
	if fmt.Sprint(names) != fmt.Sprint([]string{"echo", "fail", "probe", "bare"}) {
		t.Errorf("tools = %v (pagination over m2)", names)
	}

	out, err := cl.CallTool("echo", map[string]any{"msg": "hi"})
	if err != nil {
		t.Fatalf("CallTool echo: %v", err)
	}
	if len(out.Parts) != 1 {
		t.Fatalf("parts = %d, want 1", len(out.Parts))
	}
	if got := textContent(t, out.Parts[0]); got != "echo: hi" {
		t.Errorf("echo text = %q, want %q", got, "echo: hi")
	}

	out, err = cl.CallTool("probe", map[string]any{"kind": "env"})
	if err != nil {
		t.Fatalf("CallTool probe/env: %v", err)
	}
	if got := textContent(t, out.Parts[0]); got != "s3cret-token" {
		t.Errorf("probe env = %q, want the value passed through MCPStdioConfig.Env", got)
	}

	out, err = cl.CallTool("probe", map[string]any{"kind": "cwd"})
	if err != nil {
		t.Fatalf("CallTool probe/cwd: %v", err)
	}
	if got := textContent(t, out.Parts[0]); !strings.EqualFold(filepath.Clean(got), filepath.Clean(cfg.CWD)) {
		t.Errorf("probe cwd = %q, want %q", got, cfg.CWD)
	}

	if _, err := cl.CallTool("fail", nil); !errors.Is(err, ErrToolCall) || !strings.Contains(err.Error(), "stub failure") {
		t.Errorf("CallTool fail = %v, want ErrToolCall naming stub failure", err)
	}

	if err := cl.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := cl.CallTool("echo", nil); !errors.Is(err, ErrClosed) {
		t.Errorf("CallTool after Close = %v, want ErrClosed", err)
	}
	if err := cl.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

func TestStdioToolsWrapper(t *testing.T) {
	cfg := stdioTestConfig(t, func(c *MCPStdioConfig) {
		c.ToolNamePrefix = "loc_"
		c.AllowedTools = []string{"echo"}
		c.BlockedTools = []string{"probe"}
	})
	cl, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { cl.Close() })

	got, err := Tools(cl)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("tools = %d, want 1 (echo allowed, probe blocked, fail/bare not allowed)", len(got))
	}
	if got[0].Name() != "loc_echo" {
		t.Errorf("name = %q, want loc_echo", got[0].Name())
	}
	if got[0].DeclaredInCode() {
		t.Error("remote tool must not be declared in code")
	}
	rt := got[0].(*remoteTool)
	if rt.remoteName != "echo" {
		t.Errorf("remote name = %q, want echo", rt.remoteName)
	}
	if !strings.HasPrefix(rt.source, "mcp:"+cfg.Command+"#") || !strings.HasSuffix(rt.source, ":echo") {
		t.Errorf("source = %q, want mcp:<command#argshash>:echo", rt.source)
	}
	schemas, err := got[0].Schemas(agocontext.New(stream.NewMemoryStream()))
	if err != nil || len(schemas) != 1 {
		t.Fatalf("Schemas = %v, %v", schemas, err)
	}
	schema := schemas[0].(tools.Schema)
	if schema.Name != "loc_echo" || schema.Description != "Echo tool" {
		t.Errorf("schema = %+v", schema)
	}
	if schema.Parameters["type"] != "object" {
		t.Errorf("parameters = %v", schema.Parameters)
	}

	// The registered wrapper serves a call end-to-end on a stream.
	ms := stream.NewMemoryStream()
	cx := agocontext.New(ms)
	if _, err := got[0].Register(cx, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	gotText := ""
	done := make(chan struct{})
	ms.Subscribe(func(ev events.Event) error {
		if res, ok := events.AsValue(ev).(events.ToolResultEvent); ok && res.ParentID == "c1" {
			gotText = textContent(t, res.Result.Parts[0])
			close(done)
		}
		return nil
	}, stream.SubscribeOptions{})
	if err := ms.Send(events.ToolCallEvent{ID: "c1", Name: "loc_echo", Arguments: `{"msg":"round"}`}); err != nil {
		t.Fatalf("send call: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("tool result never arrived")
	}
	if gotText != "echo: round" {
		t.Errorf("result text = %q, want %q", gotText, "echo: round")
	}
}

func TestStdioLazyInitialize(t *testing.T) {
	cfg := stdioTestConfig(t, nil)
	cl, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { cl.Close() })
	// No explicit Initialize: the first operation opens the session.
	out, err := cl.CallTool("echo", map[string]any{"msg": "lazy"})
	if err != nil {
		t.Fatalf("CallTool without Initialize: %v", err)
	}
	if textContent(t, out.Parts[0]) != "echo: lazy" {
		t.Errorf("text = %q", textContent(t, out.Parts[0]))
	}
}

func TestStdioBadCommand(t *testing.T) {
	cfg := stdioTestConfig(t, func(c *MCPStdioConfig) {
		c.Command = "ago-mcp-no-such-command"
		c.Env = nil
	})
	cl, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { cl.Close() })
	if _, err := cl.Initialize(); err == nil {
		t.Fatal("Initialize with a missing executable must fail")
	}
}
