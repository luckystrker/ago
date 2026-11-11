package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// --- a scripted model that streams ---------------------------------------------------

// scriptedModel is a config.ModelConfig whose k-th call runs the k-th step. A step
// may stream chunks through conv.Send before it returns its response, which is
// what a real provider client does and what the progress forwarder listens for.
type scriptedModel struct {
	mu    sync.Mutex
	steps []func(conv *agocontext.Context) *events.ModelResponse
	calls int
	seen  [][]events.Event // the history each call was handed
}

func (m *scriptedModel) Provider() config.Provider                        { return "test" }
func (m *scriptedModel) Model() string                                    { return "scripted" }
func (m *scriptedModel) Create() (config.LLMClient, error)                { return m, nil }
func (m *scriptedModel) Clone(map[string]any) (config.ModelConfig, error) { return m, nil }

func (m *scriptedModel) Call(_ context.Context, messages []events.Event, conv *agocontext.Context, _ config.CallOptions) (*events.ModelResponse, error) {
	m.mu.Lock()
	i := m.calls
	m.calls++
	m.seen = append(m.seen, append([]events.Event(nil), messages...))
	var step func(*agocontext.Context) *events.ModelResponse
	if i < len(m.steps) {
		step = m.steps[i]
	}
	m.mu.Unlock()
	if step == nil {
		return nil, fmt.Errorf("scriptedModel: no step %d", i)
	}
	return step(conv), nil
}

func (m *scriptedModel) history(call int) []events.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seen[call]
}

func (m *scriptedModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func say(text string) func(*agocontext.Context) *events.ModelResponse {
	return func(*agocontext.Context) *events.ModelResponse {
		return &events.ModelResponse{Message: &events.ModelMessage{Content: text}}
	}
}

// streamThenCall streams the chunks, then answers with a call of the named tool.
func streamThenCall(tool string, chunks ...string) func(*agocontext.Context) *events.ModelResponse {
	return func(conv *agocontext.Context) *events.ModelResponse {
		for _, c := range chunks {
			_ = conv.Send(&events.ModelMessageChunk{Content: c})
		}
		return &events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "call-" + tool, Name: tool, Arguments: "{}"},
		}}}
	}
}

// streamThenSay streams the chunks, then answers with text.
func streamThenSay(text string, chunks ...string) func(*agocontext.Context) *events.ModelResponse {
	return func(conv *agocontext.Context) *events.ModelResponse {
		for _, c := range chunks {
			_ = conv.Send(&events.ModelMessageChunk{Content: c})
		}
		return &events.ModelResponse{Message: &events.ModelMessage{Content: text}}
	}
}

// --- raw streamable-HTTP plumbing ------------------------------------------------------

// httpMsg is one server-sent event as the wire carried it.
type httpMsg struct {
	ID    string
	Event string
	Data  string
}

// readEvents parses an SSE body to its end.
func readEvents(t *testing.T, body io.Reader) []httpMsg {
	t.Helper()
	var out []httpMsg
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	var cur httpMsg
	var has bool
	flush := func() {
		if has {
			out = append(out, cur)
		}
		cur, has = httpMsg{}, false
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, ":"):
		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			has = true
			switch field {
			case "id":
				cur.ID = value
			case "event":
				cur.Event = value
			case "data":
				if cur.Data != "" {
					cur.Data += "\n"
				}
				cur.Data += value
			}
		}
	}
	flush()
	return out
}

// post sends one JSON-RPC message with the headers of a conforming client.
func post(t *testing.T, url string, headers map[string]string, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// A conforming client names the negotiated revision on every request after
	// initialize (the 2025-06-18 rule the SDK's own client follows).
	if v, ok := sessionVersions.Load(req.Header.Get("Mcp-Session-Id")); ok && req.Header.Get("MCP-Protocol-Version") == "" {
		req.Header.Set("MCP-Protocol-Version", v.(string))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return resp
}

// sessionVersions remembers the revision each test session negotiated.
var sessionVersions sync.Map

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

// rpcCall posts a request and returns the JSON-RPC message that answers it,
// whether the server answered with JSON or with an SSE stream (the last event).
func rpcCall(t *testing.T, url string, headers map[string]string, body string) (map[string]any, http.Header) {
	t.Helper()
	resp := post(t, url, headers, body)
	defer resp.Body.Close()
	raw := readAll(t, resp.Body)
	var payload string
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		evs := readEvents(t, strings.NewReader(raw))
		if len(evs) == 0 {
			t.Fatalf("empty SSE answer to %s", body)
		}
		payload = evs[len(evs)-1].Data
	} else {
		payload = raw
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		t.Fatalf("decode %q (status %d): %v", payload, resp.StatusCode, err)
	}
	return out, resp.Header
}

