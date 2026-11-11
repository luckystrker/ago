package a2a

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	agoevents "github.com/luckystrker/ago/events"
)

// Full AG2 history over the wire (parity: ag2/ag2/a2a/mappers/history.py —
// events_to_payload / payload_to_events, carried in an ag2.history+json data
// part). The A2A server is stateless on AG2 history: a client ships the whole
// conversation it has seen up to the current turn, and the server re-hydrates
// it per request, so any replica can serve any turn.
//
// The payload is a list of kind-tagged event entries. Transient events
// (streaming chunks, reasoning deltas, lifecycle bookkeeping) are not durable
// conversation state and are dropped; an unknown event kind is skipped on both
// sides so a newer peer's additions never break an older one.

// History entry kinds (parity: mappers/history.py KIND_* constants).
const (
	HistoryKindUserInput     = "user_input"
	HistoryKindToolCall      = "tool_call"
	HistoryKindToolCalls     = "tool_calls"
	HistoryKindToolResult    = "tool_result"
	HistoryKindToolResults   = "tool_results"
	HistoryKindAgentMessage  = "agent_message"
	HistoryKindModelResponse = "model_response"
)

// History input part discriminators (parity: mappers/history.py _*_PART).
const (
	HistoryPartText   = "text"
	HistoryPartBinary = "binary"
	HistoryPartURL    = "url"
	HistoryPartFileID = "file_id"
	HistoryPartData   = "data"
)

// HistoryPayload is the body of one ag2.history+json data part.
type HistoryPayload struct {
	Events []HistoryEvent `json:"events"`
}

// HistoryEvent is one conversation entry. Kind selects which of the other
// fields apply; the rest stay empty and are omitted on the wire.
//
// The tool_result kind flattens the result onto the entry (parent_id, name,
// content, error) rather than nesting it, matching the payload_to_result_event
// shape the tool-result part shares; an empty Error is omitted where Python
// writes null, which both sides read identically.
type HistoryEvent struct {
	Kind string `json:"kind"`

	// user_input: the input parts of the turn.
	Parts []HistoryInput `json:"parts,omitempty"`
	// tool_calls / model_response: the batch of calls.
	Calls     []ToolCall `json:"calls,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// tool_results: the batch of results.
	Results []HistoryResult `json:"results,omitempty"`

	// tool_call: the call itself, flattened.
	ID string `json:"id,omitempty"`
	// tool_result: the call id this result answers (parent_id on this shape,
	// id on the tool-result part shape — both are read).
	ParentID  string `json:"parent_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// tool_result / model_response / agent_message: the textual content.
	Content string `json:"content,omitempty"`
	Error   string `json:"error,omitempty"`
}

// HistoryInput is one input part of a user turn: the same five flavours the A2A
// part mappers carry. Data holds a base64 string for the binary flavour and the
// raw JSON value for the data flavour (parity: _input_to_dict).
type HistoryInput struct {
	Type           string         `json:"type"`
	Text           string         `json:"text,omitempty"`
	MediaType      string         `json:"media_type,omitempty"`
	Data           any            `json:"data,omitempty"`
	Kind           string         `json:"kind,omitempty"`
	VendorMetadata map[string]any `json:"vendor_metadata,omitempty"`
	URL            string         `json:"url,omitempty"`
	FileID         string         `json:"file_id,omitempty"`
	Filename       string         `json:"filename,omitempty"`
}

// HistoryResult is one result of a tool_results entry.
type HistoryResult struct {
	ParentID string `json:"parent_id,omitempty"`
	Name     string `json:"name,omitempty"`
	Content  string `json:"content,omitempty"`
	Error    string `json:"error,omitempty"`
}

// EventsToPayload serialises a conversation into the ag2.history+json shape:
// transient and unknown events are skipped, everything else keeps its order
// (parity: events_to_payload).
func EventsToPayload(history []agoevents.Event) HistoryPayload {
	out := make([]HistoryEvent, 0, len(history))
	for _, ev := range history {
		if encoded, ok := historyEventFor(ev); ok {
			out = append(out, encoded)
		}
	}
	return HistoryPayload{Events: out}
}

