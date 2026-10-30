package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestClientEndToEnd drives Client against a real Server mounted on
// httptest (loopback only): discovery, message/send round-trip, task
// correlation, cancel.
func TestClientEndToEnd(t *testing.T) {
	store := NewMemoryTaskStore()
	server := NewServer(ServerConfig{Card: testCard(true), Handle: echoHandler, Store: store})
	httpSrv := httptest.NewServer(server.Handler())
	defer httpSrv.Close()

	client := NewClient(httpSrv.URL)

	card, err := client.AgentCard(context.Background())
	if err != nil {
		t.Fatalf("AgentCard: %v", err)
	}
	if card.Name != "tester" || !card.Capabilities.Streaming {
		t.Fatalf("card = %+v", card)
	}

	reply, err := client.SendMessage(context.Background(), NewMessage(RoleUser, NewTextPart("hello a2a")))
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	tp, ok := reply.Parts[0].(*TextPart)
	if !ok || tp.Text != "echo: hello a2a" {
		t.Fatalf("reply = %+v", reply)
	}

	task, err := client.TasksGet(context.Background(), reply.TaskID)
	if err != nil {
		t.Fatalf("TasksGet: %v", err)
	}
	if task.Status.State != TaskStateCompleted {
		t.Fatalf("task state = %q", task.Status.State)
	}

	if _, err := client.TasksCancel(context.Background(), reply.TaskID); !errors.Is(err, ErrTaskNotCancelable) {
		t.Fatalf("cancel of completed task: err = %v, want ErrTaskNotCancelable", err)
	}

	// A seeded working task cancels cleanly.
	if err := store.Save(context.Background(), &Task{ID: "t-live", Status: TaskStatus{State: TaskStateWorking}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	canceled, err := client.TasksCancel(context.Background(), "t-live")
	if err != nil {
		t.Fatalf("TasksCancel: %v", err)
	}
	if canceled.Status.State != TaskStateCanceled {
		t.Fatalf("canceled state = %q", canceled.Status.State)
	}

	if _, err := client.TasksGet(context.Background(), "no-such-task"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("TasksGet absent: err = %v, want ErrTaskNotFound", err)
	}
}

func TestClientAgentCardLegacyPath(t *testing.T) {
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != LegacyCardPath {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(testCard(false))
	}))
	defer httpSrv.Close()

	client := NewClient(httpSrv.URL, WithCardPath(LegacyCardPath))
	card, err := client.AgentCard(context.Background())
	if err != nil {
		t.Fatalf("AgentCard: %v", err)
	}
	if card.Name != "tester" {
		t.Fatalf("card name = %q", card.Name)
	}
}

func TestClientErrorMapping(t *testing.T) {
	tests := []struct {
		name string
		code int
		want error
	}{
		{"task not found", codeTaskNotFound, ErrTaskNotFound},
		{"task not cancelable", codeTaskNotCancelable, ErrTaskNotCancelable},
		{"method not found", codeMethodNotFound, ErrRPC},
		{"internal", codeInternalError, ErrRPC},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(rpcResponse{
					JSONRPC: "2.0", ID: json.RawMessage("1"),
					Error: &RPCError{Code: tt.code, Message: "remote says no"},
				})
			}))
			defer httpSrv.Close()

			client := NewClient(httpSrv.URL)
			_, err := client.SendMessage(context.Background(), NewMessage(RoleUser, NewTextPart("x")))
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want errors.Is %v", err, tt.want)
			}
			if _, err := client.TasksGet(context.Background(), "t-1"); !errors.Is(err, tt.want) {
				t.Fatalf("TasksGet err = %v, want errors.Is %v", err, tt.want)
			}
		})
	}
}

func TestClientNon2xxStatus(t *testing.T) {
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "kaboom", http.StatusBadGateway)
	}))
	defer httpSrv.Close()

	client := NewClient(httpSrv.URL)
	_, err := client.SendMessage(context.Background(), NewMessage(RoleUser, NewTextPart("x")))
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("err = %v, want ErrTransport", err)
	}
	if !strings.Contains(err.Error(), "502") {
		t.Fatalf("error should carry the status, got: %v", err)
	}
}

func TestClientSendsHeaders(t *testing.T) {
	var seen string
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == rpcPath {
			seen = r.Header.Get("X-Trace")
			_ = json.NewEncoder(w).Encode(rpcResponse{
				JSONRPC: "2.0", ID: json.RawMessage("1"),
				Result: mustJSON(t, NewMessage(RoleAgent)),
			})
			return
		}
		_ = json.NewEncoder(w).Encode(testCard(false))
	}))
	defer httpSrv.Close()

	client := NewClient(httpSrv.URL, WithHeaders(map[string]string{"X-Trace": "tr-1"}))
	if _, err := client.AgentCard(context.Background()); err != nil {
		t.Fatalf("AgentCard: %v", err)
	}
	if _, err := client.SendMessage(context.Background(), NewMessage(RoleUser)); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if seen != "tr-1" {
		t.Fatalf("header X-Trace = %q, want tr-1", seen)
	}
}

func TestClientRejectsTaskResultForSendMessage(t *testing.T) {
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		task := &Task{ID: "t-1", ContextID: "c-1", Status: TaskStatus{State: TaskStateWorking}}
		_ = json.NewEncoder(w).Encode(rpcResponse{
			JSONRPC: "2.0", ID: json.RawMessage("1"),
			Result: mustJSON(t, task),
		})
	}))
	defer httpSrv.Close()

	client := NewClient(httpSrv.URL)
	_, err := client.SendMessage(context.Background(), NewMessage(RoleUser, NewTextPart("x")))
	if err == nil {
		t.Fatal("want error when message/send returns a Task")
	}
	if !strings.Contains(err.Error(), "working") {
		t.Fatalf("error should name the task state, got: %v", err)
	}
}

// mustJSON marshals v, failing the test on error.
func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}
