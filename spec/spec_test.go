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
	// AGO-DEVIATION (documented): the agent's response schema is not publicly
	// readable, so FromAgent always leaves ResponseSchema nil.
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
