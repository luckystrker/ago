package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// tasks/list (ListTasks) — the query surface over the task store. Python
// parity: ag2/ag2/a2a/tasks.py list_tasks over the a2a-sdk ListTasksRequest;
// the wire fields follow the A2A spec's ListTasks params (camelCase).
//
// AGO-DEVIATION: pagination is an offset cursor (the page token is the decimal
// offset into the filtered, creation-ordered result), because the AGo TaskStore
// is a map of records with no keyset to page from. Python's SDK hands out
// whatever token its store returns; both sides treat the token as opaque.

// ListTasksParams filters and pages tasks/list.
type ListTasksParams struct {
	// ContextID keeps only the tasks of one context.
	ContextID string `json:"contextId,omitempty"`
	// Status keeps only the tasks in one state (for example "working").
	Status TaskState `json:"status,omitempty"`
	// PageSize caps the page; <= 0 returns everything the filter matched.
	PageSize int `json:"pageSize,omitempty"`
	// PageToken is the nextPageToken of the previous page; "" starts at the
	// beginning.
	PageToken string `json:"pageToken,omitempty"`
	// HistoryLength truncates each task's history to the most recent N
	// messages; <= 0 keeps the full history.
	HistoryLength int `json:"historyLength,omitempty"`
	// IncludeArtifacts asks for the tasks' artifacts. AGo tasks carry none
	// (the Handler contract returns a Message), so the flag is accepted and
	// has nothing to include.
	IncludeArtifacts bool `json:"includeArtifacts,omitempty"`
	// StatusTimestampAfter keeps only tasks whose status changed at or after
	// this instant. AGo TaskStatus carries no timestamp, so a non-zero value
	// matches nothing — the filter is accepted for wire compatibility.
	StatusTimestampAfter time.Time `json:"statusTimestampAfter,omitzero"`
}

// ListTasksResult is one page of tasks/list.
type ListTasksResult struct {
	Tasks         []*Task `json:"tasks"`
	NextPageToken string  `json:"nextPageToken,omitempty"`
	PageSize      int     `json:"pageSize,omitempty"`
	TotalSize     int     `json:"totalSize,omitempty"`
}

// TaskLister is the optional TaskStore extension that makes tasks/list
// possible: a store without it answers -32004 (unsupported operation), because
// a TaskStore alone has no way to enumerate its records.
type TaskLister interface {
	// List returns every stored task in a stable order (creation order for
	// the in-memory store), by deep copy.
	List(ctx context.Context) ([]*Task, error)
}

// List implements TaskLister: every record, oldest created first.
func (s *MemoryTaskStore) List(_ context.Context) ([]*Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Task, 0, len(s.tasks))
	// inserted holds creation order; ids removed by Delete may linger in it,
	// so membership is checked against the live map.
	for _, id := range s.inserted {
		stored, ok := s.tasks[id]
		if !ok {
			continue
		}
		clone, err := cloneTask(stored)
		if err != nil {
			return nil, err
		}
		out = append(out, clone)
	}
	return out, nil
}

// listTasks is the transport-independent tasks/list body: read, filter, page.
func (s *Server) listTasks(ctx context.Context, params ListTasksParams) (*ListTasksResult, *RPCError) {
	store, storeErr := s.storeFor(ctx)
	if storeErr != nil {
		return nil, storeError(storeErr)
	}
	lister, ok := store.(TaskLister)
	if !ok {
		return nil, &RPCError{
			Code:    codeUnsupportedOperation,
			Message: "tasks/list needs a TaskStore that implements TaskLister",
		}
	}
	all, err := lister.List(ctx)
	if err != nil {
		return nil, storeError(err)
	}
	matched := make([]*Task, 0, len(all))
	for _, task := range all {
		if params.ContextID != "" && task.ContextID != params.ContextID {
			continue
		}
		if params.Status != "" && task.Status.State != params.Status {
			continue
		}
		if !params.StatusTimestampAfter.IsZero() {
			// No status timestamp is stored, so nothing can satisfy the
			// bound: an honest empty page beats pretending the filter ran.
			continue
		}
		matched = append(matched, task)
	}
	total := len(matched)

	start, rpcErr := pageOffset(params.PageToken)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if start > total {
		start = total
	}
	end := total
	if params.PageSize > 0 && start+params.PageSize < end {
		end = start + params.PageSize
	}
	page := matched[start:end]
	if params.HistoryLength > 0 {
		for _, task := range page {
			if len(task.History) > params.HistoryLength {
				task.History = task.History[len(task.History)-params.HistoryLength:]
			}
		}
	}
	result := &ListTasksResult{Tasks: page, PageSize: params.PageSize, TotalSize: total}
	if end < total {
		result.NextPageToken = strconv.Itoa(end)
	}
	if result.Tasks == nil {
		result.Tasks = []*Task{}
	}
	return result, nil
}

