package a2a

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The AG2 client-tools extension (parity: ag2/ag2/a2a/extension.py and
// mappers/tools.py). Announced via the URI on the agent card, wire-tagged via
// the vnd.ag2.* media types on data parts. Private to AG2: never sent to
// non-AG2 servers, never inspected by intermediaries.
//
// Semantics kept from the Python executor (executor.py): a turn carrying a
// tool-schemas part makes the server request client-side execution — the task
// parks in input-required with the calls attached — and a turn carrying
// tool-result parts with the same task id resumes it. Client side, the
// drivers on Client (RunClientTools / RunWithHumanInput) execute the calls
// through a callback and loop until the task is terminal.
const (
	ExtensionURI = "urn:ag2:client-tools:v1"

	MIMEToolSchemas = "application/vnd.ag2.tool-schemas+json"
	MIMEToolCall    = "application/vnd.ag2.tool-call+json"
	MIMEToolResult  = "application/vnd.ag2.tool-result+json"
	MIMEHistory     = "application/vnd.ag2.history+json"

	// HeaderA2AExtensions activates extension URIs per request (parity:
	// extension_call_context — the A2A-Extensions service parameter, an HTTP
	// header on jsonrpc/rest; A2A spec: comma-separated URIs).
	HeaderA2AExtensions = "A2A-Extensions"
)

// Wire payload shapes of the extension data parts (parity:
// ag2/ag2/a2a/mappers/tools.py — the wire dicts the Python mappers build).

// ToolSchema announces one tool that lives on the calling side; Parameters is
// the JSON-Schema parameter object the remote model should call it with.
type ToolSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// ToolCall is one invocation the server asks the client to execute;
// Arguments is the arguments object rendered as a JSON string (parity:
// call_to_payload — the wire always carries the string AG2's ToolCallEvent
// holds; an empty string means no arguments).
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolResultEntry is one executed call reported back; ID is the ToolCall.ID.
// Error carries the failure text on a failed call (parity:
// result_event_to_payload — the original error type is lost in transit, only
// the rendered string survives).
type ToolResultEntry struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Content string `json:"content,omitempty"`
	Error   string `json:"error,omitempty"`
}

// ToolSchemasPart builds the data part announcing client-side tools.
func ToolSchemasPart(schemas []ToolSchema) *DataPart {
	return &DataPart{MimeType: MIMEToolSchemas, Data: map[string]any{"tools": schemas}}
}

// ToolCallsPart builds the data part requesting one client-side execution.
func ToolCallsPart(call ToolCall) *DataPart {
	return &DataPart{MimeType: MIMEToolCall, Data: call}
}

// ToolResultsPart builds the data part carrying executed results back.
func ToolResultsPart(results []ToolResultEntry) *DataPart {
	return &DataPart{MimeType: MIMEToolResult, Data: map[string]any{"results": results}}
}

// AttachClientTools stamps msg with the extension URI and appends the
// tool-schemas part (parity: build_user_message with advertise_extension —
// the schemas ride on every opening turn so the stateless server can rebuild
// the tool definitions per execute).
func AttachClientTools(msg *Message, schemas []ToolSchema) *Message {
	if msg == nil {
		msg = &Message{}
	}
	AdvertiseExtension(msg)
	msg.Parts = append(msg.Parts, ToolSchemasPart(schemas))
	return msg
}

// AdvertiseExtension adds ExtensionURI to msg.Extensions, deduplicated
// (parity: _build_message — Message.extensions declares the activated URIs).
func AdvertiseExtension(msg *Message) {
	for _, u := range msg.Extensions {
		if u == ExtensionURI {
			return
		}
	}
	msg.Extensions = append(msg.Extensions, ExtensionURI)
}

// ParseToolSchemas decodes every tool-schemas data part of msg, in order.
func ParseToolSchemas(msg *Message) ([]ToolSchema, error) {
	var out []ToolSchema
	for _, p := range dataPartsByMIME(msg, MIMEToolSchemas) {
		var payload struct {
			Tools []ToolSchema `json:"tools"`
		}
		if err := unmarshalDataPart(p, &payload); err != nil {
			return nil, err
		}
		out = append(out, payload.Tools...)
	}
	return out, nil
}

// ParseToolCalls decodes every tool-call data part of msg, in order.
func ParseToolCalls(msg *Message) ([]ToolCall, error) {
	var out []ToolCall
	for _, p := range dataPartsByMIME(msg, MIMEToolCall) {
		var call ToolCall
		if err := unmarshalDataPart(p, &call); err != nil {
			return nil, err
		}
		if call.ID == "" {
			return nil, fmt.Errorf("%w: %s part without id", ErrWire, MIMEToolCall)
		}
		out = append(out, call)
	}
	return out, nil
}

