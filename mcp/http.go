package mcp

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/luckystrker/ago/internal/httpx"
	"github.com/luckystrker/ago/internal/sse"
)

// httpTransport speaks JSON-RPC 2.0 over streamable HTTP: every message is a POST
// to the server URL; a request's answer comes back as application/json or as an
// SSE stream whose data entries are JSON-RPC messages. The session id the server
// assigns at initialize travels back on every request (Mcp-Session-Id).
type httpTransport struct {
	url     string
	headers map[string]string
	// client carries the short exchanges (notifications, replies to the
	// server, session teardown) under the configured Timeout.
	client *http.Client
	// stream carries a request whose answer may be an SSE stream. It has no
	// http.Client.Timeout: that cap would also tick while the client answers
	// an elicitation for a human, so call() runs its own deadline that pauses
	// during those answers. It shares client's transport (proxy, TLS).
	stream *http.Client
	// custom is the transport built for Proxy/InsecureSkipVerify (nil: the
	// process default), kept so close can drop its idle connections.
	custom  *http.Transport
	timeout time.Duration

	sessionID string
	// protocolVersion, when set, travels as MCP-Protocol-Version on every
	// request (the modern era announces its revision per request).
	protocolVersion string
	handler         requestHandler
	notifier        func(Notification)

	// listenCancel/listenDone belong to the standalone GET stream (startListener).
	listenCancel context.CancelFunc
	listenDone   chan struct{}
	// maxEvent is the resolved per-event cap for SSE answers; computed once at
	// construction so a session rebuild (client.go reuses this transport) keeps
	// the configured event-size policy.
	maxEvent int
}

// parseAbsoluteURL validates that raw is an absolute http(s) URL.
func parseAbsoluteURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("%w: MCPServerConfig.URL %q is not an absolute http(s) URL", ErrNotProvided, raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: MCPServerConfig.URL %q must use http or https", ErrNotProvided, raw)
	}
	return u, nil
}

// defaultExchangeTimeout caps one HTTP exchange when the config sets no
// Timeout (Python parity: MCPServerConfig.connection_timeout = 30.0 — a hung
// server must fail the call, not block the client's lock and the agent turn
// forever).
const defaultExchangeTimeout = 30 * time.Second

// maxJSONResponseBytes caps one buffered JSON-RPC answer: a hostile endpoint
// must not be able to exhaust memory through an endless body. SSE answers are
// consumed line-capped by the scanner instead.
const maxJSONResponseBytes int64 = 8 << 20

// defaultSSEEventSize caps one SSE event when the config sets no
// MaxSSEEventSize (Python parity: max_sse_event_size = 1_048_576 — a tool
// result travels as a single event, and a hostile server must not be able to
// force every client to buffer unbounded memory).
const defaultSSEEventSize = 1 << 20

// newHTTPTransport validates the config and builds the header set: custom headers
// first, then the derived Bearer token when no Authorization entry exists
// (Python parity: _resolve_config).
func newHTTPTransport(cfg *MCPServerConfig) (*httpTransport, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("%w: MCPServerConfig.URL is empty", ErrNotProvided)
	}
	if _, err := parseAbsoluteURL(cfg.URL); err != nil {
		return nil, err
	}
	headers := make(map[string]string, len(cfg.Headers)+1)
	for k, v := range cfg.Headers {
		headers[k] = v
	}
	if cfg.AuthorizationToken != "" && !hasAuthorizationHeader(headers) {
		headers["Authorization"] = "Bearer " + cfg.AuthorizationToken
	}
	timeout := defaultExchangeTimeout
	switch {
	case cfg.Timeout > 0:
		timeout = cfg.Timeout
	case cfg.Timeout < 0:
		timeout = 0 // http.Client: no cap
	}
	// The SSE event-size policy mirrors Timeout's sentinel form: zero is the
	// default cap, a positive value is the threshold, a negative value removes
	// it (Python parity: max_sse_event_size int|None = 1_048_576; None = no cap).
	maxEvent := defaultSSEEventSize
	switch {
	case cfg.MaxSSEEventSize > 0:
		maxEvent = cfg.MaxSSEEventSize
	case cfg.MaxSSEEventSize < 0:
		maxEvent = math.MaxInt
	}
	custom, err := buildHTTPTransport(cfg)
	if err != nil {
		return nil, err
	}
	var rt http.RoundTripper
	if custom != nil {
		rt = custom
	}
	// AGO-DEVIATION: the Timeout caps the whole exchange (including reading an
	// SSE response body), while Python's httpx timeout is per-phase
	// (connect/read/write); a server answering slowly but steadily is cut off
	// at the cap here. The clock stops while the client answers a request the
	// server made mid-call (a human may take minutes) and restarts afterwards.
	//
	// Custom headers (Authorization, x-api-key, ...) must not follow a
	// redirect to another host — the stdlib strips only a small fixed set —
	// so the redirect policy strips everything non-safelisted at the host
	// boundary; same-host hops keep the headers.
	check := httpx.RedirectStripCredentials.CheckRedirect()
	return &httpTransport{
		url: cfg.URL, headers: headers, maxEvent: maxEvent, custom: custom, timeout: timeout,
		client: &http.Client{Timeout: timeout, Transport: rt, CheckRedirect: check},
		stream: &http.Client{Transport: rt, CheckRedirect: check},
	}, nil
}

