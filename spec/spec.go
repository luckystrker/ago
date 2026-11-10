// Package spec carries the JSON-serializable description of an Agent: the
// declarative, data-only parts (name, prompt, tool names, response schema).
// Non-serializable parts (middleware, HITL hook, dependencies, dynamic prompts)
// are excluded by construction and supplied at reconstruction time through
// agent.Option values.
//
// Parity with Python AG2 ag2/spec.py (AgentSpec, ResponseSchemaSpec); the wire
// keys are the Python field names so specs cross the language boundary unchanged.
package spec

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/response"
	"github.com/luckystrker/ago/tools"
)

// SchemaSpec is a wire copy of a response schema (Python parity:
// ResponseSchemaSpec). Schema carries the raw JSON-schema object under the
// Python key `json_schema`.
type SchemaSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Schema      map[string]any `json:"json_schema"`
}

// ResponseProto rebuilds the wire schema as a response.Proto. It is a RawSchema,
// not the original schema object: a spec carries data, not code, so validation
// is not restored and the caller checks the content itself (Python parity:
// ResponseSchemaSpec.to_response_schema).
func (s *SchemaSpec) ResponseProto() response.Proto {
	if s == nil {
		return nil
	}
	opts := []response.RawOption{}
	if s.Name != "" {
		opts = append(opts, response.WithRawName(s.Name))
	}
	if s.Description != "" {
		opts = append(opts, response.WithRawDescription(s.Description))
	}
	return response.RawSchema(s.Schema, opts...)
}

// AgentSpec captures an Agent's declarative state (Python parity: AgentSpec).
type AgentSpec struct {
	Name           string      `json:"name"`
	Prompt         []string    `json:"prompt"`
	ToolNames      []string    `json:"tool_names"`
	ResponseSchema *SchemaSpec `json:"response_schema"`
}

// agentSpecJSON is the wire shape. MarshalJSON normalizes nil lists to empty
// JSON arrays (pydantic emits lists, never null) and UnmarshalJSON accepts the
// pydantic payload verbatim; unknown keys are ignored by encoding/json.
// AGO-DEVIATION: Python warns on unknown spec keys (UnknownSpecFieldWarning);
// a Go library cannot warn, so unknown keys are dropped silently.
type agentSpecJSON struct {
	Name           string      `json:"name"`
	Prompt         []string    `json:"prompt"`
	ToolNames      []string    `json:"tool_names"`
	ResponseSchema *SchemaSpec `json:"response_schema"`
}

// MarshalJSON renders the spec in the Python wire format; nil lists marshal
// as empty arrays and a missing response schema as null.
func (s AgentSpec) MarshalJSON() ([]byte, error) {
	wire := agentSpecJSON{Name: s.Name, Prompt: s.Prompt, ToolNames: s.ToolNames, ResponseSchema: s.ResponseSchema}
	if wire.Prompt == nil {
		wire.Prompt = []string{}
	}
	if wire.ToolNames == nil {
		wire.ToolNames = []string{}
	}
	return json.Marshal(wire)
}

// UnmarshalJSON parses a spec payload (pydantic output parity: missing keys
// fall back to zero values, `"prompt": null` to a nil list).
func (s *AgentSpec) UnmarshalJSON(data []byte) error {
	var wire agentSpecJSON
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("spec: decode AgentSpec: %w", err)
	}
	s.Name = wire.Name
	s.Prompt = wire.Prompt
	s.ToolNames = wire.ToolNames
	s.ResponseSchema = wire.ResponseSchema
	return nil
}

// FromAgent captures the serializable state of a live agent: its name, static
// prompt fragments, tool names and response schema. Dynamic prompts, middleware,
// callbacks and dependencies are dropped (Python parity: AgentSpec.from_agent).
//
// A schema whose JSONSchema is nil (prompt-driven, or one the type generator
// could not map) is captured as no schema: a spec carries wire data only.
func FromAgent(a *agent.Agent) (*AgentSpec, error) {
	ts := a.Tools()
	names := make([]string, 0, len(ts))
	for _, t := range ts {
		if t == nil {
			continue
		}
		names = append(names, t.Name())
	}
	s := &AgentSpec{
		Name:      a.Name(),
		Prompt:    a.PromptFragments(),
		ToolNames: names,
	}
	if rs := a.ResponseSchema(); rs != nil {
		if js := rs.JSONSchema(); js != nil {
			s.ResponseSchema = &SchemaSpec{Name: rs.Name(), Description: rs.Description(), Schema: js}
		}
	}
	return s, nil
}

// ToAgent rebuilds an Agent from the spec. Tools are resolved by name against
// available; the pool is already a flat name→tool index, so toolkit members
// must be listed (or flattened) by the caller (Python flattens a Toolkit while
// building its index). A spec tool missing from the pool fails the call with
// an error wrapping tools.ErrResolution.
//
// A captured response schema is restored as a RawSchema (validation happens in
// the caller, Python parity). Extra opts are applied after it, so an explicit
// agent.WithResponseSchema passed here wins over the spec's schema.
func (s *AgentSpec) ToAgent(available map[string]tools.Tool, opts ...agent.Option) (*agent.Agent, error) {
	resolved := make([]tools.Tool, 0, len(s.ToolNames))
	var missing []string
	for _, name := range s.ToolNames {
		t, ok := available[name]
		if !ok || t == nil {
			missing = append(missing, name)
			continue
		}
		resolved = append(resolved, t)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("spec: could not resolve tool(s) %v; available: %v: %w",
			missing, sortedNames(available), tools.ErrResolution)
	}
	opts = append(opts, agent.WithTools(resolved...))
	if rs := s.ResponseSchema.ResponseProto(); rs != nil {
		opts = append([]agent.Option{agent.WithResponseSchema(rs)}, opts...)
	}
	return agent.New(s.Name, strings.Join(s.Prompt, "\n"), opts...), nil
}

// sortedNames lists the pool keys sorted (Python parity: the error names
// sorted(available)).
func sortedNames(available map[string]tools.Tool) []string {
	names := make([]string, 0, len(available))
	for n := range available {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
