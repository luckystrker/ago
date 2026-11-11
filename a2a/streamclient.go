package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/luckystrker/ago/internal/sse"
)

// Client-side task streaming (parity: ag2/a2a/client.py _consume_streaming /
// _consume_polling / _poll_until_settled). MessageStream opens message/stream
// (POST /message:stream on the REST binding), reads the Server-Sent Events and
// returns a TaskStream the caller drains with Next. A broken connection is
// re-established with tasks/resubscribe (GET /tasks/{id}:subscribe), which
// opens with the task as the server currently holds it and then continues
// live, so the client's view converges to the server's state (wave-4
// invariant 7). A stream that ends before the task is terminal is read back by
// polling tasks/get, exactly like the Python client.

// Streaming knobs (parity: A2AClient streaming / max_reconnects /
// reconnect_backoff / polling_interval). The zero-value Client streams with
// three reconnects, a 500ms first backoff (doubling per attempt) and a 500ms
// polling interval, the Python defaults.
const (
	defaultMaxReconnects    = 3
	defaultReconnectBackoff = 500 * time.Millisecond
	defaultPollingInterval  = 500 * time.Millisecond
)

// WithStreaming chooses how MessageStream consumes a turn: true (the default)
// reads message/stream and reconnects through tasks/resubscribe; false sends
// the message synchronously and polls tasks/get to the terminal state
// (parity: A2AClient streaming=).
func WithStreaming(on bool) ClientOption {
	return func(c *Client) { c.streaming = on }
}

// WithMaxReconnects caps how many times one task stream is re-established
// after a broken connection before Next reports ErrReconnect (parity:
// A2AClient max_reconnects). 0 disables reconnecting.
func WithMaxReconnects(n int) ClientOption {
	return func(c *Client) { c.maxReconnects = n }
}

// WithReconnectBackoff sets the wait before the first resubscribe; every later
// attempt doubles it (parity: A2AClient reconnect_backoff).
func WithReconnectBackoff(d time.Duration) ClientOption {
	return func(c *Client) { c.reconnectBackoff = d }
}

// WithPollingInterval sets the wait between tasks/get polls, both when a
// stream ends before the task is terminal and when streaming is off (parity:
// A2AClient polling_interval).
func WithPollingInterval(d time.Duration) ClientOption {
	return func(c *Client) { c.pollingInterval = d }
}

// TaskEvent is one item a TaskStream delivered: exactly one of the four fields
// is set. Task is a full snapshot (message/stream may not open with one, a
// resubscribe always does), Status an update of the task's state, Artifact one
// chunk of its output, Message an outright reply that ends the turn.
type TaskEvent struct {
	Task     *Task
	Status   *StatusUpdate
	Artifact *ArtifactUpdate
	Message  *Message
}

// taskID reports the task the event belongs to ("" when it names none).
func (e *TaskEvent) taskID() string {
	switch {
	case e == nil:
		return ""
	case e.Task != nil:
		return e.Task.ID
	case e.Status != nil:
		return e.Status.TaskID
	case e.Artifact != nil:
		return e.Artifact.TaskID
	case e.Message != nil:
		return e.Message.TaskID
	}
	return ""
}

// settled reports whether nothing more arrives for the turn after the event:
// the task is terminal or interrupted (parity: a2a/_states.py SETTLED_STATES),
// or a Message answered the request outright.
func (e *TaskEvent) settled() bool {
	switch {
	case e == nil:
		return false
	case e.Task != nil:
		return settledTaskState(e.Task.Status.State)
	case e.Status != nil:
		return settledTaskState(e.Status.Status.State)
	case e.Message != nil:
		// A Message answers the request outright: no task updates follow it.
		return true
	}
	return false
}

// TaskStream is a live view of one remote task. Next yields the stream's items
// in order and returns io.EOF once a settled state was delivered — a terminal
// state, an interruption (input-required/auth-required) that hands the turn
// back to the caller, or an outright Message — or the stream was closed; View
// returns the task as it currently stands, folded from the events delivered so
// far. A broken connection is reconnected transparently, so a caller sees one
// logical stream even across several HTTP exchanges.
//
// A TaskStream is not safe for concurrent use: one goroutine reads Next. The
// context passed to MessageStream (or Resubscribe) must stay valid for the
// stream's lifetime; Next's context governs the exchanges it opens itself.
type TaskStream struct {
	client *Client
	ep     *endpoint
	taskID string
	view   *Task
	queue  []*TaskEvent
	// done is set once the stream delivered a settled state (terminal or
	// interrupted) or a Message: Next then reports io.EOF and never reads
	// again, so an event after it is never surfaced.
	done     bool
	attempts int
	source   streamSource
}