// PayloadToEvents rebuilds conversation events from an ag2.history+json
// payload, skipping entries whose kind this package does not know (parity:
// payload_to_events, which is forward-compatible the same way).
func PayloadToEvents(payload HistoryPayload) []agoevents.Event {
	out := make([]agoevents.Event, 0, len(payload.Events))
	for _, entry := range payload.Events {
		if ev, ok := historyEventFrom(entry); ok {
			out = append(out, ev)
		}
	}
	return out
}

// HistoryPart builds the data part carrying a conversation history; an empty
// history yields nil, so callers can append unconditionally.
func HistoryPart(history []agoevents.Event) *DataPart {
	if len(history) == 0 {
		return nil
	}
	return &DataPart{MimeType: MIMEHistory, Data: EventsToPayload(history)}
}

// AttachHistory appends the ag2.history+json part carrying history to msg
// (parity: build_user_message / build_tool_result_message history_events —
// the full conversation rides on every continuation). A nil or empty history
// leaves msg untouched.
func AttachHistory(msg *Message, history []agoevents.Event) *Message {
	if msg == nil {
		msg = &Message{}
	}
	if part := HistoryPart(history); part != nil {
		msg.Parts = append(msg.Parts, part)
	}
	return msg
}

// ParseHistory decodes every ag2.history+json part of msg, in order, into
// conversation events (parity: parse_message routing MIME_HISTORY parts into
// history_events). A message without such a part yields no events and no
// error.
func ParseHistory(msg *Message) ([]agoevents.Event, error) {
	var out []agoevents.Event
	for _, p := range dataPartsByMIME(msg, MIMEHistory) {
		var payload HistoryPayload
		if err := unmarshalDataPart(p, &payload); err != nil {
			return nil, err
		}
		out = append(out, PayloadToEvents(payload)...)
	}
	return out, nil
}

// MessageOption mutates a message a builder produced, so builders can grow
// without a new positional parameter for every knob.
type MessageOption func(*Message)

// WithHistory makes a message builder ship the full AG2 conversation history
// (parity: history_events on the Python builders' continuation messages).
func WithHistory(history []agoevents.Event) MessageOption {
	return func(m *Message) { AttachHistory(m, history) }
}

// applyMessageOptions runs opts against msg in order.
func applyMessageOptions(msg *Message, opts []MessageOption) *Message {
	for _, opt := range opts {
		if opt != nil {
			opt(msg)
		}
	}
	return msg
}

// historyEventFor encodes one AG2 event, reporting false for events that are
// not durable conversation state (parity: _event_to_dict returning None).
func historyEventFor(ev agoevents.Event) (HistoryEvent, bool) {
	switch e := ev.(type) {
	case *agoevents.ModelRequest:
		return historyEventForRequest(e.Parts), true
	case agoevents.ModelRequest:
		return historyEventForRequest(e.Parts), true
	case *agoevents.ToolCallsEvent:
		return HistoryEvent{Kind: HistoryKindToolCalls, Calls: toolCallsPayload(e.Calls)}, true
	case agoevents.ToolCallsEvent:
		return HistoryEvent{Kind: HistoryKindToolCalls, Calls: toolCallsPayload(e.Calls)}, true
	case *agoevents.ToolResultsEvent:
		return HistoryEvent{Kind: HistoryKindToolResults, Results: toolResultsPayload(e.Results)}, true
	case agoevents.ToolResultsEvent:
		return HistoryEvent{Kind: HistoryKindToolResults, Results: toolResultsPayload(e.Results)}, true
	// A tool error is a tool result with the failure text: check the narrower
	// types first, so the error branch is not lost to the embedded result.
	case *agoevents.ToolNotFoundEvent:
		return toolResultPayload(&e.ToolErrorEvent.ToolResultEvent, e.Error), true
	case agoevents.ToolNotFoundEvent:
		return toolResultPayload(&e.ToolErrorEvent.ToolResultEvent, e.Error), true
	case *agoevents.ToolErrorEvent:
		return toolResultPayload(&e.ToolResultEvent, e.Error), true
	case agoevents.ToolErrorEvent:
		return toolResultPayload(&e.ToolResultEvent, e.Error), true
	case *agoevents.ToolResultEvent:
		return toolResultPayload(e, ""), true
	case agoevents.ToolResultEvent:
		return toolResultPayload(&e, ""), true
	case *agoevents.ToolCallEvent:
		return toolCallPayload(*e), true
	case agoevents.ToolCallEvent:
		return toolCallPayload(e), true
	case *agoevents.ModelResponse:
		return modelResponsePayload(e), true
	case agoevents.ModelResponse:
		return modelResponsePayload(&e), true
	case *agoevents.ModelMessage:
		return HistoryEvent{Kind: HistoryKindAgentMessage, Content: e.Content}, true
	case agoevents.ModelMessage:
		return HistoryEvent{Kind: HistoryKindAgentMessage, Content: e.Content}, true
	}
	return HistoryEvent{}, false
}

