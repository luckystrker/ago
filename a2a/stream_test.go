package a2a

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// chunkSpec is one artifact chunk of a scripted streaming handler.
type chunkSpec struct {
	artifact    Artifact
	appendParts bool
	lastChunk   bool
}

// artifactHandler is a HandleStream that emits the chunks in order and then
// completes (or fails).
func artifactHandler(chunks []chunkSpec, fail bool) func(context.Context, *Message, StreamEmitter) (*Message, error) {
	return func(_ context.Context, msg *Message, emit StreamEmitter) (*Message, error) {
		for _, chunk := range chunks {
			if err := emit.Artifact(chunk.artifact, chunk.appendParts, chunk.lastChunk); err != nil {
				return nil, err
			}
		}
		if fail {
			return nil, errors.New("boom")
		}
		return NewMessage(RoleAgent, NewTextPart("reply")), nil
	}
}

// testClient builds a Client against base on one binding. A nil transport
// keeps the default one.
func testClient(t *testing.T, base, binding string, transport http.RoundTripper, opts ...ClientOption) *Client {
	t.Helper()
	hc := &http.Client{Timeout: 10 * time.Second}
	if transport != nil {
		hc.Transport = transport
	}
	options := []ClientOption{
		WithHTTPClient(hc),
		WithReconnectBackoff(time.Millisecond),
		WithPollingInterval(time.Millisecond),
	}
	if binding == TransportREST {
		card := &AgentCard{
			Name:         "tester",
			Capabilities: AgentCapabilities{Streaming: true},
			SupportedInterfaces: []AgentInterface{{
				URL:             base,
				ProtocolBinding: ProtocolBindingREST,
				ProtocolVersion: ProtocolVersionCurrent,
			}},
		}
		options = append(options, WithPresetCard(card), WithTransport(TransportREST))
	}
	options = append(options, opts...)
	return NewClient(base, options...)
}

// drainStream reads a stream to its EOF.
func drainStream(t *testing.T, ctx context.Context, stream *TaskStream) []*TaskEvent {
	t.Helper()
	var events []*TaskEvent
	for {
		event, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			t.Fatalf("stream.Next: %v", err)
		}
		events = append(events, event)
	}
}

// mustNext reads one stream event and fails the test when the stream errors.
func mustNext(t *testing.T, ctx context.Context, stream *TaskStream) *TaskEvent {
	t.Helper()
	event, err := stream.Next(ctx)
	if err != nil {
		t.Fatalf("stream.Next: %v", err)
	}
	return event
}

// eventKind names the item a TaskEvent carries.
func eventKind(event *TaskEvent) string {
	switch {
	case event.Task != nil:
		return "task"
	case event.Status != nil:
		return "status"
	case event.Artifact != nil:
		return "artifact"
	case event.Message != nil:
		return "message"
	}
	return "empty"
}

// artifactText returns the concatenated text parts of the artifact with id.
func artifactText(t *testing.T, artifacts []Artifact, id string) string {
	t.Helper()
	for _, artifact := range artifacts {
		if artifact.ArtifactID != id {
			continue
		}
		var b strings.Builder
		for _, part := range artifact.Parts {
			if tp, ok := part.(*TextPart); ok {
				b.WriteString(tp.Text)
			}
		}
		return b.String()
	}
	t.Fatalf("artifact %q not found in %+v", id, artifacts)
	return ""
}

// assertViewMatchesStored checks the wave-4 invariant-7 half a client can see:
// the view a finished stream left equals the server's stored task in what the
// stream carries — status and artifacts.
func assertViewMatchesStored(t *testing.T, store TaskStore, stream *TaskStream) *Task {
	t.Helper()
	view := stream.View()
	if view == nil || view.ID == "" {
		t.Fatal("stream view names no task")
	}
	stored, err := store.Get(context.Background(), view.ID)
	if err != nil {
		t.Fatalf("store.Get(%q): %v", view.ID, err)
	}
	if view.Status.State != stored.Status.State {
		t.Fatalf("view state = %q, stored = %q", view.Status.State, stored.Status.State)
	}
	if got, want := textOf(view.Status.Message), textOf(stored.Status.Message); got != want {
		t.Fatalf("view status.message = %q, stored = %q", got, want)
	}
	got, err := json.Marshal(view.Artifacts)
	if err != nil {
		t.Fatalf("marshal view artifacts: %v", err)
	}
	want, err := json.Marshal(stored.Artifacts)
	if err != nil {
		t.Fatalf("marshal stored artifacts: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("view artifacts = %s, stored = %s", got, want)
	}
	return stored
}

// TestClientStreamArtifacts is the end-to-end artifact stream on both bindings:
// working, the chunks in order, the terminal state, and a view that equals the
// server's record.
func TestClientStreamArtifacts(t *testing.T) {
	for _, binding := range []string{TransportJSONRPC, TransportREST} {
		t.Run(binding, func(t *testing.T) {
			store := NewMemoryTaskStore()
			srv := NewServer(ServerConfig{
				Card:  testCard(true),
				Store: store,
				HandleStream: artifactHandler([]chunkSpec{
					{artifact: Artifact{ArtifactID: "a1", Name: "answer", Parts: []Part{NewTextPart("Hel")}}, appendParts: true},
					{artifact: Artifact{ArtifactID: "a1", Parts: []Part{NewTextPart("lo")}}, appendParts: true, lastChunk: true},
					{artifact: Artifact{ArtifactID: "a2", Parts: []Part{NewTextPart("note")}}},
				}, false),
			})
			server := httptest.NewServer(srv.Handler())
			defer server.Close()

			ctx := context.Background()
			stream, err := testClient(t, server.URL, binding, nil).
				MessageStream(ctx, NewMessage(RoleUser, NewTextPart("go")))
			if err != nil {
				t.Fatalf("MessageStream: %v", err)
			}
			defer stream.Close()

			events := drainStream(t, ctx, stream)
			kinds := make([]string, len(events))
			for i, event := range events {
				kinds[i] = eventKind(event)
			}
			want := []string{"status", "artifact", "artifact", "artifact", "status"}
			if !reflect.DeepEqual(kinds, want) {
				t.Fatalf("stream items = %v, want %v", kinds, want)
			}
			if working := events[0].Status; working.Status.State != TaskStateWorking || working.Final {
				t.Fatalf("first item = %+v, want a non-final working update", working)
			}
			final := events[len(events)-1].Status
			if !final.Final || final.Status.State != TaskStateCompleted {
				t.Fatalf("last item = %+v, want the final completed update", final)
			}
			if got := textOf(final.Status.Message); got != "reply" {
				t.Fatalf("final status.message = %q, want the reply", got)
			}
			view := assertViewMatchesStored(t, store, stream)
			if got := artifactText(t, view.Artifacts, "a1"); got != "Hello" {
				t.Fatalf("artifact a1 = %q, want Hello", got)
			}
			if got := artifactText(t, view.Artifacts, "a2"); got != "note" {
				t.Fatalf("artifact a2 = %q, want note", got)
			}
			if view.Artifacts[0].Name != "answer" {
				t.Fatalf("artifact a1 name = %q", view.Artifacts[0].Name)
			}
		})
	}
}

// TestClientStreamFailedTurn: a failing streaming turn ends on a failed status
// whose message is the stored one.
func TestClientStreamFailedTurn(t *testing.T) {
	store := NewMemoryTaskStore()
	srv := NewServer(ServerConfig{
		Card:         testCard(true),
		Store:        store,
		HandleStream: artifactHandler([]chunkSpec{{artifact: Artifact{ArtifactID: "a1", Parts: []Part{NewTextPart("partial")}}}}, true),
	})
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	ctx := context.Background()
	stream, err := testClient(t, server.URL, TransportJSONRPC, nil).
		MessageStream(ctx, NewMessage(RoleUser, NewTextPart("go")))
	if err != nil {
		t.Fatalf("MessageStream: %v", err)
	}
	defer stream.Close()
	drainStream(t, ctx, stream)

	view := assertViewMatchesStored(t, store, stream)
	if view.Status.State != TaskStateFailed {
		t.Fatalf("view state = %q, want failed", view.Status.State)
	}
	if got := textOf(view.Status.Message); !strings.Contains(got, "boom") {
		t.Fatalf("view status.message = %q, want the handler failure", got)
	}
	if got := artifactText(t, view.Artifacts, "a1"); got != "partial" {
		t.Fatalf("artifact a1 = %q, want the chunk the failure followed", got)
	}
}

// TestResubscribeOpensWithCurrentTask is the A1/A3 regression: a resubscribe
// taken while the turn is blocked opens with the server's current task (the
// chunks already produced), not with nothing and not with the client's older
// view.
func TestResubscribeOpensWithCurrentTask(t *testing.T) {
	for _, binding := range []string{TransportJSONRPC, TransportREST} {
		t.Run(binding, func(t *testing.T) {
			store := NewMemoryTaskStore()
			release := make(chan struct{})
			emitted := make(chan struct{}, 1)
			srv := NewServer(ServerConfig{
				Card:  testCard(true),
				Store: store,
				HandleStream: func(_ context.Context, _ *Message, emit StreamEmitter) (*Message, error) {
					if err := emit.Artifact(Artifact{ArtifactID: "a1", Parts: []Part{NewTextPart("first")}}, true, false); err != nil {
						return nil, err
					}
					emitted <- struct{}{}
					<-release
					if err := emit.Artifact(Artifact{ArtifactID: "a1", Parts: []Part{NewTextPart("-second")}}, true, true); err != nil {
						return nil, err
					}
					return NewMessage(RoleAgent, NewTextPart("done")), nil
				},
			})
			server := httptest.NewServer(srv.Handler())
			defer server.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			client := testClient(t, server.URL, binding, nil)
			stream, err := client.MessageStream(ctx, NewMessage(RoleUser, NewTextPart("go")))
			if err != nil {
				t.Fatalf("MessageStream: %v", err)
			}
			defer stream.Close()

			if event := mustNext(t, ctx, stream); event.Status == nil || event.Status.Status.State != TaskStateWorking {
				t.Fatalf("first item = %+v, want the working update", event)
			}
			if event := mustNext(t, ctx, stream); event.Artifact == nil {
				t.Fatalf("second item = %+v, want the first chunk", event)
			}
			<-emitted
			taskID := stream.TaskID()
			if taskID == "" {
				t.Fatal("stream names no task")
			}

			sub, err := client.Resubscribe(ctx, taskID)
			if err != nil {
				t.Fatalf("Resubscribe: %v", err)
			}
			defer sub.Close()
			opening := mustNext(t, ctx, sub)
			if opening.Task == nil {
				t.Fatalf("resubscribe opened with %q, want a task snapshot", eventKind(opening))
			}
			if opening.Task.ID != taskID {
				t.Fatalf("snapshot task = %q, want %q", opening.Task.ID, taskID)
			}
			if opening.Task.Status.State != TaskStateWorking {
				t.Fatalf("snapshot state = %q, want working", opening.Task.Status.State)
			}
			if got := artifactText(t, opening.Task.Artifacts, "a1"); got != "first" {
				t.Fatalf("snapshot artifact = %q, want the chunk produced before the resubscribe", got)
			}

			close(release)
			var sawChunk, sawFinal bool
			for {
				event, err := sub.Next(ctx)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("resubscribed stream: %v", err)
				}
				switch {
				case event.Artifact != nil:
					sawChunk = true
				case event.Status != nil && event.Status.Final:
					sawFinal = true
				}
			}
			if !sawChunk || !sawFinal {
				t.Fatalf("resubscribed stream: chunk = %v, final = %v", sawChunk, sawFinal)
			}
			if _, err := sub.Next(ctx); !errors.Is(err, io.EOF) {
				t.Fatalf("stream after the terminal state: %v, want io.EOF (A2/A4)", err)
			}
			if got := artifactText(t, sub.View().Artifacts, "a1"); got != "first-second" {
				t.Fatalf("resubscribed view artifact = %q", got)
			}
			drainStream(t, ctx, stream)
			assertViewMatchesStored(t, store, stream)
		})
	}
}