// streamFrame decodes one SSE data line into a raw stream item.
type streamFrame func(data []byte) (json.RawMessage, error)

// jsonrpcStreamFrame unwraps the JSON-RPC response envelope of the JSON-RPC
// binding's stream.
func jsonrpcStreamFrame(data []byte) (json.RawMessage, error) {
	var envelope rpcResponse
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("%w: decode JSON-RPC stream event: %v", ErrWire, err)
	}
	if envelope.Error != nil {
		return nil, rpcErrToGo(envelope.Error)
	}
	return envelope.Result, nil
}

// restStreamFrame passes the HTTP+JSON binding's bare item through.
func restStreamFrame(data []byte) (json.RawMessage, error) {
	return json.RawMessage(data), nil
}

// streamSource is one open connection of a task stream. next returns one raw
// item, or io.EOF when the connection ended cleanly.
type streamSource interface {
	next(ctx context.Context) (json.RawMessage, error)
	close()
}

// sseSource reads one SSE response body.
type sseSource struct {
	body   io.ReadCloser
	sc     *sse.Scanner
	decode streamFrame
}

func (s *sseSource) next(ctx context.Context) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for s.sc.Next() {
		_, data := s.sc.Event()
		return s.decode(data)
	}
	if err := s.sc.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

func (s *sseSource) close() { _ = s.body.Close() }

// pollSource reads the task back with tasks/get — the fallback for a stream
// that ended before the terminal state (spec §3.1.6) and for a client with
// streaming switched off.
type pollSource struct {
	client *Client
	taskID string
	first  bool
}

func (p *pollSource) next(ctx context.Context) (json.RawMessage, error) {
	if !p.first {
		if err := sleep(ctx, p.client.pollingInterval); err != nil {
			return nil, err
		}
	}
	p.first = false
	return p.client.getRaw(ctx, p.taskID)
}

func (p *pollSource) close() {}

// reconnectDelay is the pause before attempt n (1-based): the configured
// backoff doubled once per earlier attempt.
func (c *Client) reconnectDelay(attempt int) time.Duration {
	delay := c.reconnectBackoff
	for i := 1; i < attempt; i++ {
		delay *= 2
	}
	return delay
}

// errNoStream marks a Next that found no open stream to read from.
var errNoStream = errors.New("a2a: no task stream is open")

// sleep waits for d or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// MessageStream sends message and returns the task stream the turn produces.
// With streaming on (the default) the turn is read from message/stream; with
// streaming off the message is sent synchronously and the task — if it did not
// finish right away — is polled to its terminal state.
func (c *Client) MessageStream(ctx context.Context, msg *Message) (*TaskStream, error) {
	ep, err := c.endpoint(ctx)
	if err != nil {
		return nil, err
	}
	ts := &TaskStream{client: c, ep: ep, view: &Task{}}
	if c.streaming {
		if err := ts.openMessageStream(ctx, msg); err != nil {
			return nil, err
		}
		return ts, nil
	}
	result, err := c.sendRaw(ctx, msg)
	if err != nil {
		return nil, err
	}
	event, err := decodeStreamItem(result)
	if err != nil {
		return nil, err
	}
	ts.queue = append(ts.queue, event)
	if event.Task != nil {
		ts.taskID = event.Task.ID
		ts.view.ID, ts.view.ContextID = event.Task.ID, event.Task.ContextID
		ts.source = &pollSource{client: c, taskID: event.Task.ID, first: true}
	}
	return ts, nil
}

// Resubscribe opens the live stream of an existing task without sending a
// message: the stream starts with the task as the server holds it now — which
// is what closes the gap after a dropped connection — and continues with every
// later event until the terminal state. A task that is already terminal
// answers its final state and ends; an unknown task yields ErrTaskNotFound.
func (c *Client) Resubscribe(ctx context.Context, taskID string) (*TaskStream, error) {
	if taskID == "" {
		return nil, fmt.Errorf("%w: empty task id", ErrWire)
	}
	ep, err := c.endpoint(ctx)
	if err != nil {
		return nil, err
	}
	ts := &TaskStream{client: c, ep: ep, taskID: taskID, view: &Task{ID: taskID}}
	if err := ts.openResubscribe(ctx); err != nil {
		return nil, err
	}
	return ts, nil
}

