package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"ago/internal/uuid"
)

// Card routes. DefaultCardPath is the A2A 1.x discovery path
// (ag2/ag2/a2a/transports/_common.py:78); LegacyCardPath is the v0.x alias
// kept for pre-1.0 clients (_common.py:77). Both serve the same card.
const (
	DefaultCardPath = "/.well-known/agent-card.json"
	LegacyCardPath  = "/.well-known/agent.json"
)

// rpcPath is the single JSON-RPC endpoint (parity: build_jsonrpc rpc_url="/").
const rpcPath = "/"

// maxRequestBody caps one JSON-RPC body; a well-formed A2A exchange is far
// below this, and the cap keeps a runaway client from pinning server memory.
const maxRequestBody = 8 << 20 // 8 MiB

// ServerConfig wires a Server.
type ServerConfig struct {
	// Card is served verbatim from the discovery routes. Its
	// Capabilities.Streaming flag gates message/stream.
	Card AgentCard
	// Handle turns one incoming user Message into the agent's reply Message.
	// It runs per request; implementations must be safe for concurrent use.
	// A nil Handle answers message/send and message/stream with a JSON-RPC
	// internal error wrapping ErrNoHandler.
	Handle func(ctx context.Context, msg *Message) (*Message, error)
	// Store persists tasks for tasks/get and tasks/cancel. Nil defaults to
	// NewMemoryTaskStore.
	Store TaskStore
}

// Server exposes one agent over A2A: agent-card discovery plus the JSON-RPC
// methods message/send, message/stream, tasks/get and tasks/cancel. The zero
// flow: Handler() is an http.Handler; mount it on any HTTP server.
type Server struct {
	cfg   ServerConfig
	card  AgentCard // the config card with discovery defaults applied
	store TaskStore
}

// NewServer builds a Server from cfg.
func NewServer(cfg ServerConfig) *Server {
	store := cfg.Store
	if store == nil {
		store = NewMemoryTaskStore()
	}
	return &Server{cfg: cfg, card: withDiscoveryDefaults(cfg.Card), store: store}
}

// withDiscoveryDefaults returns card with the fields a 1.x client requires to
// resolve a transport filled in: the protocol version and — when the card
// declares no interfaces — one JSONRPC interface over the card URL (parity:
// ag2/ag2/a2a/card.py _build_interfaces, which always emits supported_interfaces
// stamped with PROTOCOL_VERSION_CURRENT; an SDK 1.x or AG2 client fails card
// resolution on an empty supportedInterfaces, _http.py select_interface).
// Interfaces the config declares are served verbatim.
func withDiscoveryDefaults(card AgentCard) AgentCard {
	version := card.ProtocolVersion
	if version == "" {
		version = ProtocolVersionCurrent
	}
	if len(card.SupportedInterfaces) == 0 {
		card.SupportedInterfaces = []AgentInterface{{
			URL:             card.URL,
			ProtocolBinding: ProtocolBindingJSONRPC,
			ProtocolVersion: version,
		}}
	}
	card.ProtocolVersion = version
	return card
}

// Handler returns the http.Handler serving the card discovery routes and the
// JSON-RPC endpoint.
func (s *Server) Handler() http.Handler { return s }

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == DefaultCardPath || r.URL.Path == LegacyCardPath:
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, &s.card)
	case r.URL.Path == rpcPath:
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.serveRPC(w, r)
	default:
		http.NotFound(w, r)
	}
}

// writeJSON writes one JSON body with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

// serveRPC reads one JSON-RPC request and dispatches it. Errors before and
// during dispatch travel back as JSON-RPC error objects (HTTP 200), matching
// the JSON-RPC-over-HTTP convention of the Python transport.
func (s *Server) serveRPC(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		writeRPC(w, &rpcResponse{JSONRPC: "2.0", ID: idOr(nil), Error: &RPCError{
			Code: codeInternalError, Message: "read request body: " + err.Error(),
		}})
		return
	}
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		// A batch is well-formed JSON, so it is an invalid request, not a
		// parse error. JSON-RPC 2.0 batches have no A2A use; refuse them.
		writeRPC(w, &rpcResponse{JSONRPC: "2.0", ID: idOr(nil), Error: &RPCError{
			Code: codeInvalidRequest, Message: "batch requests are not supported",
		}})
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, &rpcResponse{JSONRPC: "2.0", ID: idOr(nil), Error: &RPCError{
			Code: codeParseError, Message: "parse error: " + err.Error(),
		}})
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeRPC(w, &rpcResponse{JSONRPC: "2.0", ID: idOr(req.ID), Error: &RPCError{
			Code: codeInvalidRequest, Message: "not a JSON-RPC 2.0 request",
		}})
		return
	}

	ctx := r.Context()
	switch req.Method {
	case MethodMessageSend:
		s.messageSend(ctx, w, &req)
	case MethodMessageStream:
		s.messageStream(ctx, w, &req)
	case MethodTasksGet:
		s.tasksGet(ctx, w, &req)
	case MethodTasksCancel:
		s.tasksCancel(ctx, w, &req)
	default:
		writeRPC(w, &rpcResponse{JSONRPC: "2.0", ID: idOr(req.ID), Error: &RPCError{
			Code: codeMethodNotFound, Message: "method not supported: " + req.Method,
		}})
	}
}

