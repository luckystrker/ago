// Package gemini implements the Google Gemini provider over the raw
// generateContent REST API (v1beta) — no SDK. It maps the AGo event history to
// contents parts, function schemas to functionDeclarations and the response
// back onto a ModelResponse. See docs/specs/config-llm.md §1/§6.
package gemini

import (
	"fmt"
	"os"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/internal/httpx"
)

// DefaultBaseURL is the Gemini API root used when no BaseURL is configured.
const DefaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"

// EnvAPIKey names the environment variable read when no API key is configured
// (Python parity: the SDK resolves the key from the environment).
const EnvAPIKey = "GEMINI_API_KEY"

// GeminiConfig is one configured Gemini model (Python parity: the GeminiConfig
// dataclass). A config is immutable after construction and safe for concurrent
// use; Create builds a fresh client per turn.
//
// AGO-DEVIATION: the brief sketches exported struct fields, but the ModelConfig
// contract requires a Model() accessor and Go forbids a field and a method with
// the same name on one type. Fields are unexported and set through NewGemini
// and the Option funcs.
type GeminiConfig struct {
	model            string
	apiKey           string
	baseURL          string
	temperature      *float64
	topP             *float64
	topK             *int64
	maxOutputTokens  *int64
	stopSequences    []string
	streaming        bool
	presencePenalty  *float64
	frequencyPenalty *float64
	seed             *int64
	thinkingLevel    *string
	thinkingBudget   *int64
}

// Option sets one field of a GeminiConfig at construction time.
type Option func(*GeminiConfig)

// WithAPIKey overrides the API key ("" keeps the constructor value).
func WithAPIKey(key string) Option { return func(c *GeminiConfig) { c.apiKey = key } }

// WithBaseURL overrides the API root ("" keeps the default).
func WithBaseURL(url string) Option { return func(c *GeminiConfig) { c.baseURL = url } }

// WithTemperature sets the sampling temperature.
func WithTemperature(v float64) Option { return func(c *GeminiConfig) { c.temperature = &v } }

// WithTopP sets the nucleus-sampling probability mass.
func WithTopP(v float64) Option { return func(c *GeminiConfig) { c.topP = &v } }

// WithTopK sets the top-k candidate count.
func WithTopK(v int64) Option { return func(c *GeminiConfig) { c.topK = &v } }

// WithMaxOutputTokens caps the generated tokens.
func WithMaxOutputTokens(v int64) Option { return func(c *GeminiConfig) { c.maxOutputTokens = &v } }

// WithStopSequences sets the stop sequences.
func WithStopSequences(v ...string) Option {
	return func(c *GeminiConfig) { c.stopSequences = append([]string(nil), v...) }
}

// WithStreaming toggles the streaming transport (streamGenerateContent?alt=sse).
func WithStreaming(v bool) Option { return func(c *GeminiConfig) { c.streaming = v } }

// WithPresencePenalty sets the presence penalty.
func WithPresencePenalty(v float64) Option { return func(c *GeminiConfig) { c.presencePenalty = &v } }

// WithFrequencyPenalty sets the frequency penalty.
func WithFrequencyPenalty(v float64) Option { return func(c *GeminiConfig) { c.frequencyPenalty = &v } }

// WithSeed pins the sampling seed.
func WithSeed(v int64) Option { return func(c *GeminiConfig) { c.seed = &v } }

// WithThinkingLevel sets the reasoning-effort level; one of "low", "medium",
// "high" (Python parity: the ThinkingLevel literal).
func WithThinkingLevel(v string) Option { return func(c *GeminiConfig) { c.thinkingLevel = &v } }

// WithThinkingBudget caps the thinking token budget.
func WithThinkingBudget(v int64) Option { return func(c *GeminiConfig) { c.thinkingBudget = &v } }

