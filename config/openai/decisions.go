// The OpenAI Decisions API: one typed question per call, taken from the
// agent's response schema. Parity with
// ag2/ag2/config/openai/{openai_decisions_client,decisions_mappers}.py and the
// OpenAIDecisionsConfig dataclass in config.py.
//
// The endpoint answers decision-only schemas (bool predicate, string choice,
// score levels numbered from 0); every tool is rejected before the request.

package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/luckystrker/ago/files"
	"github.com/luckystrker/ago/internal/httpx"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/response"
	"github.com/luckystrker/ago/tools"
)

// Compile-time conformance with the provider contracts.
var (
	_ config.ModelConfig = (*OpenAIDecisionsConfig)(nil)
	_ config.LLMClient   = (*DecisionsClient)(nil)
)

// decisionsPath is the URL path under the base URL (DefaultBaseURL already
// carries /v1).
const decisionsPath = "/decisions"

// decisionsProvider stamps ModelResponse.Provider for this client (Python
// parity: PROVIDER = "openai-decisions").
const decisionsProvider = config.Provider("openai-decisions")

// decisionAnswerKey is the name of the single question sent per request.
const decisionAnswerKey = "answer"

// DefaultDecisionsModel is the model the decisions config defaults to
// (Python parity: the dataclass default).
const DefaultDecisionsModel = "gpt-6-luna"

// DecisionRefusedError reports the Decisions API declining to answer the
// question (Python parity: DecisionRefusedError). The Name field carries the
// refused question's name.
type DecisionRefusedError struct {
	Name string
}

func (e *DecisionRefusedError) Error() string {
	return fmt.Sprintf("config/openai: the OpenAI Decisions API refused to answer question %q", e.Name)
}

// unsupportedDecisionSchemaMsg is the guidance carried by every
// not-a-decision error (Python parity: UnsupportedResponseSchemaError).
const unsupportedDecisionSchemaMsg = "the OpenAI Decisions API is decision-only: use bool, " +
	"a string enum, an int enum numbered from 0, or a 0..1 number schema as the response schema"

// OpenAIDecisionsConfig configures the Decisions API. Construct it with
// NewOpenAIDecisions (which applies the defaults) and assign the exported
// fields. Immutable after construction: Create is safe for concurrent use.
//
// AGO-DEVIATION: the model travels behind the Model() accessor like on
// OpenAICompletionsConfig (the config.ModelConfig contract owns the name);
// question_instructions carries strings only — the Python sequence form
// (Input parts / SDK parts, e.g. an image inside the question) is deferred
// (D-060).
type OpenAIDecisionsConfig struct {
	// APIKey authenticates requests. Empty falls back to OPENAI_API_KEY at
	// Create() time; neither set → Create() wraps config.ErrNotProvided.
	APIKey string
	// BaseURL overrides the endpoint root. Empty → DefaultBaseURL.
	BaseURL string

	// Organization and Project become the OpenAI-Organization and
	// OpenAI-Project request headers.
	Organization string
	Project      string

	// Timeout caps a request (0 = no cap).
	Timeout time.Duration
	// MaxRetries is the transport retry count for 429/5xx and connection
	// failures (SDK default, applied by NewOpenAIDecisions).
	MaxRetries int

	// DefaultHeaders are sent with every request on top of the auth and
	// organization/project headers; an entry here overrides those.
	DefaultHeaders map[string]string

	// SafetyIdentifier travels as safety_identifier when set; nil omits it
	// (Python parity: str | None | Omit = omit).
	SafetyIdentifier *string

	// BooleanThreshold is where a bool schema's probability turns true
	// (Python parity: boolean_threshold).
	BooleanThreshold float64

	// Descriptions are option descriptions keyed by choice value or score
	// level as a string; they outrank the schema options' own descriptions.
	Descriptions map[string]string

	// QuestionInstructions is what the question carries after the agent
	// prompt; the schema's question text is appended last.
	QuestionInstructions string

	// ExtraBody merges into the request body last (SDK parity: extra_body).
	ExtraBody map[string]any

	model string
}

