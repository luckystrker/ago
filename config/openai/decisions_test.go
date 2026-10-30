package openai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/response"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
)

// decisionsUsage is the usage fixture of the AG2 decisions tests
// (test_openai_decisions_config.py).
var decisionsUsage = map[string]any{
	"input_tokens":  30,
	"output_tokens": 0,
	"total_tokens":  30,
	"input_tokens_details": map[string]any{
		"cached_tokens":      0,
		"cache_write_tokens": 0,
	},
	"output_tokens_details": map[string]any{
		"reasoning_tokens": 0,
	},
}

// decisionsAnswer renders one answers[] entry (AG2 fixtures).
func decisionsAnswer(fields string) string {
	return fmt.Sprintf(`{"model":"gpt-6-luna","answers":[%s],"usage":{"input_tokens":30,"output_tokens":0,"total_tokens":30,`+
		`"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}`, fields)
}

// decisionServer answers every call with body and records the request bodies.
func decisionServer(t *testing.T, body string, seen *[][]byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		*seen = append(*seen, b)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// decisionsCall performs one Decisions call and returns the response, the
// published events and the request body the server saw.
func decisionsCall(t *testing.T, cfg *OpenAIDecisionsConfig, body string, prompt string, opts config.CallOptions) (*events.ModelResponse, *[]events.Event, map[string]any) {
	t.Helper()
	var seen [][]byte
	srvURL := decisionServer(t, body, &seen)
	cfg.APIKey = "test-key"
	cfg.BaseURL = srvURL + "/v1"
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ms := stream.NewMemoryStream()
	var sent []events.Event
	ms.Subscribe(func(ev events.Event) error {
		sent = append(sent, ev)
		return nil
	}, stream.SubscribeOptions{})
	conv := agocontext.New(ms)
	if prompt != "" {
		conv.AddPrompt(prompt)
	}
	resp, err := client.Call(context.Background(), []events.Event{
		&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "go"}}},
	}, conv, opts)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	return resp, &sent, bodyJSON(t, seen[0])
}

// --- config contract -------------------------------------------------------------

func TestNewOpenAIDecisionsDefaults(t *testing.T) {
	cfg := NewOpenAIDecisions()
	if cfg.Model() != "gpt-6-luna" {
		t.Fatalf("model default must be gpt-6-luna, got %q", cfg.Model())
	}
	if cfg.BooleanThreshold != 0.5 {
		t.Fatalf("boolean_threshold default must be 0.5, got %v", cfg.BooleanThreshold)
	}
	if cfg.Provider() != config.ProviderOpenAI || cfg.MaxRetries != 4 {
		t.Fatalf("provider/retries: %q %d", cfg.Provider(), cfg.MaxRetries)
	}
	cp, err := cfg.Clone(map[string]any{"model": "gpt-6-luna-2026-10-01", "boolean_threshold": 0.8})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if cp.Model() != "gpt-6-luna-2026-10-01" || cp.(*OpenAIDecisionsConfig).BooleanThreshold != 0.8 {
		t.Fatalf("clone overrides: %+v", cp)
	}
	if cfg.Model() != "gpt-6-luna" || cfg.BooleanThreshold != 0.5 {
		t.Fatalf("original mutated: %+v", cfg)
	}
}

func TestDecisionsNoFilesAPI(t *testing.T) {
	if _, err := NewOpenAIDecisions().CreateFilesClient(); err == nil {
		t.Fatal("CreateFilesClient must fail on the decisions config")
	}
}

// --- choice questions --------------------------------------------------------------

