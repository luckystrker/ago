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

// echoHandler replies with an agent text part built from the user text.
func echoHandler(ctx context.Context, msg *Message) (*Message, error) {
	text := ""
	for _, p := range msg.Parts {
		if tp, ok := p.(*TextPart); ok {
			text += tp.Text
		}
	}
	return NewMessage(RoleAgent, NewTextPart("echo: "+text)), nil
}

// failingHandler always fails.
var errHandlerBoom = errors.New("boom")

func failingHandler(ctx context.Context, msg *Message) (*Message, error) {
	return nil, errHandlerBoom
}

func testCard(streaming bool) AgentCard {
	return AgentCard{
		Name:               "tester",
		Description:        "test agent",
		URL:                "http://localhost:0",
		Version:            "0.1.0",
		Capabilities:       AgentCapabilities{Streaming: streaming},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills:             []AgentSkill{{ID: "tester", Name: "tester", Description: "echo"}},
	}
}

// postRPC sends one raw JSON-RPC body to the handler and returns the decoded
// envelope.
func postRPC(t *testing.T, h http.Handler, body string) rpcResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, rpcPath, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP status = %d, want 200 (JSON-RPC travels in the body)", rec.Code)
	}
	var envelope rpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope %q: %v", rec.Body.String(), err)
	}
	return envelope
}

// rpcBody wraps params into a JSON-RPC request body.
func rpcBody(method, params, id string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"` + method + `","params":` + params + `}`
}

// textMessageParams is a {message} params object carrying one text part.
func textMessageParams(msg string) string {
	return `{"message":{"role":"user","parts":[{"kind":"text","text":"` + msg + `"}]}}`
}

func messageSendBody(msg string, id string) string {
	return rpcBody(MethodMessageSend, textMessageParams(msg), id)
}

func messageStreamBody(msg string, id string) string {
	return rpcBody(MethodMessageStream, textMessageParams(msg), id)
}

func TestServerCardDiscovery(t *testing.T) {
	h := NewServer(ServerConfig{Card: testCard(false)}).Handler()
	for _, path := range []string{DefaultCardPath, LegacyCardPath} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("GET %s: content-type %q", path, ct)
		}
		var card AgentCard
		if err := json.Unmarshal(rec.Body.Bytes(), &card); err != nil {
			t.Fatalf("GET %s: decode: %v", path, err)
		}
		if card.Name != "tester" {
			t.Fatalf("GET %s: card name %q", path, card.Name)
		}
	}
	// Wrong method on the card route.
	req := httptest.NewRequest(http.MethodPost, DefaultCardPath, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST card route: status %d, want 405", rec.Code)
	}
	// Unknown route.
	req = httptest.NewRequest(http.MethodGet, "/nope", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET unknown route: status %d, want 404", rec.Code)
	}
}

// TestServerCardDiscoveryDefaults: a config card that leaves the discovery
// fields unset is served with them filled — protocolVersion plus one JSONRPC
// supportedInterfaces entry (an SDK 1.x or AG2 client fails card resolution
// on an empty supportedInterfaces). The check reads the raw served JSON, the
// view a wire peer has.
func TestServerCardDiscoveryDefaults(t *testing.T) {
	s := NewServer(ServerConfig{Card: testCard(false)})
	h := s.Handler()

	// The config card itself is not mutated by building the server.
	if s.cfg.Card.ProtocolVersion != "" || s.cfg.Card.SupportedInterfaces != nil {
		t.Fatalf("config card mutated: %+v", s.cfg.Card)
	}

	for _, path := range []string{DefaultCardPath, LegacyCardPath} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var raw struct {
			ProtocolVersion     string `json:"protocolVersion"`
			SupportedInterfaces []struct {
				URL             string `json:"url"`
				ProtocolBinding string `json:"protocolBinding"`
				ProtocolVersion string `json:"protocolVersion"`
				Tenant          string `json:"tenant"`
			} `json:"supportedInterfaces"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatalf("GET %s: decode: %v", path, err)
		}
		if raw.ProtocolVersion != ProtocolVersionCurrent {
			t.Errorf("GET %s: protocolVersion = %q, want %q", path, raw.ProtocolVersion, ProtocolVersionCurrent)
		}
		if len(raw.SupportedInterfaces) != 1 {
			t.Fatalf("GET %s: supportedInterfaces = %+v, want one entry", path, raw.SupportedInterfaces)
		}
		iface := raw.SupportedInterfaces[0]
		if iface.URL != "http://localhost:0" || iface.ProtocolBinding != ProtocolBindingJSONRPC ||
			iface.ProtocolVersion != ProtocolVersionCurrent {
			t.Errorf("GET %s: interface = %+v, want the card URL over %q at %q",
				path, iface, ProtocolBindingJSONRPC, ProtocolVersionCurrent)
		}
	}
}

