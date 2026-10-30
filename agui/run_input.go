package agui

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
)

// ProtocolVersion is the AG-UI protocol version this server speaks (Python
// parity: ag_ui.core PROTOCOL_VERSION).
const ProtocolVersion = "1.0"

// RunAgentInput is a request to run an agent (Python parity: run_input.py against
// ag_ui.core.RunAgentInput; camelCase wire names, as the SDK's alias generator
// spells them).
type RunAgentInput struct {
	ThreadID        string          `json:"threadId"`
	RunID           string          `json:"runId"`
	ProtocolVersion string          `json:"protocolVersion,omitempty"`
	ParentRunID     string          `json:"parentRunId,omitempty"`
	State           json.RawMessage `json:"state,omitempty"`
	Messages        []Message       `json:"messages"`
	Tools           []Tool          `json:"tools,omitempty"`
	Context         []ContextEntry  `json:"context,omitempty"`
	ForwardedProps  json.RawMessage `json:"forwardedProps,omitempty"`
	Resume          []ResumeEntry   `json:"resume,omitempty"`
}

// Tool is a tool the client offers the agent (executed on the client).
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// ContextEntry is a named piece of ambient information for the run.
type ContextEntry struct {
	Description string `json:"description"`
	Value       string `json:"value"`
}

// ResumeEntry answers one interrupt (Python parity: ag_ui.core.ResumeEntry).
// Honoring one rides on the deferred held-turn machinery; a run whose thread
// holds no interrupt ignores its resume entries with a warning and starts as an
// ordinary run, which is the Python behavior for a server holding no turn.
type ResumeEntry struct {
	InterruptID string         `json:"interruptId"`
	Status      string         `json:"status"`
	Payload     any            `json:"payload,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// The message roles the protocol describes; anything else is stripped with a
// warning before the run is served.
var knownRoles = map[string]bool{
	"developer": true, "system": true, "assistant": true,
	"user": true, "tool": true, "activity": true, "reasoning": true,
}

// Message is one message of the conversation the client sends. One flat struct
// covers every role: fields a role does not carry stay unset. Content is the raw
// JSON — a string, or an array of content parts — decoded on use.
type Message struct {
	ID             string          `json:"id,omitempty"`
	Role           string          `json:"role"`
	Name           string          `json:"name,omitempty"`
	Content        json.RawMessage `json:"content,omitempty"`
	ToolCalls      []ToolCall      `json:"toolCalls,omitempty"`
	ToolCallID     string          `json:"toolCallId,omitempty"`
	Error          string          `json:"error,omitempty"`
	ActivityType   string          `json:"activityType,omitempty"`
	EncryptedValue string          `json:"encryptedValue,omitempty"`
}

// ToolCall is a call an assistant message made.
type ToolCall struct {
	ID             string         `json:"id"`
	Type           string         `json:"type,omitempty"`
	Function       FunctionCall   `json:"function"`
	EncryptedValue string         `json:"encryptedValue,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

// FunctionCall names a tool and carries its arguments as they were written: a
// JSON string, not parsed JSON.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// The content part types and source types the protocol describes.
const (
	PartText     = "text"
	PartImage    = "image"
	PartAudio    = "audio"
	PartVideo    = "video"
	PartDocument = "document"

	SourceData = "data"
	SourceURL  = "url"
	SourceFile = "file"
)

// ContentPart is one part of a message body, discriminated by Type. Source is
// set on the media parts and nil on a text part.
type ContentPart struct {
	Type     string      `json:"type"`
	ID       string      `json:"id,omitempty"`
	Text     string      `json:"text,omitempty"`
	Source   *PartSource `json:"source,omitempty"`
	Metadata any         `json:"metadata,omitempty"`
}

// PartSource is where a media part's bytes come from.
type PartSource struct {
	Type     string `json:"type"`
	Value    string `json:"value"`
	MimeType string `json:"mimeType,omitempty"`
	Provider string `json:"provider,omitempty"`
}

// ReadRunInput parses a request body as the run input it carries (Python parity:
// run_input.py read_run_input). Material this server does not recognise — a
// message role, a content part, a source kind — is dropped with a warning and
// the run is served; a body that is not JSON, or that carries no threadId/runId
// or messages, or a base64 value that does not decode, is an error (the HTTP
// layer answers 400 before any stream exists).
func ReadRunInput(body []byte) (RunAgentInput, error) {
	var input RunAgentInput
	if err := json.Unmarshal(body, &input); err != nil {
		return RunAgentInput{}, fmt.Errorf("agui: run input is not valid JSON: %w", err)
	}
	if input.ThreadID == "" || input.RunID == "" {
		return RunAgentInput{}, fmt.Errorf("agui: run input requires threadId and runId")
	}
	if input.Messages == nil {
		return RunAgentInput{}, fmt.Errorf("agui: run input requires messages")
	}
	kept := input.Messages[:0]
	for i, m := range input.Messages {
		if !knownRoles[m.Role] {
			slog.Warn("agui: stripping a message of unrecognised role from a run input", "path", fmt.Sprintf("/messages/%d", i), "role", m.Role)
			continue
		}
		kept = append(kept, m)
	}
	input.Messages = kept
	if err := checkBase64(input); err != nil {
		return RunAgentInput{}, err
	}
	return input, nil
}

// checkBase64 refuses malformed base64 before the run starts, through the
// transport's error path rather than failing a run the client already sees as
// begun (Python parity: run_input.py _check_base64): inline data on user and
// tool messages, encrypted values on an assistant's tool calls.
func checkBase64(input RunAgentInput) error {
	for i, m := range input.Messages {
		switch m.Role {
		case "user", "tool":
			_, parts, err := decodeContent(m.Content)
			if err != nil {
				return fmt.Errorf("agui: /messages/%d/content: %w", i, err)
			}
			for j, part := range parts {
				if part.Source == nil || part.Source.Type != SourceData {
					continue
				}
				if _, err := base64.StdEncoding.DecodeString(part.Source.Value); err != nil {
					return fmt.Errorf("agui: malformed base64 at /messages/%d/content/%d/source/value", i, j)
				}
			}
		case "assistant":
			for j, call := range m.ToolCalls {
				if call.EncryptedValue == "" {
					continue
				}
				if _, err := base64.StdEncoding.DecodeString(call.EncryptedValue); err != nil {
					return fmt.Errorf("agui: malformed base64 at /messages/%d/toolCalls/%d/encryptedValue", i, j)
				}
			}
		}
	}
	return nil
}

// decodeContent reads a message body: a plain string, or an array of parts.
func decodeContent(raw json.RawMessage) (string, []ContentPart, error) {
	if len(raw) == 0 {
		return "", nil, nil
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return "", nil, err
		}
		return text, nil, nil
	}
	var parts []ContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, err
	}
	return "", parts, nil
}
