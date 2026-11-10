package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// This file is the SDK-conformant streamable-HTTP transport, served when the
// TransportConfig asks for sessions or SSE answers (mcp.server.streamable_http
// and streamable_http_manager). The zero configuration keeps the stateless JSON
// transport of http.go unchanged.

const (
	headerSession  = "Mcp-Session-Id"
	headerProtocol = "MCP-Protocol-Version"
	headerLastID   = "Last-Event-ID"
)

func (s *Server) strictHTTP() bool { return s.tr.Stateful || s.tr.StreamResponses }

// strictError is the JSON-RPC error body of a transport-level refusal: no request
// id is addressable, so id is an explicit null (JSONRPCError(id=None)).
type strictError struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Error   *rpcError `json:"error"`
}

func (s *Server) writeStrictError(w http.ResponseWriter, status, code int, msg string, sess *mcpSession, data any) {
	e := &rpcError{Code: code, Message: msg}
	if data != nil {
		e.Data, _ = json.Marshal(data)
	}
	body, _ := json.Marshal(strictError{JSONRPC: "2.0", Error: e})
	w.Header().Set("Content-Type", "application/json")
	if sess != nil {
		w.Header().Set(headerSession, sess.id)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *Server) maxBody() int64 {
	if s.tr.MaxRequestBodySize > 0 {
		return int64(s.tr.MaxRequestBodySize)
	}
	return maxRequestBody
}

// acceptTypes parses an Accept header with RFC 7231 wildcards
// (check_accept_headers).
func acceptTypes(r *http.Request) (hasJSON, hasSSE bool) {
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		t := strings.ToLower(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]))
		switch t {
		case "*/*":
			hasJSON, hasSSE = true, true
		case "application/json", "application/*":
			hasJSON = true
		case "text/event-stream", "text/*":
			hasSSE = true
		}
	}
	return
}

func (s *Server) serveStrict(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.strictPost(w, r)
	case http.MethodGet:
		s.strictGet(w, r)
	case http.MethodDelete:
		s.strictDelete(w, r)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		s.writeStrictError(w, http.StatusMethodNotAllowed, codeInvalidRequest, "Method Not Allowed", nil, nil)
	}
}

// protocolOK refuses a request naming a protocol revision this server does not
// speak: the modern era is not served, and the answer names the handshake
// revisions that are, so a client probing with server/discover falls back to the
// handshake (the SDK's auto-negotiation reads exactly this list).
func (s *Server) protocolOK(w http.ResponseWriter, r *http.Request) bool {
	pv := r.Header.Get(headerProtocol)
	if pv == "" || supportedHandshake(pv) {
		return true
	}
	s.writeStrictError(w, http.StatusBadRequest, codeUnsupportedProtocolVersion, "Unsupported protocol version", nil,
		map[string]any{"supported": handshakeVersions, "requested": pv})
	return false
}

// codeUnsupportedProtocolVersion is mcp_types.jsonrpc.UNSUPPORTED_PROTOCOL_VERSION.
const codeUnsupportedProtocolVersion = -32022

// lookupSession resolves the Mcp-Session-Id of a request on a stateful
// transport. A missing header or an unknown/expired id is answered here.
func (s *Server) lookupSession(w http.ResponseWriter, r *http.Request) (*mcpSession, bool) {
	if s.sessions == nil {
		return nil, true
	}
	id := r.Header.Get(headerSession)
	if id == "" {
		s.writeStrictError(w, http.StatusBadRequest, codeInvalidRequest, "Bad Request: Missing session ID", nil, nil)
		return nil, false
	}
	sess, err := s.sessions.get(id)
	if err != nil {
		s.writeStrictError(w, http.StatusNotFound, codeInvalidRequest, "Session not found", nil, nil)
		return nil, false
	}
	return sess, true
}

type incoming struct {
	raw    []byte
	req    rpcRequest
	method string
	hasID  bool
	// reply is set when the message is the client's reply to a server request.
	reply *wireReply
	isRsp bool
	idKey string
}