// historyEventForRequest encodes a user turn's input parts.
func historyEventForRequest(parts []agoevents.Input) HistoryEvent {
	out := HistoryEvent{Kind: HistoryKindUserInput, Parts: make([]HistoryInput, 0, len(parts))}
	for _, in := range parts {
		out.Parts = append(out.Parts, historyInputPayload(in))
	}
	return out
}

// historyInputPayload encodes one Input (parity: _input_to_dict). An input this
// package does not know is rendered as its text, like Python's str() fallback.
func historyInputPayload(in agoevents.Input) HistoryInput {
	switch e := in.(type) {
	case *agoevents.TextInput:
		return HistoryInput{Type: HistoryPartText, Text: e.Content}
	case agoevents.TextInput:
		return HistoryInput{Type: HistoryPartText, Text: e.Content}
	case *agoevents.BinaryInput:
		return binaryInputPayload(e)
	case agoevents.BinaryInput:
		return binaryInputPayload(&e)
	case *agoevents.UrlInput:
		return HistoryInput{Type: HistoryPartURL, URL: e.URL, Kind: string(e.Kind)}
	case agoevents.UrlInput:
		return HistoryInput{Type: HistoryPartURL, URL: e.URL, Kind: string(e.Kind)}
	case *agoevents.FileIdInput:
		return HistoryInput{Type: HistoryPartFileID, FileID: e.FileID, Filename: e.Filename}
	case agoevents.FileIdInput:
		return HistoryInput{Type: HistoryPartFileID, FileID: e.FileID, Filename: e.Filename}
	case *agoevents.DataInput:
		return HistoryInput{Type: HistoryPartData, Data: e.Data}
	case agoevents.DataInput:
		return HistoryInput{Type: HistoryPartData, Data: e.Data}
	}
	return HistoryInput{Type: HistoryPartText, Text: fmt.Sprint(in)}
}

// binaryInputPayload encodes raw bytes as base64 with the media type and kind
// (parity: _input_to_dict BinaryInput branch).
func binaryInputPayload(e *agoevents.BinaryInput) HistoryInput {
	kind := string(e.Kind)
	if kind == "" {
		kind = string(agoevents.BinaryBinary)
	}
	return HistoryInput{
		Type:           HistoryPartBinary,
		MediaType:      e.MediaType,
		Data:           base64.StdEncoding.EncodeToString(e.Data),
		Kind:           kind,
		VendorMetadata: e.VendorMetadata,
	}
}

// toolResultPayload encodes one tool result, with error when the result failed
// (parity: _tool_result_event_to_dict).
func toolResultPayload(e *agoevents.ToolResultEvent, errText string) HistoryEvent {
	return HistoryEvent{
		Kind:     HistoryKindToolResult,
		ParentID: e.ParentID,
		Name:     e.Name,
		Content:  toolResultText(e.Result),
		Error:    errText,
	}
}

// toolCallsPayload encodes a batch of calls (parity: call_to_payload).
func toolCallsPayload(calls []agoevents.ToolCallEvent) []ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, ToolCall{ID: c.ID, Name: c.Name, Arguments: c.Arguments})
	}
	return out
}

