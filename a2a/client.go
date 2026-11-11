package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Client is a transport-level A2A client: agent-card discovery plus the
// request/response methods (JSON-RPC, or REST when the card or WithTransport
// selects it) against one server, the push-notification
// config surface, and the client-tools / HITL drivers (clienttools.go). Safe
// for concurrent use.
//
// This is not the conversational A2AClient of Python (client.py:159 — an
// LLMClient that drives the turn machinery itself: it computes the AG2 history
// and syncs conversation state). The wire half ships here — WithHistory puts
// the conversation on a continuation, WithTenant scopes a request,
// WithCardVerifier checks the served card, and MessageStream/Resubscribe
// (streamclient.go) read a task stream with reconnect-and-backoff and a
// polling fallback — while the LLMClient adapter stays deferred (D-049).
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

	// Knobs (client_options.go). Zero values keep the plain behavior: post
	// JSON-RPC to base+"/", no card resolution, no hooks.
	preset       *AgentCard
	prefer       string
	historyLen   int
	tenant       string
	cardVerifier CardVerifier
	interceptors []CallInterceptor

	// Streaming knobs (streamclient.go); NewClient installs the Python
	// defaults, so the zero value of a Client built by hand still streams.
	streaming        bool
	maxReconnects    int
	reconnectBackoff time.Duration
	pollingInterval  time.Duration

	// Resolved endpoint (interface selection, clientconn.go); guarded by
	// connMu and set only after a successful Connect.
	connMu   sync.Mutex
	resolved *endpoint
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
		base:             strings.TrimSuffix(baseURL, "/"),
		cardPath:         DefaultCardPath,
		http:             &http.Client{Timeout: defaultHTTPTimeout},
		streaming:        true,
		maxReconnects:    defaultMaxReconnects,
		reconnectBackoff: defaultReconnectBackoff,
		pollingInterval:  defaultPollingInterval,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// WithCardVerifier makes the client verify the JWS signatures of every card
// it consumes — the fetched card, the preset card, and the authenticated
// extended card — and refuse a card that does not verify, with
// ErrCardSignature (parity: A2AConfig.card_signature_verifier, which the
// Python client applies to all three sources). Nil accepts any card, the
// unsigned default.
func WithCardVerifier(v CardVerifier) ClientOption {
	return func(c *Client) { c.cardVerifier = v }
}

// verifyCard runs the configured verifier on card, naming the source that
// failed (parity: client.py _verify_card wrapping the SDK error in
// A2ACardSignatureError with the source and URL).
func (c *Client) verifyCard(card *AgentCard, source string) error {
	if c.cardVerifier == nil {
		return nil
	}
	if err := c.cardVerifier(card); err != nil {
		return fmt.Errorf("%w (%s) for %q: %v", ErrCardSignature, source, c.base, err)
	}
	return nil
}

// AgentCard fetches and decodes the server's agent card; with WithPresetCard
// it returns a copy of the injected card without any request.
func (c *Client) AgentCard(ctx context.Context) (*AgentCard, error) {
	if c.preset != nil {
		card, err := cloneCard(c.preset)
		if err != nil {
			return nil, err
		}
		if err := c.verifyCard(card, "preset agent card"); err != nil {
			return nil, err
		}
		return card, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+c.cardPath, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrTransport, err)
	}
	if err := c.intercept(ctx, "agent-card", req); err != nil {
		return nil, err
	}
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
	if err := c.verifyCard(&card, "fetched agent card"); err != nil {
		return nil, err
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
	result, err := c.sendRaw(ctx, msg)
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
	result, err := c.getRaw(ctx, taskID)
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
	result, err := c.cancelRaw(ctx, taskID)
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
	ep, err := c.endpoint(ctx)
	if err != nil {
		return nil, err
	}
	if ep.binding == bindingREST {
		// AGO-DEVIATION: the push-notification config methods exist on
		// JSON-RPC only in this package (rest.go serves no push routes); a
		// REST-selected client refuses them with ErrBindingUnsupported rather
		// than silently speaking JSON-RPC to a REST-only server. Python's SDK
		// client carries push over whatever binding it negotiated.
		return nil, fmt.Errorf("%w: %s needs the JSON-RPC binding; this client resolved REST",
			ErrBindingUnsupported, method)
	}
	id := c.nextID.Add(1)
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("%w: encode params: %v", ErrTransport, err)
	}
	paramsJSON = tenantParams(paramsJSON, ep.tenant)
	payload, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(fmt.Sprintf("%d", id)),
		Method:  method,
		Params:  paramsJSON,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: encode request: %v", ErrTransport, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.rpcURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrTransport, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.intercept(ctx, method, req); err != nil {
		return nil, err
	}
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

// intercept stamps the configured headers, then runs the interceptors in
// registration order; the first failure aborts the request.
func (c *Client) intercept(ctx context.Context, method string, req *http.Request) error {
	c.applyHeaders(req)
	for _, fn := range c.interceptors {
		if err := fn(ctx, method, req); err != nil {
			return fmt.Errorf("%w: interceptor for %s: %w", ErrTransport, method, err)
		}
	}
	return nil
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
	case codeUnsupportedOperation:
		// Both sentinels match: callers that only know ErrRPC keep working,
		// the streaming client detects the spec §3.1.6 case and polls.
		return fmt.Errorf("%w: %w: %s", ErrRPC, ErrUnsupportedOperation, e.Message)
	default:
		return fmt.Errorf("%w: %s", ErrRPC, e.Error())
	}
}

// ExtendedCard fetches the authenticated extended agent card: the JSON-RPC
// agent/getAuthenticatedExtendedCard method, or GET /extendedAgentCard on the
// REST binding. ErrRPC wraps the -32004 a server without an extended card
// answers; the card is served only to a caller that passed the server's
// request gates (WithHeaders carries whatever credentials they need).
func (c *Client) ExtendedCard(ctx context.Context) (*AgentCard, error) {
	result, err := c.extendedCardRaw(ctx)
	if err != nil {
		return nil, err
	}
	var card AgentCard
	if err := json.Unmarshal(result, &card); err != nil {
		return nil, fmt.Errorf("%w: decode extended card: %v", ErrWire, err)
	}
	if err := c.verifyCard(&card, "extended agent card"); err != nil {
		return nil, err
	}
	return &card, nil
}

// extendedCardRaw performs the extended-card fetch on the resolved binding.
func (c *Client) extendedCardRaw(ctx context.Context) (json.RawMessage, error) {
	ep, err := c.endpoint(ctx)
	if err != nil {
		return nil, err
	}
	if ep.binding != bindingREST {
		return c.call(ctx, MethodExtendedCard, struct{}{})
	}
	return c.restDo(ctx, MethodExtendedCard, http.MethodGet, ep.restURL+ExtendedCardPath, nil)
}