// parseIncoming decodes one JSON-RPC message of the three kinds a client posts:
// a request, a notification, or a response to a server request. Anything else —
// including a batch, which the SDK does not support — is a validation error.
func parseIncoming(body []byte) (*incoming, int, string) {
	trimmed := bytes.TrimSpace(body)
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		var arr []json.RawMessage
		if json.Unmarshal(trimmed, &arr) == nil {
			return nil, codeInvalidParams, "Validation error: a JSON-RPC batch is not a valid message"
		}
		return nil, codeParseError, "Parse error: " + err.Error()
	}
	var jv string
	_ = json.Unmarshal(probe["jsonrpc"], &jv)
	if jv != "2.0" {
		return nil, codeInvalidParams, "Validation error: not a JSON-RPC 2.0 message"
	}
	in := &incoming{raw: trimmed}
	_ = json.Unmarshal(trimmed, &in.req)
	in.method = in.req.Method
	in.hasID = len(in.req.ID) > 0 && !isNullID(in.req.ID)
	if in.hasID {
		var v any
		_ = json.Unmarshal(in.req.ID, &v)
		in.idKey = fmt.Sprint(v)
	}
	if in.method == "" {
		_, hasResult := probe["result"]
		_, hasError := probe["error"]
		if !in.hasID || (!hasResult && !hasError) {
			return nil, codeInvalidParams, "Validation error: a message needs a method, or an id with a result or error"
		}
		in.isRsp = true
		var m struct {
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		_ = json.Unmarshal(trimmed, &m)
		in.reply = &wireReply{result: m.Result, err: m.Error}
	}
	return in, 0, ""
}

func (s *Server) strictPost(w http.ResponseWriter, r *http.Request) {
	if !s.protocolOK(w, r) {
		return
	}
	hasJSON, hasSSE := acceptTypes(r)
	switch {
	case s.tr.StreamResponses && !(hasJSON && hasSSE):
		s.writeStrictError(w, http.StatusNotAcceptable, codeInvalidRequest,
			"Not Acceptable: Client must accept both application/json and text/event-stream", nil, nil)
		return
	case !s.tr.StreamResponses && !hasJSON:
		s.writeStrictError(w, http.StatusNotAcceptable, codeInvalidRequest,
			"Not Acceptable: Client must accept application/json", nil, nil)
		return
	}
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]))
	if ct != "application/json" {
		s.writeStrictError(w, http.StatusUnsupportedMediaType, codeInvalidRequest,
			"Unsupported Media Type: Content-Type must be application/json", nil, nil)
		return
	}
	limit := s.maxBody()
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		s.writeStrictError(w, http.StatusInternalServerError, codeInternalError, "Error handling POST request", nil, nil)
		return
	}
	if int64(len(body)) > limit {
		s.writeStrictError(w, http.StatusRequestEntityTooLarge, codeInvalidRequest, "Request body too large", nil, nil)
		return
	}
	in, code, msg := parseIncoming(body)
	if in == nil {
		s.writeStrictError(w, http.StatusBadRequest, code, msg, nil, nil)
		return
	}

	// The session: an initialize opens one, everything else must carry its id.
	var sess *mcpSession
	if s.sessions != nil {
		if id := r.Header.Get(headerSession); id != "" {
			var ok bool
			if sess, ok = s.lookupSession(w, r); !ok {
				return
			}
		} else if in.method == "initialize" && in.hasID {
			var err error
			if sess, err = s.sessions.open(); err != nil {
				s.writeStrictError(w, http.StatusServiceUnavailable, codeInternalError, "Too many open sessions", nil, nil)
				return
			}
		} else {
			s.writeStrictError(w, http.StatusBadRequest, codeInvalidRequest, "Bad Request: Missing session ID", nil, nil)
			return
		}
		defer sess.begin()()
	}

	ctx := withRequest(r.Context(), r)
	if sess != nil {
		ctx = withSession(ctx, sess)
	}

	// A client reply to a request this server sent on a stream.
	if in.isRsp {
		if sess != nil {
			sess.deliver(in.idKey, *in.reply)
		}
		s.accepted(w, sess)
		return
	}
	if !in.hasID {
		// A notification: processed, never answered (the spec's 202).
		s.accepted(w, sess)
		s.handle(ctx, &in.req)
		return
	}

	if !s.tr.StreamResponses {
		reply := s.handle(withExchange(ctx, &exchange{sess: sess}), &in.req)
		w.Header().Set("Content-Type", "application/json")
		if sess != nil {
			w.Header().Set(headerSession, sess.id)
		}
		w.WriteHeader(http.StatusOK)
		data, _ := json.Marshal(reply)
		_, _ = w.Write(data)
		return
	}
	s.streamAnswer(w, r, ctx, sess, in)
}

