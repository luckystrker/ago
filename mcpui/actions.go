package mcpui

import (
	"strings"

	"github.com/luckystrker/ago/events"
)

// UI actions are the JSON messages an in-frame UI posts back to its host:
// window.parent.postMessage(payload, '*'). The wire field names are the Python
// attribute names verbatim (toolName, messageId — the mcp-ui-server types define
// them camelCase, so the repo's snake_case rule does not apply here).

// Action types on the wire (Python parity: UIActionType).
const (
	TypeTool   = "tool"
	TypePrompt = "prompt"
	TypeLink   = "link"
	TypeIntent = "intent"
	TypeNotify = "notify"
)

// Action is one UI action: a discriminated (type, payload) pair. MessageID is
// empty on freshly built actions — hosts stamp it when correlating a posted
// action with a pending render (Python parity: GenericActionMessage.messageId,
// dropped from the wire while unset).
type Action struct {
	MessageID string        `json:"messageId,omitempty"`
	Type      string        `json:"type"`
	Payload   ActionPayload `json:"payload"`
}

// ActionPayload is the sealed body of an Action; one payload type per action
// type. Construct actions through ToolCall/Prompt/Link/Intent/Notify.
type ActionPayload interface {
	actionPayload()
}

// ToolCallPayload asks the host to call the MCP tool ToolName with Params.
type ToolCallPayload struct {
	ToolName string         `json:"toolName"`
	Params   map[string]any `json:"params"`
}

func (ToolCallPayload) actionPayload() {}

// PromptPayload sends Prompt into the conversation as a follow-up.
type PromptPayload struct {
	Prompt string `json:"prompt"`
}

func (PromptPayload) actionPayload() {}

// LinkPayload asks the host to open URL.
type LinkPayload struct {
	URL string `json:"url"`
}

func (LinkPayload) actionPayload() {}

// IntentPayload emits the host-defined Intent with Params.
type IntentPayload struct {
	Intent string         `json:"intent"`
	Params map[string]any `json:"params"`
}

func (IntentPayload) actionPayload() {}

// NotificationPayload sends Message to the host as a notification/log.
type NotificationPayload struct {
	Message string `json:"message"`
}

func (NotificationPayload) actionPayload() {}

// ToolCall builds a "tool" action: ask the host to call the MCP tool
// toolName(params). A nil params map rides as {} (Python's params is a dict).
func ToolCall(toolName string, params map[string]any) Action {
	if params == nil {
		params = map[string]any{}
	}
	return Action{Type: TypeTool, Payload: ToolCallPayload{ToolName: toolName, Params: params}}
}

// Prompt builds a "prompt" action: send text into the conversation as a
// follow-up.
func Prompt(text string) Action {
	return Action{Type: TypePrompt, Payload: PromptPayload{Prompt: text}}
}

// Link builds a "link" action: ask the host to open url.
func Link(url string) Action {
	return Action{Type: TypeLink, Payload: LinkPayload{URL: url}}
}

// Intent builds an "intent" action: emit the host-defined intent name with
// params. A nil params map rides as {}.
func Intent(name string, params map[string]any) Action {
	if params == nil {
		params = map[string]any{}
	}
	return Action{Type: TypeIntent, Payload: IntentPayload{Intent: name, Params: params}}
}

// Notify builds a "notify" action: send message to the host as a
// notification/log.
func Notify(message string) Action {
	return Action{Type: TypeNotify, Payload: NotificationPayload{Message: message}}
}

// PostMessage renders action as an HTML-attribute-safe
// window.parent.postMessage(...) call. The payload is serialized the way
// Python's json.dumps would (": " after keys, ", " between items,
// ensure_ascii) and then html-escaped, so quotes and apostrophes in the payload
// cannot terminate the surrounding attribute:
//
//	button := `<button onclick="` + mcpui.PostMessage(action) + `">Add to cart</button>`
func PostMessage(action Action) string {
	payload := actionJSON(action)
	return htmlEscape("window.parent.postMessage(" + payload + ", '*')")
}

// actionJSON serializes the action with the field order pydantic's
// model_dump(exclude_none=True) produces: messageId (only when set), type,
// payload.
func actionJSON(a Action) string {
	var b strings.Builder
	b.WriteByte('{')
	if a.MessageID != "" {
		b.WriteString(pythonString("messageId"))
		b.WriteString(": ")
		b.WriteString(pythonString(a.MessageID))
		b.WriteString(", ")
	}
	b.WriteString(`"type": `)
	b.WriteString(pythonString(a.Type))
	b.WriteString(", ")
	b.WriteString(`"payload": `)
	b.WriteString(payloadJSON(a.Payload))
	b.WriteByte('}')
	return b.String()
}

// payloadJSON serializes the payload with pydantic's field definition order
// (Python parity: mcp_ui_server.types — toolName/params, prompt, url,
// intent/params, message). Every payload carries at least one field, so each
// non-final field is followed by the literal separator.
func payloadJSON(p ActionPayload) string {
	var b strings.Builder
	b.WriteByte('{')
	switch t := p.(type) {
	case ToolCallPayload:
		b.WriteString(`"toolName": `)
		b.WriteString(pythonString(t.ToolName))
		b.WriteString(", ")
		b.WriteString(`"params": `)
		b.WriteString(pythonJSON(t.Params))
	case PromptPayload:
		b.WriteString(`"prompt": `)
		b.WriteString(pythonString(t.Prompt))
	case LinkPayload:
		b.WriteString(`"url": `)
		b.WriteString(pythonString(t.URL))
	case IntentPayload:
		b.WriteString(`"intent": `)
		b.WriteString(pythonString(t.Intent))
		b.WriteString(", ")
		b.WriteString(`"params": `)
		b.WriteString(pythonJSON(t.Params))
	case NotificationPayload:
		b.WriteString(`"message": `)
		b.WriteString(pythonString(t.Message))
	default:
		// Unreachable for a payload built through the constructors; the generic
		// renderer keeps PostMessage total rather than trusting that invariant
		// (its leading brace is already written above).
		b.WriteString(strings.TrimPrefix(pythonJSON(t), "{"))
	}
	b.WriteByte('}')
	return b.String()
}

// WithUIResource appends UI resource blocks to a tool result, turning them into
// DataInput parts the framework carries in the result's content list (Python
// parity: the mcp-ui-server convention "include the UIResource in the content
// array of a toolResult"; AGO-DEVIATION: Python ships no helper function for
// this, only the convention — docs/DECISIONS.md D-044). The result is returned
// modified; parts already present are kept.
func WithUIResource(result events.ToolResult, resources ...Resource) events.ToolResult {
	for _, res := range resources {
		result.Parts = append(result.Parts, events.EnsureInput(res))
	}
	return result
}
