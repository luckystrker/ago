package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luckystrker/ago/agent"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// gatedModel streams "a", blocks on the gate, then streams "b" and answers: a turn
// that is demonstrably still running while its client is gone.
func gatedModel(gate <-chan struct{}) *scriptedModel {
	return &scriptedModel{steps: []func(*agocontext.Context) *events.ModelResponse{
		func(conv *agocontext.Context) *events.ModelResponse {
			_ = conv.Send(&events.ModelMessageChunk{Content: "a"})
			<-gate
			_ = conv.Send(&events.ModelMessageChunk{Content: "b"})
			return &events.ModelResponse{Message: &events.ModelMessage{Content: "done"}}
		},
	}}
}

func progressMessages(l turnLog) []string {
	var out []string
	for _, m := range l.ofMethod("notifications/progress") {
		out = append(out, fmt.Sprint(m["params"].(map[string]any)["message"]))
	}
	return out
}

// startDroppedTurn posts a streamed tools/call, reads until the first progress
// notification and drops the connection. It returns the id of the last event the
// "client" saw, plus the priming event (when the server sent one).
func startDroppedTurn(t *testing.T, url, sid string) (lastID string, priming httpMsg) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":42,"method":"tools/call","params":`+callAsk+`}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", sid)
	if v, ok := sessionVersions.Load(sid); ok {
		req.Header.Set("MCP-Protocol-Version", v.(string))
	}
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	ch := streamEvents(resp.Body)
	for {
		ev := nextEvent(t, ch)
		if ev.Data == "" {
			priming = ev
			lastID = ev.ID
			continue
		}
		if ev.ID != "" {
			lastID = ev.ID
		}
		if strings.Contains(ev.Data, "notifications/progress") {
			break
		}
	}
	cancel() // the client goes away mid-turn
	resp.Body.Close()
	return lastID, priming
}

func resumeGet(t *testing.T, url, sid, lastID string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Mcp-Session-Id", sid)
	req.Header.Set("Last-Event-ID", lastID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("resume GET = %d: %s", resp.StatusCode, readAll(t, resp.Body))
	}
	return resp
}

func TestResumeReplaysWhatTheDroppedClientMissed(t *testing.T) {
	gate := make(chan struct{})
	model := gatedModel(gate)
	store := NewMemoryEventStore(0)
	tr := statefulStreaming(store)
	tr.SSERetryInterval = 250 * time.Millisecond
	a := agent.New("served", "", agent.WithConfig(model))
	_, ts := serveAgent(t, a, tr)
	sid := openSession(t, ts.URL, `{}`, "2025-11-25")

	lastID, priming := startDroppedTurn(t, ts.URL, sid)
	// The stream opened with the priming cursor: an id, the retry hint, no payload.
	if priming.ID == "" || priming.Data != "" {
		t.Errorf("priming event = %+v", priming)
	}
	if lastID == priming.ID {
		t.Error("the progress event carried no id of its own")
	}
	// The turn is still running with nobody listening; let it finish.
	close(gate)
	waitFor(t, "the dropped turn to finish", func() bool { return model.callCount() == 1 && storeHasFinal(store, sid) })

	resp := resumeGet(t, ts.URL, sid, lastID)
	defer resp.Body.Close()
	evs := readEvents(t, resp.Body)
	var methods []string
	for _, ev := range evs {
		m := decodeObj(t, ev.Data)
		if m["method"] != nil {
			methods = append(methods, fmt.Sprint(m["method"]))
		} else if m["id"] != 42.0 {
			t.Errorf("replayed a response for id %v", m["id"])
		}
		if ev.ID == "" {
			t.Errorf("a replayed event lost its id: %+v", ev)
		}
	}
	// Everything after "a": the second chunk, then the response, in order, each once.
	last := decodeObj(t, evs[len(evs)-1].Data)
	if last["result"] == nil || last["id"] != 42.0 {
		t.Fatalf("the replay did not end with the response: %v", last)
	}
	if got := strings.Join(methods, ","); got != "notifications/progress" {
		t.Errorf("replayed methods = %v, want exactly the missed progress chunk", methods)
	}
	if p := decodeObj(t, evs[0].Data)["params"].(map[string]any); p["message"] != "b" || p["progress"] != 2.0 {
		t.Errorf("first replayed event = %v, want chunk b with progress 2", p)
	}
}

