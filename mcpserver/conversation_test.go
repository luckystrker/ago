package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/ago/agent"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

func chatModel(replies ...string) *scriptedModel {
	m := &scriptedModel{}
	for _, r := range replies {
		m.steps = append(m.steps, say(r))
	}
	return m
}

func chatServer(t *testing.T, model *scriptedModel, tr TransportConfig, cc ConversationConfig) *httptestServer {
	t.Helper()
	a := agent.New("served", "", agent.WithConfig(model))
	s, ts := serveAgent(t, a, tr, WithConversations(cc))
	return &httptestServer{s: s, url: ts.URL}
}

type httptestServer struct {
	s   *Server
	url string
}

// chat sends one ask and returns (reply text, handle from _meta, the text blocks).
func (h *httptestServer) chat(t *testing.T, sid, message, conversation string) (string, string, []string) {
	t.Helper()
	args := fmt.Sprintf(`{"message":%q`, message)
	if conversation != "" {
		args += fmt.Sprintf(`,"conversation":%q`, conversation)
	}
	args += "}"
	hdr := map[string]string{}
	if sid != "" {
		hdr["Mcp-Session-Id"] = sid
	}
	msg, _ := rpcCall(t, h.url, hdr, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"ask","arguments":`+args+`}}`)
	res, e := checkResponseEnvelope(t, msg)
	if e != nil {
		t.Fatalf("tools/call failed: %v", e)
	}
	checkCallToolResult(t, res)
	m := res.(map[string]any)
	var texts []string
	for _, b := range m["content"].([]any) {
		texts = append(texts, b.(map[string]any)["text"].(string))
	}
	handle := ""
	if meta, ok := m["_meta"].(map[string]any); ok {
		handle, _ = meta[ConversationMetaKey].(string)
	}
	if m["isError"] == true {
		return "ERROR: " + texts[0], handle, texts
	}
	return texts[0], handle, texts
}

func TestConversationContinuityPerMCPSession(t *testing.T) {
	model := chatModel("first", "second", "third", "fourth")
	h := chatServer(t, model, TransportConfig{Stateful: true}, ConversationConfig{})
	a := openSession(t, h.url, `{}`, "")
	b := openSession(t, h.url, `{}`, "")

	reply, handleA, texts := h.chat(t, a, "alpha one", "")
	if reply != "first" || handleA == "" {
		t.Fatalf("first call = %q handle %q", reply, handleA)
	}
	// The handle travels twice: a text block for the model, _meta for programs.
	if len(texts) != 2 || texts[1] != "Conversation handle: "+handleA+"\nPass it back as the 'conversation' argument to continue this conversation." {
		t.Errorf("content = %q", texts)
	}

	// Session B is its own conversation: it sees nothing of A's.
	_, handleB, _ := h.chat(t, b, "beta one", "")
	if handleB == "" || handleB == handleA {
		t.Fatalf("session B handle %q vs A %q", handleB, handleA)
	}
	if historyMentions(model.history(1), "alpha one") {
		t.Error("session B's first turn saw session A's history")
	}

	// The same session continues its conversation without naming it.
	_, again, _ := h.chat(t, a, "alpha two", "")
	if again != handleA {
		t.Errorf("session A's second call ran in %q, want %q", again, handleA)
	}
	if !historyMentions(model.history(2), "alpha one") || historyMentions(model.history(2), "beta one") {
		t.Errorf("session A's second turn saw %s", mustJSONAny(model.history(2)))
	}

	// A named handle wins over the session — across sessions, even.
	_, viaHandle, _ := h.chat(t, b, "beta two", handleA)
	if viaHandle != handleA {
		t.Errorf("explicit handle ran in %q, want %q", viaHandle, handleA)
	}
	if !historyMentions(model.history(3), "alpha two") {
		t.Errorf("the named conversation's history was not continued: %s", mustJSONAny(model.history(3)))
	}
}

func TestConversationHandles(t *testing.T) {
	model := chatModel("one", "two", "three")
	h := chatServer(t, model, TransportConfig{Stateful: true}, ConversationConfig{})
	sid := openSession(t, h.url, `{}`, "")

	// A handle this store never minted names nothing: a tool-level error, so the
	// model can start over.
	reply, _, _ := h.chat(t, sid, "hello", "not-a-handle")
	if reply != "ERROR: Unknown or expired conversation handle. Omit the 'conversation' argument to start a new conversation." {
		t.Errorf("unknown handle = %q", reply)
	}
	if model.callCount() != 0 {
		t.Error("the agent ran for an unknown conversation")
	}
	// A blank handle reads as none, not as an unknown one: a model given an
	// optional string routinely sends "".
	msg, _ := rpcCall(t, h.url, map[string]string{"Mcp-Session-Id": sid},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ask","arguments":{"message":"hello","conversation":"  "}}}`)
	res, _ := checkResponseEnvelope(t, msg)
	if m := res.(map[string]any); m["isError"] == true {
		t.Errorf("a blank handle failed the call: %v", m)
	}
}

