package acp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- child process roles ----------------------------------------------------------
//
// Each role is the serve half of one mechanism under test, re-exec'd from the
// test binary (the TestMain process helper pattern). Chunks are how a role
// reports what it saw back to the test.

// childAsk raises one agent→client request and returns its result; an error
// answer becomes {"error": code}.
func childAsk(w *bufio.Writer, sc *bufio.Scanner, id int64, method string, params any) map[string]any {
	payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	fmt.Fprintln(w, string(payload))
	w.Flush()
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var msg incoming
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if msg.Method != "" || msg.ID == nil || *msg.ID != id {
			continue
		}
		if msg.Error != nil {
			return map[string]any{"error": msg.Error.Code}
		}
		var result map[string]any
		if err := json.Unmarshal(msg.Result, &result); err != nil {
			return map[string]any{}
		}
		return result
	}
	return map[string]any{}
}

// runCancelAgent streams one chunk on session/prompt, then parks reading the
// wire until a session/cancel notification arrives, and answers with the
// cancelled stop reason (Python parity: agent.py prompt's CancelledError
// branch — the turn stops, already-streamed events stay in the history).
func runCancelAgent(in io.Reader, out io.Writer) {
	w := bufio.NewWriter(out)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), scanBuffer)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req childRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil || req.ID == nil {
			continue
		}
		switch req.Method {
		case "initialize":
			childReply(w, *req.ID, map[string]any{
				"protocolVersion":   protocolVersion,
				"agentCapabilities": map[string]any{},
				"agentInfo":         map[string]any{"name": "cancel-agent", "version": "1.0"},
			})
		case "session/new":
			childReply(w, *req.ID, map[string]any{"sessionId": "sess-1"})
		case "session/prompt":
			childNotify(w, "session/update", updateParams{SessionID: "sess-1", Update: agentMessageChunk("wor")})
			// Park until the cancel notification; any request seen meanwhile
			// is answered method-not-found so the client never hangs.
			cancelled := false
			for sc.Scan() {
				cline := strings.TrimSpace(sc.Text())
				if cline == "" {
					continue
				}
				var cmsg incoming
				if err := json.Unmarshal([]byte(cline), &cmsg); err != nil {
					continue
				}
				if cmsg.Method == "session/cancel" && cmsg.ID == nil {
					cancelled = true
					break
				}
				if cmsg.ID != nil {
					childError(w, *cmsg.ID, codeMethodNotFound, "not implemented")
				}
			}
			if cancelled {
				childReply(w, *req.ID, map[string]any{"stopReason": StopCancelled})
			} else {
				childReply(w, *req.ID, map[string]any{"stopReason": StopEndTurn})
			}
		default:
			childError(w, *req.ID, codeMethodNotFound, "method not implemented")
		}
	}
}

// runLoadAgent answers session/load by replaying one user turn and one agent
// turn before responding (Python parity: the load_session replay contract —
// the response is the line between history and whatever comes next).
func runLoadAgent(in io.Reader, out io.Writer) {
	w := bufio.NewWriter(out)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), scanBuffer)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req childRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil || req.ID == nil {
			continue
		}
		switch req.Method {
		case "initialize":
			childReply(w, *req.ID, map[string]any{
				"protocolVersion":   protocolVersion,
				"agentCapabilities": map[string]any{"loadSession": true},
				"agentInfo":         map[string]any{"name": "load-agent", "version": "1.0"},
			})
		case "session/new":
			childReply(w, *req.ID, map[string]any{"sessionId": "sess-1"})
		case "session/load":
			// The replay: history before the response, in live order.
			childNotify(w, "session/update", updateParams{SessionID: "sess-1",
				Update: Update{Kind: "user_message_chunk", Text: "u1"}})
			childNotify(w, "session/update", updateParams{SessionID: "sess-1",
				Update: Update{Kind: "agent_message_chunk", Text: "a1"}})
			childReply(w, *req.ID, map[string]any{})
		case "session/prompt":
			childReply(w, *req.ID, map[string]any{"stopReason": StopEndTurn})
		default:
			childError(w, *req.ID, codeMethodNotFound, "method not implemented")
		}
	}
}