// writeRPC sends one JSON-RPC reply over HTTP 200.
func writeRPC(w http.ResponseWriter, resp *rpcResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, "encode response: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// writeRPCResult replies with a result.
func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	data, err := json.Marshal(result)
	if err != nil {
		writeRPC(w, &rpcResponse{JSONRPC: "2.0", ID: idOr(id), Error: &RPCError{
			Code: codeInternalError, Message: "encode result: " + err.Error(),
		}})
		return
	}
	writeRPC(w, &rpcResponse{JSONRPC: "2.0", ID: idOr(id), Result: data})
}

// writeRPCError replies with an error object.
func writeRPCError(w http.ResponseWriter, id json.RawMessage, rpcErr *RPCError) {
	writeRPC(w, &rpcResponse{JSONRPC: "2.0", ID: idOr(id), Error: rpcErr})
}

// newTask derives a Task from an incoming message: the client-supplied ids
// win, missing ones are generated (parity: executor.py:89-90).
func (s *Server) newTask(msg *Message) *Task {
	id := msg.TaskID
	if id == "" {
		id = uuid.New()
	}
	contextID := msg.ContextID
	if contextID == "" {
		contextID = uuid.New()
	}
	return &Task{ID: id, ContextID: contextID, Status: TaskStatus{State: TaskStateWorking}}
}

// normalizeReply stamps the reply with role and the task identity so the
// client can correlate it.
func normalizeReply(m *Message, task *Task) *Message {
	if m == nil {
		m = &Message{}
	}
	if m.Role == "" {
		m.Role = RoleAgent
	}
	if m.MessageID == "" {
		m.MessageID = uuid.New()
	}
	if m.ContextID == "" {
		m.ContextID = task.ContextID
	}
	if m.TaskID == "" {
		m.TaskID = task.ID
	}
	return m
}

// errorMessage wraps err into the agent message carried on a failed status.
func errorMessage(err error) *Message {
	return &Message{Role: RoleAgent, MessageID: uuid.New(), Parts: []Part{NewTextPart(err.Error())}}
}

// messageSend serves the synchronous exchange: Handle, persist the task,
// answer with the reply Message (parity: AG2's immediate agents complete
// within one call, so the result is a Message, not a Task). A Handle failure
// persists a failed task and answers with a JSON-RPC internal error.
//
// AGO-DEVIATION: the exchange is recorded completed/failed in the shared
// store before answering — the synchronous handler gives the store its
// terminal state in the same call, which is what makes tasks/get and
// tasks/cancel resolvable (D-050).
func (s *Server) messageSend(ctx context.Context, w http.ResponseWriter, req *rpcRequest) {
	msg, rpcErr := decodeMessageParams(req)
	if rpcErr != nil {
		writeRPCError(w, req.ID, rpcErr)
		return
	}
	task := s.newTask(msg)
	reply, handleErr := s.runHandle(ctx, msg)
	if err := s.commitTask(ctx, task, msg, reply, handleErr); err != nil {
		writeRPCError(w, req.ID, &RPCError{Code: codeInternalError, Message: "save task: " + err.Error()})
		return
	}
	if handleErr != nil {
		writeRPCError(w, req.ID, &RPCError{Code: codeInternalError, Message: handleErr.Error()})
		return
	}
	writeRPCResult(w, req.ID, normalizeReply(reply, task))
}

// commitTask records the terminal state of one exchange: completed with the
// reply in status.message, or failed with the error text. History keeps the
// incoming message.
func (s *Server) commitTask(ctx context.Context, task *Task, msg *Message, reply *Message, handleErr error) error {
	task.History = []Message{*msg}
	if handleErr != nil {
		task.Status = TaskStatus{State: TaskStateFailed, Message: errorMessage(handleErr)}
	} else {
		task.Status = TaskStatus{State: TaskStateCompleted, Message: normalizeReply(reply, task)}
	}
	return s.store.Save(ctx, task)
}

// runHandle invokes the configured handler, mapping a nil handler to a
// sentinel error.
func (s *Server) runHandle(ctx context.Context, msg *Message) (*Message, error) {
	if s.cfg.Handle == nil {
		return nil, fmt.Errorf("%w", ErrNoHandler)
	}
	return s.cfg.Handle(ctx, msg)
}

