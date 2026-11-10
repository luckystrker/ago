package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/stream"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/luckystrker/ago/agent"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/hitl"
	"github.com/luckystrker/ago/tools"
)

// askHuman is the tool a served agent uses to put a question to its human.
func askHuman(t *testing.T) tools.Tool {
	t.Helper()
	fn := func(_ struct{}, cx *agocontext.Context) (string, error) {
		ans, err := cx.Input(context.Background(), "Which color?", 0)
		if err != nil {
			return "", err
		}
		return "color=" + ans, nil
	}
	tool, err := tools.NewFunctionTool("ask_human", "Asks the human a question", fn)
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

// servedAgent serves an agent running the scripted model, with the human-asking
// tool, on the given transport.
func servedAgent(t *testing.T, model *scriptedModel, tr TransportConfig, agentOpts []agent.Option, opts ...ServerOption) (*Server, *httptest.Server) {
	t.Helper()
	a := agent.New("served", "", append([]agent.Option{agent.WithConfig(model), agent.WithTools(askHuman(t))}, agentOpts...)...)
	return serveAgent(t, a, tr, opts...)
}

func serveAgent(t *testing.T, a *agent.Agent, tr TransportConfig, opts ...ServerOption) (*Server, *httptest.Server) {
	t.Helper()
	s, err := NewServerForAgent(a, append([]ServerOption{WithTransport(tr)}, opts...)...)
	if err != nil {
		t.Fatalf("NewServerForAgent: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() { s.Close(); ts.Close() })
	return s, ts
}

// turnLog is everything a streamed tools/call carried, in order.
type turnLog struct {
	messages []map[string]any // notifications and server requests, as decoded
	final    map[string]any   // the response to the call
	ids      []string         // event ids as sent
}

func (l turnLog) methods() []string {
	var out []string
	for _, m := range l.messages {
		out = append(out, fmt.Sprint(m["method"]))
	}
	return out
}

func (l turnLog) ofMethod(method string) []map[string]any {
	var out []map[string]any
	for _, m := range l.messages {
		if m["method"] == method {
			out = append(out, m)
		}
	}
	return out
}

// callTurn posts one streamed tools/call and drives it: every server-to-client
// request goes to respond, whose result is posted back as the client's reply.
func callTurn(t *testing.T, url, sid, params string, respond func(req map[string]any) any) turnLog {
	t.Helper()
	hdr := map[string]string{}
	if sid != "" {
		hdr["Mcp-Session-Id"] = sid
	}
	resp := post(t, url, hdr, `{"jsonrpc":"2.0","id":42,"method":"tools/call","params":`+params+`}`)
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("tools/call = %d %q: %s", resp.StatusCode, resp.Header.Get("Content-Type"), readAll(t, resp.Body))
	}
	var log turnLog
	for ev := range streamEvents(resp.Body) {
		if ev.Data == "" {
			// The priming cursor of a resumable stream: an id and no payload, and
			// (as the SDK sends it) no event name.
			if ev.Event != "" {
				t.Errorf("priming event is named %q", ev.Event)
			}
			log.ids = append(log.ids, ev.ID)
			continue
		}
		if ev.Event != "message" {
			t.Fatalf("event name = %q, want message", ev.Event)
		}
		msg := decodeObj(t, ev.Data)
		log.ids = append(log.ids, ev.ID)
		switch {
		case msg["method"] != nil && msg["id"] != nil: // a server-to-client request
			log.messages = append(log.messages, msg)
			if respond == nil {
				t.Fatalf("unexpected server request %v", msg)
			}
			reply := map[string]any{"jsonrpc": "2.0", "id": msg["id"], "result": respond(msg)}
			r := post(t, url, hdr, mustJSON(t, reply))
			r.Body.Close()
			if r.StatusCode != http.StatusAccepted {
				t.Fatalf("reply to %v = %d, want 202", msg["method"], r.StatusCode)
			}
		case msg["method"] != nil:
			log.messages = append(log.messages, msg)
		default:
			log.final = msg
		}
	}
	if log.final == nil {
		t.Fatal("the stream ended without the response")
	}
	return log
}

func finalText(t *testing.T, l turnLog) string {
	t.Helper()
	res, e := checkResponseEnvelope(t, l.final)
	if e != nil {
		t.Fatalf("the call failed: %v", e)
	}
	checkCallToolResult(t, res)
	blocks := res.(map[string]any)["content"].([]any)
	return blocks[0].(map[string]any)["text"].(string)
}

const callAsk = `{"name":"ask","arguments":{"message":"go"},"_meta":{"progressToken":"tok-1"}}`

func acceptBlue(req map[string]any) any {
	return map[string]any{"action": "accept", "content": map[string]any{"answer": "blue"}}
}

func TestServedAgentElicitation(t *testing.T) {
	model := &scriptedModel{steps: append([]func(*agocontext.Context) *events.ModelResponse{
		streamThenCall("ask_human", "a", "b"), say("done")}, nil)[:2]}
	_, ts := servedAgent(t, model, statefulStreaming(nil), nil)
	sid := openSession(t, ts.URL, `{"elicitation":{"form":{}}}`, "")

	log := callTurn(t, ts.URL, sid, callAsk, acceptBlue)
	if got := finalText(t, log); got != "done" {
		t.Errorf("final text = %q", got)
	}
	// The order a client sees: the agent's chunks, its tool call, the question,
	// and — after the answer — the tool result.
	want := []string{"notifications/progress", "notifications/progress", "notifications/message", "elicitation/create", "notifications/message"}
	if got := log.methods(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("methods = %v, want %v", got, want)
	}
	req := log.ofMethod("elicitation/create")[0]
	if _, isNumber := req["id"].(float64); !isNumber {
		t.Errorf("server request id = %v (%T)", req["id"], req["id"])
	}
	checkShape(t, "elicitation", req["params"], elicitFormParamsShape)
	params := req["params"].(map[string]any)
	if params["mode"] != "form" || params["message"] != "Which color?" {
		t.Errorf("params = %v", params)
	}
	if got := mustJSON(t, params["requestedSchema"]); got != `{"properties":{"answer":{"description":"Your answer to the agent's question.","title":"Answer","type":"string"}},"required":["answer"],"type":"object"}` {
		t.Errorf("requestedSchema = %s", got)
	}
	// The answer reached the tool, and through it the model.
	if !historyMentions(model.history(1), "color=blue") {
		t.Errorf("the model never saw the tool result color=blue")
	}
}

// historyMentions reports whether any event handed to the model carries text.
func historyMentions(h []events.Event, text string) bool {
	return strings.Contains(mustJSONAny(h), text)
}

func mustJSONAny(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestServedAgentElicitationFallsBackWhenTheClientCannotAnswer(t *testing.T) {
	ownHook := func(_ context.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
		return &events.HumanMessage{Content: "own"}, nil
	}
	for _, tc := range []struct {
		name   string
		caps   string
		tr     TransportConfig
		policy ElicitationPolicy
	}{
		{"no capability declared", `{}`, statefulStreaming(nil), ""},
		{"url-only capability", `{"elicitation":{"url":{}}}`, statefulStreaming(nil), ""},
		{"decline policy", `{"elicitation":{"form":{}}}`, statefulStreaming(nil), ElicitationDecline},
		{"JSON answers have no back-channel", `{"elicitation":{"form":{}}}`, TransportConfig{Stateful: true}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &scriptedModel{steps: []func(*agocontext.Context) *events.ModelResponse{streamThenCall("ask_human"), say("done")}}
			_, ts := servedAgent(t, model, tc.tr, []agent.Option{agent.WithHitlHook(ownHook)}, WithElicitationPolicy(tc.policy))
			sid := openSession(t, ts.URL, tc.caps, "")
			body := `{"name":"ask","arguments":{"message":"go"}}`
			if tc.tr.StreamResponses {
				log := callTurn(t, ts.URL, sid, body, nil) // nil: any server request fails the test
				if finalText(t, log) != "done" {
					t.Errorf("final = %v", log.final)
				}
			} else {
				msg, _ := rpcCall(t, ts.URL, map[string]string{"Mcp-Session-Id": sid}, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":`+body+`}`)
				if _, e := checkResponseEnvelope(t, msg); e != nil {
					t.Fatalf("call failed: %v", e)
				}
			}
			// The agent's own hook answered, exactly as without MCP.
			if !historyMentions(model.history(1), "color=own") {
				t.Errorf("the agent's own hook did not answer")
			}
		})
	}

	t.Run("a bare elicitation capability counts as form", func(t *testing.T) {
		model := &scriptedModel{steps: []func(*agocontext.Context) *events.ModelResponse{streamThenCall("ask_human"), say("done")}}
		_, ts := servedAgent(t, model, statefulStreaming(nil), nil)
		sid := openSession(t, ts.URL, `{"elicitation":{}}`, "")
		log := callTurn(t, ts.URL, sid, `{"name":"ask","arguments":{"message":"go"}}`, acceptBlue)
		if len(log.ofMethod("elicitation/create")) != 1 {
			t.Errorf("methods = %v, want one elicitation", log.methods())
		}
	})

	t.Run("without any channel the turn fails as a tool-level error", func(t *testing.T) {
		// Python parity: the absent channel is a HumanInputNotProvidedError out of
		// the agent's ask, which the executor flattens into an isError result.
		model := &scriptedModel{steps: []func(*agocontext.Context) *events.ModelResponse{streamThenCall("ask_human"), say("done")}}
		_, ts := servedAgent(t, model, statefulStreaming(nil), nil)
		sid := openSession(t, ts.URL, `{}`, "")
		log := callTurn(t, ts.URL, sid, `{"name":"ask","arguments":{"message":"go"}}`, nil)
		res, e := checkResponseEnvelope(t, log.final)
		if e != nil {
			t.Fatalf("a missing channel is a tool result, got %v", e)
		}
		if m := res.(map[string]any); m["isError"] != true || !strings.Contains(fmt.Sprint(m["content"]), "no human input hook") {
			t.Errorf("result = %v", m)
		}
		if n := len(log.ofMethod("elicitation/create")); n != 0 {
			t.Errorf("%d elicitation requests without the capability", n)
		}
	})
}

func TestServedAgentElicitationRefusals(t *testing.T) {
	// A refusal is an absent answer (hitl.ErrNotProvided) and fails the turn the
	// way an absent channel does: a tool-level error result naming the action,
	// never a made-up answer and never a JSON-RPC error.
	for _, tc := range []struct {
		name   string
		answer any
		action string
	}{
		{"declined", map[string]any{"action": "decline"}, `"decline"`},
		{"cancelled", map[string]any{"action": "cancel"}, `"cancel"`},
		{"accepted without the answer", map[string]any{"action": "accept", "content": map[string]any{"other": "x"}}, `"accept"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &scriptedModel{steps: []func(*agocontext.Context) *events.ModelResponse{streamThenCall("ask_human"), say("done")}}
			_, ts := servedAgent(t, model, statefulStreaming(nil), nil)
			sid := openSession(t, ts.URL, `{"elicitation":{"form":{}}}`, "")
			log := callTurn(t, ts.URL, sid, `{"name":"ask","arguments":{"message":"go"}}`, func(map[string]any) any { return tc.answer })
			res, e := checkResponseEnvelope(t, log.final)
			if e != nil {
				t.Fatalf("a refusal is a tool result, got %v", e)
			}
			m := res.(map[string]any)
			if m["isError"] != true || !strings.Contains(fmt.Sprint(m["content"]), "answered the agent's question with "+tc.action) {
				t.Errorf("result = %v", m)
			}
			if model.callCount() != 1 {
				t.Errorf("the model was called %d times; a refused question must stop the turn", model.callCount())
			}
		})
	}
}

func TestElicitationDeclinedErrorIsAnAbsentChannel(t *testing.T) {
	var err error = &ElicitationDeclinedError{Action: "decline"}
	if !strings.Contains(err.Error(), `"decline"`) {
		t.Errorf("message = %q", err)
	}
	if !errorsIs(err, hitl.ErrNotProvided) {
		t.Error("a refusal must read as hitl.ErrNotProvided, as in Python")
	}
}

func TestServedAgentSampling(t *testing.T) {
	sample := map[string]any{"role": "assistant", "content": map[string]any{"type": "text", "text": "borrowed answer"}, "model": "client-model", "stopReason": "endTurn"}
	t.Run("the agent's turn runs on the client's model", func(t *testing.T) {
		a := agent.New("served", "Answer in French.") // no model of its own
		_, ts := serveAgent(t, a, statefulStreaming(nil), WithClientModel())
		sid := openSession(t, ts.URL, `{"sampling":{}}`, "")
		log := callTurn(t, ts.URL, sid, `{"name":"ask","arguments":{"message":"hello there"}}`, func(map[string]any) any { return sample })
		if got := finalText(t, log); got != "borrowed answer" {
			t.Errorf("final text = %q", got)
		}
		req := log.ofMethod("sampling/createMessage")[0]
		checkShape(t, "sampling", req["params"], samplingParamsShape)
		params := req["params"].(map[string]any)
		if params["maxTokens"] != 4096.0 || params["systemPrompt"] != "Answer in French." {
			t.Errorf("params = %v", params)
		}
		if got := mustJSON(t, params["messages"]); got != `[{"content":{"text":"hello there","type":"text"},"role":"user"}]` {
			t.Errorf("messages = %s", got)
		}
		// Only maxTokens is sent of the generation parameters: a borrowed model
		// has no configuration to take the rest from.
		for _, k := range []string{"temperature", "stopSequences", "modelPreferences", "tools"} {
			if _, has := params[k]; has {
				t.Errorf("the sampling request carries %q", k)
			}
		}
	})
	t.Run("a client that cannot sample is served on the agent's own model", func(t *testing.T) {
		model := &scriptedModel{steps: []func(*agocontext.Context) *events.ModelResponse{say("own model")}}
		a := agent.New("served", "", agent.WithConfig(model))
		_, ts := serveAgent(t, a, statefulStreaming(nil), WithClientModel())
		sid := openSession(t, ts.URL, `{}`, "")
		log := callTurn(t, ts.URL, sid, `{"name":"ask","arguments":{"message":"hi"}}`, nil)
		if got := finalText(t, log); got != "own model" {
			t.Errorf("final text = %q", got)
		}
	})
	t.Run("without capability and without a model of its own the call is refused with -32021", func(t *testing.T) {
		a := agent.New("served", "")
		_, ts := serveAgent(t, a, statefulStreaming(nil), WithClientModel())
		sid := openSession(t, ts.URL, `{}`, "")
		log := callTurn(t, ts.URL, sid, `{"name":"ask","arguments":{"message":"hi"}}`, nil)
		_, e := checkResponseEnvelope(t, log.final)
		if e == nil || e["code"] != -32021.0 {
			t.Fatalf("response = %v, want error -32021", log.final)
		}
		if mustJSON(t, e["data"]) != `{"requiredCapabilities":{"sampling":{}}}` {
			t.Errorf("data = %v", e["data"])
		}
	})
	t.Run("off unless the operator turned it on", func(t *testing.T) {
		model := &scriptedModel{steps: []func(*agocontext.Context) *events.ModelResponse{say("own model")}}
		a := agent.New("served", "", agent.WithConfig(model))
		_, ts := serveAgent(t, a, statefulStreaming(nil))
		sid := openSession(t, ts.URL, `{"sampling":{}}`, "")
		log := callTurn(t, ts.URL, sid, `{"name":"ask","arguments":{"message":"hi"}}`, nil)
		if got := finalText(t, log); got != "own model" {
			t.Errorf("final text = %q", got)
		}
	})
	t.Run("an agent with tools cannot borrow a model that carries none", func(t *testing.T) {
		a := agent.New("served", "", agent.WithTools(askHuman(t)))
		_, ts := serveAgent(t, a, statefulStreaming(nil), WithClientModel())
		sid := openSession(t, ts.URL, `{"sampling":{}}`, "")
		log := callTurn(t, ts.URL, sid, `{"name":"ask","arguments":{"message":"hi"}}`, func(map[string]any) any { return sample })
		res, e := checkResponseEnvelope(t, log.final)
		if e != nil {
			t.Fatalf("a refusal is a tool result, got %v", e)
		}
		if m := res.(map[string]any); m["isError"] != true || !strings.Contains(fmt.Sprint(m["content"]), "cannot borrow the calling client's model") {
			t.Errorf("result = %v", m)
		}
		if n := len(log.ofMethod("sampling/createMessage")); n != 0 {
			t.Errorf("%d sampling requests were sent for a turn that cannot use them", n)
		}
	})
	t.Run("a completion with no text is refused", func(t *testing.T) {
		a := agent.New("served", "")
		_, ts := serveAgent(t, a, statefulStreaming(nil), WithClientModel())
		sid := openSession(t, ts.URL, `{"sampling":{}}`, "")
		img := map[string]any{"role": "assistant", "content": map[string]any{"type": "image", "data": "AA==", "mimeType": "image/png"}, "model": "m"}
		log := callTurn(t, ts.URL, sid, `{"name":"ask","arguments":{"message":"hi"}}`, func(map[string]any) any { return img })
		res, _ := checkResponseEnvelope(t, log.final)
		if m, _ := res.(map[string]any); m == nil || m["isError"] != true || !strings.Contains(fmt.Sprint(m["content"]), "carried no text") {
			t.Errorf("result = %v", res)
		}
	})
}

func TestServedAgentProgressAndLogNotifications(t *testing.T) {
	newModel := func() *scriptedModel {
		return &scriptedModel{steps: []func(*agocontext.Context) *events.ModelResponse{
			streamThenCall("ask_human", "one", "two", "three"), streamThenSay("done", "fin")}}
	}
	t.Run("in order, on the request's own stream, with the caller's token", func(t *testing.T) {
		_, ts := servedAgent(t, newModel(), statefulStreaming(nil), nil)
		sid := openSession(t, ts.URL, `{"elicitation":{"form":{}}}`, "")
		log := callTurn(t, ts.URL, sid, callAsk, acceptBlue)
		var progress []map[string]any
		for _, m := range log.ofMethod("notifications/progress") {
			if _, hasID := m["id"]; hasID {
				t.Errorf("a notification carries an id: %v", m)
			}
			checkShape(t, "progress", m["params"], progressParamsShape)
			progress = append(progress, m["params"].(map[string]any))
		}
		if len(progress) != 4 {
			t.Fatalf("%d progress notifications, want 4 (three chunks, one after the answer)", len(progress))
		}
		for i, want := range []string{"one", "two", "three", "fin"} {
			if progress[i]["message"] != want || progress[i]["progress"] != float64(i+1) || progress[i]["progressToken"] != "tok-1" {
				t.Errorf("progress[%d] = %v, want message %q progress %d token tok-1", i, progress[i], want, i+1)
			}
		}
		// Tool call and result are info-level log messages, call before result.
		var logs []string
		for _, m := range log.ofMethod("notifications/message") {
			checkShape(t, "message", m["params"], messageParamsShape)
			p := m["params"].(map[string]any)
			if p["level"] != "info" || p["logger"] != "ag2.mcp" {
				t.Errorf("log params = %v", p)
			}
			logs = append(logs, fmt.Sprint(p["data"]))
		}
		if strings.Join(logs, "|") != "tool call: ask_human|tool result: ask_human" {
			t.Errorf("logs = %v", logs)
		}
		// Notifications precede the response, which closes the stream.
		if log.final == nil || len(log.messages) == 0 {
			t.Fatal("no turn")
		}
	})
	t.Run("an integer token is echoed as an integer", func(t *testing.T) {
		_, ts := servedAgent(t, newModel(), statefulStreaming(nil), nil)
		sid := openSession(t, ts.URL, `{"elicitation":{"form":{}}}`, "")
		log := callTurn(t, ts.URL, sid, `{"name":"ask","arguments":{"message":"go"},"_meta":{"progressToken":7}}`, acceptBlue)
		if got := log.ofMethod("notifications/progress")[0]["params"].(map[string]any)["progressToken"]; got != 7.0 {
			t.Errorf("token = %v", got)
		}
	})
	t.Run("no token: log messages only", func(t *testing.T) {
		_, ts := servedAgent(t, newModel(), statefulStreaming(nil), nil)
		sid := openSession(t, ts.URL, `{"elicitation":{"form":{}}}`, "")
		log := callTurn(t, ts.URL, sid, `{"name":"ask","arguments":{"message":"go"}}`, acceptBlue)
		if n := len(log.ofMethod("notifications/progress")); n != 0 {
			t.Errorf("%d progress notifications without a progressToken", n)
		}
		if n := len(log.ofMethod("notifications/message")); n != 2 {
			t.Errorf("%d log messages, want 2", n)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		_, ts := servedAgent(t, newModel(), statefulStreaming(nil), nil, WithoutProgress())
		sid := openSession(t, ts.URL, `{"elicitation":{"form":{}}}`, "")
		log := callTurn(t, ts.URL, sid, callAsk, acceptBlue)
		if got := log.methods(); len(got) != 1 || got[0] != "elicitation/create" {
			t.Errorf("methods = %v, want only the question", got)
		}
	})
	t.Run("JSON answers carry only the response", func(t *testing.T) {
		model := &scriptedModel{steps: []func(*agocontext.Context) *events.ModelResponse{streamThenSay("done", "a", "b")}}
		a := agent.New("served", "", agent.WithConfig(model))
		_, ts := serveAgent(t, a, TransportConfig{Stateful: true})
		sid := openSession(t, ts.URL, `{}`, "")
		resp := post(t, ts.URL, map[string]string{"Mcp-Session-Id": sid}, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":`+callAsk+`}`)
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("Content-Type = %q", ct)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(readAll(t, resp.Body)), &m); err != nil || m["result"] == nil {
			t.Errorf("body = %v (%v)", m, err)
		}
	})
}

func TestClientModelPublishesAndReportsTheCompletion(t *testing.T) {
	// One borrowed completion: its text is published on the conversation like any
	// model reply, and the response names the model and finish reason the client
	// gave (or "unknown" when it gave none).
	run := func(t *testing.T, result map[string]any) (*events.ModelResponse, []events.Event) {
		t.Helper()
		sess := newSession(context.Background())
		rec := httptest.NewRecorder()
		ex := &exchange{sess: sess, sse: true, canRequest: true}
		ex.attach(rec)
		cx := agocontext.New(stream.NewMemoryStream())
		var mu sync.Mutex
		var published []events.Event
		cx.Stream.Subscribe(func(ev events.Event) error {
			mu.Lock()
			published = append(published, ev)
			mu.Unlock()
			return nil
		}, stream.SubscribeOptions{})
		type out struct {
			resp *events.ModelResponse
			err  error
		}
		done := make(chan out, 1)
		go func() {
			r, err := clientModelClient{ex: ex}.Call(context.Background(),
				[]events.Event{&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "hi"}}}}, cx, config.CallOptions{})
			done <- out{r, err}
		}()
		waitFor(t, "the sampling request", func() bool {
			sess.mu.Lock()
			defer sess.mu.Unlock()
			return len(sess.pending) == 1
		})
		raw, _ := json.Marshal(result)
		sess.deliver("1", wireReply{result: raw})
		o := <-done
		if o.err != nil {
			t.Fatalf("Call: %v", o.err)
		}
		mu.Lock()
		defer mu.Unlock()
		return o.resp, append([]events.Event(nil), published...)
	}
	text := func(ev []events.Event) []string {
		var out []string
		for _, e := range ev {
			if m, ok := events.AsValue(e).(events.ModelMessage); ok {
				out = append(out, m.Content)
			}
		}
		return out
	}
	resp, published := run(t, map[string]any{"role": "assistant", "content": map[string]any{"type": "text", "text": "borrowed"}, "model": "client-model", "stopReason": "endTurn"})
	if resp.Model != "client-model" || resp.FinishReason != "endTurn" || resp.Content() != "borrowed" {
		t.Errorf("response = model %q finish %q content %q", resp.Model, resp.FinishReason, resp.Content())
	}
	if got := text(published); len(got) != 1 || got[0] != "borrowed" {
		t.Errorf("published model messages = %v, want the completion once", got)
	}
	resp, _ = run(t, map[string]any{"role": "assistant", "content": map[string]any{"type": "text", "text": "x"}})
	if resp.Model != "unknown" {
		t.Errorf("a completion naming no model is reported as %q, want unknown", resp.Model)
	}
}

func TestCompletionTextRefusals(t *testing.T) {
	for _, raw := range []string{``, `null`, `{}`, `[]`, `{"type":"image","data":"AA==","mimeType":"image/png"}`} {
		if _, err := completionText(json.RawMessage(raw)); !errors.Is(err, ErrSamplingRefused) {
			t.Errorf("completionText(%q) = %v, want ErrSamplingRefused (and no panic)", raw, err)
		}
	}
	got, err := completionText(json.RawMessage(`[{"type":"text","text":"a"},{"type":"image"},{"type":"text","text":"b"}]`))
	if err != nil || got != "ab" {
		t.Errorf("array content = %q (%v), want the text blocks joined", got, err)
	}
	if got, err := completionText(json.RawMessage(`{"type":"text","text":""}`)); err != nil || got != "" {
		t.Errorf("an empty text block = %q (%v): a model may say nothing", got, err)
	}
}
