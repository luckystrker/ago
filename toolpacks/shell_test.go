package toolpacks

import (
	stdcontext "context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
)

// toolsSchema aliases the schema type the Schemas() calls return.
type toolsSchema = tools.Schema

// shellCmd returns a test command per platform shell (cmd /c vs sh -c).
func shellCmd(echo, failing, sleeper, cwd string) (simple, stderrExit, sleep, workdir string) {
	if runtime.GOOS == "windows" {
		// cmd: echo; echo to stderr with a non-zero exit (exit, not exit /b:
		// a bare exit sets the cmd process code through a /c chain); stall;
		// print cwd.
		return "echo " + echo,
			"echo " + failing + " 1>&2 & exit 3",
			"ping -n 30 127.0.0.1 >nul",
			"cd"
	}
	return "echo " + echo,
		"echo " + failing + " >&2; exit 3",
		"sleep 30",
		"pwd"
}

func TestShellToolEcho(t *testing.T) {
	s, err := NewShellTool()
	if err != nil {
		t.Fatalf("NewShellTool: %v", err)
	}
	defer s.Close()
	simple, _, _, _ := shellCmd("packs_ok", "", "", "")
	_, cx := newTestContext()
	ev := callTool(t, s, cx, s.Name(), `{"command":`+quoteGo(simple)+`}`)
	got := resultPartText(t, ev)
	if !strings.Contains(got, "packs_ok") {
		t.Fatalf("output = %q, want it to contain packs_ok", got)
	}
}

func TestShellToolStderrAndExitCode(t *testing.T) {
	s, err := NewShellTool()
	if err != nil {
		t.Fatalf("NewShellTool: %v", err)
	}
	defer s.Close()
	_, stderrExit, _, _ := shellCmd("", "oops_stream", "", "")
	_, cx := newTestContext()
	ev := callTool(t, s, cx, s.Name(), `{"command":`+quoteGo(stderrExit)+`}`)
	got := resultPartText(t, ev)
	// stderr lands in the result and the exit code is appended (Python parity:
	// (stdout+stderr).strip() plus the [exit code: N] suffix).
	if !strings.Contains(got, "oops_stream") || !strings.Contains(got, "[exit code: 3]") {
		t.Fatalf("output = %q, want oops_stream and [exit code: 3]", got)
	}
}

func TestShellToolTimeout(t *testing.T) {
	s, err := NewShellTool(WithShellTimeout(300 * time.Millisecond))
	if err != nil {
		t.Fatalf("NewShellTool: %v", err)
	}
	defer s.Close()
	_, _, sleep, _ := shellCmd("", "", "", "")
	_, cx := newTestContext()
	ev := callTool(t, s, cx, s.Name(), `{"command":`+quoteGo(sleep)+`}`)
	got := resultPartText(t, ev)
	if !strings.Contains(got, "timed out") || !strings.Contains(got, "[exit code: 124]") {
		t.Fatalf("output = %q, want a timeout with exit code 124", got)
	}
}

func TestShellToolWorkdir(t *testing.T) {
	dir := t.TempDir()
	s, err := NewShellTool(WithShellWorkdir(dir))
	if err != nil {
		t.Fatalf("NewShellTool: %v", err)
	}
	defer s.Close()
	if s.Workdir() == "" || !strings.EqualFold(s.Workdir(), dir) {
		t.Fatalf("Workdir = %q, want %q", s.Workdir(), dir)
	}
	_, _, _, cwd := shellCmd("", "", "", "")
	_, cx := newTestContext()
	ev := callTool(t, s, cx, s.Name(), `{"command":`+quoteGo(cwd)+`}`)
	got := resultPartText(t, ev)
	if !strings.Contains(strings.ToLower(got), strings.ToLower(dir)) {
		t.Fatalf("cwd output = %q, want it to contain %q", got, dir)
	}
}

func TestShellToolBlocklist(t *testing.T) {
	s, err := NewShellTool(WithShellBlockPatterns("rm -rf", "format c:"))
	if err != nil {
		t.Fatalf("NewShellTool: %v", err)
	}
	defer s.Close()
	_, cx := newTestContext()
	ev := callTool(t, s, cx, s.Name(), `{"command":"echo rm -rf / now"}`)
	if got := resultPartText(t, ev); !strings.Contains(got, "Command not allowed") {
		t.Fatalf("blocked command output = %q, want a refusal", got)
	}
	simple, _, _, _ := shellCmd("still_works", "", "", "")
	ev = callTool(t, s, cx, s.Name(), `{"command":`+quoteGo(simple)+`}`)
	if got := resultPartText(t, ev); !strings.Contains(got, "still_works") {
		t.Fatalf("allowed command output = %q", got)
	}
}

