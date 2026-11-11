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
	"errors"
	"fmt"
	"net/http"
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

// ErrInvalidThinking reports a ThinkingConfig the request cannot express (an
// unknown mode, or an enabled budget the API will refuse).
var ErrInvalidThinking = errors.New("anthropic: invalid thinking config")

// ThinkingType is the extended-thinking mode of a request.
type ThinkingType string

const (
	// ThinkingDisabled asks the API for no extended thinking.
	ThinkingDisabled ThinkingType = "disabled"
	// ThinkingEnabled spends a fixed budget of thinking tokens.
	ThinkingEnabled ThinkingType = "enabled"
	// ThinkingAdaptive lets the model decide whether and how much to think.
	ThinkingAdaptive ThinkingType = "adaptive"
)

// ThinkingConfig is the request's `thinking` object. Nil leaves the field off
// the wire (the API's own default).
type ThinkingConfig struct {
	Type ThinkingType
	// BudgetTokens is the thinking allowance for ThinkingEnabled. The API wants
	// a positive value below MaxTokens.
	BudgetTokens int64
}

// validate refuses a thinking mode the request cannot express. A nil config is
// valid (thinking off).
func (t *ThinkingConfig) validate(maxTokens int64) error {
	if t == nil {
		return nil
	}
	switch t.Type {
	case ThinkingDisabled, ThinkingAdaptive:
		return nil
	case ThinkingEnabled:
		if t.BudgetTokens <= 0 {
			return fmt.Errorf("%w: enabled thinking needs BudgetTokens > 0", ErrInvalidThinking)
		}
		if t.BudgetTokens >= maxTokens {
			return fmt.Errorf("%w: BudgetTokens (%d) must be less than MaxTokens (%d)",
				ErrInvalidThinking, t.BudgetTokens, maxTokens)
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown type %q (want %q, %q or %q)", ErrInvalidThinking,
			t.Type, ThinkingEnabled, ThinkingAdaptive, ThinkingDisabled)
	}
}

// body renders the request's `thinking` field.
func (t *ThinkingConfig) body() map[string]any {
	w := map[string]any{"type": string(t.Type)}
	if t.Type == ThinkingEnabled {
		w["budget_tokens"] = t.BudgetTokens
	}
	return w
}

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
	// (2023-06-01) accepts them, so they are sent when set. With extended
	// thinking on they are dropped instead: the API samples at temperature 1
	// there, and AG2's own API version sends neither.
	Temperature *float64
	TopP        *float64
	// Streaming switches the call to SSE consumption.
	Streaming bool
	// Timeout caps a non-streaming request (0 = no cap; streams rely on the
	// call's context for cancellation).
	Timeout time.Duration
	// MaxRetries is the transport retry count for 408/409/429/5xx and connection
	// failures.
	MaxRetries int
	// PromptCaching injects cache_control {"type":"ephemeral"} onto the last
	// system block and the last tool.
	PromptCaching bool
	// NativeStructuredOutput sends the call's response schema as the API's
	// output_config (a json_schema format the model is constrained to), on top
	// of the schema's prompt text. Off keeps the prompt-driven path, where an
	// off-schema answer is caught by the retry above the client.
	// AGO-DEVIATION: Python always sends output_config; AGo makes it opt-in
	// because older models answer it with a 400 and the request body of an
	// existing caller must not change under it.
	NativeStructuredOutput bool
	// DefaultHeaders travel on every request (Python parity: default_headers).
	// A case-insensitive `anthropic-beta` value is folded into the request's
	// beta header together with the framework's own flags.
	DefaultHeaders map[string]string
	// Metadata is the request `metadata` object (Python parity: metadata; the
	// API's one documented key is user_id). Nil omits it; an empty map sends {}.
	Metadata map[string]string
	// ServiceTier selects the API's service tier (e.g. "standard_only");
	// empty omits the field.
	ServiceTier string
	// Speed selects the inference speed ("standard" or "fast"); empty omits
	// the field. Some models gate it behind an anthropic-beta default header.
	Speed string
	// Thinking enables extended thinking; nil leaves the request without it.
	Thinking *ThinkingConfig
	// CacheDiagnostics asks the API to explain prompt-cache misses and emits a
	// CacheDiagnostics event for every reported one (Python parity:
	// cache_diagnostics).
	CacheDiagnostics bool
	// ExtraBody merges into the request body last, reaching fields the config
	// has no typed slot for (Python parity: extra_body — in Python it is the
	// only way to enable thinking, for one). A key that collides with a
	// derived body field replaces it, as the SDK's merge does.
	// It overrides `stream` too: "stream" here without Streaming = SSE reply on the non-streaming path (as in the SDK).
	ExtraBody map[string]any
	// HTTPClient performs the requests of Create and CreateFilesClient when
	// non-nil (proxy, custom root CAs, mTLS). It owns its own Timeout and
	// redirect policy; Timeout above applies only to the default client.
	// Nil keeps the default client (Python parity: http_client).
	HTTPClient *http.Client
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
	if err := c.Thinking.validate(maxTokens); err != nil {
		return nil, err
	}
	baseURL := c.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		model:            c.model,
		apiKey:           key,
		baseURL:          strings.TrimSuffix(baseURL, "/"),
		maxTokens:        maxTokens,
		stopSequences:    c.StopSequences,
		temperature:      c.Temperature,
		topP:             c.TopP,
		streaming:        c.Streaming,
		timeout:          c.Timeout,
		maxRetries:       c.MaxRetries,
		promptCaching:    c.PromptCaching,
		nativeOutput:     c.NativeStructuredOutput,
		defaultHeaders:   copyMap(c.DefaultHeaders),
		metadata:         copyMap(c.Metadata),
		serviceTier:      c.ServiceTier,
		speed:            c.Speed,
		thinking:         c.Thinking,
		cacheDiagnostics: c.CacheDiagnostics,
		extraBody:        copyAnyMap(c.ExtraBody),
		httpClient:       c.HTTPClient,
	}, nil
}

// copyAnyMap returns a shallow copy of m (nil stays nil), so a client never
// shares a map a caller may still mutate.
func copyAnyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// copyMap returns a copy of m (nil stays nil), so a client never shares a map a
// caller may still mutate.
func copyMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
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
	cp.DefaultHeaders = copyMap(c.DefaultHeaders)
	cp.Metadata = copyMap(c.Metadata)
	cp.ExtraBody = copyAnyMap(c.ExtraBody)
	if c.Thinking != nil {
		thinking := *c.Thinking
		cp.Thinking = &thinking
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
	if fieldKey(key) == "model" {
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
		if !field.IsExported() || fieldKey(field.Name) != fieldKey(key) {
			continue
		}
		fv := rv.Field(i)
		if value == nil {
			if field.Type.Kind() != reflect.Pointer && field.Type.Kind() != reflect.Slice && field.Type.Kind() != reflect.Map {
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

// fieldKey normalizes an override key for matching: the Python dataclass names
// are snake_case and the Go fields are CamelCase, so underscores and case are
// ignored ("http_client" and "HTTPClient" name one field).
func fieldKey(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "_", ""))
}

// Compile-time check of the ModelConfig contract.
var _ config.ModelConfig = (*AnthropicConfig)(nil)