// TestServerCardDiscoveryExplicitInterfaces: interfaces the config declares
// are served verbatim, and an explicit protocol version wins.
func TestServerCardDiscoveryExplicitInterfaces(t *testing.T) {
	card := testCard(false)
	card.ProtocolVersion = "0.3"
	card.SupportedInterfaces = []AgentInterface{
		{URL: "http://rpc.example", ProtocolBinding: "HTTP+JSON", ProtocolVersion: "1.0", Tenant: "t-1"},
	}
	req := httptest.NewRequest(http.MethodGet, DefaultCardPath, nil)
	rec := httptest.NewRecorder()
	NewServer(ServerConfig{Card: card}).Handler().ServeHTTP(rec, req)
	var back AgentCard
	if err := json.Unmarshal(rec.Body.Bytes(), &back); err != nil {
		t.Fatalf("decode served card: %v", err)
	}
	if back.ProtocolVersion != "0.3" {
		t.Errorf("protocolVersion = %q, want the explicit 0.3", back.ProtocolVersion)
	}
	if len(back.SupportedInterfaces) != 1 || back.SupportedInterfaces[0] != card.SupportedInterfaces[0] {
		t.Errorf("supportedInterfaces = %+v, want the declared interface verbatim", back.SupportedInterfaces)
	}
}

func TestServerMessageSendRoundTrip(t *testing.T) {
	store := NewMemoryTaskStore()
	h := NewServer(ServerConfig{Card: testCard(false), Handle: echoHandler, Store: store}).Handler()

	envelope := postRPC(t, h, messageSendBody("ping", `7`))
	if envelope.Error != nil {
		t.Fatalf("unexpected error: %s", envelope.Error)
	}
	if string(envelope.ID) != "7" {
		t.Fatalf("id = %s, want 7", envelope.ID)
	}
	var reply Message
	if err := json.Unmarshal(envelope.Result, &reply); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if reply.Role != RoleAgent {
		t.Fatalf("reply role = %q", reply.Role)
	}
	if len(reply.Parts) != 1 {
		t.Fatalf("reply parts = %d", len(reply.Parts))
	}
	tp, ok := reply.Parts[0].(*TextPart)
	if !ok || tp.Text != "echo: ping" {
		t.Fatalf("reply part = %#v", reply.Parts[0])
	}
	if reply.MessageID == "" || reply.TaskID == "" || reply.ContextID == "" {
		t.Fatalf("reply identity not stamped: %+v", reply)
	}

	// The exchange is recorded as a completed task holding both messages.
	task, err := store.Get(context.Background(), reply.TaskID)
	if err != nil {
		t.Fatalf("store.Get %s: %v", reply.TaskID, err)
	}
	if task.Status.State != TaskStateCompleted {
		t.Fatalf("task state = %q", task.Status.State)
	}
	if len(task.History) != 1 || task.History[0].Role != RoleUser {
		t.Fatalf("task history = %+v", task.History)
	}
	if task.ContextID != reply.ContextID {
		t.Fatalf("task contextId %q != reply contextId %q", task.ContextID, reply.ContextID)
	}
}

