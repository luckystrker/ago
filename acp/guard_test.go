package acp

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// --- the _meta merge guard ---------------------------------------------------

// A request whose _meta names one of the method's own fields is refused with
// invalid params, and the handler never runs: metadata must not be able to
// choose the session a prompt runs against.
func TestServeRejectsCollidingMeta(t *testing.T) {
	handled := 0
	cfg := ServeConfig{
		Handle: func(ctx context.Context, sessionID, userText string, emit *Emitter) (string, error) {
			handled++
			return "ok", nil
		},
	}
	fc := startServe(t, func(in io.Reader, out io.Writer) error { return serveIO(in, out, cfg) })
	if _, rpcErr := fc.call("initialize", map[string]any{"protocolVersion": protocolVersion}); rpcErr != nil {
		t.Fatalf("initialize: %v", rpcErr)
	}
	res, rpcErr := fc.call("session/new", map[string]any{"cwd": t.TempDir()})
	if rpcErr != nil {
		t.Fatalf("session/new: %v", rpcErr)
	}
	var created struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(res, &created); err != nil {
		t.Fatal(err)
	}

	// sessionId in _meta would displace the validated session id.
	_, rpcErr = fc.call("session/prompt", map[string]any{
		"sessionId": created.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "hi"}},
		"_meta":     map[string]any{"sessionId": "someone-elses-session"},
	})
	if rpcErr == nil || rpcErr.Code != codeInvalidParams {
		t.Fatalf("colliding _meta = %v, want code %d", rpcErr, codeInvalidParams)
	}
	if !strings.Contains(rpcErr.Message, "sessionId") {
		t.Fatalf("error must name the colliding field: %q", rpcErr.Message)
	}
	if handled != 0 {
		t.Fatalf("the handler ran %d times for a refused request", handled)
	}

	// Harmless metadata passes and the turn runs.
	if _, rpcErr := fc.call("session/prompt", map[string]any{
		"sessionId": created.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "hi"}},
		"_meta":     map[string]any{"provenance": "space://room/42"},
	}); rpcErr != nil {
		t.Fatalf("harmless _meta: %v", rpcErr)
	}
	if handled != 1 {
		t.Fatalf("handler ran %d times, want 1", handled)
	}
}

// A colliding _meta on a notification drops it: session/cancel must not stop a
// turn through a metadata field.
func TestServeDropsCollidingMetaNotification(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	cfg := ServeConfig{
		Handle: func(ctx context.Context, sessionID, userText string, emit *Emitter) (string, error) {
			started <- struct{}{}
			select {
			case <-release:
				return "finished", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
	}
	fc := startServe(t, func(in io.Reader, out io.Writer) error { return serveIO(in, out, cfg) })
	if _, rpcErr := fc.call("initialize", map[string]any{"protocolVersion": protocolVersion}); rpcErr != nil {
		t.Fatalf("initialize: %v", rpcErr)
	}
	res, rpcErr := fc.call("session/new", map[string]any{"cwd": t.TempDir()})
	if rpcErr != nil {
		t.Fatalf("session/new: %v", rpcErr)
	}
	var created struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(res, &created); err != nil {
		t.Fatal(err)
	}

	fc.id++
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": fc.id, "method": "session/prompt",
		"params": map[string]any{
			"sessionId": created.SessionID,
			"prompt":    []map[string]any{{"type": "text", "text": "long task"}},
		},
	})
	fc.writeMu.Lock()
	_, _ = fc.w.WriteString(string(payload) + "\n")
	_ = fc.w.Flush()
	fc.writeMu.Unlock()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never started")
	}

	// A colliding cancel notification is dropped: the turn keeps running.
	notify, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": "session/cancel",
		"params": map[string]any{"sessionId": created.SessionID, "_meta": map[string]any{"sessionId": "other"}},
	})
	fc.writeMu.Lock()
	_, _ = fc.w.WriteString(string(notify) + "\n")
	_ = fc.w.Flush()
	fc.writeMu.Unlock()

	// A well-formed cancel still works, which proves the dropped one was the
	// reason the turn survived.
	time.Sleep(50 * time.Millisecond)
	notify, _ = json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": "session/cancel",
		"params": map[string]any{"sessionId": created.SessionID},
	})
	fc.writeMu.Lock()
	_, _ = fc.w.WriteString(string(notify) + "\n")
	_ = fc.w.Flush()
	fc.writeMu.Unlock()

	_, rpcErr = fc.await(fc.id)
	if rpcErr != nil {
		t.Fatalf("prompt reply: %v", rpcErr)
	}
}

