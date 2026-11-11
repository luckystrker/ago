package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/hitl"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
)

// The provider tests drive a real agent.Agent whose model is acp.Config
// against a fake ACP agent subprocess: this test binary re-exec'd (see
// TestMain) running ServeConfig with a handler the env selects.

const (
	behaviorEnv   = "AGO_ACP_TEST_BEHAVIOR"
	pidFileEnv    = "AGO_ACP_TEST_PIDFILE"
	setOptEnv     = "AGO_ACP_TEST_SETOPT_FILE"
	outFileEnv    = "AGO_ACP_TEST_OUT_FILE"
	setOptFailEnv = "AGO_ACP_TEST_SETOPT_FAIL"
)

// runProviderFake is the fake agent of the provider tests, built on Serve.
func runProviderFake() {
	behavior := os.Getenv(behaviorEnv)
	if f := os.Getenv(pidFileEnv); f != "" {
		_ = os.WriteFile(f, []byte(strconv.Itoa(os.Getpid())), 0o644)
	}
	bag := newSessionBag()
	turns := 0
	_ = Serve(ServeConfig{
		AgentName:  "fake-provider-agent",
		MCPHTTP:    behavior != "nomcp",
		OnSession:  bag.capture,
		Permission: func(string, string) bool { return true },
		Elicit:     func(string) bool { return true },
		Handle: func(ctx context.Context, sid, text string, emit *Emitter) (string, error) {
			turns++
			sess := bag.get(sid)
			switch behavior {
			case "echo", "nomcp":
				emit.Thought("thinking")
				emit.Plan([]PlanEntry{{Content: "read the file", Status: PlanPending}})
				emit.ToolCall("t1", "Read file", KindRead, StatusPending, `{"path":"a.txt"}`)
				emit.ToolCallUpdate("t1", "Read file", StatusCompleted, "file text")
				emit.Message("turn " + strconv.Itoa(turns) + ": ")
				emit.Message("echo " + text)
				emit.PromptUsage(PromptUsage{
					InputTokens: events.I64(11), OutputTokens: events.I64(7), TotalTokens: events.I64(18),
					ThoughtTokens: events.I64(2), CachedReadTokens: events.I64(5),
				})
			case "tool":
				gw := sess.Gateway()
				if gw == nil {
					return "", errors.New("no gateway declared")
				}
				out, err := gw.Call("ag2", "add", map[string]any{"a": 2, "b": 3})
				if err != nil {
					return "", err
				}
				emit.Message("sum=" + out)
			case "silent":
				// An end_turn with nothing at all.
			case "thought":
				emit.Thought("only thinking")
			case "cwd":
				wd, _ := os.Getwd()
				emit.Message(fmt.Sprintf("wd=%s session=%s", wd, sess.CWD))
			case "elicit":
				answer, ok, err := sess.Elicit(ctx, "Deploy to which env?")
				if err != nil {
					return "", err
				}
				emit.Message(fmt.Sprintf("ok=%v answer=%q", ok, answer))
			case "files":
				emit.push(Update{Kind: "agent_message_chunk", Data: []byte{1, 2, 3}, MimeType: "image/png"})
				emit.Message("caption")
				emit.push(Update{Kind: "agent_message_chunk", Data: []byte{9, 8}, MimeType: "audio/wav"})
			case "tool-ask":
				out, err := sess.Gateway().Call("ag2", "ask", map[string]any{})
				_ = os.WriteFile(os.Getenv(outFileEnv), []byte(fmt.Sprintf("out=%q err=%v", out, err)), 0o644)
				emit.Message("asked")
			case "tool-error":
				out, err := sess.Gateway().Call("ag2", "boom", map[string]any{})
				emit.Message(fmt.Sprintf("out=%q err=%v", out, err))
			case "permission-notitle":
				d, err := sess.RequestPermission(ctx, "", "")
				if err != nil {
					return "", err
				}
				emit.Message(fmt.Sprintf("allowed=%v", d.Allowed()))
			case "permission-hang":
				// Asks, then ignores the answer and keeps "working" until the
				// client cancels: only an early stop ends this turn.
				_, _ = sess.RequestPermission(ctx, "write_file", "README.md")
				emit.Message("waiting ")
				<-ctx.Done()
				return "", ctx.Err()
			case "mcp-list":
				var parts []string
				for _, s := range sess.MCPServers {
					parts = append(parts, fmt.Sprintf("%s|%s|%s", s.Name, s.URL, s.Headers["Authorization"]))
				}
				emit.Message(strings.Join(parts, ";"))
			case "permission":
				d, err := sess.RequestPermission(ctx, "write_file", "README.md")
				if err != nil {
					return "", err
				}
				emit.Message(fmt.Sprintf("allowed=%v", d.Allowed()))
			case "crash":
				emit.Message("partial ")
				time.Sleep(50 * time.Millisecond) // let the chunk reach the wire
				os.Exit(3)
			case "hang":
				emit.Message("working ")
				<-ctx.Done()
				return "", ctx.Err()
			case "stubborn":
				emit.Message("busy ")
				select {
				case <-time.After(time.Hour):
				}
			}
			return "", nil
		},
	})
}

// runProviderModelAgent is a raw agent with a model picker: it advertises a
// plain and a grouped model option and records the set_config_option params.
func runProviderModelAgent(in io.Reader, out io.Writer) {
	if f := os.Getenv(pidFileEnv); f != "" {
		_ = os.WriteFile(f, []byte(strconv.Itoa(os.Getpid())), 0o644)
	}
	w := bufio.NewWriter(out)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), scanBuffer)
	for sc.Scan() {
		var req childRequest
		if err := json.Unmarshal([]byte(strings.TrimSpace(sc.Text())), &req); err != nil || req.ID == nil {
			continue
		}
		switch req.Method {
		case "initialize":
			childReply(w, *req.ID, map[string]any{
				"protocolVersion": protocolVersion, "agentCapabilities": map[string]any{},
				"agentInfo": map[string]any{"name": "model-agent", "version": "1"},
			})
		case "session/new":
			childReply(w, *req.ID, map[string]any{
				"sessionId": "sess-m",
				"configOptions": []map[string]any{
					{"type": "select", "id": "mode", "name": "Mode", "currentValue": "a",
						"options": []map[string]any{{"value": "a", "name": "A"}}},
					{"type": "select", "id": "model", "name": "Model", "currentValue": "m-default",
						"options": []map[string]any{
							{"value": "m-default", "name": "Default"},
							{"group": "fast", "name": "Fast", "options": []map[string]any{{"value": "m-fast", "name": "Fast"}}},
						}},
				},
			})
		case "session/set_config_option":
			if os.Getenv(setOptFailEnv) != "" {
				childError(w, *req.ID, codeInternalError, "cannot switch model")
				continue
			}
			if f := os.Getenv(setOptEnv); f != "" {
				_ = os.WriteFile(f, req.Params, 0o644)
			}
			childReply(w, *req.ID, map[string]any{})
		case "session/prompt":
			childNotify(w, "session/update", updateParams{SessionID: "sess-m", Update: agentMessageChunk("ok")})
			childReply(w, *req.ID, map[string]any{"stopReason": "end_turn"})
		default:
			childError(w, *req.ID, codeMethodNotFound, "method not implemented")
		}
	}
}

// providerConfig builds a Config launching the fake agent in the given
// behavior; the pid file lets a test check the subprocess is gone.
func providerConfig(t *testing.T, role, behavior string) (*Config, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	cfg := &Config{
		Command: []string{exe},
		CWD:     t.TempDir(),
		Env:     map[string]string{roleEnv: role, behaviorEnv: behavior, pidFileEnv: pidFile},
	}
	t.Cleanup(func() { _ = cfg.Close() })
	return cfg, pidFile
}

func readPID(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the fake agent never wrote its pid")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// processAlive reports whether pid still exists (signal 0 probes without
// signalling).
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("subprocess %d is still running", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// capture subscribes to every event a run's stream carries.
type capture struct {
	mu  sync.Mutex
	evs []events.Event
}

func (c *capture) add(ev events.Event) error {
	c.mu.Lock()
	c.evs = append(c.evs, ev)
	c.mu.Unlock()
	return nil
}

func (c *capture) all() []events.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]events.Event(nil), c.evs...)
}

func askWithin(t *testing.T, a *agent.Agent, d time.Duration, msg string) (*agent.AgentReply, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return a.Ask(ctx, msg)
}

