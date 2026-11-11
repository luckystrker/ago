package openai

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/response"
	"github.com/luckystrker/ago/stream"
)

func TestDecisionsQuestionInstructionParts(t *testing.T) {
	body := decisionsAnswer(`{"type":"predicate","name":"answer","probability":0.9}`)
	schema := response.RawSchema(map[string]any{"type": "boolean"},
		response.WithRawName("answer"), response.WithRawDescription("Is it the same cat?"))

	cfg := NewOpenAIDecisions()
	cfg.QuestionInstructions = "Compare the cats."
	cfg.QuestionInstructionParts = []any{
		"Reference photo:",
		&events.UrlInput{URL: "https://ago.example/cat.png", Kind: events.BinaryImage}, // hosted: allowed here
		&events.BinaryInput{Data: []byte("hi"), MediaType: "image/png", Kind: events.BinaryImage},
		&events.DataInput{Data: map[string]any{"k": 1}},
		map[string]any{"type": "input_json", "json": map[string]any{"raw": true}}, // SDK part, passed through
	}
	_, _, wire := decisionsCall(t, cfg, body, "You judge cats.", config.CallOptions{ResponseSchema: schema})

	q := wire["questions"].([]any)[0].(map[string]any)
	want := []any{
		map[string]any{"type": "input_text", "text": "You judge cats."}, // agent prompt leads
		map[string]any{"type": "input_text", "text": "Compare the cats."},
		map[string]any{"type": "input_text", "text": "Reference photo:"},
		map[string]any{"type": "input_image", "image_url": "https://ago.example/cat.png"},
		map[string]any{"type": "input_image", "image_url": "data:image/png;base64,aGk="},
		map[string]any{"type": "input_text", "text": `{"k":1}`},
		map[string]any{"type": "input_json", "json": map[string]any{"raw": true}},
		map[string]any{"type": "input_text", "text": "Is it the same cat?"}, // schema question closes
	}
	if !reflect.DeepEqual(q["instructions"], want) {
		t.Fatalf("instructions:\n got %#v\nwant %#v", q["instructions"], want)
	}
	if q["type"] != "predicate" {
		t.Fatalf("question type: %v", q["type"])
	}
}

// With the parts form but no agent prompt and no schema question, the items
// alone are the instructions; an unmappable item fails before the request.
func TestDecisionsQuestionInstructionPartsEdges(t *testing.T) {
	body := decisionsAnswer(`{"type":"predicate","name":"answer","probability":0.9}`)
	schema := response.RawSchema(map[string]any{"type": "boolean"}, response.WithRawName("answer"))

	cfg := NewOpenAIDecisions()
	cfg.QuestionInstructionParts = []any{"Only this."}
	_, _, wire := decisionsCall(t, cfg, body, "", config.CallOptions{ResponseSchema: schema})
	got := wire["questions"].([]any)[0].(map[string]any)["instructions"]
	if !reflect.DeepEqual(got, []any{map[string]any{"type": "input_text", "text": "Only this."}}) {
		t.Fatalf("instructions: %#v", got)
	}

	for name, parts := range map[string][]any{
		"unknown type":       {42},
		"non-image url part": {&events.UrlInput{URL: "https://ago.example/a.pdf", Kind: events.BinaryDocument}},
	} {
		var seen [][]byte
		bad := NewOpenAIDecisions()
		bad.APIKey = "k"
		bad.BaseURL = decisionServer(t, body, &seen) + "/v1"
		bad.QuestionInstructionParts = parts
		client, err := bad.Create()
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Call(context.Background(), []events.Event{
			&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "go"}}},
		}, agocontext.New(stream.NewMemoryStream()), config.CallOptions{ResponseSchema: schema})
		if !errors.Is(err, config.ErrUnsupportedInput) {
			t.Fatalf("%s: want ErrUnsupportedInput, got %v", name, err)
		}
		if len(seen) != 0 {
			t.Fatalf("%s: must not reach the API", name)
		}
	}
}

func TestDecisionsCloneQuestionInstructionParts(t *testing.T) {
	cfg := NewOpenAIDecisions()
	cl, err := cfg.Clone(map[string]any{"question_instruction_parts": []any{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := cl.(*OpenAIDecisionsConfig).QuestionInstructionParts; len(got) != 1 || got[0] != "x" {
		t.Fatalf("clone: %#v", got)
	}
	if _, err := cfg.Clone(map[string]any{"QuestionInstructionParts": "nope"}); err == nil {
		t.Fatal("a string is not a parts slice")
	}
}
