package a2ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"testing"
)

func TestActionRegistry(t *testing.T) {
	r := NewActionRegistry()
	if _, ok := r.Get("nope"); ok {
		t.Error("empty registry must not find actions")
	}
	r.Register(Action{Name: "b", Fn: func(context.Context, map[string]any) (any, error) { return nil, nil }})
	r.Register(Action{Name: "a", Description: "first", Fn: func(context.Context, map[string]any) (any, error) { return nil, nil }})
	if _, ok := r.Get("b"); !ok {
		t.Error("registered action not found")
	}
	if names := strings.Join(r.Names(), ","); names != "a,b" {
		t.Errorf("Names: want a,b, got %s", names)
	}
	if acts := r.Actions(); len(acts) != 2 || acts[0].Name != "a" {
		t.Errorf("Actions: got %+v", acts)
	}
	r.Register(Action{Name: "a", Description: "replaced",
		Fn: func(context.Context, map[string]any) (any, error) { return nil, nil }})
	if a, _ := r.Get("a"); a.Description != "replaced" {
		t.Errorf("Register must replace: got %+v", a)
	}

	// A nil registry registers nothing and must not panic.
	var nilRegistry *ActionRegistry
	if _, ok := nilRegistry.Get("x"); ok {
		t.Error("nil registry must not find actions")
	}
}

func TestMessagesOf(t *testing.T) {
	version := VersionV1
	msg := Message{DeleteSurface: &DeleteSurface{SurfaceID: "s1"}}

	// Typed message → stamped.
	got := MessagesOf(msg, version)
	if len(got) != 1 || got[0].Version != version {
		t.Errorf("typed message: %+v", got)
	}
	// Typed message with an explicit version is left alone.
	stamped := Message{Version: "custom", DeleteSurface: &DeleteSurface{SurfaceID: "s1"}}
	if got = MessagesOf(stamped, version); got[0].Version != "custom" {
		t.Errorf("explicit version clobbered: %+v", got[0])
	}
	// Typed list.
	if got = MessagesOf([]Message{msg, msg}, version); len(got) != 2 {
		t.Errorf("typed list: %+v", got)
	}
	if got = MessagesOf([]Message{}, version); got != nil {
		t.Errorf("empty typed list: %+v", got)
	}

	// Decoded map form (what a JSON handler return looks like).
	var m map[string]any
	if err := json.Unmarshal([]byte(`{"deleteSurface": {"surfaceId": "s1"}}`), &m); err != nil {
		t.Fatal(err)
	}
	got = MessagesOf(m, version)
	if len(got) != 1 || got[0].Version != version || got[0].DeleteSurface == nil {
		t.Errorf("map form: %+v", got)
	}
	raw, err := got[0].MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, []byte(`{"version":"v1.0","deleteSurface":{"surfaceId":"s1"}}`)) {
		t.Errorf("stamped wire form: %s", raw)
	}

	// A list of messages (all items must be messages).
	var list []any
	if err := json.Unmarshal([]byte(`[{"deleteSurface": {"surfaceId": "s1"}}, {"updateDataModel": {"surfaceId": "s", "value": 1}}]`), &list); err != nil {
		t.Fatal(err)
	}
	if got = MessagesOf(list, version); len(got) != 2 {
		t.Errorf("list form: %+v", got)
	}

	// A plain value has no response channel.
	if got = MessagesOf(42, version); got != nil {
		t.Errorf("plain value: %+v", got)
	}
	if got = MessagesOf(nil, version); got != nil {
		t.Errorf("nil: %+v", got)
	}
	// A list with a non-message item is rejected wholesale.
	if got = MessagesOf([]any{m, 42}, version); got != nil {
		t.Errorf("mixed list: %+v", got)
	}
}

func TestServerRegisteredActionClick(t *testing.T) {
	registry := NewActionRegistry()
	registry.Register(Action{
		Name:        "addNote",
		Description: "Add a note",
		Fn: func(_ context.Context, params map[string]any) (any, error) {
			if params["text"] != "hi" {
				return nil, errors.New("unexpected params")
			}
			return map[string]any{
				"updateDataModel": map[string]any{"surfaceId": "s1", "path": "/notes", "value": 1},
			}, nil
		},
	})
	srv := NewServer(WithActions(registry), WithEncoding(EncodingJSONL))

	body := `{"messages": [], "a2ui": [{"version": "v1.0", "action": {"name": "addNote",
		"surfaceId": "s1", "sourceComponentId": "btn1", "timestamp": "2026-10-30T00:00:00Z",
		"context": {"text": "hi"}}}]}`
	resp := serve(t, srv, body)
	if resp.code != http.StatusOK {
		t.Fatalf("status: %d (%s)", resp.code, resp.body)
	}
	want := `{"version":"v1.0","updateDataModel":{"surfaceId":"s1","path":"/notes","value":1}}` + "\n"
	if resp.body != want {
		t.Errorf("click result:\n  want %q\n  got  %q", want, resp.body)
	}
	if ct := resp.header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("content type: %s", ct)
	}
}

