package agui

import (
	"reflect"
	"testing"

	"ago/events"
)

// collect builds a Stream appending every emitted event, and a reader for the
// result.
func collect() (*Stream, func() []Event) {
	var got []Event
	s := NewStream(func(e Event) { got = append(got, e) })
	return s, func() []Event { return got }
}

func eventTypes(evs []Event) []EventType {
	out := make([]EventType, len(evs))
	for i, e := range evs {
		out[i] = e.eventType()
	}
	return out
}

func chunk(content string) *events.ModelMessageChunk {
	return &events.ModelMessageChunk{Content: content}
}

func TestStreamProjectsChunksIntoATextMessage(t *testing.T) {
	s, got := collect()
	s.Handle(chunk("Hel"))
	s.Handle(chunk("lo"))
	s.Handle(&events.ModelResponse{Message: &events.ModelMessage{Content: "Hello"}})

	evs := got()
	if want := []EventType{EventTextMessageStart, EventTextMessageContent, EventTextMessageContent, EventTextMessageEnd}; !reflect.DeepEqual(eventTypes(evs), want) {
		t.Fatalf("types = %v, want %v", eventTypes(evs), want)
	}
	id := evs[0].(TextMessageStartEvent).MessageID
	if id == "" {
		t.Fatal("message id is empty")
	}
	for i, want := range []string{"Hel", "lo"} {
		c := evs[i+1].(TextMessageContentEvent)
		if c.MessageID != id || c.Delta != want {
			t.Fatalf("content[%d] = %q on %q, want %q on %q", i, c.Delta, c.MessageID, want, id)
		}
	}
	if e := evs[3].(TextMessageEndEvent); e.MessageID != id {
		t.Fatalf("end on %q, want %q", e.MessageID, id)
	}
}

func TestStreamWholeAnswerBecomesOneChunk(t *testing.T) {
	s, got := collect()
	s.Handle(&events.ModelResponse{Message: &events.ModelMessage{Content: "all at once"}})
	s.Handle(&events.ModelResponse{}) // a second call with no content: nothing

	evs := got()
	if want := []EventType{EventTextMessageChunk}; !reflect.DeepEqual(eventTypes(evs), want) {
		t.Fatalf("types = %v, want %v", eventTypes(evs), want)
	}
	c := evs[0].(TextMessageChunkEvent)
	if c.MessageID == "" || c.Delta != "all at once" {
		t.Fatalf("chunk = %+v", c)
	}
}

func TestStreamProjectsReasoningSpans(t *testing.T) {
	s, got := collect()
	s.Handle(&events.ModelReasoning{Content: "think"})
	s.Handle(&events.ModelReasoning{Content: "ing"})
	s.Handle(chunk("answer")) // any non-reasoning event closes the span
	s.Handle(chunk(""))

	evs := got()
	want := []EventType{
		EventReasoningStart, EventReasoningMessageStart, EventReasoningMessageContent,
		EventReasoningMessageContent, EventReasoningMessageEnd, EventReasoningEnd,
		EventTextMessageStart, EventTextMessageContent,
	}
	if !reflect.DeepEqual(eventTypes(evs), want) {
		t.Fatalf("types = %v, want %v", eventTypes(evs), want)
	}
	if ms := evs[1].(ReasoningMessageStartEvent); ms.Role != "reasoning" || ms.MessageID == "" {
		t.Fatalf("message start = %+v", ms)
	}
	spanID := evs[0].(ReasoningStartEvent).MessageID
	for i := 1; i <= 5; i++ {
		if got := reflect.ValueOf(evs[i]).FieldByName("MessageID").String(); got != spanID {
			t.Fatalf("event %d carries %q, want the span id %q", i, got, spanID)
		}
	}
}

func TestStreamProjectsToolCallsAndResults(t *testing.T) {
	s, got := collect()
	call := events.ToolCallEvent{ID: "c1", Name: "get_weather", Arguments: `{"city":"Paris"}`}
	s.Handle(&call)
	s.Handle(&events.ToolResultEvent{
		ParentID: "c1", Name: "get_weather",
		Result: events.ToolResult{Parts: []events.Input{&events.TextInput{Content: "sunny"}}},
	})

	evs := got()
	want := []EventType{EventToolCallStart, EventToolCallArgs, EventToolCallEnd, EventToolCallResult}
	if !reflect.DeepEqual(eventTypes(evs), want) {
		t.Fatalf("types = %v, want %v", eventTypes(evs), want)
	}
	if a := evs[1].(ToolCallArgsEvent); a.ToolCallID != "c1" || a.Delta != `{"city":"Paris"}` {
		t.Fatalf("args = %+v", a)
	}
	// A lone text result travels as a string.
	res := evs[3].(ToolCallResultEvent)
	if res.ToolCallID != "c1" || res.Content != "sunny" || res.Role != "tool" || res.MessageID == "" {
		t.Fatalf("result = %+v", res)
	}
	if pending := s.SuccessOutcome().PendingToolCallIDs; len(pending) != 0 {
		t.Fatalf("pending = %v, want none: the call was answered", pending)
	}
}