// toolCallPayload encodes one call flattened onto the entry.
func toolCallPayload(c agoevents.ToolCallEvent) HistoryEvent {
	return HistoryEvent{Kind: HistoryKindToolCall, ID: c.ID, Name: c.Name, Arguments: c.Arguments}
}

// modelResponsePayload encodes one model answer and the calls it requested
// (parity: _event_to_dict ModelResponse branch).
func modelResponsePayload(e *agoevents.ModelResponse) HistoryEvent {
	return HistoryEvent{
		Kind:      HistoryKindModelResponse,
		Content:   e.Content(),
		ToolCalls: toolCallsPayload(e.ToolCalls.Calls),
	}
}

// toolResultText renders a tool result as the flat text the wire carries
// (parity: parts.py tool_result_to_text).
//
// AGO-DEVIATION: non-text parts are rendered as their JSON encoding; Python
// falls back to str() on the dataclass, whose repr no peer can parse and which
// loses the payload entirely.
func toolResultText(result agoevents.ToolResult) string {
	var b strings.Builder
	for _, p := range result.Parts {
		switch e := p.(type) {
		case *agoevents.TextInput:
			b.WriteString(e.Content)
			continue
		case agoevents.TextInput:
			b.WriteString(e.Content)
			continue
		}
		data, err := json.Marshal(p)
		if err != nil {
			b.WriteString(fmt.Sprint(p))
			continue
		}
		b.Write(data)
	}
	return b.String()
}

// toolResultsPayload encodes nested result events, keeping the failure text of
// error results.
func toolResultsPayload(results []agoevents.ToolResultEvent) []HistoryResult {
	if len(results) == 0 {
		return nil
	}
	out := make([]HistoryResult, 0, len(results))
	for i := range results {
		errText := ""
		switch e := any(&results[i]).(type) {
		case *agoevents.ToolErrorEvent:
			errText = e.Error
		}
		out = append(out, HistoryResult{
			ParentID: results[i].ParentID,
			Name:     results[i].Name,
			Content:  toolResultText(results[i].Result),
			Error:    errText,
		})
	}
	return out
}

// historyEventFrom decodes one entry into an AG2 event, reporting false for a
// kind this package does not know (parity: _dict_to_event returning None).
func historyEventFrom(entry HistoryEvent) (agoevents.Event, bool) {
	switch entry.Kind {
	case HistoryKindUserInput:
		if len(entry.Parts) == 0 {
			return &agoevents.ModelRequest{}, true
		}
		parts := make([]agoevents.Input, 0, len(entry.Parts))
		for _, raw := range entry.Parts {
			if in, ok := historyInputFrom(raw); ok {
				parts = append(parts, in)
			}
		}
		return &agoevents.ModelRequest{Parts: parts}, true
	case HistoryKindToolCall:
		return payloadToolCall(entry), true
	case HistoryKindToolCalls:
		return &agoevents.ToolCallsEvent{Calls: payloadToolCalls(entry.Calls)}, true
	case HistoryKindToolResult:
		leaf := historyResultEvent(entry)
		if leaf.Error == "" {
			return &leaf.ToolResultEvent, true
		}
		return leaf, true
	case HistoryKindToolResults:
		if len(entry.Results) == 0 {
			return &agoevents.ToolResultsEvent{}, true
		}
		results := make([]agoevents.ToolResultEvent, 0, len(entry.Results))
		for _, raw := range entry.Results {
			leaf := historyResultEvent(HistoryEvent{
				Kind: HistoryKindToolResult, ParentID: raw.ParentID,
				Name: raw.Name, Content: raw.Content, Error: raw.Error,
			})
			results = append(results, leaf.ToolResultEvent)
		}
		return &agoevents.ToolResultsEvent{Results: results}, true
	case HistoryKindModelResponse:
		resp := &agoevents.ModelResponse{ToolCalls: agoevents.ToolCallsEvent{Calls: payloadToolCalls(entry.ToolCalls)}}
		if entry.Content != "" {
			resp.Message = &agoevents.ModelMessage{Content: entry.Content}
		}
		return resp, true
	case HistoryKindAgentMessage:
		return &agoevents.ModelMessage{Content: entry.Content}, true
	}
	return nil, false
}

