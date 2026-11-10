package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luckystrker/ago/agent"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/mcp"
	"github.com/luckystrker/ago/stream"
)

// The end-to-end tests run AGo's own MCP client against AGo's own MCP server over
// streamable HTTP: the two roles of a real deployment, over a real socket.

type e2eNotes struct {
	mu  sync.Mutex
	got []mcp.Notification
}

func (n *e2eNotes) add(x mcp.Notification) {
	n.mu.Lock()
	n.got = append(n.got, x)
	n.mu.Unlock()
}

func (n *e2eNotes) all() []mcp.Notification {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]mcp.Notification(nil), n.got...)
}

// progressMsgs are the messages of the progress notifications received, in order,
// with the progress values.
func (n *e2eNotes) progress(t *testing.T) (msgs []string, values []float64) {
	t.Helper()
	for _, x := range n.all() {
		if x.Method != "notifications/progress" {
			continue
		}
		var p struct {
			Token    any     `json:"progressToken"`
			Progress float64 `json:"progress"`
			Message  string  `json:"message"`
		}
		if err := json.Unmarshal(x.Params, &p); err != nil {
			t.Fatalf("progress params %s: %v", x.Params, err)
		}
		msgs = append(msgs, p.Message)
		values = append(values, p.Progress)
	}
	return
}

func (n *e2eNotes) methods() []string {
	var out []string
	for _, x := range n.all() {
		out = append(out, x.Method)
	}
	return out
}

