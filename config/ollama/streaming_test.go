package ollama

// Streaming is NDJSON: one JSON object per line. These tests pin the chunk
// folding order, the final ModelResponse (tool calls and usage from the done
// chunk) and the failure paths of the hand-written line scanner.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
)

// ndjsonServer answers with the given NDJSON lines, flushing every one so the
// client reads them as they are written, and records the request.
func ndjsonServer(t *testing.T, status int, lines ...string) (*httptest.Server, *capturedRequest) {
	t.Helper()
	cap := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request: %v", err)
		}
		cap.Path = r.URL.Path
		cap.Headers = r.Header.Clone()
		cap.Body = b
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(status)
		flusher, _ := w.(http.Flusher)
		for _, line := range lines {
			fmt.Fprintln(w, line)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

func TestOllamaStreamingFoldsChunksInOrder(t *testing.T) {
	srv, cap := ndjsonServer(t, 200,
		`{"model":"llama3","message":{"role":"assistant","thinking":"weighing"},"done":false}`,
		`{"model":"llama3","message":{"role":"assistant","content":"Hel"},"done":false}`,
		`{"model":"llama3","message":{"role":"assistant","content":"lo"},"done":false}`,
		`{"model":"llama3","message":{"role":"assistant","tool_calls":[{"function":{"name":"lookup","arguments":{"q":"x"}}}]},"done":false}`,
		`{"model":"llama3","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":11,"eval_count":5}`,
	)
	cl := startClient(t, NewOllama("llama3", WithHost(srv.URL), WithStreaming(true)))
	conv, seen := newCaptureConv()

	resp, err := cl.Call(context.Background(), nil, conv, config.CallOptions{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}

	body := cap.JSON(t)
	if body["stream"] != true {
		t.Fatalf("streaming request must ask for stream: true, got %v", body["stream"])
	}
	if cap.Path != "/api/chat" {
		t.Fatalf("path: %s", cap.Path)
	}

	// Progress events in the order the chunks arrived: reasoning, then one
	// chunk per text delta. The tool call emits no progress event.
	var progress []events.Event
	for _, ev := range *seen {
		switch ev.(type) {
		case *events.ModelReasoning, *events.ModelMessageChunk, *events.ModelMessage:
			progress = append(progress, ev)
		}
	}
	if len(progress) != 4 {
		t.Fatalf("want reasoning + 2 chunks + final message, got %d (%v)", len(progress), progress)
	}
	if r, ok := progress[0].(*events.ModelReasoning); !ok || r.Content != "weighing" {
		t.Fatalf("first event must be the reasoning chunk: %T %v", progress[0], progress[0])
	}
	if c, ok := progress[1].(*events.ModelMessageChunk); !ok || c.Content != "Hel" {
		t.Fatalf("second event: %T %v", progress[1], progress[1])
	}
	if c, ok := progress[2].(*events.ModelMessageChunk); !ok || c.Content != "lo" {
		t.Fatalf("third event: %T %v", progress[2], progress[2])
	}
	if m, ok := progress[3].(*events.ModelMessage); !ok || m.Content != "Hello" {
		t.Fatalf("final event must be the whole message: %T %v", progress[3], progress[3])
	}

	// The final response joins the text, the calls and the done-chunk usage.
	if resp.Message == nil || resp.Message.Content != "Hello" {
		t.Fatalf("response message: %+v", resp.Message)
	}
	if len(resp.ToolCalls.Calls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(resp.ToolCalls.Calls))
	}
	call := resp.ToolCalls.Calls[0]
	if call.Name != "lookup" || call.Arguments != `{"q":"x"}` {
		t.Fatalf("tool call: %+v", call)
	}
	if call.ID == "" {
		t.Fatal("the synthesized call needs an id")
	}
	if resp.Usage.PromptTokens == nil || *resp.Usage.PromptTokens != 11 ||
		resp.Usage.CompletionTokens == nil || *resp.Usage.CompletionTokens != 5 ||
		resp.Usage.TotalTokens == nil || *resp.Usage.TotalTokens != 16 {
		t.Fatalf("usage: %+v", resp.Usage)
	}
	if resp.FinishReason != "stop" || resp.Model != "llama3" || resp.Provider != "ollama" {
		t.Fatalf("identity: model=%q finish=%q provider=%q", resp.Model, resp.FinishReason, resp.Provider)
	}
}

// TestOllamaStreamingWithoutDoneChunk: a cut or empty stream leaves the usage
// unreported and the identity blank (Python parity: Usage() stays empty).
func TestOllamaStreamingWithoutDoneChunk(t *testing.T) {
	srv, _ := ndjsonServer(t, 200,
		`{"model":"llama3","message":{"content":"partial"},"done":false}`,
	)
	cl := startClient(t, NewOllama("llama3", WithHost(srv.URL), WithStreaming(true)))
	resp, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if resp.Message == nil || resp.Message.Content != "partial" {
		t.Fatalf("message: %+v", resp.Message)
	}
	if resp.Usage.PromptTokens != nil || resp.Usage.CompletionTokens != nil || resp.Usage.TotalTokens != nil {
		t.Fatalf("usage must stay unset without a done chunk: %+v", resp.Usage)
	}
	if resp.FinishReason != "" || resp.Model != "" {
		t.Fatalf("finish/model come from the done chunk only: %+v", resp)
	}
	if len(resp.ToolCalls.Calls) != 0 {
		t.Fatalf("no calls expected: %+v", resp.ToolCalls.Calls)
	}
}

func TestOllamaStreamingEmptyAnswerLeavesMessageNil(t *testing.T) {
	srv, _ := ndjsonServer(t, 200, `{"model":"llama3","done":true,"done_reason":"stop"}`)
	cl := startClient(t, NewOllama("llama3", WithHost(srv.URL), WithStreaming(true)))
	resp, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if resp.Message != nil {
		t.Fatalf("an empty stream must not fabricate a message: %+v", resp.Message)
	}
	if resp.Usage.TotalTokens == nil || *resp.Usage.TotalTokens != 0 {
		t.Fatalf("the done chunk reports zero usage: %+v", resp.Usage)
	}
}

// TestOllamaStreamingToolCallsAcrossChunks: every chunk contributes its calls
// and a later chunk keeps the earlier one (Python parity: append per chunk).
func TestOllamaStreamingToolCallsAcrossChunks(t *testing.T) {
	srv, _ := ndjsonServer(t, 200,
		`{"message":{"tool_calls":[{"function":{"name":"a","arguments":{"n":1}}}]},"done":false}`,
		`{"message":{"tool_calls":[{"function":{"name":"b","arguments":null}}]},"done":false}`,
		`{"message":{},"done":true,"done_reason":"stop"}`,
	)
	cl := startClient(t, NewOllama("m", WithHost(srv.URL), WithStreaming(true)))
	resp, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(resp.ToolCalls.Calls) != 2 {
		t.Fatalf("want 2 calls, got %+v", resp.ToolCalls.Calls)
	}
	if resp.ToolCalls.Calls[0].Name != "a" || resp.ToolCalls.Calls[0].Arguments != `{"n":1}` {
		t.Fatalf("first call: %+v", resp.ToolCalls.Calls[0])
	}
	// A null arguments object becomes the empty object the event contract expects.
	if resp.ToolCalls.Calls[1].Name != "b" || resp.ToolCalls.Calls[1].Arguments != "{}" {
		t.Fatalf("second call: %+v", resp.ToolCalls.Calls[1])
	}
}

func TestOllamaStreamingHTTPError(t *testing.T) {
	srv, _ := ndjsonServer(t, 404, `{"error":"model 'm' not found"}`)
	cl := startClient(t, NewOllama("m", WithHost(srv.URL), WithStreaming(true)))
	_, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
	var apiErr *config.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *config.APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 404 || apiErr.Provider != config.ProviderOllama {
		t.Fatalf("%+v", apiErr)
	}
	if !errors.Is(err, config.ErrNotFound) {
		t.Fatal("errors.Is(ErrNotFound) failed")
	}
}

// TestOllamaStreamingChunkError: an error object inside a 200 stream is a
// failure, not an empty answer (the ollama SDK raises on it too).
func TestOllamaStreamingChunkError(t *testing.T) {
	srv, _ := ndjsonServer(t, 200, `{"error":"out of memory"}`)
	cl := startClient(t, NewOllama("m", WithHost(srv.URL), WithStreaming(true)))
	_, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
	if err == nil || !strings.Contains(err.Error(), "out of memory") {
		t.Fatalf("want the chunk error, got %v", err)
	}
}

// TestOllamaStreamingUsesInjectedClient: the caller's HTTP client carries the
// streaming request too (proxy, mTLS, test transports).
func TestOllamaStreamingUsesInjectedClient(t *testing.T) {
	srv, _ := ndjsonServer(t, 200, `{"message":{"content":"hi"},"done":true,"done_reason":"stop"}`)
	rt := &countingTransport{}
	cl := startClient(t, NewOllama("m", WithHost(srv.URL), WithStreaming(true), WithHTTPClient(&http.Client{Transport: rt})))
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if rt.n.Load() != 1 {
		t.Fatalf("injected client saw %d requests, want 1", rt.n.Load())
	}
}

// TestOllamaStreamingStatus300Rejected: 300 sits inside the rejected band
// (status < 200 || status >= 300): no retry-forever on a redirect answer.
func TestOllamaStreamingStatus300Rejected(t *testing.T) {
	srv, _ := ndjsonServer(t, 300, `{"error":"nope"}`)
	cl := startClient(t, NewOllama("m", WithHost(srv.URL), WithStreaming(true)))
	_, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
	var apiErr *config.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *config.APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 300 {
		t.Fatalf("status: %d", apiErr.StatusCode)
	}
}

// failingTransport fails every request with a plain error, so the cancellation
// branch is the only thing that can report context.Canceled.
type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("transport exploded")
}