// storeHasFinal reports whether the stored stream of a session holds a response.
func storeHasFinal(store *MemoryEventStore, sid string) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	for key, evs := range store.streams {
		if !strings.HasPrefix(key, sid+"/") {
			continue
		}
		for _, ev := range evs {
			if strings.Contains(string(ev.msg), `"result"`) {
				return true
			}
		}
	}
	return false
}

func TestResumeWhileTheTurnIsStillRunningAttachesLive(t *testing.T) {
	gate := make(chan struct{})
	model := gatedModel(gate)
	store := NewMemoryEventStore(0)
	a := agent.New("served", "", agent.WithConfig(model))
	_, ts := serveAgent(t, a, statefulStreaming(store))
	sid := openSession(t, ts.URL, `{}`, "2025-11-25")

	lastID, _ := startDroppedTurn(t, ts.URL, sid)
	resp := resumeGet(t, ts.URL, sid, lastID)
	defer resp.Body.Close()
	ch := streamEvents(resp.Body)
	// Nothing missed yet; the rest of the turn arrives on the new connection.
	close(gate)
	var got []string
	for {
		ev := nextEvent(t, ch)
		m := decodeObj(t, ev.Data)
		if m["method"] != nil {
			got = append(got, fmt.Sprint(m["params"].(map[string]any)["message"]))
			continue
		}
		if m["id"] == 42.0 && m["result"] != nil {
			break
		}
	}
	if strings.Join(got, ",") != "b" {
		t.Errorf("live tail = %v, want [b]", got)
	}
	// The stream ends with the request.
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("events after the response")
		}
	case <-time.After(5 * time.Second):
		t.Error("the resumed stream stayed open after the response")
	}
}

func TestResumeFromThePrimingCursorReplaysEverything(t *testing.T) {
	// A client that drops before any event arrives still has the priming id, which
	// is why the cursor is minted ahead of the dispatch.
	store := NewMemoryEventStore(0)
	model := &scriptedModel{steps: []func(*agocontext.Context) *events.ModelResponse{streamThenSay("done", "a", "b")}}
	a := agent.New("served", "", agent.WithConfig(model))
	_, ts := serveAgent(t, a, statefulStreaming(store))
	sid := openSession(t, ts.URL, `{}`, "2025-11-25")
	log := callTurn(t, ts.URL, sid, callAsk, nil)
	primingID := log.ids[0]
	if primingID == "" {
		t.Fatal("no priming event")
	}
	resp := resumeGet(t, ts.URL, sid, primingID)
	defer resp.Body.Close()
	evs := readEvents(t, resp.Body)
	if len(evs) != len(log.ids)-1 {
		t.Errorf("replayed %d events, want the %d that followed the priming event", len(evs), len(log.ids)-1)
	}
	for i, ev := range evs {
		if ev.ID != log.ids[i+1] {
			t.Errorf("replayed event %d has id %q, was %q", i, ev.ID, log.ids[i+1])
		}
	}
}

func TestResumeRefusals(t *testing.T) {
	store := NewMemoryEventStore(0)
	model := &scriptedModel{steps: []func(*agocontext.Context) *events.ModelResponse{streamThenSay("done", "a"), say("again")}}
	a := agent.New("served", "", agent.WithConfig(model))
	_, ts := serveAgent(t, a, statefulStreaming(store))
	sid := openSession(t, ts.URL, `{}`, "2025-11-25")
	other := openSession(t, ts.URL, `{}`, "2025-11-25")
	log := callTurn(t, ts.URL, sid, callAsk, nil)

	// An id the store never held, and an id of another session's stream, resume
	// nothing: an empty stream, not a leak.
	for name, tc := range map[string]struct{ sid, id string }{
		"unknown id":           {sid, "999999:nope"},
		"another session's id": {other, log.ids[1]},
	} {
		resp := resumeGet(t, ts.URL, tc.sid, tc.id)
		if evs := readEvents(t, resp.Body); len(evs) != 0 {
			t.Errorf("%s replayed %d events", name, len(evs))
		}
		resp.Body.Close()
	}
}