func TestDecisionsChoiceRoundTrip(t *testing.T) {
	// The AG2 fixture's choice answer: value, confidence and probabilities.
	body := decisionsAnswer(`{"type":"choice","name":"answer","choice":"billing","confidence":0.93,` +
		`"probabilities":[{"value":"billing","probability":0.95},{"value":"technical","probability":0.05}]}`)
	cfg := NewOpenAIDecisions()
	cfg.Descriptions = map[string]string{"billing": "Payments, invoices, and refunds.", "technical": "Problems using the product."}
	safety := "user-1"
	cfg.SafetyIdentifier = &safety

	schema := response.RawSchema(map[string]any{"type": "string", "enum": []any{"billing", "technical"}},
		response.WithRawName("answer"), response.WithRawDescription("Which department should handle this complaint?"))

	resp, sent, wire := decisionsCall(t, cfg, body, "You route customer complaints.",
		config.CallOptions{ResponseSchema: schema})

	if wire["model"] != "gpt-6-luna" {
		t.Fatalf("model not sent: %+v", wire["model"])
	}
	if wire["safety_identifier"] != "user-1" {
		t.Fatalf("safety_identifier not sent: %+v", wire["safety_identifier"])
	}
	input := wire["input"].([]any)
	msg := input[0].(map[string]any)
	if msg["type"] != "message" || msg["role"] != "user" {
		t.Fatalf("input message shape: %+v", msg)
	}
	part := msg["content"].([]any)[0].(map[string]any)
	if part["type"] != "input_text" || part["text"] != "go" {
		t.Fatalf("input text part: %+v", part)
	}

	questions := wire["questions"].([]any)
	if len(questions) != 1 {
		t.Fatalf("exactly one question per request: %+v", questions)
	}
	q := questions[0].(map[string]any)
	if q["type"] != "choice" || q["name"] != "answer" {
		t.Fatalf("question identity: %+v", q)
	}
	// The agent prompt leads, the schema's question text closes.
	if q["instructions"] != "You route customer complaints.\n\nWhich department should handle this complaint?" {
		t.Fatalf("instructions: %+v", q["instructions"])
	}
	choices := q["choices"].([]any)
	if len(choices) != 2 {
		t.Fatalf("choices: %+v", choices)
	}
	first := choices[0].(map[string]any)
	if first["value"] != "billing" || first["description"] != "Payments, invoices, and refunds." {
		t.Fatalf("first choice: %+v", first)
	}

	// The answer renders as the JSON the schema validates; the metadata keeps
	// the answer's own fields minus the type/name discriminators.
	if resp.Message == nil || resp.Message.Content != `"billing"` {
		t.Fatalf("content: %+v", resp.Message)
	}
	if resp.Message.Metadata["choice"] != "billing" || resp.Message.Metadata["confidence"] != 0.93 {
		t.Fatalf("metadata: %+v", resp.Message.Metadata)
	}
	if _, has := resp.Message.Metadata["type"]; has {
		t.Fatal("metadata must drop the type discriminator")
	}
	if len(*sent) != 1 {
		t.Fatalf("the message must go out on the stream: %+v", *sent)
	}
	if resp.Provider != "openai-decisions" || resp.Model != "gpt-6-luna" {
		t.Fatalf("tracing: %q %q", resp.Provider, resp.Model)
	}
	wantToken(t, resp.Usage.PromptTokens, 30, "prompt")
	wantToken(t, resp.Usage.CompletionTokens, 0, "completion")
	wantToken(t, resp.Usage.ThinkingTokens, 0, "thinking")
}

// --- predicate questions -------------------------------------------------------------

func TestDecisionsPredicateThreshold(t *testing.T) {
	body := decisionsAnswer(`{"type":"predicate","name":"answer","probability":0.92}`)
	schema := response.RawSchema(map[string]any{"type": "boolean"},
		response.WithRawName("answer"), response.WithRawDescription("Is the customer asking for a refund?"))

	// Above the default threshold → true; no safety_identifier is sent by default.
	cfg := NewOpenAIDecisions()
	resp, _, wire := decisionsCall(t, cfg, body, "", config.CallOptions{ResponseSchema: schema})
	if resp.Message == nil || resp.Message.Content != "true" {
		t.Fatalf("content: %+v", resp.Message)
	}
	if resp.Message.Metadata["probability"] != 0.92 {
		t.Fatalf("metadata: %+v", resp.Message.Metadata)
	}
	if _, has := wire["safety_identifier"]; has {
		t.Fatal("unset safety_identifier must not travel")
	}
	q := wire["questions"].([]any)[0].(map[string]any)
	if q["type"] != "predicate" || q["instructions"] != "Is the customer asking for a refund?" {
		t.Fatalf("predicate question: %+v", q)
	}

	// A threshold above the probability flips the answer to false.
	strict := NewOpenAIDecisions()
	strict.BooleanThreshold = 0.95
	resp, _, _ = decisionsCall(t, strict, body, "", config.CallOptions{ResponseSchema: schema})
	if resp.Message.Content != "false" {
		t.Fatalf("thresholded content: %+v", resp.Message)
	}
}

