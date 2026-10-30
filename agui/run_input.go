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
// A run whose thread holds no interrupt drops its resume entries with a warning
// and starts as an ordinary run; one that holds the named interrupt refuses a
// resume it cannot honour with a coded RUN_ERROR.
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
// run_input.py read_run_input). Keys are accepted in both spellings the wire
// sees — camelCase, as the SDK's alias generator writes them, and snake_case,
// as a hand-written client often does; where both name one field the camelCase
// value wins. Material this server does not recognise — a message role, a
// content part, a source kind, a resume status — is dropped with a warning and
// the run is served; a body that is not JSON, or that carries no threadId/runId
// or messages, or a base64 value that does not decode, is an error (the HTTP
// layer answers 400 before any stream exists).
func ReadRunInput(body []byte) (RunAgentInput, error) {
	wire, err := decodeRunInputWire(body)
	if err != nil {
		return RunAgentInput{}, fmt.Errorf("agui: run input is not valid JSON: %w", err)
	}
	input := wire.input()
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
	input.Resume = knownResumeEntries(input.Resume)
	if err := checkBase64(input); err != nil {
		return RunAgentInput{}, err
	}
	return input, nil
}

// The wire shapes a run input decodes through: every protocol field is read in
// its camelCase spelling and its snake_case alias, camelCase winning when a
// body carries both. An alias struct keeps RunAgentInput itself single-keyed —
// the projection's events must marshal camelCase only.
type runInputWire struct {
	ThreadID         *string         `json:"threadId"`
	ThreadIDAlias    *string         `json:"thread_id"`
	RunID            *string         `json:"runId"`
	RunIDAlias       *string         `json:"run_id"`
	ProtocolVersion  *string         `json:"protocolVersion"`
	ProtocolVersionA *string         `json:"protocol_version"`
	ParentRunID      *string         `json:"parentRunId"`
	ParentRunIDA     *string         `json:"parent_run_id"`
	State            json.RawMessage `json:"state"`
	Messages         []messageWire   `json:"messages"`
	Tools            []Tool          `json:"tools"`
	Context          []ContextEntry  `json:"context"`
	ForwardedProps   json.RawMessage `json:"forwardedProps"`
	Resume           []resumeWire    `json:"resume"`
}

type messageWire struct {
	ID             *string         `json:"id"`
	Role           string          `json:"role"`
	Name           string          `json:"name"`
	Content        json.RawMessage `json:"content"`
	ToolCalls      *[]toolCallWire `json:"toolCalls"`
	ToolCallsAlias *[]toolCallWire `json:"tool_calls"`
	ToolCallID     *string         `json:"toolCallId"`
	ToolCallIDAli  *string         `json:"tool_call_id"`
	Error          string          `json:"error"`
	ActivityType   *string         `json:"activityType"`
	ActivityTypeA  *string         `json:"activity_type"`
}

type toolCallWire struct {
	ID             string         `json:"id"`
	Type           string         `json:"type"`
	Function       FunctionCall   `json:"function"`
	EncryptedValue *string        `json:"encryptedValue"`
	EncryptedValuA *string        `json:"encrypted_value"`
	Metadata       map[string]any `json:"metadata"`
}

type resumeWire struct {
	InterruptID    *string        `json:"interruptId"`
	InterruptIDAli *string        `json:"interrupt_id"`
	Status         string         `json:"status"`
	Payload        any            `json:"payload"`
	Metadata       map[string]any `json:"metadata"`
}

// The resume statuses the protocol describes; an entry of any other status is
// dropped with a warning before the run is served, so the interrupt it claims
// to answer stays uncovered, which the run refuses (Python parity:
// _known_entries).
var knownResumeStatuses = map[string]bool{"resolved": true, "cancelled": true}

func decodeRunInputWire(body []byte) (runInputWire, error) {
	var wire runInputWire
	if err := json.Unmarshal(body, &wire); err != nil {
		return runInputWire{}, err
	}
	return wire, nil
}

func (w runInputWire) input() RunAgentInput {
	input := RunAgentInput{
		State:           w.State,
		Tools:           w.Tools,
		Context:         w.Context,
		ForwardedProps:  w.ForwardedProps,
		ProtocolVersion: first(w.ProtocolVersion, w.ProtocolVersionA),
		ParentRunID:     first(w.ParentRunID, w.ParentRunIDA),
		ThreadID:        first(w.ThreadID, w.ThreadIDAlias),
		RunID:           first(w.RunID, w.RunIDAlias),
	}
	if w.Messages != nil {
		input.Messages = make([]Message, 0, len(w.Messages))
		for _, m := range w.Messages {
			mi := Message{
				ID:         deref(m.ID),
				Role:       m.Role,
				Name:       m.Name,
				Content:    m.Content,
				ToolCallID: first(m.ToolCallID, m.ToolCallIDAli),
				Error:      m.Error,
			}
			if calls := firstCalls(m.ToolCalls, m.ToolCallsAlias); calls != nil {
				mi.ToolCalls = calls
			}
			if at := first(m.ActivityType, m.ActivityTypeA); at != "" {
				mi.ActivityType = at
			}
			input.Messages = append(input.Messages, mi)
		}
	}
	if w.Resume != nil {
		input.Resume = make([]ResumeEntry, 0, len(w.Resume))
		for _, r := range w.Resume {
			input.Resume = append(input.Resume, ResumeEntry{
				InterruptID: first(r.InterruptID, r.InterruptIDAli),
				Status:      r.Status,
				Payload:     r.Payload,
				Metadata:    r.Metadata,
			})
		}
	}
	return input
}

// first is the camelCase value when present, else its alias.
func first(v, alias *string) string {
	if v != nil {
		return *v
	}
	return deref(alias)
}

func deref(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func firstCalls(v, alias *[]toolCallWire) []ToolCall {
	var wire *[]toolCallWire
	switch {
	case v != nil:
		wire = v
	case alias != nil:
		wire = alias
	default:
		return nil
	}
	calls := make([]ToolCall, 0, len(*wire))
	for _, c := range *wire {
		encrypted := ""
		if c.EncryptedValue != nil {
			encrypted = *c.EncryptedValue
		} else if c.EncryptedValuA != nil {
			encrypted = *c.EncryptedValuA
		}
		calls = append(calls, ToolCall{
			ID:             c.ID,
			Type:           c.Type,
			Function:       c.Function,
			EncryptedValue: encrypted,
			Metadata:       c.Metadata,
		})
	}
	return calls
}

// knownResumeEntries drops the resume entries whose status the protocol does
// not describe, with a warning each (Python parity: _known_entries — status is
// required, so an entry without one is kept whole and its interrupt turns out
// uncovered, which the run refuses).
func knownResumeEntries(entries []ResumeEntry) []ResumeEntry {
	kept := entries[:0]
	for i, entry := range entries {
		if !knownResumeStatuses[entry.Status] {
			slog.Warn("agui: stripping a resume entry of unrecognised status from a run input",
				"path", fmt.Sprintf("/resume/%d", i), "status", entry.Status)
			continue
		}
		kept = append(kept, entry)
	}
	return kept
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