// NewOpenAIDecisions returns a Decisions config with the Python defaults:
// model gpt-6-luna, boolean threshold 0.5, 4 transport retries.
func NewOpenAIDecisions() *OpenAIDecisionsConfig {
	return &OpenAIDecisionsConfig{
		model:            DefaultDecisionsModel,
		BooleanThreshold: 0.5,
		MaxRetries:       4,
	}
}

// Provider reports the vendor (ModelConfig contract).
func (c *OpenAIDecisionsConfig) Provider() config.Provider { return config.ProviderOpenAI }

// Model returns the model identifier sent with every call (ModelConfig
// contract).
func (c *OpenAIDecisionsConfig) Model() string { return c.model }

// Create builds a ready-to-call client (ModelConfig contract). The API key
// comes from the config, then OPENAI_API_KEY; a missing key wraps
// config.ErrNotProvided. The BaseURL defaults to DefaultBaseURL.
func (c *OpenAIDecisionsConfig) Create() (config.LLMClient, error) {
	key, baseURL, headers, err := decisionsCredentials(c.APIKey, c.BaseURL, c.Organization, c.Project, c.DefaultHeaders)
	if err != nil {
		return nil, err
	}
	return &DecisionsClient{
		apiKey:  key,
		baseURL: baseURL,
		headers: headers,
		params:  c,
	}, nil
}

// CreateFilesClient always fails: the endpoint takes inline data URLs only,
// an uploaded file_id is rejected (Python parity: NotImplementedError).
func (c *OpenAIDecisionsConfig) CreateFilesClient() (files.Client, error) {
	return nil, fmt.Errorf("config/openai: OpenAIDecisionsConfig does not support Files API")
}

// Clone returns a copy with the overrides applied (ModelConfig contract;
// Python parity: dataclasses.replace). Keys are the Go field names, with the
// Python snake_case wire names accepted as aliases. An unknown key or a
// mismatched value is an error and the original config is left untouched.
func (c *OpenAIDecisionsConfig) Clone(overrides map[string]any) (config.ModelConfig, error) {
	cp := *c
	for k, v := range overrides {
		if err := cp.set(k, v); err != nil {
			return nil, err
		}
	}
	return &cp, nil
}

// set applies one override to cp (see Clone). A nil value clears the field
// (Python parity: dataclasses.replace(x=None)).
func (c *OpenAIDecisionsConfig) set(key string, v any) error {
	str := func(dst *string) error {
		if v == nil {
			*dst = ""
			return nil
		}
		s, ok := v.(string)
		if !ok {
			return overrideTypeErr(key, v, "string")
		}
		*dst = s
		return nil
	}
	strMap := func(dst *map[string]string) error {
		if v == nil {
			*dst = nil
			return nil
		}
		m, ok := v.(map[string]string)
		if !ok {
			return overrideTypeErr(key, v, "map[string]string")
		}
		*dst = m
		return nil
	}
	switch key {
	case "Model", "model":
		return str(&c.model)
	case "APIKey", "api_key":
		return str(&c.APIKey)
	case "BaseURL", "base_url":
		return str(&c.BaseURL)
	case "Organization", "organization":
		return str(&c.Organization)
	case "Project", "project":
		return str(&c.Project)
	case "Timeout", "timeout":
		d, ok := v.(time.Duration)
		if !ok {
			return overrideTypeErr(key, v, "time.Duration")
		}
		c.Timeout = d
	case "MaxRetries", "max_retries":
		n, ok := v.(int)
		if !ok {
			return overrideTypeErr(key, v, "int")
		}
		c.MaxRetries = n
	case "DefaultHeaders", "default_headers":
		return strMap(&c.DefaultHeaders)
	case "SafetyIdentifier", "safety_identifier":
		if v == nil {
			c.SafetyIdentifier = nil
			return nil
		}
		s, ok := v.(*string)
		if !ok {
			return overrideTypeErr(key, v, "*string")
		}
		c.SafetyIdentifier = s
	case "BooleanThreshold", "boolean_threshold":
		f, ok := v.(float64)
		if !ok {
			return overrideTypeErr(key, v, "float64")
		}
		c.BooleanThreshold = f
	case "Descriptions", "descriptions":
		return strMap(&c.Descriptions)
	case "QuestionInstructions", "question_instructions":
		return str(&c.QuestionInstructions)
	case "ExtraBody", "extra_body":
		if v == nil {
			c.ExtraBody = nil
			return nil
		}
		m, ok := v.(map[string]any)
		if !ok {
			return overrideTypeErr(key, v, "map[string]any")
		}
		c.ExtraBody = m
	default:
		return fmt.Errorf("config/openai: unknown override key %q", key)
	}
	return nil
}