func TestEventIDsAreOnlyMintedWithAStore(t *testing.T) {
	model := &scriptedModel{steps: []func(*agocontext.Context) *events.ModelResponse{streamThenSay("done", "a")}}
	a := agent.New("served", "", agent.WithConfig(model))
	_, ts := serveAgent(t, a, statefulStreaming(nil))
	sid := openSession(t, ts.URL, `{}`, "2025-11-25")
	log := callTurn(t, ts.URL, sid, callAsk, nil)
	for _, id := range log.ids {
		if id != "" {
			t.Errorf("event id %q without an event store", id)
		}
	}
	resp := resumeGet(t, ts.URL, sid, "1:x")
	defer resp.Body.Close()
	if evs := readEvents(t, resp.Body); len(evs) != 0 {
		t.Errorf("a store-less server replayed %d events", len(evs))
	}
}

func TestOldClientsGetNoPrimingEvent(t *testing.T) {
	// Only 2025-11-25 clients parse the empty-data priming event.
	for _, version := range []string{"2024-11-05", "2025-03-26", "2025-06-18"} {
		t.Run(version, func(t *testing.T) {
			store := NewMemoryEventStore(0)
			model := &scriptedModel{steps: []func(*agocontext.Context) *events.ModelResponse{streamThenSay("done", "a")}}
			a := agent.New("served", "", agent.WithConfig(model))
			_, ts := serveAgent(t, a, statefulStreaming(store))
			sid := openSession(t, ts.URL, `{}`, version)
			log := callTurn(t, ts.URL, sid, callAsk, nil)
			if len(log.ids) < 2 {
				t.Fatalf("ids = %v", log.ids)
			}
			for i, id := range log.ids {
				if id == "" {
					t.Errorf("event %d carries no id: stored events keep theirs for any client", i)
				}
			}
			// callTurn records a priming event's id too; count them from the wire.
			resp := post(t, ts.URL, map[string]string{"Mcp-Session-Id": sid}, `{"jsonrpc":"2.0","id":8,"method":"ping"}`)
			defer resp.Body.Close()
			for _, ev := range readEvents(t, resp.Body) {
				if ev.Data == "" {
					t.Errorf("a %s client was sent a priming event it cannot parse: %+v", version, ev)
				}
			}
		})
	}
	t.Run("the header decides on later requests", func(t *testing.T) {
		store := NewMemoryEventStore(0)
		model := &scriptedModel{steps: []func(*agocontext.Context) *events.ModelResponse{say("a")}}
		a := agent.New("served", "", agent.WithConfig(model))
		_, ts := serveAgent(t, a, statefulStreaming(store))
		sid := openSession(t, ts.URL, `{}`, "2025-11-25")
		resp := post(t, ts.URL, map[string]string{"Mcp-Session-Id": sid, "MCP-Protocol-Version": "2025-11-25"}, `{"jsonrpc":"2.0","id":3,"method":"ping"}`)
		defer resp.Body.Close()
		evs := readEvents(t, resp.Body)
		if len(evs) != 2 || evs[0].Data != "" || evs[0].ID == "" {
			t.Errorf("events = %+v, want a priming event then the response", evs)
		}
	})
}