func TestServerUnregisteredClickBecomesPrompt(t *testing.T) {
	var gotPrompt string
	srv := NewServer(WithEncoding(EncodingJSONL), WithPromptHandler(
		func(_ context.Context, prompt string) (Turn, error) {
			gotPrompt = prompt
			return Turn{Text: "Adding it.", Messages: []Message{
				{Version: VersionV1, UpdateDataModel: &UpdateDataModel{SurfaceID: "s1", Value: 1}}},
			}, nil
		}))

	body := `{"a2ui": [{"version": "v1.0", "action": {"name": "refresh",
		"surfaceId": "s1", "sourceComponentId": "btn1", "timestamp": "",
		"context": {"good_id": "g-1"}}}]}`
	resp := serve(t, srv, body)
	if resp.code != http.StatusOK {
		t.Fatalf("status: %d (%s)", resp.code, resp.body)
	}
	wantPrompt := "The user clicked the 'refresh' button (surface=s1, component=btn1). " +
		`Context: {"good_id": "g-1"}`
	if gotPrompt != wantPrompt {
		t.Errorf("prompt:\n  want %q\n  got  %q", wantPrompt, gotPrompt)
	}
	lines := strings.Split(strings.TrimRight(resp.body, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("frames: %q", resp.body)
	}
	if lines[0] != `{"text": "Adding it."}` {
		t.Errorf("prose frame: %q", lines[0])
	}
	if lines[1] != `{"version":"v1.0","updateDataModel":{"surfaceId":"s1","value":1}}` {
		t.Errorf("message frame: %q", lines[1])
	}
}

func TestServerCallAgentFunction(t *testing.T) {
	registry := NewActionRegistry()
	registry.Register(Action{Name: "getItemCount",
		Fn: func(_ context.Context, _ map[string]any) (any, error) { return 5, nil }})
	failing := NewActionRegistry()
	failing.Register(Action{Name: "boom",
		Fn: func(_ context.Context, _ map[string]any) (any, error) { return nil, errors.New("nope") }})

	call := func(name, catalog string) string {
		catalogJSON := "null"
		if catalog == "" {
			catalogJSON = ""
		} else if catalog != "null" {
			catalogJSON = `"` + catalog + `"`
		}
		parts := `{"@call": "` + name + `"`
		if catalogJSON != "" {
			parts += `, "catalogId": ` + catalogJSON
		}
		parts += `, "args": {}}`
		return `{"a2ui": [{"version": "v1.0", "callAgentFunction": {"surfaceId": "s1",
			"functionCallId": "fc-7", "callFunction": ` + parts + `}}]}`
	}

	// Registered + default catalog → agentFunctionResponse echoing the id.
	srv := NewServer(WithActions(registry), WithEncoding(EncodingJSONL))
	resp := serve(t, srv, call("getItemCount", "https://a2ui.org/specification/v1_0/catalogs/basic/catalog.json"))
	want := `{"version":"v1.0","agentFunctionResponse":{"functionCallId":"fc-7","value":5}}` + "\n"
	if resp.body != want {
		t.Errorf("success response:\n  want %q\n  got  %q", want, resp.body)
	}

	// Unknown function (catalog omitted) → UNKNOWN_FUNCTION.
	resp = serve(t, srv, call("nope", ""))
	want = `{"version":"v1.0","agentFunctionResponse":{"functionCallId":"fc-7","error":{"code":"UNKNOWN_FUNCTION","message":"Unknown function 'nope'."}}}` + "\n"
	if resp.body != want {
		t.Errorf("unknown function:\n  want %q\n  got  %q", want, resp.body)
	}

	// A catalog override that does not match must not select the handler.
	resp = serve(t, srv, call("getItemCount", "https://example.com/other.json"))
	if !strings.Contains(resp.body, "UNKNOWN_FUNCTION") {
		t.Errorf("catalog mismatch must answer UNKNOWN_FUNCTION: %q", resp.body)
	}

	// A malformed catalogId (present but not a string — JSON null included)
	// is INVALID_FUNCTION_CALL, checked before the handler lookup.
	resp = serve(t, srv, call("nope", "null"))
	if !strings.Contains(resp.body, "INVALID_FUNCTION_CALL") ||
		!strings.Contains(resp.body, "Invalid catalogId for function 'nope'.") {
		t.Errorf("malformed catalogId: %q", resp.body)
	}

	// Handler failure → FUNCTION_FAILED (the turn never tears down).
	fsrv := NewServer(WithActions(failing), WithEncoding(EncodingJSONL))
	resp = serve(t, fsrv, call("boom", ""))
	if !strings.Contains(resp.body, `"code":"FUNCTION_FAILED"`) {
		t.Errorf("handler failure: %q", resp.body)
	}

	// Non-object args are INVALID_FUNCTION_CALL without running the handler.
	nonObject := `{"a2ui": [{"version": "v1.0", "callAgentFunction": {"surfaceId": "s1",
		"functionCallId": "fc-8", "callFunction": {"@call": "getItemCount", "args": [1]}}}]}`
	resp = serve(t, srv, nonObject)
	if !strings.Contains(resp.body, "INVALID_FUNCTION_CALL") ||
		!strings.Contains(resp.body, "Invalid arguments for function 'getItemCount'.") {
		t.Errorf("non-object args: %q", resp.body)
	}
}

func TestServerCallAgentFunctionReturnsMessages(t *testing.T) {
	// A handler that returns surface updates emits them ahead of the response,
	// with no separate value (server_action.py run_agent_function).
	registry := NewActionRegistry()
	registry.Register(Action{Name: "add",
		Fn: func(_ context.Context, _ map[string]any) (any, error) {
			return []any{
				map[string]any{"updateDataModel": map[string]any{"surfaceId": "cart", "path": "/n", "value": 2}},
			}, nil
		}})
	srv := NewServer(WithActions(registry), WithEncoding(EncodingJSONL))
	body := `{"a2ui": [{"version": "v1.0", "callAgentFunction": {"surfaceId": "cart",
		"functionCallId": "fc-9", "callFunction": {"@call": "add", "args": {}}}}]}`
	resp := serve(t, srv, body)
	lines := strings.Split(strings.TrimRight(resp.body, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("frames: %q", resp.body)
	}
	if lines[0] != `{"version":"v1.0","updateDataModel":{"surfaceId":"cart","path":"/n","value":2}}` {
		t.Errorf("update frame: %q", lines[0])
	}
	if lines[1] != `{"version":"v1.0","agentFunctionResponse":{"functionCallId":"fc-9","value":null}}` {
		t.Errorf("response frame: %q", lines[1])
	}
}

func TestServerErrorAndFunctionResponseBecomePrompts(t *testing.T) {
	var got []string
	srv := NewServer(WithEncoding(EncodingJSONL), WithFunctionNameResolver(
		func(id string) (string, bool) { return "openUrl", id == "fc-1" }),
		WithPromptHandler(func(_ context.Context, prompt string) (Turn, error) {
			got = append(got, prompt)
			return Turn{}, nil
		}))

	body := `{"a2ui": [
		{"version": "v1.0", "error": {"code": "VALIDATION_FAILED", "surfaceId": "s1",
			"path": "/components/0/text", "message": "text is required"}},
		{"version": "v1.0", "rendererFunctionResponse": {"functionCallId": "fc-1", "value": "done"}}
	]}`
	resp := serve(t, srv, body)
	if resp.code != http.StatusOK || resp.body != "" {
		t.Fatalf("response: %d %q", resp.code, resp.body)
	}
	// The rewritten prompts ride one turn: user-facing input joined in order.
	if len(got) != 1 {
		t.Fatalf("prompts: %v", got)
	}
	if !strings.HasPrefix(got[0], "The client reported an A2UI error on surface 's1'.") ||
		!strings.Contains(got[0], "Please regenerate the UI with this issue corrected.\n") {
		t.Errorf("error prompt: %q", got[0])
	}
	want := "The client executed the function call (id=fc-1, function=openUrl) and returned: \"done\". " +
		"Use this result to continue."
	if !strings.HasSuffix(got[0], want) {
		t.Errorf("function response prompt:\n  want suffix %q\n  got  %q", want, got[0])
	}
}

func TestServerOnlyHandledInteractionsSkipThePrompt(t *testing.T) {
	// A turn whose only content is a registered click is complete without the
	// agent (dispatch.py) — the prompt handler must not run.
	called := false
	registry := NewActionRegistry()
	registry.Register(Action{Name: "ping", Fn: func(context.Context, map[string]any) (any, error) { return nil, nil }})
	srv := NewServer(WithActions(registry), WithEncoding(EncodingJSONL),
		WithPromptHandler(func(context.Context, string) (Turn, error) {
			called = true
			return Turn{}, nil
		}))
	resp := serve(t, srv, `{"a2ui": [{"version": "v1.0", "action": {"name": "ping",
		"surfaceId": "s", "sourceComponentId": "c", "timestamp": "", "context": {}}}]}`)
	if resp.code != http.StatusOK {
		t.Fatalf("status: %d", resp.code)
	}
	if called {
		t.Error("prompt handler must not run for a server-handled turn")
	}
}

func TestServerUserMessageGoesToPrompt(t *testing.T) {
	var got string
	srv := NewServer(WithEncoding(EncodingJSONL), WithPromptHandler(
		func(_ context.Context, prompt string) (Turn, error) {
			got = prompt
			return Turn{Text: "hello"}, nil
		}))
	resp := serve(t, srv, `{"messages": [
		{"role": "system", "content": "be brief"},
		{"role": "user", "content": "first"},
		{"role": "user", "content": "second"}]}`)
	if resp.code != http.StatusOK {
		t.Fatalf("status: %d (%s)", resp.code, resp.body)
	}
	if got != "first\nsecond" {
		t.Errorf("prompt: %q", got)
	}
	if resp.body != `{"text": "hello"}`+"\n" {
		t.Errorf("prose-only response: %q", resp.body)
	}
}

func TestServerRequestErrors(t *testing.T) {
	srv := NewServer(WithEncoding(EncodingJSONL))
	cases := []struct{ name, body, err string }{
		{"bad json", `{`, "request body is not valid JSON"},
		{"not an object", `[]`, "request body must be a JSON object"},
		{"messages not a list", `{"messages": "x"}`, "'messages' must be a list"},
		{"message not an object", `{"messages": [1]}`, "each message must be an object"},
		{"bad role", `{"messages": [{"role": "tool", "content": "x"}]}`, "unsupported message role: 'tool'"},
		{"variables not an object", `{"variables": 3}`, "'variables' must be an object"},
		{"a2ui not a list", `{"a2ui": 1}`, "'a2ui' must be a list"},
	}
	for _, tc := range cases {
		resp := serve(t, srv, tc.body)
		if resp.code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%s)", tc.name, resp.code, resp.body)
			continue
		}
		if !strings.Contains(resp.body, tc.err) {
			t.Errorf("%s: body %q does not contain %q", tc.name, resp.body, tc.err)
		}
	}

	// GET is refused.
	req, err := http.NewRequest(http.MethodGet, "/a2ui", nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{header: make(http.Header)}
	srv.ServeHTTP(rec, req)
	if rec.code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status %d, want 405", rec.code)
	}
}

