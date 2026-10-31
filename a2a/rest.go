package a2a

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// REST transport (parity: ag2/ag2/a2a/transports/rest.py mounting the a2a-sdk
// create_rest_routes — src/a2a/server/routes/rest_routes.py:69-85). The core
// methods ride on plain HTTP+JSON paths next to the JSON-RPC endpoint; the
// scope is the message/task pair the wave committed to:
//
//	POST /message:send     — one exchange (the 1.x send path)
//	POST /tasks            — the same exchange under the wave's alias path
//	GET  /tasks/{id}       — tasks/get (historyLength query parameter)
//	POST /tasks/{id}:cancel — tasks/cancel
//
// Payloads and errors use application/a2a+json (the a2a-sdk binding media
// type, src/a2a/utils/constants.py A2A_JSON_MEDIA_TYPE); failures map to HTTP
// statuses — bad request params → 400, unknown task → 404, cancel against a
// terminal task → 409, handler/store failures → 500 — with the JSON-RPC error
// object in the body.
const (
	RESTMessageSendPath = "/message:send"
	RESTTasksPath       = "/tasks"
	// MediaA2AJSON is the HTTP+JSON binding payload media type
	// (a2a-sdk constants.py A2A_JSON_MEDIA_TYPE).
	MediaA2AJSON = "application/a2a+json"
)

// serveREST routes the REST paths, reporting whether the request was handled.
func (s *Server) serveREST(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && (path == RESTMessageSendPath || path == RESTTasksPath):
		s.restMessageSend(w, r)
	case r.Method == http.MethodPost && strings.HasSuffix(path, ":cancel") && strings.HasPrefix(path, RESTTasksPath+"/"):
		s.restTaskCancel(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, RESTTasksPath+"/"):
		s.restTaskGet(w, r)
	default:
		return false
	}
	return true
}

// restMessageSend handles POST /message:send and POST /tasks: the body is one
// Message, the response the reply Message — or the parked Task when the turn
// went input-required (the same shapes the JSON-RPC method returns).
func (s *Server) restMessageSend(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		writeRESTError(w, http.StatusBadRequest, codeInvalidRequest, "read body: "+err.Error())
		return
	}
	var msg Message
	if err := json.Unmarshal(body, &msg); err != nil {
		writeRESTError(w, http.StatusBadRequest, codeInvalidRequest, "decode message: "+err.Error())
		return
	}
	outcome, rpcErr := s.executeSend(r.Context(), &msg)
	if rpcErr != nil {
		writeREST(w, rpcErr)
		return
	}
	if outcome.handleErr != nil {
		writeRESTError(w, http.StatusInternalServerError, codeInternalError, msgInternalError)
		return
	}
	// A refused commit answers with the stored Task record: the turn lost
	// the race to a cancel or an earlier terminal transition.
	if outcome.state == TaskStateInputRequired || outcome.refused {
		writeRESTJSON(w, http.StatusOK, outcome.task)
		return
	}
	writeRESTJSON(w, http.StatusOK, normalizeReply(outcome.reply, outcome.task))
}

// restTaskGet handles GET /tasks/{id}: the Task record, with ?historyLength=N
// truncating the echoed history (parity: tasks.py get_task history_length).
func (s *Server) restTaskGet(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, RESTTasksPath+"/")
	if id == "" || strings.Contains(id, "/") {
		writeRESTError(w, http.StatusNotFound, codeTaskNotFound, "task id is required: GET /tasks/{id}")
		return
	}
	historyLength := 0
	if raw := r.URL.Query().Get("historyLength"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeRESTError(w, http.StatusBadRequest, codeInvalidParams, "historyLength: "+raw)
			return
		}
		historyLength = n
	}
	task, rpcErr := s.getTask(r.Context(), id, historyLength)
	if rpcErr != nil {
		writeREST(w, rpcErr)
		return
	}
	writeRESTJSON(w, http.StatusOK, task)
}

// restTaskCancel handles POST /tasks/{id}:cancel.
func (s *Server) restTaskCancel(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, RESTTasksPath+"/"), ":cancel")
	if id == "" || strings.Contains(id, "/") {
		writeRESTError(w, http.StatusNotFound, codeTaskNotFound, "task id is required: POST /tasks/{id}:cancel")
		return
	}
	task, rpcErr := s.cancelTask(r.Context(), id)
	if rpcErr != nil {
		writeREST(w, rpcErr)
		return
	}
	writeRESTJSON(w, http.StatusOK, task)
}

// writeREST maps a JSON-RPC-shaped error to the REST HTTP status.
func writeREST(w http.ResponseWriter, rpcErr *RPCError) {
	switch rpcErr.Code {
	case codeTaskNotFound:
		writeRESTError(w, http.StatusNotFound, rpcErr.Code, rpcErr.Message)
	case codeTaskNotCancelable:
		writeRESTError(w, http.StatusConflict, rpcErr.Code, rpcErr.Message)
	case codeInvalidParams, codeInvalidRequest:
		writeRESTError(w, http.StatusBadRequest, rpcErr.Code, rpcErr.Message)
	default:
		writeRESTError(w, http.StatusInternalServerError, rpcErr.Code, rpcErr.Message)
	}
}

// writeRESTJSON writes one REST body with the binding media type
// (a2a-sdk constants.py A2A_JSON_MEDIA_TYPE).
func writeRESTJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", MediaA2AJSON)
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

// writeRESTError writes one {error: {...}} body with the given status.
func writeRESTError(w http.ResponseWriter, status, code int, message string) {
	writeRESTJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}
