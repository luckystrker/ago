// Package openai is the OpenAI provider: the Chat Completions config (and any
// endpoint speaking the same protocol), the Responses API config and client
// (responses.go — the surface that carries hosted tools), the Decisions API
// config and client (decisions.go), the Files API client and the strict
// JSON-schema coercion used for structured output.
//
// Chat Completions parity with Python AG2 ag2/config/openai/config.py
// (OpenAICompletionsConfig) and openai_client.py. The chat wire protocol
// itself — history→messages, tool schemas, SSE streaming with tool-call delta
// merge, usage normalization — is the shared engine in ago/config
// (config.RunChatCompat); this package supplies the provider identity, auth,
// headers and the OpenAI-specific schema rules.
package openai

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/luckystrker/ago/config"
)

// Compile-time conformance with the provider contracts.
var (
	_ config.ModelConfig = (*OpenAICompletionsConfig)(nil)
	_ config.LLMClient   = (*Client)(nil)
)

// DefaultBaseURL is the Chat Completions endpoint root used when BaseURL is
// empty. The engine appends /chat/completions to it.
const DefaultBaseURL = "https://api.openai.com/v1"

// envAPIKey names the environment variable the API key falls back to
// (SDK parity: the Python client reads the same variable).
const envAPIKey = "OPENAI_API_KEY"

// OpenAICompletionsConfig configures the Chat Completions API. Construct it
// with NewOpenAICompletions (the only way to set the model) and assign the
// exported fields. Immutable after construction: Create is safe for
// concurrent use and may be called repeatedly.
//
// AGO-DEVIATION: the Python dataclass carries the model as the public field
// `model`; AGo holds it privately behind the Model() accessor because the
// config.ModelConfig contract requires a Model() method and Go forbids a
// field and a method sharing one name. The private field also forces
// constructor use, so the defaults below always apply.
type OpenAICompletionsConfig struct {
	// APIKey authenticates requests. Empty falls back to OPENAI_API_KEY at
	// Create() time; neither set → Create() wraps config.ErrNotProvided.
	APIKey string
	// BaseURL overrides the endpoint root (compatible endpoints). Empty →
	// DefaultBaseURL.
	BaseURL string

	Temperature *float64
	TopP        *float64
	MaxTokens   *int64
	// MaxCompletionTokens travels as the wire field max_completion_tokens.
	MaxCompletionTokens *int64
	Stop                []string
	Seed                *int64
	// User travels as the wire field user when non-empty.
	User string
	// ParallelToolCalls travels as parallel_tool_calls.
	ParallelToolCalls *bool

	// ResponseFormat overrides the wire response_format wholesale. When nil, a
	// per-call response schema becomes a strict json_schema block (see
	// ResponseFormat).
	ResponseFormat map[string]any

	// Streaming switches the call to SSE consumption.
	Streaming bool
	// Timeout caps a non-streaming request (0 = no cap; streams rely on the
	// call's context for cancellation).
	Timeout time.Duration
	// MaxRetries is the transport retry count for 408/409/429/5xx and connection
	// failures (SDK default, applied by NewOpenAICompletions).
	MaxRetries int

	// Organization and Project become the OpenAI-Organization and
	// OpenAI-Project request headers.
	Organization string
	Project      string

	// DefaultHeaders are sent with every request on top of the auth and
	// organization/project headers; an entry here overrides those.
	DefaultHeaders map[string]string

	// ExtraBody merges into the request body last, overriding any engine or
	// config field (SDK parity: extra_body).
	ExtraBody map[string]any

	// HTTPClient performs the requests of Create and CreateFilesClient when
	// non-nil (proxy, custom root CAs, mTLS). It owns its own Timeout and
	// redirect policy; Timeout above applies only to the default client.
	// Nil keeps the default client (SDK parity: http_client).
	HTTPClient *http.Client
	// DefaultQuery is appended to every request URL (SDK parity:
	// default_query).
	DefaultQuery map[string]string

	// The typed Chat Completions options below travel under their wire names
	// when set; the zero value is not sent. ExtraBody still wins.
	// ToolChoice is a mode string ("auto", "none", "required") or a map
	// ({"type":"function","function":{"name":"f"}}).
	ToolChoice any
	// ReasoningEffort is "none", "minimal", "low", "medium", "high" or "xhigh".
	ReasoningEffort                   string
	FrequencyPenalty, PresencePenalty *float64
	// N asks for several completions; the client reads the first only.
	N              *int64
	Logprobs       *bool
	TopLogprobs    *int64
	LogitBias      map[string]int64
	Metadata       map[string]string
	Modalities     []string
	Prediction     map[string]any
	PromptCacheKey string
	// PromptCacheOptions travels only when non-empty (a model without the
	// feature answers an empty object with a 400).
	PromptCacheOptions map[string]any
	SafetyIdentifier   string
	// ServiceTier is "auto", "default", "flex", "scale", "priority" or "fast".
	ServiceTier string
	Store       *bool
	// Verbosity is "low", "medium" or "high".
	Verbosity        string
	WebSearchOptions map[string]any

	model string
}

