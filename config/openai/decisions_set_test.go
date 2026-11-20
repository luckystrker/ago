package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/response"
	"github.com/luckystrker/ago/stream"
)

// Several questions in one request (AG2 test_decision_set_providers.py, OpenAI side).

type setTriage struct {
	IsSpam     bool   `json:"is_spam" question:"Is this message spam?"`
	Department string `json:"department" question:"Which team should handle this ticket?" agschema:"enum=billing|technical"`
	Urgency    int    `json:"urgency" question:"How urgent is this ticket?" agschema:"enum=0|1"`
}

type setChurn struct {
	Likelihood float64 `json:"likelihood" question:"How likely is this customer to churn?" agschema:"min=0,max=1"`
}

type setSender struct {
	IsCustomer bool `json:"is_customer" question:"Is the sender a customer?"`
}

type setNested struct {
	Sender setSender `json:"sender" question:"Who sent it?"`
}

type setNullable struct {
	IsSpam *bool `json:"is_spam" question:"Is this message spam?"`
}

type setListed struct {
	Flags []bool `json:"flags" question:"Which flags apply?"`
}

type setEmpty struct{}

type setUnasked struct {
	IsSpam bool `json:"is_spam"`
}

type setSkipping struct {
	Urgency int `json:"urgency" question:"How urgent is this ticket?" agschema:"enum=1|3"`
}

var (
	spamAnswer = `{"type":"predicate","name":"is_spam","probability":0.03}`
	deptAnswer = `{"type":"choice","name":"department","choice":"billing","confidence":0.74,` +
		`"probabilities":[{"value":"billing","probability":0.8},{"value":"technical","probability":0.2}]}`
	urgencyAnswer = `{"type":"score","name":"urgency","score":0.8,"confidence":0.8}`
)

// decisionsCallErr is decisionsCall for calls that may fail: it returns the error and
// every request body the server saw.
func decisionsCallErr(t *testing.T, cfg *OpenAIDecisionsConfig, body, prompt string, opts config.CallOptions) (*events.ModelResponse, [][]byte, error) {
	t.Helper()
	var seen [][]byte
	srvURL := decisionServer(t, body, &seen)
	cfg.APIKey = "test-key"
	cfg.BaseURL = srvURL + "/v1"
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	conv := agocontext.New(stream.NewMemoryStream())
	if prompt != "" {
		conv.AddPrompt(prompt)
	}
	resp, err := client.Call(context.Background(), []events.Event{
		&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "I was charged twice for my order."}}},
	}, conv, opts)
	return resp, seen, err
}

func TestDecisionsSetThreeFieldsInOneRequest(t *testing.T) {
	// Python: test_each_field_is_one_question_of_one_request + test_metadata_is_each_answer_by_field.
	schema := response.NewSchema[setTriage]("", "")
	body := decisionsAnswer(strings.Join([]string{spamAnswer, deptAnswer, urgencyAnswer}, ","))

	resp, seen, err := decisionsCallErr(t, NewOpenAIDecisions(), body, "You triage support tickets.",
		config.CallOptions{ResponseSchema: schema})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("a struct of 3 fields must be exactly 1 request, got %d", len(seen))
	}

	var wire struct {
		Questions []map[string]any `json:"questions"`
	}
	if err := json.Unmarshal(seen[0], &wire); err != nil {
		t.Fatal(err)
	}
	wantQuestions := []map[string]any{
		{"type": "predicate", "name": "is_spam", "instructions": "You triage support tickets.\n\nIs this message spam?"},
		{
			"type": "choice", "name": "department",
			"instructions": "You triage support tickets.\n\nWhich team should handle this ticket?",
			"choices":      []any{map[string]any{"value": "billing"}, map[string]any{"value": "technical"}},
		},
		{
			"type": "score", "name": "urgency",
			"instructions": "You triage support tickets.\n\nHow urgent is this ticket?",
			"levels":       []any{map[string]any{"label": "0"}, map[string]any{"label": "1"}},
		},
	}
	if !reflect.DeepEqual(wire.Questions, wantQuestions) {
		t.Fatalf("questions =\n%v\nwant\n%v", wire.Questions, wantQuestions)
	}

	// The reply validates as the struct; score 0.8 snaps to level 1, probability 0.03 < 0.5 is false.
	if want := `{"is_spam":false,"department":"billing","urgency":1}`; resp.Message.Content != want {
		t.Fatalf("content = %s, want %s", resp.Message.Content, want)
	}
	v, err := schema.Validate(resp.Message.Content, nil)
	if err != nil || v != (setTriage{IsSpam: false, Department: "billing", Urgency: 1}) {
		t.Fatalf("Validate = %+v, %v", v, err)
	}

	// metadata is {field: answer}, each in the API's own shape minus type/name.
	md := resp.Message.Metadata
	if len(md) != 3 {
		t.Fatalf("metadata keys = %v", md)
	}
	if !reflect.DeepEqual(md["is_spam"], map[string]any{"probability": 0.03}) {
		t.Fatalf("is_spam metadata = %v", md["is_spam"])
	}
	if dept, _ := md["department"].(map[string]any); dept["choice"] != "billing" || dept["confidence"] != 0.74 {
		t.Fatalf("department metadata = %v", md["department"])
	}
	if urgency, _ := md["urgency"].(map[string]any); urgency["score"] != 0.8 {
		t.Fatalf("urgency metadata = %v", md["urgency"])
	}
	if resp.Provider != "openai-decisions" || resp.Message.Metadata["is_spam"] == nil {
		t.Fatalf("tracing: %q", resp.Provider)
	}
}