// Next returns the next item of the stream. io.EOF means the turn is over: a
// settled state (terminal or interrupted) was delivered, the stream was closed,
// or it ended before it named a task. ErrReconnect means the connection broke
// and the task could not be resubscribed (unknown task, or the reconnect
// budget ran out).
func (ts *TaskStream) Next(ctx context.Context) (*TaskEvent, error) {
	for {
		if len(ts.queue) > 0 {
			event := ts.queue[0]
			ts.queue = ts.queue[1:]
			if err := ts.absorb(event); err != nil {
				return nil, err
			}
			return event, nil
		}
		if ts.done {
			ts.closeSource()
			return nil, io.EOF
		}
		if ts.source == nil {
			// No stream is open (a stream that ended without a source and
			// without a poll to continue): reopening is a reconnect like any
			// other, so it pauses and spends the attempt budget.
			if err := ts.reconnect(ctx, errNoStream); err != nil {
				return nil, err
			}
			continue
		}
		raw, err := ts.source.next(ctx)
		if err == nil {
			if ts.done {
				// Nothing may follow the settled state: drop it.
				ts.closeSource()
				return nil, io.EOF
			}
			event, decodeErr := decodeStreamItem(raw)
			if decodeErr != nil {
				ts.closeSource()
				return nil, decodeErr
			}
			if absorbErr := ts.absorb(event); absorbErr != nil {
				ts.closeSource()
				return nil, absorbErr
			}
			return event, nil
		}
		ts.closeSource()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(err, io.EOF) {
			if ts.taskID == "" {
				// The stream ended before it named a task: there is nothing
				// to read back (parity: the Python client returns when the
				// stream closed without a task id).
				return nil, io.EOF
			}
			// The stream ended before the task settled: read the state back.
			ts.source = &pollSource{client: ts.client, taskID: ts.taskID, first: true}
			continue
		}
		if reconnectErr := ts.reconnect(ctx, err); reconnectErr != nil {
			return nil, reconnectErr
		}
	}
}

// reconnect re-establishes a broken stream by resubscribing, with the Python
// client's limits and exponential backoff. A resubscribe that fails to open
// (a server restarting answers 503, a refused connection) is an attempt like
// any other: the stream pauses, tries again, and reports ErrReconnect once the
// budget is spent — parity with the Python loop, where the SDK error of a
// failed subscribe is caught by the same retry (client.py:595-599). A task the
// server refuses to subscribe to (spec §3.1.6: it finished while the stream was
// down) falls back to polling instead of retrying.
func (ts *TaskStream) reconnect(ctx context.Context, cause error) error {
	if ts.taskID == "" {
		return fmt.Errorf("%w after %d attempt(s): %v", ErrReconnect, ts.attempts, cause)
	}
	failure := cause
	for ts.attempts < ts.client.maxReconnects {
		ts.attempts++
		if err := sleep(ctx, ts.client.reconnectDelay(ts.attempts)); err != nil {
			return err
		}
		err := ts.openResubscribe(ctx)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrUnsupportedOperation) || errors.Is(err, ErrTaskNotFound) {
			// AGO-DEVIATION: Python's client falls back to polling only on
			// UnsupportedOperationError; a resubscribe the server answers
			// "task not found" (a store eviction, a restarted server) is
			// equally unstreamable here, and polling surfaces the same
			// ErrTaskNotFound instead of wrapping it in ErrReconnect.
			ts.source = &pollSource{client: ts.client, taskID: ts.taskID, first: true}
			return nil
		}
		failure = err
	}
	return fmt.Errorf("%w after %d attempt(s): %v", ErrReconnect, ts.attempts, failure)
}

// View returns the task as the delivered events left it: status and artifacts,
// folded with the same rules the server writes its record with. The result is
// a copy, safe to keep after the stream is closed.
func (ts *TaskStream) View() *Task {
	clone, err := cloneTask(ts.view)
	if err != nil {
		return nil
	}
	return clone
}

// TaskID returns the task the stream follows ("" until the first event names
// it).
func (ts *TaskStream) TaskID() string { return ts.taskID }

// Close ends the stream: the current response body is closed and later Next
// calls report io.EOF.
func (ts *TaskStream) Close() error {
	ts.closeSource()
	ts.done = true
	return nil
}

