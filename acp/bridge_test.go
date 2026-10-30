package acp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

// The Bridge tests re-exec this test binary as a stub ACP agent: when the env
// flag is set, TestMain serves newline-delimited JSON-RPC on stdin/stdout and
// never runs the tests (the "TestMain process helper" pattern, as in
// mcp/stdio_test.go). The deny-probe role is a second agent that inspects the
// client's session/request_permission answer on the wire (nested shape,
// selected option id) and reports what it saw as chunks.
const (
	roleEnv      = "AGO_ACP_TEST_ROLE"
	roleAgent    = "acp-fake-agent"
	roleDenyProb = "acp-deny-probe-agent"
)

func TestMain(m *testing.M) {
	switch os.Getenv(roleEnv) {
	case roleAgent:
		runFakeAgent(os.Stdin, os.Stdout)
	case roleDenyProb:
		runDenyProbeAgent(os.Stdin, os.Stdout)
	default:
		os.Exit(m.Run())
	}
}

// --- the stub agent (child process) --------------------------------------------

type childRequest struct {
	ID     *int64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// runFakeAgent speaks the serve role of the protocol: initialize, session/new,
// session/prompt. Its first prompt streams two chunks ("Hel", "lo") and then
// raises the two outbound requests the subset wires — session/request_permission
// and elicitation/create — reacting to the client's answers: an allow grants a
// "granted" chunk, an accepted elicitation a "said:<content>" chunk.
func runFakeAgent(in io.Reader, out io.Writer) {
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
				"agentInfo":         map[string]any{"name": "fake-acp-agent", "title": "Fake", "version": "9.9"},
			})
		case "session/new":
			childReply(w, *req.ID, map[string]any{"sessionId": "sess-1"})
		case "session/prompt":
			childNotify(w, "session/update", updateParams{SessionID: "sess-1", Update: agentMessageChunk("Hel")})
			childNotify(w, "session/update", updateParams{SessionID: "sess-1", Update: agentMessageChunk("lo")})
			if !asked {
				asked = true
				grant := childCall(w, sc, 9001, "session/request_permission", map[string]any{
					"sessionId": "sess-1",
					"toolCall":  map[string]any{"title": "write_file", "rawInput": "README.md"},
					"options": []map[string]any{
						{"optionId": "ok-1", "name": "Allow", "kind": "allow_once"},
						{"optionId": "no-1", "name": "Reject", "kind": "reject_once"},
					},
				})
				// Standard-agent strictness: the answer must arrive nested —
				// {"outcome": {"outcome": "selected", "optionId": …}}. The
				// flat spelling of the earlier revision of this package
				// fails the assertion and grants nothing.
				if inner, ok := grant["outcome"].(map[string]any); ok &&
					inner["outcome"] == OutcomeSelected && inner["optionId"] == "ok-1" {
					childNotify(w, "session/update", updateParams{SessionID: "sess-1", Update: agentMessageChunk("granted")})
				}
				answer := childCall(w, sc, 9002, "elicitation/create", map[string]any{"message": "Deploy to prod?"})
				if answer["action"] == ElicitAccept {
					childNotify(w, "session/update", updateParams{SessionID: "sess-1",
						Update: agentMessageChunk("said:" + fmt.Sprint(answer["content"]))})
				}
			}
			childReply(w, *req.ID, map[string]any{"stopReason": "end_turn"})
		default:
			childError(w, *req.ID, codeMethodNotFound, "method not implemented")
		}
	}
}