func TestDecisionsSetAnswersMayComeInAnyOrder(t *testing.T) {
	body := decisionsAnswer(strings.Join([]string{urgencyAnswer, deptAnswer, spamAnswer}, ","))
	resp, _, err := decisionsCallErr(t, NewOpenAIDecisions(), body, "Q",
		config.CallOptions{ResponseSchema: response.NewSchema[setTriage]("", "")})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"is_spam":false,"department":"billing","urgency":1}`; resp.Message.Content != want {
		t.Fatalf("content = %s, want %s", resp.Message.Content, want)
	}
}

func TestDecisionsSetBooleanThresholdAppliesToEveryPredicate(t *testing.T) {
	// Python: test_boolean_threshold_applies_to_every_predicate.
	cfg := NewOpenAIDecisions()
	cfg.BooleanThreshold = 0.7
	body := decisionsAnswer(`{"type":"predicate","name":"is_spam","probability":0.6},` + deptAnswer + `,` + urgencyAnswer)
	resp, _, err := decisionsCallErr(t, cfg, body, "Q", config.CallOptions{ResponseSchema: response.NewSchema[setTriage]("", "")})
	if err != nil || !strings.HasPrefix(resp.Message.Content, `{"is_spam":false,`) {
		t.Fatalf("content = %v, err = %v", resp, err)
	}
}

func TestDecisionsSetProbabilityFieldIsReturnedRaw(t *testing.T) {
	// Python: test_probability_field_is_returned_raw.
	body := decisionsAnswer(`{"type":"predicate","name":"likelihood","probability":0.37}`)
	resp, _, err := decisionsCallErr(t, NewOpenAIDecisions(), body, "Q",
		config.CallOptions{ResponseSchema: response.NewSchema[setChurn]("", "")})
	if err != nil || resp.Message.Content != `{"likelihood":0.37}` {
		t.Fatalf("content = %v, err = %v", resp, err)
	}
}

func TestDecisionsSetRefusalOfSecondFieldFailsTheReply(t *testing.T) {
	// Python: test_refusal_of_one_field_fails_the_whole_reply.
	body := decisionsAnswer(spamAnswer + `,{"type":"refusal","name":"department"},` + urgencyAnswer)
	_, seen, err := decisionsCallErr(t, NewOpenAIDecisions(), body, "Q",
		config.CallOptions{ResponseSchema: response.NewSchema[setTriage]("", "")})
	var refused *DecisionRefusedError
	if !errors.As(err, &refused) || refused.Name != "department" {
		t.Fatalf("err = %v, want *DecisionRefusedError{department}", err)
	}
	if len(seen) != 1 {
		t.Fatalf("requests = %d, want 1", len(seen))
	}

	// A refusal wins over a missing answer: the reply has no value of the schema either way.
	body = decisionsAnswer(`{"type":"refusal","name":"urgency"}`)
	_, _, err = decisionsCallErr(t, NewOpenAIDecisions(), body, "Q",
		config.CallOptions{ResponseSchema: response.NewSchema[setTriage]("", "")})
	if !errors.As(err, &refused) || refused.Name != "urgency" {
		t.Fatalf("err = %v, want *DecisionRefusedError{urgency}", err)
	}
}

func TestDecisionsSetMissingAnswerNamesTheField(t *testing.T) {
	// Python: test_a_missing_answer_names_the_field.
	body := decisionsAnswer(spamAnswer + `,` + deptAnswer)
	_, _, err := decisionsCallErr(t, NewOpenAIDecisions(), body, "Q",
		config.CallOptions{ResponseSchema: response.NewSchema[setTriage]("", "")})
	if err == nil || !strings.Contains(err.Error(), `no answer for question "urgency"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestDecisionsSetOverTheQuestionLimit(t *testing.T) {
	// Python: test_more_than_400_fields_are_rejected (OpenAI limit on questions per request).
	_, seen, err := decisionsCallErr(t, NewOpenAIDecisions(), decisionsAnswer(""), "Q",
		config.CallOptions{ResponseSchema: response.NewSchema[decisionOverLimitFields]("", "")})
	if !errors.Is(err, response.ErrInvalidDecisionSet) || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "400") {
		t.Fatalf("err = %v", err)
	}
	if len(seen) != 0 {
		t.Fatalf("a 401-field struct must fail before the request, saw %d", len(seen))
	}
}