// DecisionsClient is the Decisions API client built by Create: one configured
// endpoint with its auth and headers, stateless per call. Safe for concurrent
// use. Implements config.LLMClient.
type DecisionsClient struct {
	apiKey  string
	baseURL string
	headers map[string]string
	params  *OpenAIDecisionsConfig
}

// Call performs one Decisions API call: the conversation is flattened into
// user-role input messages, the response schema becomes the single question,
// and the answer renders as the JSON the schema validates. Streaming does not
// exist on this endpoint; a nil conv is a headless call.
func (c *DecisionsClient) Call(ctx context.Context, messages []events.Event, conv *agocontext.Context, opts config.CallOptions) (*events.ModelResponse, error) {
	p := c.params

	// The Decisions API does not call tools: every tool is rejected before
	// the request (Python parity: tool_to_api raising per tool).
	for _, t := range opts.Tools {
		if err := rejectDecisionTool(t); err != nil {
			return nil, err
		}
	}

	spec, err := response.FromResponse(opts.ResponseSchema)
	if err != nil {
		return nil, fmt.Errorf("config/openai: %w: %s", err, unsupportedDecisionSchemaMsg)
	}

	var prompt []string
	if conv != nil { // a nil conv is a headless call
		prompt = conv.PromptLines()
	}
	instructions := decisionInstructions(p.QuestionInstructions, prompt, spec.Question)
	if instructions == "" {
		// Every question type requires instructions; fail before the request
		// with a way out (Python parity).
		return nil, fmt.Errorf("config/openai: a decision needs a question: set the agent prompt, " +
			"set QuestionInstructions, or give the response schema a description")
	}

	question, err := decisionQuestion(spec, instructions, p.Descriptions)
	if err != nil {
		return nil, err
	}

	input, err := decisionsInput(messages)
	if err != nil {
		return nil, err
	}

	body := map[string]any{
		"model":     p.model,
		"input":     input,
		"questions": []any{question},
	}
	if p.SafetyIdentifier != nil {
		body["safety_identifier"] = *p.SafetyIdentifier
	}
	for k, v := range p.ExtraBody {
		body[k] = v
	}

	headers := map[string]string{"Authorization": "Bearer " + c.apiKey}
	for k, v := range c.headers {
		headers[k] = v
	}

	client := &httpx.Client{MaxRetries: p.MaxRetries, Timeout: p.Timeout}
	status, data, err := client.PostJSON(ctx, joinURL(c.baseURL, decisionsPath), headers, body)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("config/openai: decisions returned %d: %s", status, bodySnippet(data))
	}

	var raw struct {
		Model   string          `json:"model"`
		Answers []answerValue   `json:"answers"`
		Usage   *map[string]any `json:"usage"`
		Error   *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("config/openai: decoding decision: %w", err)
	}
	if raw.Error != nil && raw.Error.Message != "" {
		return nil, fmt.Errorf("%w: %s", config.ErrStreamError, raw.Error.Message)
	}

	answer, err := findAnswer(raw.Answers)
	if err != nil {
		return nil, err
	}

	value, err := decisionAnswerValue(spec, answer, p.BooleanThreshold)
	if err != nil {
		return nil, err
	}
	content, err := spec.Render(value)
	if err != nil {
		return nil, fmt.Errorf("config/openai: rendering decision answer: %w", err)
	}

	msg := &events.ModelMessage{Content: content, Metadata: answerMetadata(answer)}
	if err := sendEvent(conv, msg); err != nil {
		return nil, err
	}

	resp := &events.ModelResponse{
		Message:  msg,
		Provider: string(decisionsProvider),
	}
	if raw.Usage != nil {
		resp.Usage = normalizeResponsesUsage(*raw.Usage)
	}
	resp.Model = raw.Model
	return resp, nil
}

