package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
)

// --- helpers ------------------------------------------------------------------

func newTestContext() (*stream.MemoryStream, *agocontext.Context) {
	s := stream.NewMemoryStream()
	return s, agocontext.New(s)
}

func mustFunctionTool(t *testing.T, name, description string, fn any, opts ...FunctionToolOption) *FunctionTool {
	t.Helper()
	ft, err := NewFunctionTool(name, description, fn, opts...)
	if err != nil {
		t.Fatalf("NewFunctionTool(%q): %v", name, err)
	}
	return ft
}

func schemaJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	return string(b)
}

// partJSONWithoutTimestamp marshals an input part and drops the zero "created_at"
// the Base struct carries (result parts are never stamped by the stream).
func partJSONWithoutTimestamp(t *testing.T, p events.Input) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(schemaJSON(t, p)), &m); err != nil {
		t.Fatalf("unmarshal part: %v", err)
	}
	delete(m, "created_at")
	return schemaJSON(t, m)
}

// --- interface conformance ----------------------------------------------------

var (
	_ Tool       = (*FunctionTool)(nil)
	_ Tool       = (*Toolkit)(nil)
	_ Tool       = (*ClientTool)(nil)
	_ ToolSchema = Schema{}
)

// --- Schema -------------------------------------------------------------------

func TestSchemaJSONTags(t *testing.T) {
	s := Schema{Name: "t", Description: "d", Parameters: map[string]any{"type": "object"}, DeferLoading: true}
	got := schemaJSON(t, s)
	want := `{"name":"t","description":"d","parameters":{"type":"object"},"defer_loading":true}`
	if got != want {
		t.Fatalf("schema JSON = %s, want %s", got, want)
	}
	s.DeferLoading = false
	if strings.Contains(schemaJSON(t, s), "defer_loading") {
		t.Fatalf("defer_loading=false must be omitted, got %s", schemaJSON(t, s))
	}
}

func TestSchemaSchemaType(t *testing.T) {
	if got := (Schema{}).SchemaType(); got != "function" {
		t.Fatalf("SchemaType = %q, want function", got)
	}
}

// --- NewFunctionTool: schema generation ---------------------------------------

// specArgs mirrors the spec example my_tool(a: str, b: int).
type specArgs struct {
	A string `json:"a"`
	B int    `json:"b"`
}

// wantParams asserts a JSON round-trip equality so map key order never matters.
func wantParams(t *testing.T, got map[string]any, want string) {
	t.Helper()
	var wantMap map[string]any
	if err := json.Unmarshal([]byte(want), &wantMap); err != nil {
		t.Fatalf("test bug: bad want JSON: %v", err)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal parameters: %v", err)
	}
	var gotMap map[string]any
	if err := json.Unmarshal(gotJSON, &gotMap); err != nil {
		t.Fatalf("unmarshal parameters: %v", err)
	}
	if !reflect.DeepEqual(gotMap, wantMap) {
		t.Fatalf("parameters = %s, want %s", gotJSON, want)
	}
}

func TestFunctionToolSchemaFromSpecExample(t *testing.T) {
	ft := mustFunctionTool(t, "my_tool", "Tool description.", func(a specArgs) string { return "" })
	schemas, err := ft.Schemas(nil)
	if err != nil {
		t.Fatalf("Schemas: %v", err)
	}
	if len(schemas) != 1 {
		t.Fatalf("len(schemas) = %d, want 1", len(schemas))
	}
	s, ok := schemas[0].(Schema)
	if !ok {
		t.Fatalf("schema type %T, want tools.Schema", schemas[0])
	}
	if s.Name != "my_tool" || s.Description != "Tool description." || s.DeferLoading {
		t.Fatalf("schema identity = %+v", s)
	}
	wantParams(t, s.Parameters,
		`{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer"}},"required":["a","b"]}`)
}

func TestFunctionToolSchemaNoArguments(t *testing.T) {
	ft := mustFunctionTool(t, "ping", "pings", func() string { return "pong" })
	schemas, _ := ft.Schemas(nil)
	wantParams(t, schemas[0].(Schema).Parameters, `{"type":"object","properties":{}}`)
}