// runDenyProbeAgent raises one session/request_permission and streams back
// what the client's answer selected, so a test can assert the wire answer:
// "deny-no1" when the offered reject option was selected, "allow" the allow
// option, "cancelled" the cancelled outcome — nothing for a flat or malformed
// answer, which a standard agent would refuse.
func runDenyProbeAgent(in io.Reader, out io.Writer) {
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
				"agentInfo":         map[string]any{"name": "deny-probe", "version": "1.0"},
			})
		case "session/new":
			childReply(w, *req.ID, map[string]any{"sessionId": "sess-1"})
		case "session/prompt":
			if !asked {
				asked = true
				answer := childCall(w, sc, 9001, "session/request_permission", map[string]any{
					"sessionId": "sess-1",
					"toolCall":  map[string]any{"title": "write_file", "rawInput": "README.md"},
					"options": []map[string]any{
						{"optionId": "ok-1", "name": "Allow", "kind": "allow_once"},
						{"optionId": "no-1", "name": "Reject", "kind": "reject_once"},
					},
				})
				if inner, ok := answer["outcome"].(map[string]any); ok {
					switch {
					case inner["outcome"] == OutcomeCancelled:
						childNotify(w, "session/update", updateParams{SessionID: "sess-1", Update: agentMessageChunk("cancelled")})
					case inner["outcome"] == OutcomeSelected && inner["optionId"] == "no-1":
						childNotify(w, "session/update", updateParams{SessionID: "sess-1", Update: agentMessageChunk("deny-no1")})
					case inner["outcome"] == OutcomeSelected && inner["optionId"] == "ok-1":
						childNotify(w, "session/update", updateParams{SessionID: "sess-1", Update: agentMessageChunk("allow")})
					}
				}
			}
			childReply(w, *req.ID, map[string]any{"stopReason": "end_turn"})
		default:
			childError(w, *req.ID, codeMethodNotFound, "method not implemented")
		}
	}
}

// childCall writes one agent→client request and reads the client's answer,
// skipping anything else on the wire.
func childCall(w *bufio.Writer, sc *bufio.Scanner, id int64, method string, params any) map[string]any {
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
			return map[string]any{}
		}
		var result map[string]any
		if err := json.Unmarshal(msg.Result, &result); err != nil {
			return map[string]any{}
		}
		return result
	}
	return map[string]any{}
}

func childWrite(w *bufio.Writer, msg any) {
	payload, err := json.Marshal(msg)
	if err != nil {
		return
	}
	fmt.Fprintln(w, string(payload))
	w.Flush()
}

func childReply(w *bufio.Writer, id int64, result any) {
	childWrite(w, rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func childError(w *bufio.Writer, id int64, code int, message string) {
	childWrite(w, rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}})
}

func childNotify(w *bufio.Writer, method string, params any) {
	childWrite(w, rpcNotification{JSONRPC: "2.0", Method: method, Params: params})
}

// --- the client-side tests -------------------------------------------------------

