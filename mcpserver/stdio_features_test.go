package mcpserver

import (
	"bytes"
	"context"
	agocontext "github.com/luckystrker/ago/context"
	"strings"
	"testing"

	"github.com/luckystrker/ago/agent"
)

// serveLines runs the stdio loop over the given input lines and returns the
// decoded replies.
func serveLines(t *testing.T, s *Server, lines ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := s.serveIO(strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatalf("serveIO: %v", err)
	}
	var replies []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		replies = append(replies, decodeObj(t, line))
	}
	return replies
}

func TestStdioSharesOneConversationAndServesResources(t *testing.T) {
	model := chatModel("first", "second")
	a := agent.New("served", "", agent.WithConfig(model))
	s, err := NewServerForAgent(a, WithConversations(ConversationConfig{}),
		WithResources(Resource{URI: "mem://x", Name: "x", Read: func(context.Context, *agocontext.Context) (ResourceContent, error) {
			return ResourceContent{Text: "from stdio"}, nil
		}}))
	if err != nil {
		t.Fatal(err)
	}
	replies := serveLines(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ask","arguments":{"message":"one"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ask","arguments":{"message":"two"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"resources/read","params":{"uri":"mem://x"}}`,
	)
	if len(replies) != 4 {
		t.Fatalf("%d replies, want 4 (the notification gets none)", len(replies))
	}
	handle := func(r map[string]any) string {
		return r["result"].(map[string]any)["_meta"].(map[string]any)[ConversationMetaKey].(string)
	}
	// No Mcp-Session-Id over stdio: every turn of the process shares one session,
	// so the second turn continues the first's conversation.
	if handle(replies[1]) != handle(replies[2]) {
		t.Errorf("handles %q vs %q, want one shared conversation", handle(replies[1]), handle(replies[2]))
	}
	if !historyMentions(model.history(1), "one") {
		t.Error("the second turn did not see the first")
	}
	text := replies[3]["result"].(map[string]any)["contents"].([]any)[0].(map[string]any)["text"]
	if text != "from stdio" {
		t.Errorf("resource over stdio = %v", text)
	}
	// stdio has no back-channel: a served agent's question falls to its own hook
	// (nothing here is answered on the pipe).
	if _, ok := any(s).(interface{ Close() error }); !ok {
		t.Error("Server has no Close")
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close on a stdio server = %v", err)
	}
}