// NewOpenAICompletions returns a Chat Completions config for model, with the
// SDK defaults: streaming off, no timeout cap, 4 transport retries.
func NewOpenAICompletions(model string) *OpenAICompletionsConfig {
	return &OpenAICompletionsConfig{model: model, MaxRetries: 4}
}

// Provider reports the vendor (ModelConfig contract).
func (c *OpenAICompletionsConfig) Provider() config.Provider { return config.ProviderOpenAI }

// InputDialect implements config.InputDialectConfig: Chat Completions and
// Responses share one provider but not one input vocabulary.
func (c *OpenAICompletionsConfig) InputDialect() config.InputDialect {
	return config.DialectOpenAICompletions
}

// Model returns the model identifier sent with every call (ModelConfig
// contract).
func (c *OpenAICompletionsConfig) Model() string { return c.model }

// Create builds a ready-to-call client (ModelConfig contract; Python parity:
// a fresh client per create()). The API key comes from the config, then
// OPENAI_API_KEY; a missing model or key wraps config.ErrNotProvided. The
// BaseURL defaults to DefaultBaseURL.
func (c *OpenAICompletionsConfig) Create() (config.LLMClient, error) {
	if c.model == "" {
		return nil, fmt.Errorf("config/openai: %w: model", config.ErrNotProvided)
	}
	key := c.APIKey
	if key == "" {
		key = os.Getenv(envAPIKey)
	}
	if key == "" {
		return nil, fmt.Errorf("config/openai: %w: api key (set APIKey or %s)", config.ErrNotProvided, envAPIKey)
	}
	baseURL := c.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}

	headers := map[string]string{}
	if c.Organization != "" {
		headers["OpenAI-Organization"] = c.Organization
	}
	if c.Project != "" {
		headers["OpenAI-Project"] = c.Project
	}
	for k, v := range c.DefaultHeaders {
		headers[k] = v
	}

	// ExtraBody wins over every derived field (SDK parity: extra_body merges
	// last); the engine applies it after the typed options.
	extra := c.ExtraBody

	return &Client{
		params: config.CompatParams{
			APIKey:  key,
			BaseURL: baseURL,
			Model:   c.model,
			// Provider stamps ModelResponse.Provider for usage accounting.
			Provider: config.ProviderOpenAI,

			ExtraHeaders: headers,

			Temperature:       c.Temperature,
			TopP:              c.TopP,
			MaxTokens:         c.MaxTokens,
			Stop:              c.Stop,
			ParallelToolCalls: c.ParallelToolCalls,

			ToolChoice:          c.ToolChoice,
			ReasoningEffort:     c.ReasoningEffort,
			Seed:                c.Seed,
			MaxCompletionTokens: c.MaxCompletionTokens,
			FrequencyPenalty:    c.FrequencyPenalty,
			PresencePenalty:     c.PresencePenalty,
			User:                c.User,
			N:                   c.N,
			Logprobs:            c.Logprobs,
			TopLogprobs:         c.TopLogprobs,
			LogitBias:           c.LogitBias,
			Metadata:            c.Metadata,
			Modalities:          c.Modalities,
			Prediction:          c.Prediction,
			PromptCacheKey:      c.PromptCacheKey,
			PromptCacheOptions:  c.PromptCacheOptions,
			SafetyIdentifier:    c.SafetyIdentifier,
			ServiceTier:         c.ServiceTier,
			Store:               c.Store,
			Verbosity:           c.Verbosity,
			WebSearchOptions:    c.WebSearchOptions,

			ResponseFormat: c.ResponseFormat,
			Streaming:      c.Streaming,
			Timeout:        c.Timeout,
			MaxRetries:     c.MaxRetries,
			HTTPClient:     c.HTTPClient,
			Query:          c.DefaultQuery,

			Extra: extra,
		},
	}, nil
}