// TestResubscribeTerminalTaskReturnsFinalState is the A5 regression: a
// resubscribe on a finished task answers its final state once and closes.
func TestResubscribeTerminalTaskReturnsFinalState(t *testing.T) {
	for _, binding := range []string{TransportJSONRPC, TransportREST} {
		t.Run(binding, func(t *testing.T) {
			store := NewMemoryTaskStore()
			srv := NewServer(ServerConfig{
				Card:         testCard(true),
				Store:        store,
				HandleStream: artifactHandler([]chunkSpec{{artifact: Artifact{ArtifactID: "a1", Parts: []Part{NewTextPart("done")}}}}, false),
			})
			server := httptest.NewServer(srv.Handler())
			defer server.Close()

			ctx := context.Background()
			client := testClient(t, server.URL, binding, nil)
			stream, err := client.MessageStream(ctx, NewMessage(RoleUser, NewTextPart("go")))
			if err != nil {
				t.Fatalf("MessageStream: %v", err)
			}
			drainStream(t, ctx, stream)
			taskID := stream.TaskID()
			stream.Close()

			sub, err := client.Resubscribe(ctx, taskID)
			if err != nil {
				t.Fatalf("Resubscribe: %v", err)
			}
			defer sub.Close()
			events := drainStream(t, ctx, sub)
			if len(events) != 1 || events[0].Task == nil {
				t.Fatalf("terminal resubscribe = %v, want exactly one Task snapshot", events)
			}
			if events[0].Task.Status.State != TaskStateCompleted {
				t.Fatalf("snapshot state = %q, want completed", events[0].Task.Status.State)
			}
			if got := artifactText(t, events[0].Task.Artifacts, "a1"); got != "done" {
				t.Fatalf("snapshot artifact = %q", got)
			}
			if _, err := sub.Next(ctx); !errors.Is(err, io.EOF) {
				t.Fatalf("after the terminal snapshot: %v, want io.EOF (A5)", err)
			}
		})
	}
}

// TestResubscribeUnknownTask: an unknown task is an error, on both bindings.
func TestResubscribeUnknownTask(t *testing.T) {
	srv := NewServer(ServerConfig{Card: testCard(true), Store: NewMemoryTaskStore()})
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	ctx := context.Background()
	for _, binding := range []string{TransportJSONRPC, TransportREST} {
		t.Run(binding, func(t *testing.T) {
			_, err := testClient(t, server.URL, binding, nil).Resubscribe(ctx, "no-such-task")
			if !errors.Is(err, ErrTaskNotFound) {
				t.Fatalf("Resubscribe(unknown) = %v, want ErrTaskNotFound", err)
			}
		})
	}
}

// TestServerResubscribeUnknownTask pins the raw wire answers: -32001 on
// JSON-RPC, 404 on REST.
func TestServerResubscribeUnknownTask(t *testing.T) {
	srv := NewServer(ServerConfig{Card: testCard(true), Store: NewMemoryTaskStore()})
	h := srv.Handler()

	envelope := postRPC(t, h, rpcBody(MethodTasksResubscribe, `{"id":"absent"}`, `"1"`))
	if envelope.Error == nil || envelope.Error.Code != codeTaskNotFound {
		t.Fatalf("tasks/resubscribe(absent) = %+v, want code %d", envelope.Error, codeTaskNotFound)
	}

	req := httptest.NewRequest(http.MethodGet, RESTTasksPath+"/absent"+RESTSubscribeSuffix, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /tasks/absent:subscribe = %d, want 404", rec.Code)
	}
}

// TestServerMessageStreamArtifacts pins the raw JSON-RPC SSE body of a
// streaming turn with chunks: working, artifact updates, one final status
// last (A4), and the chunks stored on the task.
func TestServerMessageStreamArtifacts(t *testing.T) {
	store := NewMemoryTaskStore()
	srv := NewServer(ServerConfig{
		Card:  testCard(true),
		Store: store,
		HandleStream: artifactHandler([]chunkSpec{
			{artifact: Artifact{ArtifactID: "a1", Parts: []Part{NewTextPart("one")}}, appendParts: true},
			{artifact: Artifact{ArtifactID: "a1", Parts: []Part{NewTextPart("two")}}, appendParts: true, lastChunk: true},
		}, false),
	})
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodPost, rpcPath, strings.NewReader(
		rpcBody(MethodMessageStream, textMessageParams("go"), `"7"`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	envelopes := decodeSSE(t, rec.Body.String())
	if len(envelopes) != 4 {
		t.Fatalf("SSE events = %d, want 4: %s", len(envelopes), rec.Body.String())
	}
	var taskID string
	var kinds []string
	for _, envelope := range envelopes {
		if envelope.Error != nil {
			t.Fatalf("stream error: %s", envelope.Error)
		}
		var probe struct {
			Kind   string `json:"kind"`
			TaskID string `json:"taskId"`
		}
		if err := json.Unmarshal(envelope.Result, &probe); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		kinds = append(kinds, probe.Kind)
		taskID = probe.TaskID
	}
	want := []string{KindStatusUpdate, KindArtifactUpdate, KindArtifactUpdate, KindStatusUpdate}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("event kinds = %v, want %v", kinds, want)
	}
	var final StatusUpdate
	if err := json.Unmarshal(envelopes[len(envelopes)-1].Result, &final); err != nil {
		t.Fatalf("decode final: %v", err)
	}
	if !final.Final || final.Status.State != TaskStateCompleted {
		t.Fatalf("final event = %+v", final)
	}
	task, err := store.Get(context.Background(), taskID)
	if err != nil {
		t.Fatalf("store.Get(%q): %v", taskID, err)
	}
	if got := artifactText(t, task.Artifacts, "a1"); got != "onetwo" {
		t.Fatalf("stored artifact = %q, want the streamed chunks", got)
	}
}

// TestServerRESTMessageStream drives POST /message:stream: the SSE body is the
// bare items (no JSON-RPC envelope).
func TestServerRESTMessageStream(t *testing.T) {
	store := NewMemoryTaskStore()
	srv := NewServer(ServerConfig{
		Card:  testCard(true),
		Store: store,
		HandleStream: artifactHandler([]chunkSpec{
			{artifact: Artifact{ArtifactID: "a1", Parts: []Part{NewTextPart("rest")}}, appendParts: true, lastChunk: true},
		}, false),
	})
	h := srv.Handler()

	body := marshalMessage(t, NewMessage(RoleUser, NewTextPart("go")))
	req := httptest.NewRequest(http.MethodPost, RESTMessageStreamPath, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	items := decodeRESTSSE(t, rec.Body.String())
	if len(items) != 3 {
		t.Fatalf("SSE items = %d, want 3: %s", len(items), rec.Body.String())
	}
	var update ArtifactUpdate
	if err := json.Unmarshal(items[1], &update); err != nil {
		t.Fatalf("decode artifact update: %v (%s)", err, items[1])
	}
	if update.Kind != KindArtifactUpdate || update.Artifact.ArtifactID != "a1" || !update.LastChunk {
		t.Fatalf("artifact update = %+v", update)
	}
}

// decodeRESTSSE splits an SSE body into the bare JSON items of the HTTP+JSON
// binding.
func decodeRESTSSE(t *testing.T, body string) []json.RawMessage {
	t.Helper()
	var items []json.RawMessage
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		line, ok := strings.CutPrefix(block, "data: ")
		if !ok {
			t.Fatalf("SSE block without a data line: %q", block)
		}
		items = append(items, json.RawMessage(line))
	}
	return items
}

// recordingTransport records every request and can break an SSE body after a
// number of events, which is what a dropped connection looks like to the
// client.
type recordingTransport struct {
	base http.RoundTripper
	// plan[i] is how many complete SSE events request i delivers before its
	// connection breaks; -1 keeps the body intact.
	plan []int

	mu       sync.Mutex
	calls    int
	requests []recordedRequest
	breaks   atomic.Int64
}

type recordedRequest struct {
	method string
	url    string
	body   string
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	index := r.calls
	r.calls++
	record := recordedRequest{method: req.Method, url: req.URL.RequestURI()}
	if req.Body != nil {
		body, _ := io.ReadAll(req.Body)
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(body))
		record.body = string(body)
	}
	r.requests = append(r.requests, record)
	r.mu.Unlock()

	resp, err := r.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if index < len(r.plan) && r.plan[index] >= 0 {
		resp.Body = &droppingBody{inner: resp.Body, after: r.plan[index], breaks: &r.breaks}
	}
	return resp, nil
}

