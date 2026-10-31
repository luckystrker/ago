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

// clientToolsServerCfg builds the config whose extension legs emulate
// executor.py: the tools leg parks the task input-required with one call per
// schema, the results leg completes with a reply naming what ran.
func clientToolsServerCfg(streaming bool) ServerConfig {
	return ServerConfig{
		Card: testCard(streaming),
		HandleClientTools: func(_ context.Context, msg *Message, schemas []ToolSchema) (*Message, []ToolCall, error) {
			if len(schemas) == 0 {
				return NewMessage(RoleAgent, NewTextPart("no tools")), nil, nil
			}
			var calls []ToolCall
			for _, s := range schemas {
				calls = append(calls, ToolCall{ID: "call-" + s.Name, Name: s.Name, Arguments: `{"x":1}`})
			}
			return nil, calls, nil
		},
		HandleToolResults: func(_ context.Context, msg *Message, results []ToolResultEntry) (*Message, error) {
			var names []string
			for _, r := range results {
				names = append(names, r.Name+"="+r.Content)
			}
			return NewMessage(RoleAgent, NewTextPart("ran: "+strings.Join(names, ","))), nil
		},
	}
}

// clientToolsServer builds that config into a Server.
func clientToolsServer() *Server { return NewServer(clientToolsServerCfg(false)) }

// TestClientToolsRoundTrip drives the full urn:ag2:client-tools:v1 loop over
// httptest: schemas announce the tools, the server parks input-required with
// tool-call parts, the driver executes locally and resumes, the results leg
// completes.
func TestClientToolsRoundTrip(t *testing.T) {
	ts := httptest.NewServer(clientToolsServer().Handler())
	defer ts.Close()
	client := NewClient(ts.URL, WithExtensions(ExtensionURI))

	var executed []ToolCall
	reply, err := client.RunClientTools(
		context.Background(),
		NewMessage(RoleUser, NewTextPart("use the tools")),
		[]ToolSchema{{Name: "lookup", Description: "find things", Parameters: map[string]any{"type": "object"}}},
		func(_ context.Context, calls []ToolCall) ([]ToolResultEntry, error) {
			executed = append(executed, calls...)
			out := make([]ToolResultEntry, len(calls))
			for i, c := range calls {
				out[i] = ToolResultEntry{ID: c.ID, Name: c.Name, Content: "found"}
			}
			return out, nil
		},
	)
	if err != nil {
		t.Fatalf("RunClientTools: %v", err)
	}
	if len(executed) != 1 || executed[0].Name != "lookup" {
		t.Fatalf("executed = %+v, want one lookup call", executed)
	}
	if textOfMessage(reply) != "ran: lookup=found" {
		t.Fatalf("reply = %q", textOfMessage(reply))
	}
	if executed[0].ID != "call-lookup" {
		t.Fatalf("call id = %q", executed[0].ID)
	}
}