// --- wire shapes -----------------------------------------------------------------

// answerValue is one entry of the decision's answers array. The typed fields
// carry what the answer renderer needs; the object itself (minus type/name)
// becomes the ModelMessage metadata, so the rest travels through as-is.
type answerValue struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Probability *float64       `json:"probability"`
	Choice      *string        `json:"choice"`
	Score       *float64       `json:"score"`
	Extra       map[string]any `json:"-"`
}

// UnmarshalJSON keeps every field the renderer does not need in Extra, so the
// metadata preserves the answer's own shape (confidence, probabilities, ...).
func (a *answerValue) UnmarshalJSON(data []byte) error {
	type plain struct {
		Type        string   `json:"type"`
		Name        string   `json:"name"`
		Probability *float64 `json:"probability"`
		Choice      *string  `json:"choice"`
		Score       *float64 `json:"score"`
	}
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	a.Type, a.Name, a.Probability, a.Choice, a.Score = p.Type, p.Name, p.Probability, p.Choice, p.Score
	if err := json.Unmarshal(data, &a.Extra); err != nil {
		return err
	}
	delete(a.Extra, "type")
	delete(a.Extra, "name")
	return nil
}

// findAnswer locates the answer to the one question sent, failing when the API
// declined it (Python parity: find_answer).
func findAnswer(answers []answerValue) (*answerValue, error) {
	var answer *answerValue
	for i := range answers {
		if answers[i].Name == decisionAnswerKey {
			answer = &answers[i]
			break
		}
	}
	if answer == nil {
		if len(answers) != 1 {
			return nil, fmt.Errorf("config/openai: OpenAI Decisions returned no answer for question %q", decisionAnswerKey)
		}
		answer = &answers[0]
	}
	if answer.Type == "refusal" {
		return nil, &DecisionRefusedError{Name: answer.Name}
	}
	return answer, nil
}

// decisionAnswerValue maps an answer onto the schema's value: the thresholded
// bool (or raw probability) for a predicate, the choice string, the nearest
// score level (Python parity: answer_to_content).
func decisionAnswerValue(spec *response.DecisionSpec, answer *answerValue, threshold float64) (any, error) {
	switch answer.Type {
	case "predicate":
		prob := 0.0
		if answer.Probability != nil {
			prob = *answer.Probability
		}
		return spec.PredicateValue(prob, threshold), nil
	case "choice":
		if answer.Choice == nil {
			return nil, fmt.Errorf("config/openai: choice answer carries no choice value")
		}
		return *answer.Choice, nil
	case "score":
		score := 0.0
		if answer.Score != nil {
			score = *answer.Score
		}
		return spec.SnapScore(score), nil
	default:
		return nil, fmt.Errorf("config/openai: unsupported decision answer type %q", answer.Type)
	}
}

// answerMetadata is the answer object minus its type/name discriminators
// (Python parity: answer_metadata).
func answerMetadata(answer *answerValue) map[string]any {
	return answer.Extra
}

// --- question building -------------------------------------------------------------