func TestServerSSEEncoding(t *testing.T) {
	srv := NewServer(WithPromptHandler(func(context.Context, string) (Turn, error) {
		return Turn{Text: "你好", Messages: []Message{
			{Version: VersionV1, DeleteSurface: &DeleteSurface{SurfaceID: "s1"}},
		}}, nil
	}))
	resp := serve(t, srv, `{"messages": [{"role": "user", "content": "x"}]}`)
	want := "event: text\ndata: {\"text\": \"\\u4f60\\u597d\"}\n\n" +
		"data: {\"version\":\"v1.0\",\"deleteSurface\":{\"surfaceId\":\"s1\"}}\n\n" +
		"event: done\ndata: {}\n\n"
	if resp.body != want {
		t.Errorf("SSE:\n  want %q\n  got  %q", want, resp.body)
	}
	if ct := resp.header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content type: %s", ct)
	}
}

func TestServerPromptHandlerFailureBecomesErrorFrame(t *testing.T) {
	srv := NewServer(WithEncoding(EncodingJSONL), WithPromptHandler(
		func(context.Context, string) (Turn, error) { return Turn{}, errors.New("model down") }))
	resp := serve(t, srv, `{"messages": [{"role": "user", "content": "x"}]}`)
	if resp.code != http.StatusOK {
		t.Fatalf("status: %d — the 200 must be committed before the failure", resp.code)
	}
	if !strings.Contains(resp.body, `"error": "model down"`) {
		t.Errorf("error frame: %q", resp.body)
	}
}

func TestServerLogging(t *testing.T) {
	var buf bytes.Buffer
	srv := NewServer(WithEncoding(EncodingJSONL), WithLogger(log.New(&buf, "", 0)))
	// A nameless action is dropped with a warning.
	serve(t, srv, `{"a2ui": [{"version": "v1.0", "action": {"name": "", "surfaceId": "s",
		"sourceComponentId": "c", "timestamp": "", "context": {}}}]} ,`)
	serve(t, srv, `{"a2ui": [{"version": "v1.0", "action": {"name": "", "surfaceId": "s",
		"sourceComponentId": "c", "timestamp": "", "context": {}}}]}`)
	if !strings.Contains(buf.String(), "Dropping A2UI action with no name.") {
		t.Errorf("log: %q", buf.String())
	}
}
