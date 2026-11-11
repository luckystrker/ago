package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agocontext "github.com/luckystrker/ago/context"
)

// The strict tests hold the server to the rules of the mcp SDK's own server
// (mcp 2.3.0, the SDK AG2's MCPServer runs on): every response is checked against
// the SDK's models — required fields present, no unknown fields — and every
// transport refusal against the status and code the SDK answers. A lenient
// check would prove only that the server agrees with itself.

func statefulStreaming(store EventStore) TransportConfig {
	return TransportConfig{Stateful: true, StreamResponses: true, EventStore: store}
}

// fullServer is a server with every surface registered: tools, resources,
// templates and prompts, on the given transport.
func fullServer(t *testing.T, tr TransportConfig, mutate func(*ServerConfig)) (*Server, *httptest.Server) {
	t.Helper()
	cfg := ServerConfig{
		Name: "strict", Version: "9.9.9", Instructions: "Be strict.",
		Ask:       func(_ context.Context, text string) (string, error) { return "reply: " + text, nil },
		Tools:     mustTestTools(),
		Transport: tr,
		Resources: []Resource{
			{URI: "mem://notes", Name: "notes", Title: "Notes", Description: "Scratch notes", MIMEType: "text/markdown",
				Meta: map[string]any{"owner": "ops"},
				Read: func(context.Context, *agocontext.Context) (ResourceContent, error) {
					return ResourceContent{Text: "# notes"}, nil
				}},
			{URI: "mem://blob", Name: "blob",
				Read: func(context.Context, *agocontext.Context) (ResourceContent, error) {
					return ResourceContent{Blob: []byte{0, 1, 2}}, nil
				}},
			{URI: "mem://hidden", Name: "hidden", Unlisted: true,
				Read: func(context.Context, *agocontext.Context) (ResourceContent, error) {
					return ResourceContent{Text: "psst", MIMEType: "text/x-secret"}, nil
				}},
		},
		ResourceTemplates: []ResourceTemplate{
			{URITemplate: "weather://{city}", Name: "weather", Description: "Weather", MIMEType: "text/plain",
				Read: func(_ context.Context, v map[string]string) (ResourceContent, error) {
					return ResourceContent{Text: "sunny in " + v["city"]}, nil
				}},
			{URITemplate: "files:///{+path}", Name: "files",
				Read: func(_ context.Context, v map[string]string) (ResourceContent, error) {
					return ResourceContent{Text: "file " + v["path"]}, nil
				}},
		},
		Prompts: []Prompt{
			{Name: "greet", Description: "Greets", Arguments: []PromptArgument{{Name: "who", Description: "Whom", Required: true}, {Name: "tone"}},
				Render: func(_ context.Context, a map[string]string) ([]PromptMessage, error) {
					return []PromptMessage{{Role: "user", Text: "Hello " + a["who"]}, {Role: "assistant", Text: "tone=" + a["tone"]}}, nil
				}},
			{Name: "plain", Render: func(context.Context, map[string]string) ([]PromptMessage, error) { return UserPrompt("just text"), nil }},
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() { s.Close(); ts.Close() })
	return s, ts
}

func call(t *testing.T, url, sid, method string, id int, params string) (any, map[string]any) {
	t.Helper()
	if params == "" {
		params = "{}"
	}
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, id, method, params)
	var hdr map[string]string
	if sid != "" {
		hdr = map[string]string{"Mcp-Session-Id": sid}
	}
	msg, _ := rpcCall(t, url, hdr, body)
	return checkResponseEnvelope(t, msg)
}