// closeSource closes and drops the current source, if any.
func (ts *TaskStream) closeSource() {
	if ts.source != nil {
		ts.source.close()
		ts.source = nil
	}
}

// absorb folds one event into the stream's view. An event naming another task
// is a wire error: a stream carries exactly one task's events.
func (ts *TaskStream) absorb(event *TaskEvent) error {
	if id := event.taskID(); id != "" {
		switch ts.taskID {
		case "":
			ts.taskID = id
			ts.view.ID = id
		case id:
		default:
			return fmt.Errorf("%w: stream for task %s carried an event of task %s", ErrWire, ts.taskID, id)
		}
	}
	switch {
	case event.Task != nil:
		snapshot, err := cloneTask(event.Task)
		if err != nil {
			return err
		}
		ts.view.ID, ts.view.ContextID = snapshot.ID, snapshot.ContextID
		ts.view.Status, ts.view.Artifacts = snapshot.Status, snapshot.Artifacts
	case event.Status != nil:
		ts.view.ContextID = event.Status.ContextID
		ts.view.Status = event.Status.Status
	case event.Artifact != nil:
		ts.view.ContextID = event.Artifact.ContextID
		ApplyArtifactUpdate(ts.view, event.Artifact)
	}
	if event.settled() {
		ts.done = true
	}
	return nil
}

// openMessageStream issues message/stream (JSON-RPC or the REST twin).
func (ts *TaskStream) openMessageStream(ctx context.Context, msg *Message) error {
	ep := ts.ep
	if ep.binding == bindingREST {
		body, err := json.Marshal(msg)
		if err != nil {
			return fmt.Errorf("%w: encode message: %v", ErrTransport, err)
		}
		source, err := ts.client.openSSE(ctx, sseRequest{
			method: MethodMessageStream, verb: http.MethodPost,
			target: restPath(ep, RESTMessageStreamPath), body: body,
			contentType: MediaA2AJSON, frame: restStreamFrame,
		})
		if err != nil {
			return err
		}
		ts.source = source
		return nil
	}
	body, err := ts.client.rpcBody(MethodMessageStream, map[string]any{"message": msg}, ep.tenant)
	if err != nil {
		return err
	}
	source, err := ts.client.openSSE(ctx, sseRequest{
		method: MethodMessageStream, verb: http.MethodPost, target: ep.rpcURL,
		body: body, contentType: "application/json", frame: jsonrpcStreamFrame,
	})
	if err != nil {
		return err
	}
	ts.source = source
	return nil
}

// openResubscribe issues tasks/resubscribe (GET /tasks/{id}:subscribe on the
// REST binding).
func (ts *TaskStream) openResubscribe(ctx context.Context) error {
	ep := ts.ep
	if ep.binding == bindingREST {
		source, err := ts.client.openSSE(ctx, sseRequest{
			method: MethodTasksResubscribe, verb: http.MethodGet,
			target: restPath(ep, RESTTasksPath+"/"+url.PathEscape(ts.taskID)+RESTSubscribeSuffix),
			frame:  restStreamFrame,
		})
		if err != nil {
			return err
		}
		ts.source = source
		return nil
	}
	body, err := ts.client.rpcBody(MethodTasksResubscribe, map[string]any{"id": ts.taskID}, ep.tenant)
	if err != nil {
		return err
	}
	source, err := ts.client.openSSE(ctx, sseRequest{
		method: MethodTasksResubscribe, verb: http.MethodPost, target: ep.rpcURL,
		body: body, contentType: "application/json", frame: jsonrpcStreamFrame,
	})
	if err != nil {
		return err
	}
	ts.source = source
	return nil
}

// sseRequest is one streaming exchange to open.
type sseRequest struct {
	method      string
	verb        string
	target      string
	body        []byte
	contentType string
	frame       streamFrame
}

