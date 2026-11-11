package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/luckystrker/ago/agent"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
)

// rpcOver posts one JSON-RPC message with the given headers and decodes the
// answer into generic JSON, so the tests assert on the wire fields themselves.
func rpcOver(t *testing.T, url string, header http.Header, msg string) map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(msg))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return out
}

const initMsg = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`

type forecast struct {
	City string `json:"city"`
	Temp int    `json:"temp"`
}

func forecastTool(t *testing.T) tools.Tool {
	t.Helper()
	ft, err := tools.NewFunctionTool("forecast", "Weather forecast",
		func(a struct {
			City string `json:"city"`
		}) (forecast, error) {
			return forecast{City: a.City, Temp: 21}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return ft
}

func boolPtr(b bool) *bool { return &b }

var forecastSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"city": map[string]any{"type": "string"},
		"temp": map[string]any{"type": "integer"},
	},
	"required": []any{"city", "temp"},
}

func toolByName(t *testing.T, list map[string]any, name string) map[string]any {
	t.Helper()
	result, _ := list["result"].(map[string]any)
	for _, raw := range result["tools"].([]any) {
		if tool := raw.(map[string]any); tool["name"] == name {
			return tool
		}
	}
	t.Fatalf("tool %q not listed in %v", name, list)
	return nil
}

// tools/list advertises title, annotations, outputSchema and _meta, and
// tools/call answers structuredContent next to the text content.
func TestToolMetaRoundTrip(t *testing.T) {
	s, err := NewServer(ServerConfig{
		Ask:   func(context.Context, string) (string, error) { return "hi", nil },
		Tools: []tools.Tool{forecastTool(t)},
		ToolMeta: map[string]ToolMeta{
			"forecast": {
				Title:        "City forecast",
				Annotations:  &ToolAnnotations{ReadOnlyHint: boolPtr(true), OpenWorldHint: boolPtr(false)},
				OutputSchema: forecastSchema,
				Meta:         map[string]any{"vendor/tag": "weather"},
			},
			"ask": {Title: "Ask the agent"},
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	rpcOver(t, ts.URL, nil, initMsg)

	list := rpcOver(t, ts.URL, nil, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	f := toolByName(t, list, "forecast")
	if f["title"] != "City forecast" {
		t.Errorf("title = %v", f["title"])
	}
	wantAnn := map[string]any{"readOnlyHint": true, "openWorldHint": false}
	if !reflect.DeepEqual(f["annotations"], wantAnn) {
		t.Errorf("annotations = %v, want %v", f["annotations"], wantAnn)
	}
	var wantSchema any
	raw, _ := json.Marshal(forecastSchema)
	_ = json.Unmarshal(raw, &wantSchema)
	if !reflect.DeepEqual(f["outputSchema"], wantSchema) {
		t.Errorf("outputSchema = %v", f["outputSchema"])
	}
	if !reflect.DeepEqual(f["_meta"], map[string]any{"vendor/tag": "weather"}) {
		t.Errorf("_meta = %v", f["_meta"])
	}
	if a := toolByName(t, list, "ask"); a["title"] != "Ask the agent" || a["outputSchema"] != nil {
		t.Errorf("ask tool = %v", a)
	}

	call := rpcOver(t, ts.URL, nil,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"forecast","arguments":{"city":"Oslo"}}}`)
	res := call["result"].(map[string]any)
	if !reflect.DeepEqual(res["structuredContent"], map[string]any{"city": "Oslo", "temp": float64(21)}) {
		t.Errorf("structuredContent = %v", res["structuredContent"])
	}
	content := res["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != `{"city":"Oslo","temp":21}` {
		t.Errorf("content = %v", content)
	}
}