func TestServerMessageSendCarriesClientIDs(t *testing.T) {
	h := NewServer(ServerConfig{Card: testCard(false), Handle: echoHandler}).Handler()
	body := `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"role":"user","messageId":"m-user","taskId":"t-42","contextId":"c-42","parts":[{"kind":"text","text":"hi"}]}}}`
	envelope := postRPC(t, h, body)
	if envelope.Error != nil {
		t.Fatalf("unexpected error: %s", envelope.Error)
	}
	var reply Message
	_ = json.Unmarshal(envelope.Result, &reply)
	if reply.TaskID != "t-42" || reply.ContextID != "c-42" {
		t.Fatalf("client-supplied ids not honored: taskId=%q contextId=%q", reply.TaskID, reply.ContextID)
	}
}

func TestServerMessageSendHandleErrorRecordsFailedTask(t *testing.T) {
	store := NewMemoryTaskStore()
	h := NewServer(ServerConfig{Card: testCard(false), Handle: failingHandler, Store: store}).Handler()
	body := `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"role":"user","taskId":"t-9","parts":[{"kind":"text","text":"x"}]}}}`
	postRPC(t, h, body)
	task, err := store.Get(context.Background(), "t-9")
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if task.Status.State != TaskStateFailed {
		t.Fatalf("task state = %q, want failed", task.Status.State)
	}
	if task.Status.Message == nil || len(task.Status.Message.Parts) != 1 {
		t.Fatalf("failed status message = %+v", task.Status.Message)
	}
	if tp, ok := task.Status.Message.Parts[0].(*TextPart); !ok || tp.Text != "boom" {
		t.Fatalf("failure text = %#v", task.Status.Message.Parts[0])
	}
}

func TestServerNoHandler(t *testing.T) {
	h := NewServer(ServerConfig{Card: testCard(false)}).Handler()
	envelope := postRPC(t, h, messageSendBody("x", `1`))
	if envelope.Error == nil || envelope.Error.Code != codeInternalError {
		t.Fatalf("want internal error, got %+v", envelope.Error)
	}
}