// buildHTTPTransport returns a dedicated transport when the config asks for a
// proxy or for TLS verification to be skipped, and nil when the process default
// is right (keeping its environment-proxy behaviour).
func buildHTTPTransport(cfg *MCPServerConfig) (*http.Transport, error) {
	if cfg.Proxy == "" && !cfg.InsecureSkipVerify {
		return nil, nil
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}
	tr := base.Clone()
	if cfg.Proxy != "" {
		u, err := url.Parse(cfg.Proxy)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("%w: MCPServerConfig.Proxy %q is not an absolute proxy URL", ErrUnsupportedConfig, cfg.Proxy)
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return nil, fmt.Errorf("%w: MCPServerConfig.Proxy %q must use http, https or socks5", ErrUnsupportedConfig, cfg.Proxy)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	if cfg.InsecureSkipVerify {
		// Explicit opt-in documented on MCPServerConfig.InsecureSkipVerify.
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402
	}
	return tr, nil
}

func hasAuthorizationHeader(headers map[string]string) bool {
	for k := range headers {
		if strings.EqualFold(k, "Authorization") {
			return true
		}
	}
	return false
}

// post sends one wire message through the given client and returns the response.
func (t *httpTransport) post(ctx context.Context, client *http.Client, body any) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%w: encode request: %v", ErrProtocol, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrProtocol, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	if t.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", t.sessionID)
	}
	if t.protocolVersion != "" {
		t.stampModernHeaders(req, body)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp: server %s unreachable: %w", t.url, err)
	}
	return resp, nil
}

// exchangeDeadline caps one call without ticking while the client answers a
// server's request (a human may take minutes): pause stops the clock, resume
// restarts it in full.
type exchangeDeadline struct {
	d     time.Duration
	timer *time.Timer
	fired atomic.Bool
	mu    sync.Mutex
}

func newExchangeDeadline(d time.Duration, cancel context.CancelFunc) *exchangeDeadline {
	dl := &exchangeDeadline{d: d}
	if d > 0 {
		dl.timer = time.AfterFunc(d, func() {
			dl.fired.Store(true)
			cancel()
		})
	}
	return dl
}

func (dl *exchangeDeadline) pause() {
	if dl.timer != nil {
		dl.mu.Lock()
		dl.timer.Stop()
		dl.mu.Unlock()
	}
}

func (dl *exchangeDeadline) resume() {
	if dl.timer != nil {
		dl.mu.Lock()
		dl.timer.Reset(dl.d)
		dl.mu.Unlock()
	}
}

func (dl *exchangeDeadline) stop() {
	if dl.timer != nil {
		dl.timer.Stop()
	}
}