// runFSProbeAgent probes the mediated filesystem: reads inside the root, an
// escape attempt, a write, and a read back — reporting each outcome as a
// chunk. The prompt names the root the test prepared.
func runFSProbeAgent(in io.Reader, out io.Writer) {
	w := bufio.NewWriter(out)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), scanBuffer)
	asked := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req childRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil || req.ID == nil {
			continue
		}
		switch req.Method {
		case "initialize":
			childReply(w, *req.ID, map[string]any{
				"protocolVersion":   protocolVersion,
				"agentCapabilities": map[string]any{},
				"agentInfo":         map[string]any{"name": "fs-probe", "version": "1.0"},
			})
		case "session/new":
			childReply(w, *req.ID, map[string]any{"sessionId": "sess-1"})
		case "session/prompt":
			if asked {
				childReply(w, *req.ID, map[string]any{"stopReason": StopEndTurn})
				continue
			}
			asked = true
			var p struct {
				Prompt []contentBlock `json:"prompt"`
			}
			_ = json.Unmarshal(req.Params, &p)
			root := blocksText(p.Prompt)

			inside := filepath.Join(root, "ok.txt")
			_ = os.WriteFile(inside, []byte("alpha\nbeta\ngamma\n"), 0o644)
			got := childAsk(w, sc, 7001, "fs/read_text_file", map[string]any{
				"sessionId": "sess-1", "path": inside, "line": 2, "limit": 1,
			})
			if content, ok := got["content"]; ok {
				childNotify(w, "session/update", updateParams{SessionID: "sess-1",
					Update: agentMessageChunk("read:" + fmt.Sprint(content))})
			} else {
				childNotify(w, "session/update", updateParams{SessionID: "sess-1",
					Update: agentMessageChunk(fmt.Sprintf("read-err:%v", got["error"]))})
			}

			// The escape attempt: an error answer reads as {"error": code}.
			got = childAsk(w, sc, 7002, "fs/read_text_file", map[string]any{
				"sessionId": "sess-1", "path": filepath.Join(root, "..", "outside.txt"),
			})
			if code, ok := got["error"]; ok {
				childNotify(w, "session/update", updateParams{SessionID: "sess-1",
					Update: agentMessageChunk(fmt.Sprintf("escaped:%v", code))})
			} else {
				childNotify(w, "session/update", updateParams{SessionID: "sess-1",
					Update: agentMessageChunk("NOT-REFUSED")})
			}

			// Write through a not-yet-existing subdirectory, then read back.
			target := filepath.Join(root, "nested", "written.txt")
			wres := childAsk(w, sc, 7003, "fs/write_text_file", map[string]any{
				"sessionId": "sess-1", "path": target, "content": "written-by-agent",
			})
			if _, failed := wres["error"]; !failed {
				got = childAsk(w, sc, 7004, "fs/read_text_file", map[string]any{
					"sessionId": "sess-1", "path": target,
				})
				childNotify(w, "session/update", updateParams{SessionID: "sess-1",
					Update: agentMessageChunk("back:" + fmt.Sprint(got["content"]))})
			} else {
				childNotify(w, "session/update", updateParams{SessionID: "sess-1",
					Update: agentMessageChunk(fmt.Sprintf("write-err:%v", wres["error"]))})
			}
			childReply(w, *req.ID, map[string]any{"stopReason": StopEndTurn})
		default:
			childError(w, *req.ID, codeMethodNotFound, "method not implemented")
		}
	}
}