// decisionInstructions assembles the question's instructions: the agent prompt
// leads, the configured instructions follow, the schema's question text closes
// (Python parity: with_prompt + the string form of _question_instructions).
// Empty pieces drop out; an empty result means there is no question at all.
func decisionInstructions(configured string, prompt []string, question string) string {
	// Python parity: "\n".join(s for s in context.prompt if s).
	var lines []string
	for _, s := range prompt {
		if s != "" {
			lines = append(lines, s)
		}
	}
	agentPrompt := strings.Join(lines, "\n")

	pieces := make([]string, 0, 3)
	for _, s := range []string{agentPrompt, configured, question} {
		if s != "" {
			pieces = append(pieces, s)
		}
	}
	return strings.Join(pieces, "\n\n")
}

// decisionQuestion converts the spec into the single wire question: predicate,
// choice or score (Python parity: response_proto_to_question).
func decisionQuestion(spec *response.DecisionSpec, instructions string, descriptions map[string]string) (map[string]any, error) {
	question := map[string]any{
		"name":         decisionAnswerKey,
		"instructions": instructions,
	}
	switch spec.Kind {
	case response.DecisionPredicate:
		question["type"] = "predicate"
		return question, nil

	case response.DecisionChoice:
		question["type"] = "choice"
		choices := make([]any, 0, len(spec.Options))
		for _, option := range spec.Options {
			choice := map[string]any{"value": fmt.Sprint(option.Value)}
			if d := optionDescription(option, descriptions); d != "" {
				choice["description"] = d
			}
			choices = append(choices, choice)
		}
		question["choices"] = choices
		return question, nil

	case response.DecisionScore:
		// The API numbers levels by position, so the rubric must already be
		// 0..n-1 with at least two levels.
		if !spec.IsRubric() || len(spec.Options) < 2 {
			return nil, fmt.Errorf("config/openai: %w: %s: a score needs 2 or more levels numbered from 0",
				response.ErrNotADecision, unsupportedDecisionSchemaMsg)
		}
		question["type"] = "score"
		levels := make([]any, 0, len(spec.Options))
		for _, option := range spec.Options {
			level := map[string]any{"label": pythonicCapitalize(option.Name)}
			if d := optionDescription(option, descriptions); d != "" {
				level["description"] = d
			}
			levels = append(levels, level)
		}
		question["levels"] = levels
		return question, nil
	}
	return nil, fmt.Errorf("config/openai: %w: unknown decision kind %q", response.ErrNotADecision, spec.Kind)
}

// optionDescription resolves the description of one choice/level: the
// config's keyed description outranks the schema option's own.
func optionDescription(option response.DecisionOption, descriptions map[string]string) string {
	if d, ok := descriptions[fmt.Sprint(option.Value)]; ok {
		return d
	}
	return option.Description
}