func TestStrictResponsesMatchTheSDKModels(t *testing.T) {
	for _, mode := range []struct {
		name string
		tr   TransportConfig
	}{
		{"stateful sse", statefulStreaming(nil)},
		{"stateful json", TransportConfig{Stateful: true}},
		{"stateless json", TransportConfig{}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			_, ts := fullServer(t, mode.tr, nil)
			sid := ""
			if mode.tr.Stateful {
				sid = openSession(t, ts.URL, `{}`, "")
			} else {
				call(t, ts.URL, "", "initialize", 1, `{"protocolVersion":"2025-11-25","capabilities":{}}`)
			}

			// initialize: the result model, and exactly the capabilities served.
			res, _ := call(t, ts.URL, sid, "initialize", 2, `{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"1"}}`)
			checkShape(t, "initialize", res, initializeResultShape)
			caps := res.(map[string]any)["capabilities"].(map[string]any)
			want := map[string]any{
				"tools":     map[string]any{"listChanged": false},
				"resources": map[string]any{"subscribe": false, "listChanged": false},
				"prompts":   map[string]any{"listChanged": false},
			}
			if mustJSON(t, caps) != mustJSON(t, want) {
				t.Errorf("capabilities = %v, want %v", caps, want)
			}

			res, _ = call(t, ts.URL, sid, "tools/list", 3, "")
			checkShape(t, "tools/list", res, shape{"!tools": []shape{toolShape}, "nextCursor": "string", "_meta": "object"})

			res, _ = call(t, ts.URL, sid, "tools/call", 4, `{"name":"ask","arguments":{"message":"hi"}}`)
			checkCallToolResult(t, res)
			res, _ = call(t, ts.URL, sid, "tools/call", 5, `{"name":"fail","arguments":{}}`)
			checkCallToolResult(t, res)
			if res.(map[string]any)["isError"] != true {
				t.Errorf("fail tool: isError = %v", res.(map[string]any)["isError"])
			}

			res, _ = call(t, ts.URL, sid, "resources/list", 6, "")
			checkShape(t, "resources/list", res, listResourcesShape)
			list := res.(map[string]any)["resources"].([]any)
			if len(list) != 2 {
				t.Errorf("listed %d resources, want 2 (the unlisted one stays out)", len(list))
			}
			notes := list[0].(map[string]any)
			if notes["uri"] != "mem://notes" || notes["title"] != "Notes" || notes["mimeType"] != "text/markdown" {
				t.Errorf("notes entry = %v", notes)
			}
			if mustJSON(t, notes["_meta"]) != `{"owner":"ops"}` {
				t.Errorf("notes _meta = %v", notes["_meta"])
			}
			if _, has := list[1].(map[string]any)["title"]; has {
				t.Errorf("an entry without a title carries one: %v", list[1])
			}

			res, _ = call(t, ts.URL, sid, "resources/templates/list", 7, "")
			checkShape(t, "templates", res, listTemplatesShape)

			res, _ = call(t, ts.URL, sid, "resources/read", 8, `{"uri":"mem://notes"}`)
			checkShape(t, "read", res, shape{"!contents": "array", "_meta": "object"})
			text := res.(map[string]any)["contents"].([]any)[0]
			checkShape(t, "read.text", text, textContentsShape)
			if m := text.(map[string]any); m["text"] != "# notes" || m["mimeType"] != "text/markdown" || mustJSON(t, m["_meta"]) != `{"owner":"ops"}` {
				t.Errorf("text contents = %v", m)
			}
			res, _ = call(t, ts.URL, sid, "resources/read", 9, `{"uri":"mem://blob"}`)
			blob := res.(map[string]any)["contents"].([]any)[0]
			checkShape(t, "read.blob", blob, blobContentsShape)
			if m := blob.(map[string]any); m["blob"] != "AAEC" || m["mimeType"] != "application/octet-stream" {
				t.Errorf("blob contents = %v", m)
			}
			// Unlisted stays readable, with its own MIME type.
			res, _ = call(t, ts.URL, sid, "resources/read", 10, `{"uri":"mem://hidden"}`)
			if m := res.(map[string]any)["contents"].([]any)[0].(map[string]any); m["text"] != "psst" || m["mimeType"] != "text/x-secret" {
				t.Errorf("unlisted read = %v", m)
			}
			// A template read: variables decoded, MIME from the template.
			res, _ = call(t, ts.URL, sid, "resources/read", 11, `{"uri":"weather://Zürich%20West"}`)
			if m := res.(map[string]any)["contents"].([]any)[0].(map[string]any); m["text"] != "sunny in Zürich West" || m["mimeType"] != "text/plain" {
				t.Errorf("template read = %v", m)
			}

			res, _ = call(t, ts.URL, sid, "prompts/list", 12, "")
			checkShape(t, "prompts/list", res, listPromptsShape)
			prompts := res.(map[string]any)["prompts"].([]any)
			plain := prompts[1].(map[string]any)
			if a, ok := plain["arguments"].([]any); !ok || len(a) != 0 {
				t.Errorf("a prompt with no arguments lists %v, want an empty array (the SDK's model)", plain["arguments"])
			}
			arg0 := prompts[0].(map[string]any)["arguments"].([]any)[1].(map[string]any)
			if arg0["required"] != false {
				t.Errorf("an optional argument lists required = %v, want false", arg0["required"])
			}

			res, _ = call(t, ts.URL, sid, "prompts/get", 13, `{"name":"greet","arguments":{"who":"Ada","tone":"warm"}}`)
			checkShape(t, "prompts/get", res, getPromptResultShape)
			msgs := res.(map[string]any)["messages"].([]any)
			if mustJSON(t, msgs[0]) != `{"content":{"text":"Hello Ada","type":"text"},"role":"user"}` ||
				mustJSON(t, msgs[1]) != `{"content":{"text":"tone=warm","type":"text"},"role":"assistant"}` {
				t.Errorf("messages = %v", msgs)
			}
			if res.(map[string]any)["description"] != "Greets" {
				t.Errorf("description = %v", res.(map[string]any)["description"])
			}
			res, _ = call(t, ts.URL, sid, "prompts/get", 14, `{"name":"plain"}`)
			if _, has := res.(map[string]any)["description"]; has {
				t.Errorf("a prompt without description answers one: %v", res)
			}
		})
	}
}

