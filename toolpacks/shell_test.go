package toolpacks

import (
	"runtime"
	"strings"
	"testing"
	"time"

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
