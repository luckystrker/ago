// Package mistral wires Mistral's chat-completions API into the AGo config
// contract. Mistral speaks an OpenAI Chat-Completions dialect, so its client
// is a thin binding of MistralConfig onto the shared engine
// config.RunChatCompat.
//
// Parity with Python AG2 ag2/config/mistral/config.py (MistralConfig): the
// sampled fields, the typed routing knobs and the vendor extras (guardrails,
// prediction, prompt_mode, logprobs). The Files API stays out of scope (D-021).
// See docs/specs/config-llm.md §1.
package mistral

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/luckystrker/ago/config"
)

// Defaults (Python parity: the mistralai SDK server URL with the version
// segment folded into the base, per the porting brief).
const (
	// DefaultBaseURL is the Mistral API root used when MistralConfig.BaseURL
	// is empty. The engine appends /chat/completions to it, which lands on the
	// same endpoint as Python's MISTRAL_DEFAULT_SERVER_URL ("https://api.mistral.ai")
	// plus the SDK's /v1 segment.
	DefaultBaseURL = "https://api.mistral.ai/v1"
	// EnvAPIKey names the environment variable consulted when
	// MistralConfig.APIKey is empty (Python parity: "api_key falls back to the
	// MISTRAL_API_KEY environment variable").
	EnvAPIKey = "MISTRAL_API_KEY"
	// chatPath is the Chat-Completions endpoint under BaseURL.
	chatPath = "/chat/completions"
)

// MistralConfig configures one Mistral model (Python parity: the MistralConfig
// dataclass). Construct with NewMistral — the only way to set the model — and
// assign the exported fields. A config is immutable once built: Create and
// Clone never mutate the receiver, so one value is safe to share across
// goroutines.
//
// AGO-DEVIATION: the Python dataclass carries the model as the public field
// `model`; AGo holds it privately behind the Model() accessor because the
// config.ModelConfig contract requires a Model() method and Go forbids a
// field and a method sharing one name.
type MistralConfig struct {
	// APIKey authenticates requests. Empty falls back to EnvAPIKey at Create()
	// time; neither set → Create() wraps config.ErrNotProvided.
	APIKey string
	// BaseURL overrides the endpoint root. Empty → DefaultBaseURL.
	BaseURL string

	// Timeout caps a non-streaming request (0 = no cap; streams rely on the
	// call's context for cancellation). Python parity: timeout_ms.
	Timeout time.Duration
	// HTTPClient performs the requests when non-nil (proxy, custom root CAs,
	// mTLS). It owns its own Timeout and redirect policy; Timeout above
	// applies only to the default client. Python parity: async_client.
	HTTPClient *http.Client
	// HTTPHeaders are sent on every request (Python parity: http_headers).
	HTTPHeaders map[string]string

	Temperature *float64 // nil = unset, not sent
	TopP        *float64 // nil = unset, not sent
	MaxTokens   *int64   // nil = unset, not sent

	// The knobs below are not sent when unset (Python parity: the
	// MistralConfig fields of the same names).
	// Stop sequences. AGO-DEVIATION: Python accepts `str | list[str]`; AGo takes
	// a list, and a single sequence is a one-element slice (the wire value is
	// always a JSON array, which every provider accepts).
	Stop              []string
	RandomSeed        *int64
	PresencePenalty   *float64
	FrequencyPenalty  *float64
	RepetitionPenalty *float64
	TopK              *int64
	MinTokens         *int64
	// N asks for several completions; the client reads the first only.
	N           *int64
	TopLogprobs *int64
	// ServiceTier is "auto" or "standard_only".
	ServiceTier string
	// Metadata is the request metadata dict.
	Metadata          map[string]any
	ParallelToolCalls *bool
	// ToolChoice is a mode string ("auto", "none", "any", "required") or a
	// map selecting one tool.
	ToolChoice any
	// ReasoningEffort is "none", "minimal", "low", "medium", "high" or "xhigh".
	ReasoningEffort string
	PromptCacheKey  string
	// Guardrails are Mistral's content-moderation configs; each entry is the
	// SDK's GuardrailConfig object (Python parity: guardrails).
	Guardrails []map[string]any
	// Prediction is the predicted-output block (Python parity: prediction).
	Prediction map[string]any
	// PromptMode selects Mistral's reasoning prompt mode; the only value is
	// "reasoning" (Python parity: Literal["reasoning"]).
	PromptMode string
	// Logprobs asks for token log probabilities; TopLogprobs caps how many
	// alternatives come back per token.
	Logprobs *bool

	// SafePrompt asks Mistral to prepend its safety prompt to the messages.
	// AGO-DEVIATION: Python's field is tri-state (None = omit, True/False =
	// send); the Go bool cannot express "explicitly false", so the field is
	// sent on the wire only when true and omitted otherwise.
	SafePrompt bool

	Streaming bool

	model string
}