func TestStrictErrorsCarryThePythonCodes(t *testing.T) {
	_, ts := fullServer(t, statefulStreaming(nil), nil)
	sid := openSession(t, ts.URL, `{}`, "")
	for _, tc := range []struct {
		name, method, params string
		code                 float64
		message              string
		data                 string
	}{
		{"unknown resource", "resources/read", `{"uri":"mem://nope"}`, -32602, "No resource matches URI 'mem://nope'.", `{"uri":"mem://nope"}`},
		{"a {var} never spans a decoded slash", "resources/read", `{"uri":"weather://a%2Fb"}`, -32602, "No resource matches URI 'weather://a%2Fb'.", `{"uri":"weather://a%2Fb"}`},
		{"escapes that are not UTF-8", "resources/read", `{"uri":"weather://%ff"}`, -32602, "No resource matches URI 'weather://%ff'.", `{"uri":"weather://%ff"}`},
		{"read without a uri", "resources/read", `{}`, -32602, "resources/read params.uri is required", ""},
		{"unknown prompt", "prompts/get", `{"name":"nope"}`, -32602, "No prompt named 'nope'.", ""},
		{"missing prompt argument", "prompts/get", `{"name":"greet","arguments":{"tone":"x"}}`, -32602, "Missing required arguments for prompt 'greet': who", ""},
		{"unknown tool", "tools/call", `{"name":"nope"}`, -32602, `Unknown tool: "nope".`, ""},
		{"unknown method", "logging/setLevel", `{"level":"info"}`, -32601, `method "logging/setLevel" is not supported`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, _ := rpcCall(t, ts.URL, map[string]string{"Mcp-Session-Id": sid},
				fmt.Sprintf(`{"jsonrpc":"2.0","id":7,"method":%q,"params":%s}`, tc.method, tc.params))
			_, e := checkResponseEnvelope(t, msg)
			if e == nil {
				t.Fatalf("no error: %v", msg)
			}
			if e["code"] != tc.code || e["message"] != tc.message {
				t.Errorf("error = %v, want code %v %q", e, tc.code, tc.message)
			}
			if tc.data != "" && mustJSON(t, e["data"]) != tc.data {
				t.Errorf("error data = %v, want %s", e["data"], tc.data)
			}
		})
	}
}