func TestConversationToolDeclaration(t *testing.T) {
	list := func(h *httptestServer, sid string) map[string]any {
		hdr := map[string]string{}
		if sid != "" {
			hdr["Mcp-Session-Id"] = sid
		}
		msg, _ := rpcCall(t, h.url, hdr, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		res, _ := checkResponseEnvelope(t, msg)
		return res.(map[string]any)["tools"].([]any)[0].(map[string]any)["inputSchema"].(map[string]any)["properties"].(map[string]any)
	}
	on := chatServer(t, chatModel("x"), TransportConfig{Stateful: true}, ConversationConfig{MaxConversations: 3, TTL: 90 * time.Second})
	props := list(on, openSession(t, on.url, `{}`, ""))
	conv, ok := props["conversation"].(map[string]any)
	if !ok {
		t.Fatalf("no conversation argument advertised: %v", props)
	}
	desc := conv["description"].(string)
	if !strings.Contains(desc, "Opaque handle naming the conversation to continue") ||
		!strings.Contains(desc, "Lifetime: a conversation is dropped after 90 seconds idle, or once it is not among the 3 most recently used.") {
		t.Errorf("description = %q", desc)
	}
	noTTL := chatServer(t, chatModel("x"), TransportConfig{Stateful: true}, ConversationConfig{MaxConversations: 5})
	d := list(noTTL, openSession(t, noTTL.url, `{}`, ""))["conversation"].(map[string]any)["description"].(string)
	if !strings.Contains(d, "Lifetime: a conversation is dropped once it is not among the 5 most recently used.") {
		t.Errorf("description without ttl = %q", d)
	}
	// Off: the argument does not exist, so none is advertised that cannot work.
	a := agent.New("served", "", agent.WithConfig(chatModel("x")))
	_, ts := serveAgent(t, a, TransportConfig{Stateful: true})
	off := &httptestServer{url: ts.URL}
	if _, has := list(off, openSession(t, off.url, `{}`, ""))["conversation"]; has {
		t.Error("a conversation argument is advertised with conversations off")
	}
}

func TestConversationBounds(t *testing.T) {
	t.Run("LRU cap evicts the least recently used idle conversation", func(t *testing.T) {
		model := chatModel("a", "b", "c", "d")
		h := chatServer(t, model, TransportConfig{Stateful: true}, ConversationConfig{MaxConversations: 2})
		s1, s2, s3 := openSession(t, h.url, `{}`, ""), openSession(t, h.url, `{}`, ""), openSession(t, h.url, `{}`, "")
		_, h1, _ := h.chat(t, s1, "one", "")
		h.chat(t, s2, "two", "")
		h.chat(t, s3, "three", "") // evicts s1's conversation
		reply, _, _ := h.chat(t, s2, "again", h1)
		if !strings.HasPrefix(reply, "ERROR: Unknown or expired conversation handle") {
			t.Errorf("evicted conversation = %q, want the unknown-handle error", reply)
		}
		if n := h.s.convs.size(); n != 2 {
			t.Errorf("%d conversations held, want the cap of 2", n)
		}
	})
	t.Run("idle TTL expires a conversation", func(t *testing.T) {
		model := chatModel("a", "b")
		h := chatServer(t, model, TransportConfig{Stateful: true}, ConversationConfig{TTL: 80 * time.Millisecond})
		s1, s2 := openSession(t, h.url, `{}`, ""), openSession(t, h.url, `{}`, "")
		_, h1, _ := h.chat(t, s1, "one", "")
		time.Sleep(200 * time.Millisecond)
		reply, _, _ := h.chat(t, s2, "two", h1)
		if !strings.HasPrefix(reply, "ERROR: Unknown or expired conversation handle") {
			t.Errorf("expired conversation = %q", reply)
		}
	})
}

func TestStatelessConversationsAreFresh(t *testing.T) {
	// With no MCP session to fall back on, every call without a handle starts a
	// conversation under a new handle (Python: SessionStore.fresh).
	model := chatModel("a", "b", "c")
	h := chatServer(t, model, TransportConfig{}, ConversationConfig{})
	rpcCall(t, h.url, nil, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`)
	_, h1, _ := h.chat(t, "", "one", "")
	_, h2, _ := h.chat(t, "", "two", "")
	if h1 == "" || h2 == "" || h1 == h2 {
		t.Fatalf("handles %q %q, want two distinct ones", h1, h2)
	}
	if historyMentions(model.history(1), "one") {
		t.Error("a handle-less stateless call saw an earlier conversation")
	}
	// The returned handle continues its conversation.
	h.chat(t, "", "three", h1)
	if !historyMentions(model.history(2), "one") {
		t.Error("the handle did not continue its conversation")
	}
}

func TestConversationsNeedAServedAgent(t *testing.T) {
	_, err := NewServer(ServerConfig{
		Ask:           func(_ context.Context, text string) (string, error) { return text, nil },
		Conversations: &ConversationConfig{},
	})
	if err == nil || !strings.Contains(err.Error(), "needs a served agent") {
		t.Errorf("NewServer = %v, want a refusal", err)
	}
}

func TestServedAgentHistoryIsPerConversationNotShared(t *testing.T) {
	// The conversation's turns run one at a time, while two conversations run
	// side by side.
	model := &scriptedModel{steps: []func(*agocontext.Context) *events.ModelResponse{say("a"), say("b")}}
	h := chatServer(t, model, TransportConfig{Stateful: true}, ConversationConfig{})
	s1, s2 := openSession(t, h.url, `{}`, ""), openSession(t, h.url, `{}`, "")
	done := make(chan string, 2)
	for _, sid := range []string{s1, s2} {
		go func() {
			r, _, _ := h.chat(t, sid, "go "+sid, "")
			done <- r
		}()
	}
	got := []string{<-done, <-done}
	if got[0] == got[1] {
		t.Errorf("both sessions got %q", got)
	}
}

func TestConversationInATurnIsNeverEvicted(t *testing.T) {
	// The cap gives way while the conversations over it are all mid-turn: a turn
	// must not lose its history to another caller's new conversation.
	gate := make(chan struct{})
	model := &scriptedModel{steps: []func(*agocontext.Context) *events.ModelResponse{
		func(*agocontext.Context) *events.ModelResponse {
			<-gate
			return &events.ModelResponse{Message: &events.ModelMessage{Content: "slow"}}
		},
		say("quick"),
	}}
	h := chatServer(t, model, TransportConfig{Stateful: true}, ConversationConfig{MaxConversations: 1})
	s1, s2 := openSession(t, h.url, `{}`, ""), openSession(t, h.url, `{}`, "")
	done := make(chan string, 1)
	go func() {
		r, _, _ := h.chat(t, s1, "long", "")
		done <- r
	}()
	waitFor(t, "the slow turn to start", func() bool { return model.callCount() == 1 })
	h.chat(t, s2, "quick", "")
	if n := h.s.convs.size(); n != 2 {
		t.Errorf("%d conversations held while one is mid-turn, want 2 (the cap gives way)", n)
	}
	close(gate)
	if got := <-done; got != "slow" {
		t.Errorf("the slow turn = %q", got)
	}
}

func TestBlankConversationHandleWithConversationsOff(t *testing.T) {
	a := agent.New("served", "", agent.WithConfig(chatModel("hi")))
	_, ts := serveAgent(t, a, TransportConfig{Stateful: true})
	sid := openSession(t, ts.URL, `{}`, "")
	msg, _ := rpcCall(t, ts.URL, map[string]string{"Mcp-Session-Id": sid},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ask","arguments":{"message":"x","conversation":"   "}}}`)
	res, _ := checkResponseEnvelope(t, msg)
	if m := res.(map[string]any); m["isError"] == true {
		t.Errorf("a blank handle is refused with conversations off: %v", m)
	}
	// A real handle is refused there, since no conversation exists to continue.
	msg, _ = rpcCall(t, ts.URL, map[string]string{"Mcp-Session-Id": sid},
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ask","arguments":{"message":"x","conversation":"abc"}}}`)
	res, _ = checkResponseEnvelope(t, msg)
	if m := res.(map[string]any); m["isError"] != true || !strings.Contains(fmt.Sprint(m["content"]), "does not maintain conversations") {
		t.Errorf("handle with conversations off = %v", m)
	}
}
