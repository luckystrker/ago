package gemini

import (
	"testing"

	"github.com/luckystrker/ago/events"
)

// thoughtSignatures indexes the thoughtSignature of every functionCall part by
// call name; a nil value means the part carries no signature. Call names are
// unique in these tests because the wire functionCall part has no id.
func thoughtSignatures(contents []wireContent) map[string]any {
	out := make(map[string]any)
	for _, content := range contents {
		for _, part := range content.Parts {
			call, ok := part["functionCall"].(map[string]any)
			if !ok {
				continue
			}
			name, _ := call["name"].(string)
			out[name] = part["thoughtSignature"]
		}
	}
	return out
}

// toolStep turns calls into the ModelResponse + ToolResultsEvent pair the
// history carries for one function-calling step.
func toolStep(calls ...events.ToolCallEvent) []events.Event {
	results := make([]events.ToolResultEvent, 0, len(calls))
	for _, call := range calls {
		results = append(results, events.ToolResultEvent{
			ParentID: call.ID,
			Name:     call.Name,
			Result:   events.NewToolResult(false, "ok"),
		})
	}
	return []events.Event{
		events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: calls}},
		events.ToolResultsEvent{Results: results},
	}
}

func userText(text string) events.Event {
	return events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: text}}}
}

func TestBuildContentsSignsCurrentTurnCalls(t *testing.T) {
	cases := []struct {
		name string
		evs  []events.Event
		want map[string]any
	}{
		{
			name: "unsigned current-turn call gets the placeholder",
			evs: append([]events.Event{userText("Find it.")},
				toolStep(events.ToolCallEvent{ID: "call-1", Name: "search", Arguments: `{}`})...),
			want: map[string]any{"search": "skip_thought_signature_validator"},
		},
		{
			name: "each step is signed on its first call only",
			evs: append([]events.Event{userText("Find both.")},
				append(
					toolStep(
						events.ToolCallEvent{ID: "step1-a", Name: "first", Arguments: `{}`},
						events.ToolCallEvent{ID: "step1-b", Name: "second", Arguments: `{}`},
					),
					toolStep(
						events.ToolCallEvent{ID: "step2-a", Name: "third", Arguments: `{}`},
						events.ToolCallEvent{ID: "step2-b", Name: "fourth", Arguments: `{}`},
					)...,
				)...),
			want: map[string]any{
				"first":  "skip_thought_signature_validator",
				"second": nil,
				"third":  "skip_thought_signature_validator",
				"fourth": nil,
			},
		},
		{
			name: "older turns stay unsigned",
			evs: append(
				append([]events.Event{userText("First question.")},
					toolStep(events.ToolCallEvent{ID: "old-call", Name: "old", Arguments: `{}`})...),
				append([]events.Event{
					events.ModelResponse{Message: &events.ModelMessage{Content: "Nothing found."}},
					userText("Second question."),
				}, toolStep(events.ToolCallEvent{ID: "new-call", Name: "new", Arguments: `{}`})...)...,
			),
			want: map[string]any{"old": nil, "new": "skip_thought_signature_validator"},
		},
		{
			name: "a history with no user turn is treated as one current turn",
			evs:  toolStep(events.ToolCallEvent{ID: "call-1", Name: "search", Arguments: `{}`}),
			want: map[string]any{"search": "skip_thought_signature_validator"},
		},
		{
			name: "text before the call in one model step does not block the placeholder",
			evs: []events.Event{
				userText("Think, then find it."),
				events.ModelResponse{
					Message:   &events.ModelMessage{Content: "Let me search."},
					ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{{ID: "call-1", Name: "search", Arguments: `{}`}}},
				},
				events.ToolResultsEvent{Results: []events.ToolResultEvent{{
					ParentID: "call-1",
					Name:     "search",
					Result:   events.NewToolResult(false, "ok"),
				}}},
			},
			want: map[string]any{"search": "skip_thought_signature_validator"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			contents, err := buildContents(tc.evs)
			if err != nil {
				t.Fatalf("buildContents: %v", err)
			}
			got := thoughtSignatures(contents)
			for name, want := range tc.want {
				if got[name] != want {
					t.Errorf("call %q signature = %v, want %v", name, got[name], want)
				}
			}
		})
	}
}

func TestSkipThoughtSignatureValue(t *testing.T) {
	if SkipThoughtSignature != "skip_thought_signature_validator" {
		t.Fatalf("SkipThoughtSignature = %q, want Google's documented sentinel", SkipThoughtSignature)
	}
}