// stampModernHeaders adds the headers a modern-era request carries beside its
// _meta envelope: MCP-Protocol-Version, Mcp-Method and, for the methods that
// name a resource, Mcp-Name. The server checks each against the body and refuses
// the request on a mismatch (HEADER_MISMATCH); a reply to a server request
// (no method) carries only the version.
func (t *httpTransport) stampModernHeaders(req *http.Request, body any) {
	req.Header.Set("MCP-Protocol-Version", t.protocolVersion)
	rpc, ok := body.(rpcRequest)
	if !ok {
		return
	}
	req.Header.Set("Mcp-Method", rpc.Method)
	params, _ := rpc.Params.(map[string]any)
	if key, named := nameBearingMethods[rpc.Method]; named {
		if name, ok := params[key].(string); ok {
			req.Header.Set("Mcp-Name", encodeHeaderValue(name))
		}
	}
}

// nameBearingMethods maps a method to the params key mirrored as Mcp-Name
// (mcp.shared.inbound.NAME_BEARING_METHODS).
var nameBearingMethods = map[string]string{
	"tools/call":     "name",
	"prompts/get":    "name",
	"resources/read": "uri",
}

// encodeHeaderValue passes printable ASCII without edge whitespace through and
// base64-wraps anything else as =?base64?...?= so the receiver recovers the exact
// bytes (mcp.shared.inbound.encode_header_value).
func encodeHeaderValue(v string) string {
	safe := strings.TrimSpace(v) == v
	for i := 0; safe && i < len(v); i++ {
		if v[i] < 0x20 || v[i] > 0x7e {
			safe = false
		}
	}
	if safe && !(strings.HasPrefix(v, "=?base64?") && strings.HasSuffix(v, "?=")) {
		return v
	}
	return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(v)) + "?="
}

func (t *httpTransport) call(ctx context.Context, id int64, method string, params any) (json.RawMessage, error) {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	dl := newExchangeDeadline(t.timeout, cancel)
	defer dl.stop()
	res, err := t.callWithin(cctx, dl, id, method, params)
	if err != nil && dl.fired.Load() {
		// The exchange missed its deadline and the request was cut: the
		// session is broken, as for a hung stdio subprocess.
		return nil, fmt.Errorf("%w: no reply to %s within %s: %w", ErrTimeout, method, t.timeout, context.DeadlineExceeded)
	}
	return res, err
}

func (t *httpTransport) callWithin(ctx context.Context, dl *exchangeDeadline, id int64, method string, params any) (json.RawMessage, error) {
	resp, err := t.post(ctx, t.stream, rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound && t.sessionID != "" {
		// Streamable-HTTP session loss: the server discarded our session id.
		return nil, errSessionExpired
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		// A modern-era server rejects a malformed envelope with a 4xx that
		// still carries a JSON-RPC error body: keep the error object, so the
		// caller can read its code (e.g. an unsupported protocol version).
		var msg wireMessage
		if json.Unmarshal(body, &msg) == nil && msg.Error != nil {
			return nil, fmt.Errorf("%w: server responded %d: %w", ErrProtocol, resp.StatusCode, msg.Error)
		}
		return nil, fmt.Errorf("%w: server responded %d: %s", ErrProtocol, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if method == "initialize" {
		// The spec's streamable-HTTP session assignment: the id the server
		// returned travels on every subsequent request. A stateless server
		// sends none.
		if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
			t.sessionID = sid
		}
	}

	contentType := resp.Header.Get("Content-Type")
	switch {
	case strings.Contains(contentType, "text/event-stream"):
		return t.callOverSSE(ctx, dl, resp.Body, id)
	default:
		// application/json (the declared branch) or a server that omits the header.
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONResponseBytes+1))
		if err != nil {
			return nil, fmt.Errorf("%w: read response: %v", ErrProtocol, err)
		}
		if int64(len(body)) > maxJSONResponseBytes {
			return nil, fmt.Errorf("%w: JSON response exceeds %d bytes", ErrProtocol, maxJSONResponseBytes)
		}
		return decodeResponse(body, id)
	}
}

// streamCursor is what a client needs to resume an SSE stream after a
// disconnect: the id of the last event it saw (sent back as Last-Event-ID) and
// the server's reconnect hint.
type streamCursor struct {
	lastID  string
	retryMs int // -1: the server gave no hint
}