func TestFunctionToolSchemaPointerFieldOptional(t *testing.T) {
	type args struct {
		A string `json:"a"`
		B *int   `json:"b"`
	}
	ft := mustFunctionTool(t, "opt", "", func(a args) string { return a.A })
	schemas, _ := ft.Schemas(nil)
	params := schemas[0].(Schema).Parameters
	req := fmt.Sprint(params["required"])
	if req != "[a]" {
		t.Fatalf("required = %s, want [a] (pointer fields are optional)", req)
	}
}

func TestFunctionToolSchemaExplicitAgschemaWins(t *testing.T) {
	type args struct {
		A string `json:"a" agschema:"description=just a"`
	}
	ft := mustFunctionTool(t, "expl", "", func(a args) string { return a.A })
	schemas, _ := ft.Schemas(nil)
	params := schemas[0].(Schema).Parameters
	props := params["properties"].(map[string]any)
	if _, hasDesc := props["a"].(map[string]any)["description"]; !hasDesc {
		t.Fatalf("explicit agschema description lost: %v", props)
	}
	if req := fmt.Sprint(params["required"]); req != "[]" {
		t.Fatalf("required = %s, want [] (field carries its own agschema tag)", req)
	}
}

func TestFunctionToolWithToolSchemaOverrides(t *testing.T) {
	override := map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "boolean"}}}
	ft := mustFunctionTool(t, "over", "", func(a specArgs) string { return "" }, WithToolSchema(override))
	schemas, _ := ft.Schemas(nil)
	params := schemas[0].(Schema).Parameters
	if _, ok := params["properties"].(map[string]any)["x"]; !ok {
		t.Fatalf("explicit schema not used: %v", params)
	}
	// Mutating the caller's map must not change the stored schema.
	override["properties"] = nil
	if _, ok := params["properties"].(map[string]any)["x"]; !ok {
		t.Fatalf("stored schema aliased the caller's map")
	}
}

// --- NewFunctionTool: validation ----------------------------------------------

func TestNewFunctionToolValidation(t *testing.T) {
	cases := []struct {
		label    string
		toolName string
		fn       any
		want     string
	}{
		{"empty name", "", func() string { return "" }, "name must not be empty"},
		{"not a func", "t", "nope", "fn must be a func"},
		{"no returns", "t", func() {}, "must return"},
		{"three returns", "t", func() (int, error, error) { return 0, nil, nil }, "must return"},
		{"bad second return", "t", func() (int, int) { return 0, 0 }, "second return value must be error"},
		{"variadic", "t", func(a specArgs, rest ...int) string { return "" }, "variadic"},
		{"flat param", "t", func(a string) string { return a }, "bind from JSON by field name"},
		{"two structs", "t", func(a specArgs, b specArgs) string { return "" }, "at most one struct argument"},
	}
	for _, tc := range cases {
		_, err := NewFunctionTool(tc.toolName, "", tc.fn)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want containing %q", tc.label, err, tc.want)
		}
	}
}

func TestNewFunctionToolEmptyName(t *testing.T) {
	_, err := NewFunctionTool("", "d", func() string { return "" })
	if err == nil || !strings.Contains(err.Error(), "name must not be empty") {
		t.Fatalf("err = %v, want empty-name error", err)
	}
}

// --- DI -----------------------------------------------------------------------

type diArgs struct {
	Q      string `json:"q"`
	APIKey Inject `aginject:"api_key"`
	Region string `agvariable:"region" agdefault:"eu"`
}