func TestServerInvalidParams(t *testing.T) {
	h := NewServer(ServerConfig{Card: testCard(false), Handle: echoHandler}).Handler()
	tests := []struct {
		name string
		body string
	}{
		{"no params", `{"jsonrpc":"2.0","id":1,"method":"message/send"}`},
		{"no message field", `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{}}`},
		{"malformed params", `{"jsonrpc":"2.0","id":1,"method":"message/send","params":"nope"}`},
		{"unknown part kind", `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"role":"user","parts":[{"kind":"zzz"}]}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			envelope := postRPC(t, h, tt.body)
			if envelope.Error == nil || envelope.Error.Code != codeInvalidParams {
				t.Fatalf("want code %d, got %+v", codeInvalidParams, envelope.Error)
			}
		})
	}
}

func TestServerUnknownMethod(t *testing.T) {
	h := NewServer(ServerConfig{Card: testCard(false)}).Handler()
	envelope := postRPC(t, h, `{"jsonrpc":"2.0","id":1,"method":"tasks/list"}`)
	if envelope.Error == nil || envelope.Error.Code != codeMethodNotFound {
		t.Fatalf("want code %d, got %+v", codeMethodNotFound, envelope.Error)
	}
}

func TestServerMalformedAndBatchBodies(t *testing.T) {
	h := NewServer(ServerConfig{Card: testCard(false)}).Handler()

	envelope := postRPC(t, h, `{not json`)
	if envelope.Error == nil || envelope.Error.Code != codeParseError {
		t.Fatalf("want code %d, got %+v", codeParseError, envelope.Error)
	}
	if string(envelope.ID) != "null" {
		t.Fatalf("parse error id = %s, want null", envelope.ID)
	}

	envelope = postRPC(t, h, `[{"jsonrpc":"2.0","id":1,"method":"tasks/get","params":{"id":"x"}}]`)
	if envelope.Error == nil || envelope.Error.Code != codeInvalidRequest {
		t.Fatalf("batch: want code %d, got %+v", codeInvalidRequest, envelope.Error)
	}

	envelope = postRPC(t, h, `{"jsonrpc":"1.0","id":1,"method":"tasks/get"}`)
	if envelope.Error == nil || envelope.Error.Code != codeInvalidRequest {
		t.Fatalf("bad jsonrpc version: want code %d, got %+v", codeInvalidRequest, envelope.Error)
	}
}

func TestServerTasksGet(t *testing.T) {
	store := NewMemoryTaskStore()
	seed := &Task{
		ID:        "t-1",
		ContextID: "c-1",
		Status:    TaskStatus{State: TaskStateWorking},
		History: []Message{
			*NewMessage(RoleUser, NewTextPart("one")),
			*NewMessage(RoleUser, NewTextPart("two")),
			*NewMessage(RoleUser, NewTextPart("three")),
		},
	}
	if err := store.Save(context.Background(), seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h := NewServer(ServerConfig{Card: testCard(false), Store: store}).Handler()

	envelope := postRPC(t, h, `{"jsonrpc":"2.0","id":1,"method":"tasks/get","params":{"id":"t-1"}}`)
	if envelope.Error != nil {
		t.Fatalf("unexpected error: %s", envelope.Error)
	}
	var task Task
	if err := json.Unmarshal(envelope.Result, &task); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	if len(task.History) != 3 {
		t.Fatalf("full history = %d messages", len(task.History))
	}

	envelope = postRPC(t, h, `{"jsonrpc":"2.0","id":2,"method":"tasks/get","params":{"id":"t-1","historyLength":2}}`)
	if envelope.Error != nil {
		t.Fatalf("unexpected error: %s", envelope.Error)
	}
	_ = json.Unmarshal(envelope.Result, &task)
	if len(task.History) != 2 || task.History[1].Parts[0].(*TextPart).Text != "three" {
		t.Fatalf("truncated history = %+v", task.History)
	}

	envelope = postRPC(t, h, `{"jsonrpc":"2.0","id":3,"method":"tasks/get","params":{"id":"absent"}}`)
	if envelope.Error == nil || envelope.Error.Code != codeTaskNotFound {
		t.Fatalf("want code %d, got %+v", codeTaskNotFound, envelope.Error)
	}

	envelope = postRPC(t, h, `{"jsonrpc":"2.0","id":4,"method":"tasks/get","params":{}}`)
	if envelope.Error == nil || envelope.Error.Code != codeInvalidParams {
		t.Fatalf("empty id: want code %d, got %+v", codeInvalidParams, envelope.Error)
	}
}

func TestServerTasksCancel(t *testing.T) {
	store := NewMemoryTaskStore()
	for _, seed := range []*Task{
		{ID: "working", Status: TaskStatus{State: TaskStateWorking}},
		{ID: "done", Status: TaskStatus{State: TaskStateCompleted}},
	} {
		if err := store.Save(context.Background(), seed); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	h := NewServer(ServerConfig{Card: testCard(false), Store: store}).Handler()

	envelope := postRPC(t, h, `{"jsonrpc":"2.0","id":1,"method":"tasks/cancel","params":{"id":"working"}}`)
	if envelope.Error != nil {
		t.Fatalf("unexpected error: %s", envelope.Error)
	}
	var task Task
	_ = json.Unmarshal(envelope.Result, &task)
	if task.Status.State != TaskStateCanceled {
		t.Fatalf("state = %q, want canceled", task.Status.State)
	}
	persisted, _ := store.Get(context.Background(), "working")
	if persisted.Status.State != TaskStateCanceled {
		t.Fatalf("cancel not persisted: %q", persisted.Status.State)
	}

	envelope = postRPC(t, h, `{"jsonrpc":"2.0","id":2,"method":"tasks/cancel","params":{"id":"done"}}`)
	if envelope.Error == nil || envelope.Error.Code != codeTaskNotCancelable {
		t.Fatalf("terminal task: want code %d, got %+v", codeTaskNotCancelable, envelope.Error)
	}

	envelope = postRPC(t, h, `{"jsonrpc":"2.0","id":3,"method":"tasks/cancel","params":{"id":"absent"}}`)
	if envelope.Error == nil || envelope.Error.Code != codeTaskNotFound {
		t.Fatalf("absent task: want code %d, got %+v", codeTaskNotFound, envelope.Error)
	}
}

// decodeSSE splits an SSE body into JSON-RPC envelopes, one per data entry.
func decodeSSE(t *testing.T, body string) []rpcResponse {
	t.Helper()
	var envelopes []rpcResponse
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		line, ok := strings.CutPrefix(block, "data: ")
		if !ok {
			t.Fatalf("SSE block without data line: %q", block)
		}
		var envelope rpcResponse
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			t.Fatalf("decode SSE payload %q: %v", line, err)
		}
		envelopes = append(envelopes, envelope)
	}
	return envelopes
}

func TestServerMessageStreamSSE(t *testing.T) {
	store := NewMemoryTaskStore()
	h := NewServer(ServerConfig{Card: testCard(true), Handle: echoHandler, Store: store}).Handler()

	req := httptest.NewRequest(http.MethodPost, rpcPath, strings.NewReader(messageStreamBody("stream", `5`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	envelopes := decodeSSE(t, rec.Body.String())
	if len(envelopes) != 2 {
		t.Fatalf("got %d SSE events, want 2: %s", len(envelopes), rec.Body.String())
	}
	var updates []StatusUpdate
	for i, envelope := range envelopes {
		if envelope.Error != nil {
			t.Fatalf("event %d: error %s", i, envelope.Error)
		}
		if string(envelope.ID) != "5" {
			t.Fatalf("event %d: id = %s, want 5", i, envelope.ID)
		}
		var update StatusUpdate
		if err := json.Unmarshal(envelope.Result, &update); err != nil {
			t.Fatalf("event %d: decode StatusUpdate: %v", i, err)
		}
		if update.Kind != KindStatusUpdate {
			t.Fatalf("event %d: kind = %q", i, update.Kind)
		}
		updates = append(updates, update)
	}
	if updates[0].Status.State != TaskStateWorking || updates[0].Final {
		t.Fatalf("first event = %+v, want working/final=false", updates[0])
	}
	if updates[1].Status.State != TaskStateCompleted || !updates[1].Final {
		t.Fatalf("last event = %+v, want completed/final=true", updates[1])
	}
	if updates[1].Status.Message == nil {
		t.Fatal("completed event carries no status.message")
	}
	tp, ok := updates[1].Status.Message.Parts[0].(*TextPart)
	if !ok || tp.Text != "echo: stream" {
		t.Fatalf("completed status.message = %+v", updates[1].Status.Message)
	}
	// The streamed task is persisted.
	task, err := store.Get(context.Background(), updates[0].TaskID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if task.Status.State != TaskStateCompleted {
		t.Fatalf("streamed task state = %q", task.Status.State)
	}
}

func TestServerMessageStreamHandleError(t *testing.T) {
	h := NewServer(ServerConfig{Card: testCard(true), Handle: failingHandler}).Handler()
	req := httptest.NewRequest(http.MethodPost, rpcPath, strings.NewReader(messageStreamBody("x", `1`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	envelopes := decodeSSE(t, rec.Body.String())
	if len(envelopes) != 2 {
		t.Fatalf("got %d SSE events, want 2", len(envelopes))
	}
	var update StatusUpdate
	if err := json.Unmarshal(envelopes[1].Result, &update); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if update.Status.State != TaskStateFailed || !update.Final {
		t.Fatalf("terminal event = %+v, want failed/final=true", update)
	}
}

func TestServerMessageStreamDisabled(t *testing.T) {
	h := NewServer(ServerConfig{Card: testCard(false), Handle: echoHandler}).Handler()
	req := httptest.NewRequest(http.MethodPost, rpcPath, strings.NewReader(messageStreamBody("x", `1`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var envelope rpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("body should be a plain JSON-RPC error: %v (%s)", err, rec.Body.String())
	}
	if envelope.Error == nil || envelope.Error.Code != codeUnsupportedOperation {
		t.Fatalf("want code %d, got %+v", codeUnsupportedOperation, envelope.Error)
	}
}
