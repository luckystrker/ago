package spec

import (
	stdcontext "context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/response"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
)

func mustTool(t *testing.T, name string) *tools.FunctionTool {
	t.Helper()
	type args struct {
		Input string `json:"input"`
	}
	ft, err := tools.NewFunctionTool(name, "Test tool "+name, func(a args) (string, error) {
		return "echo:" + a.Input, nil
	})
	if err != nil {
		t.Fatalf("NewFunctionTool(%q): %v", name, err)
	}
	return ft
}

func toolNames(ts []tools.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name())
	}
	return out
}

// newSourceAgent builds the agent the round-trip starts from: two tools,
// multi-fragment prompt.
func newSourceAgent(t *testing.T, ts ...tools.Tool) *agent.Agent {
	t.Helper()
	opts := []agent.Option{
		agent.WithPromptFragments("You are a helper.", "Be concise."),
	}
	if len(ts) > 0 {
		opts = append(opts, agent.WithTools(ts...))
	}
	return agent.New("worker", "Base prompt.", opts...)
}

func TestFromAgent(t *testing.T) {
	src := newSourceAgent(t, mustTool(t, "lookup"), mustTool(t, "record"))
	s, err := FromAgent(src)
	if err != nil {
		t.Fatalf("FromAgent: %v", err)
	}
	if s.Name != "worker" {
		t.Errorf("Name = %q, want %q", s.Name, "worker")
	}
	wantPrompt := []string{"Base prompt.", "You are a helper.", "Be concise."}
	if !reflect.DeepEqual(s.Prompt, wantPrompt) {
		t.Errorf("Prompt = %v, want %v", s.Prompt, wantPrompt)
	}
	wantTools := []string{"lookup", "record"}
	if !reflect.DeepEqual(s.ToolNames, wantTools) {
		t.Errorf("ToolNames = %v, want %v", s.ToolNames, wantTools)
	}
	// The source agent has no response schema, so the spec carries none.
	if s.ResponseSchema != nil {
		t.Errorf("ResponseSchema = %+v, want nil", s.ResponseSchema)
	}
}

