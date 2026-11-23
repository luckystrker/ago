package agui

import (
	stdcontext "context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/luckystrker/ago/agent"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
)

// offered are the choices the test agent's tool puts to the client. The ids
// spell the approval words with the opposite meaning: the reject option is
// called "y", the allow option "n".
var offered = []events.ApprovalChoice{
	{Value: "y", Label: "Reject"},
	{Value: "n", Label: "Allow once"},
}

// choosingTool asks a question offering `offered` and reports how the answer
// arrived: "choice:<value>" for a pick, "word:<text>" for words.
func choosingTool(choices []events.ApprovalChoice) *tools.FunctionTool {
	tool, err := tools.NewFunctionTool("ask_human", "Ask the human",
		func(args struct{}, cx *agocontext.Context) (string, error) {
			req := events.NewHumanInputRequest("git status", 0)
			req.Choices = choices
			msg, err := cx.AskMessage(stdcontext.Background(), &req)
			if err != nil {
				return "", err
			}
			if msg.IsChoice() {
				return "choice:" + msg.Content, nil
			}
			return "word:" + msg.Content, nil
		})
	if err != nil {
		panic(err)
	}
	return tool
}

func choosingServer(t *testing.T, choices []events.ApprovalChoice) http.Handler {
	t.Helper()
	srv, err := NewServer()
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	cfg := agotest.NewTestConfig(
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "c1", Name: "ask_human", Arguments: "{}"},
		}}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: "noted"}},
	)
	opts := append([]agent.Option{agent.WithTools(choosingTool(choices))}, srv.Options()...)
	return srv.Handler(agent.New("served", "You are served.", append(opts, agent.WithConfig(cfg))...))
}

func toolResult(evs []Event) string {
	var result string
	for _, ev := range evs {
		if res, ok := ev.(ToolCallResultEvent); ok {
			result, _ = res.Content.(string)
		}
	}
	return result
}

// TestInterruptOffersChoices: a question with choices carries a third branch in
// its response schema — {"choice": <one titled constant per choice>} — beside
// the string|boolean one, which keeps its meaning.
func TestInterruptOffersChoices(t *testing.T) {
	ts := httptest.NewServer(choosingServer(t, offered))
	t.Cleanup(ts.Close)
	first, _ := postRunURL(t, ts.URL, runBody)
	in := soleInterrupt(t, first)

	// Over the wire, as a client reads it.
	raw, err := json.Marshal(in.ResponseSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		AnyOf []struct {
			Type       any `json:"type"`
			Properties struct {
				Choice struct {
					OneOf []struct {
						Const string `json:"const"`
						Title string `json:"title"`
					} `json:"oneOf"`
				} `json:"choice"`
			} `json:"properties"`
			Required []string `json:"required"`
		} `json:"anyOf"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("schema %s: %v", raw, err)
	}
	if len(schema.AnyOf) != 2 {
		t.Fatalf("schema %s, want two anyOf branches", raw)
	}
	if !reflect.DeepEqual(schema.AnyOf[0].Type, []any{"string", "boolean"}) {
		t.Errorf("first branch type = %v, want string|boolean", schema.AnyOf[0].Type)
	}
	pick := schema.AnyOf[1]
	if pick.Type != "object" || !reflect.DeepEqual(pick.Required, []string{"choice"}) {
		t.Errorf("pick branch = %+v", pick)
	}
	var got []events.ApprovalChoice
	for _, c := range pick.Properties.Choice.OneOf {
		got = append(got, events.ApprovalChoice{Value: c.Const, Label: c.Title})
	}
	if !reflect.DeepEqual(got, offered) {
		t.Errorf("offered choices = %+v, want %+v", got, offered)
	}
}

// TestResumeAnswersKeepTheirKind: a pick is a pick, a boolean and a string are
// words — even when they spell a choice's value.
func TestResumeAnswersKeepTheirKind(t *testing.T) {
	tests := []struct {
		name    string
		payload any
		want    string
	}{
		{"picking the reject option whose id is y", map[string]any{"choice": "y"}, "choice:y"},
		{"picking the allow option whose id is n", map[string]any{"choice": "n"}, "choice:n"},
		{"the approve button is the word y, not the choice y", true, "word:y"},
		{"the refuse button is the word n, not the choice n", false, "word:n"},
		{"a string equal to a value stays a word", "y", "word:y"},
		{"a string that is not a value is a word", "always", "word:always"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(choosingServer(t, offered))
			t.Cleanup(ts.Close)
			first, _ := postRunURL(t, ts.URL, runBody)
			in := soleInterrupt(t, first)
			evs, _ := postRunURL(t, ts.URL, resumeBody("t1", "r2", in.ID, tc.payload))
			if got := toolResult(evs); got != tc.want {
				t.Fatalf("tool result = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResumeRefusesBadPicks: a pick that was not offered, a malformed one, or a
// pick of a question that offered none is refused, and the held turn survives.
func TestResumeRefusesBadPicks(t *testing.T) {
	bad := []struct {
		name    string
		choices []events.ApprovalChoice
		payload any
	}{
		{"a value that was not offered", offered, map[string]any{"choice": "чужое"}},
		{"a choice that is not a string", offered, map[string]any{"choice": 5}},
		{"extra keys beside the choice", offered, map[string]any{"choice": "y", "also": "x"}},
		{"an empty object", offered, map[string]any{}},
		{"a pick for a question that offered no choices", nil, map[string]any{"choice": "y"}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(choosingServer(t, tc.choices))
			t.Cleanup(ts.Close)
			first, _ := postRunURL(t, ts.URL, runBody)
			in := soleInterrupt(t, first)

			refused, _ := postRunURL(t, ts.URL, resumeBody("t1", "r2", in.ID, tc.payload))
			if got := eventTypes(refused); !equalTypes(got, []EventType{EventRunError}) {
				t.Fatalf("types = %v, want a lone RUN_ERROR", got)
			}
			if code := refused[0].(RunErrorEvent).Code; code != CodeInterruptPayloadRefused {
				t.Fatalf("code = %q, want %s", code, CodeInterruptPayloadRefused)
			}
			// Refused, not lost: a valid answer still lands.
			resumed, _ := postRunURL(t, ts.URL, resumeBody("t1", "r3", in.ID, "ok"))
			if got := toolResult(resumed); got != "word:ok" {
				t.Fatalf("the held turn did not survive the refusal: %q", got)
			}
		})
	}
}

// TestQuestionWithoutChoicesKeepsItsSchema: a plain question is answered as
// before — the string schema, and a boolean still translated (D-065).
func TestQuestionWithoutChoicesKeepsItsSchema(t *testing.T) {
	ts := httptest.NewServer(choosingServer(t, nil))
	t.Cleanup(ts.Close)
	first, _ := postRunURL(t, ts.URL, runBody)
	in := soleInterrupt(t, first)
	if in.ResponseSchema["type"] != "string" || in.Reason != ReasonInputRequired {
		t.Errorf("plain question interrupt = %+v", in)
	}
	evs, _ := postRunURL(t, ts.URL, resumeBody("t1", "r2", in.ID, true))
	if got := toolResult(evs); got != "word:y" {
		t.Errorf("boolean on a plain question = %q, want word:y", got)
	}
}