// NewMistral returns a config for model with the Mistral defaults (base URL).
func NewMistral(model string) *MistralConfig {
	return &MistralConfig{
		model:   model,
		BaseURL: DefaultBaseURL,
	}
}

// Provider reports the vendor (ModelConfig contract).
func (c *MistralConfig) Provider() config.Provider { return config.ProviderMistral }

// Model returns the model identifier sent to the API (ModelConfig contract).
func (c *MistralConfig) Model() string { return c.model }

// Create implements config.ModelConfig: it validates the required values and
// binds them into a fresh client (Python parity: fresh client per create()).
// The API key falls back to EnvAPIKey when the field is empty; a missing model
// or key is an error wrapping config.ErrNotProvided.
func (c *MistralConfig) Create() (config.LLMClient, error) {
	params, err := c.params()
	if err != nil {
		return nil, err
	}
	return &chatClient{params: params}, nil
}

// params renders the shared-engine parameters for this config.
func (c *MistralConfig) params() (config.CompatParams, error) {
	if c.model == "" {
		return config.CompatParams{}, fmt.Errorf("%w: mistral: model", config.ErrNotProvided)
	}
	key := c.APIKey
	if key == "" {
		key = os.Getenv(EnvAPIKey)
	}
	if key == "" {
		return config.CompatParams{}, fmt.Errorf("%w: mistral: api key (set %s)", config.ErrNotProvided, EnvAPIKey)
	}
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	p := config.CompatParams{
		APIKey:       key,
		BaseURL:      base,
		Path:         chatPath,
		Model:        c.model,
		Provider:     config.ProviderMistral,
		Temperature:  c.Temperature,
		TopP:         c.TopP,
		MaxTokens:    c.MaxTokens,
		Streaming:    c.Streaming,
		Timeout:      c.Timeout,
		HTTPClient:   c.HTTPClient,
		ExtraHeaders: c.HTTPHeaders,

		// Wire names shared with the Chat Completions dialect.
		Stop:              c.Stop,
		PresencePenalty:   c.PresencePenalty,
		FrequencyPenalty:  c.FrequencyPenalty,
		N:                 c.N,
		TopLogprobs:       c.TopLogprobs,
		ServiceTier:       c.ServiceTier,
		ParallelToolCalls: c.ParallelToolCalls,
		ToolChoice:        c.ToolChoice,
		ReasoningEffort:   c.ReasoningEffort,
		PromptCacheKey:    c.PromptCacheKey,
		Logprobs:          c.Logprobs,
		Prediction:        c.Prediction,
	}
	// Mistral-only wire fields (the mistralai SDK's request model names).
	extra := map[string]any{}
	if c.SafePrompt {
		// Omitted when false (see the field comment).
		extra["safe_prompt"] = true
	}
	if c.RandomSeed != nil {
		extra["random_seed"] = *c.RandomSeed
	}
	if c.RepetitionPenalty != nil {
		extra["repetition_penalty"] = *c.RepetitionPenalty
	}
	if c.TopK != nil {
		extra["top_k"] = *c.TopK
	}
	if c.MinTokens != nil {
		extra["min_tokens"] = *c.MinTokens
	}
	if c.Metadata != nil {
		extra["metadata"] = c.Metadata
	}
	if len(c.Guardrails) > 0 {
		extra["guardrails"] = c.Guardrails
	}
	if c.PromptMode != "" {
		extra["prompt_mode"] = c.PromptMode
	}
	if len(extra) > 0 {
		p.Extra = extra
	}
	return p, nil
}