func TestStrictCapabilitiesOnlyWhenRegistered(t *testing.T) {
	// Python advertises a capability from the handlers actually registered, and
	// answers method-not-found for the rest.
	_, ts := fullServer(t, TransportConfig{Stateful: true}, func(c *ServerConfig) {
		c.Resources, c.ResourceTemplates, c.Prompts = nil, nil, nil
	})
	sid := openSession(t, ts.URL, `{}`, "")
	res, _ := call(t, ts.URL, sid, "initialize", 2, `{"protocolVersion":"2025-11-25","capabilities":{}}`)
	if caps := res.(map[string]any)["capabilities"].(map[string]any); mustJSON(t, caps) != `{"tools":{"listChanged":false}}` {
		t.Errorf("capabilities = %v, want only tools", caps)
	}
	for _, m := range []string{"resources/list", "resources/read", "resources/templates/list", "prompts/list", "prompts/get"} {
		msg, _ := rpcCall(t, ts.URL, map[string]string{"Mcp-Session-Id": sid}, fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":%q,"params":{}}`, m))
		if _, e := checkResponseEnvelope(t, msg); e == nil || e["code"] != -32601.0 {
			t.Errorf("%s with nothing registered = %v, want method not found", m, msg)
		}
	}
	// Static resources only: no templates endpoint.
	_, ts2 := fullServer(t, TransportConfig{Stateful: true}, func(c *ServerConfig) { c.ResourceTemplates = nil })
	sid2 := openSession(t, ts2.URL, `{}`, "")
	msg, _ := rpcCall(t, ts2.URL, map[string]string{"Mcp-Session-Id": sid2}, `{"jsonrpc":"2.0","id":3,"method":"resources/templates/list"}`)
	if _, e := checkResponseEnvelope(t, msg); e == nil || e["code"] != -32601.0 {
		t.Errorf("templates/list without templates = %v, want method not found", msg)
	}
}

func TestStrictTransportRefusals(t *testing.T) {
	_, ts := fullServer(t, statefulStreaming(nil), func(c *ServerConfig) { c.Transport.MaxRequestBodySize = 2048 })
	sid := openSession(t, ts.URL, `{}`, "")
	hdr := map[string]string{"Mcp-Session-Id": sid}
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`

	type refusal struct {
		name    string
		do      func() *http.Response
		status  int
		code    float64
		message string
	}
	postRaw := func(headers map[string]string, body string) func() *http.Response {
		return func() *http.Response { return post(t, ts.URL, headers, body) }
	}
	merge := func(extra map[string]string) map[string]string {
		out := map[string]string{"Mcp-Session-Id": sid}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	for _, tc := range []refusal{
		{"json-only Accept on an SSE server", postRaw(merge(map[string]string{"Accept": "application/json"}), ping), 406, -32600, "Not Acceptable: Client must accept both application/json and text/event-stream"},
		{"wrong Content-Type", postRaw(merge(map[string]string{"Content-Type": "text/plain"}), ping), 415, -32600, "Unsupported Media Type: Content-Type must be application/json"},
		{"garbage body", postRaw(hdr, `not json`), 400, -32700, ""},
		{"a batch", postRaw(hdr, `[`+ping+`]`), 400, -32602, ""},
		{"not JSON-RPC 2.0", postRaw(hdr, `{"jsonrpc":"1.0","id":1,"method":"ping"}`), 400, -32602, ""},
		{"no session id", postRaw(nil, ping), 400, -32600, "Bad Request: Missing session ID"},
		{"unknown session id", postRaw(map[string]string{"Mcp-Session-Id": "nope"}, ping), 404, -32600, "Session not found"},
		{"body over the limit", postRaw(hdr, `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"`+strings.Repeat("x", 4096)+`"}}`), 413, -32600, ""},
		{"a revision the server does not speak", postRaw(merge(map[string]string{"MCP-Protocol-Version": "2026-07-28"}), ping), 400, -32022, "Unsupported protocol version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := tc.do()
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			body := decodeObj(t, readAll(t, resp.Body))
			// A transport-level error has no addressable request: id is null, not absent.
			if id, has := body["id"]; !has || id != nil {
				t.Errorf("id = %v (present=%v), want an explicit null", id, has)
			}
			e := body["error"].(map[string]any)
			checkShape(t, "error", e, shape{"!code": "number", "!message": "string", "data": "any"})
			if e["code"] != tc.code {
				t.Errorf("code = %v, want %v", e["code"], tc.code)
			}
			if tc.message != "" && e["message"] != tc.message {
				t.Errorf("message = %v, want %q", e["message"], tc.message)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q", ct)
			}
		})
	}

	t.Run("the unsupported-revision answer names the revisions that are served", func(t *testing.T) {
		// The client's auto-negotiation reads this list to fall back to the
		// handshake (mcp.client._probe).
		resp := post(t, ts.URL, merge(map[string]string{"MCP-Protocol-Version": "2026-07-28"}), `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}`)
		defer resp.Body.Close()
		data := decodeObj(t, readAll(t, resp.Body))["error"].(map[string]any)["data"].(map[string]any)
		if mustJSON(t, data["supported"]) != `["2024-11-05","2025-03-26","2025-06-18","2025-11-25"]` || data["requested"] != "2026-07-28" {
			t.Errorf("data = %v", data)
		}
	})

	t.Run("GET without text/event-stream", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
		req.Header.Set("Mcp-Session-Id", sid)
		req.Header.Set("Accept", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 406 {
			t.Errorf("status = %d, want 406", resp.StatusCode)
		}
	})
	t.Run("unsupported method", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPut, ts.URL, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 405 || resp.Header.Get("Allow") != "GET, POST, DELETE" {
			t.Errorf("PUT = %d Allow %q", resp.StatusCode, resp.Header.Get("Allow"))
		}
	})
}

func TestStrictSessionLifecycle(t *testing.T) {
	s, ts := fullServer(t, TransportConfig{Stateful: true, MaxSessions: 2, SessionIdleTimeout: 150 * time.Millisecond}, nil)
	a := openSession(t, ts.URL, `{}`, "")
	b := openSession(t, ts.URL, `{}`, "")
	if a == b {
		t.Fatal("two sessions share an id")
	}
	// The cap: a third initialize is refused 503 (INTERNAL_ERROR), and nothing
	// about the open ones changes.
	resp := post(t, ts.URL, nil, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`)
	if resp.StatusCode != 503 {
		t.Errorf("third session = %d, want 503", resp.StatusCode)
	}
	if e := decodeObj(t, readAll(t, resp.Body))["error"].(map[string]any); e["code"] != -32603.0 || e["message"] != "Too many open sessions" {
		t.Errorf("503 error = %v", e)
	}
	resp.Body.Close()

	// DELETE ends one; its id answers 404 afterwards, the other still works.
	req, _ := http.NewRequest(http.MethodDelete, ts.URL, nil)
	req.Header.Set("Mcp-Session-Id", a)
	del, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	del.Body.Close()
	if del.StatusCode != 200 {
		t.Errorf("DELETE = %d, want 200", del.StatusCode)
	}
	r := post(t, ts.URL, map[string]string{"Mcp-Session-Id": a}, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	r.Body.Close()
	if r.StatusCode != 404 {
		t.Errorf("request on a deleted session = %d, want 404", r.StatusCode)
	}
	if msg, _ := rpcCall(t, ts.URL, map[string]string{"Mcp-Session-Id": b}, `{"jsonrpc":"2.0","id":3,"method":"ping"}`); msg["error"] != nil {
		t.Errorf("the other session broke: %v", msg)
	}
	// A freed slot admits a new session again.
	openSession(t, ts.URL, `{}`, "")

	// Idle reaping: with no request in flight and no stream open, an idle session
	// answers 404 once the timeout passed.
	time.Sleep(300 * time.Millisecond)
	r = post(t, ts.URL, map[string]string{"Mcp-Session-Id": b}, `{"jsonrpc":"2.0","id":4,"method":"ping"}`)
	r.Body.Close()
	if r.StatusCode != 404 {
		t.Errorf("request on an idle-reaped session = %d, want 404", r.StatusCode)
	}
	if n := s.sessions.count(); n > 1 {
		t.Errorf("%d sessions still held after reaping", n)
	}
}

