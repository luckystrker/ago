package agui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ago/agent"
	"ago/config"
	agocontext "ago/context"
	"ago/events"
	"ago/internal/sse"
	agotest "ago/testing"
)

// postRun drives one POST and decodes the SSE frames the way a client does.
func postRun(t *testing.T, h http.Handler, body string) ([]Event, [][]byte) {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q, want text/event-stream", ct)
	}
	var (
		evs []Event
		raw [][]byte
	)
	sc := sse.NewScanner(resp.Body)
	for sc.Next() {
		_, data := sc.Event()
		ev, err := UnmarshalEvent(data)
		if err != nil {
			t.Fatalf("frame %q: %v", data, err)
		}
		evs = append(evs, ev)
		raw = append(raw, data)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("sse: %v", err)
	}
	return evs, raw
}

const runBody = `{"threadId":"t1","runId":"r1","messages":[{"id":"m1","role":"user","content":"hi"}]}`

func servedAgent(cfg config.ModelConfig) *agent.Agent {
	return agent.New("served", "You are served.", agent.WithConfig(cfg))
}

func TestGETCapabilities(t *testing.T) {
	ts := httptest.NewServer(ServeAgent(servedAgent(agotest.NewTestConfig())))
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var caps AgentCapabilities
	if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if caps.Identity == nil || caps.Identity.Name != "served" || caps.Identity.Type != "ago" {
		t.Fatalf("identity = %+v", caps.Identity)
	}
	if caps.Transport == nil || caps.Transport.Streaming == nil || !*caps.Transport.Streaming {
		t.Fatalf("transport = %+v, want streaming", caps.Transport)
	}
	if caps.Tools == nil || caps.Tools.Supported == nil || !*caps.Tools.Supported {
		t.Fatalf("tools = %+v, want supported", caps.Tools)
	}
	// The sections AGo cannot read off the agent stay undeclared rather than
	// declared wrong.
	if caps.State != nil {
		t.Fatalf("state declared, want omitted: %+v", caps.State)
	}
	if caps.MultiAgent != nil {
		t.Fatalf("multiAgent declared, want omitted: %+v", caps.MultiAgent)
	}
	if caps.Reasoning != nil {
		t.Fatalf("reasoning declared, want omitted: %+v", caps.Reasoning)
	}
	if caps.Multimodal != nil {
		t.Fatalf("multimodal declared, want omitted: %+v", caps.Multimodal)
	}
	// camelCase on the wire.
	b, _ := json.Marshal(caps)
	for _, key := range []string{`"identity"`, `"transport"`, `"humanInTheLoop"`} {
		if !strings.Contains(string(b), key) {
			t.Fatalf("wire form missing %s: %s", key, b)
		}
	}
}

// A scripted model whose answer reaches the projection as provider-style chunks.
type chunkyConfig struct{ inner *agotest.TestConfig }

func (c chunkyConfig) Provider() config.Provider                          { return c.inner.Provider() }
func (c chunkyConfig) Model() string                                      { return c.inner.Model() }
func (c chunkyConfig) Clone(m map[string]any) (config.ModelConfig, error) { return c.inner.Clone(m) }
func (c chunkyConfig) Create() (config.LLMClient, error)                  { return chunkyClient{c.inner}, nil }

type chunkyClient struct{ inner *agotest.TestConfig }

func (c chunkyClient) Call(ctx context.Context, messages []events.Event, conv *agocontext.Context, opts config.CallOptions) (*events.ModelResponse, error) {
	resp, err := c.inner.Call(ctx, messages, conv, opts)
	if err != nil {
		return nil, err
	}
	if text := resp.Content(); text != "" {
		for _, chunk := range strings.SplitAfter(text, "") {
			if err := conv.Send(&events.ModelMessageChunk{Content: chunk}); err != nil {
				return nil, err
			}
		}
	}
	return resp, nil
}

