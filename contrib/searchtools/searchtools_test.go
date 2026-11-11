package searchtools

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
)

// newTestContext builds the conversation context a tool runs in.
func newTestContext() (*stream.MemoryStream, *agocontext.Context) {
	s := stream.NewMemoryStream()
	return s, agocontext.New(s)
}

// callTool drives one tool call through the real machinery: Register on the
// stream, the call event, and the settlement.
func callTool(t *testing.T, tool tools.Tool, cx *agocontext.Context, name, args string) events.Event {
	t.Helper()
	subs, err := tool.Register(cx, nil)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, id := range subs {
		defer cx.Stream.Unsubscribe(id)
	}
	s := cx.Stream
	ch := make(chan events.Event, 8)
	sub := s.Subscribe(func(ev events.Event) error { ch <- ev; return nil }, stream.SubscribeOptions{})
	defer s.Unsubscribe(sub)
	call := events.NewToolCallEvent(name)
	call.Arguments = args
	if err := s.Send(call); err != nil {
		t.Fatalf("send call: %v", err)
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-ch:
			switch events.AsValue(ev).(type) {
			case events.ToolResultEvent, events.ToolErrorEvent:
				return ev
			}
		case <-deadline:
			t.Fatal("timed out waiting for the tool result")
		}
	}
}

// firstDataJSON returns the JSON of the first result part's data payload — the
// text the model receives.
func firstDataJSON(t *testing.T, ev events.Event) string {
	t.Helper()
	res, ok := events.AsValue(ev).(events.ToolResultEvent)
	if !ok {
		t.Fatalf("event %T is not a result", ev)
	}
	if len(res.Result.Parts) == 0 {
		t.Fatal("no result parts")
	}
	b, err := json.Marshal(events.AsValue(res.Result.Parts[0]))
	if err != nil {
		t.Fatalf("marshal part: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal part: %v", err)
	}
	data, err := json.Marshal(m["data"])
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	return string(data)
}

// toolErrorText returns the error text of a failed tool call.
func toolErrorText(t *testing.T, ev events.Event) string {
	t.Helper()
	res, ok := events.AsValue(ev).(events.ToolErrorEvent)
	if !ok {
		t.Fatalf("event %T is not a tool error: %s", ev, firstDataJSON(t, ev))
	}
	return res.Error
}

// readBody returns the raw request body.
func readBody(t *testing.T, r *http.Request) string {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// respondJSON writes one canned JSON body with a 200 status.
func respondJSON(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatalf("write response: %v", err)
	}
}

// newTestServer starts a fake provider endpoint and closes it with the test.
func newTestServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// sameJSON asserts that two JSON documents are equal regardless of object key
// order (Go marshals maps sorted, so exact strings are used where the shape is
// small).
func sameJSON(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("decode got %q: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("decode want %q: %v", want, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("json = %s, want %s", got, want)
	}
}
