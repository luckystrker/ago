package events

// History-repair helpers (Python parity: ag2/history.py unanswered_tool_calls /
// close_unanswered_tool_calls). A turn killed mid tool-flight leaves model-requested
// calls without results; replaying such a transcript to a provider is invalid, so the
// agent appends sentinel results before the next call.

// SentinelToolResult is the content of the repair result for abandoned tool calls —
// the sentence the model reads (Python parity: history.py HUMAN_INPUT_ABANDONED_TOOL_RESULT).
const SentinelToolResult = "This tool call was not completed: the turn ended because a human-input request could not be answered."

// UnansweredToolCalls returns calls that have no ToolResultEvent, ToolErrorEvent or
// ClientToolCallEvent answer in evs.
func UnansweredToolCalls(evs []Event) []ToolCallEvent {
	answered := map[string]bool{}
	for _, ev := range evs {
		switch e := AsValue(ev).(type) {
		case ToolResultEvent:
			answered[e.ParentID] = true
		case ToolErrorEvent:
			answered[e.ParentID] = true
		case BuiltinToolResultEvent:
			answered[e.ParentID] = true
		case ClientToolCallEvent:
			answered[e.ID] = true
		}
	}
	var out []ToolCallEvent
	for _, ev := range evs {
		switch e := AsValue(ev).(type) {
		case ToolCallEvent:
			if !answered[e.ID] {
				out = append(out, e)
			}
		case BuiltinToolCallEvent:
			if !answered[e.ToolCallEvent.ID] {
				out = append(out, e.ToolCallEvent)
			}
		}
	}
	return out
}

// CloseUnansweredToolCalls returns the sentinel ToolResultEvents that repair evs.
// The caller appends them to the history (streams are append-only).
func CloseUnansweredToolCalls(evs []Event) []ToolResultEvent {
	var out []ToolResultEvent
	for _, call := range UnansweredToolCalls(evs) {
		out = append(out, ToolResultEvent{
			ParentID: call.ID,
			Name:     call.Name,
			Result: ToolResult{
				Parts:    []Input{TextInput{Content: SentinelToolResult}},
				Metadata: map[string]any{"repair": true},
			},
		})
	}
	return out
}