func TestStandaloneGetStream(t *testing.T) {
	s, ts := fullServer(t, statefulStreaming(nil), func(c *ServerConfig) { c.Transport.SessionIdleTimeout = 100 * time.Millisecond })
	sid := openSession(t, ts.URL, `{}`, "")
	get := func() *http.Response {
		req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Mcp-Session-Id", sid)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	first := get()
	if first.StatusCode != 200 || first.Header.Get("Content-Type") != "text/event-stream" || first.Header.Get("Mcp-Session-Id") != sid {
		t.Fatalf("GET = %d %v", first.StatusCode, first.Header)
	}
	// Only one standalone stream per session.
	second := get()
	second.Body.Close()
	if second.StatusCode != 409 {
		t.Errorf("second GET = %d, want 409", second.StatusCode)
	}
	// An open stream holds the session open past its idle timeout.
	time.Sleep(250 * time.Millisecond)
	if msg, _ := rpcCall(t, ts.URL, map[string]string{"Mcp-Session-Id": sid}, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); msg["error"] != nil {
		t.Errorf("the session was reaped under an open stream: %v", msg)
	}
	// DELETE ends the session and closes its stream.
	req, _ := http.NewRequest(http.MethodDelete, ts.URL, nil)
	req.Header.Set("Mcp-Session-Id", sid)
	del, _ := http.DefaultClient.Do(req)
	del.Body.Close()
	done := make(chan struct{})
	go func() {
		readAll(t, first.Body)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the standalone stream stayed open after DELETE")
	}
	first.Body.Close()
	if n := s.sessions.count(); n != 0 {
		t.Errorf("%d sessions after DELETE", n)
	}
}

func TestServerCloseEndsRunningTurnsAndStreams(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	model := gatedModel(gate)
	store := NewMemoryEventStore(0)
	a := agent.New("served", "", agent.WithConfig(model))
	s, ts := serveAgent(t, a, statefulStreaming(store))
	sid := openSession(t, ts.URL, `{}`, "2025-11-25")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		startDroppedTurn(t, ts.URL, sid)
	}()
	wg.Wait()
	// An open standalone stream must end with the server.
	getReq, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	getReq.Header.Set("Accept", "text/event-stream")
	getReq.Header.Set("Mcp-Session-Id", sid)
	stream, err := http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	ended := make(chan struct{})
	go func() {
		readAll(t, stream.Body)
		close(ended)
	}()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Error("Close left the standalone stream open")
	}
	r := post(t, ts.URL, map[string]string{"Mcp-Session-Id": sid}, `{"jsonrpc":"2.0","id":5,"method":"ping"}`)
	r.Body.Close()
	if r.StatusCode != 404 {
		t.Errorf("after Close, the session answers %d, want 404", r.StatusCode)
	}
}

func TestARunningRequestHoldsItsSessionOpen(t *testing.T) {
	// The idle clock runs only while nothing is in flight: a turn longer than the
	// idle timeout must not have its session reaped from under it.
	gate := make(chan struct{})
	model := gatedModel(gate)
	a := agent.New("served", "", agent.WithConfig(model))
	_, ts := serveAgent(t, a, TransportConfig{Stateful: true, StreamResponses: true, SessionIdleTimeout: 80 * time.Millisecond})
	sid := openSession(t, ts.URL, `{}`, "")
	done := make(chan turnLog, 1)
	go func() { done <- callTurn(t, ts.URL, sid, callAsk, nil) }()
	waitFor(t, "the turn to start", func() bool { return model.callCount() == 1 })
	time.Sleep(300 * time.Millisecond) // several idle timeouts, with the turn running
	if msg, _ := rpcCall(t, ts.URL, map[string]string{"Mcp-Session-Id": sid}, `{"jsonrpc":"2.0","id":2,"method":"ping"}`); msg["error"] != nil {
		t.Errorf("the session was reaped under a running turn: %v", msg)
	}
	close(gate)
	if got := finalText(t, <-done); got != "done" {
		t.Errorf("final = %q", got)
	}
}