// TestProviderEndToEnd: an Agent whose model is acp.Config returns the fake
// agent's streamed text; thoughts, tool calls, plans and usage land as events;
// the session is reused for the next turn (only the new input is sent); and
// Close leaves no subprocess behind.
func TestProviderEndToEnd(t *testing.T) {
	cfg, pidFile := providerConfig(t, roleProvider, "echo")
	a := agent.New("acp-agent", "", agent.WithConfig(cfg))

	run, err := a.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	seen := &capture{}
	run.Stream().Subscribe(seen.add, stream.SubscribeOptions{})
	if err := run.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	reply, err := run.Result(context.Background())
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	run.Close() // releases the conversation's turn lock for the next Ask
	if got := reply.Body(); got != "turn 1: echo hello" {
		t.Fatalf("reply = %q, want %q", got, "turn 1: echo hello")
	}
	if r := reply.Response(); r.Provider != "acp" || r.FinishReason != StopEndTurn {
		t.Errorf("response provider=%q finish=%q", r.Provider, r.FinishReason)
	}

	var (
		chunks, reasoning []string
		call              *events.BuiltinToolCallEvent
		result            *events.BuiltinToolResultEvent
		plan              *PlanEvent
		usage             *events.UsageEvent
	)
	for _, ev := range seen.all() {
		switch e := events.AsValue(ev).(type) {
		case events.ModelMessageChunk:
			chunks = append(chunks, e.Content)
		case events.ModelReasoning:
			reasoning = append(reasoning, e.Content)
		case events.BuiltinToolCallEvent:
			call = &e
		case events.BuiltinToolResultEvent:
			result = &e
		case PlanEvent:
			plan = &e
		case events.UsageEvent:
			usage = &e
		}
	}
	if strings.Join(chunks, "|") != "turn 1: |echo hello" {
		t.Errorf("message chunks = %q", chunks)
	}
	if strings.Join(reasoning, "|") != "thinking" {
		t.Errorf("reasoning = %q", reasoning)
	}
	if call == nil || call.ID != "t1" || call.Name != "Read file" || call.Arguments != `{"path":"a.txt"}` {
		t.Errorf("tool call event = %+v", call)
	}
	if result == nil || result.ParentID != "t1" || len(result.Result.Parts) != 1 ||
		events.AsValue(result.Result.Parts[0]).(events.TextInput).Content != "file text" {
		t.Errorf("tool result event = %+v", result)
	}
	if plan == nil || len(plan.Entries) != 1 || plan.Entries[0].Content != "read the file" {
		t.Errorf("plan event = %+v", plan)
	}
	if usage == nil || *usage.Usage.PromptTokens != 11 || *usage.Usage.CompletionTokens != 7 ||
		*usage.Usage.TotalTokens != 18 || *usage.Usage.ThinkingTokens != 2 || *usage.Usage.CacheReadInputTokens != 5 ||
		usage.Provider != "acp" || usage.Kind != events.UsageKindModelCall {
		t.Errorf("usage event = %+v", usage)
	}

	pid := readPID(t, pidFile)

	// Second turn on the same conversation: same process, only the new input
	// goes over the wire (the fake echoes exactly what the prompt carried).
	reply2, err := reply.Ask(context.Background(), "again")
	if err != nil {
		t.Fatalf("second Ask: %v", err)
	}
	if got := reply2.Body(); got != "turn 2: echo again" {
		t.Fatalf("second reply = %q, want %q", got, "turn 2: echo again")
	}
	if pid2 := readPID(t, pidFile); pid2 != pid {
		t.Errorf("a second subprocess started: pid %d then %d", pid, pid2)
	}

	if err := cfg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitGone(t, pid)
}

// TestProviderLazyStart: Create launches nothing; the subprocess starts on
// the first turn.
func TestProviderLazyStart(t *testing.T) {
	cfg, pidFile := providerConfig(t, roleProvider, "echo")
	if _, err := cfg.Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := os.Stat(pidFile); err == nil {
		t.Fatal("Create started the subprocess")
	}
	if cfg.Provider() != ProviderACP {
		t.Errorf("Provider = %q", cfg.Provider())
	}
}

// TestProviderToolRoundTrip: a function tool of the Agent is served to the
// external agent over the in-process MCP host; the external agent calls it
// and the result flows back through the run's tool machinery.
func TestProviderToolRoundTrip(t *testing.T) {
	var calls int
	add, err := tools.NewFunctionTool("add", "Add two integers", func(args struct {
		A int `json:"a" agschema:"description=first,required"`
		B int `json:"b" agschema:"description=second,required"`
	}) (string, error) {
		calls++
		return strconv.Itoa(args.A + args.B), nil
	})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	cfg, _ := providerConfig(t, roleProvider, "tool")
	a := agent.New("acp-agent", "", agent.WithConfig(cfg), agent.WithTools(add))

	reply, err := askWithin(t, a, 20*time.Second, "add them")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got := reply.Body(); got != "sum=5" {
		t.Fatalf("reply = %q, want sum=5", got)
	}
	if calls != 1 {
		t.Errorf("tool ran %d times, want 1", calls)
	}
	evs, _ := reply.History().GetEvents()
	var sawCall, sawResult bool
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.ToolCallEvent:
			sawCall = sawCall || e.Name == "add"
		case events.ToolResultEvent:
			sawResult = sawResult || e.Name == "add"
		}
	}
	if !sawCall || !sawResult {
		t.Errorf("tool call/result events on the stream: call=%v result=%v", sawCall, sawResult)
	}
}

// TestProviderExternalMCPServer: an MCPServerTool is handed to the agent as a
// declared HTTP MCP server, bearer token as an Authorization header.
func TestProviderExternalMCPServer(t *testing.T) {
	srv, err := tools.NewMCPServer("http://mcp.example/x", "docs", tools.WithAuthorizationToken("tok"))
	if err != nil {
		t.Fatalf("NewMCPServer: %v", err)
	}
	cfg, _ := providerConfig(t, roleProvider, "mcp-list")
	a := agent.New("acp-agent", "", agent.WithConfig(cfg), agent.WithTools(srv))
	reply, err := askWithin(t, a, 20*time.Second, "go")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got := reply.Body(); got != "docs|http://mcp.example/x|Bearer tok" {
		t.Fatalf("declared servers = %q", got)
	}
}

// TestProviderToolsRefusedWithoutMCPHTTP: tools cannot be exposed to an agent
// that does not advertise HTTP MCP; DisableTools lets it run without them.
func TestProviderToolsRefusedWithoutMCPHTTP(t *testing.T) {
	noop, _ := tools.NewFunctionTool("noop", "does nothing", func(struct{}) (string, error) { return "", nil })
	cfg, pidFile := providerConfig(t, roleProvider, "nomcp")
	a := agent.New("acp-agent", "", agent.WithConfig(cfg), agent.WithTools(noop))
	_, err := askWithin(t, a, 20*time.Second, "go")
	if !errors.Is(err, ErrMCPCapability) {
		t.Fatalf("err = %v, want ErrMCPCapability", err)
	}
	waitGone(t, readPID(t, pidFile)) // the failed start left no subprocess

	cfg.DisableTools = true
	reply, err := askWithin(t, a, 20*time.Second, "go")
	if err != nil {
		t.Fatalf("Ask with DisableTools: %v", err)
	}
	if !strings.HasPrefix(reply.Body(), "turn 1: echo go") {
		t.Errorf("reply = %q", reply.Body())
	}
}

// TestProviderRefusesUnsupportedTools: hosted tools, filtered MCP servers and
// a server named like the tool host fail fast, before a subprocess starts.
func TestProviderRefusesUnsupportedTools(t *testing.T) {
	noop, _ := tools.NewFunctionTool("noop", "does nothing", func(struct{}) (string, error) { return "", nil })
	clash, _ := tools.NewMCPServer("http://x", gatewayServerName)

	for name, ts := range map[string][]tools.Tool{
		"hosted":     {tools.NewWebSearch()},
		"name clash": {noop, clash},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, pidFile := providerConfig(t, roleProvider, "echo")
			a := agent.New("acp-agent", "", agent.WithConfig(cfg), agent.WithTools(ts...))
			_, err := askWithin(t, a, 20*time.Second, "go")
			if !errors.Is(err, config.ErrUnsupportedTool) {
				t.Fatalf("err = %v, want ErrUnsupportedTool", err)
			}
			if _, err := os.Stat(pidFile); err == nil {
				t.Error("a subprocess was started for a refused tool set")
			}
		})
	}
}

func TestPartitionToolsFilteredMCPServer(t *testing.T) {
	_, _, err := partitionTools([]any{tools.MCPServerToolSchema{ServerURL: "http://x", ServerLabel: "s", AllowedTools: []string{"a"}}})
	if !errors.Is(err, config.ErrUnsupportedTool) {
		t.Fatalf("allowed_tools: err = %v", err)
	}
	_, _, err = partitionTools([]any{tools.MCPServerToolSchema{ServerURL: "http://x", ServerLabel: "s", BlockedTools: []string{"a"}}})
	if !errors.Is(err, config.ErrUnsupportedTool) {
		t.Fatalf("blocked_tools: err = %v", err)
	}
	fns, ext, err := partitionTools([]any{
		tools.Schema{Name: "f", Parameters: map[string]any{"type": "object"}},
		tools.MCPServerToolSchema{ServerURL: "http://x", ServerLabel: "s",
			Headers: map[string]string{"authorization": "mine"}, AuthorizationToken: "ignored"},
	})
	if err != nil || len(fns) != 1 || len(ext) != 1 {
		t.Fatalf("partition = %v %v %v", fns, ext, err)
	}
	// An Authorization header already present wins over the token.
	if ext[0].Headers["authorization"] != "mine" || len(ext[0].Headers) != 1 {
		t.Errorf("headers = %v", ext[0].Headers)
	}
}