// TestOllamaStreamingCanceledBeforeRequest: a canceled context wins over the
// transport error (stream.go:56) — the caller must see context.Canceled, not
// the wrapped transport failure.
func TestOllamaStreamingCanceledBeforeRequest(t *testing.T) {
	cl := startClient(t, NewOllama("m", WithHost("http://unused"), WithStreaming(true),
		WithHTTPClient(&http.Client{Transport: failingTransport{}})))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := cl.Call(ctx, nil, nil, config.CallOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if strings.Contains(err.Error(), "transport exploded") {
		t.Fatalf("the transport error must not wrap the cancellation: %v", err)
	}
}

// cancelBody serves one chunk, then blocks until the call's context ends and
// fails the read the way a torn-down connection does (a transport error that
// does not wrap context.Canceled).
type cancelBody struct {
	ctx    context.Context
	sent   bool
	closed atomic.Bool
}

func (b *cancelBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, `{"message":{"content":"a"},"done":false}`+"\n"), nil
	}
	<-b.ctx.Done()
	return 0, errors.New("use of closed network connection")
}

func (b *cancelBody) Close() error {
	b.closed.Store(true)
	return nil
}

// fixedBodyTransport answers every request with a prepared body.
type fixedBodyTransport struct{ body io.ReadCloser }

