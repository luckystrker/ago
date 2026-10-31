package agui

import (
	stdcontext "context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/ago/agent"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/internal/sse"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
)

// The question the test agent's tool asks, and the tool that asks it (Python
// parity: the asking_agent fixture).
const testQuestion = "What color?"

func askingTool() *tools.FunctionTool {
	tool, err := tools.NewFunctionTool("ask_human", "Ask the human",
		func(args struct{}, cx *agocontext.Context) (string, error) {
			answer, err := cx.Input(stdcontext.Background(), testQuestion, 0)
			if err != nil {
				return "", err
			}
			return "heard: " + answer, nil
		})
	if err != nil {
		panic(err)
	}
	return tool
}

// askingServer builds a served agent whose script asks the question on the
// first model call and answers with text on the second.
func askingServer(t *testing.T, opts ...ServerOption) (*Server, *agotest.TestConfig, http.Handler) {
	t.Helper()
	srv, err := NewServer(opts...)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	cfg := agotest.NewTestConfig(
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "c1", Name: "ask_human", Arguments: "{}"},
		}}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: "noted"}},
	)
	base := append([]agent.Option{agent.WithTools(askingTool())}, srv.Options()...)
	h := srv.Handler(agent.New("served", "You are served.", append(base, agent.WithConfig(cfg))...))
	return srv, cfg, h
}

// soleInterrupt is the one interrupt a paused run's RUN_FINISHED carries.
func soleInterrupt(t *testing.T, evs []Event) Interrupt {
	t.Helper()
	fin, ok := evs[len(evs)-1].(RunFinishedEvent)
	if !ok {
		t.Fatalf("last event = %T, want RUN_FINISHED", evs[len(evs)-1])
	}
	out, ok := fin.Outcome.(InterruptOutcome)
	if !ok {
		t.Fatalf("outcome = %#v, want interrupt", fin.Outcome)
	}
	if len(out.Interrupts) != 1 {
		t.Fatalf("%d interrupts, want 1", len(out.Interrupts))
	}
	return out.Interrupts[0]
}