func (r *recordingTransport) sawRequest(fragment string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, request := range r.requests {
		if strings.Contains(request.body, fragment) || strings.Contains(request.url, fragment) {
			return true
		}
	}
	return false
}

var errTestDropped = errors.New("test: connection dropped")

// droppingBody delivers `after` complete SSE events and then reports a broken
// connection, discarding whatever followed in the same read.
type droppingBody struct {
	inner  io.ReadCloser
	after  int
	events int
	broken bool
	breaks *atomic.Int64
}

func (b *droppingBody) Read(p []byte) (int, error) {
	if b.broken {
		return 0, errTestDropped
	}
	if b.after <= 0 {
		b.broken = true
		b.breaks.Add(1)
		return 0, errTestDropped
	}
	n, err := b.inner.Read(p)
	if n == 0 {
		return n, err
	}
	start := 0
	for {
		idx := bytes.Index(p[start:n], []byte("\n\n"))
		if idx < 0 {
			return n, err
		}
		b.events++
		end := start + idx + len("\n\n")
		if b.events >= b.after {
			b.broken = true
			b.breaks.Add(1)
			return end, nil
		}
		start = end
	}
}

func (b *droppingBody) Close() error { return b.inner.Close() }

// TestStreamReconnectAfterDrop is the end-to-end dropped-connection test on
// both bindings: the opening stream breaks mid-turn, the client resubscribes,
// the resubscribe opens with the server's current task, and the turn still
// ends with the terminal state exactly once and a view equal to the server's.
func TestStreamReconnectAfterDrop(t *testing.T) {
	for _, binding := range []string{TransportJSONRPC, TransportREST} {
		t.Run(binding, func(t *testing.T) {
			store := NewMemoryTaskStore()
			release := make(chan struct{})
			srv := NewServer(ServerConfig{
				Card:  testCard(true),
				Store: store,
				HandleStream: func(_ context.Context, _ *Message, emit StreamEmitter) (*Message, error) {
					if err := emit.Artifact(Artifact{ArtifactID: "a1", Parts: []Part{NewTextPart("first")}}, true, false); err != nil {
						return nil, err
					}
					if err := emit.Artifact(Artifact{ArtifactID: "a1", Parts: []Part{NewTextPart("-second")}}, true, true); err != nil {
						return nil, err
					}
					<-release
					return NewMessage(RoleAgent, NewTextPart("done")), nil
				},
			})
			server := httptest.NewServer(srv.Handler())
			defer server.Close()
			var releaseOnce sync.Once
			releaseNow := func() { releaseOnce.Do(func() { close(release) }) }
			defer releaseNow()

			transport := &recordingTransport{base: http.DefaultTransport, plan: []int{3}}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			stream, err := testClient(t, server.URL, binding, transport).
				MessageStream(ctx, NewMessage(RoleUser, NewTextPart("go")))
			if err != nil {
				t.Fatalf("MessageStream: %v", err)
			}
			defer stream.Close()

			// working + both chunks, then the connection breaks; the client
			// resubscribes and blocks on the snapshot, which already carries
			// both chunks.
			var events []*TaskEvent
			for len(events) < 4 {
				event, err := stream.Next(ctx)
				if err != nil {
					t.Fatalf("stream.Next: %v", err)
				}
				events = append(events, event)
			}
			kinds := make([]string, len(events))
			for i, event := range events {
				kinds[i] = eventKind(event)
			}
			if !reflect.DeepEqual(kinds, []string{"status", "artifact", "artifact", "task"}) {
				t.Fatalf("items before and after the drop = %v, want working, two chunks, then a resubscribe snapshot", kinds)
			}
			if got := artifactText(t, events[3].Task.Artifacts, "a1"); got != "first-second" {
				t.Fatalf("resubscribe snapshot artifact = %q", got)
			}

			releaseNow()
			rest := drainStream(t, ctx, stream)
			if len(rest) != 1 || rest[0].Status == nil || !rest[0].Status.Final {
				t.Fatalf("items after the release = %v, want exactly the terminal update", rest)
			}
			if transport.breaks.Load() != 1 {
				t.Fatalf("transport breaks = %d, want 1", transport.breaks.Load())
			}
			if !transport.sawRequest(MethodTasksResubscribe) && !transport.sawRequest(RESTSubscribeSuffix) {
				t.Fatalf("no resubscribe request: %+v", transport.requests)
			}
			assertViewMatchesStored(t, store, stream)
		})
	}
}

// countsByKind counts the stream items by kind.
func countsByKind(events []*TaskEvent) map[string]int {
	counts := make(map[string]int)
	for _, event := range events {
		counts[eventKind(event)]++
	}
	return counts
}

// TestClientStreamIgnoresEventsAfterTerminal is the A4 regression on the
// client: a server that keeps sending after the terminal event must not reach
// the caller.
func TestClientStreamIgnoresEventsAfterTerminal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		writeJSONRPCEvent(w, flusher, &StatusUpdate{
			Kind: KindStatusUpdate, TaskID: "t1", ContextID: "c1",
			Status: TaskStatus{State: TaskStateWorking},
		})
		writeJSONRPCEvent(w, flusher, &StatusUpdate{
			Kind: KindStatusUpdate, TaskID: "t1", ContextID: "c1", Final: true,
			Status: TaskStatus{State: TaskStateCompleted},
		})
		writeJSONRPCEvent(w, flusher, &ArtifactUpdate{
			Kind: KindArtifactUpdate, TaskID: "t1", ContextID: "c1",
			Artifact: Artifact{ArtifactID: "late", Parts: []Part{NewTextPart("after the end")}},
		})
	}))
	defer server.Close()

	ctx := context.Background()
	stream, err := NewClient(server.URL).MessageStream(ctx, NewMessage(RoleUser, NewTextPart("go")))
	if err != nil {
		t.Fatalf("MessageStream: %v", err)
	}
	defer stream.Close()
	events := drainStream(t, ctx, stream)
	if len(events) != 2 {
		t.Fatalf("delivered items = %d, want 2 (nothing after the terminal state)", len(events))
	}
	if !events[1].settled() {
		t.Fatalf("last item = %+v, want the terminal update", events[1])
	}
	if artifacts := stream.View().Artifacts; len(artifacts) != 0 {
		t.Fatalf("view artifacts = %+v, want nothing from after the terminal state", artifacts)
	}
}

// writeJSONRPCEvent writes one SSE event of the JSON-RPC binding. It runs on a
// server goroutine, so it must not fail the test itself.
func writeJSONRPCEvent(w io.Writer, flusher http.Flusher, item any) {
	result, err := json.Marshal(item)
	if err != nil {
		return
	}
	envelope, err := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("1"), Result: result})
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", envelope)
	flusher.Flush()
}

