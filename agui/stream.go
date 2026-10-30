package agui

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"

	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/internal/uuid"
)

// Stream projects one served turn: an ago conversation stream's events become
// AG-UI events on emit (Python parity: stream.py's map_events_to_ag_ui
// subscriber, plus the usage meter). One Stream serves one run; it is not safe
// for concurrent use — the projection runs inside the stream's synchronous
// dispatch, which serializes it.
type Stream struct {
	emit func(Event)

	// message ids opened on the wire and not yet closed (Python parity:
	// streaming_msg_id / reasoning_msg_id).
	streamingMsgID string
	reasoningMsgID string
	// announced records the tool call ids TOOL_CALL_START went out for, so a
	// client hand-off (ClientToolCallEvent) is not announced twice (see Handle).
	announced map[string]bool
	// calls are the tool calls this run started, and whether each was answered
	// (Python parity: TurnOutput._calls, for pendingToolCallIds).
	calls map[string]bool
	// usage accumulates every UsageEvent the run spent, as it was spent.
	usage []events.UsageEvent
}

// NewStream builds a projection emitting AG-UI events through emit.
func NewStream(emit func(Event)) *Stream {
	return &Stream{
		emit:      emit,
		announced: map[string]bool{},
		calls:     map[string]bool{},
	}
}