func TestDependencyInjectionViaTagsAndTypes(t *testing.T) {
	s, cx := newTestContext()
	cx.SetDependency("api_key", "sk-1")

	called := ""
	ft := mustFunctionTool(t, "di", "", func(a diArgs, cx *agocontext.Context) (string, error) {
		called = fmt.Sprintf("%s|%v|%s|id=%s", a.Q, a.APIKey.Default, a.Region, cx.ToolCallID)
		return called, nil
	})
	if _, err := ft.Register(cx, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Send(events.ToolCallEvent{ID: "call-1", Name: "di", Arguments: `{"q":"hi"}`}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	want := "hi|sk-1|eu|id=call-1"
	if called != want {
		t.Fatalf("tool got %q, want %q", called, want)
	}
	// The delivered annotation carries the key and the resolved payload; the
	// declared default was applied without the variable being set.
	if v, ok := cx.Variable("region"); ok && v != "eu" {
		t.Fatalf("unexpected variable materialization on the conversation: %v", v)
	}
}

func TestDIDefaultsFromNewInjectAndNewVariable(t *testing.T) {
	in := NewInject("dep", "fallback")
	if !in.HasDefault || in.Default != "fallback" || in.ID != "dep" {
		t.Fatalf("NewInject = %+v", in)
	}
	v := NewVariable("var")
	if v.HasDefault || v.Name != "var" {
		t.Fatalf("NewVariable = %+v", v)
	}
}

func TestDIMissingDependencyFails(t *testing.T) {
	s, cx := newTestContext()
	ft := mustFunctionTool(t, "needy", "", func(a diArgs) string { return "ok" })
	if _, err := ft.Register(cx, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// A missing dependency is a tool failure, not a transport error: the call is
	// answered with a ToolErrorEvent (Python parity: validation error).
	if err := s.Send(events.ToolCallEvent{ID: "c", Name: "needy", Arguments: "{}"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	hist, err := cx.History()
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	var last events.Event
	for _, ev := range hist {
		last = events.AsValue(ev)
	}
	te, ok := last.(events.ToolErrorEvent)
	if !ok {
		t.Fatalf("last event %T, want ToolErrorEvent", last)
	}
	if !strings.Contains(te.Error, `inject "api_key" is not provided`) {
		t.Fatalf("error text = %q", te.Error)
	}
}

func TestDIVariableFromContextWinsAndSchemaExcludesDI(t *testing.T) {
	s, cx := newTestContext()
	cx.SetDependency("api_key", "sk-1")
	cx.SetVariable("region", "us")

	got := ""
	ft := mustFunctionTool(t, "di", "", func(a diArgs) string { got = a.Region; return got })
	if _, err := ft.Register(cx, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Send(events.ToolCallEvent{ID: "c", Name: "di", Arguments: `{"q":"x","region":"spoofed"}`}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got != "us" {
		t.Fatalf("region = %q, want %q (context values beat model-supplied ones)", got, "us")
	}

	schemas, _ := ft.Schemas(nil)
	params := schemas[0].(Schema).Parameters
	props := params["properties"].(map[string]any)
	if _, ok := props["q"]; !ok {
		t.Fatalf("model argument missing from schema: %v", props)
	}
	for _, di := range []string{"APIKey", "Region", "api_key", "region"} {
		if _, ok := props[di]; ok {
			t.Fatalf("DI field %q leaked into the schema: %v", di, props)
		}
	}
	if req := fmt.Sprint(params["required"]); req != "[q]" {
		t.Fatalf("required = %s, want [q]", req)
	}
}

func TestDIInvalidTagsRejectedAtConstruction(t *testing.T) {
	type args struct {
		A string `aginject:"a" agvariable:"a"`
	}
	_, err := NewFunctionTool("t", "", func(x args) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "both aginject and agvariable") {
		t.Fatalf("err = %v, want tag-conflict error", err)
	}
	type args2 struct {
		A Inject `agdefault:"x"`
	}
	_, err = NewFunctionTool("t", "", func(x args2) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "agdefault is not supported") {
		t.Fatalf("err = %v, want agdefault-on-typed error", err)
	}
}

func TestDIStandaloneInjectParamRejected(t *testing.T) {
	// AGO-DEVIATION: Go reflection exposes no parameter names, so a standalone
	// Inject/Variable parameter has no lookup key.
	_, err := NewFunctionTool("t", "", func(dep Inject) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "no name to resolve") {
		t.Fatalf("err = %v, want standalone-DI error", err)
	}
	_, err = NewFunctionTool("t", "", func(dep Variable) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "no name to resolve") {
		t.Fatalf("err = %v, want standalone-DI error", err)
	}
}

// --- return value mapping -----------------------------------------------------

type payload struct {
	K string `json:"k"`
}

func callDirectly(t *testing.T, ft *FunctionTool, cx *agocontext.Context, args string) (events.Event, error) {
	t.Helper()
	if _, err := ft.Register(cx, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	s := cx.Stream
	ch := make(chan events.Event, 8)
	sub := s.Subscribe(func(ev events.Event) error { ch <- ev; return nil }, stream.SubscribeOptions{})
	defer s.Unsubscribe(sub)
	call := events.NewToolCallEvent(ft.Name())
	call.Arguments = args
	if err := s.Send(call); err != nil {
		return nil, err
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			switch v := events.AsValue(ev).(type) {
			case events.ToolResultEvent, events.ToolErrorEvent:
				return v, nil
			}
		case <-deadline:
			t.Fatal("timed out waiting for the tool result")
		}
	}
}

func TestReturnMapping(t *testing.T) {
	cases := []struct {
		name string
		fn   any
		args string
		want string // JSON of the first part
	}{
		{"string", func(a specArgs) string { return "text " + a.A }, `{"a":"x"}`, `{"content":"text x"}`},
		{"struct", func(a specArgs) payload { return payload{K: a.A} }, `{"a":"x"}`, `{"data":{"k":"x"}}`},
		{"slice", func(a specArgs) []events.Input {
			return []events.Input{events.TextInput{Content: "a"}, events.DataInput{Data: 1.0}}
		}, `{}`, `{"content":"a"}`},
		{"tool result", func() events.ToolResult {
			return events.NewToolResult(false, "hi")
		}, `{}`, `{"content":"hi"}`},
		{"nil value", func() any { return nil }, `{}`, `{"data":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, cx := newTestContext()
			ft := mustFunctionTool(t, tc.name, "", tc.fn)
			ev, err := callDirectly(t, ft, cx, tc.args)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			res, ok := ev.(events.ToolResultEvent)
			if !ok {
				t.Fatalf("event type %T, want ToolResultEvent", ev)
			}
			// Compare the part without its Base timestamp: result parts are not
			// stamped by the stream, so the zero time appears in their JSON.
			got := partJSONWithoutTimestamp(t, res.Result.Parts[0])
			if got != tc.want {
				t.Fatalf("first part = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestToolResultPassthroughKeepsFinalAndMetadata(t *testing.T) {
	_, cx := newTestContext()
	ft := mustFunctionTool(t, "passthrough", "", func() events.ToolResult {
		return events.ToolResult{
			Parts:    []events.Input{events.TextInput{Content: "done"}},
			Final:    true,
			Metadata: map[string]any{"k": "v"},
		}
	})
	ev, err := callDirectly(t, ft, cx, "{}")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	res := ev.(events.ToolResultEvent)
	if !res.Result.Final || res.Result.Metadata["k"] != "v" {
		t.Fatalf("ToolResult not passed through: %+v", res.Result)
	}
}

func TestErrorReturnsBecomeToolErrorEvents(t *testing.T) {
	_, cx := newTestContext()
	boom := errors.New("boom")
	ft := mustFunctionTool(t, "failing", "", func(a specArgs) (string, error) {
		if a.A == "bad" {
			return "", boom
		}
		return "ok", nil
	})
	ev, err := callDirectly(t, ft, cx, `{"a":"bad"}`)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	te, ok := ev.(events.ToolErrorEvent)
	if !ok {
		t.Fatalf("event type %T, want ToolErrorEvent", ev)
	}
	if te.Error != "boom" {
		t.Fatalf("error text = %q, want boom", te.Error)
	}
	if len(te.Result.Parts) != 1 {
		t.Fatalf("error result must carry the formatted error text, got %+v", te.Result)
	}

	// error-only function returning nil produces an empty result
	ftNil := mustFunctionTool(t, "silent", "", func() error { return nil })
	ev, err = callDirectly(t, ftNil, cx, "{}")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	res, ok := ev.(events.ToolResultEvent)
	if !ok || len(res.Result.Parts) != 0 {
		t.Fatalf("nil-error return = %#v, want empty ToolResultEvent", ev)
	}
}

func TestPanickingFunctionBecomesToolErrorEvent(t *testing.T) {
	_, cx := newTestContext()
	ft := mustFunctionTool(t, "panic", "", func() string { panic("kaboom") })
	ev, err := callDirectly(t, ft, cx, "{}")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	te, ok := ev.(events.ToolErrorEvent)
	if !ok || !strings.Contains(te.Error, "kaboom") {
		t.Fatalf("event = %#v, want ToolErrorEvent mentioning kaboom", ev)
	}
}

func TestArgumentsMustBeValidJSON(t *testing.T) {
	_, cx := newTestContext()
	ft := mustFunctionTool(t, "json", "", func(a specArgs) string { return a.A })
	ev, err := callDirectly(t, ft, cx, `{"a":`)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if _, ok := ev.(events.ToolErrorEvent); !ok {
		t.Fatalf("event = %#v, want ToolErrorEvent for invalid JSON", ev)
	}
}

// --- source -------------------------------------------------------------------

func TestFunctionToolSource(t *testing.T) {
	ft := mustFunctionTool(t, "src", "", func(a specArgs) string { return "" })
	if !strings.HasPrefix(ft.Source(), "function:") || !strings.HasSuffix(ft.Source(), ":src") {
		t.Fatalf("source = %q, want function:<func>:src", ft.Source())
	}
	ft2 := mustFunctionTool(t, "src", "", func(a specArgs) string { return "" }, WithSource("custom:1"))
	if ft2.Source() != "custom:1" {
		t.Fatalf("WithSource overridden source = %q", ft2.Source())
	}
}

// --- options ------------------------------------------------------------------

func TestWithTimeoutValidation(t *testing.T) {
	_, err := NewFunctionTool("t", "", func(a specArgs) string { return "" }, WithTimeout(-time.Second))
	if err == nil || !strings.Contains(err.Error(), "timeout must not be negative") {
		t.Fatalf("err = %v, want negative-timeout error", err)
	}
}

// --- Toolkit ------------------------------------------------------------------

func TestToolkitDuplicateNameConflicts(t *testing.T) {
	a := mustFunctionTool(t, "dup", "", func() string { return "a" })
	b := mustFunctionTool(t, "dup", "", func() string { return "b" })
	_, err := NewToolkit("tk", nil, a, b)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "dup") {
		t.Fatalf("error text = %v, want the tool name", err)
	}
}

func TestToolkitSchemasConcatenateAndRegisterDelegates(t *testing.T) {
	s, cx := newTestContext()
	a := mustFunctionTool(t, "a", "", func(a specArgs) string { return "" })
	b := mustFunctionTool(t, "b", "", func(a specArgs) string { return "" })
	tk, err := NewToolkit("tk", nil, a, b)
	if err != nil {
		t.Fatalf("NewToolkit: %v", err)
	}
	schemas, err := tk.Schemas(cx)
	if err != nil {
		t.Fatalf("Schemas: %v", err)
	}
	if len(schemas) != 2 {
		t.Fatalf("len(schemas) = %d, want 2", len(schemas))
	}
	if schemas[0].(Schema).Name != "a" || schemas[1].(Schema).Name != "b" {
		t.Fatalf("schema order = %v, want a then b", schemas)
	}
	if !tk.DeclaredInCode() || tk.Name() != "tk" {
		t.Fatalf("toolkit identity: name=%q declared=%v", tk.Name(), tk.DeclaredInCode())
	}

	registered := ""
	ft := mustFunctionTool(t, "member", "", func(x specArgs) string {
		registered = "called"
		return ""
	})
	tk2, _ := NewToolkit("tk2", nil, ft)
	if _, err := tk2.Register(cx, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Send(events.ToolCallEvent{ID: "c", Name: "member", Arguments: "{}"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if registered != "called" {
		t.Fatalf("toolkit Register did not delegate to members")
	}
}

func TestToolkitPrependsItsMiddleware(t *testing.T) {
	var order []string
	rec := func(name string) middleware.Middleware {
		return &recordingMW{label: name, log: &order}
	}
	tk, err := NewToolkit("tk", []middleware.Middleware{rec("tk-mw")},
		mustFunctionTool(t, "member", "", func() string { return "" }, WithMiddleware(rec("tool-mw"))))
	if err != nil {
		t.Fatalf("NewToolkit: %v", err)
	}
	_, cx := newTestContext()
	if _, err := tk.Register(cx, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	s := cx.Stream
	if err := s.Send(events.ToolCallEvent{ID: "c", Name: "member", Arguments: "{}"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	want := []string{"tk-mw", "tool-mw"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("middleware order = %v, want %v", order, want)
	}
}

func TestToolkitEmptyNameDefaults(t *testing.T) {
	tk, err := NewToolkit("", nil)
	if err != nil {
		t.Fatalf("NewToolkit: %v", err)
	}
	if tk.Name() != "toolkit" {
		t.Fatalf("name = %q, want toolkit", tk.Name())
	}
}

// --- ClientTool ---------------------------------------------------------------

func TestClientToolSchemaParsing(t *testing.T) {
	ct, err := NewClientTool(map[string]any{
		"name":          "client_action",
		"description":   "runs on the client",
		"parameters":    map[string]any{"type": "object", "title": "strip me"},
		"defer_loading": true,
	})
	if err != nil {
		t.Fatalf("NewClientTool: %v", err)
	}
	if ct.Name() != "client_action" {
		t.Fatalf("name = %q", ct.Name())
	}
	if ct.DeclaredInCode() {
		t.Fatalf("client tools are runtime-provided")
	}
	schemas, _ := ct.Schemas(nil)
	sc := schemas[0].(Schema)
	if _, has := sc.Parameters["title"]; has {
		t.Fatalf("title not stripped: %v", sc.Parameters)
	}
	if !sc.DeferLoading {
		t.Fatalf("defer_loading lost")
	}
	if _, err := NewClientTool(map[string]any{}); !errors.Is(err, ErrResolution) {
		t.Fatalf("err = %v, want ErrResolution for missing name", err)
	}
}

func TestClientToolCallEmitsClientToolCallEvent(t *testing.T) {
	s, cx := newTestContext()
	ct, err := NewClientTool(map[string]any{"name": "client_action"})
	if err != nil {
		t.Fatalf("NewClientTool: %v", err)
	}
	if _, err := ct.Register(cx, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	ch := make(chan events.Event, 4)
	s.Subscribe(func(ev events.Event) error { ch <- ev; return nil },
		stream.SubscribeOptions{Condition: events.TypeOf[events.ClientToolCallEvent]()})
	if err := s.Send(events.ToolCallEvent{ID: "cc1", Name: "client_action", Arguments: `{"x":1}`}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case ev := <-ch:
		ce := ev.(events.ClientToolCallEvent)
		if ce.ID != "cc1" || ce.Name != "client_action" || ce.Arguments != `{"x":1}` {
			t.Fatalf("client call = %+v", ce)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no ClientToolCallEvent")
	}
}

// --- recording middleware -----------------------------------------------------

type recordingMW struct {
	middleware.Base
	label string
	log   *[]string
}

func (m *recordingMW) OnToolExecution(next middleware.ToolExecFunc, ev *events.ToolCallEvent, cx *agocontext.Context) (events.Event, error) {
	*m.log = append(*m.log, m.label)
	return next(ev, cx)
}

func TestMiddlewareLayering(t *testing.T) {
	s, cx := newTestContext()
	var order []string
	toolMW := []middleware.Middleware{
		&recordingMW{label: "tool0", log: &order},
		&recordingMW{label: "tool1", log: &order},
	}
	agentMW := []middleware.Middleware{
		&recordingMW{label: "agent0", log: &order},
		&recordingMW{label: "agent1", log: &order},
	}
	ft := mustFunctionTool(t, "layered", "", func() string { return "" }, WithMiddleware(toolMW...))
	if _, err := ft.Register(cx, agentMW); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Send(events.ToolCallEvent{ID: "c", Name: "layered", Arguments: "{}"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	want := []string{"agent0", "agent1", "tool0", "tool1"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v (agent middleware outermost, tool middleware innermost)", order, want)
	}
}
