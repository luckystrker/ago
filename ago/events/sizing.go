package events

import (
	"fmt"
	"strings"
)

// binaryMediaTokens is the flat budget for one produced file (Python parity:
// sizing.py _MODALITY_TOKENS["binary"]; files yield the "binary" modality).
const binaryMediaTokens = 1000

// EstimatedTokens estimates the token footprint of an event (Python parity:
// events/sizing.py, chars_per_token default 4; flat budgets image/audio 1000,
// video/document 2000). Used by compaction and token budgets, not for billing.
func EstimatedTokens(ev Event, charsPerToken int) int {
	// Telemetry never reaches the model, so it occupies no context budget
	// (Python parity: sizing.py is_conversational → 0, before any floor).
	if !ev.Conversational() {
		return 0
	}
	if charsPerToken <= 0 {
		charsPerToken = 4
	}
	est := 0
	switch e := AsValue(ev).(type) {
	case TextInput:
		est = len(e.Content) / charsPerToken
	case DataInput:
		est = len(fmt.Sprint(e.Data)) / charsPerToken
	case BinaryInput:
		est = len(e.Data)/charsPerToken + 32
	case UrlInput, FileIdInput:
		est = 32
	case ModelRequest:
		est = estimateParts(e.Parts, charsPerToken)
	case DrainedModelRequest:
		// A Go type switch does not reach the embedded ModelRequest the way
		// Python isinstance covers the subclass; size it identically.
		est = estimateParts(e.Parts, charsPerToken)
	case ModelMessage:
		est = len(e.Content) / charsPerToken
	case ModelMessageChunk:
		est = len(e.Content) / charsPerToken
	case ModelReasoning:
		est = len(e.Content) / charsPerToken
	case ModelResponse:
		est = len(e.Content())/charsPerToken + estimatedUsage(e.Usage)
		for _, c := range e.ToolCalls.Calls {
			// "name(arguments)" (Python parity: sizing.py).
			est += (len(c.Name) + len(c.Arguments) + 2) / charsPerToken
		}
		// One flat media budget per produced file (Python parity: sizing.py).
		est += len(e.Files) * binaryMediaTokens
	case CompactionSummary:
		est = len(e.Content) / charsPerToken
	case ToolCallEvent:
		est = (len(e.Name) + len(e.Arguments)) / charsPerToken
	case ToolResultsEvent:
		for _, r := range e.Results {
			est += estimateParts(r.Result.Parts, charsPerToken)
		}
	case ToolResultEvent:
		est = estimateParts(e.Result.Parts, charsPerToken)
	case ToolErrorEvent:
		est = len(e.Error) / charsPerToken
	case HumanInputRequest:
		est = len(e.Content) / charsPerToken
	case HumanMessage:
		est = len(e.Content) / charsPerToken
	case BuiltinToolResultEvent:
		est = estimateParts(e.Result.Parts, charsPerToken)
	}
	if est < 1 {
		est = 1
	}
	return est
}

func estimateParts(parts []Input, charsPerToken int) int {
	est := 0
	for _, p := range parts {
		est += EstimatedTokens(p, charsPerToken)
	}
	return est
}

func estimatedUsage(u Usage) int {
	if u.TotalTokens != nil {
		return int(*u.TotalTokens)
	}
	return int(deref(u.PromptTokens) + deref(u.CompletionTokens))
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// RenderForPrompt renders an event as plain text (used by summarizers and debug views).
func RenderForPrompt(ev Event) string {
	// Telemetry renders empty so it never enters a prompt (Python parity:
	// sizing.py render_for_prompt is_conversational → "").
	if !ev.Conversational() {
		return ""
	}
	switch e := AsValue(ev).(type) {
	case TextInput:
		return e.Content
	case DataInput:
		return fmt.Sprint(e.Data)
	case ModelRequest:
		return "User: " + renderParts(e.Parts)
	case DrainedModelRequest:
		// Same label as ModelRequest (Python parity: sizing.py isinstance labels).
		return "User: " + renderParts(e.Parts)
	case ModelResponse:
		return "Assistant: " + e.Content()
	case CompactionSummary:
		return "[Summary of earlier conversation]\n" + e.Content
	case ToolCallEvent:
		return fmt.Sprintf("tool call %s(%s)", e.Name, e.Arguments)
	case ToolResultsEvent:
		results := make([]string, 0, len(e.Results))
		for _, r := range e.Results {
			results = append(results, renderParts(r.Result.Parts))
		}
		return "Tool: " + strings.Join(results, "\n")
	case ToolResultEvent:
		return renderParts(e.Result.Parts)
	case HumanMessage:
		return "[human] " + e.Content
	default:
		return ""
	}
}

func renderParts(parts []Input) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, RenderForPrompt(p))
	}
	return strings.Join(out, "\n")
}
