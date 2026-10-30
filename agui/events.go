package agui

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/luckystrker/ago/events"
)

// EventType is the "type" discriminator every AG-UI event carries on the wire.
type EventType string

// The event types this package models. The set is what the Python projection
// emits (ag_ui/stream.py map_events_to_ag_ui) plus RUN_STARTED/RUN_FINISHED/
// RUN_ERROR, which interrupts.py's exchange emits. THINKING_* left the protocol
// in 1.0: reasoning travels as REASONING_*.
const (
	EventRunStarted              EventType = "RUN_STARTED"
	EventRunFinished             EventType = "RUN_FINISHED"
	EventRunError                EventType = "RUN_ERROR"
	EventTextMessageStart        EventType = "TEXT_MESSAGE_START"
	EventTextMessageContent      EventType = "TEXT_MESSAGE_CONTENT"
	EventTextMessageEnd          EventType = "TEXT_MESSAGE_END"
	EventTextMessageChunk        EventType = "TEXT_MESSAGE_CHUNK"
	EventToolCallStart           EventType = "TOOL_CALL_START"
	EventToolCallArgs            EventType = "TOOL_CALL_ARGS"
	EventToolCallEnd             EventType = "TOOL_CALL_END"
	EventToolCallChunk           EventType = "TOOL_CALL_CHUNK"
	EventToolCallResult          EventType = "TOOL_CALL_RESULT"
	EventReasoningStart          EventType = "REASONING_START"
	EventReasoningMessageStart   EventType = "REASONING_MESSAGE_START"
	EventReasoningMessageEnd     EventType = "REASONING_MESSAGE_END"
	EventReasoningMessageContent EventType = "REASONING_MESSAGE_CONTENT"
	EventReasoningEnd            EventType = "REASONING_END"
	EventStateSnapshot           EventType = "STATE_SNAPSHOT"
	EventCustom                  EventType = "CUSTOM"
)

// Error codes this server puts on a RUN_ERROR (Python parity: interrupts.py).
// The interrupt-refusal codes (INTERRUPT_NOT_COVERED and friends) ride on the
// deferred held-turn machinery and are not defined here.
const (
	CodeUnsupportedProtocolVersion = "UNSUPPORTED_PROTOCOL_VERSION"
)

// Event is one AG-UI protocol event. The marker is unexported, so events are
// built in this package or through NewCustomEvent/NewStateSnapshotEvent/Raw.
type Event interface {
	eventType() EventType
}

// baseEvent carries the fields every AG-UI event shares. Plain struct tags give
// the wire form: "type" always present, every optional field omitted when unset
// — the SDK's omit-when-None serialization rule.
type baseEvent struct {
	Type      EventType `json:"type"`
	Timestamp *int64    `json:"timestamp,omitempty"`
}

// TimestampMs is now as the wire spells a timestamp, milliseconds since the
// Unix epoch (Python parity: interrupts.py timestamp_ms).
func TimestampMs() *int64 {
	t := time.Now().UnixMilli()
	return &t
}

// TokenUsage is one provider/model entry of a run's token accounting. Counts are
// totals: reasoning and cache counts break input and output down, never add to
// them (AG-UI 1.0 accounting).
type TokenUsage struct {
	Provider              string `json:"provider,omitempty"`
	Model                 string `json:"model,omitempty"`
	InputTokens           *int64 `json:"inputTokens,omitempty"`
	OutputTokens          *int64 `json:"outputTokens,omitempty"`
	TotalTokens           *int64 `json:"totalTokens,omitempty"`
	ReasoningTokens       *int64 `json:"reasoningTokens,omitempty"`
	CachedInputTokens     *int64 `json:"cachedInputTokens,omitempty"`
	CacheWriteInputTokens *int64 `json:"cacheWriteInputTokens,omitempty"`
}

// Outcome is why a run ended. The interrupt outcome — RUN_FINISHED pausing on a
// question — rides on the deferred held-turn machinery and is not modeled.
type Outcome interface {
	outcomeType() string
}

// SuccessOutcome closes a run that completed. A run that left frontend tool
// calls unanswered names them; absent means the consumer derives the list from
// the stream.
type SuccessOutcome struct {
	PendingToolCallIDs []string `json:"pendingToolCallIds,omitempty"`
}

func (SuccessOutcome) outcomeType() string { return "success" }

// MarshalJSON adds the outcome's "type" discriminator.
func (s SuccessOutcome) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type               string   `json:"type"`
		PendingToolCallIDs []string `json:"pendingToolCallIds,omitempty"`
	}{s.outcomeType(), s.PendingToolCallIDs})
}