// TestClientToolsRoundTripTaskShape checks the parked turn: message/send
// answers with the Task record, status input-required, the tool-call parts on
// status.message, and the task resumable by id.
func TestClientToolsRoundTripTaskShape(t *testing.T) {
	h := clientToolsServer().Handler()
	body, err := json.Marshal(map[string]any{
		"message": AttachClientTools(NewMessage(RoleUser, NewTextPart("go")), []ToolSchema{{Name: "calc"}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	env := postRPC(t, h, rpcBody(MethodMessageSend, string(body), `"1"`))
	if env.Error != nil {
		t.Fatalf("send: %v", env.Error)
	}
	var task Task
	if err := json.Unmarshal(env.Result, &task); err != nil {
		t.Fatalf("decode task: %v (raw %s)", err, env.Result)
	}
	if task.Status.State != TaskStateInputRequired {
		t.Fatalf("state = %q, want input-required", task.Status.State)
	}
	calls, err := ParseToolCalls(task.Status.Message)
	if err != nil {
		t.Fatalf("parse calls: %v", err)
	}
	if len(calls) != 1 || calls[0].Name != "calc" {
		t.Fatalf("calls = %+v", calls)
	}
	// The task is queryable while parked.
	resume := BuildToolResultMessage(task.ID, task.ContextID,
		[]ToolResultEntry{{ID: calls[0].ID, Name: calls[0].Name, Content: "42"}}, nil)
	resBody, err := json.Marshal(map[string]any{"message": resume})
	if err != nil {
		t.Fatal(err)
	}
	env = postRPC(t, h, rpcBody(MethodMessageSend, string(resBody), `"2"`))
	if env.Error != nil {
		t.Fatalf("resume: %v", env.Error)
	}
	var reply Message
	if err := json.Unmarshal(env.Result, &reply); err != nil {
		t.Fatalf("decode reply: %v (raw %s)", err, env.Result)
	}
	if textOfMessage(&reply) != "ran: calc=42" {
		t.Fatalf("reply = %q", textOfMessage(&reply))
	}
	if reply.TaskID != task.ID {
		t.Fatalf("reply task id = %q, want %q", reply.TaskID, task.ID)
	}
}

// TestStreamParksInputRequired checks the SSE twin: working, then final with
// the input-required state and the calls attached.
func TestStreamParksInputRequired(t *testing.T) {
	h := NewServer(clientToolsServerCfg(true)).Handler()
	req := httptest.NewRequest(http.MethodPost, rpcPath, strings.NewReader(
		rpcBody(MethodMessageStream, `{"message":`+marshalMessage(t, AttachClientTools(NewMessage(RoleUser, NewTextPart("go")), []ToolSchema{{Name: "calc"}}))+`}`, `"1"`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	events := decodeSSE(t, rec.Body.String())
	if len(events) != 2 {
		t.Fatalf("streamed %d events, want 2", len(events))
	}
	var final StatusUpdate
	if err := json.Unmarshal(events[1].Result, &final); err != nil {
		t.Fatalf("decode final: %v", err)
	}
	if !final.Final || final.Status.State != TaskStateInputRequired {
		t.Fatalf("final = %+v", final.Status)
	}
	calls, err := ParseToolCalls(final.Status.Message)
	if err != nil || len(calls) != 1 {
		t.Fatalf("final calls = %+v err %v", final.Status.Message, err)
	}
}

// TestClientToolsWithoutCallback fails the turn honestly when tool results
// arrive but no HandleToolResults is configured. The wire error is the fixed
// internal message; the explanation rides the failed task record.
func TestClientToolsWithoutCallback(t *testing.T) {
	store := &recordingStore{MemoryTaskStore: NewMemoryTaskStore()}
	srv := NewServer(ServerConfig{Card: testCard(false), Handle: echoHandler, Store: store})
	msg := BuildToolResultMessage("task-x", "ctx-x", []ToolResultEntry{{ID: "c1", Name: "calc", Content: "1"}}, nil)
	body, err := json.Marshal(map[string]any{"message": msg})
	if err != nil {
		t.Fatal(err)
	}
	env := postRPC(t, srv.Handler(), rpcBody(MethodMessageSend, string(body), `"1"`))
	if env.Error == nil {
		t.Fatalf("results without HandleToolResults succeeded, want error")
	}
	if env.Error.Code != codeInternalError || strings.Contains(env.Error.Message, "HandleToolResults") {
		t.Fatalf("wire error = %+v, want the fixed internal message", env.Error)
	}

	store.mu.Lock()
	ids := append([]string(nil), store.ids...)
	store.mu.Unlock()
	if len(ids) != 1 {
		t.Fatalf("stored ids = %v, want the one task of the failed turn", ids)
	}
	task, err := store.Get(context.Background(), ids[0])
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if task.Status.State != TaskStateFailed || task.Status.Message == nil {
		t.Fatalf("stored task = %+v, want failed with a status message", task)
	}
	if !strings.Contains(textOfMessage(task.Status.Message), "HandleToolResults") {
		t.Fatalf("stored failure text = %q, want the HandleToolResults explanation", textOfMessage(task.Status.Message))
	}
}

// TestValidateExtensionActivation covers both refusal directions of
// extension.py's card reconciliation.
func TestValidateExtensionActivation(t *testing.T) {
	card := testCard(false)
	card.Capabilities.Extensions = []AgentExtension{
		{URI: ExtensionURI},
		{URI: "urn:example:must", Required: true},
	}
	if err := ValidateExtensionActivation(&card, []string{ExtensionURI}); err == nil {
		t.Fatalf("missing required extension accepted")
	}
	var mismatch *ExtensionNotSupportedError
	if err := ValidateExtensionActivation(&card, []string{ExtensionURI, "urn:example:must"}); err != nil {
		t.Fatalf("full activation refused: %v", err)
	}
	if err := ValidateExtensionActivation(&card, []string{"urn:example:unknown"}); !errors.As(err, &mismatch) {
		t.Fatalf("unknown uri err = %v, want ExtensionNotSupportedError", err)
	}
}

// TestA2AExtensionsHeader checks the client stamps the activation header on
// every request (extension_call_context parity).
func TestA2AExtensionsHeader(t *testing.T) {
	var got string
	card := testCard(false)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(HeaderA2AExtensions)
		if strings.Contains(r.URL.Path, "well-known") {
			writeJSON(w, http.StatusOK, &card)
			return
		}
		// Minimal message/send reply.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"role":"agent","parts":[{"kind":"text","text":"ok"}]}}`))
	}))
	defer ts.Close()
	client := NewClient(ts.URL, WithExtensions(ExtensionURI, "urn:example:extra"))
	if _, err := client.SendMessage(context.Background(), NewMessage(RoleUser, NewTextPart("hi"))); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if want := ExtensionURI + ",urn:example:extra"; got != want {
		t.Fatalf("A2A-Extensions = %q, want %q", got, want)
	}
}

// TestRunWithHumanInput drives the HITL loop: the server parks
// input-required with the question as the reply text (plus the pending
// client-tool call that made it ask, Python's requires_input shape), the
// answer callback supplies the reply, the text continuation under the same
// task id reaches Handle and completes.
func TestRunWithHumanInput(t *testing.T) {
	var asked int
	srv := NewServer(ServerConfig{
		Card: testCard(false),
		HandleClientTools: func(_ context.Context, msg *Message, schemas []ToolSchema) (*Message, []ToolCall, error) {
			if asked == 0 {
				asked++
				question := NewMessage(RoleAgent, NewTextPart("what is your name?"))
				return question, []ToolCall{{ID: "c-ask", Name: schemas[0].Name}}, nil
			}
			return NewMessage(RoleAgent, NewTextPart("answered")), nil, nil
		},
		Handle: func(_ context.Context, msg *Message) (*Message, error) {
			return NewMessage(RoleAgent, NewTextPart("hello, "+textOfMessage(msg))), nil
		},
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := NewClient(ts.URL, WithExtensions(ExtensionURI))

	msg := AttachClientTools(NewMessage(RoleUser, NewTextPart("begin")), []ToolSchema{{Name: "ask_human"}})
	reply, err := client.RunWithHumanInput(context.Background(), msg, func(_ context.Context, question string) (string, error) {
		if question != "what is your name?" {
			t.Fatalf("question = %q", question)
		}
		return "Ada", nil
	})
	if err != nil {
		t.Fatalf("RunWithHumanInput: %v", err)
	}
	if textOfMessage(reply) != "hello, Ada" {
		t.Fatalf("reply = %q", textOfMessage(reply))
	}
}

// marshalMessage encodes one message for inline params.
func marshalMessage(t *testing.T, msg *Message) string {
	t.Helper()
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	return string(raw)
}