func newFakeAgentBridge(t *testing.T) *Bridge {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	b, err := NewBridge(exe, nil, append(os.Environ(), roleEnv+"="+roleAgent))
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func TestBridgeRoundTrip(t *testing.T) {
	b := newFakeAgentBridge(t)
	if b.Info.Name != "fake-acp-agent" || b.Info.Version != "9.9" || b.Info.Title != "Fake" {
		t.Errorf("Info = %+v", b.Info)
	}
	if b.Info.ProtocolVersion != protocolVersion {
		t.Errorf("protocol version = %d, want %d", b.Info.ProtocolVersion, protocolVersion)
	}

	id, err := b.SessionNew(t.TempDir())
	if err != nil {
		t.Fatalf("SessionNew: %v", err)
	}
	if id != "sess-1" {
		t.Errorf("session id = %q, want sess-1", id)
	}

	// The agent streams "Hel" then "lo"; Prompt assembles them in order.
	out, err := b.Prompt(id, "hi")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if out != "Hello" {
		t.Errorf("Prompt text = %q, want %q", out, "Hello")
	}

	// A second prompt on the same session works (the stub raises its outbound
	// requests only once).
	if out, err = b.Prompt(id, "again"); err != nil || out != "Hello" {
		t.Errorf("second Prompt = %q, %v, want %q, nil", out, err, "Hello")
	}

	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := b.Prompt(id, "x"); !errors.Is(err, ErrClosed) {
		t.Errorf("Prompt after Close = %v, want ErrClosed", err)
	}
	if _, err := b.SessionNew("."); !errors.Is(err, ErrClosed) {
		t.Errorf("SessionNew after Close = %v, want ErrClosed", err)
	}
	if err := b.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

// TestBridgeDeniesByDefault: without policies the agent's permission request
// is answered with the reject decision and its question declined — nothing is
// allowed on the user's behalf, so the turn ends with the streamed chunks only.
func TestBridgeDeniesByDefault(t *testing.T) {
	b := newFakeAgentBridge(t)
	id, err := b.SessionNew(t.TempDir())
	if err != nil {
		t.Fatalf("SessionNew: %v", err)
	}
	out, err := b.Prompt(id, "hi")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if out != "Hello" {
		t.Errorf("Prompt text = %q, want %q (no granted/said chunks)", out, "Hello")
	}
}

// TestBridgePoliciesAllow: the policies receive the tool call and the question
// verbatim; their allow/accept answers select the agent's own allow option and
// carry the answer content.
func TestBridgePoliciesAllow(t *testing.T) {
	b := newFakeAgentBridge(t)
	var (
		gotTool  string
		gotInput string
		gotMsg   string
	)
	b.Permission = func(tool, input string) bool {
		gotTool, gotInput = tool, input
		return true
	}
	b.Elicit = func(message string) (string, bool) {
		gotMsg = message
		return "yes", true
	}
	id, err := b.SessionNew(t.TempDir())
	if err != nil {
		t.Fatalf("SessionNew: %v", err)
	}
	out, err := b.Prompt(id, "hi")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if gotTool != "write_file" || gotInput != "README.md" {
		t.Errorf("policy saw tool=%q input=%q, want write_file/README.md", gotTool, gotInput)
	}
	if gotMsg != "Deploy to prod?" {
		t.Errorf("elicitation policy saw %q, want %q", gotMsg, "Deploy to prod?")
	}
	if out != "Hellograntedsaid:yes" {
		t.Errorf("Prompt text = %q, want %q", out, "Hellograntedsaid:yes")
	}
}

// TestBridgeElicitDeclined: ok=false declines without fabricating an answer.
func TestBridgeElicitDeclined(t *testing.T) {
	b := newFakeAgentBridge(t)
	b.Permission = func(tool, input string) bool { return true }
	b.Elicit = func(message string) (string, bool) { return "", false }
	id, err := b.SessionNew(t.TempDir())
	if err != nil {
		t.Fatalf("SessionNew: %v", err)
	}
	out, err := b.Prompt(id, "hi")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if out != "Hellogranted" {
		t.Errorf("Prompt text = %q, want %q", out, "Hellogranted")
	}
}

// TestBridgePolicyFalseDeniesWithRejectOption: a false answer selects the
// agent's reject option rather than the allow one.
func TestBridgePolicyFalseDeniesWithRejectOption(t *testing.T) {
	b := newFakeAgentBridge(t)
	b.Permission = func(tool, input string) bool { return false }
	id, err := b.SessionNew(t.TempDir())
	if err != nil {
		t.Fatalf("SessionNew: %v", err)
	}
	out, err := b.Prompt(id, "hi")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if out != "Hello" {
		t.Errorf("Prompt text = %q, want %q", out, "Hello")
	}
}

// TestBridgeDenySelectsOfferedRejectOption: the deny answer carries the
// agent's own reject_once option id — nested as the SDK schema demands —
// never a synthesized id the agent did not offer (Python parity:
// resolve_permission_option_id returns the reject option; bridge.py wraps it
// in an AllowedOutcome). A flat or synthesized answer leaves the probe silent.
func TestBridgeDenySelectsOfferedRejectOption(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	b, err := NewBridge(exe, nil, append(os.Environ(), roleEnv+"="+roleDenyProb))
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	id, err := b.SessionNew(t.TempDir())
	if err != nil {
		t.Fatalf("SessionNew: %v", err)
	}
	out, err := b.Prompt(id, "hi")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if out != "deny-no1" {
		t.Errorf("Prompt text = %q, want %q (nested answer selecting the offered reject option)", out, "deny-no1")
	}
}

func TestBridgeBadCommand(t *testing.T) {
	if _, err := NewBridge("", nil, nil); !errors.Is(err, ErrNotProvided) {
		t.Errorf("NewBridge(\"\") = %v, want ErrNotProvided", err)
	}
	if _, err := NewBridge("ago-acp-no-such-command", nil, nil); err == nil {
		t.Error("NewBridge with a missing executable must fail")
	}
}