// TestClientStreamRejectsForeignTask: an event of another task is a wire
// error, not silently folded in — for every kind of stream item.
func TestClientStreamRejectsForeignTask(t *testing.T) {
	foreign := map[string]any{
		"status": &StatusUpdate{
			Kind: KindStatusUpdate, TaskID: "t2", ContextID: "c1",
			Status: TaskStatus{State: TaskStateCompleted},
		},
		"artifact": &ArtifactUpdate{
			Kind: KindArtifactUpdate, TaskID: "t2", ContextID: "c1",
			Artifact: Artifact{ArtifactID: "a", Parts: []Part{NewTextPart("x")}},
		},
		"message": &Message{
			Role: RoleAgent, TaskID: "t2", ContextID: "c1", MessageID: "m2",
			Parts: []Part{NewTextPart("x")},
		},
	}
	for kind, item := range foreign {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Content-Type"); got != "application/json" {
					t.Errorf("stream request Content-Type = %q, want application/json", got)
				}
				if got := r.Header.Get("Accept"); got != "text/event-stream" {
					t.Errorf("stream request Accept = %q, want text/event-stream", got)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				flusher := w.(http.Flusher)
				writeJSONRPCEvent(w, flusher, &StatusUpdate{
					Kind: KindStatusUpdate, TaskID: "t1", ContextID: "c1",
					Status: TaskStatus{State: TaskStateWorking},
				})
				writeJSONRPCEvent(w, flusher, item)
			}))
			defer server.Close()

			ctx := context.Background()
			stream, err := NewClient(server.URL).MessageStream(ctx, NewMessage(RoleUser, NewTextPart("go")))
			if err != nil {
				t.Fatalf("MessageStream: %v", err)
			}
			defer stream.Close()
			if _, err := stream.Next(ctx); err != nil {
				t.Fatalf("first item: %v", err)
			}
			if _, err := stream.Next(ctx); !errors.Is(err, ErrWire) {
				t.Fatalf("%s event of another task = %v, want ErrWire", kind, err)
			}
		})
	}
}

// TestClientStreamPollsWhenStreamEndsEarly: a stream that closes before the
// terminal state is read back with tasks/get, like the Python client.
func TestClientStreamPollsWhenStreamEndsEarly(t *testing.T) {
	var gets atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case bytes.Contains(body, []byte(MethodMessageStream)):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			writeJSONRPCEvent(w, flusher, &StatusUpdate{
				Kind: KindStatusUpdate, TaskID: "t1", ContextID: "c1",
				Status: TaskStatus{State: TaskStateWorking},
			})
		case bytes.Contains(body, []byte(MethodTasksGet)):
			state := TaskStateWorking
			if gets.Add(1) >= 2 {
				state = TaskStateCompleted
			}
			writeJSONRPCResult(w, &Task{ID: "t1", ContextID: "c1", Status: TaskStatus{State: state}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, body)
		}
	}))
	defer server.Close()

	ctx := context.Background()
	stream, err := testClient(t, server.URL, TransportJSONRPC, nil, WithPollingInterval(time.Millisecond)).
		MessageStream(ctx, NewMessage(RoleUser, NewTextPart("go")))
	if err != nil {
		t.Fatalf("MessageStream: %v", err)
	}
	defer stream.Close()
	events := drainStream(t, ctx, stream)
	if gets.Load() != 2 {
		t.Fatalf("tasks/get calls = %d, want 2", gets.Load())
	}
	kinds := make([]string, len(events))
	for i, event := range events {
		kinds[i] = eventKind(event)
	}
	if !reflect.DeepEqual(kinds, []string{"status", "task", "task"}) {
		t.Fatalf("items = %v, want the working update then two polled snapshots", kinds)
	}
	if stream.View().Status.State != TaskStateCompleted {
		t.Fatalf("view state = %q, want completed", stream.View().Status.State)
	}
}

// TestClientStreamingOffPolls: WithStreaming(false) sends synchronously and
// polls the task to its terminal state.
func TestClientStreamingOffPolls(t *testing.T) {
	var gets atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case bytes.Contains(body, []byte(MethodMessageSend)):
			writeJSONRPCResult(w, &Task{ID: "t1", ContextID: "c1", Status: TaskStatus{State: TaskStateWorking}})
		case bytes.Contains(body, []byte(MethodTasksGet)):
			gets.Add(1)
			writeJSONRPCResult(w, &Task{ID: "t1", ContextID: "c1", Status: TaskStatus{State: TaskStateCompleted}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, body)
		}
	}))
	defer server.Close()

	ctx := context.Background()
	stream, err := testClient(t, server.URL, TransportJSONRPC, nil,
		WithStreaming(false), WithPollingInterval(time.Millisecond)).
		MessageStream(ctx, NewMessage(RoleUser, NewTextPart("go")))
	if err != nil {
		t.Fatalf("MessageStream: %v", err)
	}
	defer stream.Close()
	events := drainStream(t, ctx, stream)
	if gets.Load() != 1 {
		t.Fatalf("tasks/get calls = %d, want 1", gets.Load())
	}
	kinds := make([]string, len(events))
	for i, event := range events {
		kinds[i] = eventKind(event)
	}
	if !reflect.DeepEqual(kinds, []string{"task", "task"}) {
		t.Fatalf("items = %v, want the send result then the polled snapshot", kinds)
	}
	if stream.View().Status.State != TaskStateCompleted {
		t.Fatalf("view state = %q", stream.View().Status.State)
	}
}

// writeJSONRPCResult writes one JSON-RPC result reply. It runs on a server
// goroutine, so it must not fail the test itself.
func writeJSONRPCResult(w http.ResponseWriter, result any) {
	data, err := json.Marshal(result)
	if err != nil {
		return
	}
	writeRPC(w, &rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("1"), Result: data})
}

// TestResubscribeRespectsTenant: a task stream is resolved through the tenant's
// store, so another tenant sees the task as unknown — on both bindings.
func TestResubscribeRespectsTenant(t *testing.T) {
	stores := newTenantStores("acme", "beta")
	srv := httptest.NewServer(NewServer(ServerConfig{
		Card:           testCard(true),
		HandleStream:   artifactHandler(nil, false),
		StoreForTenant: stores.resolve,
	}).Handler())
	defer srv.Close()

	ctx := context.Background()
	for _, binding := range []string{TransportJSONRPC, TransportREST} {
		t.Run(binding, func(t *testing.T) {
			owner := testClient(t, srv.URL, binding, nil, WithTenant("acme"))
			stream, err := owner.MessageStream(ctx, NewMessage(RoleUser, NewTextPart("go")))
			if err != nil {
				t.Fatalf("MessageStream: %v", err)
			}
			drainStream(t, ctx, stream)
			taskID := stream.TaskID()
			stream.Close()

			sub, err := owner.Resubscribe(ctx, taskID)
			if err != nil {
				t.Fatalf("owner resubscribe: %v", err)
			}
			drainStream(t, ctx, sub)
			sub.Close()

			_, err = testClient(t, srv.URL, binding, nil, WithTenant("beta")).Resubscribe(ctx, taskID)
			if !errors.Is(err, ErrTaskNotFound) {
				t.Fatalf("foreign tenant resubscribe = %v, want ErrTaskNotFound", err)
			}
		})
	}
}

// TestResubscribeFanOut: two streams on one task each get the current snapshot
// and every later event, and both end at the terminal state.
func TestResubscribeFanOut(t *testing.T) {
	store := NewMemoryTaskStore()
	release := make(chan struct{})
	srv := NewServer(ServerConfig{
		Card:  testCard(true),
		Store: store,
		HandleStream: func(_ context.Context, _ *Message, emit StreamEmitter) (*Message, error) {
			if err := emit.Artifact(Artifact{ArtifactID: "a1", Parts: []Part{NewTextPart("first")}}, true, false); err != nil {
				return nil, err
			}
			<-release
			if err := emit.Artifact(Artifact{ArtifactID: "a1", Parts: []Part{NewTextPart("-second")}}, true, true); err != nil {
				return nil, err
			}
			return NewMessage(RoleAgent, NewTextPart("done")), nil
		},
	})
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := testClient(t, server.URL, TransportJSONRPC, nil)
	stream, err := client.MessageStream(ctx, NewMessage(RoleUser, NewTextPart("go")))
	if err != nil {
		t.Fatalf("MessageStream: %v", err)
	}
	defer stream.Close()
	mustNext(t, ctx, stream) // working
	mustNext(t, ctx, stream) // first chunk
	taskID := stream.TaskID()

	first, err := client.Resubscribe(ctx, taskID)
	if err != nil {
		t.Fatalf("first Resubscribe: %v", err)
	}
	defer first.Close()
	second, err := client.Resubscribe(ctx, taskID)
	if err != nil {
		t.Fatalf("second Resubscribe: %v", err)
	}
	defer second.Close()
	for i, sub := range []*TaskStream{first, second} {
		opening := mustNext(t, ctx, sub)
		if opening.Task == nil || artifactText(t, opening.Task.Artifacts, "a1") != "first" {
			t.Fatalf("subscriber %d opening = %+v, want the current task", i, opening)
		}
	}

	close(release)
	for i, sub := range []*TaskStream{first, second} {
		events := drainStream(t, ctx, sub)
		if len(events) != 2 || events[0].Artifact == nil || events[1].Status == nil || !events[1].Status.Final {
			t.Fatalf("subscriber %d items = %v, want the second chunk then the terminal update", i, events)
		}
		if got := artifactText(t, sub.View().Artifacts, "a1"); got != "first-second" {
			t.Fatalf("subscriber %d view artifact = %q", i, got)
		}
	}
}

