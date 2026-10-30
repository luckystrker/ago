package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestServerPushConfigLifecycle drives the four push JSON-RPC methods through
// a mounted server: set (server-issued id), get, list, delete, plus the
// unknown-config miss surfacing as ErrTaskNotFound.
func TestServerPushConfigLifecycle(t *testing.T) {
	srv := NewServer(ServerConfig{
		Card:      testCard(false),
		Handle:    echoHandler,
		PushStore: NewMemoryPushConfigStore(),
	})
	h := srv.Handler()

	// A task must exist before a config can scope to it.
	sendEnvelope := postRPC(t, h, messageSendBody("hello", `"1"`))
	if sendEnvelope.Error != nil {
		t.Fatalf("message/send: %v", sendEnvelope.Error)
	}
	var reply Message
	if err := json.Unmarshal(sendEnvelope.Result, &reply); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	taskID := reply.TaskID

	setBody := rpcBody(MethodPushSet, `{"taskId":"`+taskID+`","pushNotificationConfig":{"url":"http://hooks.example/push","token":"sekret","authentication":{"scheme":"bearer","credentials":"tok"}}}`, `"2"`)
	env := postRPC(t, h, setBody)
	if env.Error != nil {
		t.Fatalf("push set: %v", env.Error)
	}
	var stored PushConfig
	if err := json.Unmarshal(env.Result, &stored); err != nil {
		t.Fatalf("decode stored config: %v", err)
	}
	if stored.URL != "http://hooks.example/push" || stored.Token != "sekret" {
		t.Fatalf("stored config = %+v", stored)
	}
	if stored.ID == "" {
		t.Fatalf("server-issued config id is empty")
	}

	env = postRPC(t, h, rpcBody(MethodPushGet, `{"taskId":"`+taskID+`","id":"`+stored.ID+`"}`, `"3"`))
	if env.Error != nil {
		t.Fatalf("push get: %v", env.Error)
	}
	var got PushConfig
	if err := json.Unmarshal(env.Result, &got); err != nil {
		t.Fatalf("decode got config: %v", err)
	}
	if got.ID != stored.ID {
		t.Fatalf("get returned id %q, want %q", got.ID, stored.ID)
	}

	env = postRPC(t, h, rpcBody(MethodPushList, `{"taskId":"`+taskID+`"}`, `"4"`))
	if env.Error != nil {
		t.Fatalf("push list: %v", env.Error)
	}
	var listed struct {
		Configs []PushConfig `json:"configs"`
	}
	if err := json.Unmarshal(env.Result, &listed); err != nil {
		t.Fatalf("decode listed configs: %v", err)
	}
	if len(listed.Configs) != 1 {
		t.Fatalf("listed %d configs, want 1", len(listed.Configs))
	}

	env = postRPC(t, h, rpcBody(MethodPushDelete, `{"taskId":"`+taskID+`","id":"`+stored.ID+`"}`, `"5"`))
	if env.Error != nil {
		t.Fatalf("push delete: %v", env.Error)
	}
	env = postRPC(t, h, rpcBody(MethodPushGet, `{"taskId":"`+taskID+`","id":"`+stored.ID+`"}`, `"6"`))
	if env.Error == nil {
		t.Fatalf("get after delete succeeded, want task-not-found")
	}
	if !strings.Contains(env.Error.Message, "task not found") && env.Error.Code != codeTaskNotFound {
		t.Fatalf("get after delete error = %+v, want task-not-found", env.Error)
	}

	// The served card flips the push capability from the store (server.py:218).
	req := httptest.NewRequest(http.MethodGet, DefaultCardPath, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var card AgentCard
	if err := json.Unmarshal(rec.Body.Bytes(), &card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	if !card.Capabilities.PushNotifications {
		t.Fatalf("card capabilities.pushNotifications = false with PushStore configured")
	}
}

// TestServerPushDisabled refuses the push methods with -32004 when no store
// is configured.
func TestServerPushDisabled(t *testing.T) {
	srv := NewServer(ServerConfig{Card: testCard(false), Handle: echoHandler})
	env := postRPC(t, srv.Handler(), rpcBody(MethodPushSet, `{"taskId":"t","pushNotificationConfig":{"url":"http://x"}}`, `"1"`))
	if env.Error == nil || env.Error.Code != codeUnsupportedOperation {
		t.Fatalf("push set without store = %+v, want -32004", env.Error)
	}
}

// TestPushDispatchOnCompletion runs the full webhook loop over httptest: a
// task with a push config completes, the default sender POSTs the task record
// to the webhook, the PushReceiver validates the token and hands the task to
// the callback. A bad token gets 401 and never reaches the callback.
func TestPushDispatchOnCompletion(t *testing.T) {
	var (
		mu       sync.Mutex
		received []*Task
	)
	receiver := NewPushReceiver("sekret", func(_ context.Context, task *Task) error {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, task)
		return nil
	})
	hooks := httptest.NewServer(receiver)
	defer hooks.Close()

	srv := NewServer(ServerConfig{
		Card:      testCard(false),
		Handle:    echoHandler,
		PushStore: NewMemoryPushConfigStore(),
	})
	h := srv.Handler()

	sendEnv := postRPC(t, h, messageSendBody("push-me", `"1"`))
	var reply Message
	if err := json.Unmarshal(sendEnv.Result, &reply); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	cfgBody, err := json.Marshal(map[string]any{
		"taskId":                 reply.TaskID,
		"pushNotificationConfig": PushConfig{URL: hooks.URL + "/push", Token: "sekret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if env := postRPC(t, h, rpcBody(MethodPushSet, string(cfgBody), `"2"`)); env.Error != nil {
		t.Fatalf("push set: %v", env.Error)
	}
	// The synchronous server commits a task terminal within its own RPC, so a
	// config on a past task can only fire on a later turn for the same id —
	// the second send continues the first task (client-supplied taskId wins).
	cont := NewMessage(RoleUser, NewTextPart("again"))
	cont.TaskID = reply.TaskID
	cont.ContextID = reply.ContextID
	contBody, err := json.Marshal(map[string]any{"message": cont})
	if err != nil {
		t.Fatal(err)
	}
	if env := postRPC(t, h, rpcBody(MethodMessageSend, string(contBody), `"3"`)); env.Error != nil {
		t.Fatalf("second send: %v", env.Error)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n == 1 {
			mu.Lock()
			task := received[0]
			mu.Unlock()
			if task.Status.State != TaskStateCompleted {
				t.Fatalf("pushed task state = %q, want completed", task.Status.State)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("webhook never fired; received=%d", len(received))
}

// TestPushReceiverTokenValidation checks the receiver contract: wrong/missing
// token → 401 without invoking the callback; GET → 405.
func TestPushReceiverTokenValidation(t *testing.T) {
	called := false
	r := NewPushReceiver("t0k3n", func(_ context.Context, task *Task) error {
		called = true
		if task.ID != "task-1" {
			t.Fatalf("callback task id = %q", task.ID)
		}
		return nil
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/push", strings.NewReader(`{"id":"task-1","contextId":"c","status":{"state":"completed"}}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: status %d, want 401", rec.Code)
	}
	if called {
		t.Fatalf("callback ran on a rejected push")
	}

	req := httptest.NewRequest(http.MethodPost, "/push", strings.NewReader(`{"id":"task-1","contextId":"c","status":{"state":"completed"}}`))
	req.Header.Set(HeaderPushToken, "wrong")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: status %d, want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/push", strings.NewReader(`{"id":"task-1","contextId":"c","status":{"state":"completed"}}`))
	req.Header.Set(HeaderPushToken, "t0k3n")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid token: status %d, want 200", rec.Code)
	}
	if !called {
		t.Fatalf("callback did not run")
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/push", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: status %d, want 405", rec.Code)
	}
}

// TestPushURLValidator rejects a webhook the validator refuses.
func TestPushURLValidator(t *testing.T) {
	srv := NewServer(ServerConfig{
		Card:             testCard(false),
		Handle:           echoHandler,
		PushStore:        NewMemoryPushConfigStore(),
		PushURLValidator: func(url string) bool { return strings.HasPrefix(url, "https://") },
	})
	h := srv.Handler()
	sendEnv := postRPC(t, h, messageSendBody("hello", `"1"`))
	var reply Message
	if err := json.Unmarshal(sendEnv.Result, &reply); err != nil {
		t.Fatal(err)
	}
	env := postRPC(t, h, rpcBody(MethodPushSet, `{"taskId":"`+reply.TaskID+`","pushNotificationConfig":{"url":"http://insecure"}}`, `"2"`))
	if env.Error == nil {
		t.Fatalf("insecure url accepted, want invalid-params rejection")
	}
}

// TestClientPushConfigMethods drives the client-side parity of push.py's
// create/get/list/delete against a live server.
func TestClientPushConfigMethods(t *testing.T) {
	srv := NewServer(ServerConfig{
		Card:      testCard(false),
		Handle:    echoHandler,
		PushStore: NewMemoryPushConfigStore(),
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := NewClient(ts.URL)

	msg := NewMessage(RoleUser, NewTextPart("hello"))
	sent, err := client.SendMessage(context.Background(), msg)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	stored, err := client.SetPushConfig(context.Background(), sent.TaskID, PushConfig{URL: "http://hooks.example/push", Token: "t"})
	if err != nil {
		t.Fatalf("SetPushConfig: %v", err)
	}
	if stored.ID == "" {
		t.Fatalf("stored config has no id")
	}
	got, err := client.GetPushConfig(context.Background(), sent.TaskID, stored.ID)
	if err != nil {
		t.Fatalf("GetPushConfig: %v", err)
	}
	if got.URL != stored.URL {
		t.Fatalf("got url %q, want %q", got.URL, stored.URL)
	}
	list, err := client.ListPushConfigs(context.Background(), sent.TaskID)
	if err != nil {
		t.Fatalf("ListPushConfigs: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list len = %d, want 1", len(list))
	}
	if err := client.DeletePushConfig(context.Background(), sent.TaskID, stored.ID); err != nil {
		t.Fatalf("DeletePushConfig: %v", err)
	}
	_, err = client.GetPushConfig(context.Background(), sent.TaskID, stored.ID)
	if !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("get after delete err = %v, want ErrTaskNotFound", err)
	}
}