// --- score questions -----------------------------------------------------------------

func TestDecisionsScoreRoundTrip(t *testing.T) {
	body := decisionsAnswer(`{"type":"score","name":"answer","score":1.6}`)
	cfg := NewOpenAIDecisions()
	cfg.Descriptions = map[string]string{"0": "bad", "2": "great"}

	schema := response.RawSchema(map[string]any{"type": "integer", "enum": []any{0, 1, 2}},
		response.WithRawName("answer"), response.WithRawDescription("Rate the answer."))

	resp, _, wire := decisionsCall(t, cfg, body, "", config.CallOptions{ResponseSchema: schema})
	q := wire["questions"].([]any)[0].(map[string]any)
	if q["type"] != "score" {
		t.Fatalf("question type: %+v", q)
	}
	levels := q["levels"].([]any)
	if len(levels) != 3 {
		t.Fatalf("levels: %+v", levels)
	}
	// Labels are the option names, Python-capitalize'd.
	l0 := levels[0].(map[string]any)
	if l0["label"] != "0" || l0["description"] != "bad" {
		t.Fatalf("level 0: %+v", l0)
	}
	l2 := levels[2].(map[string]any)
	if l2["label"] != "2" || l2["description"] != "great" {
		t.Fatalf("level 2: %+v", l2)
	}
	// 1.6 snaps to the nearest level (2).
	if resp.Message == nil || resp.Message.Content != "2" {
		t.Fatalf("content: %+v", resp.Message)
	}

	// A rubric not numbered from 0 is rejected before the request.
	var seen [][]byte
	srvURL := decisionServer(t, body, &seen)
	bad := NewOpenAIDecisions()
	bad.APIKey = "k"
	bad.BaseURL = srvURL + "/v1"
	badSchema := response.RawSchema(map[string]any{"type": "integer", "enum": []any{1, 2, 3}},
		response.WithRawName("answer"), response.WithRawDescription("Rate the answer."))
	client, err := bad.Create()
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Call(context.Background(), []events.Event{
		&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "go"}}},
	}, agocontext.New(stream.NewMemoryStream()), config.CallOptions{ResponseSchema: badSchema})
	if !errors.Is(err, response.ErrNotADecision) {
		t.Fatalf("non-rubric score: want ErrNotADecision, got %v", err)
	}
	if len(seen) != 0 {
		t.Fatal("a rejected question must not reach the API")
	}
}

// --- refusals, unsupported schemas and tools ------------------------------------------

func TestDecisionsRefusal(t *testing.T) {
	cfg := NewOpenAIDecisions()
	schema := response.RawSchema(map[string]any{"type": "boolean"},
		response.WithRawName("answer"), response.WithRawDescription("Is this harmful?"))
	var seen [][]byte
	srvURL := decisionServer(t, decisionsAnswer(`{"type":"refusal","name":"answer"}`), &seen)
	cfg.APIKey = "k"
	cfg.BaseURL = srvURL + "/v1"
	client, err := cfg.Create()
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Call(context.Background(), []events.Event{
		&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "go"}}},
	}, agocontext.New(stream.NewMemoryStream()), config.CallOptions{ResponseSchema: schema})
	var refused *DecisionRefusedError
	if !errors.As(err, &refused) || refused.Name != "answer" {
		t.Fatalf("want DecisionRefusedError, got %v", err)
	}
}

