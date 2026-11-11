package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/luckystrker/ago/internal/sse"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// notes collects the notifications a Client delivers.
type notes struct {
	mu  sync.Mutex
	got []Notification
}

func (n *notes) add(x Notification) {
	n.mu.Lock()
	n.got = append(n.got, x)
	n.mu.Unlock()
}

func (n *notes) all() []Notification {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]Notification(nil), n.got...)
}

func (n *notes) methods() []string {
	var out []string
	for _, x := range n.all() {
		if x.Method == "notifications/stub-hello" {
			continue // the stdio stub greets at initialize
		}
		out = append(out, x.Method)
	}
	return out
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *scenarioHTTP) getCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.log {
		if r.Method == "GET" {
			n++
		}
	}
	return n
}

func (s *scenarioHTTP) replies() []loggedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []loggedRequest
	for _, r := range s.log {
		if r.Reply {
			out = append(out, r)
		}
	}
	return out
}

func TestClientOpensTheStandaloneStreamAfterInitialize(t *testing.T) {
	cl, srv := newScenarioClient(t, overHTTP, false, "", 0)
	if _, err := cl.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	waitUntil(t, "the GET stream", func() bool { return srv.getCount() == 1 })
	srv.mu.Lock()
	var get loggedRequest
	for _, r := range srv.log {
		if r.Method == "GET" {
			get = r
		}
	}
	srv.mu.Unlock()
	if get.Session != "sess-1" || get.Headers["accept"] != "text/event-stream" || get.Headers["last-event-id"] != "" {
		t.Errorf("GET = %+v, want the session id, Accept: text/event-stream and no Last-Event-ID", get)
	}
	// It opens after notifications/initialized, once the session exists.
	methods := srv.methods()
	if len(methods) < 2 || methods[0] != "initialize" || methods[1] != "notifications/initialized" {
		t.Errorf("requests = %v", methods)
	}
}

func TestRequestsOutsideAnyCallAreAnsweredOnTheStandaloneStream(t *testing.T) {
	var n notes
	cl, srv := newScenarioClient(t, overHTTP, false, "", 0, WithNotifications(n.add), WithAnswering(AnswerPolicy{Roots: []string{t.TempDir()}}))
	if _, err := cl.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	waitUntil(t, "the GET stream", func() bool { return srv.getCount() == 1 })

	// A ping while no call is running is answered with an empty result…
	srv.push <- map[string]any{"jsonrpc": "2.0", "id": "p1", "method": "ping"}
	waitUntil(t, "the ping reply", func() bool { return len(srv.replies()) == 1 })
	reply := srv.replies()[0]
	if reply.Session != "sess-1" {
		t.Errorf("reply session = %q, want the session the stream belongs to", reply.Session)
	}
	// …roots, which need no conversation, are answered too…
	srv.push <- map[string]any{"jsonrpc": "2.0", "id": "r1", "method": "roots/list"}
	waitUntil(t, "the roots reply", func() bool { return len(srv.replies()) == 2 })
	// …and a request that does need one (an elicitation for a human) is refused
	// with an error instead of hanging the server.
	srv.push <- map[string]any{"jsonrpc": "2.0", "id": "e1", "method": "elicitation/create", "params": map[string]any{}}
	waitUntil(t, "the elicitation reply", func() bool { return len(srv.replies()) == 3 })

	srv.push <- map[string]any{"jsonrpc": "2.0", "method": "notifications/message", "params": map[string]any{"level": "info", "data": "hello"}}
	waitUntil(t, "the notification", func() bool { return len(n.all()) == 1 })
	if got := n.all()[0]; got.Method != "notifications/message" || string(got.Params) != `{"data":"hello","level":"info"}` {
		t.Errorf("notification = %s %s", got.Method, got.Params)
	}
}

