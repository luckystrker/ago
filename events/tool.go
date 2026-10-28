package events

import (
	"encoding/json"
	"fmt"

	"ago/internal/uuid"
)

// ToolResult is the outcome of a tool execution: a list of input parts, optionally
// forced as the final answer of the turn (Final=true), plus free-form metadata.
type ToolResult struct {
	Parts    []Input        `json:"parts"`
	Final    bool           `json:"final,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

func (t ToolResult) MarshalJSON() ([]byte, error) {
	parts, err := marshalParts(t.Parts)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Parts    []json.RawMessage `json:"parts"`
		Final    bool              `json:"final,omitempty"`
		Metadata map[string]any    `json:"metadata,omitempty"`
	}{parts, t.Final, t.Metadata})
}

func (t *ToolResult) UnmarshalJSON(data []byte) error {
	var w struct {
		Parts    []json.RawMessage `json:"parts"`
		Final    bool              `json:"final"`
		Metadata map[string]any    `json:"metadata"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	parts, err := unmarshalParts(w.Parts)
	if err != nil {
		return err
	}
	t.Parts, t.Final, t.Metadata = parts, w.Final, w.Metadata
	return nil
}

// NewToolResult builds a ToolResult from raw values (strings → TextInput, rest → DataInput).
func NewToolResult(final bool, values ...any) ToolResult {
	r := ToolResult{Final: final, Parts: make([]Input, 0, len(values))}
	for _, v := range values {
		r.Parts = append(r.Parts, EnsureInput(v))
	}
	return r
}

// --- tool call events --------------------------------------------------------

// ToolCallEvent is one model-requested tool invocation. Arguments is raw JSON.
type ToolCallEvent struct {
	Base
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Arguments      string         `json:"arguments"`
	VendorMetadata map[string]any `json:"vendor_metadata,omitempty"`
	Source         string         `json:"source,omitempty"`
}

func (ToolCallEvent) EventType() string    { return "ToolCallEvent" }
func (ToolCallEvent) Transient() bool      { return false }
func (ToolCallEvent) Conversational() bool { return true }

// NewToolCallEvent creates a call with a fresh id and "{}" arguments.
func NewToolCallEvent(name string) ToolCallEvent {
	return ToolCallEvent{ID: uuid.New(), Name: name, Arguments: "{}"}
}

// SerializedArguments parses Arguments into a map.
func (c ToolCallEvent) SerializedArguments() (map[string]any, error) {
	if c.Arguments == "" {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(c.Arguments), &m); err != nil {
		return nil, fmt.Errorf("tool call %s (%s): invalid arguments JSON: %w", c.Name, c.ID, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// HandledBy returns a copy of the call stamped with source (parity: tool.handled_by).
func (c ToolCallEvent) HandledBy(source string) ToolCallEvent {
	c.Source = source
	return c
}

// ToAPI renders the OpenAI wire shape for replay in assistant messages.
func (c ToolCallEvent) ToAPI() map[string]any {
	return map[string]any{
		"id":   c.ID,
		"type": "function",
		"function": map[string]any{
			"name":      c.Name,
			"arguments": c.Arguments,
		},
	}
}

// ToolCallsEvent carries the batch of calls requested by one model response.
type ToolCallsEvent struct {
	Base
	Calls []ToolCallEvent `json:"calls"`
}

func (ToolCallsEvent) EventType() string    { return "ToolCallsEvent" }
func (ToolCallsEvent) Transient() bool      { return false }
func (ToolCallsEvent) Conversational() bool { return false }

// ToolResultsEvent carries the batch of results answering one ToolCallsEvent; it is
// the event that drives the next model call.
type ToolResultsEvent struct {
	Base
	Results []ToolResultEvent `json:"results"`
}

func (ToolResultsEvent) EventType() string    { return "ToolResultsEvent" }
func (ToolResultsEvent) Transient() bool      { return false }
func (ToolResultsEvent) Conversational() bool { return true }

// ToolResultEvent is the answer to one tool call (ParentID = call ID).
type ToolResultEvent struct {
	Base
	ParentID string     `json:"parent_id"`
	Name     string     `json:"name,omitempty"`
	Result   ToolResult `json:"result"`
}

func (ToolResultEvent) EventType() string    { return "ToolResultEvent" }
func (ToolResultEvent) Transient() bool      { return false }
func (ToolResultEvent) Conversational() bool { return true }

// FromCall builds a result event answering the given call.
func FromResultCall(call ToolCallEvent, result ToolResult) ToolResultEvent {
	return ToolResultEvent{ParentID: call.ID, Name: call.Name, Result: result}
}

// ToolErrorEvent reports a tool failure; the result carries the formatted error text.
type ToolErrorEvent struct {
	ToolResultEvent
	Error string `json:"error"`
}

func (ToolErrorEvent) EventType() string    { return "ToolErrorEvent" }
func (ToolErrorEvent) Transient() bool      { return false }
func (ToolErrorEvent) Conversational() bool { return true }

// FromErrorCall builds an error event answering the given call.
func FromErrorCall(call ToolCallEvent, err error) ToolErrorEvent {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	return ToolErrorEvent{
		ToolResultEvent: ToolResultEvent{
			ParentID: call.ID,
			Name:     call.Name,
			Result:   ToolResult{Parts: []Input{TextInput{Content: msg}}},
		},
		Error: msg,
	}
}

// ToolNotFoundEvent reports a call whose name has no registered tool.
type ToolNotFoundEvent struct {
	ToolErrorEvent
}

func (ToolNotFoundEvent) EventType() string    { return "ToolNotFoundEvent" }
func (ToolNotFoundEvent) Transient() bool      { return false }
func (ToolNotFoundEvent) Conversational() bool { return true }

// ClientToolCallEvent marks a call delegated to the client application for execution.
type ClientToolCallEvent struct {
	Base
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func (ClientToolCallEvent) EventType() string    { return "ClientToolCallEvent" }
func (ClientToolCallEvent) Transient() bool      { return false }
func (ClientToolCallEvent) Conversational() bool { return true }

// BuiltinToolCallEvent is a provider-hosted tool call (executed at the provider).
type BuiltinToolCallEvent struct {
	ToolCallEvent
}

func (BuiltinToolCallEvent) EventType() string { return "BuiltinToolCallEvent" }

// BuiltinToolResultEvent is the provider's answer to a hosted tool call.
type BuiltinToolResultEvent struct {
	ToolResultEvent
}

func (BuiltinToolResultEvent) EventType() string { return "BuiltinToolResultEvent" }

func init() {
	RegisterEvent(ToolCallEvent{})
	RegisterEvent(ToolCallsEvent{})
	RegisterEvent(ToolResultsEvent{})
	RegisterEvent(ToolResultEvent{})
	RegisterEvent(ToolErrorEvent{})
	RegisterEvent(ToolNotFoundEvent{})
	RegisterEvent(ClientToolCallEvent{})
	RegisterEvent(BuiltinToolCallEvent{})
	RegisterEvent(BuiltinToolResultEvent{})
}