// tasksGet serves tasks/get. historyLength>0 truncates the echoed history to
// the most recent N messages (parity: tasks.py get_task history_length).
func (s *Server) tasksGet(ctx context.Context, w http.ResponseWriter, req *rpcRequest) {
	var params struct {
		ID            string `json:"id"`
		HistoryLength int    `json:"historyLength"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeRPCError(w, req.ID, &RPCError{Code: codeInvalidParams, Message: "params: " + err.Error()})
		return
	}
	if params.ID == "" {
		writeRPCError(w, req.ID, &RPCError{Code: codeInvalidParams, Message: "params.id is required"})
		return
	}
	task, err := s.store.Get(ctx, params.ID)
	if err != nil {
		writeRPCError(w, req.ID, storeError(err))
		return
	}
	if n := params.HistoryLength; n > 0 && len(task.History) > n {
		task.History = task.History[len(task.History)-n:]
	}
	writeRPCResult(w, req.ID, task)
}

// tasksCancel serves tasks/cancel: a submitted/working task flips to
// canceled; a terminal task answers -32002 (parity: a2a TaskNotCancelableError).
func (s *Server) tasksCancel(ctx context.Context, w http.ResponseWriter, req *rpcRequest) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeRPCError(w, req.ID, &RPCError{Code: codeInvalidParams, Message: "params: " + err.Error()})
		return
	}
	if params.ID == "" {
		writeRPCError(w, req.ID, &RPCError{Code: codeInvalidParams, Message: "params.id is required"})
		return
	}
	task, err := s.store.Get(ctx, params.ID)
	if err != nil {
		writeRPCError(w, req.ID, storeError(err))
		return
	}
	if terminalTaskState(task.Status.State) {
		writeRPCError(w, req.ID, &RPCError{
			Code:    codeTaskNotCancelable,
			Message: fmt.Sprintf("task %s is already in terminal state %q", task.ID, task.Status.State),
		})
		return
	}
	task.Status = TaskStatus{State: TaskStateCanceled}
	if err := s.store.Save(ctx, task); err != nil {
		writeRPCError(w, req.ID, &RPCError{Code: codeInternalError, Message: "save task: " + err.Error()})
		return
	}
	writeRPCResult(w, req.ID, task)
}

// storeError maps a TaskStore failure to its wire error.
func storeError(err error) *RPCError {
	if errors.Is(err, ErrTaskNotFound) {
		return &RPCError{Code: codeTaskNotFound, Message: err.Error()}
	}
	return &RPCError{Code: codeInternalError, Message: err.Error()}
}

// messageStream serves message/stream as a Server-Sent Events body: each SSE
// data entry is a JSON-RPC response whose result is a StatusUpdate — one
// working event, then the terminal event carrying the reply on status.message
// (the shape AG2's client absorbs from a completed transition,
// client.py:561-566).
//
// AGO-DEVIATION: two terminal-outcome events only — no chunked text
// artifacts, artifact updates, resubscription or mid-stream cancels; the
// Handle contract is one synchronous call, so the stream cannot do better
// (D-050).
func (s *Server) messageStream(ctx context.Context, w http.ResponseWriter, req *rpcRequest) {
	if !s.cfg.Card.Capabilities.Streaming {
		writeRPCError(w, req.ID, &RPCError{
			Code:    codeUnsupportedOperation,
			Message: "message/stream requires capabilities.streaming on the agent card",
		})
		return
	}
	msg, rpcErr := decodeMessageParams(req)
	if rpcErr != nil {
		writeRPCError(w, req.ID, rpcErr)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeRPCError(w, req.ID, &RPCError{
			Code:    codeInternalError,
			Message: "streaming unsupported by the HTTP connection",
		})
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	emit := func(result any) {
		data, err := json.Marshal(result)
		if err != nil {
			return
		}
		payload, err := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: idOr(req.ID), Result: data})
		if err != nil {
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", payload)
		flusher.Flush()
	}

	task := s.newTask(msg)
	emit(&StatusUpdate{
		Kind: KindStatusUpdate, TaskID: task.ID, ContextID: task.ContextID,
		Status: TaskStatus{State: TaskStateWorking},
	})
	reply, handleErr := s.runHandle(ctx, msg)
	done := &StatusUpdate{Kind: KindStatusUpdate, TaskID: task.ID, ContextID: task.ContextID, Final: true}
	if handleErr != nil {
		done.Status = TaskStatus{State: TaskStateFailed, Message: errorMessage(handleErr)}
	} else {
		done.Status = TaskStatus{State: TaskStateCompleted, Message: normalizeReply(reply, task)}
	}
	// Persist best-effort: the stream is already committed to SSE, so a
	// store failure cannot become a JSON-RPC error here — tasks/get on this
	// id simply may not find the task.
	_ = s.commitTask(ctx, task, msg, reply, handleErr)
	emit(done)
}

// decodeMessageParams unmarshals the {message} params shared by message/send
// and message/stream.
func decodeMessageParams(req *rpcRequest) (*Message, *RPCError) {
	if len(req.Params) == 0 {
		return nil, &RPCError{Code: codeInvalidParams, Message: "params are required: {\"message\": {...}}"}
	}
	var params struct {
		Message *Message `json:"message"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, &RPCError{Code: codeInvalidParams, Message: "params: " + err.Error()}
	}
	if params.Message == nil {
		return nil, &RPCError{Code: codeInvalidParams, Message: "params.message is required"}
	}
	return params.Message, nil
}