func TestServerWithoutAStandaloneStreamIsLeftAlone(t *testing.T) {
	// 405 is the spec's "no stream here": the client keeps working and does not
	// hammer the endpoint.
	cl, srv := newScenarioClient(t, overHTTP, false, "", 0)
	srv.getStatus = http.StatusMethodNotAllowed
	if _, err := cl.CallTool("echo", nil); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	waitUntil(t, "the GET attempt", func() bool { return srv.getCount() >= 1 })
	time.Sleep(1300 * time.Millisecond) // past the reconnect delay
	if n := srv.getCount(); n != 1 {
		t.Errorf("%d GET attempts against a server that answered 405, want 1", n)
	}
}

func TestCloseStopsTheStandaloneStream(t *testing.T) {
	cl, srv := newScenarioClient(t, overHTTP, false, "", 0)
	if _, err := cl.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	waitUntil(t, "the GET stream", func() bool { return srv.getCount() == 1 })
	done := make(chan struct{})
	go func() {
		cl.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on the open stream")
	}
	time.Sleep(1300 * time.Millisecond)
	if n := srv.getCount(); n != 1 {
		t.Errorf("%d GETs after Close, want the one", n)
	}
}

func TestNoListenerOnStatelessServersOrStdio(t *testing.T) {
	// A server that issues no session id has no stream to listen on.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			t.Error("GET on a session-less server")
			return
		}
		var msg wireMessage
		json.NewDecoder(r.Body).Decode(&msg)
		if !msg.hasID() {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "x", "version": "1"}}})
	}))
	defer srv.Close()
	cl, err := New(&MCPServerConfig{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	if _, err := cl.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
}

func TestNotificationsOnTheCallStream(t *testing.T) {
	for _, kind := range bothKinds {
		t.Run(string(kind), func(t *testing.T) {
			var n notes
			cl, _ := newScenarioClient(t, kind, false, "", 0, WithNotifications(n.add))
			out, err := cl.CallToolMeta(context.Background(), nil, "notify", nil, map[string]any{"progressToken": "t"})
			if err != nil {
				t.Fatalf("CallToolMeta: %v", err)
			}
			if got := firstText(t, out); got != "notified" {
				t.Errorf("result = %q", got)
			}
			// In the order the server sent them, before the result returned.
			if got := n.methods(); len(got) != 2 || got[0] != "notifications/progress" || got[1] != "notifications/message" {
				t.Fatalf("notifications = %v", got)
			}
			var p struct {
				Token    string  `json:"progressToken"`
				Progress float64 `json:"progress"`
				Message  string  `json:"message"`
			}
			var first Notification
			for _, x := range n.all() {
				if x.Method == "notifications/progress" {
					first = x
				}
			}
			if err := json.Unmarshal(first.Params, &p); err != nil || p.Token != "t" || p.Progress != 1 || p.Message != "first" {
				t.Errorf("progress params = %s (%v)", first.Params, err)
			}
		})
	}
}

func TestProgressTokenRidesInMeta(t *testing.T) {
	cl, srv := newScenarioClient(t, overHTTP, false, "", 0)
	if _, err := cl.CallToolMeta(context.Background(), nil, "echo", nil, map[string]any{"progressToken": 7}); err != nil {
		t.Fatalf("CallToolMeta: %v", err)
	}
	var p map[string]json.RawMessage
	_ = json.Unmarshal(srv.requests("tools/call")[0].Params, &p)
	if string(p["_meta"]) != `{"progressToken":7}` {
		t.Errorf("params = %s", srv.requests("tools/call")[0].Params)
	}
	// In the modern era the caller's keys ride beside the envelope, never over it.
	cl2, srv2 := newScenarioClient(t, overHTTP, true, ProtocolAuto, 0)
	if _, err := cl2.CallToolMeta(context.Background(), nil, "echo", nil, map[string]any{"progressToken": 7, metaVersionKey: "bogus"}); err != nil {
		t.Fatalf("modern CallToolMeta: %v", err)
	}
	var m struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	_ = json.Unmarshal(srv2.requests("tools/call")[0].Params, &m)
	if string(m.Meta["progressToken"]) != "7" || string(m.Meta[metaVersionKey]) != `"2026-07-28"` {
		t.Errorf("_meta = %v", m.Meta)
	}
}

func TestRequestReturnsTheRawResult(t *testing.T) {
	cl, srv := newScenarioClient(t, overHTTP, false, "", 0)
	raw, err := cl.Request(context.Background(), "tools/list", map[string]any{})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	var res struct{ Tools []map[string]any }
	if err := json.Unmarshal(raw, &res); err != nil || len(res.Tools) == 0 {
		t.Errorf("result = %s (%v)", raw, err)
	}
	if _, err := cl.Request(context.Background(), "nope/method", nil); !errors.Is(err, ErrProtocol) {
		t.Errorf("an unknown method = %v, want ErrProtocol", err)
	}
	_ = srv
}

func TestResumeAfterTheCallStreamDrops(t *testing.T) {
	var n notes
	cl, srv := newScenarioClient(t, overHTTP, false, "", 0, WithNotifications(n.add))
	start := time.Now()
	out, err := cl.CallTool("cut", nil)
	// The server's retry hint (5ms) replaces the one-second default.
	if elapsed := time.Since(start); elapsed > 700*time.Millisecond {
		t.Errorf("resumed after %v, want the server's retry hint honoured", elapsed)
	}
	if err != nil {
		t.Fatalf("CallTool across a dropped stream: %v", err)
	}
	if got := firstText(t, out); got != "resumed" {
		t.Errorf("result = %q, want the resumed response", got)
	}
	if got := n.methods(); len(got) != 1 || got[0] != "notifications/progress" {
		t.Errorf("notifications = %v, want the one seen before the drop, once", got)
	}
	// The resume was a GET naming the last event the client saw, on the session.
	var resumed []loggedRequest
	srv.mu.Lock()
	for _, r := range srv.log {
		if r.Method == "GET" && r.Headers["last-event-id"] != "" {
			resumed = append(resumed, r)
		}
	}
	srv.mu.Unlock()
	if len(resumed) != 1 || resumed[0].Headers["last-event-id"] != "e1" || resumed[0].Session != "sess-1" {
		t.Errorf("resume requests = %+v", resumed)
	}
}

func TestResumeGivesUpAfterTwoAttempts(t *testing.T) {
	cl, srv := newScenarioClient(t, overHTTP, false, "", 0)
	srv.resumeEnds = true
	_, err := cl.CallTool("cut", nil)
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("err = %v, want ErrProtocol once the reconnect attempts ran out", err)
	}
	srv.mu.Lock()
	resumes := srv.resumes
	srv.mu.Unlock()
	if resumes != maxReconnectAttempts {
		t.Errorf("%d resume attempts, want %d", resumes, maxReconnectAttempts)
	}
}