// e2eFixture is a served agent plus the registrations the round trips exercise.
func e2eFixture(t *testing.T, model *scriptedModel, store EventStore, wrap func(http.Handler) http.Handler, agentOpts ...agent.Option) (*Server, string) {
	t.Helper()
	a := agent.New("served", "", append([]agent.Option{agent.WithConfig(model), agent.WithTools(askHuman(t))}, agentOpts...)...)
	s, err := NewServerForAgent(a,
		WithTransport(TransportConfig{Stateful: true, StreamResponses: true, EventStore: store}),
		WithConversations(ConversationConfig{}),
		WithResources(Resource{URI: "mem://notes", Name: "notes", MIMEType: "text/markdown",
			Read: func(context.Context, *agocontext.Context) (ResourceContent, error) {
				return ResourceContent{Text: "# notes"}, nil
			}}),
		WithResourceTemplates(ResourceTemplate{URITemplate: "weather://{city}", Name: "weather",
			Read: func(_ context.Context, v map[string]string) (ResourceContent, error) {
				return ResourceContent{Text: "sunny in " + v["city"]}, nil
			}}),
		WithPrompts(Prompt{Name: "greet", Description: "Greets", Arguments: []PromptArgument{{Name: "who", Required: true}},
			Render: func(_ context.Context, a map[string]string) ([]PromptMessage, error) {
				return UserPrompt("Hello " + a["who"]), nil
			}}),
	)
	if err != nil {
		t.Fatalf("NewServerForAgent: %v", err)
	}
	var h http.Handler = s.Handler()
	if wrap != nil {
		h = wrap(h)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(func() { s.Close(); ts.Close() })
	return s, ts.URL
}

// humanCx is a conversation whose human answers every question with answer.
func humanCx(answer string, asked *[]string) *agocontext.Context {
	cx := agocontext.New(stream.NewMemoryStream())
	var mu sync.Mutex
	cx.SetHitlHook(func(_ context.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
		mu.Lock()
		if asked != nil {
			*asked = append(*asked, req.Content)
		}
		mu.Unlock()
		return &events.HumanMessage{Content: answer}, nil
	})
	return cx
}

func newClient(t *testing.T, url string, notes *e2eNotes, policy mcp.AnswerPolicy) *mcp.Client {
	t.Helper()
	opts := []mcp.Option{mcp.WithAnswering(policy)}
	if notes != nil {
		opts = append(opts, mcp.WithNotifications(notes.add))
	}
	cl, err := mcp.New(&mcp.MCPServerConfig{URL: url, Timeout: 20 * time.Second}, opts...)
	if err != nil {
		t.Fatalf("mcp.New: %v", err)
	}
	t.Cleanup(func() { cl.Close() })
	return cl
}

func textOf(t *testing.T, out mcp.ToolOutcome) string {
	t.Helper()
	if len(out.Parts) == 0 {
		t.Fatal("empty tool result")
	}
	if ti, ok := events.AsValue(out.Parts[0]).(events.TextInput); ok {
		return ti.Content
	}
	t.Fatalf("part %#v is not text", out.Parts[0])
	return ""
}

var askPolicy = mcp.AnswerPolicy{Elicitation: mcp.ElicitationAsk}

func TestE2EAgentToolAsksTheClientsHuman(t *testing.T) {
	model := &scriptedModel{steps: []func(*agocontext.Context) *events.ModelResponse{
		streamThenCall("ask_human", "a", "b", "c"), say("picked blue")}}
	_, url := e2eFixture(t, model, NewMemoryEventStore(0), nil)
	var notes e2eNotes
	cl := newClient(t, url, &notes, askPolicy)
	var asked []string

	out, err := cl.CallToolMeta(context.Background(), humanCx("blue", &asked), "ask", map[string]any{"message": "pick a color"},
		map[string]any{"progressToken": "p-1"})
	if err != nil {
		t.Fatalf("CallToolMeta: %v", err)
	}
	// The elicitation reached the client's HITL hook, the answer went back, the
	// tool used it and the agent finished.
	if len(asked) != 1 || asked[0] != "Which color?" {
		t.Errorf("the client's human was asked %q", asked)
	}
	if got := textOf(t, out); got != "picked blue" {
		t.Errorf("result = %q", got)
	}
	if !historyMentions(model.history(1), "color=blue") {
		t.Errorf("the agent's tool never saw the human's answer")
	}
	// Progress arrived in order, then the log messages, all before the result.
	msgs, values := notes.progress(t)
	if strings.Join(msgs, ",") != "a,b,c" || len(values) != 3 || values[0] != 1 || values[1] != 2 || values[2] != 3 {
		t.Errorf("progress = %v %v, want a,b,c with 1,2,3", msgs, values)
	}
	if got := strings.Join(notes.methods(), ","); got != "notifications/progress,notifications/progress,notifications/progress,notifications/message,notifications/message" {
		t.Errorf("notification order = %s", got)
	}
}

func TestE2EResourcesAndPromptsRoundTrip(t *testing.T) {
	_, url := e2eFixture(t, &scriptedModel{}, nil, nil)
	cl := newClient(t, url, nil, mcp.AnswerPolicy{})
	ctx := context.Background()
	req := func(method string, params map[string]any) map[string]any {
		t.Helper()
		raw, err := cl.Request(ctx, method, params)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s result %s: %v", method, raw, err)
		}
		return out
	}

	list := req("resources/list", map[string]any{})["resources"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["uri"] != "mem://notes" {
		t.Errorf("resources = %v", list)
	}
	read := req("resources/read", map[string]any{"uri": "mem://notes"})["contents"].([]any)[0].(map[string]any)
	if read["text"] != "# notes" || read["mimeType"] != "text/markdown" {
		t.Errorf("read = %v", read)
	}
	tpl := req("resources/templates/list", map[string]any{})["resourceTemplates"].([]any)
	if len(tpl) != 1 || tpl[0].(map[string]any)["uriTemplate"] != "weather://{city}" {
		t.Errorf("templates = %v", tpl)
	}
	read = req("resources/read", map[string]any{"uri": "weather://Lima"})["contents"].([]any)[0].(map[string]any)
	if read["text"] != "sunny in Lima" {
		t.Errorf("template read = %v", read)
	}
	if _, err := cl.Request(ctx, "resources/read", map[string]any{"uri": "mem://nope"}); !errors.Is(err, mcp.ErrProtocol) ||
		!strings.Contains(err.Error(), "-32602") || !strings.Contains(err.Error(), "No resource matches URI 'mem://nope'.") {
		t.Errorf("unknown resource = %v", err)
	}

	prompts := req("prompts/list", map[string]any{})["prompts"].([]any)
	if len(prompts) != 1 || prompts[0].(map[string]any)["name"] != "greet" {
		t.Errorf("prompts = %v", prompts)
	}
	got := req("prompts/get", map[string]any{"name": "greet", "arguments": map[string]any{"who": "Ada"}})
	msgs := got["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" ||
		msgs[0].(map[string]any)["content"].(map[string]any)["text"] != "Hello Ada" {
		t.Errorf("prompt = %v", got)
	}
	if _, err := cl.Request(ctx, "prompts/get", map[string]any{"name": "greet"}); err == nil ||
		!strings.Contains(err.Error(), "Missing required arguments for prompt 'greet': who") {
		t.Errorf("missing argument = %v", err)
	}
}

func TestE2EInvalidArgumentIsAToolErrorWithPythonsMessage(t *testing.T) {
	model := &scriptedModel{steps: []func(*agocontext.Context) *events.ModelResponse{say("never")}}
	_, url := e2eFixture(t, model, nil, nil)
	cl := newClient(t, url, nil, mcp.AnswerPolicy{})
	_, err := cl.CallTool("ask", map[string]any{"message": 5})
	if !errors.Is(err, mcp.ErrToolCall) || !strings.Contains(err.Error(), "Input validation error: 5 is not of type 'string'") {
		t.Errorf("err = %v, want the validation error as a tool failure", err)
	}
	_, err = cl.CallTool("ask", nil)
	if !errors.Is(err, mcp.ErrToolCall) || !strings.Contains(err.Error(), "Input validation error: 'message' is a required property") {
		t.Errorf("err = %v", err)
	}
	if model.callCount() != 0 {
		t.Error("the agent ran on invalid arguments")
	}
}

// cutAfterProgress hangs up a tools/call stream right after its first progress
// event, once: the connection the "client" loses mid-turn.
type cutWriter struct {
	http.ResponseWriter
	once     *sync.Once
	cut      atomic.Bool
	hijacked *atomic.Int32 // shared: how many connections were really torn down
	failed   *atomic.Int32 // shared: how many hijack attempts failed
}

func (c *cutWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

func (c *cutWriter) Write(b []byte) (int, error) {
	if c.cut.Load() {
		return 0, http.ErrHijacked
	}
	n, err := c.ResponseWriter.Write(b)
	if strings.Contains(string(b), "notifications/progress") {
		c.once.Do(func() {
			rc := http.NewResponseController(c.ResponseWriter)
			_ = rc.Flush()
			if conn, _, herr := rc.Hijack(); herr == nil {
				conn.Close()
				c.hijacked.Add(1)
			} else {
				c.failed.Add(1)
			}
			c.cut.Store(true)
		})
	}
	return n, err
}

func (c *cutWriter) Flush() {
	if !c.cut.Load() {
		_ = http.NewResponseController(c.ResponseWriter).Flush()
	}
}

func TestE2EDisconnectThenResumeReplaysTheMissedEvents(t *testing.T) {
	gate := make(chan struct{})
	var resumeSeen atomic.Int32
	model := &scriptedModel{steps: []func(*agocontext.Context) *events.ModelResponse{
		func(conv *agocontext.Context) *events.ModelResponse {
			_ = conv.Send(&events.ModelMessageChunk{Content: "a"})
			<-gate // the turn is still running when the client reconnects
			_ = conv.Send(&events.ModelMessageChunk{Content: "b"})
			_ = conv.Send(&events.ModelMessageChunk{Content: "c"})
			return &events.ModelResponse{Message: &events.ModelMessage{Content: "all done"}}
		}}}
	var once sync.Once
	var hijacked, failed atomic.Int32
	wrap := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.Header.Get("Last-Event-ID") != "":
				if resumeSeen.Add(1) == 1 {
					go func() { time.Sleep(50 * time.Millisecond); close(gate) }() // after the resume attached
				}
			case r.Method == http.MethodPost && strings.Contains(r.Header.Get("Accept"), "text/event-stream"):
				w = &cutWriter{ResponseWriter: w, once: &once, hijacked: &hijacked, failed: &failed}
			}
			h.ServeHTTP(w, r)
		})
	}
	_, url := e2eFixture(t, model, NewMemoryEventStore(0), wrap)
	var notes e2eNotes
	cl := newClient(t, url, &notes, mcp.AnswerPolicy{})

	out, err := cl.CallToolMeta(context.Background(), nil, "ask", map[string]any{"message": "go"}, map[string]any{"progressToken": "r"})
	if err != nil {
		t.Fatalf("CallToolMeta across a dropped connection: %v", err)
	}
	if got := textOf(t, out); got != "all done" {
		t.Errorf("result = %q", got)
	}
	// A cut that did not happen would leave this test proving nothing (and, with a
	// connection left open, hanging until the client's own timeout).
	if hijacked.Load() != 1 || failed.Load() != 0 {
		t.Fatalf("the connection cut did not happen as intended: %d torn down, %d hijacks failed", hijacked.Load(), failed.Load())
	}
	if resumeSeen.Load() < 1 {
		t.Fatal("the client never resumed with Last-Event-ID")
	}
	// Chunk a came before the drop, b and c after the resume; nothing lost,
	// nothing twice, order kept.
	msgs, values := notes.progress(t)
	if strings.Join(msgs, ",") != "a,b,c" || values[0] != 1 || values[2] != 3 {
		t.Errorf("progress = %v %v, want a,b,c exactly once each", msgs, values)
	}
}