func (c *streamCursor) note(ev sseEvent) {
	if ev.ID != "" {
		c.lastID = ev.ID
	}
	if ev.Retry >= 0 {
		c.retryMs = ev.Retry
	}
}

// Reconnection policy for a request stream that ended without its reply
// (mcp.client.streamable_http: MAX_RECONNECTION_ATTEMPTS, DEFAULT_RECONNECTION_DELAY_MS).
const (
	maxReconnectAttempts  = 2
	defaultReconnectDelay = time.Second
)

// callOverSSE scans the response stream for the reply with the awaited id,
// delivering the notifications the server interleaves and answering the requests
// it sends on this stream (elicitation, sampling, roots, ping): each answer is a
// POST of the JSON-RPC reply to the same endpoint and session. A stream that
// ends before the reply, after the server handed out event ids, is resumed with
// a GET carrying Last-Event-ID (the server replays what was missed).
func (t *httpTransport) callOverSSE(ctx context.Context, dl *exchangeDeadline, r io.Reader, id int64) (json.RawMessage, error) {
	cur := streamCursor{retryMs: -1}
	body := r
	for attempt := 0; ; attempt++ {
		res, done, err := t.readStream(ctx, dl, body, id, &cur)
		if done || err != nil {
			return res, err
		}
		// The stream ended without the reply.
		if cur.lastID == "" || attempt >= maxReconnectAttempts {
			return nil, fmt.Errorf("%w: SSE stream ended without the reply to request %d", ErrProtocol, id)
		}
		delay := defaultReconnectDelay
		if cur.retryMs >= 0 {
			delay = time.Duration(cur.retryMs) * time.Millisecond
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		resp, err := t.resume(ctx, cur.lastID)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		body = resp.Body
	}
}

// readStream consumes one SSE body. done is true when the awaited reply arrived.
func (t *httpTransport) readStream(ctx context.Context, dl *exchangeDeadline, r io.Reader, id int64, cur *streamCursor) (json.RawMessage, bool, error) {
	er := newEventReader(r, t.maxEvent)
	for er.next() {
		ev := er.event()
		cur.note(ev)
		if !ev.HasData {
			continue // a priming event: it carries only the cursor
		}
		var msg wireMessage
		if err := json.Unmarshal(ev.Data, &msg); err != nil {
			return nil, false, fmt.Errorf("%w: malformed SSE message: %v", ErrProtocol, err)
		}
		switch {
		case msg.isRequest():
			dl.pause()
			err := t.reply(ctx, msg)
			dl.resume()
			if err != nil {
				return nil, false, err
			}
		case msg.Method != "":
			t.deliver(msg)
		case msg.replyTo(id):
			res, err := msg.outcome()
			return res, true, err
		}
	}
	if err := er.failure(); err != nil {
		// A broken connection after the server handed out event ids is a
		// disconnect to resume from, like a clean end; an oversized event is not
		// a transport fault and stays fatal.
		if cur.lastID != "" && !errors.Is(err, sse.ErrEventTooLarge) && ctx.Err() == nil {
			return nil, false, nil
		}
		// Double %w: consumers (and the tests) must be able to detect an
		// oversize event through errors.Is(err, sse.ErrEventTooLarge).
		return nil, false, fmt.Errorf("%w: SSE stream: %w", ErrProtocol, err)
	}
	return nil, false, nil
}

// deliver hands one server notification to the Client's notification handler.
func (t *httpTransport) deliver(msg wireMessage) {
	if fn := t.notifier; fn != nil {
		fn(Notification{Method: msg.Method, Params: msg.Params})
	}
}

// openStream issues a GET for the session's SSE stream, optionally resuming after
// lastID. The caller closes the body.
func (t *httpTransport) openStream(ctx context.Context, sessionID, lastID string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.url, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrProtocol, err)
	}
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	if t.protocolVersion != "" {
		req.Header.Set("MCP-Protocol-Version", t.protocolVersion)
	}
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := t.stream.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp: server %s unreachable: %w", t.url, err)
	}
	return resp, nil
}

