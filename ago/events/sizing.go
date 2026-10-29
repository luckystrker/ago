package events

import (
	"fmt"
	"strings"
)

// EstimatedTokens estimates the token footprint of an event (Python parity:
// events/sizing.py, chars_per_token default 4; flat budgets image/audio 1000,
// video/document 2000). Used by compaction and token budgets, not for billing.
func EstimatedTokens(ev Event, charsPerToken int) int {
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
		for _, p := range e.Parts {
			est += EstimatedTokens(p, charsPerToken)
		}
	case ModelMessage:
		est = len(e.Content) / charsPerToken
	case ModelMessageChunk:
		est = len(e.Content) / charsPerToken
	case ModelReasoning:
		est = len(e.Content) / charsPerToken
	case ModelResponse:
		est = len(e.Content())/charsPerToken + estimatedUsage(e.Usage)
	case CompactionSummary:
		est = len(e.Content) / charsPerToken
	case ToolCallEvent:
		est = (len(e.Name) + len(e.Arguments)) / charsPerToken
	case ToolResultEvent:
		for _, p := range e.Result.Parts {
			est += EstimatedTokens(p, charsPerToken)
		}
	case ToolErrorEvent:
		est = len(e.Error) / charsPerToken
	case HumanInputRequest:
		est = len(e.Content) / charsPerToken
	case HumanMessage:
		est = len(e.Content) / charsPerToken
	case BuiltinToolResultEvent:
		for _, p := range e.Result.Parts {
			est += EstimatedTokens(p, charsPerToken)
		}
	}
	if est < 1 {
		est = 1
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
	switch e := AsValue(ev).(type) {
	case TextInput:
		return e.Content
	case DataInput:
		return fmt.Sprint(e.Data)
	case ModelRequest:
		parts := make([]string, 0, len(e.Parts))
		for _, p := range e.Parts {
			parts = append(parts, RenderForPrompt(p))
		}
		return strings.Join(parts, "\n")
	case ModelResponse:
		return e.Content()
	case CompactionSummary:
		return "[Summary of earlier conversation]\n" + e.Content
	case ToolCallEvent:
		return fmt.Sprintf("tool call %s(%s)", e.Name, e.Arguments)
	case ToolResultEvent:
		parts := make([]string, 0, len(e.Result.Parts))
		for _, p := range e.Result.Parts {
			parts = append(parts, RenderForPrompt(p))
		}
		return strings.Join(parts, "\n")
	case HumanMessage:
		return "[human] " + e.Content
	default:
		return ""
	}
}