func TestShellToolSchema(t *testing.T) {
	s, err := NewShellTool()
	if err != nil {
		t.Fatalf("NewShellTool: %v", err)
	}
	defer s.Close()
	_, cx := newTestContext()
	schemas, err := s.Schemas(cx)
	if err != nil || len(schemas) != 1 {
		t.Fatalf("schemas = %v, %v", schemas, err)
	}
	sch, ok := schemas[0].(toolsSchema)
	if !ok {
		t.Fatalf("schema type %T", schemas[0])
	}
	if sch.Name != "run_shell_command" {
		t.Fatalf("name = %q", sch.Name)
	}
	props, _ := sch.Parameters["properties"].(map[string]any)
	if _, ok := props["command"]; !ok {
		t.Fatalf("schema properties = %v, want command", sch.Parameters)
	}
	// The default description is the model's contract (security audit
	// 2026-10-31, C-1): it must state the real execution semantics, not a
	// sandbox that does not exist.
	for _, want := range []string{"without isolation", "not a security boundary", "human approval"} {
		if !strings.Contains(sch.Description, want) {
			t.Fatalf("description = %q, want it to contain %q", sch.Description, want)
		}
	}
}

// quoteGo renders s as a JSON string literal.
func quoteGo(s string) string {
	b := make([]byte, 0, len(s)+2)
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		default:
			b = append(b, c)
		}
	}
	return string(append(b, '"'))
}

// --- Cancel and Close stop in-flight commands (upstream parity 153ef4cdd) -----

// toolCallOutcome is the settlement of one tool call driven off the test
// goroutine: the result/error event, or the machinery failure.
type toolCallOutcome struct {
	ev  events.Event
	err error
}

// startToolCall fires one tool call on the stream and reports its settlement on
// the returned channel — the callTool machinery minus t, because the goroutine
// driving an in-flight call cannot touch the testing state. The returned stop
// func unsubscribes; the caller must drain the channel before stopping.
func startToolCall(cx *agocontext.Context, tool tools.Tool, name, args string) (<-chan toolCallOutcome, func(), error) {
	subs, err := tool.Register(cx, nil)
	if err != nil {
		return nil, nil, err
	}
	ch := make(chan toolCallOutcome, 1)
	s := cx.Stream
	sub := s.Subscribe(func(ev events.Event) error {
		switch events.AsValue(ev).(type) {
		case events.ToolResultEvent, events.ToolErrorEvent:
			ch <- toolCallOutcome{ev: ev}
		}
		return nil
	}, stream.SubscribeOptions{})
	stop := func() {
		for _, id := range subs {
			cx.Stream.Unsubscribe(id)
		}
		s.Unsubscribe(sub)
	}
	go func() {
		call := events.NewToolCallEvent(name)
		call.Arguments = args
		if err := s.Send(call); err != nil {
			ch <- toolCallOutcome{err: err}
		}
	}()
	return ch, stop, nil
}

// probeCommand renders a grandchildProbe argv as the shell command string the
// ShellTool receives (the wrapper shell is the tool's own business).
func probeCommand(start []string) string { return start[len(start)-1] }

