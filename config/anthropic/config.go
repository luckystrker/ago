// Package anthropic implements the Anthropic Messages API provider: the
// AnthropicConfig model-config factory and an LLMClient speaking
// POST {base}/v1/messages over JSON and SSE (internal/httpx).
//
// Parity with Python AG2 ag2/config/anthropic/ (config.py, anthropic_client.py,
// mappers.py). The transport is raw HTTP instead of the anthropic SDK; the
// wire vocabulary (content blocks, tool_use/tool_result, cache_control) follows
// docs/specs/config-llm.md §6 and the porting brief.
package anthropic

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/luckystrker/ago/config"
)

const (
	// DefaultBaseURL is the public Messages API endpoint.
	DefaultBaseURL = "https://api.anthropic.com"
	// APIVersion is the anthropic-version header value every request carries.
	APIVersion = "2023-06-01"
	// DefaultMaxTokens caps one completion when the config leaves it unset.
	DefaultMaxTokens = int64(4096)
	// DefaultMaxRetries is the transport retry count (Python parity:
	// AnthropicConfig.max_retries).
	DefaultMaxRetries = 2
	// APIKeyEnv is read when the config carries no explicit API key.
	APIKeyEnv = "ANTHROPIC_API_KEY"

	messagesPath = "/v1/messages"
)

// AnthropicConfig is one configured Anthropic model. Immutable after
// construction; Create is safe for concurrent use and builds a fresh client
// per turn.
//
// AGO-DEVIATION: the Python dataclass carries the model as the public field
// `model`; AGo holds it privately behind the Model() accessor because the
// config.ModelConfig contract requires a Model() method and Go forbids a field
// and a method sharing one name (same resolution as the sibling providers).
// The model is set by NewAnthropic and by the Clone override "Model".
type AnthropicConfig struct {
	model string

	// APIKey authenticates requests. Empty falls back to APIKeyEnv at Create()
	// time; neither set → Create() wraps config.ErrNotProvided.
	APIKey string
	// BaseURL overrides the endpoint root. Empty → DefaultBaseURL.
	BaseURL string
	// MaxTokens caps the completion (0 → DefaultMaxTokens; the API requires
	// the field).
	MaxTokens int64
	// StopSequences end the completion when the model emits one.
	StopSequences []string
	// Temperature and TopP sample the output; nil = unset.
	// AGO-DEVIATION: the 2026 Python config deprecates these (the newer API
	// version it targets rejects them); the ported wire version
	// (2023-06-01) accepts them, so they are sent when set.
	Temperature *float64
	TopP        *float64
	// Streaming switches the call to SSE consumption.
	Streaming bool
	// Timeout caps a non-streaming request (0 = no cap; streams rely on the
	// call's context for cancellation).
	Timeout time.Duration
	// MaxRetries is the transport retry count for 429/5xx and connection
	// failures.
	MaxRetries int
	// PromptCaching injects cache_control {"type":"ephemeral"} onto the last
	// system block and the last tool.
	PromptCaching bool
}

// NewAnthropic returns a config for one model with the documented defaults
// applied (BaseURL, MaxTokens 4096, MaxRetries 2, PromptCaching true).
func NewAnthropic(model string) *AnthropicConfig {
	return &AnthropicConfig{
		model:         model,
		BaseURL:       DefaultBaseURL,
		MaxTokens:     DefaultMaxTokens,
		MaxRetries:    DefaultMaxRetries,
		PromptCaching: true,
	}
}

// Provider names the vendor (Python parity: ModelProvider.ANTHROPIC).
func (c *AnthropicConfig) Provider() config.Provider { return config.ProviderAnthropic }

// Model returns the model identifier sent to the API.
func (c *AnthropicConfig) Model() string { return c.model }

// Create resolves the env fallbacks and builds a ready-to-call client (Python
// parity: a fresh client per create()). A missing model or API key (explicit
// or env) is an error wrapping config.ErrNotProvided.
func (c *AnthropicConfig) Create() (config.LLMClient, error) {
	if c.model == "" {
		return nil, fmt.Errorf("%w: model", config.ErrNotProvided)
	}
	key := c.APIKey
	if key == "" {
		key = os.Getenv(APIKeyEnv)
	}
	if key == "" {
		return nil, fmt.Errorf("%w: api key (set APIKey or %s)", config.ErrNotProvided, APIKeyEnv)
	}
	maxTokens := c.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	baseURL := c.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		model:         c.model,
		apiKey:        key,
		baseURL:       strings.TrimSuffix(baseURL, "/"),
		maxTokens:     maxTokens,
		stopSequences: c.StopSequences,
		temperature:   c.Temperature,
		topP:          c.TopP,
		streaming:     c.Streaming,
		timeout:       c.Timeout,
		maxRetries:    c.MaxRetries,
		promptCaching: c.PromptCaching,
	}, nil
}

// Clone returns a copy with overrides applied (Python parity:
// dataclasses.replace). Keys match the exported field names
// case-insensitively ("Model", "model", ...); for a pointer field the plain
// element type is accepted too (float64 → *float64) and nil clears the field.
// Unknown keys and mismatched types are errors.
func (c *AnthropicConfig) Clone(overrides map[string]any) (config.ModelConfig, error) {
	cp := *c
	if c.StopSequences != nil {
		cp.StopSequences = append([]string(nil), c.StopSequences...)
	}
	for key, value := range overrides {
		if err := cp.applyOverride(key, value); err != nil {
			return nil, err
		}
	}
	return &cp, nil
}

// applyOverride sets one field by case-insensitive name. The private model
// field is handled explicitly; the exported fields via reflection.
func (c *AnthropicConfig) applyOverride(key string, value any) error {
	if strings.EqualFold(key, "model") {
		s, ok := value.(string)
		if !ok {
			return fmt.Errorf("config: override %q: want string, got %T", key, value)
		}
		c.model = s
		return nil
	}
	rv := reflect.ValueOf(c).Elem()
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		field := rt.Field(i)
		if !field.IsExported() || !strings.EqualFold(field.Name, key) {
			continue
		}
		fv := rv.Field(i)
		if value == nil {
			if field.Type.Kind() != reflect.Pointer && field.Type.Kind() != reflect.Slice {
				return fmt.Errorf("config: override %q: field does not accept nil", key)
			}
			fv.Set(reflect.Zero(field.Type))
			return nil
		}
		v := reflect.ValueOf(value)
		switch {
		case v.Type().AssignableTo(field.Type):
			fv.Set(v)
		case field.Type.Kind() == reflect.Pointer && v.Type().AssignableTo(field.Type.Elem()):
			w := reflect.New(field.Type.Elem())
			w.Elem().Set(v)
			fv.Set(w)
		default:
			return fmt.Errorf("config: override %q: want %s, got %T", key, field.Type, value)
		}
		return nil
	}
	return fmt.Errorf("config: unknown AnthropicConfig override %q", key)
}

// Compile-time check of the ModelConfig contract.
var _ config.ModelConfig = (*AnthropicConfig)(nil)
