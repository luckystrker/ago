package network

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/task"
	"github.com/luckystrker/ago/tools"
)

// toolResultJSON marshals a tool result to a generic value for assertions.
func toolResultJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("tool result is not an object: %v (%s)", err, raw)
	}
	return m
}

func toolResultList(t *testing.T, v any) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var list []map[string]any
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("tool result is not a list: %v (%s)", err, raw)
	}
	return list
}

// callTool invokes a tool by name through the FunctionTool machinery (real
// schema + binding path, not the closure directly).
func callTool(t *testing.T, set []tools.Tool, name string, args string) any {
	t.Helper()
	return callToolIn(t, set, name, args, nil)
}

// callToolIn is callTool over a caller-supplied context (nil → fresh).
func callToolIn(t *testing.T, set []tools.Tool, name string, args string, cx *agocontext.Context) any {
	t.Helper()
	var target tools.Tool
	for _, tool := range set {
		if tool.Name() == name {
			target = tool
			break
		}
	}
	if target == nil {
		t.Fatalf("tool %q not in the set", name)
	}
	if cx == nil {
		cx = agocontext.New(stream.NewMemoryStream())
	}
	// A fresh stream per call keeps the result scan unambiguous when the
	// caller-supplied context already carries earlier tool results.
	s := stream.NewMemoryStream()
	cx = agocontext.NewWithDeps(s, cx.Dependencies, cx.Variables)
	if _, err := target.Register(cx, nil); err != nil {
		t.Fatal(err)
	}
	if err := cx.Send(events.ToolCallEvent{ID: "call-1", Name: name, Arguments: args}); err != nil {
		t.Fatal(err)
	}
	// The tool runs synchronously in the dispatch, so the result event is
	// usually already in history — poll the transcript instead of Get
	// (which only sees future events).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		evs, err := s.History().GetEvents()
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range evs {
			r, ok := ev.(events.ToolResultEvent)
			if !ok || r.ParentID != "call-1" {
				continue
			}
			if r.Name == "" || r.Name == name {
				if e, isErr := ev.(events.ToolErrorEvent); isErr {
					t.Fatalf("tool errored: %s / %v", e.Error, e.Result)
				}
				if len(r.Result.Parts) == 0 {
					t.Fatal("empty tool result")
				}
				switch part := r.Result.Parts[0].(type) {
				case *events.TextInput:
					return part.Content
				case *events.DataInput:
					return part.Data
				default:
					t.Fatalf("unexpected result part %T", r.Result.Parts[0])
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if evs, err := s.History().GetEvents(); err == nil {
		for _, ev := range evs {
			t.Logf("stream event: %T %+v", ev, ev)
		}
	}
	t.Fatal("tool never returned")
	return nil
}

// clientToolsFixture registers alice (the caller) with a resume and bob
// (the peer) on one hub, and hands out the tool set for alice.
func clientToolsFixture(t *testing.T) (*Hub, *HubClient, *AgentClient, []tools.Tool) {
	t.Helper()
	h := newTestHub(t)
	hc := NewHubClient(h)

	alice, err := hc.Register("alice", WithAgentResume(Resume{Capabilities: []string{"math"}}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { alice.Close() })
	_, err = hc.Register("bob", WithAgentResume(Resume{Capabilities: []string{"poetry"}}))
	if err != nil {
		t.Fatal(err)
	}

	set, err := ClientTools(hc, alice)
	if err != nil {
		t.Fatal(err)
	}
	return h, hc, alice, set
}

func TestClientToolsSetShape(t *testing.T) {
	_, _, _, set := clientToolsFixture(t)
	want := []string{"peers", "channels", "tasks", "context", "delegate"}
	if len(set) != len(want) {
		t.Fatalf("tool set %v", toolNames(set))
	}
	for i, name := range want {
		if set[i].Name() != name {
			t.Fatalf("tool %d = %s, want %s", i, set[i].Name(), name)
		}
		if !set[i].DeclaredInCode() {
			t.Fatalf("tool %s must be code-declared", name)
		}
	}
}

func toolNames(set []tools.Tool) []string {
	out := make([]string, len(set))
	for i, tool := range set {
		out[i] = tool.Name()
	}
	return out
}

func TestPeersToolFindAndDescribe(t *testing.T) {
	_, _, _, set := clientToolsFixture(t)

	got := callTool(t, set, "peers", `{"action":"find","capability":"poetry"}`)
	list := toolResultList(t, got)
	if len(list) != 1 || list[0]["name"] != "bob" {
		t.Fatalf("find by capability = %v", list)
	}

	got = callTool(t, set, "peers", `{"action":"find"}`)
	list = toolResultList(t, got)
	if len(list) != 1 || list[0]["name"] != "bob" {
		t.Fatalf("find must exclude the calling agent: %v", list)
	}

	got = callTool(t, set, "peers", `{"action":"describe","name":"bob"}`)
	m := toolResultJSON(t, got)
	passport := m["passport"].(map[string]any)
	if passport["name"] != "bob" || passport["agent_id"] == "" {
		t.Fatalf("describe = %v", m)
	}

	got = callTool(t, set, "peers", `{"action":"describe"}`)
	if s, _ := got.(string); !strings.HasPrefix(s, "Error:") {
		t.Fatalf("describe without name must error, got %v", got)
	}
}

func TestChannelsToolListOpenInfoClose(t *testing.T) {
	h, _, _, set := clientToolsFixture(t)

	got := callTool(t, set, "channels", `{"action":"open","type":"conversation","target":"bob","message":"hello peer"}`)
	m := toolResultJSON(t, got)
	channelID, _ := m["channel_id"].(string)
	if channelID == "" {
		t.Fatalf("open = %v", m)
	}

	got = callTool(t, set, "channels", `{"action":"list"}`)
	list := toolResultList(t, got)
	if len(list) != 1 || list[0]["channel_id"] != channelID {
		t.Fatalf("list = %v", list)
	}

	got = callTool(t, set, "channels", `{"action":"info","channel_id":"`+channelID+`"}`)
	m = toolResultJSON(t, got)
	if m["state"] != "active" {
		t.Fatalf("info = %v", m)
	}

	// The seeded message landed in the WAL.
	envs, _ := h.ReadWAL(channelID)
	found := false
	for _, env := range envs {
		if env.EventType == EnvText && env.Text() == "hello peer" {
			found = true
		}
	}
	if !found {
		t.Fatalf("seed message missing from WAL: %v", envs)
	}

	got = callTool(t, set, "channels", `{"action":"close","channel_id":"`+channelID+`"}`)
	m = toolResultJSON(t, got)
	if m["state"] != "closed" {
		t.Fatalf("close = %v", m)
	}
}

func TestTasksToolListStatusWait(t *testing.T) {
	h, _, alice, set := clientToolsFixture(t)

	// Mirror a task for alice through the hub store.
	_ = h.ObserveTask(task.Metadata{TaskID: "tk-1", OwnerID: alice.AgentID(), State: task.StateRunning,
		Spec: task.Spec{Title: "grind"}})

	got := callTool(t, set, "tasks", `{"action":"list","scope":"own"}`)
	list := toolResultList(t, got)
	if len(list) != 1 || list[0]["task_id"] != "tk-1" {
		t.Fatalf("list = %v", list)
	}

	got = callTool(t, set, "tasks", `{"action":"status","task_id":"tk-1"}`)
	m := toolResultJSON(t, got)
	if m["state"] != "running" {
		t.Fatalf("status = %v", m)
	}

	// Complete through the store; wait must observe the terminal state.
	_ = h.UpdateTask("tk-1", task.StateCompleted, nil, "done", "")
	got = callTool(t, set, "tasks", `{"action":"wait","task_id":"tk-1","timeout":2}`)
	m = toolResultJSON(t, got)
	if m["state"] != "completed" {
		t.Fatalf("wait = %v", m)
	}
}

func TestTasksToolProgressCompleteNeedActiveTask(t *testing.T) {
	_, _, _, set := clientToolsFixture(t)

	got := callTool(t, set, "tasks", `{"action":"progress","payload":{"step":1}}`)
	if s, _ := got.(string); !strings.HasPrefix(s, "Error:") {
		t.Fatalf("progress without an active task must error, got %v", got)
	}

	// With an active task stamped on the context (task.Start stamps
	// task.DepKey) the action lands on that task.
	s := stream.NewMemoryStream()
	cx := agocontext.New(s)
	tk, err := task.Start(cx, "owner-x", task.Spec{Title: "active"})
	if err != nil {
		t.Fatal(err)
	}
	got = callToolIn(t, set, "tasks", `{"action":"progress","payload":{"step":1}}`, cx)
	if got != "progress emitted on "+tk.TaskID() {
		t.Fatalf("progress with active task = %v", got)
	}
	got = callToolIn(t, set, "tasks", `{"action":"complete","result":"all done"}`, cx)
	if got != "completed "+tk.TaskID() {
		t.Fatalf("complete with active task = %v", got)
	}
}

func TestContextToolSearchAndQuote(t *testing.T) {
	h, _, alice, set := clientToolsFixture(t)
	bobP, _ := h.Passport("bob")

	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bobP.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = h.Send(NewTextEnvelope(ch.ID, alice.AgentID(), "", "the launch is on Friday"))
	_, _ = h.Send(NewTextEnvelope(ch.ID, bobP.AgentID, "", "confirmed, Friday works"))

	got := callTool(t, set, "context", `{"action":"search","query":"friday","channel_id":"`+ch.ID+`"}`)
	list := toolResultList(t, got)
	if len(list) != 2 {
		t.Fatalf("search = %v", list)
	}

	got = callTool(t, set, "context", `{"action":"quote","speaker":"bob","channel_id":"`+ch.ID+`"}`)
	list = toolResultList(t, got)
	if len(list) != 1 || list[0]["text"] != "confirmed, Friday works" {
		t.Fatalf("quote = %v", list)
	}
}

func TestDelegateToolConsult(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)

	// bob is the consultant: his handler answers anything addressed to him.
	bob, err := hc.Register("bob", WithAgentFunc(func(msg string) (string, error) {
		return "bob says: 42", nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()

	alice, err := hc.Register("alice")
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()

	set, err := ClientTools(hc, alice)
	if err != nil {
		t.Fatal(err)
	}

	got := callTool(t, set, "delegate", `{"target":"bob","prompt":"what is the answer?","timeout":5}`)
	if s, _ := got.(string); s != "bob says: 42" {
		t.Fatalf("delegate = %v", got)
	}

	// The consulting channel closed itself on the reply (adapter parity).
	for _, ch := range h.ListChannels() {
		if ch.Manifest.Type == TypeConsulting && !ch.State.Terminal() {
			t.Fatalf("consulting channel %s never closed", ch.ID)
		}
	}
}

func TestDelegateToolUnknownTargetListsPeers(t *testing.T) {
	_, _, _, set := clientToolsFixture(t)

	got := callTool(t, set, "delegate", `{"target":"nobody","prompt":"hi","timeout":1}`)
	s, _ := got.(string)
	if !strings.HasPrefix(s, "Error: target \"nobody\" not found. Available targets: [bob]") {
		t.Fatalf("unknown target must list the directory, got %q", s)
	}
}

func TestDelegateToolFunctionsNamespaceFallback(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)
	bob, err := hc.Register("bob", WithAgentFunc(func(msg string) (string, error) {
		return "pong", nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()
	alice, err := hc.Register("alice")
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()
	set, err := ClientTools(hc, alice)
	if err != nil {
		t.Fatal(err)
	}

	got := callTool(t, set, "delegate", `{"target":"functions.bob","prompt":"ping","timeout":5}`)
	if s, _ := got.(string); s != "pong" {
		t.Fatalf("functions.-prefixed target must resolve, got %v", got)
	}
	_ = h
}

func TestDelegateToolSelfRejected(t *testing.T) {
	_, _, _, set := clientToolsFixture(t)
	got := callTool(t, set, "delegate", `{"target":"alice","prompt":"echo?","timeout":1}`)
	s, _ := got.(string)
	if !strings.HasPrefix(s, "Error: cannot delegate to self") {
		t.Fatalf("self-delegate must fail fast, got %q", s)
	}
}

func TestDelegateToolTimeout(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)
	// bob participates but never answers (no handler registered).
	bob, err := hc.Register("bob", WithAgentFunc(func(msg string) (string, error) { return "", nil }))
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()
	alice, err := hc.Register("alice")
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()
	set, err := ClientTools(hc, alice)
	if err != nil {
		t.Fatal(err)
	}

	got := callTool(t, set, "delegate", `{"target":"bob","prompt":"hello?","timeout":0.3}`)
	s, _ := got.(string)
	if !strings.Contains(s, "timed out after 0.3s") {
		t.Fatalf("timeout must be reported, got %q", s)
	}
	// The timed-out channel is closed so it does not linger.
	for _, ch := range h.ListChannels() {
		if ch.Manifest.Type == TypeConsulting && !ch.State.Terminal() {
			t.Fatalf("timed-out consulting channel %s never closed", ch.ID)
		}
	}
}
