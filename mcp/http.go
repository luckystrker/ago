package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/luckystrker/ago/internal/sse"
)

// httpTransport speaks JSON-RPC 2.0 over streamable HTTP: every message is a POST
// to the server URL; a request's answer comes back as application/json or as an
// SSE stream whose data entries are JSON-RPC messages. The session id the server
// assigns at initialize travels back on every request (Mcp-Session-Id).
type httpTransport struct {
	url       string
	headers   map[string]string
	client    *http.Client
	sessionID string
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
	// AGO-DEVIATION: http.Client.Timeout caps the whole exchange (including
	// reading an SSE response body), while Python's httpx timeout is per-phase
	// (connect/read/write); a server answering slowly but steadily is cut off
	// at the cap here.
	return &httpTransport{url: cfg.URL, headers: headers, client: &http.Client{Timeout: timeout}}, nil
}

func hasAuthorizationHeader(headers map[string]string) bool {
	for k := range headers {
		if strings.EqualFold(k, "Authorization") {
			return true
		}
	}
	return false
}

// post sends one wire message and returns the response.
func (t *httpTransport) post(ctx context.Context, body any) (*http.Response, error) {
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
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp: server %s unreachable: %w", t.url, err)
	}
	return resp, nil
}

func (t *httpTransport) call(ctx context.Context, id int64, method string, params any) (json.RawMessage, error) {
	resp, err := t.post(ctx, rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
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
		return t.callOverSSE(resp.Body, id)
	default:
		// application/json (the declared branch) or a server that omits the header.
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("%w: read response: %v", ErrProtocol, err)
		}
		return decodeResponse(body, id)
	}
}

// callOverSSE scans the response stream for the reply with the awaited id,
// skipping any notification the server interleaves.
func (t *httpTransport) callOverSSE(r io.Reader, id int64) (json.RawMessage, error) {
	sc := sse.NewScanner(r)
	for sc.Next() {
		_, data := sc.Event()
		var msg wireMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("%w: malformed SSE message: %v", ErrProtocol, err)
		}
		if msg.Method != "" || msg.ID == nil {
			continue // notification or server-to-client request
		}
		if *msg.ID != id {
			continue
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("%w: %s", ErrProtocol, msg.Error.Error())
		}
		return msg.Result, nil
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%w: SSE stream: %v", ErrProtocol, err)
	}
	return nil, fmt.Errorf("%w: SSE stream ended without the reply to request %d", ErrProtocol, id)
}

func (t *httpTransport) notify(ctx context.Context, method string, params any) error {
	resp, err := t.post(ctx, rpcNotification{JSONRPC: "2.0", Method: method, Params: params})
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

// close ends the HTTP session (a DELETE is the spec's teardown hint; a server may
// ignore it) and drops the session id.
func (t *httpTransport) close() error {
	if t.sessionID == "" {
		return nil
	}
	req, err := http.NewRequest(http.MethodDelete, t.url, nil)
	if err == nil {
		req.Header.Set("Mcp-Session-Id", t.sessionID)
		for k, v := range t.headers {
			req.Header.Set(k, v)
		}
		resp, err := t.client.Do(req)
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
		}
	}
	t.sessionID = ""
	return nil
}