func TestPOSTStreamsTextMessage(t *testing.T) {
	cfg := chunkyConfig{inner: agotest.NewTestConfig(
		&events.ModelResponse{Message: &events.ModelMessage{Content: "Hello AG-UI"}},
	)}
	evs, raw := postRun(t, ServeAgent(servedAgent(cfg)), runBody)

	// The sequence the contract names: RUN_STARTED → TEXT_MESSAGE_* → RUN_FINISHED.
	got := eventTypes(evs)
	if got[0] != EventRunStarted || got[1] != EventTextMessageStart {
		t.Fatalf("opening events = %v", got[:2])
	}
	if n := len(got); got[n-2] != EventTextMessageEnd || got[n-1] != EventRunFinished {
		t.Fatalf("closing events = %v", got[n-2:])
	}
	for _, k := range got[2 : len(got)-2] {
		if k != EventTextMessageContent {
			t.Fatalf("middle event %q, want TEXT_MESSAGE_CONTENT", k)
		}
	}
	if len(got) != 4+len("Hello AG-UI") {
		t.Fatalf("%d events, want one content event per character", len(got))
	}

	start := evs[0].(RunStartedEvent)
	if start.ThreadID != "t1" || start.RunID != "r1" || start.ProtocolVersion != ProtocolVersion {
		t.Fatalf("run started = %+v", start)
	}
	id := evs[1].(TextMessageStartEvent).MessageID
	if id == "" {
		t.Fatal("empty message id")
	}
	var text strings.Builder
	for _, e := range evs[2 : len(evs)-2] {
		c := e.(TextMessageContentEvent)
		if c.MessageID != id {
			t.Fatalf("content on %q, want %q", c.MessageID, id)
		}
		text.WriteString(c.Delta)
	}
	if text.String() != "Hello AG-UI" {
		t.Fatalf("streamed text = %q", text.String())
	}

	fin := evs[len(evs)-1].(RunFinishedEvent)
	if fin.ThreadID != "t1" || fin.RunID != "r1" {
		t.Fatalf("run finished = %+v", fin)
	}
	if _, ok := fin.Outcome.(SuccessOutcome); !ok {
		t.Fatalf("outcome = %#v, want success", fin.Outcome)
	}
	if fin.Usage != nil {
		t.Fatalf("usage = %+v, want none for an unreported run", fin.Usage)
	}

	// The JSON form is camelCase with the type discriminator up front.
	var obj map[string]any
	if err := json.Unmarshal(raw[2], &obj); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if obj["type"] != "TEXT_MESSAGE_CONTENT" {
		t.Fatalf("type = %v", obj["type"])
	}
	for _, key := range []string{"messageId", "delta", "timestamp"} {
		if _, ok := obj[key]; !ok {
			t.Fatalf("frame missing %q: %s", key, raw[2])
		}
	}
	if _, snake := obj["message_id"]; snake {
		t.Fatalf("snake_case key on the wire: %s", raw[2])
	}
}

func TestPOSTReportsRunError(t *testing.T) {
	// A script with no responses: every model call fails, the turn fails, and
	// the failure reaches the client as RUN_ERROR.
	evs, _ := postRun(t, ServeAgent(servedAgent(agotest.NewTestConfig())), runBody)

	if got := eventTypes(evs); !equalTypes(got, []EventType{EventRunStarted, EventRunError}) {
		t.Fatalf("types = %v, want RUN_STARTED then RUN_ERROR", got)
	}
	runErr := evs[1].(RunErrorEvent)
	// The model call's failure — the script ran dry — is what the client reads.
	if !strings.Contains(runErr.Message, "scripted responses exhausted") {
		t.Fatalf("message = %q, want the model failure", runErr.Message)
	}
}

func TestPOSTProjectsToolCalls(t *testing.T) {
	cfg := agotest.NewTestConfig(
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "c1", Name: "get_weather", Arguments: `{"city":"Paris"}`,
				Base: events.Base{}},
		}}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: "It is sunny."}},
	)
	evs, raw := postRun(t, ServeAgent(servedAgent(cfg)), runBody)

	want := []EventType{
		EventRunStarted,
		EventToolCallStart, EventToolCallArgs, EventToolCallEnd,
		EventToolCallResult,
		EventTextMessageChunk,
		EventRunFinished,
	}
	if got := eventTypes(evs); !equalTypes(got, want) {
		t.Fatalf("types = %v\nwant  = %v", got, want)
	}
	start := evs[1].(ToolCallStartEvent)
	if start.ToolCallID != "c1" || start.ToolCallName != "get_weather" {
		t.Fatalf("start = %+v", start)
	}
	// The unregistered call was answered with the not-found text.
	res := evs[4].(ToolCallResultEvent)
	if res.ToolCallID != "c1" {
		t.Fatalf("result for %q, want c1", res.ToolCallID)
	}
	if text, ok := res.Content.(string); !ok || text == "" {
		t.Fatalf("result content = %#v, want the not-found text", res.Content)
	}
	var frame map[string]any
	if err := json.Unmarshal(raw[4], &frame); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if frame["toolCallId"] != "c1" || frame["role"] != "tool" {
		t.Fatalf("frame = %s", raw[4])
	}
}