// TestProviderPermissionPolicies: PermissionPolicy decides the agent's
// session/request_permission; "ask" goes to the run's hook, and with no hook
// the turn fails with ErrChannelFailed instead of silently denying.
func TestProviderPermissionPolicies(t *testing.T) {
	yes := func(context.Context, *events.HumanInputRequest) (*events.HumanMessage, error) {
		return &events.HumanMessage{Content: "yes"}, nil
	}
	tests := []struct {
		name    string
		policy  PermissionPolicy
		hook    hitl.HumanHook
		want    string
		wantErr error
	}{
		{"auto allows", PermissionAuto, nil, "allowed=true", nil},
		{"deny rejects", PermissionDeny, nil, "allowed=false", nil},
		{"ask routes to the hook", PermissionAsk, yes, "allowed=true", nil},
		{"zero policy is ask", "", yes, "allowed=true", nil},
		{"ask without a hook fails the turn", PermissionAsk, nil, "", ErrChannelFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _ := providerConfig(t, roleProvider, "permission")
			cfg.PermissionPolicy = tc.policy
			opts := []agent.Option{agent.WithConfig(cfg)}
			if tc.hook != nil {
				opts = append(opts, agent.WithHitlHook(tc.hook))
			}
			a := agent.New("acp-agent", "", opts...)
			reply, err := askWithin(t, a, 20*time.Second, "go")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if !hitl.IsError(err) && !errors.Is(err, hitl.ErrNotProvided) {
					t.Errorf("the hitl cause is not reachable from %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Ask: %v", err)
			}
			if reply.Body() != tc.want {
				t.Fatalf("reply = %q, want %q", reply.Body(), tc.want)
			}
		})
	}
}

// TestProviderCrashIsTypedError: the agent exits mid-turn; the call returns a
// *TransportError (never a hang), the session is dropped, and the next turn
// starts a new subprocess.
func TestProviderCrashIsTypedError(t *testing.T) {
	cfg, pidFile := providerConfig(t, roleProvider, "crash")
	a := agent.New("acp-agent", "", agent.WithConfig(cfg))

	start := time.Now()
	_, err := askWithin(t, a, 15*time.Second, "go")
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("err = %v, want ErrTransport", err)
	}
	var te *TransportError
	if !errors.As(err, &te) || te.Transport != "stdio" || !errors.Is(err, ErrAgentExited) {
		t.Errorf("transport error = %#v (cause reachable: %v)", te, errors.Is(err, ErrAgentExited))
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("the crash took %v to surface", elapsed)
	}
	waitGone(t, readPID(t, pidFile))

	// The registry forgot the dead session: a fresh conversation can start.
	cfg.mu.Lock()
	n := len(cfg.sessions)
	cfg.mu.Unlock()
	if n != 0 {
		t.Errorf("%d sessions left registered after the crash", n)
	}
}

// TestProviderLaunchFailure: a command that cannot start is an error, not a
// hang.
func TestProviderLaunchFailure(t *testing.T) {
	cfg := &Config{Command: []string{filepath.Join(t.TempDir(), "no-such-agent")}, CWD: t.TempDir()}
	a := agent.New("acp-agent", "", agent.WithConfig(cfg))
	if _, err := askWithin(t, a, 10*time.Second, "go"); err == nil {
		t.Fatal("expected a launch error")
	}
}

// TestProviderTurnTimeoutCooperative: the deadline asks the agent to cancel;
// it honors it, and the turn answers with finish reason "timeout" and the
// text that streamed.
func TestProviderTurnTimeoutCooperative(t *testing.T) {
	cfg, _ := providerConfig(t, roleProvider, "hang")
	cfg.TurnTimeout = 300 * time.Millisecond
	a := agent.New("acp-agent", "", agent.WithConfig(cfg))
	reply, err := askWithin(t, a, 15*time.Second, "go")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if reply.Response().FinishReason != "timeout" || reply.Body() != "working " {
		t.Fatalf("finish=%q body=%q, want timeout / %q", reply.Response().FinishReason, reply.Body(), "working ")
	}
}

// TestProviderTurnTimeoutHardStop: an agent that ignores the cancel is killed
// after CancelTimeout; the partial text still comes back and the session is
// dropped.
func TestProviderTurnTimeoutHardStop(t *testing.T) {
	cfg, pidFile := providerConfig(t, roleProvider, "stubborn")
	cfg.TurnTimeout = 200 * time.Millisecond
	cfg.CancelTimeout = 200 * time.Millisecond
	a := agent.New("acp-agent", "", agent.WithConfig(cfg))
	reply, err := askWithin(t, a, 15*time.Second, "go")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if reply.Response().FinishReason != "timeout" || reply.Body() != "busy " {
		t.Fatalf("finish=%q body=%q", reply.Response().FinishReason, reply.Body())
	}
	waitGone(t, readPID(t, pidFile))
	cfg.mu.Lock()
	n := len(cfg.sessions)
	cfg.mu.Unlock()
	if n != 0 {
		t.Errorf("%d sessions left registered after the hard stop", n)
	}
}

// TestProviderContextCancel: cancelling the caller's context ends the turn at
// once and leaves no subprocess.
func TestProviderContextCancel(t *testing.T) {
	cfg, pidFile := providerConfig(t, roleProvider, "hang")
	a := agent.New("acp-agent", "", agent.WithConfig(cfg))
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := a.Ask(ctx, "go")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("cancellation took %v", time.Since(start))
	}
	waitGone(t, readPID(t, pidFile))
}

// TestProviderModelSelection: ModelName is applied through
// session/set_config_option when the agent offers a model picker (group
// members included); an unoffered model is refused up front.
func TestProviderModelSelection(t *testing.T) {
	t.Run("offered model is set", func(t *testing.T) {
		cfg, _ := providerConfig(t, roleProviderModel, "")
		setFile := filepath.Join(t.TempDir(), "setopt.json")
		cfg.Env[setOptEnv] = setFile
		cfg.ModelName = "m-fast"
		a := agent.New("acp-agent", "", agent.WithConfig(cfg))
		reply, err := askWithin(t, a, 15*time.Second, "go")
		if err != nil {
			t.Fatalf("Ask: %v", err)
		}
		if reply.Response().Model != "m-fast" {
			t.Errorf("response model = %q", reply.Response().Model)
		}
		raw, err := os.ReadFile(setFile)
		if err != nil {
			t.Fatalf("set_config_option never reached the agent: %v", err)
		}
		var p map[string]string
		_ = json.Unmarshal(raw, &p)
		if p["sessionId"] != "sess-m" || p["configId"] != "model" || p["value"] != "m-fast" {
			t.Errorf("set_config_option params = %s", raw)
		}
	})
	t.Run("default model is reported, nothing is set", func(t *testing.T) {
		cfg, _ := providerConfig(t, roleProviderModel, "")
		setFile := filepath.Join(t.TempDir(), "setopt.json")
		cfg.Env[setOptEnv] = setFile
		a := agent.New("acp-agent", "", agent.WithConfig(cfg))
		reply, err := askWithin(t, a, 15*time.Second, "go")
		if err != nil {
			t.Fatalf("Ask: %v", err)
		}
		if reply.Response().Model != "m-default" {
			t.Errorf("response model = %q, want the agent's current model", reply.Response().Model)
		}
		if _, err := os.Stat(setFile); err == nil {
			t.Error("set_config_option was sent although the requested model is the current one")
		}
	})
	t.Run("unoffered model is refused", func(t *testing.T) {
		cfg, pidFile := providerConfig(t, roleProviderModel, "")
		cfg.ModelName = "m-fas"
		a := agent.New("acp-agent", "", agent.WithConfig(cfg))
		_, err := askWithin(t, a, 15*time.Second, "go")
		if !errors.Is(err, ErrModelNotOffered) {
			t.Fatalf("err = %v, want ErrModelNotOffered", err)
		}
		if !strings.Contains(err.Error(), "m-fast") {
			t.Errorf("the error does not name the closest model: %v", err)
		}
		waitGone(t, readPID(t, pidFile))
	})
	t.Run("agent without a picker keeps the name as metadata", func(t *testing.T) {
		cfg, _ := providerConfig(t, roleProvider, "echo")
		cfg.ModelName = "whatever"
		a := agent.New("acp-agent", "", agent.WithConfig(cfg))
		reply, err := askWithin(t, a, 15*time.Second, "go")
		if err != nil {
			t.Fatalf("Ask: %v", err)
		}
		if reply.Response().Model != "whatever" {
			t.Errorf("response model = %q", reply.Response().Model)
		}
	})
}

