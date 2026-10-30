package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/luckystrker/ago/internal/uuid"
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
	// A nil Handle answers plain message/send and message/stream with a
	// JSON-RPC internal error wrapping ErrNoHandler.
	Handle func(ctx context.Context, msg *Message) (*Message, error)
	// HandleClientTools serves the urn:ag2:client-tools:v1 extension (D-056):
	// when the incoming message announces client-side tools (a tool-schemas
	// data part), it replaces Handle for that turn. Returning calls parks the
	// task input-required with the calls attached on the status message — the
	// client executes them locally and resumes the task — while returning no
	// calls completes the task with reply directly (parity: executor.py's
	// pending_client_calls → updater.requires_input branch).
	HandleClientTools func(ctx context.Context, msg *Message, schemas []ToolSchema) (reply *Message, calls []ToolCall, err error)
	// HandleToolResults resumes a task parked input-required: the client came
	// back with tool-result data parts under the same task id. The returned
	// message completes the task (parity: executor.py's ToolResultsEvent
	// continuation leg). A message carrying results without this callback
	// fails the turn.
	HandleToolResults func(ctx context.Context, msg *Message, results []ToolResultEntry) (*Message, error)
	// SyncVariables engages the context-variable sync (D-056): when the
	// incoming message carries a context_update metadata delta, it is
	// stripped of reserved prefixes and handed here keyed by task id; the
	// returned final set is stripped and attached to the reply under the same
	// key (parity: executor.py merged_variables / _build_final_message over
	// mappers/messages.py extract_context_update). Nil disables the sync.
	SyncVariables func(ctx context.Context, taskID string, incoming map[string]any) (outgoing map[string]any, err error)
	// PushStore enables the push-notification surface (parity: server.py —
	// push_config_store is not None flips capabilities.pushNotifications).
	// Nil keeps the card flag as configured and refuses the push JSON-RPC
	// methods with -32004.
	PushStore PushConfigStore
	// PushSender delivers a terminal task to one subscriber webhook; nil
	// defaults to DefaultPushSender. Delivery is best-effort and asynchronous.
	PushSender PushSender
	// PushURLValidator rejects webhook URLs at set time; nil accepts every
	// non-empty URL (parity: server.py push_url_validator, whose absence
	// disables validation).
	PushURLValidator func(url string) bool
	// Store persists tasks for tasks/get and tasks/cancel. Nil defaults to
	// NewMemoryTaskStore.
	Store TaskStore
}

// Server exposes one agent over A2A: agent-card discovery, the JSON-RPC
// methods message/send, message/stream, tasks/get, tasks/cancel and the
// push-notification config surface, plus the REST twin of the core methods.
// The zero flow: Handler() is an http.Handler; mount it on any HTTP server.
// Safe for concurrent use: task state transitions are atomic (see updateTask).
type Server struct {
	cfg   ServerConfig
	card  AgentCard // the config card with discovery defaults applied
	store TaskStore
	// taskLocks serializes one read-modify-write cycle per task id for stores
	// without TaskUpdater, which leave the cycle split across a Get and a
	// Save. Entries live for the Server's lifetime — bounded by the number of
	// distinct task ids, and unused at all with the default MemoryTaskStore.
	locksMu   sync.Mutex
	taskLocks map[string]*sync.Mutex
}