func (s *Server) accepted(w http.ResponseWriter, sess *mcpSession) {
	w.Header().Set("Content-Type", "application/json")
	if sess != nil {
		w.Header().Set(headerSession, sess.id)
	}
	w.WriteHeader(http.StatusAccepted)
}

func streamHeaders(w http.ResponseWriter, sess *mcpSession) {
	h := w.Header()
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("Content-Type", "text/event-stream")
	if sess != nil {
		h.Set(headerSession, sess.id)
	}
}

// requestVersion is the protocol revision a request speaks, for the priming
// decision: an initialize names it in its params, anything else in the header.
func requestVersion(r *http.Request, in *incoming) string {
	if in.method == "initialize" {
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(in.req.Params, &p) == nil && p.ProtocolVersion != "" {
			return p.ProtocolVersion
		}
		return defaultNegotiatedVersion
	}
	if v := r.Header.Get(headerProtocol); v != "" {
		return v
	}
	return defaultNegotiatedVersion
}

// streamAnswer answers one request as an SSE stream: the notifications and
// server-to-client requests of the turn, then the response as the stream's last
// event. With an event store the turn runs on the session's lifetime rather than
// the connection's, so a client that drops mid-turn collects the rest by
// reconnecting with Last-Event-ID.
func (s *Server) streamAnswer(w http.ResponseWriter, r *http.Request, ctx context.Context, sess *mcpSession, in *incoming) {
	key := in.idKey
	if sess != nil {
		key = sess.id + "/" + in.idKey
	}
	// Only a session can be resumed (the GET that resumes needs its id), so a
	// stateless transport stores nothing, as in the SDK.
	var store EventStore
	if sess != nil {
		store = s.tr.EventStore
	}
	ex := &exchange{sess: sess, key: key, store: store, sse: true, canRequest: sess != nil, done: make(chan struct{})}
	if sess != nil {
		sess.mu.Lock()
		sess.exchanges[key] = ex
		sess.mu.Unlock()
		defer func() {
			sess.mu.Lock()
			delete(sess.exchanges, key)
			sess.mu.Unlock()
		}()
	}
	defer close(ex.done)

	if ex.store != nil {
		// The turn outlives the connection; the session (or the server) ends it.
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.WithoutCancel(ctx))
		defer cancel()
		if sess != nil {
			stop := context.AfterFunc(sess.ctx, cancel)
			defer stop()
		}
	}
	ctx = withExchange(ctx, ex)

	streamHeaders(w, sess)
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	if ex.store != nil && atLeast2025_11_25(requestVersion(r, in)) {
		// The priming cursor is minted before the request is dispatched, so its
		// row precedes anything the turn stores for this stream.
		if id, err := ex.store.StoreEvent(ctx, key, nil); err == nil {
			var b strings.Builder
			b.WriteString("id: " + id + "\n")
			if ra := s.tr.SSERetryInterval; ra > 0 {
				fmt.Fprintf(&b, "retry: %d\n", ra/time.Millisecond)
			}
			b.WriteString("data: \n\n")
			_, _ = io.WriteString(w, b.String())
		}
	}
	if fl != nil {
		fl.Flush()
	}
	ex.attach(w)
	reply := s.handle(ctx, &in.req)
	if reply != nil {
		ex.emit(ctx, reply)
	}
	ex.detach()
}