func TestCollidingMetaKeys(t *testing.T) {
	params := json.RawMessage(`{"sessionId":"s","_meta":{"sessionId":"x","prompt":"p","other":1}}`)
	got := collidingMetaKeys("session/prompt", params)
	if len(got) != 2 || got[0] != "prompt" || got[1] != "sessionId" {
		t.Fatalf("collisions = %v, want [prompt sessionId] (sorted)", got)
	}
	// An unknown method, absent metadata and non-object params collide with
	// nothing.
	if got := collidingMetaKeys("session/unknown", params); got != nil {
		t.Fatalf("unknown method = %v", got)
	}
	if got := collidingMetaKeys("session/prompt", json.RawMessage(`{"sessionId":"s"}`)); got != nil {
		t.Fatalf("no _meta = %v", got)
	}
	if got := collidingMetaKeys("session/prompt", json.RawMessage(`"_meta"`)); got != nil {
		t.Fatalf("non-object params = %v", got)
	}
	// Every declared method has its reserved names: a typo in the table would
	// silently disable the guard for that method.
	for method := range reservedMetaNames {
		probe := map[string]any{}
		for _, name := range reservedMetaNames[method] {
			probe[name] = true
		}
		raw, err := json.Marshal(map[string]any{"_meta": probe})
		if err != nil {
			t.Fatal(err)
		}
		if len(collidingMetaKeys(method, raw)) != len(reservedMetaNames[method]) {
			t.Errorf("%s: not every reserved name collides", method)
		}
	}
}

// --- auth method kinds -------------------------------------------------------

// The three declared kinds travel on the wire with their own fields, and an
// agent-kind method keeps the pre-existing shape (no type, no vars, no args).
func TestAuthMethodKindsOnTheWire(t *testing.T) {
	auth := &DeclaredAuth{
		Advertised: []AuthMethod{
			NewAgentAuthMethod("token", "Shared token", "one secret"),
			NewEnvVarAuthMethod("env", "API key in the environment", "", AuthEnvVar{Name: "OPENAI_API_KEY", Secret: true}),
			NewTerminalAuthMethod("login", "Log in with the CLI", "", "auth", "login"),
		},
		Validate: func(methodID string, meta map[string]any) error { return nil },
	}
	cfg := ServeConfig{Auth: auth, Handle: func(context.Context, string, string, *Emitter) (string, error) {
		return "ok", nil
	}}
	fc := startServe(t, func(in io.Reader, out io.Writer) error { return serveIO(in, out, cfg) })

	res, rpcErr := fc.call("initialize", map[string]any{"protocolVersion": protocolVersion})
	if rpcErr != nil {
		t.Fatalf("initialize: %v", rpcErr)
	}
	var init struct {
		AuthMethods []map[string]any `json:"authMethods"`
	}
	if err := json.Unmarshal(res, &init); err != nil {
		t.Fatal(err)
	}
	if len(init.AuthMethods) != 3 {
		t.Fatalf("authMethods = %+v", init.AuthMethods)
	}
	if init.AuthMethods[0]["type"] != AuthKindAgent {
		t.Fatalf("agent method type = %v", init.AuthMethods[0]["type"])
	}
	if _, has := init.AuthMethods[0]["vars"]; has {
		t.Fatal("an agent method carries no vars")
	}
	if init.AuthMethods[1]["type"] != AuthKindEnvVar {
		t.Fatalf("env-var method type = %v", init.AuthMethods[1]["type"])
	}
	vars, _ := init.AuthMethods[1]["vars"].([]any)
	if len(vars) != 1 {
		t.Fatalf("env-var vars = %v", init.AuthMethods[1]["vars"])
	}
	if v := vars[0].(map[string]any); v["name"] != "OPENAI_API_KEY" || v["secret"] != true {
		t.Fatalf("env var = %v", v)
	}
	if init.AuthMethods[2]["type"] != AuthKindTerminal {
		t.Fatalf("terminal method type = %v", init.AuthMethods[2]["type"])
	}
	args, _ := init.AuthMethods[2]["args"].([]any)
	if len(args) != 2 || args[0] != "auth" || args[1] != "login" {
		t.Fatalf("terminal args = %v", init.AuthMethods[2]["args"])
	}

	// The declared kinds are advertised; the admission decision is still the
	// provider's.
	if _, rpcErr := fc.call("authenticate", map[string]any{"methodId": "env"}); rpcErr != nil {
		t.Fatalf("authenticate through DeclaredAuth: %v", rpcErr)
	}
	refusing := &DeclaredAuth{Advertised: auth.Advertised}
	if err := refusing.Authenticate("token", nil); err == nil {
		t.Fatal("a DeclaredAuth with no Validate refuses every method")
	}
}

// The shared-token provider advertises the agent kind, which is what it
// validates.
func TestStaticTokenAuthAdvertisesAgentKind(t *testing.T) {
	auth, err := NewStaticTokenAuth("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	methods := auth.Methods()
	if len(methods) != 1 || methods[0].Kind() != AuthKindAgent {
		t.Fatalf("methods = %+v", methods)
	}
	if methods[0].Vars != nil || methods[0].Args != nil {
		t.Fatalf("a token method carries neither vars nor args: %+v", methods[0])
	}
}