// openSession runs initialize on a stateful server and confirms it, returning the
// session id the server issued.
func openSession(t *testing.T, url string, caps string, version string) string {
	t.Helper()
	if version == "" {
		version = "2025-11-25"
	}
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%q,"capabilities":%s,"clientInfo":{"name":"t","version":"1"}}}`, version, caps)
	resp, hdr := rpcCall(t, url, nil, body)
	if resp["error"] != nil {
		t.Fatalf("initialize: %v", resp["error"])
	}
	sid := hdr.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("initialize issued no Mcp-Session-Id")
	}
	sessionVersions.Store(sid, version)
	r := post(t, url, map[string]string{"Mcp-Session-Id": sid}, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("initialized = %d, want 202", r.StatusCode)
	}
	return sid
}

// --- the SDK's wire shapes ---------------------------------------------------------------

// shape describes one JSON object the way the mcp SDK's models do: every key the
// object may carry, with "!" marking the required ones. A value is a kind name
// ("string", "number", "bool", "object", "array", "any"), a nested shape, or a
// one-element []shape slice for an array of objects. A key not listed is a wire
// violation: the SDK's models forbid unknown fields on the messages it parses.
type shape map[string]any

func checkShape(t *testing.T, path string, v any, spec shape) {
	t.Helper()
	obj, ok := v.(map[string]any)
	if !ok {
		t.Errorf("%s: %T, want an object", path, v)
		return
	}
	allowed := map[string]any{}
	for k, s := range spec {
		name := strings.TrimPrefix(k, "!")
		allowed[name] = s
		if strings.HasPrefix(k, "!") {
			if _, present := obj[name]; !present {
				t.Errorf("%s: missing required field %q", path, name)
			}
		}
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s, known := allowed[k]
		if !known {
			t.Errorf("%s: unexpected field %q (not in the SDK's model)", path, k)
			continue
		}
		checkValue(t, path+"."+k, obj[k], s)
	}
}

func checkValue(t *testing.T, path string, v any, s any) {
	t.Helper()
	switch want := s.(type) {
	case string:
		ok := false
		switch want {
		case "string":
			_, ok = v.(string)
		case "number":
			_, ok = v.(float64)
		case "bool":
			_, ok = v.(bool)
		case "object":
			_, ok = v.(map[string]any)
		case "array":
			_, ok = v.([]any)
		case "any":
			ok = true
		}
		if !ok {
			t.Errorf("%s: %T is not %s", path, v, want)
		}
	case shape:
		checkShape(t, path, v, want)
	case []shape:
		arr, ok := v.([]any)
		if !ok {
			t.Errorf("%s: %T, want an array", path, v)
			return
		}
		for i, e := range arr {
			checkShape(t, fmt.Sprintf("%s[%d]", path, i), e, want[0])
		}
	}
}

var (
	implementationShape = shape{"!name": "string", "!version": "string", "title": "string", "description": "string", "websiteUrl": "string", "icons": "array"}
	serverCapsShape     = shape{
		"experimental": "object", "extensions": "object", "logging": "object", "completions": "object",
		"prompts":   shape{"listChanged": "bool"},
		"resources": shape{"subscribe": "bool", "listChanged": "bool"},
		"tools":     shape{"listChanged": "bool"},
	}
	initializeResultShape = shape{"!protocolVersion": "string", "!capabilities": serverCapsShape, "!serverInfo": implementationShape, "instructions": "string", "_meta": "object"}

	toolShape = shape{"!name": "string", "description": "string", "!inputSchema": "object", "title": "string", "annotations": "object", "outputSchema": "object", "icons": "array", "_meta": "object", "execution": "object"}

	textBlockShape     = shape{"!type": "string", "!text": "string", "annotations": "object", "_meta": "object"}
	imageBlockShape    = shape{"!type": "string", "!data": "string", "!mimeType": "string", "annotations": "object", "_meta": "object"}
	resourceBlockShape = shape{"!type": "string", "!resource": "object", "annotations": "object", "_meta": "object"}
	linkBlockShape     = shape{"!type": "string", "!uri": "string", "!name": "string", "title": "string", "description": "string", "mimeType": "string", "size": "number", "annotations": "object", "icons": "array", "_meta": "object"}

	resourceShape         = shape{"!uri": "string", "!name": "string", "title": "string", "description": "string", "mimeType": "string", "size": "number", "annotations": "object", "icons": "array", "_meta": "object"}
	listResourcesShape    = shape{"!resources": []shape{resourceShape}, "nextCursor": "string", "_meta": "object"}
	templateShape         = shape{"!uriTemplate": "string", "!name": "string", "title": "string", "description": "string", "mimeType": "string", "annotations": "object", "icons": "array", "_meta": "object"}
	listTemplatesShape    = shape{"!resourceTemplates": []shape{templateShape}, "nextCursor": "string", "_meta": "object"}
	textContentsShape     = shape{"!uri": "string", "mimeType": "string", "!text": "string", "_meta": "object"}
	blobContentsShape     = shape{"!uri": "string", "mimeType": "string", "!blob": "string", "_meta": "object"}
	promptArgShape        = shape{"!name": "string", "title": "string", "description": "string", "required": "bool"}
	promptShape           = shape{"!name": "string", "title": "string", "description": "string", "arguments": []shape{promptArgShape}, "icons": "array", "_meta": "object"}
	listPromptsShape      = shape{"!prompts": []shape{promptShape}, "nextCursor": "string", "_meta": "object"}
	promptMessageShape    = shape{"!role": "string", "!content": "object"}
	getPromptResultShape  = shape{"description": "string", "!messages": []shape{promptMessageShape}, "_meta": "object"}
	progressParamsShape   = shape{"!progressToken": "any", "!progress": "number", "total": "number", "message": "string", "_meta": "object"}
	messageParamsShape    = shape{"!level": "string", "!data": "any", "logger": "string", "_meta": "object"}
	elicitFormParamsShape = shape{"mode": "string", "!message": "string", "!requestedSchema": shape{"!type": "string", "!properties": "object", "required": "array", "$schema": "string"}, "_meta": "object"}
	samplingParamsShape   = shape{"!messages": []shape{{"!role": "string", "!content": "object"}}, "systemPrompt": "string", "!maxTokens": "number", "temperature": "number", "stopSequences": "array", "metadata": "object", "modelPreferences": "object", "includeContext": "string", "tools": "array", "toolChoice": "object", "_meta": "object"}
)

// checkContentBlocks validates every block of a result against its variant.
func checkContentBlocks(t *testing.T, path string, v any) {
	t.Helper()
	blocks, ok := v.([]any)
	if !ok || len(blocks) == 0 {
		t.Errorf("%s: want a non-empty content array, got %v", path, v)
		return
	}
	for i, b := range blocks {
		p := fmt.Sprintf("%s[%d]", path, i)
		obj, _ := b.(map[string]any)
		switch obj["type"] {
		case "text":
			checkShape(t, p, b, textBlockShape)
		case "image", "audio":
			checkShape(t, p, b, imageBlockShape)
		case "resource":
			checkShape(t, p, b, resourceBlockShape)
		case "resource_link":
			checkShape(t, p, b, linkBlockShape)
		default:
			t.Errorf("%s: unknown content block type %v", p, obj["type"])
		}
	}
}

func checkCallToolResult(t *testing.T, v any) {
	t.Helper()
	checkShape(t, "result", v, shape{"!content": "array", "isError": "bool", "structuredContent": "object", "_meta": "object", "resultType": "string"})
	checkContentBlocks(t, "result.content", v.(map[string]any)["content"])
}

// checkResponseEnvelope validates the JSON-RPC envelope of one answer and
// returns its result (or the error object).
func checkResponseEnvelope(t *testing.T, msg map[string]any) (result any, rpcErr map[string]any) {
	t.Helper()
	if msg["jsonrpc"] != "2.0" {
		t.Errorf("envelope: jsonrpc = %v", msg["jsonrpc"])
	}
	if _, hasID := msg["id"]; !hasID {
		t.Errorf("envelope: no id")
	}
	if e, ok := msg["error"].(map[string]any); ok {
		checkShape(t, "error", e, shape{"!code": "number", "!message": "string", "data": "any"})
		if _, both := msg["result"]; both {
			t.Errorf("envelope carries both result and error")
		}
		return nil, e
	}
	if _, ok := msg["result"]; !ok {
		t.Errorf("envelope carries neither result nor error: %v", msg)
	}
	return msg["result"], nil
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func decodeObj(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(bytes.NewReader([]byte(raw))).Decode(&m); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return m
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// streamEvents parses an SSE body as it arrives, so a test can answer a server
// request mid-stream. The channel closes when the body ends.
func streamEvents(body io.Reader) <-chan httpMsg {
	ch := make(chan httpMsg, 64)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
		var cur httpMsg
		var has bool
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if has {
					ch <- cur
				}
				cur, has = httpMsg{}, false
			case strings.HasPrefix(line, ":"):
			default:
				field, value, _ := strings.Cut(line, ":")
				value = strings.TrimPrefix(value, " ")
				has = true
				switch field {
				case "id":
					cur.ID = value
				case "event":
					cur.Event = value
				case "data":
					if cur.Data != "" {
						cur.Data += "\n"
					}
					cur.Data += value
				}
			}
		}
		if has {
			ch <- cur
		}
	}()
	return ch
}

// next returns the next event or fails the test after a deadline.
func nextEvent(t *testing.T, ch <-chan httpMsg) httpMsg {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("the stream ended before the expected event")
		}
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for an SSE event")
	}
	return httpMsg{}
}

func errorsIs(err, target error) bool { return errors.Is(err, target) }