func TestPOSTCarriesUsageOnRunFinished(t *testing.T) {
	cfg := agotest.NewTestConfig(&events.ModelResponse{
		Message: &events.ModelMessage{Content: "ok"},
		Usage:   events.Usage{PromptTokens: i64(10), CompletionTokens: i64(5)},
		Model:   "test", Provider: "test",
	})
	evs, _ := postRun(t, ServeAgent(servedAgent(cfg)), runBody)

	fin := evs[len(evs)-1].(RunFinishedEvent)
	want := []TokenUsage{{Provider: "test", Model: "test", InputTokens: i64(10), OutputTokens: i64(5), TotalTokens: i64(15)}}
	if len(fin.Usage) != 1 || fin.Usage[0].TotalTokens == nil || *fin.Usage[0].TotalTokens != 15 ||
		*fin.Usage[0].InputTokens != 10 {
		t.Fatalf("usage = %+v, want one entry totalling 15", fin.Usage)
	}
	_ = want
}

// The input's history reaches the model: an assistant message with its tool call
// and the tool's answer are on the transcript of the model call.
func TestPOSTSeedsHistoryFromInput(t *testing.T) {
	cfg := agotest.NewTestConfig(&events.ModelResponse{Message: &events.ModelMessage{Content: "done"}})
	body := `{"threadId":"t1","runId":"r1","messages":[
	  {"id":"m0","role":"system","content":"be brief"},
	  {"id":"m1","role":"user","content":"weather?"},
	  {"id":"m2","role":"assistant","content":"calling",
	   "toolCalls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},
	  {"id":"m3","role":"tool","toolCallId":"c1","content":"sunny"},
	  {"id":"m4","role":"user","content":"thanks"}
	]}`
	postRun(t, ServeAgent(servedAgent(cfg)), body)

	calls := cfg.Calls()
	if len(calls) != 1 {
		t.Fatalf("%d model calls, want 1", len(calls))
	}
	history := calls[0]
	var kinds []string
	for _, ev := range history {
		kinds = append(kinds, events.AsValue(ev).EventType())
	}
	want := []string{"ModelRequest", "ModelRequest", "ModelResponse", "ToolResultsEvent", "ModelRequest"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("history = %v, want %v", kinds, want)
	}
	// [0] system folded into the opening request; [1] the first user turn.
	if p := events.AsValue(history[0].(*events.ModelRequest).Parts[0]).(events.TextInput); p.Content != "be brief" {
		t.Fatalf("system prompt = %q", p.Content)
	}
	// [2] the assistant turn replays with its call.
	resp := history[2].(*events.ModelResponse)
	if resp.Content() != "calling" || len(resp.ToolCalls.Calls) != 1 || resp.ToolCalls.Calls[0].ID != "c1" {
		t.Fatalf("assistant replay = %+v", resp)
	}
	// [3] the tool result is tied to its call, named off the assistant's calls.
	results := history[3].(*events.ToolResultsEvent).Results
	if results[0].ParentID != "c1" || results[0].Name != "get_weather" {
		t.Fatalf("tool result = %+v", results[0])
	}
}

func TestPOSTInvalidBodyIs400(t *testing.T) {
	ts := httptest.NewServer(ServeAgent(servedAgent(agotest.NewTestConfig())))
	defer ts.Close()

	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(`{not json`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] != "invalid AG-UI RunAgentInput body" {
		t.Fatalf("body = %v", body)
	}
}

func TestPOSTOtherProtocolMajorIsRefused(t *testing.T) {
	evs, raw := postRun(t, ServeAgent(servedAgent(agotest.NewTestConfig())),
		`{"threadId":"t1","runId":"r1","protocolVersion":"2.0","messages":[]}`)

	if got := eventTypes(evs); !equalTypes(got, []EventType{EventRunError}) {
		t.Fatalf("types = %v, want a lone RUN_ERROR before any run starts", got)
	}
	runErr := evs[0].(RunErrorEvent)
	if runErr.Code != CodeUnsupportedProtocolVersion {
		t.Fatalf("code = %q", runErr.Code)
	}
	if !strings.Contains(runErr.Message, "2.0") {
		t.Fatalf("message = %q, want the declared version named", runErr.Message)
	}
	var frame map[string]any
	_ = json.Unmarshal(raw[0], &frame)
}

func TestPOSTCancelledRunFinishesCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	a := servedAgent(agotest.NewTestConfig(&events.ModelResponse{Message: &events.ModelMessage{Content: "x"}}))
	var evs []Event
	if err := serveTurn(ctx, a, RunAgentInput{ThreadID: "t1", RunID: "r1"},
		func(e Event) { evs = append(evs, e) }); err != nil {
		t.Fatalf("serveTurn: %v", err)
	}

	if got := eventTypes(evs); !equalTypes(got, []EventType{EventRunStarted, EventRunFinished}) {
		t.Fatalf("types = %v, want RUN_STARTED then RUN_FINISHED", got)
	}
	if _, ok := evs[1].(RunFinishedEvent).Outcome.(CancelledOutcome); !ok {
		t.Fatalf("outcome = %#v, want cancelled", evs[1].(RunFinishedEvent).Outcome)
	}
}

// A Handler with its own callback: custom events pass through verbatim, the
// capability answer is the configured one, and a run that ends without a
// terminal event is repaired with RUN_ERROR.
func TestHandlerCallback(t *testing.T) {
	t.Run("custom events and state snapshots pass through", func(t *testing.T) {
		h := Handler(HandlerConfig{
			Capabilities: func() AgentCapabilities {
				return AgentCapabilities{State: &StateCapabilities{Snapshots: boolPtr(true)}}
			},
			Run: func(ctx context.Context, input RunAgentInput, emit func(Event)) error {
				emit(NewStateSnapshotEvent(map[string]any{"step": 1}))
				emit(NewCustomEvent("progress", "half"))
				emit(NewRunFinishedEvent(input.ThreadID, input.RunID))
				return nil
			},
		})
		evs, _ := postRun(t, h, runBody)
		if got := eventTypes(evs); !equalTypes(got, []EventType{EventStateSnapshot, EventCustom, EventRunFinished}) {
			t.Fatalf("types = %v", got)
		}
		if snap := evs[0].(StateSnapshotEvent); snap.Snapshot.(map[string]any)["step"] != float64(1) {
			t.Fatalf("snapshot = %+v", snap)
		}
	})

	t.Run("a run with no terminal event gets one RUN_ERROR", func(t *testing.T) {
		h := Handler(HandlerConfig{
			Run: func(ctx context.Context, input RunAgentInput, emit func(Event)) error {
				emit(NewRunStartedEvent(input.ThreadID, input.RunID))
				return nil // and nothing else
			},
		})
		evs, _ := postRun(t, h, runBody)
		if got := eventTypes(evs); !equalTypes(got, []EventType{EventRunStarted, EventRunError}) {
			t.Fatalf("types = %v, want the run repaired with RUN_ERROR", got)
		}
		if m := evs[1].(RunErrorEvent).Message; m != "the run was stopped before it could finish" {
			t.Fatalf("message = %q", m)
		}
	})

	t.Run("a panicking callback is repaired with RUN_ERROR", func(t *testing.T) {
		h := Handler(HandlerConfig{
			Run: func(ctx context.Context, input RunAgentInput, emit func(Event)) error {
				emit(NewRunStartedEvent(input.ThreadID, input.RunID))
				panic("callback blew up")
			},
		})
		evs, _ := postRun(t, h, runBody)
		if got := eventTypes(evs); !equalTypes(got, []EventType{EventRunStarted, EventRunError}) {
			t.Fatalf("types = %v, want the panicked run repaired with RUN_ERROR", got)
		}
	})

	t.Run("a raw frame travels verbatim", func(t *testing.T) {
		payload := []byte(`{"type":"STEP_STARTED","timestamp":7,"stepName":"s1"}`)
		h := Handler(HandlerConfig{
			Run: func(ctx context.Context, input RunAgentInput, emit func(Event)) error {
				emit(Raw{Payload: payload})
				emit(NewRunFinishedEvent(input.ThreadID, input.RunID))
				return nil
			},
		})
		_, raw := postRun(t, h, runBody)
		if string(raw[0]) != string(payload) {
			t.Fatalf("frame = %s, want %s", raw[0], payload)
		}
	})
}

func equalTypes(got, want []EventType) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