func TestStreamWithoutEventIDsIsNotResumable(t *testing.T) {
	// No id was ever seen, so there is nothing to resume from: the end of the
	// stream without a response is an error, as before.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var msg wireMessage
		json.NewDecoder(r.Body).Decode(&msg)
		if !msg.hasID() {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if msg.Method == "initialize" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", "s")
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "x", "version": "1"}}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{}}\n\n"))
	}))
	defer srv.Close()
	cl, _ := New(&MCPServerConfig{URL: srv.URL})
	defer cl.Close()
	if _, err := cl.CallTool("echo", nil); !errors.Is(err, ErrProtocol) {
		t.Errorf("err = %v, want ErrProtocol", err)
	}
}

func TestEventReader(t *testing.T) {
	src := ": comment\nid: 7\nretry: 250\nevent: message\ndata: {\"a\":1}\n\nid: 8\ndata: \n\ndata: line1\ndata: line2\n\nretry: bogus\ndata: x\n\n"
	er := newEventReader(stringsReader(src), 1<<20)
	var got []sseEvent
	for er.next() {
		got = append(got, er.event())
	}
	if len(got) != 4 {
		t.Fatalf("%d events: %+v", len(got), got)
	}
	if got[0].ID != "7" || got[0].Retry != 250 || got[0].Name != "message" || string(got[0].Data) != `{"a":1}` || !got[0].HasData {
		t.Errorf("event 0 = %+v", got[0])
	}
	// A priming event: an id and an empty data line.
	if got[1].ID != "8" || got[1].HasData || got[1].Retry != -1 {
		t.Errorf("priming = %+v", got[1])
	}
	if string(got[2].Data) != "line1\nline2" {
		t.Errorf("multi-line data = %q", got[2].Data)
	}
	if got[3].Retry != -1 {
		t.Errorf("a malformed retry was accepted: %+v", got[3])
	}
	// retry: 0 is a valid hint (reconnect at once), distinct from "no hint".
	er = newEventReader(stringsReader("retry: 0\ndata: x\n\n"), 1<<20)
	if !er.next() || er.event().Retry != 0 {
		t.Errorf("retry: 0 read as %+v", er.event())
	}
	// A cap of zero (or less) means no cap, not "nothing fits".
	er = newEventReader(stringsReader("data: "+strings.Repeat("x", 4096)+"\n\n"), 0)
	if !er.next() || len(er.event().Data) != 4096 {
		t.Error("an event was refused under an unlimited cap")
	}
	// A line the scanner cannot hold is a read failure, not a clean end.
	er = newEventReader(stringsReader("data: "+strings.Repeat("y", 9<<20)+"\n\n"), 0)
	for er.next() {
	}
	if er.failure() == nil {
		t.Error("an over-long line ended the stream cleanly")
	}
	// The cap fails with the shared error.
	er = newEventReader(stringsReader("data: 0123456789\n\n"), 4)
	for er.next() {
	}
	if err := er.failure(); err == nil {
		t.Error("an oversized event was delivered")
	}
}