// CancelledOutcome closes a run the server stopped before it completed.
type CancelledOutcome struct{}

func (CancelledOutcome) outcomeType() string { return "cancelled" }

// MarshalJSON adds the outcome's "type" discriminator.
func (CancelledOutcome) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type string `json:"type"`
	}{"cancelled"})
}

// --- run lifecycle -----------------------------------------------------------

type RunStartedEvent struct {
	baseEvent
	ThreadID        string `json:"threadId"`
	RunID           string `json:"runId"`
	ProtocolVersion string `json:"protocolVersion,omitempty"`
	ParentRunID     string `json:"parentRunId,omitempty"`
}

func (RunStartedEvent) eventType() EventType { return EventRunStarted }

// NewRunStartedEvent opens a run, stamped with this server's protocol version.
func NewRunStartedEvent(threadID, runID string) RunStartedEvent {
	return RunStartedEvent{
		baseEvent:       baseEvent{Type: EventRunStarted, Timestamp: TimestampMs()},
		ThreadID:        threadID,
		RunID:           runID,
		ProtocolVersion: ProtocolVersion,
	}
}

type RunFinishedEvent struct {
	baseEvent
	ThreadID string       `json:"threadId"`
	RunID    string       `json:"runId"`
	Result   any          `json:"result,omitempty"`
	Outcome  Outcome      `json:"outcome,omitempty"`
	Usage    []TokenUsage `json:"usage,omitempty"`
}

func (RunFinishedEvent) eventType() EventType { return EventRunFinished }

