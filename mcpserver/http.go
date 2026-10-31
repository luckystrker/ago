package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// maxRequestBody caps one JSON-RPC body; a well-formed MCP exchange is far
// below this, and the cap keeps a runaway client from pinning server memory
// (Python parity: TransportConfig.max_request_body_size, reduced to one cap).
const maxRequestBody = 8 << 20 // 8 MiB

// Handler returns the http.Handler serving MCP over streamable HTTP: every
// message is a POST; a request is answered application/json, a notification
// is accepted with 202 and no body (Python parity: the streamable-HTTP
// session manager). GET opens a server-to-client SSE stream, which this
// subset keeps open but silent — no server-initiated messages exist in it.
// Mount it on any HTTP server; the handler serves every path it is mounted on.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.serveHTTP)
}

// serveHTTP dispatches one HTTP exchange.
func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	// The allowlists are the DNS-rebinding boundary and run before any
	// dispatch, on every method alike. Both are fail-closed once set: a
	// configured list admits nothing until a header matches, and the 403
	// carries a fixed message so the configured entries stay off the wire.
	if len(s.allowedHosts) > 0 && !matchFold(r.Host, s.allowedHosts) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if origin := r.Header.Get("Origin"); len(s.allowedOrigins) > 0 && origin != "" &&
		!matchFold(origin, s.allowedOrigins) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.servePost(w, r)
	case http.MethodGet:
		s.serveListenStream(w, r)
	default:
		// DELETE would be the spec's session teardown hint; the subset is
		// stateless and issues no sessions to tear down.
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// matchFold reports whether s equals one of the allowed entries
// case-insensitively. Host and Origin headers are matched whole (host or
// host:port; scheme://host[:port]) — no prefix or suffix tricks, so a
// "good.example" entry never admits "good.example.evil.io".
func matchFold(s string, allowed []string) bool {
	for _, a := range allowed {
		if strings.EqualFold(s, a) {
			return true
		}
	}
	return false
}

// requestKey is the context key carrying the HTTP request a message arrived
// on, so ServerConfig.Authorize can read the caller's credentials.
type requestKey struct{}

// withRequest attaches the originating HTTP request to a dispatch context.
func withRequest(ctx context.Context, r *http.Request) context.Context {
	return context.WithValue(ctx, requestKey{}, r)
}

// RequestFromContext returns the HTTP request the message being handled
// arrived on, or nil when there is none (the stdio transport, or a call
// outside the serve loop). A ServerConfig.Authorize hook reads the caller's
// material from it — Authorization header, client certificate, remote
// address.
func RequestFromContext(ctx context.Context) *http.Request {
	if r, ok := ctx.Value(requestKey{}).(*http.Request); ok {
		return r
	}
	return nil
}

// servePost reads one JSON-RPC message and answers it.
//
// AGO-DEVIATION: replies are always application/json — Python's
// json_response=True mode — so the Accept header is not negotiated and the
// per-request SSE answer branch of streamable HTTP is not served; the SSE
// surface is the GET listener stream only. No Mcp-Session-Id is issued and an
// incoming one is ignored: the sessions that would give it meaning are
// deferred (D-056), and a stateless server answers every request the same way.
func (s *Server) servePost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		writeRPC(w, &rpcResponse{JSONRPC: "2.0",
			Error: rpcErr(codeInternalError, "read request body: %v", err)})
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		writeRPC(w, &rpcResponse{JSONRPC: "2.0",
			Error: rpcErr(codeParseError, "empty request body")})
		return
	}
	if trimmed[0] == '[' {
		// A batch is well-formed JSON, so it is an invalid request, not a
		// parse error. JSON-RPC 2.0 batches have no MCP use here; refuse them.
		writeRPC(w, &rpcResponse{JSONRPC: "2.0",
			Error: rpcErr(codeInvalidRequest, "batch requests are not supported")})
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(trimmed, &req); err != nil {
		writeRPC(w, &rpcResponse{JSONRPC: "2.0",
			Error: rpcErr(codeParseError, "parse error: %v", err)})
		return
	}
	if req.JSONRPC != "2.0" {
		writeRPC(w, &rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: rpcErr(codeInvalidRequest, "not a JSON-RPC 2.0 message")})
		return
	}
	if req.ID == nil || isNullID(req.ID) {
		// A notification: processed, never answered (the spec's 202).
		s.handle(withRequest(r.Context(), r), &req)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeRPC(w, s.handle(withRequest(r.Context(), r), &req))
}

// serveListenStream holds a server-to-client SSE stream open until the client
// goes away. The subset pushes nothing over it — every reply this server owes
// travels inside the request's own POST exchange — so the stream stays silent,
// which the streamable-HTTP transport permits: a client listening on GET
// simply never receives server-initiated messages. The comment frame is the
// SSE keepalive convention, so intermediaries see liveness.
//
// Each held stream pins a connection and a goroutine until the client goes
// away, so the count is bounded: a GET over the cap is refused without
// holding a slot, and the client retries on its own schedule.
func (s *Server) serveListenStream(w http.ResponseWriter, r *http.Request) {
	select {
	case s.streams <- struct{}{}:
		defer func() { <-s.streams }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "too many open streams", http.StatusServiceUnavailable)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if flusher, ok := w.(http.Flusher); ok {
		_, _ = io.WriteString(w, ": mcpserver stream open\n\n")
		flusher.Flush()
	}
	<-r.Context().Done()
}

// writeRPC sends one JSON-RPC reply over HTTP 200 (a JSON-RPC error object is
// still a successful HTTP exchange — the protocol, not the transport,
// carries the failure).
func writeRPC(w http.ResponseWriter, resp *rpcResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, "encode response: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data) // a client gone mid-reply leaves nothing to do
}