func stringsReader(s string) *strings.Reader { return strings.NewReader(s) }

func TestPrimingEventIsACursorNotAMessage(t *testing.T) {
	cl, _ := newScenarioClient(t, overHTTP, false, "", 0)
	out, err := cl.CallTool("primed", nil)
	if err != nil {
		t.Fatalf("CallTool over a primed stream: %v", err)
	}
	if got := firstText(t, out); got != "primed ok" {
		t.Errorf("result = %q", got)
	}
}

func TestScopeKeepsTheFirstFailure(t *testing.T) {
	s := &callScope{}
	first, second := errors.New("first"), errors.New("second")
	s.fail(first)
	s.fail(second)
	if !errors.Is(s.failed(), first) {
		t.Errorf("failed() = %v, want the first failure", s.failed())
	}
	var none *callScope
	none.fail(first) // a missing scope is a no-op, not a panic
	if none.failed() != nil {
		t.Error("a nil scope reports a failure")
	}
}

func TestResumeAfterABrokenConnection(t *testing.T) {
	// The connection dying mid-stream (a read error, not a clean end) is a
	// disconnect too: the client resumes from the last event it saw.
	cl, srv := newScenarioClient(t, overHTTP, false, "", 0)
	out, err := cl.CallTool("rst", nil)
	if err != nil {
		t.Fatalf("CallTool across a reset connection: %v", err)
	}
	if got := firstText(t, out); got != "resumed" {
		t.Errorf("result = %q", got)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.resumes != 1 {
		t.Errorf("%d resumes, want 1", srv.resumes)
	}
}

func TestOversizeEventIsNotResumedFrom(t *testing.T) {
	// An event over the cap is a hostile or broken server, not a dropped
	// connection: the call fails with the size error and nothing is replayed.
	srv := newScenarioHTTP(false)
	hs := httptest.NewServer(srv)
	defer hs.Close()
	cl, err := New(&MCPServerConfig{URL: hs.URL, MaxSSEEventSize: 512})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	_, err = cl.CallTool("huge", nil)
	if !errors.Is(err, sse.ErrEventTooLarge) {
		t.Fatalf("err = %v, want ErrEventTooLarge", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.resumes != 0 {
		t.Errorf("%d resume attempts after an oversize event", srv.resumes)
	}
}