func TestSignCurrentTurnCallsKeepsSignaturesAndTracksTurns(t *testing.T) {
	call := func(name string) map[string]any {
		return map[string]any{"functionCall": map[string]any{"name": name}}
	}
	signed := func(name, signature string) map[string]any {
		return map[string]any{
			"functionCall":     map[string]any{"name": name},
			"thoughtSignature": signature,
		}
	}
	cases := []struct {
		name     string
		contents []wireContent
		want     map[string]any
	}{
		{
			name: "a signed first call is kept and later calls stay unsigned",
			contents: []wireContent{
				{Role: "user", Parts: []map[string]any{{"text": "q"}}},
				{Role: "model", Parts: []map[string]any{signed("first", "real-signature"), call("second")}},
			},
			want: map[string]any{"first": "real-signature", "second": nil},
		},
		{
			name: "text before the first call does not block the placeholder",
			contents: []wireContent{
				{Role: "user", Parts: []map[string]any{{"text": "q"}}},
				{Role: "model", Parts: []map[string]any{{"text": "thinking"}, call("search")}},
			},
			want: map[string]any{"search": SkipThoughtSignature},
		},
		{
			name: "model content without calls is untouched",
			contents: []wireContent{
				{Role: "user", Parts: []map[string]any{{"text": "q"}}},
				{Role: "model", Parts: []map[string]any{{"text": "answer"}}},
			},
			want: map[string]any{},
		},
		{
			name: "function-response batches do not open a new turn",
			contents: []wireContent{
				{Role: "user", Parts: []map[string]any{{"text": "q"}}},
				{Role: "model", Parts: []map[string]any{call("step1")}},
				{Role: "user", Parts: []map[string]any{{"functionResponse": map[string]any{"name": "step1"}}}},
				{Role: "model", Parts: []map[string]any{call("step2")}},
			},
			want: map[string]any{"step1": SkipThoughtSignature, "step2": SkipThoughtSignature},
		},
		{
			name: "an earlier signed turn is not rewritten",
			contents: []wireContent{
				{Role: "user", Parts: []map[string]any{{"text": "first"}}},
				{Role: "model", Parts: []map[string]any{signed("old", "old-signature")}},
				{Role: "user", Parts: []map[string]any{{"text": "second"}}},
				{Role: "model", Parts: []map[string]any{call("new")}},
			},
			want: map[string]any{"old": "old-signature", "new": SkipThoughtSignature},
		},
		{
			// The turn boundary is the last user content itself, not the content
			// before it: a previous-turn model step that sits directly before the
			// new user message must stay unsigned.
			name: "a previous-turn call directly before a new user stays unsigned",
			contents: []wireContent{
				{Role: "user", Parts: []map[string]any{{"text": "first"}}},
				{Role: "model", Parts: []map[string]any{call("old")}},
				{Role: "user", Parts: []map[string]any{{"text": "second"}}},
				{Role: "model", Parts: []map[string]any{call("new")}},
			},
			want: map[string]any{"old": nil, "new": SkipThoughtSignature},
		},
		{
			name: "a signed later call keeps its signature",
			contents: []wireContent{
				{Role: "user", Parts: []map[string]any{{"text": "q"}}},
				{Role: "model", Parts: []map[string]any{call("first"), signed("second", "real-signature")}},
			},
			want: map[string]any{"first": SkipThoughtSignature, "second": "real-signature"},
		},
		{
			name:     "empty contents is a no-op",
			contents: nil,
			want:     map[string]any{},
		},
		{
			name: "a nil signature is treated as missing",
			contents: []wireContent{
				{Role: "user", Parts: []map[string]any{{"text": "q"}}},
				{Role: "model", Parts: []map[string]any{{
					"functionCall":     map[string]any{"name": "search"},
					"thoughtSignature": nil,
				}}},
			},
			want: map[string]any{"search": SkipThoughtSignature},
		},
		{
			name: "a nil functionCall does not count as the first call",
			contents: []wireContent{
				{Role: "user", Parts: []map[string]any{{"text": "q"}}},
				{Role: "model", Parts: []map[string]any{{"functionCall": nil}, call("search")}},
			},
			want: map[string]any{"search": SkipThoughtSignature},
		},
		{
			name: "a nil functionResponse part opens a new turn",
			contents: []wireContent{
				{Role: "user", Parts: []map[string]any{{"text": "first"}}},
				{Role: "model", Parts: []map[string]any{call("old")}},
				{Role: "user", Parts: []map[string]any{{"functionResponse": nil}}},
				{Role: "model", Parts: []map[string]any{call("new")}},
			},
			want: map[string]any{"old": nil, "new": SkipThoughtSignature},
		},
		{
			name: "an empty user content does not open a new turn",
			contents: []wireContent{
				{Role: "user", Parts: []map[string]any{{"text": "q"}}},
				{Role: "model", Parts: []map[string]any{call("step1")}},
				{Role: "user", Parts: nil},
				{Role: "model", Parts: []map[string]any{call("step2")}},
			},
			want: map[string]any{"step1": SkipThoughtSignature, "step2": SkipThoughtSignature},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			signCurrentTurnCalls(tc.contents)
			if got := thoughtSignatures(tc.contents); !mapsEqual(got, tc.want) {
				t.Fatalf("signatures = %v, want %v", got, tc.want)
			}
		})
	}
}

func mapsEqual(got, want map[string]any) bool {
	if len(got) != len(want) {
		return false
	}
	for name, wantValue := range want {
		gotValue, ok := got[name]
		if !ok || gotValue != wantValue {
			return false
		}
	}
	return true
}