// openSSE performs one streaming request and returns the open body. A non-2xx
// status or a non-SSE answer (a JSON-RPC error object, a REST error body) is
// mapped to the package sentinels, so a refused stream reads like the
// synchronous methods' errors.
func (c *Client) openSSE(ctx context.Context, req sseRequest) (streamSource, error) {
	var rd io.Reader
	if req.body != nil {
		rd = bytes.NewReader(req.body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.verb, req.target, rd)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrTransport, err)
	}
	httpReq.Header.Set("Accept", "text/event-stream")
	if req.contentType != "" {
		httpReq.Header.Set("Content-Type", req.contentType)
	}
	if err := c.intercept(ctx, req.method, httpReq); err != nil {
		return nil, err
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrTransport, req.method, err)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxRequestBody))
		if err := errorFromBody(data); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s responded %d: %s", ErrTransport, req.method, resp.StatusCode,
			strings.TrimSpace(string(data)))
	}
	if mediaType := resp.Header.Get("Content-Type"); !strings.HasPrefix(mediaType, "text/event-stream") {
		// The server answered the request instead of opening a stream; that
		// is how a refused stream (an unknown task, a server without
		// streaming) travels on both bindings.
		defer resp.Body.Close()
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxRequestBody))
		if err != nil {
			return nil, fmt.Errorf("%w: read response: %v", ErrTransport, err)
		}
		if err := errorFromBody(data); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s answered %q instead of a stream", ErrWire, req.method,
			strings.TrimSpace(string(data)))
	}
	return &sseSource{body: resp.Body, sc: sse.NewScanner(resp.Body), decode: req.frame}, nil
}

// errorFromBody decodes an error answer: the JSON-RPC error object or the REST
// {"error": {...}} body (one struct matches both).
func errorFromBody(data []byte) error {
	var envelope struct {
		Error *RPCError `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err == nil && envelope.Error != nil {
		return rpcErrToGo(envelope.Error)
	}
	return nil
}

// rpcBody builds one JSON-RPC request payload with a fresh id and the tenant
// stamped into the params.
func (c *Client) rpcBody(method string, params any, tenant string) ([]byte, error) {
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("%w: encode params: %v", ErrTransport, err)
	}
	paramsJSON = tenantParams(paramsJSON, tenant)
	payload, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(fmt.Sprintf("%d", c.nextID.Add(1))),
		Method:  method,
		Params:  paramsJSON,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: encode request: %v", ErrTransport, err)
	}
	return payload, nil
}

// decodeStreamItem decodes one StreamResponse item. The discriminator is the
// A2A "kind" when the server states one; otherwise the shape decides — a task
// update names its task in "taskId", a Task snapshot carries "id"/"status", a
// Message carries a role.
func decodeStreamItem(raw json.RawMessage) (*TaskEvent, error) {
	var probe struct {
		Kind     string          `json:"kind"`
		TaskID   string          `json:"taskId"`
		ID       string          `json:"id"`
		Status   json.RawMessage `json:"status"`
		Artifact json.RawMessage `json:"artifact"`
		Role     string          `json:"role"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("%w: decode stream event: %v", ErrWire, err)
	}
	switch {
	case probe.Kind == KindStatusUpdate:
		return statusEvent(raw)
	case probe.Kind == KindArtifactUpdate:
		return artifactEvent(raw)
	case probe.Kind == "message":
		return messageEvent(raw)
	}
	switch {
	case probe.TaskID != "" && probe.Status != nil:
		return statusEvent(raw)
	case len(probe.Artifact) > 0:
		return artifactEvent(raw)
	case probe.Role != "" && probe.ID == "":
		return messageEvent(raw)
	}
	var task Task
	if err := json.Unmarshal(raw, &task); err != nil {
		return nil, fmt.Errorf("%w: decode task snapshot: %v", ErrWire, err)
	}
	if task.ID == "" {
		return nil, fmt.Errorf("%w: stream event names no task: %s", ErrWire, raw)
	}
	return &TaskEvent{Task: &task}, nil
}

func statusEvent(raw json.RawMessage) (*TaskEvent, error) {
	var update StatusUpdate
	if err := json.Unmarshal(raw, &update); err != nil {
		return nil, fmt.Errorf("%w: decode status update: %v", ErrWire, err)
	}
	return &TaskEvent{Status: &update}, nil
}

func artifactEvent(raw json.RawMessage) (*TaskEvent, error) {
	var update ArtifactUpdate
	if err := json.Unmarshal(raw, &update); err != nil {
		return nil, fmt.Errorf("%w: decode artifact update: %v", ErrWire, err)
	}
	return &TaskEvent{Artifact: &update}, nil
}

func messageEvent(raw json.RawMessage) (*TaskEvent, error) {
	var msg Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, fmt.Errorf("%w: decode message: %v", ErrWire, err)
	}
	return &TaskEvent{Message: &msg}, nil
}