// TestProviderRejectsNonTextInput: an image cannot be carried by the text
// prompt; it fails fast, before any subprocess starts.
func TestProviderRejectsNonTextInput(t *testing.T) {
	cfg, pidFile := providerConfig(t, roleProvider, "echo")
	a := agent.New("acp-agent", "", agent.WithConfig(cfg))
	img, ierr := events.ImageInput("", "", []byte{1, 2, 3}, "image/png", "")
	if ierr != nil {
		t.Fatalf("ImageInput: %v", ierr)
	}
	_, err := a.Ask(context.Background(), img)
	if !errors.Is(err, config.ErrUnsupportedInput) {
		t.Fatalf("err = %v, want ErrUnsupportedInput", err)
	}
	if _, serr := os.Stat(pidFile); serr == nil {
		t.Error("a subprocess was started for refused input")
	}
}

// TestProviderPresets pins the exact argv and environment each preset builds.
// Only the whitelisted parent variables travel; Config.Env is merged on top.
func TestProviderPresets(t *testing.T) {
	t.Setenv("HOME", "/home/test")
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("LOGNAME", "tester")
	t.Setenv("USER", "tester")
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("TERM", "xterm")
	t.Setenv("ANTHROPIC_API_KEY", "must-not-leak")

	wantBase := []string{
		"HOME=/home/test", "LOGNAME=tester", "PATH=/usr/bin", "SHELL=/bin/sh", "TERM=xterm", "USER=tester",
	}
	tests := []struct {
		name     string
		cfg      *Config
		wantName string
		wantArgs []string
	}{
		{"claude-code", ClaudeCodeConfig(), "claude-agent-acp", []string{}},
		{"codex", CodexConfig(), "codex-acp", []string{}},
		{"opencode", OpenCodeConfig(), "opencode", []string{"acp"}},
		{"kilo-code", KiloCodeConfig(), "kilo", []string{"acp"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name, args, env := tc.cfg.launchSpec()
			if name != tc.wantName {
				t.Errorf("executable = %q, want %q", name, tc.wantName)
			}
			if strings.Join(args, " ") != strings.Join(tc.wantArgs, " ") || len(args) != len(tc.wantArgs) {
				t.Errorf("args = %q, want %q", args, tc.wantArgs)
			}
			if strings.Join(env, "\n") != strings.Join(wantBase, "\n") {
				t.Errorf("env = %q, want %q", env, wantBase)
			}
			if _, err := tc.cfg.Create(); err != nil {
				t.Errorf("Create: %v", err)
			}
		})
	}

	// Config.Env merges on top and may override a whitelisted variable.
	cfg := ClaudeCodeConfig()
	cfg.Env = map[string]string{"ANTHROPIC_API_KEY": "sk-1", "HOME": "/other"}
	_, _, env := cfg.launchSpec()
	want := []string{
		"ANTHROPIC_API_KEY=sk-1", "HOME=/other", "LOGNAME=tester", "PATH=/usr/bin", "SHELL=/bin/sh", "TERM=xterm", "USER=tester",
	}
	if strings.Join(env, "\n") != strings.Join(want, "\n") {
		t.Errorf("merged env = %q, want %q", env, want)
	}
}

func TestProviderConfigContract(t *testing.T) {
	if _, err := (&Config{}).Create(); !errors.Is(err, config.ErrNotProvided) {
		t.Errorf("Create without a command: err = %v, want ErrNotProvided", err)
	}
	if _, err := (&Config{Command: []string{"x"}, PermissionPolicy: "sometimes"}).Create(); err == nil {
		t.Error("an unknown permission policy was accepted")
	}

	base := OpenCodeConfig()
	base.ModelName = "p/m"
	base.Env = map[string]string{"K": "v"}
	cl, err := base.Clone(map[string]any{
		"model": "p/other", "permission_policy": "auto", "allow_terminal": false,
		"turn_timeout": 1.5, "expose_tools": false, "env": map[string]string{"X": "1"},
	})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	c := cl.(*Config)
	if c.Model() != "p/other" || c.PermissionPolicy != PermissionAuto || !c.DisableTerminal ||
		c.TurnTimeout != 1500*time.Millisecond || !c.DisableTools || c.Env["X"] != "1" || len(c.Env) != 1 {
		t.Errorf("clone = %+v", c)
	}
	if base.Model() != "p/m" || base.Env["K"] != "v" {
		t.Errorf("Clone mutated the original: %+v", base)
	}
	if strings.Join(c.Command, " ") != "opencode acp" {
		t.Errorf("clone command = %q", c.Command)
	}
	if _, err := base.Clone(map[string]any{"bogus": 1}); err == nil {
		t.Error("an unknown override key was accepted")
	}
	if _, err := base.Clone(map[string]any{"permission_policy": "sometimes"}); err == nil {
		t.Error("an unknown policy override was accepted")
	}
}