func TestStrictStatelessAndDelete(t *testing.T) {
	_, ts := fullServer(t, TransportConfig{StreamResponses: true}, nil)
	// A stateless transport issues no session id and DELETE has nothing to end.
	call(t, ts.URL, "", "initialize", 1, `{"protocolVersion":"2025-11-25","capabilities":{}}`)
	resp := post(t, ts.URL, nil, `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`)
	resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.Errorf("a stateless server issued session id %q", sid)
	}
	req, _ := http.NewRequest(http.MethodDelete, ts.URL, nil)
	del, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	del.Body.Close()
	if del.StatusCode != 405 {
		t.Errorf("DELETE on a stateless transport = %d, want 405", del.StatusCode)
	}
}

func TestStrictSSEFraming(t *testing.T) {
	_, ts := fullServer(t, statefulStreaming(nil), nil)
	sid := openSession(t, ts.URL, `{}`, "")
	resp := post(t, ts.URL, map[string]string{"Mcp-Session-Id": sid}, `{"jsonrpc":"2.0","id":9,"method":"tools/list"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	for k, want := range map[string]string{
		"Content-Type": "text/event-stream", "Cache-Control": "no-cache, no-transform",
		"Connection": "keep-alive", "Mcp-Session-Id": sid,
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	evs := readEvents(t, resp.Body)
	if len(evs) != 1 || evs[0].Event != "message" {
		t.Fatalf("events = %+v, want one event: message", evs)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(evs[0].Data), &m); err != nil || m["id"] != 9.0 {
		t.Errorf("data = %q (%v)", evs[0].Data, err)
	}
}

func TestAuthorizeGatesResourcesAndPrompts(t *testing.T) {
	var seen []string
	_, ts := fullServer(t, TransportConfig{Stateful: true}, func(c *ServerConfig) {
		c.Authorize = func(_ context.Context, method string) error {
			seen = append(seen, method)
			if strings.HasPrefix(method, "resources/") || strings.HasPrefix(method, "prompts/") {
				return fmt.Errorf("no")
			}
			return nil
		}
	})
	sid := openSession(t, ts.URL, `{}`, "")
	for _, m := range []string{"resources/list", "resources/read", "resources/templates/list", "prompts/list", "prompts/get"} {
		msg, _ := rpcCall(t, ts.URL, map[string]string{"Mcp-Session-Id": sid}, fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":{"uri":"mem://notes","name":"greet"}}`, m))
		if _, e := checkResponseEnvelope(t, msg); e == nil || e["code"] != -32001.0 || e["message"] != "unauthorized" {
			t.Errorf("%s = %v, want the unauthorized refusal", m, msg)
		}
	}
	if msg, _ := rpcCall(t, ts.URL, map[string]string{"Mcp-Session-Id": sid}, `{"jsonrpc":"2.0","id":2,"method":"ping"}`); msg["error"] != nil {
		t.Errorf("ping was gated: %v", msg)
	}
}