// UnmarshalJSON reads the outcome by its own "type" discriminator; the interrupt
// outcome of the deferred held-turn machinery decodes as nil.
func (e *RunFinishedEvent) UnmarshalJSON(data []byte) error {
	type alias RunFinishedEvent
	var w struct {
		alias
		Outcome json.RawMessage `json:"outcome"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	*e = RunFinishedEvent(w.alias)
	if len(w.Outcome) > 0 && string(w.Outcome) != "null" {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(w.Outcome, &head); err != nil {
			return err
		}
		switch head.Type {
		case "success":
			var o SuccessOutcome
			if err := json.Unmarshal(w.Outcome, &o); err != nil {
				return err
			}
			e.Outcome = o
		case "cancelled":
			e.Outcome = CancelledOutcome{}
		}
	}
	return nil
}

// NewRunFinishedEvent closes a run that did not fail.
func NewRunFinishedEvent(threadID, runID string) RunFinishedEvent {
	return RunFinishedEvent{
		baseEvent: baseEvent{Type: EventRunFinished, Timestamp: TimestampMs()},
		ThreadID:  threadID,
		RunID:     runID,
	}
}

type RunErrorEvent struct {
	baseEvent
	Message string       `json:"message"`
	Code    string       `json:"code,omitempty"`
	Usage   []TokenUsage `json:"usage,omitempty"`
}

func (RunErrorEvent) eventType() EventType { return EventRunError }

// --- text messages -----------------------------------------------------------

type TextMessageStartEvent struct {
	baseEvent
	MessageID string `json:"messageId"`
	Role      string `json:"role,omitempty"`
	Name      string `json:"name,omitempty"`
}

func (TextMessageStartEvent) eventType() EventType { return EventTextMessageStart }

type TextMessageContentEvent struct {
	baseEvent
	MessageID string `json:"messageId"`
	Delta     string `json:"delta"`
}

func (TextMessageContentEvent) eventType() EventType { return EventTextMessageContent }

type TextMessageEndEvent struct {
	baseEvent
	MessageID string `json:"messageId"`
}

func (TextMessageEndEvent) eventType() EventType { return EventTextMessageEnd }

// TextMessageChunkEvent is the shorthand standing in for a start/content/end
// sequence, for content that arrives whole (Python parity: a final ModelMessage
// with no streamed chunks behind it).
type TextMessageChunkEvent struct {
	baseEvent
	MessageID string `json:"messageId,omitempty"`
	Role      string `json:"role,omitempty"`
	Delta     string `json:"delta,omitempty"`
	Name      string `json:"name,omitempty"`
}

func (TextMessageChunkEvent) eventType() EventType { return EventTextMessageChunk }

// --- tool calls --------------------------------------------------------------

type ToolCallStartEvent struct {
	baseEvent
	ToolCallID      string `json:"toolCallId"`
	ToolCallName    string `json:"toolCallName"`
	ParentMessageID string `json:"parentMessageId,omitempty"`
}

func (ToolCallStartEvent) eventType() EventType { return EventToolCallStart }

type ToolCallArgsEvent struct {
	baseEvent
	ToolCallID string `json:"toolCallId"`
	Delta      string `json:"delta"`
}

func (ToolCallArgsEvent) eventType() EventType { return EventToolCallArgs }

type ToolCallEndEvent struct {
	baseEvent
	ToolCallID string `json:"toolCallId"`
}

func (ToolCallEndEvent) eventType() EventType { return EventToolCallEnd }

type ToolCallChunkEvent struct {
	baseEvent
	ToolCallID      string `json:"toolCallId,omitempty"`
	ToolCallName    string `json:"toolCallName,omitempty"`
	ParentMessageID string `json:"parentMessageId,omitempty"`
	Delta           string `json:"delta,omitempty"`
}

func (ToolCallChunkEvent) eventType() EventType { return EventToolCallChunk }

// ToolCallResultEvent carries what a tool returned; it mints a tool message,
// which is why it has its own message id. Content is a string for a lone text
// result, or a list of content parts otherwise.
type ToolCallResultEvent struct {
	baseEvent
	MessageID  string `json:"messageId"`
	ToolCallID string `json:"toolCallId"`
	Content    any    `json:"content"`
	Role       string `json:"role,omitempty"`
}

func (ToolCallResultEvent) eventType() EventType { return EventToolCallResult }

// --- reasoning ----------------------------------------------------------------

type ReasoningStartEvent struct {
	baseEvent
	MessageID string `json:"messageId"`
}

func (ReasoningStartEvent) eventType() EventType { return EventReasoningStart }

// reasoningRole is the one role a REASONING_MESSAGE_START carries; the schema
// makes it required, not defaulted.
const reasoningRole = "reasoning"

type ReasoningMessageStartEvent struct {
	baseEvent
	MessageID string `json:"messageId"`
	Role      string `json:"role"`
}

func (ReasoningMessageStartEvent) eventType() EventType { return EventReasoningMessageStart }

type ReasoningMessageContentEvent struct {
	baseEvent
	MessageID string `json:"messageId"`
	Delta     string `json:"delta"`
}

func (ReasoningMessageContentEvent) eventType() EventType { return EventReasoningMessageContent }

type ReasoningMessageEndEvent struct {
	baseEvent
	MessageID string `json:"messageId"`
}

func (ReasoningMessageEndEvent) eventType() EventType { return EventReasoningMessageEnd }

type ReasoningEndEvent struct {
	baseEvent
	MessageID string `json:"messageId"`
}

func (ReasoningEndEvent) eventType() EventType { return EventReasoningEnd }

// --- state, custom, subagents --------------------------------------------------

// StateSnapshotEvent replaces the agent state wholesale.
type StateSnapshotEvent struct {
	baseEvent
	Snapshot any `json:"snapshot"`
}

func (StateSnapshotEvent) eventType() EventType { return EventStateSnapshot }

// NewStateSnapshotEvent builds a snapshot event stamped now.
func NewStateSnapshotEvent(snapshot any) StateSnapshotEvent {
	return StateSnapshotEvent{
		baseEvent: baseEvent{Type: EventStateSnapshot, Timestamp: TimestampMs()},
		Snapshot:  snapshot,
	}
}

// CustomEvent is the protocol's extension point for an application's own events.
type CustomEvent struct {
	baseEvent
	Name  string `json:"name"`
	Value any    `json:"value"`
}

func (CustomEvent) eventType() EventType { return EventCustom }

// NewCustomEvent builds a custom event stamped now.
func NewCustomEvent(name string, value any) CustomEvent {
	return CustomEvent{
		baseEvent: baseEvent{Type: EventCustom, Timestamp: TimestampMs()},
		Name:      name,
		Value:     value,
	}
}

type SubagentStartedEvent struct {
	baseEvent
	SubagentRunID    string `json:"subagentRunId"`
	Name             string `json:"name"`
	Description      string `json:"description,omitempty"`
	ParentToolCallID string `json:"parentToolCallId,omitempty"`
}

func (SubagentStartedEvent) eventType() EventType { return "SUBAGENT_STARTED" }

type SubagentFinishedEvent struct {
	baseEvent
	SubagentRunID string `json:"subagentRunId"`
	Result        any    `json:"result,omitempty"`
}

func (SubagentFinishedEvent) eventType() EventType { return "SUBAGENT_FINISHED" }

type SubagentErrorEvent struct {
	baseEvent
	SubagentRunID string `json:"subagentRunId"`
	Message       string `json:"message"`
	Code          string `json:"code,omitempty"`
}

func (SubagentErrorEvent) eventType() EventType { return "SUBAGENT_ERROR" }

// Raw is an AG-UI event of a type this package does not model, carried verbatim.
// It is the escape hatch for a cfg.Run callback emitting event shapes beyond the
// constructors above.
type Raw struct {
	Payload json.RawMessage
}

func (r Raw) eventType() EventType {
	var head struct {
		Type EventType `json:"type"`
	}
	if err := json.Unmarshal(r.Payload, &head); err != nil {
		return ""
	}
	return head.Type
}

// MarshalJSON emits the payload verbatim.
func (r Raw) MarshalJSON() ([]byte, error) {
	if len(r.Payload) == 0 {
		return nil, fmt.Errorf("agui: Raw event carries no payload")
	}
	return r.Payload, nil
}

// UnmarshalEvent decodes one AG-UI event by its "type" discriminator. A type
// this package does not model comes back as Raw carrying the payload.
func UnmarshalEvent(data []byte) (Event, error) {
	var head baseEvent
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("agui: bad AG-UI event: %w", err)
	}
	switch head.Type {
	case EventRunStarted:
		return decodeOne[RunStartedEvent](data)
	case EventRunFinished:
		return decodeOne[RunFinishedEvent](data)
	case EventRunError:
		return decodeOne[RunErrorEvent](data)
	case EventTextMessageStart:
		return decodeOne[TextMessageStartEvent](data)
	case EventTextMessageContent:
		return decodeOne[TextMessageContentEvent](data)
	case EventTextMessageEnd:
		return decodeOne[TextMessageEndEvent](data)
	case EventTextMessageChunk:
		return decodeOne[TextMessageChunkEvent](data)
	case EventToolCallStart:
		return decodeOne[ToolCallStartEvent](data)
	case EventToolCallArgs:
		return decodeOne[ToolCallArgsEvent](data)
	case EventToolCallEnd:
		return decodeOne[ToolCallEndEvent](data)
	case EventToolCallChunk:
		return decodeOne[ToolCallChunkEvent](data)
	case EventToolCallResult:
		return decodeOne[ToolCallResultEvent](data)
	case EventReasoningStart:
		return decodeOne[ReasoningStartEvent](data)
	case EventReasoningMessageStart:
		return decodeOne[ReasoningMessageStartEvent](data)
	case EventReasoningMessageContent:
		return decodeOne[ReasoningMessageContentEvent](data)
	case EventReasoningMessageEnd:
		return decodeOne[ReasoningMessageEndEvent](data)
	case EventReasoningEnd:
		return decodeOne[ReasoningEndEvent](data)
	case EventStateSnapshot:
		return decodeOne[StateSnapshotEvent](data)
	case EventCustom:
		return decodeOne[CustomEvent](data)
	case "SUBAGENT_STARTED":
		return decodeOne[SubagentStartedEvent](data)
	case "SUBAGENT_FINISHED":
		return decodeOne[SubagentFinishedEvent](data)
	case "SUBAGENT_ERROR":
		return decodeOne[SubagentErrorEvent](data)
	default:
		return Raw{Payload: data}, nil
	}
}

// decodeOne unmarshals one typed event (the value form the projection emits).
func decodeOne[T Event](data []byte) (Event, error) {
	var e T
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("agui: decoding %s: %w", e.eventType(), err)
	}
	return e, nil
}

// isTerminal reports whether the event ends (or pauses) a run: the exchange
// stops reading on it (Python parity: serve_exchange breaks on
// RUN_FINISHED | RUN_ERROR).
func isTerminal(e Event) bool {
	switch e.eventType() {
	case EventRunFinished, EventRunError:
		return true
	}
	return false
}

// AGUIEvent carries a pre-encoded AG-UI event on an ago stream, so agent-side
// code can push protocol events the projection forwards verbatim (Python parity:
// ag_ui/events.py AGUIEvent; the projection's `isinstance(event, AGUIEvent)`).
type AGUIEvent struct {
	events.Base
	Payload json.RawMessage `json:"event"`
}

func (AGUIEvent) EventType() string    { return "AGUIEvent" }
func (AGUIEvent) Transient() bool      { return false }
func (AGUIEvent) Conversational() bool { return false }

// MarshalJSON writes the "@type"-discriminated ago transcript form.
func (a AGUIEvent) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type string `json:"@type"`
		events.Base
		Payload json.RawMessage `json:"event"`
	}{"AGUIEvent", a.Base, a.Payload})
}

// UnmarshalJSON reads the transcript form; the AG-UI payload is kept verbatim.
func (a *AGUIEvent) UnmarshalJSON(data []byte) error {
	var w struct {
		events.Base
		Payload json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	a.Base, a.Payload = w.Base, w.Payload
	return nil
}

func init() {
	events.RegisterEvent(AGUIEvent{})
}