func TestStreamToolResultParts(t *testing.T) {
	s, got := collect()
	s.Handle(&events.ToolResultEvent{
		ParentID: "c1",
		Result: events.ToolResult{Parts: []events.Input{
			&events.TextInput{Content: "text"},
			&events.DataInput{Data: map[string]any{"k": 1}},
			&events.UrlInput{URL: "http://x", Kind: events.BinaryImage},
			&events.BinaryInput{Data: []byte("ABC"), MediaType: "image/png", Kind: events.BinaryImage},
		}},
	})
	res := got()[0].(ToolCallResultEvent)
	parts, ok := res.Content.([]ContentPart)
	if !ok || len(parts) != 4 {
		t.Fatalf("content = %#v, want four parts", res.Content)
	}
	if parts[0].Type != PartText || parts[0].Text != "text" {
		t.Fatalf("part 0 = %+v", parts[0])
	}
	if parts[1].Type != PartText || parts[1].Text != `{"k":1}` {
		t.Fatalf("structured part = %+v, want its JSON text", parts[1])
	}
	if parts[2].Type != PartImage || parts[2].Source.Type != SourceURL || parts[2].Source.Value != "http://x" {
		t.Fatalf("url part = %+v", parts[2])
	}
	if parts[3].Source.Type != SourceData || parts[3].Source.Value != "QUJD" || parts[3].Source.MimeType != "image/png" {
		t.Fatalf("data part = %+v", parts[3])
	}
}

func TestStreamClientToolHandoff(t *testing.T) {
	t.Run("announced after the model's call is not announced twice", func(t *testing.T) {
		s, got := collect()
		s.Handle(&events.ToolCallEvent{ID: "c1", Name: "draw", Arguments: `{"x":1}`})
		s.Handle(&events.ClientToolCallEvent{ID: "c1", Name: "draw", Arguments: `{"x":1}`})

		evs := got()
		if want := []EventType{EventToolCallStart, EventToolCallArgs, EventToolCallEnd}; !reflect.DeepEqual(eventTypes(evs), want) {
			t.Fatalf("types = %v, want %v", eventTypes(evs), want)
		}
		if pending := s.SuccessOutcome().PendingToolCallIDs; !reflect.DeepEqual(pending, []string{"c1"}) {
			t.Fatalf("pending = %v, want the hand-off named", pending)
		}
	})

	t.Run("a bare hand-off goes out as one chunk", func(t *testing.T) {
		s, got := collect()
		s.Handle(&events.ClientToolCallEvent{ID: "c9", Name: "draw", Arguments: "{}"})
		evs := got()
		if want := []EventType{EventToolCallChunk}; !reflect.DeepEqual(eventTypes(evs), want) {
			t.Fatalf("types = %v, want %v", eventTypes(evs), want)
		}
		c := evs[0].(ToolCallChunkEvent)
		if c.ToolCallID != "c9" || c.ToolCallName != "draw" {
			t.Fatalf("chunk = %+v", c)
		}
	})
}

func TestStreamToolNotFoundProjectsResult(t *testing.T) {
	s, got := collect()
	notFound := events.ToolNotFoundEvent{ToolErrorEvent: events.FromErrorCall(
		events.ToolCallEvent{ID: "c1", Name: "nope"}, errFake)}
	s.Handle(&notFound)

	res, ok := got()[0].(ToolCallResultEvent)
	if !ok {
		t.Fatalf("got %T, want a TOOL_CALL_RESULT", got()[0])
	}
	if res.ToolCallID != "c1" || res.Content == "" {
		t.Fatalf("result = %+v, want the error text", res)
	}
}

var errFake = &fakeError{}

type fakeError struct{}

func (*fakeError) Error() string { return "no tool named nope" }