// runTermProbeAgent drives the mediated terminals: create a command that
// prints and exits, read its output, wait for the exit status, then kill and
// release bookkeeping. The prompt names the test binary to run and the env
// role to invoke it with.
func runTermProbeAgent(in io.Reader, out io.Writer) {
	w := bufio.NewWriter(out)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), scanBuffer)
	asked := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req childRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil || req.ID == nil {
			continue
		}
		switch req.Method {
		case "initialize":
			childReply(w, *req.ID, map[string]any{
				"protocolVersion":   protocolVersion,
				"agentCapabilities": map[string]any{},
				"agentInfo":         map[string]any{"name": "term-probe", "version": "1.0"},
			})
		case "session/new":
			childReply(w, *req.ID, map[string]any{"sessionId": "sess-1"})
		case "session/prompt":
			if asked {
				childReply(w, *req.ID, map[string]any{"stopReason": StopEndTurn})
				continue
			}
			asked = true
			var p struct {
				Prompt []contentBlock `json:"prompt"`
			}
			_ = json.Unmarshal(req.Params, &p)
			// The prompt carries "exe|role".
			parts := strings.SplitN(blocksText(p.Prompt), "|", 2)

			res := childAsk(w, sc, 8001, "terminal/create", map[string]any{
				"sessionId":       "sess-1",
				"command":         parts[0],
				"args":            []string{},
				"env":             []map[string]any{{"name": roleEnv, "value": parts[1]}},
				"outputByteLimit": 1024,
			})
			termID, _ := res["terminalId"].(string)
			if termID == "" {
				childNotify(w, "session/update", updateParams{SessionID: "sess-1",
					Update: agentMessageChunk(fmt.Sprintf("create-failed:%v", res))})
				childReply(w, *req.ID, map[string]any{"stopReason": StopEndTurn})
				continue
			}
			childNotify(w, "session/update", updateParams{SessionID: "sess-1",
				Update: agentMessageChunk("term:" + termID)})

			got := childAsk(w, sc, 8002, "terminal/wait_for_exit", map[string]any{
				"sessionId": "sess-1", "terminalId": termID,
			})
			childNotify(w, "session/update", updateParams{SessionID: "sess-1",
				Update: agentMessageChunk(fmt.Sprintf("exit:%v", got["exitCode"]))})
			got = childAsk(w, sc, 8003, "terminal/output", map[string]any{
				"sessionId": "sess-1", "terminalId": termID,
			})
			childNotify(w, "session/update", updateParams{SessionID: "sess-1",
				Update: agentMessageChunk("out:" + fmt.Sprint(got["output"]))})
			childAsk(w, sc, 8004, "terminal/release", map[string]any{
				"sessionId": "sess-1", "terminalId": termID,
			})
			// A released id is gone: the next output request must be refused.
			got = childAsk(w, sc, 8005, "terminal/output", map[string]any{
				"sessionId": "sess-1", "terminalId": termID,
			})
			if code, ok := got["error"]; ok {
				childNotify(w, "session/update", updateParams{SessionID: "sess-1",
					Update: agentMessageChunk(fmt.Sprintf("released:%v", code))})
			} else {
				childNotify(w, "session/update", updateParams{SessionID: "sess-1",
					Update: agentMessageChunk("NOT-RELEASED")})
			}
			childReply(w, *req.ID, map[string]any{"stopReason": StopEndTurn})
		default:
			childError(w, *req.ID, codeMethodNotFound, "method not implemented")
		}
	}
}

// runFakeMCPServer speaks the serve half of MCP over stdio (newline JSON-RPC):
// initialize, then tools/call answering one text block — the gateway test's
// subprocess server.
func runFakeMCPServer(in io.Reader, out io.Writer) {
	w := bufio.NewWriter(out)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), scanBuffer)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req childRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil || req.ID == nil {
			continue // notifications (notifications/initialized) have no id
		}
		switch req.Method {
		case "initialize":
			childReply(w, *req.ID, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "fake-mcp", "version": "1.0"},
			})
		case "tools/call":
			childReply(w, *req.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": "stdio-mcp-hello"}},
				"isError": false,
			})
		default:
			childError(w, *req.ID, -32601, "method not found")
		}
	}
	// The disconnect test's proof: this process writes the marker once its
	// stdin closes — i.e. once the owning gateway was closed.
	if marker := os.Getenv(markerEnv); marker != "" {
		_ = os.WriteFile(marker, []byte("exited"), 0o644)
	}
}