// TestMessageSendAdvancesLiveStream: a turn committed by another request (a
// plain message/send continuation) reaches the live stream of the task as a
// snapshot and closes it at the terminal state.
func TestMessageSendAdvancesLiveStream(t *testing.T) {
	store := NewMemoryTaskStore()
	if err := store.Save(context.Background(), &Task{
		ID: "t1", ContextID: "c1", Status: TaskStatus{State: TaskStateWorking},
	}); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	srv := NewServer(ServerConfig{Card: testCard(true), Store: store, Handle: echoHandler})
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := testClient(t, server.URL, TransportJSONRPC, nil)
	sub, err := client.Resubscribe(ctx, "t1")
	if err != nil {
		t.Fatalf("Resubscribe: %v", err)
	}
	defer sub.Close()
	opening := mustNext(t, ctx, sub)
	if opening.Task == nil || opening.Task.Status.State != TaskStateWorking {
		t.Fatalf("opening = %+v, want the working snapshot", opening)
	}

	msg := NewMessage(RoleUser, NewTextPart("continue"))
	msg.TaskID = "t1"
	if _, err := client.SendMessage(ctx, msg); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	events := drainStream(t, ctx, sub)
	if len(events) != 1 || events[0].Task == nil {
		t.Fatalf("items after the continuation = %v, want one snapshot", events)
	}
	if events[0].Task.Status.State != TaskStateCompleted {
		t.Fatalf("snapshot state = %q, want completed", events[0].Task.Status.State)
	}
	if got := textOf(events[0].Task.Status.Message); got != "echo: continue" {
		t.Fatalf("snapshot status.message = %q", got)
	}
}

// TestClientStreamEndsOnInterruption: a turn parked input-required ends the
// stream (the task hands control back to the caller) instead of polling a task
// that will not move on its own.
func TestClientStreamEndsOnInterruption(t *testing.T) {
	var polls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(MethodTasksGet)) {
			polls.Add(1)
			writeJSONRPCResult(w, &Task{ID: "t1", ContextID: "c1", Status: TaskStatus{State: TaskStateInputRequired}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		writeJSONRPCEvent(w, flusher, &StatusUpdate{
			Kind: KindStatusUpdate, TaskID: "t1", ContextID: "c1",
			Status: TaskStatus{State: TaskStateWorking},
		})
		writeJSONRPCEvent(w, flusher, &StatusUpdate{
			Kind: KindStatusUpdate, TaskID: "t1", ContextID: "c1", Final: true,
			Status: TaskStatus{State: TaskStateInputRequired, Message: NewMessage(RoleAgent, NewTextPart("which one?"))},
		})
	}))
	defer server.Close()

	ctx := context.Background()
	stream, err := testClient(t, server.URL, TransportJSONRPC, nil).
		MessageStream(ctx, NewMessage(RoleUser, NewTextPart("go")))
	if err != nil {
		t.Fatalf("MessageStream: %v", err)
	}
	defer stream.Close()
	events := drainStream(t, ctx, stream)
	if len(events) != 2 || !events[1].settled() {
		t.Fatalf("items = %v, want the working update then the input-required one", events)
	}
	if polls.Load() != 0 {
		t.Fatalf("tasks/get calls = %d, want none after an interruption", polls.Load())
	}
	if stream.View().Status.State != TaskStateInputRequired {
		t.Fatalf("view state = %q, want input-required", stream.View().Status.State)
	}
}

// TestClientStreamReconnectBudget: the reconnect limit is a limit — once the
// resubscribes are spent the stream reports ErrReconnect instead of retrying
// forever (parity: A2AReconnectError).
func TestClientStreamReconnectBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		if bytes.Contains(body, []byte(MethodTasksResubscribe)) {
			writeJSONRPCEvent(w, flusher, &Task{
				ID: "t1", ContextID: "c1", Status: TaskStatus{State: TaskStateWorking},
			})
			return
		}
		writeJSONRPCEvent(w, flusher, &StatusUpdate{
			Kind: KindStatusUpdate, TaskID: "t1", ContextID: "c1",
			Status: TaskStatus{State: TaskStateWorking},
		})
	}))
	defer server.Close()

	transport := &recordingTransport{base: http.DefaultTransport, plan: []int{1, 1}}
	ctx := context.Background()
	stream, err := testClient(t, server.URL, TransportJSONRPC, transport, WithMaxReconnects(1)).
		MessageStream(ctx, NewMessage(RoleUser, NewTextPart("go")))
	if err != nil {
		t.Fatalf("MessageStream: %v", err)
	}
	defer stream.Close()
	if event := mustNext(t, ctx, stream); event.Status == nil {
		t.Fatalf("first item = %+v, want the working update", event)
	}
	if event := mustNext(t, ctx, stream); event.Task == nil {
		t.Fatalf("second item = %+v, want the resubscribe snapshot", event)
	}
	if _, err := stream.Next(ctx); !errors.Is(err, ErrReconnect) {
		t.Fatalf("Next after the reconnect budget = %v, want ErrReconnect", err)
	}
}

// TestClientStreamRetriesFailedResubscribe: a resubscribe that fails to open
// (503 while the server restarts) is an attempt like any other — the client
// pauses with backoff and tries again, and the turn still reaches its terminal
// state (parity: the Python client retries the SDK error inside the loop).
func TestClientStreamRetriesFailedResubscribe(t *testing.T) {
	var resubscribes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case bytes.Contains(body, []byte(MethodMessageStream)):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			writeJSONRPCEvent(w, w.(http.Flusher), &StatusUpdate{
				Kind: KindStatusUpdate, TaskID: "t1", ContextID: "c1",
				Status: TaskStatus{State: TaskStateWorking},
			})
			// The client's transport breaks this body after the event.
		case bytes.Contains(body, []byte(MethodTasksResubscribe)):
			if resubscribes.Add(1) == 1 {
				http.Error(w, "server restarting", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			writeJSONRPCEvent(w, flusher, &Task{
				ID: "t1", ContextID: "c1", Status: TaskStatus{State: TaskStateWorking},
			})
			writeJSONRPCEvent(w, flusher, &StatusUpdate{
				Kind: KindStatusUpdate, TaskID: "t1", ContextID: "c1", Final: true,
				Status: TaskStatus{State: TaskStateCompleted},
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, body)
		}
	}))
	defer server.Close()

	transport := &recordingTransport{base: http.DefaultTransport, plan: []int{1}}
	ctx := context.Background()
	start := time.Now()
	stream, err := testClient(t, server.URL, TransportJSONRPC, transport,
		WithMaxReconnects(3), WithReconnectBackoff(20*time.Millisecond)).
		MessageStream(ctx, NewMessage(RoleUser, NewTextPart("go")))
	if err != nil {
		t.Fatalf("MessageStream: %v", err)
	}
	defer stream.Close()
	drainStream(t, ctx, stream)
	if got := resubscribes.Load(); got != 2 {
		t.Fatalf("tasks/resubscribe calls = %d, want 2 (the failed open then the retry)", got)
	}
	// Attempt 1 waits 20ms, attempt 2 waits 40ms: a failed open must pause.
	if elapsed := time.Since(start); elapsed < 55*time.Millisecond {
		t.Fatalf("retries took %v, want the two backoff pauses at least", elapsed)
	}
	if stream.View().Status.State != TaskStateCompleted {
		t.Fatalf("view state = %q, want completed", stream.View().Status.State)
	}
}

// TestClientStreamReconnectBudgetOnFailedOpen: when even the resubscribes only
// fail, the budget is spent and the client reports ErrReconnect — after
// max_reconnects attempts, each with its pause.
func TestClientStreamReconnectBudgetOnFailedOpen(t *testing.T) {
	var resubscribes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case bytes.Contains(body, []byte(MethodMessageStream)):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			writeJSONRPCEvent(w, w.(http.Flusher), &StatusUpdate{
				Kind: KindStatusUpdate, TaskID: "t1", ContextID: "c1",
				Status: TaskStatus{State: TaskStateWorking},
			})
		case bytes.Contains(body, []byte(MethodTasksResubscribe)):
			resubscribes.Add(1)
			http.Error(w, "server restarting", http.StatusServiceUnavailable)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, body)
		}
	}))
	defer server.Close()

	transport := &recordingTransport{base: http.DefaultTransport, plan: []int{1}}
	ctx := context.Background()
	start := time.Now()
	stream, err := testClient(t, server.URL, TransportJSONRPC, transport,
		WithMaxReconnects(3), WithReconnectBackoff(10*time.Millisecond)).
		MessageStream(ctx, NewMessage(RoleUser, NewTextPart("go")))
	if err != nil {
		t.Fatalf("MessageStream: %v", err)
	}
	defer stream.Close()
	if _, err := stream.Next(ctx); err != nil {
		t.Fatalf("first item: %v", err)
	}
	if _, err := stream.Next(ctx); !errors.Is(err, ErrReconnect) {
		t.Fatalf("Next after the failed resubscribes = %v, want ErrReconnect", err)
	}
	if got := resubscribes.Load(); got != 3 {
		t.Fatalf("tasks/resubscribe calls = %d, want the whole budget (3)", got)
	}
	if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
		t.Fatalf("budget took %v, want the backoff pauses (10+20+40ms)", elapsed)
	}
}