// TestToolHostSecurity: the host answers only POSTs to its secret path from a
// loopback Host, and serves the tools it was given.
func TestToolHostSecurity(t *testing.T) {
	h, err := newToolHost([]tools.Schema{{Name: "add", Description: "adds", Parameters: map[string]any{"type": "object"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.start(); err != nil {
		t.Fatal(err)
	}
	defer h.close()

	do := func(method, url, host, body string) (int, string) {
		req, _ := http.NewRequest(method, url, strings.NewReader(body))
		if host != "" {
			req.Host = host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	list := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

	if code, body := do(http.MethodPost, h.url, "", list); code != 200 || !strings.Contains(body, `"name":"add"`) {
		t.Errorf("tools/list = %d %s", code, body)
	}
	root := strings.TrimSuffix(h.url, h.path)
	if code, _ := do(http.MethodPost, root+"/mcp/wrong", "", list); code != http.StatusForbidden {
		t.Errorf("wrong path: %d, want 403", code)
	}
	if code, _ := do(http.MethodPost, h.url, "evil.example", list); code != http.StatusForbidden {
		t.Errorf("foreign Host: %d, want 403", code)
	}
	if code, _ := do(http.MethodGet, h.url, "", ""); code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d, want 405", code)
	}
	if code, _ := do(http.MethodPost, h.url, "", `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != http.StatusAccepted {
		t.Errorf("notification: %d, want 202", code)
	}
	// Outside a turn there is no run to execute in; unknown tools are errors.
	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"add","arguments":{}}}`
	if code, body := do(http.MethodPost, h.url, "", call); code != 200 || !strings.Contains(body, `"isError":true`) {
		t.Errorf("call with no active run = %d %s", code, body)
	}
	unknown := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"nope","arguments":{}}}`
	if _, body := do(http.MethodPost, h.url, "", unknown); !strings.Contains(body, "unknown tool") {
		t.Errorf("unknown tool = %s", body)
	}
}

// TestMCPServerWire: declarations travel in the ACP wire shape (type "http",
// headers and env as name/value lists) and decode from both that shape and the
// earlier map shape.
func TestMCPServerWire(t *testing.T) {
	b, err := json.Marshal(MCPServer{Name: "h", URL: "http://x", Headers: map[string]string{"B": "2", "A": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"type":"http","name":"h","url":"http://x","headers":[{"name":"A","value":"1"},{"name":"B","value":"2"}]}`; got != want {
		t.Errorf("http wire = %s\nwant      %s", got, want)
	}
	b, _ = json.Marshal(MCPServer{Name: "h", URL: "http://x"})
	if !strings.Contains(string(b), `"headers":[]`) {
		t.Errorf("an http entry must carry an empty headers list: %s", b)
	}
	b, _ = json.Marshal(MCPServer{Name: "s", Command: "run", Args: []string{"-v"}, Env: map[string]string{"K": "v"}})
	if got, want := string(b), `{"name":"s","command":"run","args":["-v"],"env":[{"name":"K","value":"v"}]}`; got != want {
		t.Errorf("stdio wire = %s\nwant       %s", got, want)
	}

	for name, in := range map[string]string{
		"list": `{"name":"h","url":"http://x","headers":[{"name":"A","value":"1"}],"command":"","env":[{"name":"K","value":"v"}]}`,
		"map":  `{"name":"h","url":"http://x","headers":{"A":"1"},"env":{"K":"v"}}`,
	} {
		var m MCPServer
		if err := json.Unmarshal([]byte(in), &m); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if m.Name != "h" || m.URL != "http://x" || m.Headers["A"] != "1" || m.Env["K"] != "v" {
			t.Errorf("%s decoded to %+v", name, m)
		}
	}
}

// TestProviderToolErrorReachesAgent: a failing tool is reported to the
// external agent as a tool error, not as a successful result.
func TestProviderToolErrorReachesAgent(t *testing.T) {
	boom, err := tools.NewFunctionTool("boom", "always fails", func(struct{}) (string, error) {
		return "", errors.New("kaboom")
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := providerConfig(t, roleProvider, "tool-error")
	a := agent.New("acp-agent", "", agent.WithConfig(cfg), agent.WithTools(boom))
	reply, err := askWithin(t, a, 20*time.Second, "go")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got := reply.Body(); !strings.Contains(got, "kaboom") || !strings.Contains(got, "err=") || strings.Contains(got, "err=<nil>") {
		t.Fatalf("the agent saw %q, want a tool error mentioning kaboom", got)
	}
}

// TestProviderChannelFailureStopsTurn: a dead human-input channel ends a turn
// the agent would otherwise keep running — the agent is cancelled instead of
// being waited out.
func TestProviderChannelFailureStopsTurn(t *testing.T) {
	cfg, _ := providerConfig(t, roleProvider, "permission-hang")
	a := agent.New("acp-agent", "", agent.WithConfig(cfg)) // ask policy, no hook
	start := time.Now()
	_, err := askWithin(t, a, 15*time.Second, "go")
	if !errors.Is(err, ErrChannelFailed) {
		t.Fatalf("err = %v, want ErrChannelFailed", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("the turn was not stopped early: %v", time.Since(start))
	}
}

// TestRefreshTools: the tool set may change between turns only where ACP
// allows it.
func TestRefreshTools(t *testing.T) {
	ext := []MCPServer{{Name: "docs", URL: "http://x"}}
	fn := []tools.Schema{{Name: "f"}}

	s := &providerSession{cfg: &Config{}, external: ext}
	if err := s.refreshTools(nil, []MCPServer{{Name: "other", URL: "http://y"}}); err == nil {
		t.Error("a changed MCP server set was accepted")
	}
	if err := s.refreshTools(nil, ext); err != nil {
		t.Errorf("unchanged set: %v", err)
	}
	if err := s.refreshTools(fn, ext); err == nil {
		t.Error("function tools appearing without a tool host were accepted")
	}

	h, _ := newToolHost(nil, nil)
	s = &providerSession{cfg: &Config{}, external: ext, host: h}
	if err := s.refreshTools(fn, ext); err != nil {
		t.Fatalf("hot update: %v", err)
	}
	if got := h.listTools()["tools"].([]map[string]any); len(got) != 1 || got[0]["name"] != "f" {
		t.Errorf("host tools after refresh = %v", got)
	}
	s.cfg.DisableTools = true
	if err := s.refreshTools(fn, []MCPServer{{Name: "anything"}}); err != nil {
		t.Errorf("DisableTools must skip validation: %v", err)
	}
}

// onlySession returns the single live session registered on the config.
func onlySession(t *testing.T, cfg *Config) *providerSession {
	t.Helper()
	cfg.mu.Lock()
	defer cfg.mu.Unlock()
	if len(cfg.sessions) != 1 {
		t.Fatalf("%d live sessions, want 1", len(cfg.sessions))
	}
	for _, s := range cfg.sessions {
		return s
	}
	return nil
}

func realPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", p, err)
	}
	return r
}

// TestProviderCWD: Config.CWD is the subprocess's working directory and the
// cwd announced in session/new — the agent must work in the right tree. A
// relative CWD is made absolute; FSRoot defaults to CWD.
func TestProviderCWD(t *testing.T) {
	t.Run("explicit", func(t *testing.T) {
		cfg, _ := providerConfig(t, roleProvider, "cwd")
		a := agent.New("acp-agent", "", agent.WithConfig(cfg))
		reply, err := askWithin(t, a, 15*time.Second, "go")
		if err != nil {
			t.Fatalf("Ask: %v", err)
		}
		want := realPath(t, cfg.CWD)
		if got := reply.Body(); got != "wd="+want+" session="+cfg.CWD && got != "wd="+want+" session="+want {
			t.Fatalf("agent reported %q, want wd=%s and session cwd %s", got, want, cfg.CWD)
		}
		s := onlySession(t, cfg)
		if root := s.b.FS.Root; root != cfg.CWD {
			t.Errorf("fs root = %q, want the cwd %q", root, cfg.CWD)
		}
	})
	t.Run("relative becomes absolute", func(t *testing.T) {
		cfg, _ := providerConfig(t, roleProvider, "cwd")
		cfg.CWD = "."
		wd, _ := os.Getwd()
		a := agent.New("acp-agent", "", agent.WithConfig(cfg))
		reply, err := askWithin(t, a, 15*time.Second, "go")
		if err != nil {
			t.Fatalf("Ask: %v", err)
		}
		if got := reply.Body(); !strings.HasSuffix(got, "session="+wd) {
			t.Fatalf("session cwd in %q, want the absolute %s", got, wd)
		}
	})
	t.Run("FSRoot overrides", func(t *testing.T) {
		cfg, _ := providerConfig(t, roleProvider, "cwd")
		cfg.FSRoot = t.TempDir()
		a := agent.New("acp-agent", "", agent.WithConfig(cfg))
		if _, err := askWithin(t, a, 15*time.Second, "go"); err != nil {
			t.Fatalf("Ask: %v", err)
		}
		if root := onlySession(t, cfg).b.FS.Root; root != cfg.FSRoot {
			t.Errorf("fs root = %q, want %q", root, cfg.FSRoot)
		}
	})
}

// TestProviderTerminalCapability: the terminal capability follows
// DisableTerminal.
func TestProviderTerminalCapability(t *testing.T) {
	for _, disable := range []bool{false, true} {
		cfg, _ := providerConfig(t, roleProvider, "echo")
		cfg.DisableTerminal = disable
		a := agent.New("acp-agent", "", agent.WithConfig(cfg))
		if _, err := askWithin(t, a, 15*time.Second, "go"); err != nil {
			t.Fatalf("Ask: %v", err)
		}
		b := onlySession(t, cfg).b
		caps := clientCapabilities(b)
		_, has := caps["terminal"]
		if has == disable {
			t.Errorf("DisableTerminal=%v: terminal advertised=%v", disable, has)
		}
		if _, ok := caps["fs"]; !ok {
			t.Error("fs capability not advertised")
		}
	}
}

// TestProviderCloseStopsToolHost: after Close the loopback MCP server no
// longer answers.
func TestProviderCloseStopsToolHost(t *testing.T) {
	add, _ := tools.NewFunctionTool("add", "adds", func(args struct {
		A int `json:"a" agschema:"required"`
		B int `json:"b" agschema:"required"`
	}) (string, error) {
		return strconv.Itoa(args.A + args.B), nil
	})
	cfg, _ := providerConfig(t, roleProvider, "tool")
	a := agent.New("acp-agent", "", agent.WithConfig(cfg), agent.WithTools(add))
	if _, err := askWithin(t, a, 20*time.Second, "go"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	hostURL := onlySession(t, cfg).host.url
	post := func() error {
		req, _ := http.NewRequest(http.MethodPost, hostURL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	if err := post(); err != nil {
		t.Fatalf("host does not answer before Close: %v", err)
	}
	if err := cfg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := post(); err == nil {
		t.Fatal("the tool host still answers after Close")
	}
}

// TestToolHostListsToolDetails: tools/list carries each tool's description
// and parameter schema; a tool without parameters gets the empty object
// schema, a tool without a description omits it.
func TestToolHostListsToolDetails(t *testing.T) {
	params := map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}
	h, _ := newToolHost([]tools.Schema{
		{Name: "search", Description: "search the docs", Parameters: params},
		{Name: "bare"},
	}, nil)
	got := h.listTools()["tools"].([]map[string]any)
	if len(got) != 2 {
		t.Fatalf("tools = %v", got)
	}
	if got[0]["name"] != "search" || got[0]["description"] != "search the docs" {
		t.Errorf("first tool = %v", got[0])
	}
	if gotJSON, _ := json.Marshal(got[0]["inputSchema"]); string(gotJSON) != `{"properties":{"q":{"type":"string"}},"type":"object"}` {
		t.Errorf("inputSchema = %s", gotJSON)
	}
	if _, has := got[1]["description"]; has {
		t.Errorf("an empty description was listed: %v", got[1])
	}
	if gotJSON, _ := json.Marshal(got[1]["inputSchema"]); string(gotJSON) != `{"properties":{},"type":"object"}` {
		t.Errorf("empty-parameter schema = %s", gotJSON)
	}
	// setTools replaces the list wholesale.
	h.setTools([]tools.Schema{{Name: "only"}})
	if got := h.listTools()["tools"].([]map[string]any); len(got) != 1 || got[0]["name"] != "only" {
		t.Errorf("tools after setTools = %v", got)
	}
}

// TestProviderElicitationPolicies: ElicitationPolicy decides the agent's
// elicitation/create; "ask" reaches the human hook, "decline" never does, and
// "ask" with no hook fails the turn.
func TestProviderElicitationPolicies(t *testing.T) {
	var asked []string
	answer := func(_ context.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
		asked = append(asked, req.Content)
		return &events.HumanMessage{Content: "prod"}, nil
	}
	tests := []struct {
		name    string
		policy  ElicitationPolicy
		hook    hitl.HumanHook
		want    string
		asked   int
		wantErr error
	}{
		{"ask answers", ElicitationAsk, answer, `ok=true answer="prod"`, 1, nil},
		{"zero policy is ask", "", answer, `ok=true answer="prod"`, 1, nil},
		{"decline never asks", ElicitationDecline, answer, `ok=false answer=""`, 0, nil},
		{"ask without a hook fails the turn", ElicitationAsk, nil, "", 0, ErrChannelFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			asked = nil
			cfg, _ := providerConfig(t, roleProvider, "elicit")
			cfg.ElicitationPolicy = tc.policy
			opts := []agent.Option{agent.WithConfig(cfg)}
			if tc.hook != nil {
				opts = append(opts, agent.WithHitlHook(tc.hook))
			}
			a := agent.New("acp-agent", "", opts...)
			reply, err := askWithin(t, a, 20*time.Second, "go")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Ask: %v", err)
			}
			if reply.Body() != tc.want {
				t.Fatalf("reply = %q, want %q", reply.Body(), tc.want)
			}
			if len(asked) != tc.asked {
				t.Errorf("the hook was asked %d times, want %d: %v", len(asked), tc.asked, asked)
			}
			if tc.asked > 0 && !strings.Contains(asked[0], "Deploy to which env?") {
				t.Errorf("the question did not reach the human: %q", asked[0])
			}
		})
	}
}

// TestProviderToolNeedsHumanEndsTurn: a gateway tool that has to ask a person
// who cannot be reached ends the turn with ErrChannelFailed; the agent is told
// why, and the call is closed off in the transcript so it stays replayable.
func TestProviderToolNeedsHumanEndsTurn(t *testing.T) {
	ask, err := tools.NewFunctionTool("ask", "asks a person", func(_ struct{}, cx *agocontext.Context) (string, error) {
		return cx.Input(context.Background(), "which env?", 0)
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := providerConfig(t, roleProvider, "tool-ask")
	outFile := filepath.Join(t.TempDir(), "out")
	cfg.Env[outFileEnv] = outFile
	a := agent.New("acp-agent", "", agent.WithConfig(cfg), agent.WithTools(ask)) // no hook

	run, err := a.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer run.Close()
	if err := run.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	_, err = run.Result(context.Background())
	if !errors.Is(err, ErrChannelFailed) {
		t.Fatalf("err = %v, want ErrChannelFailed", err)
	}
	raw, rerr := os.ReadFile(outFile)
	if rerr != nil || !strings.Contains(string(raw), "had to ask a person") {
		t.Errorf("the agent was told %q (%v), want the human-input explanation", raw, rerr)
	}
	evs, _ := run.Stream().History().GetEvents()
	var closedOff bool
	for _, ev := range evs {
		if r, ok := events.AsValue(ev).(events.ToolResultEvent); ok && r.Name == "ask" {
			for _, part := range r.Result.Parts {
				if txt, ok := events.AsValue(part).(events.TextInput); ok && txt.Content == events.SentinelToolResult {
					closedOff = true
				}
			}
		}
	}
	if !closedOff {
		t.Error("the abandoned call was not closed off in the transcript")
	}
}

// TestProviderFilesReturned: image and audio blocks of the agent's reply
// become ModelResponse.Files; their text siblings stay the message.
func TestProviderFilesReturned(t *testing.T) {
	cfg, _ := providerConfig(t, roleProvider, "files")
	a := agent.New("acp-agent", "", agent.WithConfig(cfg))
	reply, err := askWithin(t, a, 15*time.Second, "go")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if reply.Body() != "caption" {
		t.Errorf("body = %q", reply.Body())
	}
	files := reply.Files()
	if len(files) != 2 || string(files[0].Data) != "\x01\x02\x03" || files[0].Metadata["mimeType"] != "image/png" ||
		string(files[1].Data) != "\x09\x08" || files[1].Metadata["mimeType"] != "audio/wav" {
		t.Fatalf("files = %+v", files)
	}
}

// TestUpdateFileBlockRoundTrip: an image/audio message chunk survives the
// wire, and a corrupt payload is dropped rather than failing the update.
func TestUpdateFileBlockRoundTrip(t *testing.T) {
	for _, mime := range []string{"image/png", "audio/mpeg"} {
		b, err := json.Marshal(Update{Kind: "agent_message_chunk", Data: []byte("xyz"), MimeType: mime})
		if err != nil {
			t.Fatal(err)
		}
		wantType := strings.SplitN(mime, "/", 2)[0]
		if !strings.Contains(string(b), `"type":"`+wantType+`"`) {
			t.Errorf("%s wire = %s", mime, b)
		}
		var u Update
		if err := json.Unmarshal(b, &u); err != nil {
			t.Fatal(err)
		}
		if string(u.Data) != "xyz" || u.MimeType != mime || u.Text != "" {
			t.Errorf("%s decoded to %+v", mime, u)
		}
	}
	var u Update
	bad := `{"sessionUpdate":"agent_message_chunk","content":{"type":"image","data":"!!!","mimeType":"image/png"}}`
	if err := json.Unmarshal([]byte(bad), &u); err != nil || len(u.Data) != 0 {
		t.Errorf("corrupt payload: err=%v data=%v", err, u.Data)
	}
}

// TestProviderConfigCloneCopiesEverything: Clone carries every field over, by
// value (the copy shares no slice or map with the original), and an override
// replaces exactly its own field.
func TestProviderConfigCloneCopiesEverything(t *testing.T) {
	base := &Config{
		Command: []string{"agent", "acp"}, CWD: "/w", Env: map[string]string{"K": "v"}, ModelName: "m",
		PermissionPolicy: PermissionDeny, ElicitationPolicy: ElicitationDecline, FSRoot: "/fs",
		DisableTerminal: true, AdditionalDirectories: []string{"/extra"},
		StartupTimeout: time.Second, TurnTimeout: 2 * time.Second, CancelTimeout: 3 * time.Second, DisableTools: true,
	}
	cl, err := base.Clone(nil)
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	c := cl.(*Config)
	if strings.Join(c.Command, " ") != "agent acp" || c.CWD != "/w" || c.Env["K"] != "v" || c.ModelName != "m" ||
		c.PermissionPolicy != PermissionDeny || c.ElicitationPolicy != ElicitationDecline || c.FSRoot != "/fs" ||
		!c.DisableTerminal || len(c.AdditionalDirectories) != 1 || c.AdditionalDirectories[0] != "/extra" ||
		c.StartupTimeout != time.Second || c.TurnTimeout != 2*time.Second || c.CancelTimeout != 3*time.Second || !c.DisableTools {
		t.Fatalf("clone lost a field: %+v", c)
	}
	// By value: mutating the clone leaves the original alone.
	c.Command[0], c.Env["K"], c.AdditionalDirectories[0] = "x", "changed", "/changed"
	if base.Command[0] != "agent" || base.Env["K"] != "v" || base.AdditionalDirectories[0] != "/extra" {
		t.Errorf("clone shares storage with the original: %+v", base)
	}

	// Every override key, in both spellings.
	over := map[string]map[string]any{
		"Command":               {"command": []string{"c"}},
		"CWD":                   {"cwd": "/c"},
		"FSRoot":                {"fs_root": "/r"},
		"elicitation":           {"elicitation_policy": "ask"},
		"AdditionalDirectories": {"additional_directories": []string{"/a", "/b"}},
		"startup":               {"startup_timeout": 4},
		"cancel":                {"cancel_timeout": 6 * time.Second},
		"DisableTerminal":       {"DisableTerminal": false},
		"DisableTools":          {"DisableTools": false},
		"allow_terminal":        {"allow_terminal": true},
		"expose_tools":          {"expose_tools": true},
	}
	for name, o := range over {
		got, err := base.Clone(o)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		g := got.(*Config)
		switch name {
		case "Command":
			if len(g.Command) != 1 || g.Command[0] != "c" {
				t.Errorf("%s: %v", name, g.Command)
			}
		case "CWD":
			if g.CWD != "/c" {
				t.Errorf("%s: %q", name, g.CWD)
			}
		case "FSRoot":
			if g.FSRoot != "/r" {
				t.Errorf("%s: %q", name, g.FSRoot)
			}
		case "elicitation":
			if g.ElicitationPolicy != ElicitationAsk {
				t.Errorf("%s: %q", name, g.ElicitationPolicy)
			}
		case "AdditionalDirectories":
			if len(g.AdditionalDirectories) != 2 {
				t.Errorf("%s: %v", name, g.AdditionalDirectories)
			}
		case "startup":
			if g.StartupTimeout != 4*time.Second {
				t.Errorf("%s: %v", name, g.StartupTimeout)
			}
		case "cancel":
			if g.CancelTimeout != 6*time.Second {
				t.Errorf("%s: %v", name, g.CancelTimeout)
			}
		case "DisableTerminal", "allow_terminal":
			if g.DisableTerminal {
				t.Errorf("%s: terminal still disabled", name)
			}
		case "DisableTools", "expose_tools":
			if g.DisableTools {
				t.Errorf("%s: tools still disabled", name)
			}
		}
	}
	// Wrong types are refused, not coerced.
	for k, v := range map[string]any{"cwd": 1, "command": "x", "env": "x", "allow_terminal": "yes", "turn_timeout": "soon", "fs_root": 2} {
		if _, err := base.Clone(map[string]any{k: v}); err == nil {
			t.Errorf("override %s=%v of the wrong type was accepted", k, v)
		}
	}
}

// TestProviderPermissionPromptNamesTheAction: the question asked of the human
// names the action, with a fallback when the agent gave no title.
func TestProviderPermissionPromptNamesTheAction(t *testing.T) {
	for behavior, want := range map[string]string{"permission": "write_file", "permission-notitle": "an action"} {
		var prompt string
		hook := func(_ context.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
			prompt = req.Content
			return &events.HumanMessage{Content: "yes"}, nil
		}
		cfg, _ := providerConfig(t, roleProvider, behavior)
		a := agent.New("acp-agent", "", agent.WithConfig(cfg), agent.WithHitlHook(hook))
		if _, err := askWithin(t, a, 20*time.Second, "go"); err != nil {
			t.Fatalf("%s: %v", behavior, err)
		}
		if !strings.Contains(prompt, "permission for: "+want) {
			t.Errorf("%s: prompt = %q, want it to name %q", behavior, prompt, want)
		}
	}
}

func TestConfigAgentLabel(t *testing.T) {
	if got := (&Config{Command: []string{"claude-agent-acp", "x"}}).agentLabel(); got != "claude-agent-acp" {
		t.Errorf("label = %q", got)
	}
	for _, c := range []*Config{{}, {Command: []string{""}}} {
		if got := c.agentLabel(); got != "acp-agent" {
			t.Errorf("label of %v = %q, want the generic one", c.Command, got)
		}
	}
	b := &Bridge{}
	c := &Config{Command: []string{"cmd"}}
	if got := agentName(b, c); got != "cmd" {
		t.Errorf("agentName without Info = %q", got)
	}
	b.Info.Name = "Introduced"
	if got := agentName(b, c); got != "Introduced" {
		t.Errorf("agentName with Info = %q", got)
	}
}

// TestProviderUpdateMapping: the tool-call updates map onto the builtin tool
// events — arguments default to {} when the agent sent none, the result text
// falls back to the status.
func TestProviderUpdateMapping(t *testing.T) {
	cx := agocontext.New(stream.NewMemoryStream())
	seen := &capture{}
	cx.Stream.Subscribe(seen.add, stream.SubscribeOptions{})
	s := &providerSession{cfg: &Config{}}
	s.setTurn(newTurnState(context.Background(), cx))

	s.onUpdate(Update{Kind: "tool_call", ToolCallID: "a"})
	s.onUpdate(Update{Kind: "tool_call", ToolCallID: "b", Title: "T", RawInput: json.RawMessage("null")})
	s.onUpdate(Update{Kind: "tool_call", ToolCallID: "c", Title: "T", RawInput: json.RawMessage(`{"x":1}`)})
	s.onUpdate(Update{Kind: "tool_call_update", ToolCallID: "a", Status: StatusFailed})
	s.onUpdate(Update{Kind: "tool_call_update", ToolCallID: "c", Title: "T", Status: StatusCompleted, ContentText: "done"})
	s.onUpdate(Update{Kind: "usage_update", Used: 1, Size: 2}) // no AGo event
	s.onUpdate(Update{Kind: "current_mode_update", ModeID: "code"})

	var calls []events.BuiltinToolCallEvent
	var results []events.BuiltinToolResultEvent
	var modes int
	for _, ev := range seen.all() {
		switch e := events.AsValue(ev).(type) {
		case events.BuiltinToolCallEvent:
			calls = append(calls, e)
		case events.BuiltinToolResultEvent:
			results = append(results, e)
		case ModeChange:
			modes++
		}
	}
	if len(calls) != 3 || calls[0].Name != "tool" || calls[0].Arguments != "{}" ||
		calls[1].Arguments != "{}" || calls[2].Arguments != `{"x":1}` || calls[2].Name != "T" {
		t.Errorf("call events = %+v", calls)
	}
	text := func(r events.BuiltinToolResultEvent) string {
		return events.AsValue(r.Result.Parts[0]).(events.TextInput).Content
	}
	if len(results) != 2 || text(results[0]) != StatusFailed || text(results[1]) != "done" {
		t.Errorf("result events = %+v", results)
	}
	if modes != 1 || len(seen.all()) != 6 {
		t.Errorf("modes=%d events=%d, want 1 mode change and no event for usage_update", modes, len(seen.all()))
	}
	// Outside a turn an update is dropped, not a panic.
	s.setTurn(nil)
	s.onUpdate(Update{Kind: "agent_message_chunk", Text: "late"})
}

// TestProviderStartupFailures: an agent that dies before answering the
// handshake is a typed transport error, a failing model switch is reported,
// and neither leaves a subprocess.
func TestProviderStartupFailures(t *testing.T) {
	t.Run("exits during the handshake", func(t *testing.T) {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		// The test binary with no tests selected exits at once, mid-handshake.
		cfg := &Config{Command: []string{exe, "-test.run=^$"}, CWD: t.TempDir()}
		a := agent.New("acp-agent", "", agent.WithConfig(cfg))
		_, err = askWithin(t, a, 15*time.Second, "go")
		if !errors.Is(err, ErrTransport) || !errors.Is(err, ErrAgentExited) {
			t.Fatalf("err = %v, want a transport error wrapping ErrAgentExited", err)
		}
	})
	t.Run("model switch refused by the agent", func(t *testing.T) {
		cfg, pidFile := providerConfig(t, roleProviderModel, "")
		cfg.Env[setOptFailEnv] = "1"
		cfg.ModelName = "m-fast"
		a := agent.New("acp-agent", "", agent.WithConfig(cfg))
		_, err := askWithin(t, a, 15*time.Second, "go")
		if !errors.Is(err, ErrProtocol) || !strings.Contains(err.Error(), "cannot switch model") {
			t.Fatalf("err = %v, want the agent's refusal", err)
		}
		waitGone(t, readPID(t, pidFile))
	})
}

func TestClosestModels(t *testing.T) {
	values := []string{"gpt-5", "gpt-5-mini", "gpt-5-nano", "gpt-5-pro", "claude"}
	got := closestModels("gpt-5", values)
	if got != "; closest offered: [gpt-5 gpt-5-mini gpt-5-nano]" {
		t.Errorf("closest = %q, want at most three", got)
	}
	if got := closestModels("zzz", values); got != "" {
		t.Errorf("no resemblance should name nothing, got %q", got)
	}
	if got := closestModels("GPT-5-MINI", values); !strings.Contains(got, "gpt-5-mini") {
		t.Errorf("matching is case-insensitive, got %q", got)
	}
}

// TestProviderWarnsOnSilentTurn: an agent that ends a turn with nothing —
// no text, no files, no work — is logged; one that did anything is not.
func TestProviderWarnsOnSilentTurn(t *testing.T) {
	for behavior, wantWarn := range map[string]bool{
		"silent": true, "echo": false, "files": false, "thought": false,
	} {
		t.Run(behavior, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			defer slog.SetDefault(prev)

			cfg, _ := providerConfig(t, roleProvider, behavior)
			cfg.ModelName = "some-model"
			a := agent.New("acp-agent", "", agent.WithConfig(cfg))
			if _, err := askWithin(t, a, 15*time.Second, "go"); err != nil {
				t.Fatalf("Ask: %v", err)
			}
			got := buf.String()
			if warned := strings.Contains(got, "produced no output"); warned != wantWarn {
				t.Fatalf("warned=%v, want %v; log: %s", warned, wantWarn, got)
			}
			if wantWarn && (!strings.Contains(got, "some-model") || !strings.Contains(got, filepath.Base(cfg.Command[0]))) {
				t.Errorf("the warning does not name the agent and model: %s", got)
			}
		})
	}
}

// TestIsTransportErr: a broken connection is a transport error; an agent's
// own protocol error is not.
func TestIsTransportErr(t *testing.T) {
	for name, err := range map[string]error{
		"agent exited":  fmt.Errorf("x: %w", ErrAgentExited),
		"bridge closed": ErrClosed,
		"timeout":       ErrTimeout,
		"closed pipe":   fmt.Errorf("write: %w", io.ErrClosedPipe),
		"closed file":   fmt.Errorf("write: %w", os.ErrClosed),
		"broken pipe":   fmt.Errorf("write: %w", syscall.EPIPE),
	} {
		if !isTransportErr(err) {
			t.Errorf("%s: not classified as transport", name)
		}
	}
	for name, err := range map[string]error{
		"nil":            nil,
		"protocol error": fmt.Errorf("%w: internal error", ErrProtocol),
		"plain":          errors.New("boom"),
	} {
		if isTransportErr(err) {
			t.Errorf("%s: classified as transport", name)
		}
	}
}

// TestPartitionToolsPointerForms: schemas may arrive by pointer; a nil
// pointer is refused, a filtered MCP server is refused by pointer too.
func TestPartitionToolsPointerForms(t *testing.T) {
	var nilSchema *tools.Schema
	var nilServer *tools.MCPServerToolSchema
	if _, _, err := partitionTools([]any{nilSchema}); !errors.Is(err, config.ErrUnsupportedTool) {
		t.Errorf("nil *Schema: err = %v", err)
	}
	if _, _, err := partitionTools([]any{nilServer}); !errors.Is(err, config.ErrUnsupportedTool) {
		t.Errorf("nil *MCPServerToolSchema: err = %v", err)
	}
	if _, _, err := partitionTools([]any{42}); !errors.Is(err, config.ErrUnsupportedTool) {
		t.Errorf("unknown schema type: err = %v", err)
	}
	fns, ext, err := partitionTools([]any{
		&tools.Schema{Name: "f"},
		&tools.MCPServerToolSchema{ServerURL: "http://x", ServerLabel: "s"},
	})
	if err != nil || len(fns) != 1 || fns[0].Name != "f" || len(ext) != 1 || ext[0].Name != "s" || ext[0].Headers != nil {
		t.Errorf("pointer forms = %v %v %v", fns, ext, err)
	}
	if _, _, err := partitionTools([]any{&tools.MCPServerToolSchema{ServerURL: "http://x", ServerLabel: "s", BlockedTools: []string{"a"}}}); !errors.Is(err, config.ErrUnsupportedTool) {
		t.Errorf("filtered *MCPServerToolSchema: err = %v", err)
	}
}

// TestToolHostRPCEdges: malformed requests, the initialize handshake's version
// echo and bad tools/call params all answer with well-formed JSON-RPC.
func TestToolHostRPCEdges(t *testing.T) {
	h, err := newToolHost(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.start(); err != nil {
		t.Fatal(err)
	}
	defer h.close()
	post := func(body string) string {
		resp, err := http.Post(h.url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	for name, body := range map[string]string{
		"not json":       `not json`,
		"missing method": `{"jsonrpc":"2.0","id":1}`,
		"empty method":   `{"jsonrpc":"2.0","id":1,"method":""}`,
	} {
		if got := post(body); !strings.Contains(got, `"code":-32600`) {
			t.Errorf("%s: %s, want invalid request", name, got)
		}
	}
	if got := post(`{"jsonrpc":"2.0","id":1,"method":"nope"}`); !strings.Contains(got, `"code":-32601`) {
		t.Errorf("unknown method: %s", got)
	}
	if got := post(`{"jsonrpc":"2.0","id":2,"method":"ping"}`); !strings.Contains(got, `"result":{}`) {
		t.Errorf("ping: %s", got)
	}
	for name, params := range map[string]string{
		"no name":       `{"arguments":{}}`,
		"empty":         `{"name":""}`,
		"not an object": `[1]`,
	} {
		got := post(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":` + params + `}`)
		if !strings.Contains(got, `"code":-32602`) {
			t.Errorf("tools/call %s: %s, want invalid params", name, got)
		}
	}
	for name, tc := range map[string]struct{ params, want string }{
		"echoes the client's version": {`{"protocolVersion":"2024-11-05"}`, `"protocolVersion":"2024-11-05"`},
		"defaults without one":        {`{}`, `"protocolVersion":"` + mcpProtocolVersion + `"`},
		"defaults on an empty one":    {`{"protocolVersion":""}`, `"protocolVersion":"` + mcpProtocolVersion + `"`},
		"defaults on garbage params":  {`7`, `"protocolVersion":"` + mcpProtocolVersion + `"`},
	} {
		got := post(`{"jsonrpc":"2.0","id":4,"method":"initialize","params":` + tc.params + `}`)
		if !strings.Contains(got, tc.want) || !strings.Contains(got, `"name":"ag2"`) {
			t.Errorf("initialize %s: %s, want %s", name, got, tc.want)
		}
	}
}

// TestSettleCondition: a call is settled by its own result, error, not-found
// or client hand-off — and by nothing else.
func TestSettleCondition(t *testing.T) {
	cond := settleCondition("c1")
	res := func(id string) events.ToolResultEvent { return events.ToolResultEvent{ParentID: id} }
	cases := []struct {
		name string
		ev   events.Event
		want bool
	}{
		{"result", res("c1"), true},
		{"other result", res("c2"), false},
		{"error", events.ToolErrorEvent{ToolResultEvent: res("c1")}, true},
		{"other error", events.ToolErrorEvent{ToolResultEvent: res("c2")}, false},
		{"not found", events.ToolNotFoundEvent{ToolErrorEvent: events.ToolErrorEvent{ToolResultEvent: res("c1")}}, true},
		{"other not found", events.ToolNotFoundEvent{ToolErrorEvent: events.ToolErrorEvent{ToolResultEvent: res("c2")}}, false},
		{"client hand-off", events.ClientToolCallEvent{ID: "c1"}, true},
		{"other hand-off", events.ClientToolCallEvent{ID: "c2"}, false},
		{"unrelated", events.ModelMessageChunk{Content: "c1"}, false},
	}
	for _, tc := range cases {
		if got := cond(tc.ev); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestResultBlocks: every result-part kind renders as MCP content.
func TestResultBlocks(t *testing.T) {
	blocks := resultBlocks(events.ToolResult{Parts: []events.Input{
		events.TextInput{Content: "t"},
		events.DataInput{Data: map[string]any{"k": 1}},
		events.DataInput{Data: make(chan int)}, // cannot be JSON-encoded: falls back to its printed form
		events.BinaryInput{Data: []byte{1, 2}, MediaType: "image/png"},
		events.BinaryInput{Data: []byte{1, 2, 3}, MediaType: "application/pdf"},
		events.UrlInput{URL: "http://u"},
		events.FileIdInput{FileID: "f1"},
	}})
	if len(blocks) != 7 {
		t.Fatalf("blocks = %+v", blocks)
	}
	if blocks[0].Text != "t" || blocks[1].Text != `{"k":1}` {
		t.Errorf("text and data blocks = %+v %+v", blocks[0], blocks[1])
	}
	if blocks[2].Type != "text" || !strings.HasPrefix(blocks[2].Text, "0x") {
		t.Errorf("unencodable data block = %+v", blocks[2])
	}
	if blocks[3].Type != "image" || blocks[3].MimeType != "image/png" || blocks[3].Data != "AQI=" {
		t.Errorf("image block = %+v", blocks[3])
	}
	if blocks[4].Text != "<binary application/pdf, 3 bytes>" || blocks[5].Text != "http://u" || blocks[6].Text != "<file f1>" {
		t.Errorf("remaining blocks = %+v %+v %+v", blocks[4], blocks[5], blocks[6])
	}
}

// TestToolHostCloseCutsOffStuckRequests: a request that never completes does
// not keep the host alive past the shutdown grace.
func TestToolHostCloseCutsOffStuckRequests(t *testing.T) {
	h, err := newToolHost(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.shutdownGrace = 100 * time.Millisecond
	if err := h.start(); err != nil {
		t.Fatal(err)
	}
	addr := strings.TrimSuffix(strings.TrimPrefix(h.url, "http://"), h.path)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Headers promise a body that never arrives: the handler blocks reading it.
	fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: %s\r\nContent-Length: 100\r\n\r\n{", h.path, addr)
	time.Sleep(100 * time.Millisecond) // let the server accept and start reading

	start := time.Now()
	h.close()
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("close waited %v for a stuck request", d)
	}
	if c2, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c2.Close()
		t.Error("the host still accepts connections after close")
	}
	// Cut off means closed by the server (EOF or reset), not a read timeout.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, rerr := conn.Read(make([]byte, 1))
	var ne net.Error
	if rerr == nil || (errors.As(rerr, &ne) && ne.Timeout()) {
		t.Errorf("the stuck connection was not cut off: read err = %v", rerr)
	}
}

// TestToolHostCloseLetsInFlightFinish: with the default grace, a request that
// is mid-flight when close starts still gets its answer.
func TestToolHostCloseLetsInFlightFinish(t *testing.T) {
	h, err := newToolHost(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.start(); err != nil {
		t.Fatal(err)
	}
	addr := strings.TrimSuffix(strings.TrimPrefix(h.url, "http://"), h.path)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: %s\r\nContent-Length: %d\r\n\r\n%s", h.path, addr, len(body), body[:10])
	time.Sleep(100 * time.Millisecond)

	closed := make(chan struct{})
	go func() { h.close(); close(closed) }()
	time.Sleep(150 * time.Millisecond) // close is now waiting on the half-sent request
	fmt.Fprint(conn, body[10:])

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	reply, _ := io.ReadAll(bufio.NewReader(conn))
	if !strings.Contains(string(reply), "200 OK") || !strings.Contains(string(reply), `"result":{}`) {
		t.Errorf("the in-flight request was not answered: %q", reply)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Error("close never returned")
	}
}