// Handle projects one ago event. Events with no projection are dropped
// silently; among them the gathered ToolResultsEvent, whose results already went
// out individually (AGO-DEVIATION: Python has no such batch on the stream, so
// double-projecting results is not a question it faces — see D-053).
func (s *Stream) Handle(ev events.Event) {
	e := events.AsValue(ev)

	// A reasoning span ends where the reasoning stops, whatever follows it
	// (Python parity: the close at the top of the subscriber).
	if _, isReasoning := e.(events.ModelReasoning); !isReasoning {
		s.closeReasoning()
	}

	switch e := e.(type) {
	case events.ModelReasoning:
		if e.Content == "" {
			return
		}
		if s.reasoningMsgID == "" {
			s.reasoningMsgID = uuid.New()
			s.emit(ReasoningStartEvent{
				baseEvent: baseEvent{Type: EventReasoningStart, Timestamp: TimestampMs()},
				MessageID: s.reasoningMsgID,
			})
			s.emit(ReasoningMessageStartEvent{
				baseEvent: baseEvent{Type: EventReasoningMessageStart, Timestamp: TimestampMs()},
				MessageID: s.reasoningMsgID,
				Role:      reasoningRole,
			})
		}
		s.emit(ReasoningMessageContentEvent{
			baseEvent: baseEvent{Type: EventReasoningMessageContent, Timestamp: TimestampMs()},
			MessageID: s.reasoningMsgID,
			Delta:     e.Content,
		})

	case events.ModelMessageChunk:
		if e.Content == "" {
			return
		}
		if s.streamingMsgID == "" {
			s.streamingMsgID = uuid.New()
			s.emit(TextMessageStartEvent{
				baseEvent: baseEvent{Type: EventTextMessageStart, Timestamp: TimestampMs()},
				MessageID: s.streamingMsgID,
			})
		}
		s.emit(TextMessageContentEvent{
			baseEvent: baseEvent{Type: EventTextMessageContent, Timestamp: TimestampMs()},
			MessageID: s.streamingMsgID,
			Delta:     e.Content,
		})

	case events.ModelResponse:
		// The durable end of one model call. A streamed message closes here; a
		// whole answer with no chunks behind it goes out as one chunk.
		if s.streamingMsgID != "" {
			s.emit(TextMessageEndEvent{
				baseEvent: baseEvent{Type: EventTextMessageEnd, Timestamp: TimestampMs()},
				MessageID: s.streamingMsgID,
			})
			s.streamingMsgID = ""
		} else if e.Content() != "" {
			s.emit(TextMessageChunkEvent{
				baseEvent: baseEvent{Type: EventTextMessageChunk, Timestamp: TimestampMs()},
				MessageID: uuid.New(),
				Delta:     e.Content(),
			})
		}

	case events.ToolCallEvent:
		// One call of a model-requested batch. Announced and closed as soon as
		// its arguments are complete, not after it runs (Python parity: the
		// comment on the ToolCallEndEvent — a run may end with the call still
		// pending, and clients refuse a RUN_FINISHED while a call is open).
		s.announced[e.ID] = true
		s.calls[e.ID] = false
		s.emit(ToolCallStartEvent{
			baseEvent:    baseEvent{Type: EventToolCallStart, Timestamp: TimestampMs()},
			ToolCallID:   e.ID,
			ToolCallName: e.Name,
		})
		s.emit(ToolCallArgsEvent{
			baseEvent:  baseEvent{Type: EventToolCallArgs, Timestamp: TimestampMs()},
			ToolCallID: e.ID,
			Delta:      e.Arguments,
		})
		s.emit(ToolCallEndEvent{
			baseEvent:  baseEvent{Type: EventToolCallEnd, Timestamp: TimestampMs()},
			ToolCallID: e.ID,
		})

	case events.ClientToolCallEvent:
		// A call delegated to the client application. When the same call was
		// already announced (the executor sends the model's ToolCallEvent before
		// the client tool's subscriber answers it — the reverse of Python's
		// dispatch order), the START/ARGS/END above stands in for Python's
		// single chunk. AGO-DEVIATION: suppression is by call id, not by tool
		// name, because the call arrives first here. An unannounced hand-off
		// goes out as the chunk Python sends.
		if s.announced[e.ID] {
			s.calls[e.ID] = false
			return
		}
		s.calls[e.ID] = false
		s.emit(ToolCallChunkEvent{
			baseEvent:    baseEvent{Type: EventToolCallChunk, Timestamp: TimestampMs()},
			ToolCallID:   e.ID,
			ToolCallName: e.Name,
			Delta:        e.Arguments,
		})

	case events.ToolResultEvent:
		s.answerCall(e.ParentID)
		s.emitToolResult(e)

	case events.ToolErrorEvent:
		s.answerCall(e.ParentID)
		s.emitToolResult(e.ToolResultEvent)

	case events.ToolNotFoundEvent:
		s.answerCall(e.ParentID)
		s.emitToolResult(e.ToolErrorEvent.ToolResultEvent)

	case events.BuiltinToolCallEvent:
		s.Handle(e.ToolCallEvent)

	case events.BuiltinToolResultEvent:
		s.Handle(e.ToolResultEvent)

	case events.UsageEvent:
		s.usage = append(s.usage, e)

	case events.TaskStarted:
		s.emit(SubagentStartedEvent{
			baseEvent:        baseEvent{Type: "SUBAGENT_STARTED", Timestamp: TimestampMs()},
			SubagentRunID:    e.TaskID,
			Name:             e.AgentName,
			Description:      e.Objective,
			ParentToolCallID: e.ParentToolCallID,
		})

	case events.TaskCompleted:
		// No usage rides on these: the run's own total already holds the
		// delegated spend (Python parity: map_task_event_to_ag_ui).
		var result any = e.Result
		if e.Result == "" {
			result = nil
		}
		s.emit(SubagentFinishedEvent{
			baseEvent:     baseEvent{Type: "SUBAGENT_FINISHED", Timestamp: TimestampMs()},
			SubagentRunID: e.TaskID,
			Result:        result,
		})

	case events.TaskFailed:
		message := e.Error
		if message == "" {
			message = "subtask failed"
		}
		s.emit(subagentError(e.TaskID, message))

	case events.TaskCancelled:
		message := "cancelled"
		if e.Reason != "" {
			message = "cancelled: " + e.Reason
		}
		s.emit(subagentError(e.TaskID, message))

	case events.TaskExpired:
		s.emit(subagentError(e.TaskID, "expired"))

	case AGUIEvent:
		forwarded, err := UnmarshalEvent(e.Payload)
		if err != nil {
			slog.Warn("agui: dropping an AGUIEvent whose payload does not decode", "err", err)
			return
		}
		s.recordForwarded(forwarded)
		s.emit(forwarded)
	}
}

func subagentError(taskID, message string) SubagentErrorEvent {
	return SubagentErrorEvent{
		baseEvent:     baseEvent{Type: "SUBAGENT_ERROR", Timestamp: TimestampMs()},
		SubagentRunID: taskID,
		Message:       message,
	}
}