// NewGemini builds a config for one Gemini model.
func NewGemini(apiKey, model string, opts ...Option) *GeminiConfig {
	c := &GeminiConfig{model: model, apiKey: apiKey, baseURL: DefaultBaseURL}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Provider names the vendor (Python parity: ModelProvider.GEMINI).
func (c *GeminiConfig) Provider() config.Provider { return config.ProviderGemini }

// Model returns the model identifier sent to the API.
func (c *GeminiConfig) Model() string { return c.model }

// Create resolves the env fallbacks and builds a ready-to-call client
// (Python parity: a fresh client per create()).
func (c *GeminiConfig) Create() (config.LLMClient, error) {
	if c.model == "" {
		return nil, fmt.Errorf("%w: gemini model", config.ErrNotProvided)
	}
	key := c.apiKey
	if key == "" {
		key = os.Getenv(EnvAPIKey)
	}
	if key == "" {
		return nil, fmt.Errorf("%w: gemini api key (set %s)", config.ErrNotProvided, EnvAPIKey)
	}
	base := c.baseURL
	if base == "" {
		base = DefaultBaseURL
	}
	// No transport retries: the Python google-genai SDK posts plain httpx
	// without retry logic; callers cancel via ctx.
	return &client{
		cfg:     *c,
		apiKey:  key,
		baseURL: base,
		http:    &httpx.Client{},
	}, nil
}

// Clone returns a copy with overrides applied (Python parity:
// dataclasses.replace). Keys are the Python field names; values keep the
// config's field types (*float64/*int64 or the plain scalars, nil clears the
// field). Unknown keys are an error.
func (c *GeminiConfig) Clone(overrides map[string]any) (config.ModelConfig, error) {
	cc := *c
	cc.stopSequences = append([]string(nil), c.stopSequences...)
	cc.temperature = cloneFloat(c.temperature)
	cc.topP = cloneFloat(c.topP)
	cc.topK = cloneInt(c.topK)
	cc.maxOutputTokens = cloneInt(c.maxOutputTokens)
	cc.presencePenalty = cloneFloat(c.presencePenalty)
	cc.frequencyPenalty = cloneFloat(c.frequencyPenalty)
	cc.seed = cloneInt(c.seed)
	cc.thinkingLevel = cloneString(c.thinkingLevel)
	cc.thinkingBudget = cloneInt(c.thinkingBudget)
	for k, v := range overrides {
		switch k {
		case "model":
			s, err := stringOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.model = s
		case "api_key":
			s, err := stringOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.apiKey = s
		case "base_url":
			s, err := stringOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.baseURL = s
		case "temperature":
			f, err := floatOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.temperature = f
		case "top_p":
			f, err := floatOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.topP = f
		case "top_k":
			i, err := intOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.topK = i
		case "max_output_tokens":
			i, err := intOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.maxOutputTokens = i
		case "stop_sequences":
			ss, err := stringsOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.stopSequences = ss
		case "streaming":
			b, err := boolOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.streaming = b
		case "presence_penalty":
			f, err := floatOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.presencePenalty = f
		case "frequency_penalty":
			f, err := floatOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.frequencyPenalty = f
		case "seed":
			i, err := intOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.seed = i
		case "thinking_level":
			// AGO-DEVIATION: Python enforces the ThinkingLevel literal
			// statically; the dynamic Clone boundary checks the value instead
			// (D-035). A nil clears the field.
			if v == nil {
				cc.thinkingLevel = nil
			} else {
				s, err := stringOverride(k, v)
				if err != nil {
					return nil, err
				}
				if !validThinkingLevel(s) {
					return nil, fmt.Errorf("gemini: override %q must be one of \"low\", \"medium\", \"high\", got %q", k, s)
				}
				cc.thinkingLevel = &s
			}
		case "thinking_budget":
			i, err := intOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.thinkingBudget = i
		default:
			return nil, fmt.Errorf("gemini: unknown config override %q", k)
		}
	}
	return &cc, nil
}

// --- override value coercion (Python parity: replace() type errors) ----------

func overrideErr(key string, v any) error {
	return fmt.Errorf("gemini: override %q expects the config field type, got %T", key, v)
}

func stringOverride(key string, v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", overrideErr(key, v)
	}
	return s, nil
}

func floatOverride(key string, v any) (*float64, error) {
	switch n := v.(type) {
	case nil:
		return nil, nil
	case *float64:
		return cloneFloat(n), nil
	case float64:
		return &n, nil
	default:
		return nil, overrideErr(key, v)
	}
}

func intOverride(key string, v any) (*int64, error) {
	switch n := v.(type) {
	case nil:
		return nil, nil
	case *int64:
		return cloneInt(n), nil
	case int64:
		return &n, nil
	case int:
		i := int64(n)
		return &i, nil
	default:
		return nil, overrideErr(key, v)
	}
}

func stringsOverride(key string, v any) ([]string, error) {
	switch s := v.(type) {
	case nil:
		return nil, nil
	case []string:
		return append([]string(nil), s...), nil
	default:
		return nil, overrideErr(key, v)
	}
}

func boolOverride(key string, v any) (bool, error) {
	switch b := v.(type) {
	case bool:
		return b, nil
	case *bool:
		if b == nil {
			return false, nil
		}
		return *b, nil
	default:
		return false, overrideErr(key, v)
	}
}

func cloneFloat(v *float64) *float64 {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

func cloneInt(v *int64) *int64 {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

func cloneString(v *string) *string {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

// validThinkingLevel reports whether level is an accepted thinking level
// (Python parity: ThinkingLevel = Literal["low", "medium", "high"]).
func validThinkingLevel(level string) bool {
	return level == "low" || level == "medium" || level == "high"
}