// hubStreamCount reports how many task streams the hub holds (its lock makes
// the read safe against a running turn).
func hubStreamCount(h *taskHub) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.streams)
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestHubDropsIdleStreams: the hub keeps a stream only while it has
// subscribers, and a commit for a task nobody watches creates none — the hub
// is not a second unbounded task store.
func TestHubDropsIdleStreams(t *testing.T) {
	store := NewMemoryTaskStore()
	release := make(chan struct{})
	srv := NewServer(ServerConfig{
		Card:   testCard(true),
		Store:  store,
		Handle: echoHandler,
		HandleStream: func(_ context.Context, _ *Message, _ StreamEmitter) (*Message, error) {
			<-release
			return NewMessage(RoleAgent, NewTextPart("done")), nil
		},
	})
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := testClient(t, server.URL, TransportJSONRPC, nil)
	stream, err := client.MessageStream(ctx, NewMessage(RoleUser, NewTextPart("go")))
	if err != nil {
		t.Fatalf("MessageStream: %v", err)
	}
	defer stream.Close()
	mustNext(t, ctx, stream) // working
	taskID := stream.TaskID()

	sub, err := client.Resubscribe(ctx, taskID)
	if err != nil {
		t.Fatalf("Resubscribe: %v", err)
	}
	mustNext(t, ctx, sub) // snapshot
	if got := hubStreamCount(srv.hub); got != 1 {
		t.Fatalf("hub streams with a subscriber = %d, want 1", got)
	}

	close(release)
	drainStream(t, ctx, sub)
	sub.Close()
	waitFor(t, 5*time.Second, "the idle stream to be dropped", func() bool {
		return hubStreamCount(srv.hub) == 0
	})

	if _, err := client.SendMessage(ctx, NewMessage(RoleUser, NewTextPart("plain"))); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if got := hubStreamCount(srv.hub); got != 0 {
		t.Fatalf("hub streams after a commit with no subscribers = %d, want 0", got)
	}
}

// TestStreamEmitterStatusRefusesSettled: Status reports progress only — the
// turn's outcome (terminal or interrupted) and an unknown state are refused,
// so a handler cannot contradict the state it returns.
func TestStreamEmitterStatusRefusesSettled(t *testing.T) {
	type call struct {
		state TaskState
		err   error
	}
	var mu sync.Mutex
	var calls []call
	recorded := make(chan struct{})
	srv := NewServer(ServerConfig{
		Card:  testCard(true),
		Store: NewMemoryTaskStore(),
		HandleStream: func(_ context.Context, _ *Message, emit StreamEmitter) (*Message, error) {
			states := []TaskState{
				TaskStateWorking, TaskStateInputRequired, TaskStateAuthRequired,
				TaskStateCompleted, TaskStateFailed, TaskStateCanceled,
				TaskStateRejected, TaskStateUnknown,
			}
			for _, state := range states {
				err := emit.Status(state, nil)
				mu.Lock()
				calls = append(calls, call{state: state, err: err})
				mu.Unlock()
			}
			close(recorded)
			return NewMessage(RoleAgent, NewTextPart("done")), nil
		},
	})
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	stream, err := testClient(t, server.URL, TransportJSONRPC, nil).
		MessageStream(ctx, NewMessage(RoleUser, NewTextPart("go")))
	if err != nil {
		t.Fatalf("MessageStream: %v", err)
	}
	defer stream.Close()
	<-recorded

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 8 {
		t.Fatalf("recorded %d calls, want 8", len(calls))
	}
	for _, c := range calls {
		switch c.state {
		case TaskStateWorking:
			if c.err != nil {
				t.Fatalf("Status(%q) = %v, want nil", c.state, c.err)
			}
		default:
			if !errors.Is(c.err, ErrWire) {
				t.Fatalf("Status(%q) = %v, want ErrWire", c.state, c.err)
			}
		}
	}
}

// TestClientStreamPollingPause: polling waits polling_interval between
// tasks/get calls instead of spinning the server.
func TestClientStreamPollingPause(t *testing.T) {
	var mu sync.Mutex
	var polls []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case bytes.Contains(body, []byte(MethodMessageStream)):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			writeJSONRPCEvent(w, w.(http.Flusher), &StatusUpdate{
				Kind: KindStatusUpdate, TaskID: "t1", ContextID: "c1",
				Status: TaskStatus{State: TaskStateWorking},
			})
		case bytes.Contains(body, []byte(MethodTasksGet)):
			mu.Lock()
			polls = append(polls, time.Now())
			n := len(polls)
			mu.Unlock()
			state := TaskStateWorking
			if n >= 3 {
				state = TaskStateCompleted
			}
			writeJSONRPCResult(w, &Task{ID: "t1", ContextID: "c1", Status: TaskStatus{State: state}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, body)
		}
	}))
	defer server.Close()

	ctx := context.Background()
	stream, err := testClient(t, server.URL, TransportJSONRPC, nil,
		WithPollingInterval(40*time.Millisecond)).
		MessageStream(ctx, NewMessage(RoleUser, NewTextPart("go")))
	if err != nil {
		t.Fatalf("MessageStream: %v", err)
	}
	defer stream.Close()
	drainStream(t, ctx, stream)

	mu.Lock()
	defer mu.Unlock()
	if len(polls) != 3 {
		t.Fatalf("tasks/get calls = %d, want 3", len(polls))
	}
	if gap := polls[1].Sub(polls[0]); gap < 30*time.Millisecond {
		t.Fatalf("first polling gap = %v, want the polling interval", gap)
	}
	if gap := polls[2].Sub(polls[1]); gap < 30*time.Millisecond {
		t.Fatalf("second polling gap = %v, want the polling interval", gap)
	}
}

// TestServerResubscribeClosesStreamAtTerminal is the A2 regression at the HTTP
// level: the SSE body of a resubscribe ends right after the terminal event —
// the reader reaches EOF without disconnecting itself.
func TestServerResubscribeClosesStreamAtTerminal(t *testing.T) {
	store := NewMemoryTaskStore()
	srv := NewServer(ServerConfig{Card: testCard(true), Store: store})
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	for _, tc := range []struct {
		name   string
		verb   string
		target string
		body   string
		frame  streamFrame
	}{
		{name: "jsonrpc", verb: http.MethodPost, target: "/",
			body:  rpcBody(MethodTasksResubscribe, `{"id":"t1"}`, `"9"`),
			frame: jsonrpcStreamFrame},
		{name: "rest", verb: http.MethodGet, target: RESTTasksPath + "/t1" + RESTSubscribeSuffix,
			frame: restStreamFrame},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := store.Save(context.Background(), &Task{
				ID: "t1", ContextID: "c1", Status: TaskStatus{State: TaskStateWorking},
			}); err != nil {
				t.Fatalf("seed task: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, tc.verb, server.URL+tc.target, strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
			if err != nil {
				t.Fatalf("open stream: %v", err)
			}
			defer resp.Body.Close()
			if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
				t.Fatalf("content-type = %q", ct)
			}
			reader := bufio.NewReader(resp.Body)

			opening, err := readSSEBlock(reader, tc.frame)
			if err != nil {
				t.Fatalf("read the opening snapshot: %v", err)
			}
			var snapshot Task
			if err := json.Unmarshal(opening, &snapshot); err != nil {
				t.Fatalf("decode the opening snapshot: %v (%s)", err, opening)
			}
			if snapshot.Status.State != TaskStateWorking {
				t.Fatalf("opening state = %q, want working", snapshot.Status.State)
			}

			// End the task while the stream is open.
			envelope := postRPC(t, srv.Handler(), rpcBody(MethodTasksCancel, `{"id":"t1"}`, `"10"`))
			if envelope.Error != nil {
				t.Fatalf("cancel: %v", envelope.Error)
			}

			// The body must end after the terminal event; a stream left open
			// blocks here until the request timeout.
			var after []json.RawMessage
			for {
				item, err := readSSEBlock(reader, tc.frame)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("the stream did not close after the terminal state: %v", err)
				}
				after = append(after, item)
			}
			if len(after) != 1 {
				t.Fatalf("items after the cancel = %d, want 1 (the terminal snapshot)", len(after))
			}
			var final Task
			if err := json.Unmarshal(after[0], &final); err != nil {
				t.Fatalf("decode the terminal item: %v (%s)", err, after[0])
			}
			if final.Status.State != TaskStateCanceled {
				t.Fatalf("terminal state = %q, want canceled", final.Status.State)
			}
		})
	}
}