// NewServer builds a Server from cfg. The served card flips the
// pushNotifications capability from the configured PushStore (parity:
// server.py:218), so the config card does not have to keep the flag in sync.
func NewServer(cfg ServerConfig) *Server {
	store := cfg.Store
	if store == nil {
		store = NewMemoryTaskStore()
	}
	card := withDiscoveryDefaults(cfg.Card)
	if cfg.PushStore != nil {
		card.Capabilities.PushNotifications = true
	}
	return &Server{cfg: cfg, card: card, store: store}
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

// Handler returns the http.Handler serving the card discovery routes, the
// JSON-RPC endpoint and the REST transport.
func (s *Server) Handler() http.Handler { return s }

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.serveREST(w, r) {
		return
	}
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

// writeJSON writes one JSON body with the given status. Discovery and
// JSON-RPC replies use application/json; the REST transport overrides the
// header to the binding media type (rest.go).
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
	case MethodPushSet:
		s.servePushSet(ctx, w, &req)
	case MethodPushGet:
		s.servePushGet(ctx, w, &req)
	case MethodPushList:
		s.servePushList(ctx, w, &req)
	case MethodPushDelete:
		s.servePushDelete(ctx, w, &req)
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

// loadOrCreateTask resolves the task a turn belongs to: a message carrying a
// task id continues the stored record (the client-tools and HITL
// continuations do exactly that), an unknown or absent id opens a fresh task.
func (s *Server) loadOrCreateTask(ctx context.Context, msg *Message) (*Task, error) {
	if msg.TaskID != "" {
		if task, err := s.store.Get(ctx, msg.TaskID); err == nil {
			return task, nil
		}
	}
	return s.newTask(msg), nil
}

// normalizeReply stamps the reply with role and the task identity so the
// client can correlate it. Reply-side metadata survives so a context_update
// delta reaches the client.
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

// sendOutcome is what one message/send-style turn produced.
type sendOutcome struct {
	task      *Task     // the stored record, terminal or parked
	reply     *Message  // the reply message (completed) or the parked message (input-required)
	state     TaskState // terminal state, or input-required
	handleErr error     // the handler failure, if the turn failed
	// refused reports that the stored task was already canceled when the
	// turn tried to commit (the cancel won the race): the turn's outcome was
	// dropped, the stored record stayed untouched and task/reply/state carry
	// that stored record, not the turn's result.
	refused bool
}

// executeSend runs one turn end to end: resolve the task, dispatch (plain
// handle, client-tools extension leg, or tool-results continuation), apply
// the context-variable sync, commit the outcome and fire the push webhooks.
// The *RPCError return covers task-store failures only; handler failures come
// back inside the outcome as a failed task.
func (s *Server) executeSend(ctx context.Context, msg *Message) (*sendOutcome, *RPCError) {
	task, rpcErr := s.beginTurn(ctx, msg)
	if rpcErr != nil {
		return nil, rpcErr
	}
	return s.finishTurn(ctx, task, msg), nil
}

// beginTurn resolves the task a turn continues.
func (s *Server) beginTurn(ctx context.Context, msg *Message) (*Task, *RPCError) {
	task, err := s.loadOrCreateTask(ctx, msg)
	if err != nil {
		return nil, storeError(err)
	}
	return task, nil
}

// finishTurn runs and commits one turn against task. The commit is atomic
// against other turns on the same task and against tasks/cancel: when the
// stored record turned terminal in the meantime the turn's outcome is
// refused and the outcome reports the stored record (a canceled task never
// resurrects as completed).
func (s *Server) finishTurn(ctx context.Context, task *Task, msg *Message) *sendOutcome {
	reply, state, handleErr := s.runTurn(ctx, task, msg)
	stored, refused, err := s.commitTask(ctx, task, msg, reply, state, handleErr)
	if err != nil {
		return &sendOutcome{
			task: task, state: TaskStateFailed,
			handleErr: fmt.Errorf("save task: %w", err),
		}
	}
	s.dispatchPush(ctx, stored.ID, stored)
	if refused {
		return &sendOutcome{
			task: stored, reply: stored.Status.Message,
			state: stored.Status.State, refused: true,
		}
	}
	return &sendOutcome{task: stored, reply: reply, state: state, handleErr: handleErr}
}

// runTurn dispatches one turn and applies the context-variable sync around
// it: inbound deltas merge before the turn (via the configured callback), the
// returned final set rides the reply (parity: executor.py's
// _dispatch_to_agent merged_variables / _build_final_message bracket).
func (s *Server) runTurn(ctx context.Context, task *Task, msg *Message) (*Message, TaskState, error) {
	var outgoing map[string]any
	if s.cfg.SyncVariables != nil {
		if incoming := ExtractContextUpdate(msg); len(incoming) > 0 {
			var err error
			outgoing, err = s.cfg.SyncVariables(ctx, task.ID, StripReservedVariables(incoming))
			if err != nil {
				return nil, TaskStateFailed, fmt.Errorf("context sync: %w", err)
			}
		}
	}
	reply, state, err := s.dispatchTurn(ctx, msg)
	if err != nil {
		return nil, TaskStateFailed, err
	}
	if len(outgoing) > 0 {
		if reply == nil {
			reply = &Message{}
		}
		WithContextUpdate(reply, StripReservedVariables(outgoing))
	}
	return reply, state, nil
}

// dispatchTurn picks the turn's leg by the message's extension parts
// (parity: executor.py _build_initial_event — tool results make a
// continuation, tool schemas arm client execution, anything else is a plain
// request).
func (s *Server) dispatchTurn(ctx context.Context, msg *Message) (*Message, TaskState, error) {
	results, err := ParseToolResults(msg)
	if err != nil {
		return nil, TaskStateFailed, err
	}
	if len(results) > 0 {
		if s.cfg.HandleToolResults == nil {
			return nil, TaskStateFailed, fmt.Errorf("%w: tool results arrived but no HandleToolResults is configured", ErrNoHandler)
		}
		reply, err := s.cfg.HandleToolResults(ctx, msg, results)
		return reply, TaskStateCompleted, err
	}
	schemas, err := ParseToolSchemas(msg)
	if err != nil {
		return nil, TaskStateFailed, err
	}
	if len(schemas) > 0 && s.cfg.HandleClientTools != nil {
		reply, calls, err := s.cfg.HandleClientTools(ctx, msg, schemas)
		if err != nil {
			return nil, TaskStateFailed, err
		}
		if len(calls) == 0 {
			return reply, TaskStateCompleted, nil
		}
		if reply == nil {
			reply = &Message{Role: RoleAgent}
		}
		for _, c := range calls {
			reply.Parts = append(reply.Parts, ToolCallsPart(c))
		}
		return reply, TaskStateInputRequired, nil
	}
	reply, err := s.runHandle(ctx, msg)
	return reply, TaskStateCompleted, err
}

// updateTask applies mutate to the stored record for id as one atomic
// read-modify-write: the guard inside mutate and the write cannot interleave
// with another transition on the same task. Stores implementing TaskUpdater
// run the cycle under their own lock; for the rest the server holds a
// per-task mutex across the Get→Save pair. The mutate decides the policy and
// returns whether to apply; returning false keeps the stored record
// untouched and refused reports that. ErrTaskNotFound passes through for
// absent ids.
func (s *Server) updateTask(ctx context.Context, id string, mutate func(current *Task) (apply bool)) (stored *Task, refused bool, err error) {
	if up, ok := s.store.(TaskUpdater); ok {
		stored, err := up.Update(ctx, id, func(current *Task) (*Task, error) {
			if !mutate(current) {
				refused = true
			}
			return current, nil
		})
		return stored, refused, err
	}
	unlock := s.lockTask(id)
	defer unlock()
	current, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if !mutate(current) {
		return current, true, nil
	}
	if err := s.store.Save(ctx, current); err != nil {
		return nil, false, fmt.Errorf("save task: %w", err)
	}
	return current, false, nil
}

// lockTask returns the unlock func of the per-task mutex for id, creating it
// on first use.
func (s *Server) lockTask(id string) func() {
	s.locksMu.Lock()
	if s.taskLocks == nil {
		s.taskLocks = make(map[string]*sync.Mutex)
	}
	mu := s.taskLocks[id]
	if mu == nil {
		mu = &sync.Mutex{}
		s.taskLocks[id] = mu
	}
	s.locksMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// commitTask records the turn's outcome atomically: completed with the reply
// on status.message, input-required parked with the calls/question, or failed
// with the error text. History merges onto the record as the store holds it
// at commit time — a concurrent continuation's entry stays, and the turn
// adds exactly its incoming message. A stored canceled record refuses the
// commit (the cancel won the race; the turn's outcome is dropped and the
// task stays canceled — it never resurrects as completed). A completed or
// failed record continues: an explicit new message on the same task id is a
// client-driven continuation (parity: the executor keeps appending history
// for whatever task id arrives). The first commit of a task not yet in the
// store Saves it as-is: there is no stored state to merge with and nothing
// to resurrect. Returns the record now in the store and whether the commit
// was refused.
func (s *Server) commitTask(ctx context.Context, task *Task, msg *Message, reply *Message, state TaskState, handleErr error) (stored *Task, refused bool, err error) {
	apply := func(current *Task) bool {
		if current.Status.State == TaskStateCanceled {
			return false
		}
		current.History = append(current.History, *msg)
		switch {
		case handleErr != nil:
			current.Status = TaskStatus{State: TaskStateFailed, Message: errorMessage(handleErr)}
		case state == TaskStateInputRequired:
			current.Status = TaskStatus{State: TaskStateInputRequired, Message: normalizeReply(reply, current)}
		default:
			current.Status = TaskStatus{State: TaskStateCompleted, Message: normalizeReply(reply, current)}
		}
		return true
	}
	stored, refused, err = s.updateTask(ctx, task.ID, apply)
	if errors.Is(err, ErrTaskNotFound) {
		apply(task)
		if err := s.store.Save(ctx, task); err != nil {
			return nil, false, err
		}
		return task, false, nil
	}
	return stored, refused, err
}

// runHandle invokes the configured handler, mapping a nil handler to a
// sentinel error.
func (s *Server) runHandle(ctx context.Context, msg *Message) (*Message, error) {
	if s.cfg.Handle == nil {
		return nil, fmt.Errorf("%w", ErrNoHandler)
	}
	return s.cfg.Handle(ctx, msg)
}

// messageSend serves the synchronous exchange: Handle (or the extension leg
// the message selects), persist the task, answer with the reply Message —
// or, when the turn parked input-required (client-tools round trip, HITL
// question), with the Task record whose status carries the parked message
// (parity: the a2a-sdk message/send returning a Task for deferred turns;
// AG2's client absorbs both shapes, client.py result probing).
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
	outcome, rpcErr := s.executeSend(ctx, msg)
	if rpcErr != nil {
		writeRPCError(w, req.ID, rpcErr)
		return
	}
	if outcome.handleErr != nil {
		writeRPCError(w, req.ID, &RPCError{Code: codeInternalError, Message: outcome.handleErr.Error()})
		return
	}
	// A refused commit answers with the stored Task record — the turn lost
	// the race to a cancel or an earlier terminal transition, and the record
	// is the only honest reply (same shape as the deferred turns below).
	if outcome.state == TaskStateInputRequired || outcome.refused {
		writeRPCResult(w, req.ID, outcome.task)
		return
	}
	writeRPCResult(w, req.ID, normalizeReply(outcome.reply, outcome.task))
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
	task, rpcErr := s.getTask(ctx, params.ID, params.HistoryLength)
	if rpcErr != nil {
		writeRPCError(w, req.ID, rpcErr)
		return
	}
	writeRPCResult(w, req.ID, task)
}

// getTask is the transport-independent tasks/get body.
func (s *Server) getTask(ctx context.Context, id string, historyLength int) (*Task, *RPCError) {
	task, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, storeError(err)
	}
	if n := historyLength; n > 0 && len(task.History) > n {
		task.History = task.History[len(task.History)-n:]
	}
	return task, nil
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
	task, rpcErr := s.cancelTask(ctx, params.ID)
	if rpcErr != nil {
		writeRPCError(w, req.ID, rpcErr)
		return
	}
	writeRPCResult(w, req.ID, task)
}

