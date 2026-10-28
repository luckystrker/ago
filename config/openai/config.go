// Package openai is the Chat Completions provider for the OpenAI API and any
// endpoint speaking the same protocol: the typed config, its client factory
// and the strict JSON-schema coercion used for structured output.
//
// Parity with Python AG2 ag2/config/openai/config.py (OpenAICompletionsConfig)
// and openai_client.py. The wire protocol itself — history→messages, tool
// schemas, SSE streaming with tool-call delta merge, usage normalization — is
// the shared engine in ago/config (config.RunChatCompat); this package
// supplies the provider identity, auth, headers and the OpenAI-specific
// schema rules.
package openai

import (
	"fmt"
	"os"
	"time"

	"ago/config"
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
	// MaxRetries is the transport retry count for 429/5xx and connection
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

	model string
}

// NewOpenAICompletions returns a Chat Completions config for model, with the
// SDK defaults: streaming off, no timeout cap, 4 transport retries.
func NewOpenAICompletions(model string) *OpenAICompletionsConfig {
	return &OpenAICompletionsConfig{model: model, MaxRetries: 4}
}

// Provider reports the vendor (ModelConfig contract).
func (c *OpenAICompletionsConfig) Provider() config.Provider { return config.ProviderOpenAI }

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

	extra := map[string]any{}
	// Sampling fields the shared engine has no typed slot for travel as plain
	// body fields (Python parity: regular create options).
	if c.MaxCompletionTokens != nil {
		extra["max_completion_tokens"] = *c.MaxCompletionTokens
	}
	if c.Seed != nil {
		extra["seed"] = *c.Seed
	}
	if c.User != "" {
		extra["user"] = c.User
	}
	// ExtraBody wins over the derived fields (SDK parity: extra_body merges
	// last).
	for k, v := range c.ExtraBody {
		extra[k] = v
	}

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

			ResponseFormat: c.ResponseFormat,
			Streaming:      c.Streaming,
			Timeout:        c.Timeout,
			MaxRetries:     c.MaxRetries,

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
	default:
		return fmt.Errorf("config/openai: unknown override key %q", key)
	}
	return nil
}

func overrideTypeErr(key string, v any, want string) error {
	return fmt.Errorf("config/openai: override %q: want %s, got %T", key, want, v)
}