// pageOffset reads a page token: "" is the first page, anything else must be a
// non-negative decimal offset.
func pageOffset(token string) (int, *RPCError) {
	if token == "" {
		return 0, nil
	}
	offset, err := strconv.Atoi(token)
	if err != nil || offset < 0 {
		return 0, &RPCError{Code: codeInvalidParams, Message: "params.pageToken is not a page token this server issued"}
	}
	return offset, nil
}

// tasksList serves tasks/list over JSON-RPC.
func (s *Server) tasksList(ctx context.Context, w http.ResponseWriter, req *rpcRequest) {
	var params ListTasksParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			writeRPCError(w, req.ID, &RPCError{Code: codeInvalidParams, Message: "params: " + err.Error()})
			return
		}
	}
	result, rpcErr := s.listTasks(ctx, params)
	if rpcErr != nil {
		writeRPCError(w, req.ID, rpcErr)
		return
	}
	writeRPCResult(w, req.ID, result)
}

// restTasksList serves GET /tasks (the REST twin of tasks/list): the same
// filters as query parameters.
func (s *Server) restTasksList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	params := ListTasksParams{
		ContextID:        q.Get("contextId"),
		Status:           TaskState(q.Get("status")),
		IncludeArtifacts: q.Get("includeArtifacts") == "true",
		PageToken:        q.Get("pageToken"),
	}
	var rpcErr *RPCError
	if params.PageSize, rpcErr = queryInt(q.Get("pageSize"), "pageSize"); rpcErr != nil {
		writeREST(w, rpcErr)
		return
	}
	if params.HistoryLength, rpcErr = queryInt(q.Get("historyLength"), "historyLength"); rpcErr != nil {
		writeREST(w, rpcErr)
		return
	}
	result, rpcErr := s.listTasks(r.Context(), params)
	if rpcErr != nil {
		writeREST(w, rpcErr)
		return
	}
	writeRESTJSON(w, http.StatusOK, result)
}

// queryInt reads an optional integer query parameter.
func queryInt(raw, name string) (int, *RPCError) {
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, &RPCError{Code: codeInvalidParams, Message: fmt.Sprintf("query %s must be a non-negative integer", name)}
	}
	return n, nil
}

// TasksList lists the server's tasks on the resolved binding (JSON-RPC
// tasks/list, or GET /tasks on REST). ErrBindingUnsupported is not used: both
// bindings serve it.
func (c *Client) TasksList(ctx context.Context, params ListTasksParams) (*ListTasksResult, error) {
	result, err := c.listRaw(ctx, params)
	if err != nil {
		return nil, err
	}
	var listed ListTasksResult
	if err := json.Unmarshal(result, &listed); err != nil {
		return nil, fmt.Errorf("%w: decode task list: %v", ErrWire, err)
	}
	if listed.Tasks == nil {
		listed.Tasks = []*Task{}
	}
	return &listed, nil
}

// listRaw performs tasks/list on the resolved binding.
func (c *Client) listRaw(ctx context.Context, params ListTasksParams) (json.RawMessage, error) {
	ep, err := c.endpoint(ctx)
	if err != nil {
		return nil, err
	}
	if params.HistoryLength == 0 {
		params.HistoryLength = c.historyLen
	}
	if ep.binding != bindingREST {
		return c.call(ctx, MethodTasksList, params)
	}
	q := make([]string, 0, 6)
	add := func(key, value string) {
		if value != "" {
			q = append(q, key+"="+url.QueryEscape(value))
		}
	}
	add("contextId", params.ContextID)
	add("status", string(params.Status))
	add("pageToken", params.PageToken)
	if params.PageSize > 0 {
		add("pageSize", strconv.Itoa(params.PageSize))
	}
	if params.HistoryLength > 0 {
		add("historyLength", strconv.Itoa(params.HistoryLength))
	}
	if params.IncludeArtifacts {
		add("includeArtifacts", "true")
	}
	target := restPath(ep, RESTTasksPath)
	if len(q) > 0 {
		target += "?" + strings.Join(q, "&")
	}
	return c.restDo(ctx, MethodTasksList, http.MethodGet, target, nil)
}