func TestNilExchangeHasNoChannel(t *testing.T) {
	// A call outside any request (no exchange at all) must neither panic nor
	// pretend to deliver: notifications drop, requests report no back-channel.
	var ex *exchange
	ex.notify(context.Background(), "notifications/message", map[string]any{"data": "x"})
	if _, err := ex.request(context.Background(), "ping", nil); !errors.Is(err, ErrNoBackChannel) {
		t.Errorf("request on a nil exchange = %v, want ErrNoBackChannel", err)
	}
	// An exchange that is not answering as a stream drops notifications too, and
	// one without a session cannot ask.
	plain := &exchange{}
	plain.notify(context.Background(), "notifications/message", nil)
	if _, err := plain.request(context.Background(), "ping", nil); !errors.Is(err, ErrNoBackChannel) {
		t.Errorf("request without a channel = %v, want ErrNoBackChannel", err)
	}
	noSession := &exchange{canRequest: true, sse: true}
	if _, err := noSession.request(context.Background(), "ping", nil); !errors.Is(err, ErrNoBackChannel) {
		t.Errorf("request without a session = %v, want ErrNoBackChannel", err)
	}
}

func TestNegativeSessionBoundsMeanUnbounded(t *testing.T) {
	_, ts := fullServer(t, TransportConfig{Stateful: true, MaxSessions: -1, SessionIdleTimeout: -1}, nil)
	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, openSession(t, ts.URL, `{}`, ""))
	}
	time.Sleep(150 * time.Millisecond)
	for _, id := range ids {
		r := post(t, ts.URL, map[string]string{"Mcp-Session-Id": id}, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
		r.Body.Close()
		if r.StatusCode != 200 {
			t.Errorf("session %s = %d: a negative bound removes the limit", id, r.StatusCode)
		}
	}
}