func TestDecisionsSetAtTheQuestionLimit(t *testing.T) {
	answers := make([]string, 0, 400)
	for i := 1; i <= 400; i++ {
		answers = append(answers, fmt.Sprintf(`{"type":"predicate","name":"f%03d","probability":0.9}`, i))
	}
	resp, seen, err := decisionsCallErr(t, NewOpenAIDecisions(), decisionsAnswer(strings.Join(answers, ",")), "Q",
		config.CallOptions{ResponseSchema: response.NewSchema[decisionLimitFields]("", "")})
	if err != nil {
		t.Fatalf("a 400-field struct is allowed: %v", err)
	}
	var wire struct {
		Questions []map[string]any `json:"questions"`
	}
	if len(seen) != 1 || json.Unmarshal(seen[0], &wire) != nil || len(wire.Questions) != 400 {
		t.Fatalf("requests = %d, questions = %d", len(seen), len(wire.Questions))
	}
	if !strings.HasPrefix(resp.Message.Content, `{"f001":true,"f002":true,`) || !strings.HasSuffix(resp.Message.Content, `"f400":true}`) {
		t.Fatalf("content = %.60s...", resp.Message.Content)
	}
}

func TestDecisionsSetRejectionsBeforeTheRequest(t *testing.T) {
	// Python TestRejections: nothing reaches the API. "numbered from 0" is the provider's rule.
	cases := []struct {
		name    string
		schema  response.Proto
		contain string
	}{
		{"nested struct", response.NewSchema[setNested]("", ""), "Sender"},
		{"pointer (Optional)", response.NewSchema[setNullable]("", ""), "IsSpam"},
		{"list", response.NewSchema[setListed]("", ""), "Flags"},
		{"empty struct", response.NewSchema[setEmpty]("", ""), "no fields"},
		{"field without a question", response.NewSchema[setUnasked]("", ""), "no question"},
		{"description on the struct", response.NewSchema[setTriage]("", "Triage this ticket."), "description of its own"},
		{"score not numbered from 0", response.NewSchema[setSkipping]("", ""), "numbered from 0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, seen, err := decisionsCallErr(t, NewOpenAIDecisions(), decisionsAnswer(""), "Q",
				config.CallOptions{ResponseSchema: c.schema})
			if !errors.Is(err, response.ErrNotADecision) {
				t.Fatalf("err = %v, want ErrNotADecision", err)
			}
			if c.name != "score not numbered from 0" && !errors.Is(err, response.ErrInvalidDecisionSet) {
				t.Fatalf("err = %v, want ErrInvalidDecisionSet", err)
			}
			if !strings.Contains(err.Error(), c.contain) {
				t.Fatalf("err = %q, want it to mention %q", err, c.contain)
			}
			if len(seen) != 0 {
				t.Fatalf("saw %d requests, want 0", len(seen))
			}
		})
	}
}

