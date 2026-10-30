package mcpserver

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
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
		s.handle(r.Context(), &req)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeRPC(w, s.handle(r.Context(), &req))
}

// serveListenStream holds a server-to-client SSE stream open until the client
// goes away. The subset pushes nothing over it — every reply this server owes
// travels inside the request's own POST exchange — so the stream stays silent,
// which the streamable-HTTP transport permits: a client listening on GET
// simply never receives server-initiated messages. The comment frame is the
// SSE keepalive convention, so intermediaries see liveness.
func (s *Server) serveListenStream(w http.ResponseWriter, r *http.Request) {
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