// answerCall marks a call answered; a ToolCallResultEvent for a call this run
// never started still goes out, but names nothing pending.
func (s *Stream) answerCall(callID string) {
	if _, ok := s.calls[callID]; ok {
		s.calls[callID] = true
	}
}

// emitToolResult sends the wire event for a tool's result: a lone text as a
// string, anything else in parts (Python parity: map_tool_result_to_ag_ui).
func (s *Stream) emitToolResult(res events.ToolResultEvent) {
	content := toolResultContent(res.Result)
	s.emit(ToolCallResultEvent{
		baseEvent:  baseEvent{Type: EventToolCallResult, Timestamp: TimestampMs()},
		MessageID:  uuid.New(),
		ToolCallID: res.ParentID,
		Content:    content,
		Role:       "tool",
	})
}

// toolResultContent maps a result as a 1.0 client reads it: a lone text as a
// string, anything else in parts. The protocol has no JSON part: structured
// output travels as its text.
func toolResultContent(r events.ToolResult) any {
	parts := make([]ContentPart, 0, len(r.Parts))
	for _, part := range r.Parts {
		p := events.AsValue(part)
		switch part := p.(type) {
		case events.TextInput:
			parts = append(parts, ContentPart{Type: PartText, Text: part.Content})
		case events.DataInput:
			parts = append(parts, ContentPart{Type: PartText, Text: jsonString(part.Data)})
		case events.UrlInput:
			parts = append(parts, ContentPart{Type: partKind(part.Kind), Source: &PartSource{Type: SourceURL, Value: part.URL}})
		case events.BinaryInput:
			parts = append(parts, ContentPart{
				Type:   partKind(part.Kind),
				Source: &PartSource{Type: SourceData, Value: base64Of(part.Data), MimeType: part.MediaType},
			})
		case events.FileIdInput:
			parts = append(parts, ContentPart{
				Type:   PartDocument,
				Source: &PartSource{Type: SourceFile, Value: part.FileID},
			})
		default:
			slog.Warn("agui: dropping a tool result part with no AG-UI content form", "part", p.EventType())
		}
	}
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 && parts[0].Type == PartText && parts[0].Source == nil {
		return parts[0].Text
	}
	return parts
}

// partKind maps an ago binary kind to the content part type that carries it; an
// input of no particular kind travels as a document (Python parity:
// _PART_OF_KIND).
func partKind(kind events.BinaryType) string {
	switch kind {
	case events.BinaryImage:
		return PartImage
	case events.BinaryAudio:
		return PartAudio
	case events.BinaryVideo:
		return PartVideo
	default:
		return PartDocument
	}
}

// recordForwarded keeps the pending-call ledger true for events a client pushed
// through AGUIEvent, so the projection's accounting covers them too.
func (s *Stream) recordForwarded(e Event) {
	switch e := e.(type) {
	case ToolCallStartEvent:
		s.announced[e.ToolCallID] = true
		s.calls[e.ToolCallID] = false
	case ToolCallChunkEvent:
		if e.ToolCallID != "" {
			s.calls[e.ToolCallID] = false
		}
	case ToolCallResultEvent:
		s.answerCall(e.ToolCallID)
	}
}

// closeReasoning ends an open reasoning span: the message first, then the span.
func (s *Stream) closeReasoning() {
	if s.reasoningMsgID == "" {
		return
	}
	id := s.reasoningMsgID
	s.reasoningMsgID = ""
	s.emit(ReasoningMessageEndEvent{
		baseEvent: baseEvent{Type: EventReasoningMessageEnd, Timestamp: TimestampMs()},
		MessageID: id,
	})
	s.emit(ReasoningEndEvent{
		baseEvent: baseEvent{Type: EventReasoningEnd, Timestamp: TimestampMs()},
		MessageID: id,
	})
}

// PendingToolCallIDs names the calls this run started and left unanswered, in
// call order (Python parity: TurnOutput.success_outcome). The map is iterated in
// insertion order through a slice, so the result is stable.
func (s *Stream) SuccessOutcome() SuccessOutcome {
	var pending []string
	for id, answered := range s.calls {
		if !answered {
			pending = append(pending, id)
		}
	}
	return SuccessOutcome{PendingToolCallIDs: pending}
}