func TestJSONAnswersStoreNothing(t *testing.T) {
	// A JSON body carries only the response: the notifications of the turn have no
	// wire and no replay, so they are dropped rather than parked in the store.
	store := NewMemoryEventStore(0)
	model := &scriptedModel{steps: []func(*agocontext.Context) *events.ModelResponse{streamThenSay("done", "a", "b")}}
	a := agent.New("served", "", agent.WithConfig(model))
	_, ts := serveAgent(t, a, TransportConfig{Stateful: true, EventStore: store})
	sid := openSession(t, ts.URL, `{}`, "2025-11-25")
	msg, _ := rpcCall(t, ts.URL, map[string]string{"Mcp-Session-Id": sid}, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":`+callAsk+`}`)
	if msg["result"] == nil {
		t.Fatalf("call failed: %v", msg)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.streams) != 0 {
		t.Errorf("a JSON-answering server stored %d streams", len(store.streams))
	}
}

// hookStore runs a hook once, right after the first replay a client asks for.
type hookStore struct {
	*MemoryEventStore
	once  sync.Once
	after func()
}

func (h *hookStore) ReplayEventsAfter(ctx context.Context, id string, send func(string, json.RawMessage) error) (string, error) {
	key, err := h.MemoryEventStore.ReplayEventsAfter(ctx, id, send)
	h.once.Do(func() {
		if h.after != nil {
			h.after()
		}
	})
	return key, err
}

func TestResumeCatchesTheTurnThatFinishesBetweenReplayAndAttach(t *testing.T) {
	// The request completes in the window after the first replay pass and before
	// the server looks its exchange up: its last chunk and its response were
	// stored after the snapshot and must still reach the reconnecting client.
	gate := make(chan struct{})
	model := gatedModel(gate)
	store := &hookStore{MemoryEventStore: NewMemoryEventStore(0)}
	a := agent.New("served", "", agent.WithConfig(model))
	s, ts := serveAgent(t, a, statefulStreaming(store))
	sid := openSession(t, ts.URL, `{}`, "2025-11-25")
	store.after = func() {
		close(gate)
		sess, _ := s.sessions.get(sid)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			sess.mu.Lock()
			n := len(sess.exchanges)
			sess.mu.Unlock()
			if n == 0 && storeHasFinal(store.MemoryEventStore, sid) {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	lastID, _ := startDroppedTurn(t, ts.URL, sid)
	resp := resumeGet(t, ts.URL, sid, lastID)
	defer resp.Body.Close()
	evs := readEvents(t, resp.Body)
	if len(evs) != 2 {
		t.Fatalf("%d events replayed, want the missed chunk and the response: %+v", len(evs), evs)
	}
	if p := decodeObj(t, evs[0].Data)["params"].(map[string]any); p["message"] != "b" {
		t.Errorf("first event = %v", p)
	}
	if last := decodeObj(t, evs[1].Data); last["id"] != 42.0 || last["result"] == nil {
		t.Errorf("the replay does not end with the response: %v", last)
	}
}

func TestEventStoreIsBoundedAndFollowsTheSession(t *testing.T) {
	store := NewMemoryEventStore(0)
	model := &scriptedModel{steps: []func(*agocontext.Context) *events.ModelResponse{say("a"), say("b"), say("c")}}
	a := agent.New("served", "", agent.WithConfig(model))
	_, ts := serveAgent(t, a, statefulStreaming(store))
	sid := openSession(t, ts.URL, `{}`, "2025-11-25")
	other := openSession(t, ts.URL, `{}`, "2025-11-25")
	callTurn(t, ts.URL, sid, callAsk, nil)
	callTurn(t, ts.URL, other, callAsk, nil)
	if n := store.streamCount(); n != 4 {
		t.Fatalf("%d streams held, want one per request (initialize and the call, for each session)", n)
	}
	// Ending a session forgets its streams, and only its streams.
	req, _ := http.NewRequest(http.MethodDelete, ts.URL, nil)
	req.Header.Set("Mcp-Session-Id", sid)
	del, _ := http.DefaultClient.Do(req)
	del.Body.Close()
	waitFor(t, "the ended session's streams to go", func() bool { return store.streamCount() == 2 })
	store.mu.Lock()
	for key := range store.streams {
		if !strings.HasPrefix(key, other+"/") {
			t.Errorf("stream %q outlived its session", key)
		}
	}
	store.mu.Unlock()
	for id, stream := range store.owner {
		if !strings.HasPrefix(stream, other+"/") {
			t.Errorf("event %q of the forgotten stream %q is still indexed", id, stream)
		}
	}
}

func TestMemoryEventStoreDropsTheStreamWrittenLongestAgo(t *testing.T) {
	st := NewMemoryEventStore(0).WithMaxStreams(2)
	ctx := context.Background()
	a1, _ := st.StoreEvent(ctx, "a", []byte(`1`))
	st.StoreEvent(ctx, "b", []byte(`2`))
	st.StoreEvent(ctx, "a", []byte(`3`)) // a is now the most recently written
	st.StoreEvent(ctx, "c", []byte(`4`)) // over the bound: b goes
	if n := st.streamCount(); n != 2 {
		t.Errorf("%d streams, want the bound of 2", n)
	}
	if _, err := st.ReplayEventsAfter(ctx, a1, func(string, json.RawMessage) error { return nil }); err != nil {
		t.Errorf("the recently written stream was dropped: %v", err)
	}
	st.mu.Lock()
	_, hasB := st.streams["b"]
	owners := len(st.owner)
	st.mu.Unlock()
	if hasB {
		t.Error("the stream written longest ago was kept")
	}
	if owners != 3 { // a's two events and c's one: nothing indexed for the dropped stream
		t.Errorf("%d event ids indexed, want 3", owners)
	}
	if st.WithMaxStreams(0).maxStreams != defaultStoreStreams {
		t.Error("a non-positive bound did not restore the default")
	}
}

func TestStatelessStreamingStoresNothing(t *testing.T) {
	store := NewMemoryEventStore(0)
	model := &scriptedModel{steps: []func(*agocontext.Context) *events.ModelResponse{streamThenSay("done", "a")}}
	a := agent.New("served", "", agent.WithConfig(model))
	_, ts := serveAgent(t, a, TransportConfig{StreamResponses: true, EventStore: store})
	call(t, ts.URL, "", "initialize", 1, `{"protocolVersion":"2025-11-25","capabilities":{}}`)
	log := callTurn(t, ts.URL, "", callAsk, nil)
	if finalText(t, log) != "done" {
		t.Fatalf("final = %v", log.final)
	}
	if n := store.streamCount(); n != 0 {
		t.Errorf("a stateless transport stored %d streams: nothing could ever resume them", n)
	}
	for _, id := range log.ids {
		if id != "" {
			t.Errorf("event id %q on a stateless stream", id)
		}
	}
}

func TestMemoryEventStoreEvictsInWriteOrder(t *testing.T) {
	// Twenty streams through a bound of five: exactly the five written last
	// survive, whatever order the map is walked in.
	st := NewMemoryEventStore(0).WithMaxStreams(5)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		st.StoreEvent(ctx, fmt.Sprintf("s%02d", i), []byte(`1`))
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.streams) != 5 {
		t.Fatalf("%d streams, want 5", len(st.streams))
	}
	for i := 15; i < 20; i++ {
		if _, ok := st.streams[fmt.Sprintf("s%02d", i)]; !ok {
			t.Errorf("stream s%02d, written recently, was dropped", i)
		}
	}
	if len(st.owner) != 5 || len(st.written) != 5 {
		t.Errorf("indexes hold %d event ids and %d write marks, want 5 each", len(st.owner), len(st.written))
	}
}

func TestMemoryEventStoreEvictsExactlyTheOldestStreams(t *testing.T) {
	// Bound 3, five streams written in order: exactly the two written longest ago
	// go. Repeated, because a victim picked from an unordered walk would pass any
	// single run by luck.
	for round := 0; round < 40; round++ {
		st := NewMemoryEventStore(0).WithMaxStreams(3)
		ctx := context.Background()
		for _, id := range []string{"s0", "s1", "s2", "s3", "s4"} {
			st.StoreEvent(ctx, id, []byte(`1`))
		}
		st.mu.Lock()
		var kept []string
		for id := range st.streams {
			kept = append(kept, id)
		}
		st.mu.Unlock()
		sort.Strings(kept)
		if strings.Join(kept, ",") != "s2,s3,s4" {
			t.Fatalf("round %d: kept %v, want exactly the three written last", round, kept)
		}
	}
	// A stream written again counts as recent: it outlives a newer-but-idle one.
	st := NewMemoryEventStore(0).WithMaxStreams(3)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		st.StoreEvent(ctx, id, []byte(`1`))
	}
	st.StoreEvent(ctx, "a", []byte(`2`)) // a is now the most recent
	st.StoreEvent(ctx, "d", []byte(`1`)) // evicts b, the longest idle
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.streams["b"]; ok || len(st.streams) != 3 {
		t.Errorf("streams = %v, want b evicted", st.streams)
	}
}