func TestDecisionsUnsupportedSchema(t *testing.T) {
	cfg := NewOpenAIDecisions()
	schema := response.RawSchema(map[string]any{"type": "string"},
		response.WithRawName("answer"), response.WithRawDescription("Free text."))
	var seen [][]byte
	srvURL := decisionServer(t, decisionsAnswer(`{}`), &seen)
	cfg.APIKey = "k"
	cfg.BaseURL = srvURL + "/v1"
	client, err := cfg.Create()
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Call(context.Background(), []events.Event{
		&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "go"}}},
	}, agocontext.New(stream.NewMemoryStream()), config.CallOptions{ResponseSchema: schema})
	if !errors.Is(err, response.ErrNotADecision) || !strings.Contains(err.Error(), "decision-only") {
		t.Fatalf("want ErrNotADecision with guidance, got %v", err)
	}
	if len(seen) != 0 {
		t.Fatal("an unsupported schema must not reach the API")
	}
}

func TestDecisionsToolsRejectedBeforeRequest(t *testing.T) {
	cfg := NewOpenAIDecisions()
	schema := response.RawSchema(map[string]any{"type": "boolean"},
		response.WithRawName("answer"), response.WithRawDescription("yes or no"))
	var seen [][]byte
	srvURL := decisionServer(t, decisionsAnswer(`{}`), &seen)
	cfg.APIKey = "k"
	cfg.BaseURL = srvURL + "/v1"
	client, err := cfg.Create()
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Call(context.Background(), []events.Event{
		&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "go"}}},
	}, agocontext.New(stream.NewMemoryStream()), config.CallOptions{
		ResponseSchema: schema,
		Tools:          []any{tools.Schema{Name: "lookup"}},
	})
	if !errors.Is(err, config.ErrUnsupportedTool) || !strings.Contains(err.Error(), "openai-decisions") {
		t.Fatalf("want ErrUnsupportedTool naming the provider, got %v", err)
	}
	// Hosted tools are rejected the same way.
	_, err = client.Call(context.Background(), []events.Event{
		&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "go"}}},
	}, agocontext.New(stream.NewMemoryStream()), config.CallOptions{
		ResponseSchema: schema,
		Tools:          []any{tools.WebSearchToolSchema{}},
	})
	if !errors.Is(err, config.ErrUnsupportedTool) {
		t.Fatalf("hosted tool: want ErrUnsupportedTool, got %v", err)
	}
	if len(seen) != 0 {
		t.Fatal("tools must be rejected before the request")
	}
}

// --- instruction assembly and input flattening -----------------------------------------

func TestDecisionsInstructionsAndInput(t *testing.T) {
	// question_instructions joins after the prompt, before the schema question.
	body := decisionsAnswer(`{"type":"predicate","name":"answer","probability":0.9}`)
	schema := response.RawSchema(map[string]any{"type": "boolean"},
		response.WithRawName("answer"), response.WithRawDescription("Is the sky blue?"))

	cfg := NewOpenAIDecisions()
	cfg.QuestionInstructions = "Answer for a clear day."
	_, _, wire := decisionsCall(t, cfg, body, "Is the sky blue?", config.CallOptions{ResponseSchema: schema})
	q := wire["questions"].([]any)[0].(map[string]any)
	if q["instructions"] != "Is the sky blue?\n\nAnswer for a clear day.\n\nIs the sky blue?" {
		t.Fatalf("instructions: %+v", q["instructions"])
	}

	// Empty instructions leave the prompt alone; an assistant turn flattens to
	// a [Assistant]-tagged user message; tool results to a [Tool result] one.
	history := []events.Event{
		&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "go"}}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: "earlier answer"}},
		&events.ToolResultsEvent{Results: []events.ToolResultEvent{
			{ParentID: "c1", Result: events.ToolResult{Parts: []events.Input{&events.TextInput{Content: "42"}}}},
		}},
	}
	plain := NewOpenAIDecisions()
	plain.APIKey = "k"
	var seen [][]byte
	plain.BaseURL = decisionServer(t, body, &seen) + "/v1"
	client, err := plain.Create()
	if err != nil {
		t.Fatal(err)
	}
	ms := stream.NewMemoryStream()
	conv := agocontext.New(ms)
	conv.AddPrompt("Is the sky blue?")
	if _, err := client.Call(context.Background(), history, conv, config.CallOptions{ResponseSchema: schema}); err != nil {
		t.Fatalf("call: %v", err)
	}
	wire = bodyJSON(t, seen[0])
	q = wire["questions"].([]any)[0].(map[string]any)
	if q["instructions"] != "Is the sky blue?\n\nIs the sky blue?" {
		t.Fatalf("prompt-only instructions: %+v", q["instructions"])
	}
	input := wire["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input: %+v", input)
	}
	assistant := input[1].(map[string]any)
	text := assistant["content"].([]any)[0].(map[string]any)
	if text["text"] != "[Assistant]\nearlier answer" {
		t.Fatalf("assistant turn: %+v", text)
	}
	toolTurn := input[2].(map[string]any)
	parts := toolTurn["content"].([]any)
	if parts[0].(map[string]any)["text"] != "[Tool result]" || parts[1].(map[string]any)["text"] != "42" {
		t.Fatalf("tool-result turn: %+v", parts)
	}
}