func (t *fixedBodyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Status: "200 OK", Header: http.Header{}, Body: t.body}, nil
}

// TestOllamaStreamingCanceledMidStream: canceling during the stream returns
// context.Canceled and closes the response body.
func TestOllamaStreamingCanceledMidStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	body := &cancelBody{ctx: ctx}
	cl := startClient(t, NewOllama("m", WithHost("http://unused"), WithStreaming(true),
		WithHTTPClient(&http.Client{Transport: &fixedBodyTransport{body: body}})))

	ms := stream.NewMemoryStream()
	chunk := make(chan struct{}, 1)
	ms.Subscribe(func(ev events.Event) error {
		if _, ok := ev.(*events.ModelMessageChunk); ok {
			select {
			case chunk <- struct{}{}:
			default:
			}
		}
		return nil
	}, stream.SubscribeOptions{})
	conv := agocontext.New(ms)

	done := make(chan error, 1)
	go func() {
		_, err := cl.Call(ctx, nil, conv, config.CallOptions{})
		done <- err
	}()
	select {
	case <-chunk:
	case <-time.After(5 * time.Second):
		t.Fatal("the first chunk never arrived")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
		if strings.Contains(err.Error(), "closed network connection") {
			t.Fatalf("the transport teardown must not surface: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the call did not return after cancellation")
	}
	if !body.closed.Load() {
		t.Fatal("the response body must be closed")
	}
}

// longLineBody yields an endless line of bytes with no newline.
type longLineBody struct {
	remaining int
}

func (b *longLineBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > b.remaining {
		n = b.remaining
	}
	for i := 0; i < n; i++ {
		p[i] = 'x'
	}
	b.remaining -= n
	return n, nil
}

func (b *longLineBody) Close() error { return nil }

// TestOllamaStreamingLineTooLong: a line over the cap fails with the scanner's
// ErrTooLong instead of growing memory without bound.
func TestOllamaStreamingLineTooLong(t *testing.T) {
	cl := startClient(t, NewOllama("m", WithHost("http://unused"), WithStreaming(true),
		WithHTTPClient(&http.Client{Transport: &fixedBodyTransport{body: &longLineBody{remaining: streamLineLimit + 1}}})))
	_, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("want bufio.ErrTooLong, got %v", err)
	}
}

// TestOllamaStreamingBlankLinesAndBadJSON: blank keep-alive lines are skipped,
// malformed JSON fails the call.
func TestOllamaStreamingBlankLinesAndBadJSON(t *testing.T) {
	srv, _ := ndjsonServer(t, 200, ``, `{"message":{"content":"ok"},"done":true,"done_reason":"stop"}`, ``)
	cl := startClient(t, NewOllama("m", WithHost(srv.URL), WithStreaming(true)))
	resp, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
	if err != nil {
		t.Fatalf("blank lines must be skipped: %v", err)
	}
	if resp.Message == nil || resp.Message.Content != "ok" {
		t.Fatalf("message: %+v", resp.Message)
	}

	bad, _ := ndjsonServer(t, 200, `{not json`)
	cl = startClient(t, NewOllama("m", WithHost(bad.URL), WithStreaming(true)))
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err == nil {
		t.Fatal("malformed chunk must fail the call")
	}
}