// Clone implements config.ModelConfig (Python parity: dataclasses.replace): a
// copy with overrides applied, receiver untouched. Keys are Go field
// names, matched case-insensitively; an unknown key or a wrongly typed value
// is an error. A pointer field
// also accepts its value type (wrapped) and nil (cleared).
func (c *MistralConfig) Clone(overrides map[string]any) (config.ModelConfig, error) {
	out := *c
	for k, v := range overrides {
		var err error
		switch strings.ToLower(k) {
		case "model":
			err = setString(&out.model, k, v)
		case "apikey":
			err = setString(&out.APIKey, k, v)
		case "baseurl":
			err = setString(&out.BaseURL, k, v)
		case "temperature":
			err = setF64(&out.Temperature, k, v)
		case "topp":
			err = setF64(&out.TopP, k, v)
		case "maxtokens":
			err = setI64(&out.MaxTokens, k, v)
		case "safeprompt":
			err = setBool(&out.SafePrompt, k, v)
		case "streaming":
			err = setBool(&out.Streaming, k, v)
		case "timeout":
			err = setDuration(&out.Timeout, k, v)
		case "httpclient", "http_client":
			err = setHTTPClient(&out.HTTPClient, k, v)
		case "httpheaders", "http_headers":
			err = setStringMap(&out.HTTPHeaders, k, v)
		case "stop":
			err = setStrings(&out.Stop, k, v)
		case "randomseed", "random_seed":
			err = setI64(&out.RandomSeed, k, v)
		case "presencepenalty", "presence_penalty":
			err = setF64(&out.PresencePenalty, k, v)
		case "frequencypenalty", "frequency_penalty":
			err = setF64(&out.FrequencyPenalty, k, v)
		case "repetitionpenalty", "repetition_penalty":
			err = setF64(&out.RepetitionPenalty, k, v)
		case "topk", "top_k":
			err = setI64(&out.TopK, k, v)
		case "mintokens", "min_tokens":
			err = setI64(&out.MinTokens, k, v)
		case "n":
			err = setI64(&out.N, k, v)
		case "toplogprobs", "top_logprobs":
			err = setI64(&out.TopLogprobs, k, v)
		case "servicetier", "service_tier":
			err = setString(&out.ServiceTier, k, v)
		case "metadata":
			err = setAnyMap(&out.Metadata, k, v)
		case "paralleltoolcalls", "parallel_tool_calls":
			err = setBoolPtr(&out.ParallelToolCalls, k, v)
		case "toolchoice", "tool_choice":
			out.ToolChoice = v
		case "reasoningeffort", "reasoning_effort":
			err = setString(&out.ReasoningEffort, k, v)
		case "promptcachekey", "prompt_cache_key":
			err = setString(&out.PromptCacheKey, k, v)
		case "guardrails":
			err = setAnyMaps(&out.Guardrails, k, v)
		case "prediction":
			err = setAnyMap(&out.Prediction, k, v)
		case "logprobs":
			err = setBoolPtr(&out.Logprobs, k, v)
		case "promptmode", "prompt_mode":
			if v == nil {
				out.PromptMode = ""
				break
			}
			s, ok := v.(string)
			if !ok {
				err = fmt.Errorf("config/mistral: override %q: got %T, want string", k, v)
				break
			}
			// Python parity: the field is Literal["reasoning"]; the dynamic
			// override boundary enforces the vocabulary.
			if s != "reasoning" {
				return nil, fmt.Errorf("mistral: override %q must be \"reasoning\", got %q", k, s)
			}
			out.PromptMode = s
		default:
			err = fmt.Errorf("config/mistral: unknown config field %q", k)
		}
		if err != nil {
			return nil, err
		}
	}
	return &out, nil
}