// TakeUsage returns the AG-UI usage entries for what was spent since the last
// take, or nil for a run that spent nothing (Python parity:
// map_usage_events_to_ag_ui).
func (s *Stream) TakeUsage() []TokenUsage {
	spent := s.usage
	s.usage = nil
	return tokenUsageFromEvents(spent)
}

// tokenUsageFromEvents groups attributed spend per (provider, model) in the
// protocol's accounting (Python parity: usage.py). A delegation's rollup is
// replaced by the calls it sums; absent counts stay unset unless some call
// reported them; totalTokens is recomputed, never copied.
func tokenUsageFromEvents(evs []events.UsageEvent) []TokenUsage {
	type key struct{ provider, model string }
	groups := map[key]*TokenUsage{}
	var order []key
	var walk func(ev events.UsageEvent)
	walk = func(ev events.UsageEvent) {
		if len(ev.Parts) > 0 {
			for _, p := range ev.Parts {
				walk(p)
			}
			return
		}
		k := key{ev.Provider, ev.Model}
		g, ok := groups[k]
		if !ok {
			g = &TokenUsage{Provider: ev.Provider, Model: ev.Model}
			groups[k] = g
			order = append(order, k)
		}
		g.InputTokens = addPresent(g.InputTokens, inputTotal(ev.Provider, ev.Usage))
		g.OutputTokens = addPresent(g.OutputTokens, countOf(ev.Usage.CompletionTokens))
		g.ReasoningTokens = addPresent(g.ReasoningTokens, countOf(ev.Usage.ThinkingTokens))
		g.CachedInputTokens = addPresent(g.CachedInputTokens, countOf(ev.Usage.CacheReadInputTokens))
		g.CacheWriteInputTokens = addPresent(g.CacheWriteInputTokens, countOf(ev.Usage.CacheCreationInputTokens))
	}
	for _, ev := range evs {
		walk(ev)
	}
	if len(order) == 0 {
		return nil
	}
	out := make([]TokenUsage, 0, len(order))
	for _, k := range order {
		g := groups[k]
		g.TotalTokens = sumPositive(g.InputTokens, g.OutputTokens)
		out = append(out, *g)
	}
	return out
}

// cacheOutsidePrompt are the providers whose prompt count leaves out the tokens
// read from and written to the cache; AG-UI's input is a total, so the cache
// counts are added in before the entry leaves (Python parity:
// usage.py _CACHE_OUTSIDE_PROMPT).
var cacheOutsidePrompt = map[string]bool{"anthropic": true, "bedrock": true}

func inputTotal(provider string, usage events.Usage) *int64 {
	if usage.PromptTokens == nil || !cacheOutsidePrompt[provider] {
		return countOf(usage.PromptTokens)
	}
	total := *usage.PromptTokens
	for _, v := range []*int64{usage.CacheReadInputTokens, usage.CacheCreationInputTokens} {
		if v != nil && *v > 0 {
			total += *v
		}
	}
	return &total
}

// countOf admits only non-negative counts; the wire type allows nothing else,
// and this runs on the failure path while a run is being reported (Python
// parity: usage.py _token_count).
func countOf(v *int64) *int64 {
	if v == nil || *v < 0 {
		return nil
	}
	return v
}

// addPresent sums two optional counts; an absent count stays absent unless one
// side reported it.
func addPresent(a, b *int64) *int64 {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	default:
		s := *a + *b
		return &s
	}
}

// sumPositive totals the two counts only when both are present, so a grouped
// total is never short of input plus output (Python parity: usage.py
// _with_total).
func sumPositive(a, b *int64) *int64 {
	if a == nil || b == nil {
		return nil
	}
	s := *a + *b
	if s < 0 {
		return nil
	}
	return &s
}

// base64Of encodes bytes the way the wire carries inline sources.
func base64Of(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

// jsonString serializes a structured tool-result part as its text (Python
// parity: stream.py serializer.encode(part.data)).
func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
