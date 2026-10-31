package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Client is a transport-level A2A client: agent-card discovery plus the
// request/response JSON-RPC methods against one server, the push-notification
// config surface, and the client-tools / HITL drivers (clienttools.go). Safe
// for concurrent use.
//
// This is not the conversational A2AClient of Python (client.py:159 — an
// LLMClient that ships full AG2 history per turn and syncs conversation
// state); that turn machinery is deferred (D-049).
//
// AGO-DEVIATION: Python's A2AClient is an LLMClient provider (not a tool), so
// no tool adapter ships from this package; D-049.
type Client struct {
	base       string
	cardPath   string
	http       *http.Client
	headers    map[string]string
	extensions []string
	nextID     atomic.Int64
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithHTTPClient replaces the underlying *http.Client entirely — transport,
// proxies and Timeout; the default client caps every exchange at
// defaultHTTPTimeout, a custom one is the caller's policy alone.
func WithHTTPClient(hc *http.Client) ClientOption {
	return func(c *Client) { c.http = hc }
}

// WithHeaders adds headers sent with every request (auth, tracing).
func WithHeaders(h map[string]string) ClientOption {
	return func(c *Client) {
		if c.headers == nil {
			c.headers = make(map[string]string, len(h))
		}
		for k, v := range h {
			c.headers[k] = v
		}
	}
}

// WithCardPath overrides the discovery path (default DefaultCardPath; point
// it at LegacyCardPath for v0.x-only servers).
func WithCardPath(p string) ClientOption {
	return func(c *Client) { c.cardPath = p }
}

// WithExtensions activates protocol extension URIs on the connection: every
// request carries the A2A-Extensions header with them (comma-separated, the
// A2A spec spelling; parity: extension.py extension_call_context — the
// header on jsonrpc/rest), and the drivers keep the URIs on continuation
// messages. Card reconciliation is ValidateExtensionActivation's job — call
// it with the fetched card before the first exchange.
func WithExtensions(uris ...string) ClientOption {
	return func(c *Client) { c.extensions = append(c.extensions, uris...) }
}

// defaultHTTPTimeout bounds every exchange of the built-in client so a hung
// server cannot pin the calling goroutine forever.
const defaultHTTPTimeout = 120 * time.Second

// NewClient builds a Client against the server at baseURL (for example
// "http://localhost:8000"). The JSON-RPC endpoint is baseURL + "/".
func NewClient(baseURL string, opts ...ClientOption) *Client {
	c := &Client{
		base:     strings.TrimSuffix(baseURL, "/"),
		cardPath: DefaultCardPath,
		http:     &http.Client{Timeout: defaultHTTPTimeout},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// AgentCard fetches and decodes the server's agent card.
func (c *Client) AgentCard(ctx context.Context) (*AgentCard, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+c.cardPath, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrTransport, err)
	}
	c.applyHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: fetch agent card: %v", ErrTransport, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: agent card responded %d", ErrTransport, resp.StatusCode)
	}
	var card AgentCard
	// The card is bytes from a remote, unauthenticated endpoint: cap the
	// read like every other body this package touches, or a hostile server
	// streams an endless "card" into the decoder.
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRequestBody)).Decode(&card); err != nil {
		return nil, fmt.Errorf("%w: decode agent card: %v", ErrWire, err)
	}
	return &card, nil
}

// SendMessage exchanges one message: JSON-RPC message/send, the reply Message
// as the result. The server records the exchange as a Task in its store;
// correlate via the reply's TaskID and Client.TasksGet. A remote JSON-RPC
// error (Handle failure → internal error, unknown task → ErrTaskNotFound)
// comes back through the sentinels of this package. A server that parked the
// turn input-required answers with a Task record, which this method refuses —
// use SendMessageTask or one of the drivers for extension/HITL exchanges.
func (c *Client) SendMessage(ctx context.Context, msg *Message) (*Message, error) {
	result, err := c.call(ctx, MethodMessageSend, map[string]any{"message": msg})
	if err != nil {
		return nil, err
	}
	var probe struct {
		Role   string      `json:"role"`
		Status *TaskStatus `json:"status"`
	}
	if err := json.Unmarshal(result, &probe); err != nil {
		return nil, fmt.Errorf("%w: decode reply: %v", ErrWire, err)
	}
	if probe.Status != nil {
		return nil, fmt.Errorf("%w: message/send returned a Task in state %q; want a Message",
			ErrWire, probe.Status.State)
	}
	var reply Message
	if err := json.Unmarshal(result, &reply); err != nil {
		return nil, fmt.Errorf("%w: decode reply: %v", ErrWire, err)
	}
	return &reply, nil
}

// TasksGet fetches one task by id; ErrTaskNotFound when the server does not
// know it.
func (c *Client) TasksGet(ctx context.Context, taskID string) (*Task, error) {
	result, err := c.call(ctx, MethodTasksGet, map[string]any{"id": taskID})
	if err != nil {
		return nil, err
	}
	var task Task
	if err := json.Unmarshal(result, &task); err != nil {
		return nil, fmt.Errorf("%w: decode task: %v", ErrWire, err)
	}
	return &task, nil
}

// TasksCancel cancels a submitted/working task and returns its record; a
// terminal task yields ErrTaskNotCancelable.
func (c *Client) TasksCancel(ctx context.Context, taskID string) (*Task, error) {
	result, err := c.call(ctx, MethodTasksCancel, map[string]any{"id": taskID})
	if err != nil {
		return nil, err
	}
	var task Task
	if err := json.Unmarshal(result, &task); err != nil {
		return nil, fmt.Errorf("%w: decode task: %v", ErrWire, err)
	}
	return &task, nil
}

// call performs one JSON-RPC exchange and returns the raw result on success.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("%w: encode params: %v", ErrTransport, err)
	}
	payload, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(fmt.Sprintf("%d", id)),
		Method:  method,
		Params:  paramsJSON,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: encode request: %v", ErrTransport, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+rpcPath, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrTransport, err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.applyHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrTransport, method, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRequestBody))
	if err != nil {
		return nil, fmt.Errorf("%w: read response: %v", ErrTransport, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%w: %s responded %d: %s", ErrTransport, method, resp.StatusCode,
			strings.TrimSpace(string(body)))
	}
	var envelope rpcResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("%w: decode JSON-RPC envelope: %v", ErrWire, err)
	}
	if envelope.Error != nil {
		return nil, rpcErrToGo(envelope.Error)
	}
	return envelope.Result, nil
}

// applyHeaders stamps the configured headers onto an outgoing request.
func (c *Client) applyHeaders(req *http.Request) {
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	if len(c.extensions) > 0 {
		req.Header.Set(HeaderA2AExtensions, strings.Join(c.extensions, ","))
	}
}

// ValidateExtensions fetches the agent card and reconciles the client's
// activated URIs against it in both directions (parity: client.py:328 —
// validate_extension_activation at connect). ErrExtensionNotSupported on a
// mismatch.
func (c *Client) ValidateExtensions(ctx context.Context) error {
	card, err := c.AgentCard(ctx)
	if err != nil {
		return err
	}
	return ValidateExtensionActivation(card, c.extensions)
}

// rpcErrToGo maps a wire error object to the package sentinels; the code
// stays visible in the text.
func rpcErrToGo(e *RPCError) error {
	switch e.Code {
	case codeTaskNotFound:
		return fmt.Errorf("%w: %s", ErrTaskNotFound, e.Message)
	case codeTaskNotCancelable:
		return fmt.Errorf("%w: %s", ErrTaskNotCancelable, e.Message)
	default:
		return fmt.Errorf("%w: %s", ErrRPC, e.Error())
	}
}