func TestShellToolTurnCancelKillsCommand(t *testing.T) {
	start, listCmd, ok := grandchildProbe()
	if !ok {
		t.Skip("no process-listing tool on this platform")
	}
	// A stray matching process from outside the test would poison the verdict.
	if strings.Contains(strings.ToLower(listMatching(t, listCmd)), strings.ToLower(nameOf(start))) {
		t.Skip("a matching process is already running; cannot isolate the check")
	}
	s, err := NewShellTool()
	if err != nil {
		t.Fatalf("NewShellTool: %v", err)
	}
	defer s.Close()
	_, cx := newTestContext()
	turnCtx, cancelTurn := stdcontext.WithCancel(stdcontext.Background())
	defer cancelTurn()
	cx.SetTurnContext(turnCtx)
	ch, stop, err := startToolCall(cx, s, s.Name(), `{"command":`+quoteGo(probeCommand(start))+`}`)
	if err != nil {
		t.Fatalf("startToolCall: %v", err)
	}
	defer stop()
	if !waitProbeRunning(t, listCmd, nameOf(start), 10*time.Second) {
		t.Fatal("the command never started")
	}
	cancelTurn()
	select {
	case o := <-ch:
		if o.err != nil {
			t.Fatalf("call machinery: %v", o.err)
		}
		if got := resultPartText(t, o.ev); !strings.Contains(got, "interrupted") {
			t.Fatalf("canceled call output = %q, want the interruption error", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the tool call did not return after the turn cancel")
	}
	if out := listMatching(t, listCmd); strings.Contains(strings.ToLower(out), strings.ToLower(nameOf(start))) {
		t.Fatalf("a command survived the turn cancel:\n%s", out)
	}
}

func TestShellToolCloseStopsRunningCommand(t *testing.T) {
	start, listCmd, ok := grandchildProbe()
	if !ok {
		t.Skip("no process-listing tool on this platform")
	}
	// A stray matching process from outside the test would poison the verdict.
	if strings.Contains(strings.ToLower(listMatching(t, listCmd)), strings.ToLower(nameOf(start))) {
		t.Skip("a matching process is already running; cannot isolate the check")
	}
	s, err := NewShellTool()
	if err != nil {
		t.Fatalf("NewShellTool: %v", err)
	}
	_, cx := newTestContext()
	ch, stop, err := startToolCall(cx, s, s.Name(), `{"command":`+quoteGo(probeCommand(start))+`}`)
	if err != nil {
		t.Fatalf("startToolCall: %v", err)
	}
	defer stop()
	if !waitProbeRunning(t, listCmd, nameOf(start), 10*time.Second) {
		t.Fatal("the command never started")
	}
	// Close returns only once the command tree is dead and the workdir is gone.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if out := listMatching(t, listCmd); strings.Contains(strings.ToLower(out), strings.ToLower(nameOf(start))) {
		t.Fatalf("a command survived Close:\n%s", out)
	}
	if _, err := os.Stat(s.Workdir()); !os.IsNotExist(err) {
		t.Fatalf("auto workdir survived Close: %v", err)
	}
	select {
	case o := <-ch:
		if o.err != nil {
			t.Fatalf("call machinery: %v", o.err)
		}
		if got := resultPartText(t, o.ev); !strings.Contains(got, "closed while the command was running") {
			t.Fatalf("interrupted call output = %q, want the closed-while-running error", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the in-flight call never settled after Close")
	}
	// A call after Close fails instead of starting work in a disposed sandbox.
	if _, err := s.run(cx, "echo x"); !errors.Is(err, ErrSandboxClosed) {
		t.Fatalf("run after Close: err = %v, want ErrSandboxClosed", err)
	}
}

// TestShellToolNameOption pins the name override: a custom name lands on the
// tool, an empty one is refused at construction (Python parity: the name kwarg).
func TestShellToolNameOption(t *testing.T) {
	if _, err := NewShellTool(WithShellName("")); err == nil {
		t.Fatal("WithShellName(\"\") must be refused")
	}
	s, err := NewShellTool(WithShellName("my_shell"))
	if err != nil {
		t.Fatalf("NewShellTool with a name: %v", err)
	}
	defer s.Close()
	if s.Name() != "my_shell" {
		t.Fatalf("tool name = %q, want my_shell", s.Name())
	}
}

// TestShellToolOptionValidation pins the option guards: a non-positive timeout,
// an empty env key and a non-positive max_output are construction errors; their
// valid counterparts pass.
func TestShellToolOptionValidation(t *testing.T) {
	if _, err := NewShellTool(WithShellTimeout(0)); err == nil {
		t.Fatal("WithShellTimeout(0) must be refused")
	}
	if _, err := NewShellTool(WithShellTimeout(-time.Second)); err == nil {
		t.Fatal("a negative timeout must be refused")
	}
	s, err := NewShellTool(WithShellTimeout(2 * time.Second))
	if err != nil {
		t.Fatalf("valid timeout refused: %v", err)
	}
	defer s.Close()

	if _, err := NewShellTool(WithShellEnv(map[string]string{"": "value"})); err == nil {
		t.Fatal("an empty env key must be refused")
	}
	s2, err := NewShellTool(WithShellEnv(map[string]string{"AGO_TEST_VAR": "v"}))
	if err != nil {
		t.Fatalf("valid env refused: %v", err)
	}
	defer s2.Close()

	if _, err := NewShellTool(WithShellMaxOutput(0)); err == nil {
		t.Fatal("WithShellMaxOutput(0) must be refused")
	}
	s3, err := NewShellTool(WithShellMaxOutput(10))
	if err != nil {
		t.Fatalf("valid max_output refused: %v", err)
	}
	defer s3.Close()
}

// TestShellToolCallerWorkdirSurvivesClose pins the retention contract: a
// caller-supplied workdir is never deleted by Close — only the auto-created
// one is (Python parity: LocalSandbox.aclose cleans up its own temp dir).
func TestShellToolCallerWorkdirSurvivesClose(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(marker, []byte("mine"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	s, err := NewShellTool(WithShellWorkdir(dir))
	if err != nil {
		t.Fatalf("NewShellTool: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("caller-supplied workdir removed by Close: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("caller files inside the workdir removed by Close: %v", err)
	}
}