func resumeBody(threadID, runID, interruptID string, payload any) string {
	b, err := json.Marshal(map[string]any{
		"threadId": threadID, "runId": runID, "messages": []any{},
		"resume": []map[string]any{{"interruptId": interruptID, "status": "resolved", "payload": payload}},
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestServerInterruptResumeCycle(t *testing.T) {
	_, cfg, h := askingServer(t)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	// The question pauses the run: partial events, then a RUN_FINISHED whose
	// outcome is the interrupt — never RUN_ERROR.
	first, _ := postRunURL(t, ts.URL, runBody)
	got := eventTypes(first)
	if got[0] != EventRunStarted {
		t.Fatalf("first event = %s, want RUN_STARTED", got[0])
	}
	if last := got[len(got)-1]; last != EventRunFinished {
		t.Fatalf("last event = %s, want the interrupt RUN_FINISHED", last)
	}
	for _, k := range got[1 : len(got)-1] {
		if k != EventToolCallStart && k != EventToolCallArgs && k != EventToolCallEnd {
			t.Fatalf("middle event %s, want the ask_human tool call projection", k)
		}
	}
	interrupt := soleInterrupt(t, first)
	if interrupt.ID == "" || interrupt.Reason != ReasonInputRequired || interrupt.Message != testQuestion {
		t.Fatalf("interrupt = %+v", interrupt)
	}
	if _, err := time.Parse(time.RFC3339Nano, interrupt.ExpiresAt); err != nil {
		t.Fatalf("expiresAt %q does not parse: %v", interrupt.ExpiresAt, err)
	}
	if interrupt.ResponseSchema["type"] != "string" {
		t.Fatalf("response schema = %+v", interrupt.ResponseSchema)
	}
	if _, ok := proofIn(interrupt.Metadata); !ok {
		t.Fatalf("metadata = %+v, want the %s.%s proof envelope", interrupt.Metadata, metadataKey, proofKey)
	}

	// The answer rides the next POST as a resume entry; the same turn carries
	// on and finishes.
	second, raw := postRunURL(t, ts.URL, resumeBody("t1", "r2", interrupt.ID, "blue"))
	got = eventTypes(second)
	if got[0] != EventRunStarted {
		t.Fatalf("resume first event = %s, want RUN_STARTED", got[0])
	}
	start := second[0].(RunStartedEvent)
	if start.RunID != "r2" || start.ProtocolVersion != ProtocolVersion {
		t.Fatalf("resuming run started = %+v", start)
	}
	if last := got[len(got)-1]; last != EventRunFinished {
		t.Fatalf("resume last event = %s, want RUN_FINISHED", last)
	}
	if _, ok := second[len(second)-1].(RunFinishedEvent).Outcome.(SuccessOutcome); !ok {
		t.Fatalf("resume outcome = %#v, want success", second[len(second)-1].(RunFinishedEvent).Outcome)
	}
	// The tool result naming the answer went out on the resumed run.
	var result string
	for _, ev := range second {
		if res, ok := ev.(ToolCallResultEvent); ok {
			result, _ = res.Content.(string)
		}
	}
	if result != "heard: blue" {
		t.Fatalf("tool result = %q, want the resumed answer", result)
	}
	for _, frame := range raw {
		if strings.Contains(string(frame), `"outcome":{"type":"interrupt"`) {
			t.Fatalf("the resumed run paused again: %s", frame)
		}
	}

	// The answer reached the model: the resumed call's history carries the
	// tool's result.
	calls := cfg.Calls()
	if len(calls) != 2 {
		t.Fatalf("%d model calls, want 2", len(calls))
	}
	found := false
	for _, ev := range calls[1] {
		if res, ok := ev.(*events.ToolResultsEvent); ok {
			p := events.AsValue(res.Results[0].Result.Parts[0]).(events.TextInput)
			found = p.Content == "heard: blue"
		}
	}
	if !found {
		t.Fatalf("the resumed model call never saw the answer: %v", calls[1])
	}
}

// postRunURL is postRun against a running server, so one server carries several
// POSTs of a cycle.
func postRunURL(t *testing.T, url, body string) ([]Event, [][]byte) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
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

func TestServerStateSnapshotOpeningRun(t *testing.T) {
	srv, err := NewServer(WithStateSnapshots())
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	cfg := agotest.NewTestConfig(&events.ModelResponse{Message: &events.ModelMessage{Content: "ok"}})
	base := append(srv.Options(),
		agent.WithVariables(map[string]any{"serverVar": "v", "ag:internal": "hidden"}),
		agent.WithConfig(cfg))
	h := srv.Handler(agent.New("served", "You are served.", base...))

	evs, _ := postRun(t, h, `{"threadId":"t1","runId":"r1","state":{"clientVar":true,"ag:client":1},"messages":[{"id":"m1","role":"user","content":"hi"}]}`)
	got := eventTypes(evs)
	if len(got) < 2 || got[0] != EventRunStarted || got[1] != EventStateSnapshot {
		t.Fatalf("opening events = %v, want RUN_STARTED then STATE_SNAPSHOT", got[:2])
	}
	snap := evs[1].(StateSnapshotEvent).Snapshot.(map[string]any)
	if snap["clientVar"] != true || snap["serverVar"] != "v" {
		t.Fatalf("snapshot = %+v, want the client's keys and the server's variables together", snap)
	}
	for key := range snap {
		if reservedVariable(key) {
			t.Fatalf("reserved key %q in the snapshot", key)
		}
	}
	// Without the config, no snapshot: the default server does not declare or
	// emit one.
	_, _, plain := askingServer(t)
	evs, _ = postRun(t, plain, runBody)
	if got := eventTypes(evs); got[1] == EventStateSnapshot {
		t.Fatalf("snapshot emitted while disabled")
	}
}

func TestServerStateSnapshotCapabilitity(t *testing.T) {
	srv, err := NewServer(WithStateSnapshots())
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	cfg := agotest.NewTestConfig()
	base := append(srv.Options(), agent.WithConfig(cfg))
	h := srv.Handler(agent.New("served", "You are served.", base...))
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var caps AgentCapabilities
	if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if caps.HumanInTheLoop == nil || caps.HumanInTheLoop.Interrupts == nil || !*caps.HumanInTheLoop.Interrupts {
		t.Fatalf("humanInTheLoop = %+v, want interrupts declared", caps.HumanInTheLoop)
	}
	if caps.State == nil || caps.State.Snapshots == nil || !*caps.State.Snapshots {
		t.Fatalf("state = %+v, want snapshots declared", caps.State)
	}
}

func TestServerRefusesFreshRunOnHeldThread(t *testing.T) {
	_, _, h := askingServer(t)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	first, _ := postRunURL(t, ts.URL, runBody)
	interrupt := soleInterrupt(t, first)

	// A run with no resume entries while the thread waits: refused before any
	// run starts, and the question stays answerable.
	refused, _ := postRunURL(t, ts.URL, runBody)
	if got := eventTypes(refused); !equalTypes(got, []EventType{EventRunError}) {
		t.Fatalf("types = %v, want a lone RUN_ERROR", got)
	}
	if code := refused[0].(RunErrorEvent).Code; code != CodeInterruptNotCovered {
		t.Fatalf("code = %q, want %s", code, CodeInterruptNotCovered)
	}

	resumed, _ := postRunURL(t, ts.URL, resumeBody("t1", "r3", interrupt.ID, "late"))
	if got := eventTypes(resumed); got[len(got)-1] != EventRunFinished {
		t.Fatalf("the question did not survive the refusal: %v", got)
	}
}

func TestServerRefusesUncoveredResumeAndKeepsHolding(t *testing.T) {
	_, _, h := askingServer(t)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	first, _ := postRunURL(t, ts.URL, runBody)
	interrupt := soleInterrupt(t, first)

	// An entry for another interrupt: the resume is refused, and the turn is
	// put back with its deadline unchanged, so a legitimate answer arriving in
	// time still resumes it.
	body := resumeBody("t1", "r2", "not-the-interrupt", "blue")
	refused, _ := postRunURL(t, ts.URL, body)
	if got := eventTypes(refused); !equalTypes(got, []EventType{EventRunError}) {
		t.Fatalf("types = %v, want a lone RUN_ERROR", got)
	}
	if code := refused[0].(RunErrorEvent).Code; code != CodeInterruptNotCovered {
		t.Fatalf("code = %q, want %s", code, CodeInterruptNotCovered)
	}

	resumed, _ := postRunURL(t, ts.URL, resumeBody("t1", "r3", interrupt.ID, "blue"))
	if last := eventTypes(resumed)[len(eventTypes(resumed))-1]; last != EventRunFinished {
		t.Fatalf("the held turn was lost to the refused resume: %v", resumed)
	}
}

func TestServerRefusesPayloadOfWrongType(t *testing.T) {
	_, _, h := askingServer(t)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	first, _ := postRunURL(t, ts.URL, runBody)
	interrupt := soleInterrupt(t, first)

	refused, _ := postRunURL(t, ts.URL, resumeBody("t1", "r2", interrupt.ID, []any{"not", "a string"}))
	if got := eventTypes(refused); !equalTypes(got, []EventType{EventRunError}) {
		t.Fatalf("types = %v, want a lone RUN_ERROR", got)
	}
	if code := refused[0].(RunErrorEvent).Code; code != CodeInterruptPayloadRefused {
		t.Fatalf("code = %q, want %s", code, CodeInterruptPayloadRefused)
	}

	// Refused, not lost: the right answer still lands.
	resumed, _ := postRunURL(t, ts.URL, resumeBody("t1", "r3", interrupt.ID, "blue"))
	if last := eventTypes(resumed); last[len(last)-1] != EventRunFinished {
		t.Fatalf("the held turn was lost to the refused resume")
	}
}

func TestServerBooleanPayloadAnswersApprovalStyle(t *testing.T) {
	_, _, h := askingServer(t)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	first, _ := postRunURL(t, ts.URL, runBody)
	interrupt := soleInterrupt(t, first)

	evs, _ := postRunURL(t, ts.URL, resumeBody("t1", "r2", interrupt.ID, true))
	var result string
	for _, ev := range evs {
		if res, ok := ev.(ToolCallResultEvent); ok {
			result, _ = res.Content.(string)
		}
	}
	if result != "heard: y" {
		t.Fatalf("tool result = %q, want the boolean translated to the approval words", result)
	}
}

func TestServerCancelledResumeAbandons(t *testing.T) {
	srv, _, h := askingServer(t)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	first, _ := postRunURL(t, ts.URL, runBody)
	interrupt := soleInterrupt(t, first)

	b, _ := json.Marshal(map[string]any{
		"threadId": "t1", "runId": "r2", "messages": []any{},
		"resume": []map[string]any{{"interruptId": interrupt.ID, "status": "cancelled"}},
	})
	evs, _ := postRunURL(t, ts.URL, string(b))
	got := eventTypes(evs)
	if !equalTypes(got, []EventType{EventRunStarted, EventRunFinished}) {
		t.Fatalf("types = %v, want RUN_STARTED then the cancelled RUN_FINISHED", got)
	}
	if _, ok := evs[1].(RunFinishedEvent).Outcome.(CancelledOutcome); !ok {
		t.Fatalf("outcome = %#v, want cancelled", evs[1].(RunFinishedEvent).Outcome)
	}
	if srv.turns.holds("t1") {
		t.Fatal("the thread still holds the abandoned question")
	}
}

func TestServerRetentionExpiresUnansweredQuestion(t *testing.T) {
	srv, _, h := askingServer(t, WithRetention(Retention{TTL: 40 * time.Millisecond, MaxHeld: 4}))
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	first, _ := postRunURL(t, ts.URL, runBody)
	interrupt := soleInterrupt(t, first)
	if !srv.turns.holds("t1") {
		t.Fatal("the question is not held")
	}
	time.Sleep(120 * time.Millisecond)
	if srv.turns.holds("t1") {
		t.Fatal("the expired question is still held")
	}

	// A late answer answers nothing this server holds: an ordinary run starts.
	evs, _ := postRunURL(t, ts.URL, resumeBody("t1", "r2", interrupt.ID, "blue"))
	got := eventTypes(evs)
	if got[len(got)-1] != EventRunFinished {
		t.Fatalf("late resume ended with %s", got[len(got)-1])
	}
	for _, ev := range evs {
		if res, ok := ev.(ToolCallResultEvent); ok {
			if text, _ := res.Content.(string); text == "heard: blue" {
				t.Fatal("the expired turn was resumed by the late answer")
			}
		}
	}
}

func TestServerResumeProof(t *testing.T) {
	t.Run("required: a resume without the proof is refused", func(t *testing.T) {
		_, _, h := askingServer(t, WithResumeProof())
		ts := httptest.NewServer(h)
		t.Cleanup(ts.Close)

		first, _ := postRunURL(t, ts.URL, runBody)
		interrupt := soleInterrupt(t, first)

		refused, _ := postRunURL(t, ts.URL, resumeBody("t1", "r2", interrupt.ID, "blue"))
		if code := refused[0].(RunErrorEvent).Code; code != CodeInterruptNotProven {
			t.Fatalf("code = %q, want %s", code, CodeInterruptNotProven)
		}

		// The refusal put the turn back: the proven answer still resumes it.
		proven := resumeBody("t1", "r3", interrupt.ID, "blue")
		proven = withResumeMetadata(proven, interrupt.Metadata)
		resumed, _ := postRunURL(t, ts.URL, proven)
		if last := eventTypes(resumed); last[len(last)-1] != EventRunFinished {
			t.Fatalf("a proven resume could not resume the held turn")
		}
		var result string
		for _, ev := range resumed {
			if res, ok := ev.(ToolCallResultEvent); ok {
				result, _ = res.Content.(string)
			}
		}
		if result != "heard: blue" {
			t.Fatalf("tool result = %q, want the resumed answer", result)
		}
	})

	t.Run("default: a resume with no proof is served", func(t *testing.T) {
		_, _, h := askingServer(t)
		ts := httptest.NewServer(h)
		t.Cleanup(ts.Close)

		first, _ := postRunURL(t, ts.URL, runBody)
		interrupt := soleInterrupt(t, first)
		evs, _ := postRunURL(t, ts.URL, resumeBody("t1", "r2", interrupt.ID, "blue"))
		if last := eventTypes(evs); last[len(last)-1] != EventRunFinished {
			t.Fatalf("an unproven resume was refused by default")
		}
	})

	t.Run("a proof that is present must be the one issued", func(t *testing.T) {
		_, _, h := askingServer(t)
		ts := httptest.NewServer(h)
		t.Cleanup(ts.Close)

		first, _ := postRunURL(t, ts.URL, runBody)
		interrupt := soleInterrupt(t, first)
		wrong := withResumeMetadata(resumeBody("t1", "r2", interrupt.ID, "blue"),
			map[string]any{metadataKey: map[string]any{proofKey: "forged"}})
		refused, _ := postRunURL(t, ts.URL, wrong)
		if code := refused[0].(RunErrorEvent).Code; code != CodeInterruptNotProven {
			t.Fatalf("code = %q, want %s", code, CodeInterruptNotProven)
		}
	})
}

// withResumeMetadata copies an interrupt's metadata into the resume entry of a
// body already built, the way a proof-aware client would.
func withResumeMetadata(body string, metadata map[string]any) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		panic(err)
	}
	m["resume"].([]any)[0].(map[string]any)["metadata"] = metadata
	b, _ := json.Marshal(m)
	return string(b)
}

func TestServerDropResumeOnUnknownThread(t *testing.T) {
	_, _, h := askingServer(t)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	// A resume for a thread that holds nothing: entries dropped with a
	// warning, ordinary run served.
	evs, _ := postRunURL(t, ts.URL, resumeBody("t9", "r1", "no-such-interrupt", "blue"))
	if got := eventTypes(evs); got[0] != EventRunStarted || got[len(got)-1] != EventRunFinished {
		t.Fatalf("types = %v, want an ordinary served run", got)
	}
}

func TestServerInvalidRetentionIsRejected(t *testing.T) {
	if _, err := NewServer(WithRetention(Retention{TTL: 0, MaxHeld: 1})); err == nil {
		t.Fatal("a non-positive TTL must be refused")
	}
	if _, err := NewServer(WithRetention(Retention{TTL: time.Second, MaxHeld: 0})); err == nil {
		t.Fatal("a MaxHeld below 1 must be refused")
	}
}

func TestServerCloseReleasesHeldTurns(t *testing.T) {
	srv, _, h := askingServer(t, WithRetention(Retention{TTL: time.Minute, MaxHeld: 4}))
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	first, _ := postRunURL(t, ts.URL, runBody)
	_ = soleInterrupt(t, first)
	if !srv.turns.holds("t1") {
		t.Fatal("the question is not held")
	}
	srv.Close()
	if srv.turns.holds("t1") {
		t.Fatal("Close left the question held")
	}
}

// A Guard given to the Server applies to the endpoint it serves.
func TestServerGuardAppliesToEndpoint(t *testing.T) {
	_, _, h := askingServer(t, WithGuard(NewGuard(WithAuthToken("s3cret"))))
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(runBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("POST without the token: status = %d, want 401", resp.StatusCode)
	}

	req, err := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(runBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer s3cret")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post with the token: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("POST with the token: status = %d, want 200", resp2.StatusCode)
	}
}