// resume reopens a request's stream after a disconnect.
func (t *httpTransport) resume(ctx context.Context, lastID string) (*http.Response, error) {
	resp, err := t.openStream(ctx, t.sessionID, lastID)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		resp.Body.Close()
		return nil, fmt.Errorf("%w: resuming the stream after %q was refused with status %d", ErrProtocol, lastID, resp.StatusCode)
	}
	return resp, nil
}

// startListener opens the standalone GET stream the server uses for messages
// outside any request, once the session exists (the SDK opens it right after
// notifications/initialized). A server that offers none (405, or any non-2xx)
// ends the listener quietly: the stream is optional. A stream that ends is
// reopened with Last-Event-ID, up to maxReconnectAttempts consecutive failures.
func (t *httpTransport) startListener() {
	if t.sessionID == "" || t.listenDone != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.listenCancel, t.listenDone = cancel, done
	sessionID := t.sessionID
	go func() {
		defer close(done)
		t.listen(ctx, sessionID)
	}()
}

func (t *httpTransport) listen(ctx context.Context, sessionID string) {
	cur := streamCursor{retryMs: -1}
	failures := 0
	for failures < maxReconnectAttempts {
		resp, err := t.openStream(ctx, sessionID, cur.lastID)
		if err == nil && (resp.StatusCode < 200 || resp.StatusCode > 299) {
			resp.Body.Close()
			// 405 and friends: this server has no listener stream. The SDK retries
			// such a refusal once more after the reconnect delay; AGo stops at the
			// first, since a status answer is not a transient fault.
			return
		}
		if err != nil {
			failures++
		} else {
			failures = 0
			er := newEventReader(resp.Body, t.maxEvent)
			for er.next() {
				ev := er.event()
				cur.note(ev)
				if !ev.HasData {
					continue
				}
				var msg wireMessage
				if json.Unmarshal(ev.Data, &msg) != nil {
					continue
				}
				switch {
				case msg.isRequest():
					// A request outside any call: answered like one inside.
					_ = t.reply(ctx, msg)
				case msg.Method != "":
					t.deliver(msg)
				}
			}
			resp.Body.Close()
		}
		delay := defaultReconnectDelay
		if cur.retryMs >= 0 {
			delay = time.Duration(cur.retryMs) * time.Millisecond
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
	}
}

func (t *httpTransport) stopListener() {
	if t.listenCancel == nil {
		return
	}
	t.listenCancel()
	<-t.listenDone
	t.listenCancel, t.listenDone = nil, nil
}

func (t *httpTransport) setNotifier(fn func(Notification)) { t.notifier = fn }

// reply answers one server-initiated request and delivers the answer.
func (t *httpTransport) reply(ctx context.Context, msg wireMessage) error {
	resp, err := t.post(ctx, t.client, answerRequest(ctx, t.handler, msg))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%w: server rejected the reply to its %s request with status %d", ErrProtocol, msg.Method, resp.StatusCode)
	}
	return nil
}

func (t *httpTransport) setHandler(h requestHandler) { t.handler = h }

func (t *httpTransport) setProtocolVersion(v string) { t.protocolVersion = v }

func (t *httpTransport) notify(ctx context.Context, method string, params any) error {
	resp, err := t.post(ctx, t.client, rpcNotification{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%w: notification %s rejected with status %d", ErrProtocol, method, resp.StatusCode)
	}
	return nil
}

// closeBudget caps the session-teardown DELETE: a hung server must not hold
// the Client (which closes a timed-out transport under its lock) for a whole
// exchange timeout.
const closeBudget = 5 * time.Second

// close ends the HTTP session (a DELETE is the spec's teardown hint; a server may
// ignore it) and drops the session id.
func (t *httpTransport) close() error {
	t.stopListener()
	if t.sessionID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), closeBudget)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.url, nil)
		if err == nil {
			req.Header.Set("Mcp-Session-Id", t.sessionID)
			if t.protocolVersion != "" {
				req.Header.Set("MCP-Protocol-Version", t.protocolVersion)
			}
			for k, v := range t.headers {
				req.Header.Set(k, v)
			}
			resp, err := t.client.Do(req)
			if err == nil {
				io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
			}
		}
	}
	t.sessionID = ""
	t.protocolVersion = ""
	if t.custom != nil {
		t.custom.CloseIdleConnections()
	}
	return nil
}
