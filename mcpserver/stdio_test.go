package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/luckystrker/ago/mcp"
	"github.com/luckystrker/ago/tools"
)

type echoArgs struct {
	Text string `json:"text"`
}

// The stdio tests re-exec this test binary as the server: when the env flag is
// set, TestMain serves newline-delimited JSON-RPC on stdin/stdout and never
// runs the tests (the "TestMain process helper" pattern of mcp/stdio_test.go,
// with the roles flipped — here the child is the system under test and the
// parent drives it with the real ago/mcp client).
const stdioServerEnv = "AGO_MCPSERVER_TEST_STDIO_SERVER"

func TestMain(m *testing.M) {
	if os.Getenv(stdioServerEnv) == "1" {
		runStdioServer()
		return
	}
	os.Exit(m.Run())
}

// runStdioServer is the child process body: the round-trip server on real
// stdio. It returns when the parent closes the pipe (stdin EOF).
func runStdioServer() {
	s, err := NewServer(ServerConfig{
		Name:    "stdio-served",
		Version: "4.5.6",
		Ask: func(_ context.Context, userText string) (string, error) {
			return "reply: " + userText, nil
		},
		Tools: mustTestTools(),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "child:", err)
		os.Exit(1)
	}
	if err := s.Stdio(); err != nil {
		fmt.Fprintln(os.Stderr, "child:", err)
		os.Exit(1)
	}
}

// mustTestTools builds the round-trip tool pair for the child process, where
// there is no *testing.T to fail through.
func mustTestTools() []tools.Tool {
	echo, err := tools.NewFunctionTool("echo", "Echoes its input",
		func(a echoArgs) (string, error) { return "echo: " + a.Text, nil })
	if err != nil {
		panic(err)
	}
	fail, err := tools.NewFunctionTool("fail", "Always fails",
		func(struct{}) error { return errors.New("boom") })
	if err != nil {
		panic(err)
	}
	return []tools.Tool{echo, fail}
}

func TestStdioRoundTrip(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cl, err := mcp.New(&mcp.MCPStdioConfig{
		Command: exe,
		CWD:     t.TempDir(),
		Env:     append(os.Environ(), stdioServerEnv+"=1"),
	})
	if err != nil {
		t.Fatalf("mcp.New: %v", err)
	}
	t.Cleanup(func() { cl.Close() })

	info, err := cl.Initialize()
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if info.Name != "stdio-served" || info.Version != "4.5.6" {
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
	if fmt.Sprint(names) != fmt.Sprint([]string{"ask", "echo", "fail"}) {
		t.Errorf("tools = %v", names)
	}

	out, err := cl.CallTool("ask", map[string]any{"message": "over stdio"})
	if err != nil {
		t.Fatalf("CallTool ask: %v", err)
	}
	if textPart(t, out.Parts[0]) != "reply: over stdio" {
		t.Errorf("ask = %q", textPart(t, out.Parts[0]))
	}

	out, err = cl.CallTool("echo", map[string]any{"text": "pipe"})
	if err != nil {
		t.Fatalf("CallTool echo: %v", err)
	}
	if textPart(t, out.Parts[0]) != "echo: pipe" {
		t.Errorf("echo = %q", textPart(t, out.Parts[0]))
	}

	if _, err := cl.CallTool("fail", nil); !errors.Is(err, mcp.ErrToolCall) || !strings.Contains(err.Error(), "boom") {
		t.Errorf("CallTool fail = %v, want ErrToolCall naming boom", err)
	}

	// Closing the client terminates the server process (stdin EOF → Stdio
	// returns nil); later operations report ErrClosed.
	if err := cl.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := cl.CallTool("ask", nil); !errors.Is(err, mcp.ErrClosed) {
		t.Errorf("CallTool after Close = %v, want ErrClosed", err)
	}
	if err := cl.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}