func (s *Server) strictGet(w http.ResponseWriter, r *http.Request) {
	if !s.protocolOK(w, r) {
		return
	}
	if _, hasSSE := acceptTypes(r); !hasSSE {
		s.writeStrictError(w, http.StatusNotAcceptable, codeInvalidRequest,
			"Not Acceptable: Client must accept text/event-stream", nil, nil)
		return
	}
	sess, ok := s.lookupSession(w, r)
	if !ok {
		return
	}
	if sess == nil {
		// A stateless transport has no session to listen for: the silent stream
		// of the plain transport.
		s.serveListenStream(w, r)
		return
	}
	if last := r.Header.Get(headerLastID); last != "" {
		s.resumeStream(w, r, sess, last)
		return
	}
	select {
	case s.streams <- struct{}{}:
		defer func() { <-s.streams }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "too many open streams", http.StatusServiceUnavailable)
		return
	}
	sess.mu.Lock()
	if sess.standalone {
		sess.mu.Unlock()
		s.writeStrictError(w, http.StatusConflict, codeInvalidRequest, "Conflict: Only one SSE stream is allowed per session", nil, nil)
		return
	}
	sess.standalone = true
	sess.streams++
	sess.mu.Unlock()
	defer func() {
		sess.mu.Lock()
		sess.standalone = false
		sess.streams--
		sess.last = time.Now()
		sess.mu.Unlock()
	}()
	streamHeaders(w, sess)
	w.WriteHeader(http.StatusOK)
	if fl, ok := w.(http.Flusher); ok {
		_, _ = io.WriteString(w, ": mcpserver stream open\n\n")
		fl.Flush()
	}
	select {
	case <-r.Context().Done():
	case <-sess.ctx.Done():
	}
}

// resumeStream replays what a reconnecting client missed on the stream its
// Last-Event-ID names, then — if that request is still running — attaches the
// connection to it so the rest of the turn flows here.
func (s *Server) resumeStream(w http.ResponseWriter, r *http.Request, sess *mcpSession, lastID string) {
	streamHeaders(w, sess)
	store := s.tr.EventStore
	fl, _ := w.(http.Flusher)
	flush := func() {
		if fl != nil {
			fl.Flush()
		}
	}
	w.WriteHeader(http.StatusOK)
	flush()
	if store == nil {
		return
	}
	sess.mu.Lock()
	sess.streams++
	sess.mu.Unlock()
	defer func() {
		sess.mu.Lock()
		sess.streams--
		sess.last = time.Now()
		sess.mu.Unlock()
	}()
	write := func(id string, msg json.RawMessage) error {
		_, err := fmt.Fprintf(w, "id: %s\nevent: message\ndata: %s\n\n", id, msg)
		flush()
		return err
	}
	ctx := r.Context()
	last := lastID
	// The store answers which stream the id belongs to only after replaying, so
	// the events are held back until that stream is known to be this session's:
	// another session's id must replay nothing.
	type held struct {
		id  string
		msg json.RawMessage
	}
	var pending []held
	key, err := store.ReplayEventsAfter(ctx, last, func(id string, msg json.RawMessage) error {
		pending = append(pending, held{id, msg})
		return nil
	})
	if err != nil || !strings.HasPrefix(key, sess.id+"/") {
		return // an id this store does not hold, or another session's: nothing to resume
	}
	for _, h := range pending {
		last = h.id
		if err := write(h.id, h.msg); err != nil {
			return
		}
	}
	sess.mu.Lock()
	ex := sess.exchanges[key]
	sess.mu.Unlock()
	if ex == nil {
		// The request finished somewhere between the first replay and this lookup:
		// what it stored in that window — its last chunks and its response — is not
		// in the first pass. Replay once more from where the first pass stopped.
		_, _ = store.ReplayEventsAfter(ctx, last, func(id string, msg json.RawMessage) error {
			last = id
			return write(id, msg)
		})
		return
	}
	// Catch up on anything stored since the first pass and attach under the
	// exchange's lock, so no message falls between replay and live tail.
	ex.mu.Lock()
	_, err = store.ReplayEventsAfter(ctx, last, func(id string, msg json.RawMessage) error {
		last = id
		return write(id, msg)
	})
	if err != nil {
		ex.mu.Unlock()
		return
	}
	ex.w, ex.fl = w, fl
	ex.mu.Unlock()
	select {
	case <-ex.done:
	case <-ctx.Done():
	case <-sess.ctx.Done():
	}
	ex.mu.Lock()
	if ex.w == w {
		ex.w, ex.fl = nil, nil
	}
	ex.mu.Unlock()
}

func (s *Server) strictDelete(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		s.writeStrictError(w, http.StatusMethodNotAllowed, codeInvalidRequest, "Method Not Allowed: Session termination not supported", nil, nil)
		return
	}
	if !s.protocolOK(w, r) {
		return
	}
	sess, ok := s.lookupSession(w, r)
	if !ok {
		return
	}
	s.sessions.drop(sess.id)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
}