// Without metadata the wire stays as it was: no extra list fields and no
// structuredContent, even for a tool answering a JSON object.
func TestToolWithoutMetaKeepsOldWire(t *testing.T) {
	s, err := NewServer(ServerConfig{
		Ask:   func(context.Context, string) (string, error) { return "hi", nil },
		Tools: []tools.Tool{forecastTool(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	rpcOver(t, ts.URL, nil, initMsg)
	list := rpcOver(t, ts.URL, nil, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	f := toolByName(t, list, "forecast")
	for _, key := range []string{"title", "annotations", "outputSchema", "_meta"} {
		if _, ok := f[key]; ok {
			t.Errorf("list carries %q without metadata: %v", key, f)
		}
	}
	call := rpcOver(t, ts.URL, nil,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"forecast","arguments":{"city":"Oslo"}}}`)
	if _, ok := call["result"].(map[string]any)["structuredContent"]; ok {
		t.Errorf("structuredContent without an output schema: %v", call)
	}
}

func TestToolMetaIsValidatedAtConstruction(t *testing.T) {
	ask := func(context.Context, string) (string, error) { return "", nil }
	cases := map[string]map[string]ToolMeta{
		"unknown tool":      {"nope": {Title: "x"}},
		"non-object schema": {"ask": {OutputSchema: map[string]any{"type": "string"}}},
		"schema w/o type":   {"ask": {OutputSchema: map[string]any{}}},
	}
	for name, meta := range cases {
		if _, err := NewServer(ServerConfig{Ask: ask, ToolMeta: meta}); !errors.Is(err, ErrInvalidToolMeta) {
			t.Errorf("%s: err = %v, want ErrInvalidToolMeta", name, err)
		}
	}
}

// The ask tool answers structuredContent when it advertises an output schema
// and the reply is one JSON object.
func TestAskStructuredContent(t *testing.T) {
	s, err := NewServer(ServerConfig{
		Ask:      func(context.Context, string) (string, error) { return `{"verdict":"ok"}`, nil },
		ToolMeta: map[string]ToolMeta{"ask": {OutputSchema: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	rpcOver(t, ts.URL, nil, initMsg)
	call := rpcOver(t, ts.URL, nil,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ask","arguments":{"message":"go"}}}`)
	got := call["result"].(map[string]any)["structuredContent"]
	if !reflect.DeepEqual(got, map[string]any{"verdict": "ok"}) {
		t.Errorf("structuredContent = %v", got)
	}
}

type whoArgs struct {
	Req    tools.Inject `aginject:"mcp_request"`
	Region string       `agvariable:"region"`
}

// A tool reaches the request it serves — HTTP request, method, tool name — through
// the injected RequestContext, and its variables and dependencies through the
// server-level maps; a ToolMeta.Resolve sees the same context per request.
func TestRequestScopedContext(t *testing.T) {
	var sawFromTurnCtx string
	who, err := tools.NewFunctionTool("who", "Who is calling",
		func(a whoArgs, cx *agocontext.Context) (string, error) {
			rc := a.Req.Default.(*RequestContext)
			if r := RequestFromContext(cx.TurnContext()); r != nil {
				sawFromTurnCtx = r.Header.Get("X-Tenant")
			}
			return rc.Method + "/" + rc.Tool + "/" + rc.HTTP.Header.Get("X-Tenant") + "/" + a.Region, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(ServerConfig{
		Ask:       func(context.Context, string) (string, error) { return "", nil },
		Tools:     []tools.Tool{who},
		Variables: map[string]any{"region": "eu"},
		ToolMeta: map[string]ToolMeta{"who": {Resolve: func(cx *agocontext.Context) ToolMeta {
			rc := cx.Dependencies[RequestDependency].(*RequestContext)
			region, _ := cx.Variable("region")
			return ToolMeta{Title: "Who for " + rc.HTTP.Header.Get("X-Tenant") + " in " + region.(string)}
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	rpcOver(t, ts.URL, nil, initMsg)

	for _, tenant := range []string{"acme", "globex"} {
		h := http.Header{"X-Tenant": {tenant}}
		call := rpcOver(t, ts.URL, h,
			`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"who","arguments":{}}}`)
		text := call["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
		if want := "tools/call/who/" + tenant + "/eu"; text != want {
			t.Errorf("tool saw %v, want %q (response %v)", text, want, call)
		}
		if sawFromTurnCtx != tenant {
			t.Errorf("RequestFromContext(TurnContext) tenant = %q, want %q", sawFromTurnCtx, tenant)
		}
		list := rpcOver(t, ts.URL, h, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
		if title := toolByName(t, list, "who")["title"]; title != "Who for "+tenant+" in eu" {
			t.Errorf("title = %v for tenant %s", title, tenant)
		}
	}
}

// The per-request copies keep one request's variable writes from reaching the next.
func TestRequestVariablesAreCopiedPerRequest(t *testing.T) {
	bump, err := tools.NewFunctionTool("bump", "Writes a variable",
		func(_ struct{}, cx *agocontext.Context) (string, error) {
			prev, _ := cx.Variable("n")
			if err := cx.SetVariable("n", "dirty"); err != nil {
				return "", err
			}
			s, _ := prev.(string)
			return s, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]any{"n": "clean"}
	s, err := NewServer(ServerConfig{
		Ask:       func(context.Context, string) (string, error) { return "", nil },
		Tools:     []tools.Tool{bump},
		Variables: vars,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	rpcOver(t, ts.URL, nil, initMsg)
	for i := 0; i < 2; i++ {
		call := rpcOver(t, ts.URL, nil,
			`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"bump","arguments":{}}}`)
		text := call["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
		if text != "clean" {
			t.Fatalf("call %d saw %v, want clean", i, text)
		}
	}
	if vars["n"] != "clean" {
		t.Fatalf("the configured map was written through: %v", vars)
	}
}

type greetArgs struct {
	Greeting tools.Inject `aginject:"greeting"`
	Region   string       `agvariable:"region"`
}

// The option funcs reach the server: WithToolMeta advertises metadata for the
// conversational tool and a served one, WithVariables and WithDependencies are
// what a tool's slots resolve against.
func TestServerOptionsCarryMetaVariablesAndDependencies(t *testing.T) {
	greet, err := tools.NewFunctionTool("greet", "Greets",
		func(a greetArgs) (string, error) { return a.Greeting.Default.(string) + " from " + a.Region, nil })
	if err != nil {
		t.Fatal(err)
	}
	cfg := agotest.NewTestConfig()
	a := agent.New("served", "", agent.WithConfig(cfg))
	s, err := NewServerForAgent(a,
		WithTools(greet),
		WithToolMeta("ask", ToolMeta{Title: "Ask it"}),
		WithToolMeta("greet", ToolMeta{Title: "Greeter", Annotations: &ToolAnnotations{IdempotentHint: boolPtr(true)}}),
		WithVariables(map[string]any{"region": "eu"}),
		WithDependencies(map[string]any{"greeting": "hello"}),
	)
	if err != nil {
		t.Fatalf("NewServerForAgent: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	rpcOver(t, ts.URL, nil, initMsg)

	list := rpcOver(t, ts.URL, nil, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if got := toolByName(t, list, "ask")["title"]; got != "Ask it" {
		t.Errorf("ask title = %v", got)
	}
	g := toolByName(t, list, "greet")
	if g["title"] != "Greeter" || !reflect.DeepEqual(g["annotations"], map[string]any{"idempotentHint": true}) {
		t.Errorf("greet = %v", g)
	}
	call := rpcOver(t, ts.URL, nil,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"greet","arguments":{}}}`)
	text := call["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
	if text != "hello from eu" {
		t.Errorf("greet answered %v, want %q", text, "hello from eu")
	}
}

// Resolve contributes each of the four fields on its own, per request: for
// tools/list and for the structuredContent decision of tools/call. Fields it
// leaves zero keep the static value.
func TestToolMetaResolveEveryField(t *testing.T) {
	obj := map[string]any{"type": "object"}
	ann := &ToolAnnotations{DestructiveHint: boolPtr(true)}
	cases := []struct {
		name     string
		static   ToolMeta
		resolved ToolMeta
		check    func(t *testing.T, tool map[string]any)
	}{
		{"title", ToolMeta{Title: "static"}, ToolMeta{Title: "resolved"},
			func(t *testing.T, tool map[string]any) {
				if tool["title"] != "resolved" {
					t.Errorf("title = %v", tool["title"])
				}
			}},
		{"annotations", ToolMeta{}, ToolMeta{Annotations: ann},
			func(t *testing.T, tool map[string]any) {
				if !reflect.DeepEqual(tool["annotations"], map[string]any{"destructiveHint": true}) {
					t.Errorf("annotations = %v", tool["annotations"])
				}
			}},
		{"outputSchema", ToolMeta{}, ToolMeta{OutputSchema: obj},
			func(t *testing.T, tool map[string]any) {
				if !reflect.DeepEqual(tool["outputSchema"], map[string]any{"type": "object"}) {
					t.Errorf("outputSchema = %v", tool["outputSchema"])
				}
			}},
		{"_meta", ToolMeta{Meta: map[string]any{"old": 1}}, ToolMeta{Meta: map[string]any{"new": "v"}},
			func(t *testing.T, tool map[string]any) {
				if !reflect.DeepEqual(tool["_meta"], map[string]any{"new": "v"}) {
					t.Errorf("_meta = %v", tool["_meta"])
				}
			}},
		{"zero resolution keeps static", ToolMeta{Title: "kept", Annotations: ann, Meta: map[string]any{"k": 1}}, ToolMeta{},
			func(t *testing.T, tool map[string]any) {
				if tool["title"] != "kept" || tool["annotations"] == nil || tool["_meta"] == nil {
					t.Errorf("static fields lost: %v", tool)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := tc.static
			resolved := tc.resolved
			meta.Resolve = func(*agocontext.Context) ToolMeta { return resolved }
			s, err := NewServer(ServerConfig{
				Ask:      func(context.Context, string) (string, error) { return "", nil },
				Tools:    []tools.Tool{forecastTool(t)},
				ToolMeta: map[string]ToolMeta{"forecast": meta},
			})
			if err != nil {
				t.Fatal(err)
			}
			ts := httptest.NewServer(s.Handler())
			defer ts.Close()
			rpcOver(t, ts.URL, nil, initMsg)
			list := rpcOver(t, ts.URL, nil, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
			tc.check(t, toolByName(t, list, "forecast"))

			// A resolved output schema decides structuredContent on tools/call.
			call := rpcOver(t, ts.URL, nil,
				`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"forecast","arguments":{"city":"Oslo"}}}`)
			_, got := call["result"].(map[string]any)["structuredContent"]
			if want := tc.name == "outputSchema"; got != want {
				t.Errorf("structuredContent present = %v, want %v", got, want)
			}
		})
	}
}

// merged without a Resolve is the metadata itself, and needs no context.
func TestToolMetaMergedWithoutResolve(t *testing.T) {
	m := ToolMeta{Title: "t", Meta: map[string]any{"k": 1}}
	got := m.merged(nil)
	if got.Title != "t" || !reflect.DeepEqual(got.Meta, m.Meta) {
		t.Fatalf("merged = %+v", got)
	}
}

// Only a result that is exactly one data object is structured: a result with
// further parts next to the object answers text content only.
func TestStructuredContentNeedsExactlyOneDataPart(t *testing.T) {
	mixed, err := tools.NewFunctionTool("mixed", "object plus text",
		func(struct{}) ([]events.Input, error) {
			return []events.Input{events.DataInput{Data: map[string]any{"a": 1}}, events.TextInput{Content: "note"}}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	single, err := tools.NewFunctionTool("single", "one object",
		func(struct{}) ([]events.Input, error) {
			return []events.Input{events.DataInput{Data: map[string]any{"a": 1}}}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	schema := map[string]any{"type": "object"}
	s, err := NewServer(ServerConfig{
		Ask:   func(context.Context, string) (string, error) { return "", nil },
		Tools: []tools.Tool{mixed, single},
		ToolMeta: map[string]ToolMeta{
			"mixed": {OutputSchema: schema}, "single": {OutputSchema: schema},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	rpcOver(t, ts.URL, nil, initMsg)
	for name, want := range map[string]bool{"mixed": false, "single": true} {
		call := rpcOver(t, ts.URL, nil,
			`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"`+name+`","arguments":{}}}`)
		if _, got := call["result"].(map[string]any)["structuredContent"]; got != want {
			t.Errorf("%s: structuredContent present = %v, want %v (%v)", name, got, want, call)
		}
	}
}