func TestDecisionsSetScoreErrorNamesTheField(t *testing.T) {
	_, _, err := decisionsCallErr(t, NewOpenAIDecisions(), decisionsAnswer(""), "Q",
		config.CallOptions{ResponseSchema: response.NewSchema[setSkipping]("", "")})
	if err == nil || !strings.Contains(err.Error(), `question "urgency"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestDecisionsSetDescriptionsAreScalarOnly(t *testing.T) {
	// ADR 0038 decision 4: option descriptions are keyed by value, which two fields may share.
	cfg := NewOpenAIDecisions()
	cfg.Descriptions = map[string]string{"billing": "Money."}
	_, seen, err := decisionsCallErr(t, cfg, decisionsAnswer(""), "Q",
		config.CallOptions{ResponseSchema: response.NewSchema[setTriage]("", "")})
	if err == nil || !strings.Contains(err.Error(), "single-question") || len(seen) != 0 {
		t.Fatalf("err = %v, requests = %d", err, len(seen))
	}
}

func TestDecisionsSetNeedsAQuestionOnlyThroughTheFieldTags(t *testing.T) {
	// The struct's fields carry their own question text, so no agent prompt is needed.
	body := decisionsAnswer(spamAnswer + `,` + deptAnswer + `,` + urgencyAnswer)
	_, seen, err := decisionsCallErr(t, NewOpenAIDecisions(), body, "",
		config.CallOptions{ResponseSchema: response.NewSchema[setTriage]("", "")})
	if err != nil || len(seen) != 1 {
		t.Fatalf("err = %v, requests = %d", err, len(seen))
	}
	if !strings.Contains(string(seen[0]), `"instructions":"Is this message spam?"`) {
		t.Fatalf("body = %s", seen[0])
	}
}

func TestDecisionsSetInstructionPartsLeadEveryQuestion(t *testing.T) {
	cfg := NewOpenAIDecisions()
	cfg.QuestionInstructionParts = []any{"Shared context."}
	body := decisionsAnswer(spamAnswer + `,` + deptAnswer + `,` + urgencyAnswer)
	_, seen, err := decisionsCallErr(t, cfg, body, "Prompt.",
		config.CallOptions{ResponseSchema: response.NewSchema[setTriage]("", "")})
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Questions []struct {
			Name         string           `json:"name"`
			Instructions []map[string]any `json:"instructions"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(seen[0], &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Questions) != 3 {
		t.Fatalf("questions = %+v", wire.Questions)
	}
	for _, q := range wire.Questions {
		if len(q.Instructions) != 3 || q.Instructions[0]["text"] != "Prompt." || q.Instructions[1]["text"] != "Shared context." {
			t.Fatalf("%s instructions = %v", q.Name, q.Instructions)
		}
	}
	if wire.Questions[1].Instructions[2]["text"] != "Which team should handle this ticket?" {
		t.Fatalf("each question closes with its own text: %v", wire.Questions[1].Instructions)
	}
}

func TestDecisionsScalarRequestBodyIsUnchanged(t *testing.T) {
	// A scalar schema still sends exactly one question named "answer"; this is the byte
	// sequence the client produced before struct schemas existed.
	const want = `{"input":[{"content":[{"text":"I was charged twice for my order.","type":"input_text"}],` +
		`"role":"user","type":"message"}],"model":"gpt-6-luna","questions":[{"choices":[{"value":"billing"},` +
		`{"value":"technical"}],"instructions":"You triage support tickets.\n\nWhich department should handle ` +
		`this complaint?","name":"answer","type":"choice"}]}`
	body := decisionsAnswer(`{"type":"choice","name":"answer","choice":"billing","confidence":0.93,"probabilities":[]}`)
	schema := response.RawSchema(map[string]any{"type": "string", "enum": []any{"billing", "technical"}},
		response.WithRawName("answer"), response.WithRawDescription("Which department should handle this complaint?"))

	resp, seen, err := decisionsCallErr(t, NewOpenAIDecisions(), body, "You triage support tickets.",
		config.CallOptions{ResponseSchema: schema})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || string(seen[0]) != want {
		t.Fatalf("request body =\n%s\nwant\n%s", seen[0], want)
	}
	if resp.Message.Content != `"billing"` || resp.Message.Metadata["choice"] != "billing" {
		t.Fatalf("reply = %q %v", resp.Message.Content, resp.Message.Metadata)
	}
}