func TestE2ETwoSessionsAreIsolated(t *testing.T) {
	ownHook := func(_ context.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
		return &events.HumanMessage{Content: "own"}, nil
	}
	model := &scriptedModel{steps: []func(*agocontext.Context) *events.ModelResponse{
		streamThenCall("ask_human"), say("A one"), // session A, first call
		streamThenCall("ask_human"), say("B one"), // session B, first call
		say("A two"), // session A, second call
	}}
	s, url := e2eFixture(t, model, nil, nil, agent.WithHitlHook(ownHook))
	a := newClient(t, url, nil, askPolicy) // A can answer questions
	b := newClient(t, url, nil, mcp.AnswerPolicy{})

	if _, err := a.CallToolWith(context.Background(), humanCx("red", nil), "ask", map[string]any{"message": "alpha"}); err != nil {
		t.Fatalf("A: %v", err)
	}
	if _, err := b.CallToolWith(context.Background(), nil, "ask", map[string]any{"message": "beta"}); err != nil {
		t.Fatalf("B: %v", err)
	}
	if n := s.sessions.count(); n != 2 {
		t.Fatalf("%d MCP sessions, want 2", n)
	}
	// A's question went to A's human; B, which declared no capability, was served
	// by the agent's own hook. Neither saw the other's answer.
	if !historyMentions(model.history(1), "color=red") {
		t.Error("A's tool did not get A's human's answer")
	}
	if !historyMentions(model.history(3), "color=own") || historyMentions(model.history(3), "color=red") {
		t.Errorf("B's tool saw %s", mustJSONAny(model.history(3)))
	}
	// Each session keeps its own conversation: B's turn knows nothing of alpha,
	// and A's second turn remembers alpha but not beta.
	if historyMentions(model.history(2), "alpha") {
		t.Error("session B's conversation contains session A's message")
	}
	if _, err := a.CallToolWith(context.Background(), humanCx("red", nil), "ask", map[string]any{"message": "again"}); err != nil {
		t.Fatalf("A again: %v", err)
	}
	h := model.history(4)
	if !historyMentions(h, "alpha") || historyMentions(h, "beta") {
		t.Errorf("A's second turn saw %s", mustJSONAny(h))
	}
	// Ending one session leaves the other working.
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "A's session to end", func() bool { return s.sessions.count() == 1 })
	if _, err := b.Request(context.Background(), "ping", map[string]any{}); err != nil {
		t.Errorf("B after A closed: %v", err)
	}
}