// readSSEBlock reads one SSE event and returns its stream item, decoded for
// the binding's frame.
func readSSEBlock(reader *bufio.Reader, frame streamFrame) (json.RawMessage, error) {
	var data strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if data.Len() == 0 {
				continue
			}
			return frame([]byte(data.String()))
		}
		if rest, ok := strings.CutPrefix(line, "data: "); ok {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(rest)
		}
	}
}

// failingStore is a TaskStore whose write failAt fails once, to exercise the
// emitter's error path.
type failingStore struct {
	inner  *MemoryTaskStore
	mu     sync.Mutex
	writes int
	failAt int
}

func (f *failingStore) Save(ctx context.Context, task *Task) error {
	f.mu.Lock()
	f.writes++
	write := f.writes
	f.mu.Unlock()
	if write == f.failAt {
		return errors.New("store write failed")
	}
	return f.inner.Save(ctx, task)
}

func (f *failingStore) Get(ctx context.Context, id string) (*Task, error) {
	return f.inner.Get(ctx, id)
}

func (f *failingStore) Delete(ctx context.Context, id string) error {
	return f.inner.Delete(ctx, id)
}

// TestStreamEmitterReportsStoreErrors: a progress write the store refuses
// reaches the handler as an error, so it can stop instead of streaming state
// the server did not record.
func TestStreamEmitterReportsStoreErrors(t *testing.T) {
	store := &failingStore{inner: NewMemoryTaskStore(), failAt: 2}
	var mu sync.Mutex
	var statusErr error
	recorded := make(chan struct{})
	srv := NewServer(ServerConfig{
		Card:  testCard(true),
		Store: store,
		HandleStream: func(_ context.Context, _ *Message, emit StreamEmitter) (*Message, error) {
			defer close(recorded)
			err := emit.Status(TaskStateWorking, nil)
			mu.Lock()
			statusErr = err
			mu.Unlock()
			return NewMessage(RoleAgent, NewTextPart("done")), nil
		},
	})
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	stream, err := testClient(t, server.URL, TransportJSONRPC, nil).
		MessageStream(ctx, NewMessage(RoleUser, NewTextPart("go")))
	if err != nil {
		t.Fatalf("MessageStream: %v", err)
	}
	defer stream.Close()
	<-recorded

	mu.Lock()
	defer mu.Unlock()
	if statusErr == nil {
		t.Fatal("Status after a refused store write = nil, want the store error")
	}
	if errors.Is(statusErr, ErrStreamClosed) || errors.Is(statusErr, ErrWire) {
		t.Fatalf("Status error = %v, want the store failure", statusErr)
	}
}

// TestStreamNextReopensWithoutASource: a Next that finds no open stream
// reopens it through the reconnect path — with the backoff pause and an attempt
// spent — instead of opening a raw resubscribe.
func TestStreamNextReopensWithoutASource(t *testing.T) {
	var resubscribes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(MethodTasksResubscribe)) {
			t.Errorf("unexpected request: %s %s", r.Method, body)
			return
		}
		resubscribes.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		writeJSONRPCEvent(w, flusher, &Task{
			ID: "t1", ContextID: "c1", Status: TaskStatus{State: TaskStateWorking},
		})
		writeJSONRPCEvent(w, flusher, &StatusUpdate{
			Kind: KindStatusUpdate, TaskID: "t1", ContextID: "c1", Final: true,
			Status: TaskStatus{State: TaskStateCompleted},
		})
	}))
	defer server.Close()

	ctx := context.Background()
	const pause = 25 * time.Millisecond
	client := testClient(t, server.URL, TransportJSONRPC, nil,
		WithReconnectBackoff(pause), WithMaxReconnects(2))
	ep, err := client.endpoint(ctx)
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	stream := &TaskStream{client: client, ep: ep, taskID: "t1", view: &Task{ID: "t1"}}
	defer stream.Close()

	start := time.Now()
	event, err := stream.Next(ctx)
	if err != nil {
		t.Fatalf("Next without a source: %v", err)
	}
	if event.Task == nil || event.Task.ID != "t1" {
		t.Fatalf("first item = %+v, want the resubscribe snapshot", event)
	}
	if got := resubscribes.Load(); got != 1 {
		t.Fatalf("tasks/resubscribe calls = %d, want 1", got)
	}
	if elapsed := time.Since(start); elapsed < pause/2 {
		t.Fatalf("reopen took %v, want the backoff pause", elapsed)
	}
	events := drainStream(t, ctx, stream)
	if len(events) != 1 || events[0].Status == nil || !events[0].Status.Final {
		t.Fatalf("items after the snapshot = %v, want the terminal update", events)
	}
}

// TestReconnectDelay pins the backoff schedule: the configured pause before the
// first resubscribe, doubled once per earlier attempt (parity: Python's
// reconnect_backoff * 2 ** (attempt - 1)).
func TestReconnectDelay(t *testing.T) {
	client := NewClient("http://example.invalid", WithReconnectBackoff(10*time.Millisecond))
	for attempt, want := range map[int]time.Duration{
		1: 10 * time.Millisecond,
		2: 20 * time.Millisecond,
		3: 40 * time.Millisecond,
		4: 80 * time.Millisecond,
	} {
		if got := client.reconnectDelay(attempt); got != want {
			t.Fatalf("reconnectDelay(%d) = %v, want %v", attempt, got, want)
		}
	}
}

// TestApplyArtifactUpdate pins the folding rule both the server record and the
// client view follow.
func TestApplyArtifactUpdate(t *testing.T) {
	task := &Task{}
	ApplyArtifactUpdate(task, &ArtifactUpdate{
		Artifact: Artifact{ArtifactID: "a", Name: "first", Parts: []Part{NewTextPart("one")}},
		Append:   true,
	})
	ApplyArtifactUpdate(task, &ArtifactUpdate{
		Artifact:  Artifact{ArtifactID: "a", Parts: []Part{NewTextPart("two")}},
		Append:    true,
		LastChunk: true,
	})
	ApplyArtifactUpdate(task, &ArtifactUpdate{
		Artifact: Artifact{ArtifactID: "b", Parts: []Part{NewTextPart("b1")}},
	})
	ApplyArtifactUpdate(task, &ArtifactUpdate{
		Artifact: Artifact{ArtifactID: "b", Parts: []Part{NewTextPart("b2")}},
	})
	if got := artifactText(t, task.Artifacts, "a"); got != "onetwo" {
		t.Fatalf("append artifact = %q, want onetwo", got)
	}
	if task.Artifacts[0].Name != "first" {
		t.Fatalf("append kept name = %q, want first", task.Artifacts[0].Name)
	}
	if got := artifactText(t, task.Artifacts, "b"); got != "b2" {
		t.Fatalf("replace artifact = %q, want b2", got)
	}
	if len(task.Artifacts) != 2 {
		t.Fatalf("artifacts = %d, want one per id", len(task.Artifacts))
	}
	empty := &Task{}
	ApplyArtifactUpdate(empty, &ArtifactUpdate{Artifact: Artifact{Parts: []Part{NewTextPart("x")}}})
	if len(empty.Artifacts) != 0 {
		t.Fatalf("an update without an artifactId must be ignored, got %+v", empty.Artifacts)
	}
}

// TestStreamEmitterContract: the emitter refuses a state the handler's return
// value owns, refuses an artifact without an id, and an emitter outside a turn
// is inert.
func TestStreamEmitterContract(t *testing.T) {
	var zero StreamEmitter
	if err := zero.Artifact(Artifact{ArtifactID: "a"}, true, true); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("zero emitter Artifact = %v, want ErrStreamClosed", err)
	}
	if err := zero.Status(TaskStateWorking, nil); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("zero emitter Status = %v, want ErrStreamClosed", err)
	}

	srv := NewServer(ServerConfig{
		Card:  testCard(true),
		Store: NewMemoryTaskStore(),
		HandleStream: func(_ context.Context, _ *Message, emit StreamEmitter) (*Message, error) {
			if err := emit.Status(TaskStateCompleted, nil); !errors.Is(err, ErrWire) {
				return nil, fmt.Errorf("terminal Status = %v, want ErrWire", err)
			}
			if err := emit.Artifact(Artifact{Parts: []Part{NewTextPart("nameless")}}, true, true); !errors.Is(err, ErrWire) {
				return nil, fmt.Errorf("artifact without an id = %v, want ErrWire", err)
			}
			if err := emit.Artifact(Artifact{ArtifactID: "a", Parts: []Part{NewTextPart("ok")}}, true, true); err != nil {
				return nil, err
			}
			return NewMessage(RoleAgent, NewTextPart("done")), nil
		},
	})
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	ctx := context.Background()
	stream, err := testClient(t, server.URL, TransportJSONRPC, nil).
		MessageStream(ctx, NewMessage(RoleUser, NewTextPart("go")))
	if err != nil {
		t.Fatalf("MessageStream: %v", err)
	}
	defer stream.Close()
	drainStream(t, ctx, stream)
	if got := artifactText(t, stream.View().Artifacts, "a"); got != "ok" {
		t.Fatalf("view artifact = %q, want ok", got)
	}
}