// --- the client-side tests --------------------------------------------------------

func newRoleBridge(t *testing.T, role string, opts func(*Bridge)) *Bridge {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	b, err := NewBridge(exe, nil, append(os.Environ(), roleEnv+"="+role))
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	if opts != nil {
		opts(b)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// TestBridgeCancelReportsCancelledStopReason: Cancel reaches the agent while
// the prompt is in flight; the prompt returns the streamed text and the
// cancelled stop reason (Python parity: the client side of session/cancel —
// the prompt in flight comes back on its own).
func TestBridgeCancelReportsCancelledStopReason(t *testing.T) {
	b := newRoleBridge(t, roleCancel, nil)
	id, err := b.SessionNew(t.TempDir())
	if err != nil {
		t.Fatalf("SessionNew: %v", err)
	}
	var (
		tr      TurnResult
		turnErr error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tr, turnErr = b.PromptTurn(id, "long work")
	}()
	// Let the prompt land and the agent stream its first chunk before
	// cancelling; the agent parks until the notification arrives, so the
	// ordering is: prompt in flight → cancel → cancelled answer.
	time.Sleep(200 * time.Millisecond)
	if err := b.Cancel(id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("PromptTurn did not return after Cancel")
	}
	if turnErr != nil {
		t.Fatalf("PromptTurn: %v", turnErr)
	}
	if tr.Text != "wor" {
		t.Errorf("text = %q, want the streamed %q", tr.Text, "wor")
	}
	if tr.StopReason != StopCancelled {
		t.Errorf("stopReason = %q, want %q", tr.StopReason, StopCancelled)
	}
	// Cancelling after Close reports ErrClosed, like the locked operations.
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := b.Cancel(id); !errors.Is(err, ErrClosed) {
		t.Errorf("Cancel after Close = %v, want ErrClosed", err)
	}
}

// TestBridgeLoadReceivesReplay: session/load delivers the agent's replayed
// history through OnUpdate before the response (Python parity: load_session).
func TestBridgeLoadReceivesReplay(t *testing.T) {
	var mu sync.Mutex
	var kinds []string
	b := newRoleBridge(t, roleLoad, func(br *Bridge) {
		br.OnUpdate = func(u Update) {
			mu.Lock()
			kinds = append(kinds, u.Kind)
			mu.Unlock()
		}
	})
	id, err := b.SessionNew(t.TempDir())
	if err != nil {
		t.Fatalf("SessionNew: %v", err)
	}
	if err := b.Load(id, t.TempDir()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(kinds) != fmt.Sprint([]string{"user_message_chunk", "agent_message_chunk"}) {
		t.Errorf("replay kinds = %v, want [user_message_chunk agent_message_chunk]", kinds)
	}
}

// TestBridgeFSMediated: with FS configured the Bridge answers fs/read_text_file
// (with the line/limit window), refuses escapes with the resource-not-found
// code, and writes through not-yet-existing directories (Python parity:
// BridgeState.read_text_file/write_text_file + _confine).
func TestBridgeFSMediated(t *testing.T) {
	root := t.TempDir()
	// fs/write_text_file rides the Permission policy; allow it so this test
	// exercises the filesystem, not the gate (TestBridgeFSWriteRequiresPermission
	// covers the denial).
	b := newRoleBridge(t, roleFS, func(br *Bridge) {
		br.FS = &FSConfig{Root: root}
		br.Permission = func(toolCall, input string) bool { return true }
	})
	id, err := b.SessionNew(t.TempDir())
	if err != nil {
		t.Fatalf("SessionNew: %v", err)
	}
	out, err := b.Prompt(id, root)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if !strings.Contains(out, "read:beta\n") {
		t.Errorf("line/limit read = %q, want the windowed line %q", out, "beta")
	}
	if !strings.Contains(out, fmt.Sprintf("escaped:%d", codeResourceNotFound)) {
		t.Errorf("escape attempt outcome = %q, want refusal code %d", out, codeResourceNotFound)
	}
	if !strings.Contains(out, "back:written-by-agent") {
		t.Errorf("write+read back = %q, want the written content", out)
	}
	// The write really landed on this side's disk, in the created directory.
	data, err := os.ReadFile(filepath.Join(root, "nested", "written.txt"))
	if err != nil || string(data) != "written-by-agent" {
		t.Errorf("written file = %q, %v", data, err)
	}

	// FS disabled: the same requests are refused method-not-found (the
	// disable option — nothing is advertised that does not work).
	b2 := newRoleBridge(t, roleFS, nil)
	id2, err := b2.SessionNew(t.TempDir())
	if err != nil {
		t.Fatalf("SessionNew: %v", err)
	}
	out2, err := b2.Prompt(id2, root)
	if err != nil {
		t.Fatalf("Prompt (FS nil): %v", err)
	}
	if strings.Contains(out2, "read:beta") || strings.Contains(out2, "back:written") {
		t.Errorf("FS nil answered a mediated request: %q", out2)
	}
	if strings.Contains(out2, "NOT-REFUSED") {
		t.Errorf("FS nil allowed an escape: %q", out2)
	}
	// With FS nil the mediated methods answer method-not-found.
	if !strings.Contains(out2, fmt.Sprintf("read-err:%d", codeMethodNotFound)) {
		t.Errorf("FS nil read outcome = %q, want method-not-found %d", out2, codeMethodNotFound)
	}
}

// TestBridgeTerminalMediated: with FS configured the Bridge runs the agent's
// commands under the root, captures their output, reports the exit status,
// and retires released ids (Python parity: TerminalManager).
func TestBridgeTerminalMediated(t *testing.T) {
	root := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	// terminal/create rides the Permission policy; allow it so this test
	// exercises the terminals, not the gate (TestBridgeTerminalRequiresPermission
	// covers the denial).
	b := newRoleBridge(t, roleTerm, func(br *Bridge) {
		br.FS = &FSConfig{Root: root}
		br.Permission = func(toolCall, input string) bool { return true }
	})
	id, err := b.SessionNew(t.TempDir())
	if err != nil {
		t.Fatalf("SessionNew: %v", err)
	}
	out, err := b.Prompt(id, exe+"|"+roleTermEcho)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	for _, want := range []string{"term:term-1", "exit:0", "out:term-ok", "released:-32002"} {
		if !strings.Contains(out, want) {
			t.Errorf("terminal transcript %q misses %q", out, want)
		}
	}
	// FS nil disables the terminals with the filesystem.
	b2 := newRoleBridge(t, roleTerm, nil)
	id2, err := b2.SessionNew(t.TempDir())
	if err != nil {
		t.Fatalf("SessionNew: %v", err)
	}
	out2, err := b2.Prompt(id2, exe+"|"+roleTermEcho)
	if err != nil {
		t.Fatalf("Prompt (FS nil): %v", err)
	}
	if strings.Contains(out2, "term:term-") || strings.Contains(out2, "out:term-ok") {
		t.Errorf("FS nil answered a terminal request: %q", out2)
	}
}

// TestBridgeSessionNewWithServers: the Bridge announces HTTP MCP servers in
// session/new and refuses stdio entries — a command only the declarer can
// run is useless to a subprocess agent (Python parity: session.ensure takes
// HttpMcpServer entries).
func TestBridgeSessionNewWithServers(t *testing.T) {
	b := newRoleBridge(t, roleLoad, nil)
	if _, err := b.SessionNewWithServers(t.TempDir(), []MCPServer{
		{Name: "remote", URL: "http://127.0.0.1:1/mcp"},
	}); err != nil {
		t.Errorf("SessionNewWithServers(http): %v", err)
	}
	if _, err := b.SessionNewWithServers(t.TempDir(), []MCPServer{
		{Name: "local", Command: "whatever"},
	}); !errors.Is(err, ErrUndeclarableServer) {
		t.Errorf("SessionNewWithServers(stdio) = %v, want ErrUndeclarableServer", err)
	}
	if _, err := b.SessionNewWithServers(t.TempDir(), []MCPServer{{URL: "http://x"}}); !errors.Is(err, ErrUndeclarableServer) {
		t.Errorf("SessionNewWithServers(nameless) = %v, want ErrUndeclarableServer", err)
	}
}

// TestBridgeToolGatewayViaStdio: the gateway proxies a tool call through the
// ago/mcp client to a stdio MCP server — the re-exec'd fake — and refuses
// servers it was never given (the serve role's Session.Gateway wraps exactly
// this; tested against HTTP in the serve tests).
func TestBridgeToolGatewayViaStdio(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	servers := []MCPServer{{Name: "demo", Command: exe, Args: []string{}, Env: map[string]string{roleEnv: roleMCPServe}}}
	gw := NewToolGateway(servers)
	t.Cleanup(gw.Close)
	text, callErr := gw.Call("demo", "echo", map[string]any{"msg": "hi"})
	if callErr != nil {
		t.Fatalf("gateway call: %v", callErr)
	}
	if text != "stdio-mcp-hello" {
		t.Errorf("gateway tool result = %q, want %q", text, "stdio-mcp-hello")
	}
	// Undeclared server, unknown client state.
	if _, err := NewToolGateway(nil).Call("demo", "echo", nil); !errors.Is(err, ErrServerNotFound) {
		t.Errorf("gateway on an empty declaration set = %v, want ErrServerNotFound", err)
	}
}

// TestBridgeTerminalRequiresPermission: terminal/create rides the Permission
// policy — a nil policy or a false answer refuses the request and no command
// runs, exactly as a session/request_permission is denied (the mediated
// methods act on this side of the wire, so they take the same consent).
func TestBridgeTerminalRequiresPermission(t *testing.T) {
	root := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cases := map[string]func(string, string) bool{
		"nil policy":  nil,
		"deny answer": func(string, string) bool { return false },
	}
	for name, policy := range cases {
		b := newRoleBridge(t, roleTerm, func(br *Bridge) {
			br.FS = &FSConfig{Root: root}
			br.Permission = policy
		})
		id, err := b.SessionNew(t.TempDir())
		if err != nil {
			t.Fatalf("%s: SessionNew: %v", name, err)
		}
		out, err := b.Prompt(id, exe+"|"+roleTermEcho)
		if err != nil {
			t.Fatalf("%s: Prompt: %v", name, err)
		}
		if !strings.Contains(out, fmt.Sprintf("create-failed:map[error:%d]", codeInvalidRequest)) {
			t.Errorf("%s: transcript %q misses the invalid-request refusal", name, out)
		}
		if strings.Contains(out, "term:term-") || strings.Contains(out, "out:term-ok") {
			t.Errorf("%s: a terminal was created without permission: %q", name, out)
		}
	}
}

// TestBridgeFSWriteRequiresPermission: fs/write_text_file rides the Permission
// policy — a denying answer leaves the disk untouched, and the policy sees the
// path it was asked about and the raw request.
func TestBridgeFSWriteRequiresPermission(t *testing.T) {
	root := t.TempDir()
	var askedTool, askedInput string
	b := newRoleBridge(t, roleFS, func(br *Bridge) {
		br.FS = &FSConfig{Root: root}
		br.Permission = func(toolCall, input string) bool {
			if strings.HasPrefix(toolCall, "fs/write_text_file") {
				askedTool, askedInput = toolCall, input
			}
			return false
		}
	})
	id, err := b.SessionNew(t.TempDir())
	if err != nil {
		t.Fatalf("SessionNew: %v", err)
	}
	out, err := b.Prompt(id, root)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if !strings.Contains(out, fmt.Sprintf("write-err:%d", codeInvalidRequest)) {
		t.Errorf("transcript %q misses the write refusal code %d", out, codeInvalidRequest)
	}
	if _, err := os.Stat(filepath.Join(root, "nested", "written.txt")); !os.IsNotExist(err) {
		t.Errorf("the denied write reached the disk (stat err = %v)", err)
	}
	if !strings.Contains(askedTool, "written.txt") || !strings.Contains(askedInput, "written-by-agent") {
		t.Errorf("policy saw tool=%q input=%q, want the write path and content", askedTool, askedInput)
	}
}

// TestBridgeExchangeTimeout: a hung agent — handshake answered, the next
// request never — fails the exchange with ErrTimeout once ExchangeTimeout is
// set, instead of parking the caller forever. The subprocess is killed to
// unblock the wait.
func TestBridgeExchangeTimeout(t *testing.T) {
	b := newRoleBridge(t, roleSilent, func(br *Bridge) { br.ExchangeTimeout = 300 * time.Millisecond })
	if id, err := b.SessionNew(t.TempDir()); !errors.Is(err, ErrTimeout) {
		t.Fatalf("SessionNew = %q, %v; want ErrTimeout", id, err)
	}
	// The killed agent tears down cleanly.
	if err := b.Close(); err != nil {
		t.Errorf("Close after a timed-out exchange: %v", err)
	}
}

// TestBridgeDefaultTerminalOutputLimit: a terminal whose create request sets
// no outputByteLimit still caps the captured output at the default — an unset
// limit must not mean an unbounded buffer.
func TestBridgeDefaultTerminalOutputLimit(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	m := newTerminalManager(t.TempDir(), false)
	id, rpcErr := m.create(createTerminalParams{
		Command: exe,
		Env: []envVariable{
			{Name: roleEnv, Value: roleTermSpam},
			{Name: spamBytesEnv, Value: "2500000"}, // 2.5 MiB, no explicit limit
		},
	})
	if rpcErr != nil {
		t.Fatalf("terminal create: %v", rpcErr)
	}
	if _, rpcErr := m.wait(id); rpcErr != nil {
		t.Fatalf("terminal wait: %v", rpcErr)
	}
	res, rpcErr := m.output(id)
	if rpcErr != nil {
		t.Fatalf("terminal output: %v", rpcErr)
	}
	if len(res.Output) != defaultTerminalOutputLimit {
		t.Errorf("captured %d bytes without a requested limit, want the default cap %d",
			len(res.Output), defaultTerminalOutputLimit)
	}
	if !res.Truncated {
		t.Error("output beyond the default cap must be marked truncated")
	}
}

// TestTerminalManagerEnvWhitelist: a terminal child starts from the shared
// toolpacks whitelist — a parent-only secret stays out of its environment
// (audit H-6, the acp/terminal.go half) while a whitelisted variable (PATH)
// and the per-request env both reach it.
func TestTerminalManagerEnvWhitelist(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	t.Setenv(parentSecretEnv, "parent-secret-value")
	m := newTerminalManager(t.TempDir(), false)
	id, rpcErr := m.create(createTerminalParams{
		Command: exe,
		Env:     []envVariable{{Name: roleEnv, Value: roleTermEnv}},
	})
	if rpcErr != nil {
		t.Fatalf("terminal create: %v", rpcErr)
	}
	if _, rpcErr := m.wait(id); rpcErr != nil {
		t.Fatalf("terminal wait: %v", rpcErr)
	}
	res, rpcErr := m.output(id)
	if rpcErr != nil {
		t.Fatalf("terminal output: %v", rpcErr)
	}
	for _, want := range []string{"secret-absent", "path-present"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("terminal output %q misses %q: the whitelist did not hold", res.Output, want)
		}
	}
	if strings.Contains(res.Output, "secret-leaked") {
		t.Errorf("terminal output %q carries the parent-only secret: the whitelist leaked", res.Output)
	}
}

// TestToolGatewayAllowlist: with an allowlist wired, a stdio declaration
// naming a command outside it fails its first dial instead of spawning; the
// allowed entry still dials.
func TestToolGatewayAllowlist(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	gw := NewToolGateway([]MCPServer{
		{Name: "ok", Command: exe, Env: map[string]string{roleEnv: roleMCPServe}},
		{Name: "nope", Command: "definitely-not-allowed"},
	})
	gw.allowedCommands = []string{exe}
	t.Cleanup(gw.Close)
	if _, err := gw.Call("ok", "echo", map[string]any{"msg": "hi"}); err != nil {
		t.Fatalf("allowed command dial: %v", err)
	}
	if _, err := gw.Call("nope", "echo", nil); !errors.Is(err, ErrServerNotAllowed) {
		t.Errorf("disallowed command = %v, want ErrServerNotAllowed", err)
	}
}

// TestToolGatewayURLScheme: HTTP declarations are dialed to http/https
// endpoints only — every other scheme is refused before any transport sees
// the URL.
func TestToolGatewayURLScheme(t *testing.T) {
	gw := NewToolGateway([]MCPServer{
		{Name: "fs", URL: "file:///etc/passwd"},
		{Name: "ftp", URL: "ftp://127.0.0.1/mcp"},
		{Name: "hostless", URL: "http://"},
	})
	t.Cleanup(gw.Close)
	for _, name := range []string{"fs", "ftp", "hostless"} {
		if _, err := gw.Call(name, "echo", nil); !errors.Is(err, ErrServerNotAllowed) {
			t.Errorf("Call(%q) = %v, want ErrServerNotAllowed", name, err)
		}
	}
}

// TestBridgeFSSymlinkEscape: a symlink under the root pointing outside it is
// refused by both mediated file methods — confine resolves it up front, and
// the os.Root handle re-resolves at open time, so the class stays closed even
// when a swapped component lands between the two checks (audit M-21). An
// in-root symlink still reads through: confinement, not a symlink ban.
func TestBridgeFSSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("fine"), 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(filepath.Dir(root), "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "leak.txt")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "ok.txt"), filepath.Join(root, "inner.txt")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	b := &Bridge{FS: &FSConfig{Root: root}}

	if _, rpcErr := b.fsRead(readTextFileParams{Path: "leak.txt"}); rpcErr == nil || rpcErr.Code != codeResourceNotFound {
		t.Errorf("fsRead via an escaping symlink = %v, want resource-not-found", rpcErr)
	}
	if rpcErr := b.fsWrite(writeTextFileParams{Path: "leak.txt", Content: "x"}); rpcErr == nil || rpcErr.Code != codeResourceNotFound {
		t.Errorf("fsWrite via an escaping symlink = %v, want resource-not-found", rpcErr)
	}
	if got, rpcErr := b.fsRead(readTextFileParams{Path: "inner.txt"}); rpcErr != nil || got.Content != "fine" {
		t.Errorf("fsRead via an in-root symlink = %q, %v; want the target content", got.Content, rpcErr)
	}
	// Plain paths still work through the root handle: write into a new
	// subdirectory, read it back.
	if rpcErr := b.fsWrite(writeTextFileParams{Path: "nested/file.txt", Content: "written"}); rpcErr != nil {
		t.Fatalf("fsWrite through the root handle: %v", rpcErr)
	}
	if got, rpcErr := b.fsRead(readTextFileParams{Path: "nested/file.txt"}); rpcErr != nil || got.Content != "written" {
		t.Errorf("fsRead after write = %q, %v", got.Content, rpcErr)
	}
}