// cancelTask is the transport-independent tasks/cancel body: the terminal
// check and the Canceled write are one atomic step, so a turn finishing
// concurrently either lands before the cancel (which then answers -32002) or
// the cancel lands first and the turn's commit is refused — a canceled task
// never resurrects (parity: a2a TaskNotCancelableError).
func (s *Server) cancelTask(ctx context.Context, id string) (*Task, *RPCError) {
	stored, refused, err := s.updateTask(ctx, id, func(current *Task) bool {
		if terminalTaskState(current.Status.State) {
			return false
		}
		current.Status = TaskStatus{State: TaskStateCanceled}
		return true
	})
	if err != nil {
		return nil, storeError(err)
	}
	if refused {
		return nil, &RPCError{
			Code:    codeTaskNotCancelable,
			Message: fmt.Sprintf("task %s is already in terminal state %q", stored.ID, stored.Status.State),
		}
	}
	return stored, nil
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
// client.py:561-566). A turn parked input-required streams a non-final
// input-required update before the final event (the client-tools and HITL
// continuations resume the task through message/send).
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

	task, rpcErr := s.beginTurn(ctx, msg)
	if rpcErr != nil {
		// The SSE body is already committed, so the store failure surfaces as
		// a final failed event rather than a JSON-RPC error.
		task = s.newTask(msg)
		emit(&StatusUpdate{
			Kind: KindStatusUpdate, TaskID: task.ID, ContextID: task.ContextID,
			Status: TaskStatus{State: TaskStateWorking},
		})
		emit(&StatusUpdate{
			Kind: KindStatusUpdate, TaskID: task.ID, ContextID: task.ContextID, Final: true,
			Status: TaskStatus{State: TaskStateFailed, Message: errorMessage(errors.New(rpcErr.Message))},
		})
		return
	}
	emit(&StatusUpdate{
		Kind: KindStatusUpdate, TaskID: task.ID, ContextID: task.ContextID,
		Status: TaskStatus{State: TaskStateWorking},
	})
	outcome := s.finishTurn(ctx, task, msg)
	done := &StatusUpdate{Kind: KindStatusUpdate, TaskID: task.ID, ContextID: task.ContextID, Final: true}
	switch {
	case outcome.handleErr != nil:
		done.Status = TaskStatus{State: TaskStateFailed, Message: errorMessage(outcome.handleErr)}
	case outcome.refused:
		// The stored record turned terminal mid-turn (a cancel won); report
		// its status verbatim instead of the dropped turn outcome.
		done.Status = outcome.task.Status
	default:
		done.Status = TaskStatus{State: outcome.state, Message: normalizeReply(outcome.reply, outcome.task)}
	}
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