func TestMarshalJSONWireKeys(t *testing.T) {
	s := &AgentSpec{
		Name:      "worker",
		Prompt:    []string{"a", "b"},
		ToolNames: []string{"lookup"},
		ResponseSchema: &SchemaSpec{
			Name:        "Answer",
			Description: "structured answer",
			Schema:      map[string]any{"type": "object"},
		},
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Python wire keys: snake_case, schema under json_schema.
	for _, key := range []string{`"name":"worker"`, `"prompt":["a","b"]`, `"tool_names":["lookup"]`, `"json_schema"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("wire JSON missing %s: %s", key, b)
		}
	}

	// Zero-value spec: pydantic-style lists (never null) and null schema.
	b, err = json.Marshal(&AgentSpec{})
	if err != nil {
		t.Fatalf("marshal zero: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(b, &generic); err != nil {
		t.Fatalf("unmarshal zero: %v", err)
	}
	if v, ok := generic["prompt"].([]any); !ok || len(v) != 0 {
		t.Errorf("zero spec prompt = %v, want empty list", generic["prompt"])
	}
	if v, ok := generic["tool_names"].([]any); !ok || len(v) != 0 {
		t.Errorf("zero spec tool_names = %v, want empty list", generic["tool_names"])
	}
	if _, ok := generic["response_schema"]; !ok {
		t.Error("zero spec must still carry the response_schema key")
	}
}

func TestUnmarshalPythonPayload(t *testing.T) {
	// A payload as pydantic would emit it: null schema, unknown extra key.
	payload := `{"name":"worker","prompt":["You are a helper."],"tool_names":["lookup"],` +
		`"response_schema":null,"future_field":1}`
	var s AgentSpec
	if err := json.Unmarshal([]byte(payload), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.Name != "worker" || len(s.Prompt) != 1 || len(s.ToolNames) != 1 || s.ResponseSchema != nil {
		t.Fatalf("decoded spec = %+v", s)
	}

	payload = `{"name":"worker","prompt":[],"tool_names":[],
		"response_schema":{"name":"Answer","description":"d","json_schema":{"type":"object"}}}`
	if err := json.Unmarshal([]byte(payload), &s); err != nil {
		t.Fatalf("unmarshal schema payload: %v", err)
	}
	if s.ResponseSchema == nil {
		t.Fatal("response_schema not decoded")
	}
	if s.ResponseSchema.Name != "Answer" || s.ResponseSchema.Description != "d" {
		t.Errorf("schema spec = %+v", s.ResponseSchema)
	}
	if !reflect.DeepEqual(s.ResponseSchema.Schema, map[string]any{"type": "object"}) {
		t.Errorf("schema = %v", s.ResponseSchema.Schema)
	}

	if err := json.Unmarshal([]byte("{not json"), &s); err == nil {
		t.Error("malformed payload must fail")
	}
}

// TestRoundTrip covers the ask's contract: From → JSON → To against a pool of
// two tools, then To with one tool missing from the pool.
func TestRoundTrip(t *testing.T) {
	lookup := mustTool(t, "lookup")
	record := mustTool(t, "record")
	src := newSourceAgent(t, lookup, record)

	s, err := FromAgent(src)
	if err != nil {
		t.Fatalf("FromAgent: %v", err)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded AgentSpec
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(s, &decoded) {
		t.Fatalf("round trip changed the spec:\n got %+v\nwant %+v", &decoded, s)
	}

	pool := map[string]tools.Tool{
		lookup.Name(): lookup,
		record.Name(): record,
	}
	built, err := decoded.ToAgent(pool)
	if err != nil {
		t.Fatalf("ToAgent: %v", err)
	}
	if built.Name() != "worker" {
		t.Errorf("rebuilt name = %q, want %q", built.Name(), "worker")
	}
	if got := toolNames(built.Tools()); !reflect.DeepEqual(got, []string{"lookup", "record"}) {
		t.Errorf("rebuilt tools = %v, want [lookup record]", got)
	}
	// Prompt fragments survive as one joined fragment (same lines).
	want := strings.Join(src.PromptFragments(), "\n")
	if got := built.PromptFragments(); len(got) != 1 || got[0] != want {
		t.Errorf("rebuilt prompt = %q, want %q", got, want)
	}

	// One of the spec's tools is absent from the pool → resolution error.
	missing := &AgentSpec{Name: "worker", ToolNames: []string{"lookup", "ghost"}}
	_, err = missing.ToAgent(pool)
	if err == nil {
		t.Fatal("ToAgent with a missing tool must fail")
	}
	if !errors.Is(err, tools.ErrResolution) {
		t.Errorf("err = %v, want wrapping tools.ErrResolution", err)
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("err should name the missing tool: %v", err)
	}
}

// TestToAgentRebuiltAgentAsks proves the reconstructed agent is functional:
// a scripted config answer flows back through a full ask.
func TestToAgentRebuiltAgentAsks(t *testing.T) {
	lookup := mustTool(t, "lookup")
	src := agent.New("worker", "You are a helper.", agent.WithTools(lookup))
	s, err := FromAgent(src)
	if err != nil {
		t.Fatalf("FromAgent: %v", err)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded AgentSpec
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cfg := agotest.NewTestConfig(&events.ModelResponse{
		Message: &events.ModelMessage{Content: "rebuilt and answering"},
	})
	built, err := decoded.ToAgent(map[string]tools.Tool{lookup.Name(): lookup}, agent.WithConfig(cfg))
	if err != nil {
		t.Fatalf("ToAgent: %v", err)
	}
	reply, err := built.Ask(stdcontext.Background(), "hi")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if reply.Body() != "rebuilt and answering" {
		t.Errorf("body = %q", reply.Body())
	}
}

// verdict is the structured answer the schema round-trip test captures.
type verdict struct {
	OK     bool   `json:"ok" agschema:"required"`
	Reason string `json:"reason"`
}

// TestFromAgentCapturesResponseSchema pins that a schema-bearing agent round
// trips through the spec: FromAgent reads the schema through the agent's public
// introspection view and ToAgent puts a schema back on the rebuilt agent.
func TestFromAgentCapturesResponseSchema(t *testing.T) {
	schema := response.NewSchema[verdict]("Verdict", "a yes/no answer")
	src := agent.New("judge", "Judge.", agent.WithResponseSchema(schema))

	s, err := FromAgent(src)
	if err != nil {
		t.Fatalf("FromAgent: %v", err)
	}
	if s.ResponseSchema == nil {
		t.Fatal("FromAgent dropped the response schema")
	}
	if s.ResponseSchema.Name != "Verdict" || s.ResponseSchema.Description != "a yes/no answer" {
		t.Fatalf("schema spec = %+v", s.ResponseSchema)
	}
	if !reflect.DeepEqual(s.ResponseSchema.Schema, schema.JSONSchema()) {
		t.Fatalf("schema = %v, want %v", s.ResponseSchema.Schema, schema.JSONSchema())
	}

	// The wire form survives a round trip unchanged.
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded AgentSpec
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	built, err := decoded.ToAgent(nil)
	if err != nil {
		t.Fatalf("ToAgent: %v", err)
	}
	restored := built.ResponseSchema()
	if restored == nil {
		t.Fatal("ToAgent did not restore the response schema")
	}
	if restored.Name() != "Verdict" || restored.Description() != "a yes/no answer" {
		t.Fatalf("restored schema = %q/%q", restored.Name(), restored.Description())
	}
	if !reflect.DeepEqual(restored.JSONSchema(), schema.JSONSchema()) {
		// The wire round trip turns typed slices ([]string) into []any, so compare
		// the JSON form.
		got, _ := json.Marshal(restored.JSONSchema())
		want, _ := json.Marshal(schema.JSONSchema())
		if string(got) != string(want) {
			t.Fatalf("restored JSON schema = %s, want %s", got, want)
		}
	}
	// A spec carries data, not code: the restored schema is a RawSchema, so it
	// passes the text through instead of validating it (Python parity).
	if v, err := restored.Validate(`{"ok":true}`, nil); err != nil || v != `{"ok":true}` {
		t.Fatalf("restored Validate = %v, %v; want the raw text", v, err)
	}
}

// TestFromAgentIgnoresPromptOnlySchema pins that a schema with no JSON schema
// (prompt-driven) is not captured: there is nothing to put on a spec.
func TestFromAgentIgnoresPromptOnlySchema(t *testing.T) {
	prompted := response.NewPromptedSchema(response.NewSchema[verdict]("Verdict", ""))
	src := agent.New("judge", "Judge.", agent.WithResponseSchema(prompted))
	s, err := FromAgent(src)
	if err != nil {
		t.Fatalf("FromAgent: %v", err)
	}
	if s.ResponseSchema != nil {
		t.Fatalf("ResponseSchema = %+v, want nil for a prompt-only schema", s.ResponseSchema)
	}
}

// TestToAgentExplicitSchemaWins pins the precedence: an explicit
// WithResponseSchema at reconstruction time overrides the spec's schema
// (Python parity: explicit response_schema > spec).
func TestToAgentExplicitSchemaWins(t *testing.T) {
	s := &AgentSpec{
		Name: "judge",
		ResponseSchema: &SchemaSpec{
			Name: "FromSpec", Schema: map[string]any{"type": "object"},
		},
	}
	explicit := response.NewSchema[verdict]("Explicit", "")
	built, err := s.ToAgent(nil, agent.WithResponseSchema(explicit))
	if err != nil {
		t.Fatalf("ToAgent: %v", err)
	}
	if got := built.ResponseSchema(); got != response.Proto(explicit) {
		t.Fatalf("response schema = %v, want the explicit one", got.Name())
	}

	// Without an explicit schema the spec's schema is used.
	built, err = s.ToAgent(nil)
	if err != nil {
		t.Fatalf("ToAgent: %v", err)
	}
	if got := built.ResponseSchema(); got == nil || got.Name() != "FromSpec" {
		t.Fatalf("response schema = %v, want the spec's FromSpec", got)
	}
}

// TestSchemaSpecResponseProto pins the wire-spec conversion on its own,
// including the empty-name default and a nil receiver.
func TestSchemaSpecResponseProto(t *testing.T) {
	p := (&SchemaSpec{Schema: map[string]any{"type": "string"}}).ResponseProto()
	if p == nil || p.Name() != "ResponseSchema" {
		t.Fatalf("ResponseProto = %v, want the default name", p)
	}
	var missing *SchemaSpec
	if missing.ResponseProto() != nil {
		t.Fatal("a nil SchemaSpec must convert to nil")
	}
}