// payloadToolCall decodes one flattened call: an absent arguments string reads
// as "{}", Python's dict.get default (payload_to_call).
//
// AGO-DEVIATION: an *empty* arguments string cannot be told apart from an
// absent one on this shape, and the encoder omits it (omitempty), so a call
// that carried "" comes back as "{}" where Python round-trips the "". The wire
// spelling "" is an anomaly — AG2 builds calls with "{}" (NewToolCallEvent) —
// and "{}" is the documented default, so the coercion is the safe direction.
func payloadToolCall(entry HistoryEvent) *agoevents.ToolCallEvent {
	args := entry.Arguments
	if args == "" {
		args = "{}"
	}
	return &agoevents.ToolCallEvent{ID: entry.ID, Name: entry.Name, Arguments: args}
}

// payloadToolCalls decodes a batch of calls.
func payloadToolCalls(calls []ToolCall) []agoevents.ToolCallEvent {
	if len(calls) == 0 {
		return nil
	}
	out := make([]agoevents.ToolCallEvent, 0, len(calls))
	for _, c := range calls {
		args := c.Arguments
		if args == "" {
			args = "{}"
		}
		out = append(out, agoevents.ToolCallEvent{ID: c.ID, Name: c.Name, Arguments: args})
	}
	return out
}

// historyResultEvent decodes one tool result; a non-empty error rebuilds the
// error event with the failure text (parity: payload_to_result_event — the
// original error type is not on the wire, only its rendering).
//
// AGO-DEVIATION: the tool_results batch decodes to []ToolResultEvent, which
// cannot hold the narrower ToolErrorEvent, so a failed leaf inside a batch
// keeps its error text in the result content only (parity: Python's list holds
// the ToolErrorEvent subclass). A standalone tool_result entry round-trips as
// the error event.
func historyResultEvent(entry HistoryEvent) *agoevents.ToolErrorEvent {
	parentID := entry.ParentID
	if parentID == "" {
		parentID = entry.ID
	}
	result := agoevents.ToolResult{Parts: []agoevents.Input{agoevents.TextInput{Content: entry.Content}}}
	return &agoevents.ToolErrorEvent{
		ToolResultEvent: agoevents.ToolResultEvent{ParentID: parentID, Name: entry.Name, Result: result},
		Error:           entry.Error,
	}
}

// historyInputFrom decodes one input part, reporting false for an unknown
// flavour (parity: _dict_to_input returning None).
func historyInputFrom(entry HistoryInput) (agoevents.Input, bool) {
	switch entry.Type {
	case HistoryPartText:
		return agoevents.TextInput{Content: entry.Text}, true
	case HistoryPartBinary:
		raw, _ := entry.Data.(string)
		data, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return nil, false
		}
		mediaType := entry.MediaType
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		return agoevents.BinaryInput{
			Data:           data,
			MediaType:      mediaType,
			Kind:           binaryKindOrBinary(entry.Kind),
			VendorMetadata: entry.VendorMetadata,
		}, true
	case HistoryPartURL:
		return agoevents.UrlInput{URL: entry.URL, Kind: binaryKindOrBinary(entry.Kind)}, true
	case HistoryPartFileID:
		return agoevents.FileIdInput{FileID: entry.FileID, Filename: entry.Filename}, true
	case HistoryPartData:
		return agoevents.DataInput{Data: entry.Data}, true
	}
	return nil, false
}

// binaryKindOrBinary reads a binary kind, defaulting to "binary" for an absent
// or unknown spelling (parity: _binary_kind_or_default).
func binaryKindOrBinary(raw string) agoevents.BinaryType {
	switch agoevents.BinaryType(raw) {
	case agoevents.BinaryBinary, agoevents.BinaryImage, agoevents.BinaryAudio,
		agoevents.BinaryDocument, agoevents.BinaryVideo:
		return agoevents.BinaryType(raw)
	}
	return agoevents.BinaryBinary
}
