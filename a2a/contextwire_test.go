package a2a

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// TestExtractContextUpdate covers the extract contract: absent metadata, a
// non-object payload and a real delta (mappers/messages.py
// extract_context_update — callers merge the empty map unconditionally).
func TestExtractContextUpdate(t *testing.T) {
	if got := ExtractContextUpdate(nil); len(got) != 0 {
		t.Fatalf("nil message → %v", got)
	}
	msg := NewMessage(RoleUser)
	if got := ExtractContextUpdate(msg); len(got) != 0 {
		t.Fatalf("no metadata → %v", got)
	}
	msg.Metadata = map[string]any{"other": "value", ContextUpdateKey: "not-an-object"}
	if got := ExtractContextUpdate(msg); len(got) != 0 {
		t.Fatalf("non-object payload → %v", got)
	}
	msg.Metadata[ContextUpdateKey] = map[string]any{"user_name": "Ada"}
	got := ExtractContextUpdate(msg)
	if got["user_name"] != "Ada" {
		t.Fatalf("delta = %v", got)
	}
}

// TestStripReservedVariables drops the reserved prefixes from both wire
// directions (context.py strip_reserved_variables / RESERVED_VARIABLE_PREFIXES).
func TestStripReservedVariables(t *testing.T) {
	payload := map[string]any{
		"user_name":  "Ada",
		"ag:turn":    3,
		"a2a:tenant": "acme",
	}
	kept := StripReservedVariables(payload)
	if len(kept) != 1 || kept["user_name"] != "Ada" {
		t.Fatalf("kept = %v", kept)
	}
	if len(payload) != 3 {
		t.Fatalf("StripReservedVariables mutated the input map")
	}
	if got := StripReservedVariables(nil); got == nil || len(got) != 0 {
		t.Fatalf("nil payload → %v", got)
	}
}

// TestWithContextUpdateRoundTrip marshals a message with a delta, decodes it
// back, and gets the same delta — the wire shape survives the round trip.
func TestWithContextUpdateRoundTrip(t *testing.T) {
	msg := WithContextUpdate(NewMessage(RoleUser, NewTextPart("hi")), map[string]any{"k": "v"})
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var back Message
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := ExtractContextUpdate(&back)
	if got["k"] != "v" {
		t.Fatalf("round-tripped delta = %v", got)
	}
}

// TestServerContextSync drives the server-side sync through message/send:
// the inbound delta (reserved keys pre-stripped) reaches SyncVariables, the
// returned final set rides the reply metadata (stripped on the way out).
func TestServerContextSync(t *testing.T) {
	var gotIncoming map[string]any
	var gotTaskID string
	srv := NewServer(ServerConfig{
		Card:   testCard(false),
		Handle: echoHandler,
		SyncVariables: func(_ context.Context, taskID string, incoming map[string]any) (map[string]any, error) {
			gotTaskID = taskID
			gotIncoming = incoming
			return map[string]any{"user_name": "Ada", "ag:internal": "x", "turn": 2}, nil
		},
	})
	msg := WithContextUpdate(NewMessage(RoleUser, NewTextPart("hello")), map[string]any{
		"user_name": "Ada",
		"ag:turn":   1,
	})
	body, err := json.Marshal(map[string]any{"message": msg})
	if err != nil {
		t.Fatal(err)
	}
	env := postRPC(t, srv.Handler(), rpcBody(MethodMessageSend, string(body), `"1"`))
	if env.Error != nil {
		t.Fatalf("send: %v", env.Error)
	}
	if gotTaskID == "" {
		t.Fatalf("SyncVariables got an empty task id")
	}
	if _, reserved := gotIncoming["ag:turn"]; reserved {
		t.Fatalf("inbound delta kept reserved keys: %v", gotIncoming)
	}
	if gotIncoming["user_name"] != "Ada" {
		t.Fatalf("inbound delta = %v", gotIncoming)
	}
	var reply Message
	if err := json.Unmarshal(env.Result, &reply); err != nil {
		t.Fatal(err)
	}
	outgoing := ExtractContextUpdate(&reply)
	if outgoing["user_name"] != "Ada" {
		t.Fatalf("outgoing delta = %v", outgoing)
	}
	if _, reserved := outgoing["ag:internal"]; reserved {
		t.Fatalf("outgoing delta kept reserved keys: %v", outgoing)
	}
	if outgoing["turn"] != float64(2) {
		t.Fatalf("outgoing turn = %v", outgoing["turn"])
	}
}

// TestServerContextSyncDisabled keeps a server without the callback honest:
// an inbound delta is ignored, the reply carries no delta.
func TestServerContextSyncDisabled(t *testing.T) {
	srv := NewServer(ServerConfig{Card: testCard(false), Handle: echoHandler})
	msg := WithContextUpdate(NewMessage(RoleUser, NewTextPart("hello")), map[string]any{"k": "v"})
	body, _ := json.Marshal(map[string]any{"message": msg})
	env := postRPC(t, srv.Handler(), rpcBody(MethodMessageSend, string(body), `"1"`))
	if env.Error != nil {
		t.Fatalf("send: %v", env.Error)
	}
	var reply Message
	if err := json.Unmarshal(env.Result, &reply); err != nil {
		t.Fatal(err)
	}
	if got := ExtractContextUpdate(&reply); len(got) != 0 {
		t.Fatalf("reply carried a delta without SyncVariables: %v", got)
	}
}

// TestServerContextSyncFailure fails the turn when the sync callback does,
// and the task record lands failed.
func TestServerContextSyncFailure(t *testing.T) {
	var syncTaskID string
	srv := NewServer(ServerConfig{
		Card:   testCard(false),
		Handle: echoHandler,
		SyncVariables: func(_ context.Context, taskID string, _ map[string]any) (map[string]any, error) {
			syncTaskID = taskID
			return nil, errHandlerBoom
		},
	})
	msg := WithContextUpdate(NewMessage(RoleUser, NewTextPart("hello")), map[string]any{"k": "v"})
	body, _ := json.Marshal(map[string]any{"message": msg})
	env := postRPC(t, srv.Handler(), rpcBody(MethodMessageSend, string(body), `"1"`))
	if env.Error == nil {
		t.Fatalf("sync failure succeeded, want internal error")
	}
	task, err := srv.store.Get(context.Background(), syncTaskID)
	if err != nil {
		t.Fatalf("task not stored: %v", err)
	}
	if task.Status.State != TaskStateFailed {
		t.Fatalf("sync-failed turn recorded state %q", task.Status.State)
	}
}

// TestClientContextSyncRoundTrip runs the client side: a delta rides the
// opening message; the reply's delta is readable via ExtractContextUpdate.
func TestClientContextSyncRoundTrip(t *testing.T) {
	srv := NewServer(ServerConfig{
		Card:   testCard(false),
		Handle: echoHandler,
		SyncVariables: func(_ context.Context, _ string, incoming map[string]any) (map[string]any, error) {
			return map[string]any{"echo_of": incoming["user_name"]}, nil
		},
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := NewClient(ts.URL)

	msg := WithContextUpdate(NewMessage(RoleUser, NewTextPart("hi")), map[string]any{"user_name": "Ada"})
	reply, err := client.SendMessage(context.Background(), msg)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	got := ExtractContextUpdate(reply)
	if got["echo_of"] != "Ada" {
		t.Fatalf("client saw delta %v", got)
	}
}