// pythonicCapitalize mirrors Python's str.capitalize: the first rune upper,
// the rest lower.
func pythonicCapitalize(s string) string {
	s = strings.ReplaceAll(s, "_", " ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

// rejectDecisionTool fails on every tool schema: the Decisions API answers
// questions, it never calls tools (Python parity: tool_to_api).
func rejectDecisionTool(tool any) error {
	switch t := tool.(type) {
	case tools.Schema:
		return fmt.Errorf("%w: function %q for the %s API", config.ErrUnsupportedTool, t.Name, decisionsProvider)
	case *tools.Schema:
		if t == nil {
			return fmt.Errorf("%w: nil function schema for the %s API", config.ErrUnsupportedTool, decisionsProvider)
		}
		return fmt.Errorf("%w: function %q for the %s API", config.ErrUnsupportedTool, t.Name, decisionsProvider)
	case tools.ToolSchema:
		return fmt.Errorf("%w: tool %q for the %s API", config.ErrUnsupportedTool, t.SchemaType(), decisionsProvider)
	default:
		return fmt.Errorf("%w: unsupported schema type %T for the %s API", config.ErrUnsupportedTool, tool, decisionsProvider)
	}
}

// --- input mapping -----------------------------------------------------------------

// decisionsInput flattens the conversation into Decisions input messages: the
// API accepts only the user role, so earlier assistant turns and tool results
// travel as user messages tagged with where they came from (Python parity:
// convert_input).
func decisionsInput(messages []events.Event) ([]any, error) {
	out := make([]any, 0, len(messages))
	for _, ev := range messages {
		switch e := events.AsValue(ev).(type) {
		case events.ModelRequest:
			blocks, err := decisionParts(e.Parts)
			if err != nil {
				return nil, err
			}
			out = append(out, decisionUserMessage(blocks))
		case events.DrainedModelRequest:
			blocks, err := decisionParts(e.Parts)
			if err != nil {
				return nil, err
			}
			out = append(out, decisionUserMessage(blocks))
		case events.CompactionSummary:
			out = append(out, decisionUserMessage([]any{
				decisionText("[Summary of earlier conversation]\n" + e.Content),
			}))
		case events.ModelResponse:
			if e.Message != nil && e.Message.Content != "" {
				out = append(out, decisionUserMessage([]any{
					decisionText("[Assistant]\n" + e.Message.Content),
				}))
			}
		case events.ToolResultsEvent:
			for _, r := range e.Results {
				blocks, err := decisionParts(r.Result.Parts)
				if err != nil {
					return nil, err
				}
				msg := []any{decisionText("[Tool result]")}
				out = append(out, decisionUserMessage(append(msg, blocks...)))
			}
		}
	}
	return out, nil
}

func decisionUserMessage(content []any) map[string]any {
	return map[string]any{"type": "message", "role": "user", "content": content}
}

func decisionText(text string) map[string]any {
	return map[string]any{"type": "input_text", "text": text}
}

// decisionParts maps one batch of input parts: text and JSON as text, inline
// images as data-URL input_image, and an inline data: URL image as input_image
// — hosted image URLs are rejected by the API, so a plain URL fails (Python
// parity: _part).
func decisionParts(parts []events.Input) ([]any, error) {
	out := make([]any, 0, len(parts))
	for _, part := range parts {
		switch p := events.AsValue(part).(type) {
		case events.TextInput:
			out = append(out, decisionText(p.Content))
		case events.DataInput:
			b, err := json.Marshal(p.Data)
			if err != nil {
				return nil, fmt.Errorf("config/openai: encoding DataInput: %w", err)
			}
			out = append(out, decisionText(string(b)))
		case events.BinaryInput:
			if p.Kind != events.BinaryImage {
				return nil, fmt.Errorf("%w: BinaryInput(%s) for the %s API", config.ErrUnsupportedInput, p.Kind, decisionsProvider)
			}
			image := map[string]any{"type": "input_image", "image_url": dataURL(p.MediaType, p.Data)}
			detail, err := imageDetail(p, responsesImageDetails)
			if err != nil {
				return nil, err
			}
			if detail != "" {
				image["detail"] = detail
			}
			out = append(out, image)
		case events.UrlInput:
			if p.Kind == events.BinaryImage && strings.HasPrefix(p.URL, "data:") {
				out = append(out, map[string]any{"type": "input_image", "image_url": p.URL})
				continue
			}
			return nil, fmt.Errorf("%w: UrlInput(%s) for the %s API", config.ErrUnsupportedInput, p.Kind, decisionsProvider)
		default:
			return nil, fmt.Errorf("%w: %T for the %s API", config.ErrUnsupportedInput, part, decisionsProvider)
		}
	}
	return out, nil
}

// decisionsCredentials resolves the shared auth/base/header triple.
func decisionsCredentials(apiKey, baseURL, organization, project string, defaultHeaders map[string]string) (string, string, map[string]string, error) {
	key := apiKey
	if key == "" {
		key = os.Getenv(envAPIKey)
	}
	if key == "" {
		return "", "", nil, fmt.Errorf("config/openai: %w: api key (set APIKey or %s)", config.ErrNotProvided, envAPIKey)
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	headers := map[string]string{}
	if organization != "" {
		headers["OpenAI-Organization"] = organization
	}
	if project != "" {
		headers["OpenAI-Project"] = project
	}
	for k, v := range defaultHeaders {
		headers[k] = v
	}
	return key, strings.TrimSuffix(baseURL, "/"), headers, nil
}