func TestDecisionsImageInput(t *testing.T) {
	// Inline images travel as data-URL input_image; hosted URLs are rejected.
	cfg := NewOpenAIDecisions()
	cfg.APIKey = "k"
	var seen [][]byte
	cfg.BaseURL = decisionServer(t, decisionsAnswer(`{"type":"predicate","name":"answer","probability":0.9}`), &seen) + "/v1"
	client, err := cfg.Create()
	if err != nil {
		t.Fatal(err)
	}
	schema := response.RawSchema(map[string]any{"type": "boolean"},
		response.WithRawName("answer"), response.WithRawDescription("What is shown?"))
	history := []events.Event{
		&events.ModelRequest{Parts: []events.Input{
			&events.BinaryInput{Data: []byte("hi"), MediaType: "image/png", Kind: events.BinaryImage},
			&events.UrlInput{URL: "https://ago.example/pic.png", Kind: events.BinaryImage},
		}},
	}
	_, err = client.Call(context.Background(), history, agocontext.New(stream.NewMemoryStream()),
		config.CallOptions{ResponseSchema: schema})
	if !errors.Is(err, config.ErrUnsupportedInput) {
		t.Fatalf("hosted image url: want ErrUnsupportedInput, got %v", err)
	}
	if len(seen) != 0 {
		t.Fatal("an unmappable input must not reach the API")
	}

	history = history[:1]
	history[0] = &events.ModelRequest{Parts: []events.Input{
		&events.BinaryInput{Data: []byte("hi"), MediaType: "image/png", Kind: events.BinaryImage},
	}}
	if _, err := client.Call(context.Background(), history, agocontext.New(stream.NewMemoryStream()),
		config.CallOptions{ResponseSchema: schema}); err != nil {
		t.Fatalf("inline image call: %v", err)
	}
	msg := bodyJSON(t, seen[0])["input"].([]any)[0].(map[string]any)
	image := msg["content"].([]any)[0].(map[string]any)
	if image["type"] != "input_image" || image["image_url"] != "data:image/png;base64,aGk=" {
		t.Fatalf("image part: %+v", image)
	}
}

// --- missing question -------------------------------------------------------------------

func TestDecisionsNeedsQuestion(t *testing.T) {
	// No prompt, no instructions, no schema description → fail before the request.
	cfg := NewOpenAIDecisions()
	cfg.APIKey = "k"
	var seen [][]byte
	cfg.BaseURL = decisionServer(t, decisionsAnswer(`{}`), &seen) + "/v1"
	client, err := cfg.Create()
	if err != nil {
		t.Fatal(err)
	}
	schema := response.RawSchema(map[string]any{"type": "boolean"}, response.WithRawName("answer"))
	_, err = client.Call(context.Background(), []events.Event{
		&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "go"}}},
	}, agocontext.New(stream.NewMemoryStream()), config.CallOptions{ResponseSchema: schema})
	if err == nil || !strings.Contains(err.Error(), "a decision needs a question") {
		t.Fatalf("want the needs-question error, got %v", err)
	}
	if len(seen) != 0 {
		t.Fatal("a questionless decision must not reach the API")
	}
}