// ParseToolResults decodes every tool-result data part of msg, in order.
func ParseToolResults(msg *Message) ([]ToolResultEntry, error) {
	var out []ToolResultEntry
	for _, p := range dataPartsByMIME(msg, MIMEToolResult) {
		var payload struct {
			Results []ToolResultEntry `json:"results"`
		}
		if err := unmarshalDataPart(p, &payload); err != nil {
			return nil, err
		}
		for _, r := range payload.Results {
			if r.ID == "" {
				return nil, fmt.Errorf("%w: %s entry without id", ErrWire, MIMEToolResult)
			}
		}
		out = append(out, payload.Results...)
	}
	return out, nil
}

// BuildToolResultMessage builds the continuation message reporting executed
// results for taskID: the results part first, then the schemas again so the
// stateless server can rebuild tool definitions on the resume turn (parity:
// build_tool_result_message — results, schemas, history ride on every
// continuation; history shipping itself is a deferred conversational-client
// feature, D-049/D-056).
func BuildToolResultMessage(taskID, contextID string, results []ToolResultEntry, schemas []ToolSchema) *Message {
	msg := NewMessage(RoleUser, ToolResultsPart(results))
	msg.TaskID = taskID
	msg.ContextID = contextID
	AdvertiseExtension(msg)
	if len(schemas) > 0 {
		msg.Parts = append(msg.Parts, ToolSchemasPart(schemas))
	}
	return msg
}

// NewInputResponseMessage wraps a human answer as the continuation message
// resuming an input-required task (parity: build_input_response_message —
// single text part, existing task id so the server resumes the same task).
// extensions are kept on the continuation: activation is per-request, and
// dropping them mid-task would silently deactivate.
func NewInputResponseMessage(taskID, contextID, text string, extensions []string) *Message {
	msg := NewMessage(RoleUser, NewTextPart(text))
	msg.TaskID = taskID
	msg.ContextID = contextID
	msg.Extensions = dedupeURIs(append(msg.Extensions, extensions...))
	return msg
}

// ValidateExtensionActivation reconciles activated URIs with the card in both
// directions (parity: extension.py validate_extension_activation): a URI the
// card does not advertise is refused, and a required URI the caller neither
// activated nor natively implements (this package implements
// urn:ag2:client-tools:v1) is refused too. The error is ErrExtensionNotSupported.
func ValidateExtensionActivation(card *AgentCard, uris []string) error {
	advertised := make(map[string]AgentExtension, len(card.Capabilities.Extensions))
	for _, ext := range card.Capabilities.Extensions {
		advertised[ext.URI] = ext
	}
	var unknown []string
	for _, u := range uris {
		if _, ok := advertised[u]; !ok {
			unknown = append(unknown, u)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return &ExtensionNotSupportedError{URIs: unknown, Reason: "not advertised by the server card"}
	}
	var unmet []string
	for _, ext := range card.Capabilities.Extensions {
		if !ext.Required || containsURI(uris, ext.URI) || ext.URI == ExtensionURI {
			continue
		}
		unmet = append(unmet, ext.URI)
	}
	if len(unmet) > 0 {
		sort.Strings(unmet)
		return &ExtensionNotSupportedError{URIs: unmet, Reason: "required by the server card; add these URIs to the client"}
	}
	return nil
}

// ExtensionNotSupportedError reports a card/activation extension mismatch
// (parity: errors.py A2AExtensionNotSupportedError). Match with
// errors.As on ErrExtensionNotSupported.
type ExtensionNotSupportedError struct {
	URIs   []string
	Reason string
}

// Error implements error.
func (e *ExtensionNotSupportedError) Error() string {
	return fmt.Sprintf("a2a: extension mismatch (%s): %s", e.Reason, strings.Join(e.URIs, ", "))
}

// dataPartsByMIME returns the data parts of msg carrying the given media
// type, in order (parity: is_data_part_with_mime routing in parse_message).
func dataPartsByMIME(msg *Message, mime string) []*DataPart {
	var out []*DataPart
	for _, p := range msg.Parts {
		if dp, ok := p.(*DataPart); ok && dp.MimeType == mime {
			out = append(out, dp)
		}
	}
	return out
}

// unmarshalDataPart decodes a data part's payload into out.
func unmarshalDataPart(p *DataPart, out any) error {
	raw, err := json.Marshal(p.Data)
	if err != nil {
		return fmt.Errorf("%w: encode data part: %v", ErrWire, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: decode %s part: %v", ErrWire, p.MimeType, err)
	}
	return nil
}

func containsURI(uris []string, uri string) bool {
	for _, u := range uris {
		if u == uri {
			return true
		}
	}
	return false
}

func dedupeURIs(uris []string) []string {
	seen := make(map[string]bool, len(uris))
	out := uris[:0]
	for _, u := range uris {
		if seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}
