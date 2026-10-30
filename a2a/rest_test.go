package a2a

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRESTMessageSend drives POST /message:send and its /tasks alias: the
// body is a Message, the reply a Message, the exchange recorded in the store.
func TestRESTMessageSend(t *testing.T) {
	srv := NewServer(ServerConfig{Card: testCard(false), Handle: echoHandler})
	h := srv.Handler()

	for _, path := range []string{RESTMessageSendPath, RESTTasksPath} {
		body := marshalMessage(t, NewMessage(RoleUser, NewTextPart("rest hello")))
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s: status %d body %s", path, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != MediaA2AJSON {
			t.Fatalf("POST %s: content-type %q, want %q", path, ct, MediaA2AJSON)
		}
		var reply Message
		if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
			t.Fatalf("POST %s: decode: %v", path, err)
		}
		if !strings.HasPrefix(textOfMessage(&reply), "echo: rest hello") {
			t.Fatalf("POST %s: reply %q", path, textOfMessage(&reply))
		}
		if reply.TaskID == "" {
			t.Fatalf("POST %s: reply has no task id", path)
		}
	}
}

// TestRESTTaskGetAndCancel drives GET /tasks/{id} (with historyLength), the
// cancel path, and the error mappings: unknown id → 404, terminal cancel →
// 409, bad params → 400.
func TestRESTTaskGetAndCancel(t *testing.T) {
	srv := NewServer(ServerConfig{Card: testCard(false), Handle: echoHandler})
	h := srv.Handler()

	// One completed exchange, then a continuation so history accumulates two
	// messages on the same task.
	first := marshalMessage(t, NewMessage(RoleUser, NewTextPart("one")))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, RESTMessageSendPath, strings.NewReader(first)))
	if rec.Code != http.StatusOK {
		t.Fatalf("first send: %d", rec.Code)
	}
	var reply Message
	_ = json.Unmarshal(rec.Body.Bytes(), &reply)

	cont := NewMessage(RoleUser, NewTextPart("two"))
	cont.TaskID = reply.TaskID
	cont.ContextID = reply.ContextID
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, RESTMessageSendPath, strings.NewReader(marshalMessage(t, cont))))
	if rec.Code != http.StatusOK {
		t.Fatalf("second send: %d %s", rec.Code, rec.Body.String())
	}

	// Full history.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, RESTTasksPath+"/"+reply.TaskID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	var task Task
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	if len(task.History) != 2 {
		t.Fatalf("history len = %d, want 2", len(task.History))
	}
	if task.Status.State != TaskStateCompleted {
		t.Fatalf("state = %q", task.Status.State)
	}

	// historyLength=1 truncates to the last message.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, RESTTasksPath+"/"+reply.TaskID+"?historyLength=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get truncated: %d", rec.Code)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &task)
	if len(task.History) != 1 {
		t.Fatalf("truncated history len = %d, want 1", len(task.History))
	}

	// Unknown task → 404 with the JSON-RPC code in the body.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, RESTTasksPath+"/absent", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("absent task: %d, want 404", rec.Code)
	}
	var errBody struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil || errBody.Error.Code != codeTaskNotFound {
		t.Fatalf("absent task body = %s", rec.Body.String())
	}

	// Bad historyLength → 400.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, RESTTasksPath+"/"+reply.TaskID+"?historyLength=nope", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad historyLength: %d, want 400", rec.Code)
	}

	// Cancel the completed task → 409 (terminal).
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, RESTTasksPath+"/"+reply.TaskID+":cancel", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("terminal cancel: %d, want 409", rec.Code)
	}

	// A cancelable task: park one input-required via the extension leg (a
	// parked task is non-terminal), cancel via REST → 200, state canceled.
	parking := NewServer(ServerConfig{
		Card: testCard(false),
		HandleClientTools: func(_ context.Context, _ *Message, _ []ToolSchema) (*Message, []ToolCall, error) {
			return nil, []ToolCall{{ID: "c1", Name: "calc"}}, nil
		},
	})
	parked := marshalMessage(t, AttachClientTools(NewMessage(RoleUser, NewTextPart("park")), []ToolSchema{{Name: "calc"}}))
	rec = httptest.NewRecorder()
	parking.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, RESTTasksPath, strings.NewReader(parked)))
	if rec.Code != http.StatusOK {
		t.Fatalf("park send: %d %s", rec.Code, rec.Body.String())
	}
	var parkedTask Task
	if err := json.Unmarshal(rec.Body.Bytes(), &parkedTask); err != nil {
		t.Fatalf("decode parked task: %v (%s)", err, rec.Body.String())
	}
	if parkedTask.Status.State != TaskStateInputRequired {
		t.Fatalf("parked state = %q, want input-required", parkedTask.Status.State)
	}
	rec = httptest.NewRecorder()
	parking.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, RESTTasksPath+"/"+parkedTask.ID+":cancel", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel parked: %d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &task)
	if task.Status.State != TaskStateCanceled {
		t.Fatalf("canceled state = %q", task.Status.State)
	}
}

// TestRESTOverHTTPClient exercises the REST surface through a real Client's
// HTTP machinery — the same wire a non-JSON-RPC consumer would speak.
func TestRESTOverHTTPClient(t *testing.T) {
	srv := NewServer(ServerConfig{Card: testCard(false), Handle: echoHandler})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body, err := json.Marshal(NewMessage(RoleUser, NewTextPart("via http")))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(ts.URL+"/tasks", MediaA2AJSON, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	var reply Message
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		t.Fatalf("decode: %v", err)
	}

	resp, err = http.Get(ts.URL + "/tasks/" + reply.TaskID)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var task Task
	if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	if task.ID != reply.TaskID {
		t.Fatalf("task id %q, want %q", task.ID, reply.TaskID)
	}
}

// TestRESTUnknownPathFallsThrough checks non-REST paths still hit the JSON-RPC
// / discovery routing (404 from the default branch, not the REST one).
func TestRESTUnknownPathFallsThrough(t *testing.T) {
	srv := NewServer(ServerConfig{Card: testCard(false), Handle: echoHandler})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nowhere", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path: %d, want 404", rec.Code)
	}
}