// Clone returns a copy with the overrides applied (ModelConfig contract;
// Python parity: dataclasses.replace). Keys are the Go field names, with the
// Python snake_case wire names accepted as aliases; values must keep the
// field's type (e.g. *float64 for Temperature, time.Duration for Timeout).
// An unknown key or a mismatched value is an error and the original config is
// left untouched.
func (c *OpenAICompletionsConfig) Clone(overrides map[string]any) (config.ModelConfig, error) {
	cp := *c
	for k, v := range overrides {
		if err := cp.set(k, v); err != nil {
			return nil, err
		}
	}
	return &cp, nil
}

// set applies one override to cp. Both spellings are accepted because the
// config vocabulary is Python's and the struct is Go's.
func (c *OpenAICompletionsConfig) set(key string, v any) error {
	switch key {
	case "Model", "model":
		s, ok := v.(string)
		if !ok {
			return overrideTypeErr(key, v, "string")
		}
		c.model = s
	case "APIKey", "api_key":
		s, ok := v.(string)
		if !ok {
			return overrideTypeErr(key, v, "string")
		}
		c.APIKey = s
	case "BaseURL", "base_url":
		s, ok := v.(string)
		if !ok {
			return overrideTypeErr(key, v, "string")
		}
		c.BaseURL = s
	case "Temperature", "temperature":
		f, ok := v.(*float64)
		if !ok {
			return overrideTypeErr(key, v, "*float64")
		}
		c.Temperature = f
	case "TopP", "top_p":
		f, ok := v.(*float64)
		if !ok {
			return overrideTypeErr(key, v, "*float64")
		}
		c.TopP = f
	case "MaxTokens", "max_tokens":
		n, ok := v.(*int64)
		if !ok {
			return overrideTypeErr(key, v, "*int64")
		}
		c.MaxTokens = n
	case "MaxCompletionTokens", "max_completion_tokens":
		n, ok := v.(*int64)
		if !ok {
			return overrideTypeErr(key, v, "*int64")
		}
		c.MaxCompletionTokens = n
	case "Stop", "stop":
		s, ok := v.([]string)
		if !ok {
			return overrideTypeErr(key, v, "[]string")
		}
		c.Stop = s
	case "Seed", "seed":
		n, ok := v.(*int64)
		if !ok {
			return overrideTypeErr(key, v, "*int64")
		}
		c.Seed = n
	case "User", "user":
		s, ok := v.(string)
		if !ok {
			return overrideTypeErr(key, v, "string")
		}
		c.User = s
	case "ParallelToolCalls", "parallel_tool_calls":
		b, ok := v.(*bool)
		if !ok {
			return overrideTypeErr(key, v, "*bool")
		}
		c.ParallelToolCalls = b
	case "ResponseFormat", "response_format":
		m, ok := v.(map[string]any)
		if !ok {
			return overrideTypeErr(key, v, "map[string]any")
		}
		c.ResponseFormat = m
	case "Streaming", "streaming":
		b, ok := v.(bool)
		if !ok {
			return overrideTypeErr(key, v, "bool")
		}
		c.Streaming = b
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
	case "Organization", "organization":
		s, ok := v.(string)
		if !ok {
			return overrideTypeErr(key, v, "string")
		}
		c.Organization = s
	case "Project", "project":
		s, ok := v.(string)
		if !ok {
			return overrideTypeErr(key, v, "string")
		}
		c.Project = s
	case "DefaultHeaders", "default_headers":
		m, ok := v.(map[string]string)
		if !ok {
			return overrideTypeErr(key, v, "map[string]string")
		}
		c.DefaultHeaders = m
	case "ExtraBody", "extra_body":
		m, ok := v.(map[string]any)
		if !ok {
			return overrideTypeErr(key, v, "map[string]any")
		}
		c.ExtraBody = m
	case "HTTPClient", "http_client":
		if v == nil {
			c.HTTPClient = nil
			return nil
		}
		h, ok := v.(*http.Client)
		if !ok {
			return overrideTypeErr(key, v, "*http.Client")
		}
		c.HTTPClient = h
	case "DefaultQuery", "default_query":
		m, ok := v.(map[string]string)
		if !ok {
			return overrideTypeErr(key, v, "map[string]string")
		}
		c.DefaultQuery = m
	case "ToolChoice", "tool_choice":
		c.ToolChoice = v
	case "ReasoningEffort", "reasoning_effort":
		return setString(key, v, &c.ReasoningEffort)
	case "PromptCacheKey", "prompt_cache_key":
		return setString(key, v, &c.PromptCacheKey)
	case "SafetyIdentifier", "safety_identifier":
		return setString(key, v, &c.SafetyIdentifier)
	case "ServiceTier", "service_tier":
		return setString(key, v, &c.ServiceTier)
	case "Verbosity", "verbosity":
		return setString(key, v, &c.Verbosity)
	case "FrequencyPenalty", "frequency_penalty":
		return setPtr(key, v, &c.FrequencyPenalty, "*float64")
	case "PresencePenalty", "presence_penalty":
		return setPtr(key, v, &c.PresencePenalty, "*float64")
	case "N", "n":
		return setPtr(key, v, &c.N, "*int64")
	case "TopLogprobs", "top_logprobs":
		return setPtr(key, v, &c.TopLogprobs, "*int64")
	case "Logprobs", "logprobs":
		return setPtr(key, v, &c.Logprobs, "*bool")
	case "Store", "store":
		return setPtr(key, v, &c.Store, "*bool")
	case "LogitBias", "logit_bias":
		return setMap(key, v, &c.LogitBias, "map[string]int64")
	case "Metadata", "metadata":
		return setMap(key, v, &c.Metadata, "map[string]string")
	case "Prediction", "prediction":
		return setMap(key, v, &c.Prediction, "map[string]any")
	case "PromptCacheOptions", "prompt_cache_options":
		return setMap(key, v, &c.PromptCacheOptions, "map[string]any")
	case "WebSearchOptions", "web_search_options":
		return setMap(key, v, &c.WebSearchOptions, "map[string]any")
	case "Modalities", "modalities":
		if v == nil {
			c.Modalities = nil
			return nil
		}
		m, ok := v.([]string)
		if !ok {
			return overrideTypeErr(key, v, "[]string")
		}
		c.Modalities = m
	default:
		return fmt.Errorf("config/openai: unknown override key %q", key)
	}
	return nil
}

func overrideTypeErr(key string, v any, want string) error {
	return fmt.Errorf("config/openai: override %q: want %s, got %T", key, want, v)
}

// setString assigns a string override; nil clears the field.
func setString(key string, v any, dst *string) error {
	if v == nil {
		*dst = ""
		return nil
	}
	str, ok := v.(string)
	if !ok {
		return overrideTypeErr(key, v, "string")
	}
	*dst = str
	return nil
}

// setPtr assigns a pointer-typed override; nil clears the field.
func setPtr[T any](key string, v any, dst **T, want string) error {
	if v == nil {
		*dst = nil
		return nil
	}
	p, ok := v.(*T)
	if !ok {
		return overrideTypeErr(key, v, want)
	}
	*dst = p
	return nil
}

// setMap assigns a map-typed override; nil clears the field.
func setMap[M ~map[string]V, V any](key string, v any, dst *M, want string) error {
	if v == nil {
		*dst = nil
		return nil
	}
	m, ok := v.(M)
	if !ok {
		return overrideTypeErr(key, v, want)
	}
	*dst = m
	return nil
}