// TestCancelClosesResubscribedStream: a cancel mid-turn is a terminal
// transition the resubscribed stream observes — the canceled snapshot, then the
// end. A later emit from the handler is refused.
func TestCancelClosesResubscribedStream(t *testing.T) {
	store := NewMemoryTaskStore()
	release := make(chan struct{})
	cancelErr := make(chan error, 1)
	srv := NewServer(ServerConfig{
		Card:  testCard(true),
		Store: store,
		HandleStream: func(_ context.Context, _ *Message, emit StreamEmitter) (*Message, error) {
			if err := emit.Artifact(Artifact{ArtifactID: "a1", Parts: []Part{NewTextPart("first")}}, true, false); err != nil {
				return nil, err
			}
			<-release
			cancelErr <- emit.Artifact(Artifact{ArtifactID: "a1", Parts: []Part{NewTextPart("late")}}, true, true)
			return NewMessage(RoleAgent, NewTextPart("done")), nil
		},
	})
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := testClient(t, server.URL, TransportJSONRPC, nil)
	stream, err := client.MessageStream(ctx, NewMessage(RoleUser, NewTextPart("go")))
	if err != nil {
		t.Fatalf("MessageStream: %v", err)
	}
	defer stream.Close()
	mustNext(t, ctx, stream) // working
	mustNext(t, ctx, stream) // first chunk
	taskID := stream.TaskID()

	sub, err := client.Resubscribe(ctx, taskID)
	if err != nil {
		t.Fatalf("Resubscribe: %v", err)
	}
	defer sub.Close()
	opening := mustNext(t, ctx, sub)
	if opening.Task == nil || opening.Task.Status.State != TaskStateWorking {
		t.Fatalf("resubscribe opening = %+v, want a working snapshot", opening)
	}
	if _, err := client.TasksCancel(ctx, taskID); err != nil {
		t.Fatalf("TasksCancel: %v", err)
	}
	close(release)
	if err := <-cancelErr; !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("emit after the cancel = %v, want ErrStreamClosed", err)
	}

	events := drainStream(t, ctx, sub)
	if len(events) != 1 || events[0].Task == nil {
		t.Fatalf("resubscribed stream after the cancel = %v, want the canceled snapshot then the end", events)
	}
	if events[0].Task.Status.State != TaskStateCanceled {
		t.Fatalf("canceled snapshot state = %q", events[0].Task.Status.State)
	}
	stored, err := store.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if stored.Status.State != TaskStateCanceled {
		t.Fatalf("stored state = %q, want canceled", stored.Status.State)
	}
}

// TestResubscribeFallsBackToPolling: a server that refuses tasks/resubscribe
// with -32004 (spec §3.1.6: the task cannot be subscribed to) is read back with
// tasks/get instead of failing the stream.
func TestResubscribeFallsBackToPolling(t *testing.T) {
	var resubscribes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case bytes.Contains(body, []byte(MethodMessageStream)):
			// Open the stream and publish the working state; the client's
			// transport breaks the connection after it, so the client must
			// resubscribe.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			writeJSONRPCEvent(w, w.(http.Flusher), &StatusUpdate{
				Kind: KindStatusUpdate, TaskID: "t1", ContextID: "c1",
				Status: TaskStatus{State: TaskStateWorking},
			})
		case bytes.Contains(body, []byte(MethodTasksResubscribe)):
			resubscribes.Add(1)
			writeRPC(w, &rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("1"), Error: &RPCError{
				Code: codeUnsupportedOperation, Message: "task cannot be subscribed to",
			}})
		case bytes.Contains(body, []byte(MethodTasksGet)):
			writeJSONRPCResult(w, &Task{ID: "t1", ContextID: "c1", Status: TaskStatus{State: TaskStateCompleted}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, body)
		}
	}))
	defer server.Close()

	ctx := context.Background()
	transport := &recordingTransport{base: http.DefaultTransport, plan: []int{1}}
	stream, err := testClient(t, server.URL, TransportJSONRPC, transport,
		WithPollingInterval(time.Millisecond), WithMaxReconnects(3)).
		MessageStream(ctx, NewMessage(RoleUser, NewTextPart("go")))
	if err != nil {
		t.Fatalf("MessageStream: %v", err)
	}
	defer stream.Close()
	events := drainStream(t, ctx, stream)
	if resubscribes.Load() != 1 {
		t.Fatalf("tasks/resubscribe calls = %d, want 1", resubscribes.Load())
	}
	if len(events) < 2 || events[len(events)-1].Task == nil {
		t.Fatalf("items = %v, want the working update then the polled terminal snapshot", events)
	}
	if stream.View().Status.State != TaskStateCompleted {
		t.Fatalf("view state = %q, want completed", stream.View().Status.State)
	}
}

// spySource is a streamSource that records whether it was closed.
type spySource struct{ closed bool }

func (s *spySource) next(context.Context) (json.RawMessage, error) { return nil, io.EOF }
func (s *spySource) close()                                        { s.closed = true }

// TestTaskStreamCloseReleasesSource: Close closes the open connection, a second
// Close is a no-op, and a stream without a source closes cleanly.
func TestTaskStreamCloseReleasesSource(t *testing.T) {
	spy := &spySource{}
	stream := &TaskStream{view: &Task{}, source: spy}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !spy.closed {
		t.Fatal("Close left the open stream source dangling")
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := (&TaskStream{view: &Task{}}).Close(); err != nil {
		t.Fatalf("Close without a source: %v", err)
	}
}

// TestClientStreamOpenErrors pins what a stream that never opened reports: the
// transport refusal as ErrTransport, a JSON answer instead of a stream as
// ErrWire, and a JSON-RPC error object through its sentinel.
func TestClientStreamOpenErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{
			name:   "unavailable",
			status: http.StatusServiceUnavailable,
			body:   "server restarting",
			want:   ErrTransport,
		},
		{
			name:   "json result instead of a stream",
			status: http.StatusOK,
			body:   `{"jsonrpc":"2.0","id":1,"result":{"id":"t1","contextId":"c1","status":{"state":"working"}}}`,
			want:   ErrWire,
		},
		{
			name:   "json-rpc error object",
			status: http.StatusOK,
			body:   `{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"task not found"}}`,
			want:   ErrTaskNotFound,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			ctx := context.Background()
			stream, err := NewClient(server.URL).MessageStream(ctx, NewMessage(RoleUser, NewTextPart("go")))
			if err == nil {
				stream.Close()
				t.Fatal("MessageStream accepted a response that is not a stream")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("MessageStream = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestClientStreamBareMessageEvent: a Message that names no kind is decoded
// from its shape (a role and parts), and a Message whose parts do not decode is
// a wire error rather than a half-filled event.
func TestClientStreamBareMessageEvent(t *testing.T) {
	t.Run("decoded", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			writeJSONRPCEvent(w, flusher, map[string]any{
				"role": "agent", "messageId": "m1", "message_id": "m1",
				"taskId": "t1", "contextId": "c1",
				"parts": []map[string]any{{"kind": "text", "text": "plain answer"}},
			})
		}))
		defer server.Close()

		ctx := context.Background()
		stream, err := NewClient(server.URL).MessageStream(ctx, NewMessage(RoleUser, NewTextPart("go")))
		if err != nil {
			t.Fatalf("MessageStream: %v", err)
		}
		defer stream.Close()
		event, err := stream.Next(ctx)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if event.Message == nil {
			t.Fatalf("event = %+v, want a Message", event)
		}
		if got := textOf(event.Message); got != "plain answer" {
			t.Fatalf("message text = %q", got)
		}
		if _, err := stream.Next(ctx); !errors.Is(err, io.EOF) {
			t.Fatalf("Next after the message = %v, want io.EOF", err)
		}
	})

	t.Run("undecodable parts", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			writeJSONRPCEvent(w, w.(http.Flusher), map[string]any{
				"role": "agent", "messageId": "m1",
				"taskId": "t1", "contextId": "c1",
				"parts": []map[string]any{{"kind": "nonsense", "text": "x"}},
			})
		}))
		defer server.Close()

		ctx := context.Background()
		stream, err := NewClient(server.URL).MessageStream(ctx, NewMessage(RoleUser, NewTextPart("go")))
		if err != nil {
			t.Fatalf("MessageStream: %v", err)
		}
		defer stream.Close()
		if _, err := stream.Next(ctx); !errors.Is(err, ErrWire) {
			t.Fatalf("Next = %v, want ErrWire", err)
		}
	})
}

// TestClientStreamRejectsEmptyEvent: a stream item that names neither a task, a
// status, an artifact nor a message is a wire error, not an empty event.
func TestClientStreamRejectsEmptyEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeJSONRPCEvent(w, w.(http.Flusher), map[string]any{})
	}))
	defer server.Close()

	ctx := context.Background()
	stream, err := NewClient(server.URL).MessageStream(ctx, NewMessage(RoleUser, NewTextPart("go")))
	if err != nil {
		t.Fatalf("MessageStream: %v", err)
	}
	defer stream.Close()
	if _, err := stream.Next(ctx); !errors.Is(err, ErrWire) {
		t.Fatalf("Next on an empty stream item = %v, want ErrWire", err)
	}
}