func TestStreamSubagentLifecycle(t *testing.T) {
	s, got := collect()
	s.Handle(&events.TaskStarted{TaskID: "task1", AgentName: "helper", Objective: "research", ParentToolCallID: "c1"})
	s.Handle(&events.TaskCompleted{TaskID: "task1", AgentName: "helper", Result: "found it"})
	s.Handle(&events.TaskFailed{TaskID: "task2", AgentName: "helper", Error: "blew up"})
	s.Handle(&events.TaskCancelled{TaskID: "task3", AgentName: "helper", Reason: "user asked"})
	s.Handle(&events.TaskCancelled{TaskID: "task4", AgentName: "helper"})
	s.Handle(&events.TaskExpired{TaskID: "task5", AgentName: "helper"})

	evs := got()
	want := []EventType{"SUBAGENT_STARTED", "SUBAGENT_FINISHED", "SUBAGENT_ERROR", "SUBAGENT_ERROR", "SUBAGENT_ERROR", "SUBAGENT_ERROR"}
	if !reflect.DeepEqual(eventTypes(evs), want) {
		t.Fatalf("types = %v, want %v", eventTypes(evs), want)
	}
	if st := evs[0].(SubagentStartedEvent); st.SubagentRunID != "task1" || st.Name != "helper" ||
		st.Description != "research" || st.ParentToolCallID != "c1" {
		t.Fatalf("started = %+v", st)
	}
	if fin := evs[1].(SubagentFinishedEvent); fin.SubagentRunID != "task1" || fin.Result != "found it" {
		t.Fatalf("finished = %+v", fin)
	}
	if m := evs[2].(SubagentErrorEvent).Message; m != "blew up" {
		t.Fatalf("failed message = %q", m)
	}
	if m := evs[3].(SubagentErrorEvent).Message; m != "cancelled: user asked" {
		t.Fatalf("cancel message = %q", m)
	}
	if m := evs[4].(SubagentErrorEvent).Message; m != "cancelled" {
		t.Fatalf("bare cancel message = %q", m)
	}
	if m := evs[5].(SubagentErrorEvent).Message; m != "expired" {
		t.Fatalf("expired message = %q", m)
	}
}

func TestStreamForwardedAGUIEvent(t *testing.T) {
	s, got := collect()
	s.Handle(&AGUIEvent{Payload: []byte(`{"type":"CUSTOM","name":"progress","value":{"done":1}}`)})
	s.Handle(&AGUIEvent{Payload: []byte(`{not json`)}) // dropped with a warning

	evs := got()
	if len(evs) != 1 {
		t.Fatalf("got %d events, want one", len(evs))
	}
	if c, ok := evs[0].(CustomEvent); !ok || c.Name != "progress" {
		t.Fatalf("forwarded = %#v", evs[0])
	}
}

func TestStreamUsageMapping(t *testing.T) {
	t.Run("entries group per provider and model", func(t *testing.T) {
		s, _ := collect()
		s.Handle(&events.UsageEvent{Usage: events.Usage{
			PromptTokens: i64(10), CompletionTokens: i64(5)}, Provider: "p", Model: "m"})
		s.Handle(&events.UsageEvent{Usage: events.Usage{
			PromptTokens: i64(3), CompletionTokens: i64(2)}, Provider: "p", Model: "m"})

		got := s.TakeUsage()
		want := []TokenUsage{{
			Provider: "p", Model: "m",
			InputTokens: i64(13), OutputTokens: i64(7), TotalTokens: i64(20),
		}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("usage = %+v, want %+v", got, want)
		}
		if again := s.TakeUsage(); again != nil {
			t.Fatalf("take is not a take: %+v", again)
		}
	})

	t.Run("a rollup is replaced by the calls it sums", func(t *testing.T) {
		s, _ := collect()
		s.Handle(&events.UsageEvent{
			Usage: events.Usage{PromptTokens: i64(100), CompletionTokens: i64(50)},
			Parts: []events.UsageEvent{
				{Usage: events.Usage{PromptTokens: i64(40), CompletionTokens: i64(30)}, Provider: "a", Model: "m1"},
				{Usage: events.Usage{PromptTokens: i64(60), CompletionTokens: i64(20)}, Provider: "b", Model: "m2"},
			},
		})
		got := s.TakeUsage()
		if len(got) != 2 {
			t.Fatalf("got %d entries, want the two calls the rollup sums: %+v", len(got), got)
		}
		if got[0].Provider != "a" || got[0].InputTokens == nil || *got[0].InputTokens != 40 {
			t.Fatalf("entry 0 = %+v", got[0])
		}
	})

	t.Run("cache counts outside the prompt are added in", func(t *testing.T) {
		s, _ := collect()
		s.Handle(&events.UsageEvent{Usage: events.Usage{
			PromptTokens:             i64(10),
			CacheReadInputTokens:     i64(4),
			CacheCreationInputTokens: i64(2),
		}, Provider: "anthropic", Model: "claude"})
		got := s.TakeUsage()
		if *got[0].InputTokens != 16 {
			t.Fatalf("input = %d, want 10 + 4 + 2", *got[0].InputTokens)
		}
		if *got[0].CachedInputTokens != 4 || *got[0].CacheWriteInputTokens != 2 {
			t.Fatalf("cache breakdown = %+v", got[0])
		}
	})

	t.Run("nothing spent is no entries", func(t *testing.T) {
		s, _ := collect()
		if got := s.TakeUsage(); got != nil {
			t.Fatalf("usage = %+v, want nil", got)
		}
	})
}
