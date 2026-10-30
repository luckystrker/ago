// Package zai wires Z.AI (Zhipu) GLM models into the AGo config contract.
// Z.AI exposes an OpenAI Chat-Completions dialect, so its client is a thin
// binding of ZAIConfig onto the shared engine config.RunChatCompat.
//
// Parity with Python AG2 ag2/config/zai/config.py. The Z.AI-specific extras
// (thinking flags, token-cache opt-out, sensitive-word check, Files API) are
// out of the wave-1 scope (D-008); the sampled fields follow the porting brief.
// See docs/specs/config-llm.md §1.
package zai

import (
	"fmt"
	"os"
	"strings"

	"github.com/luckystrker/ago/config"
)

// Defaults (Python parity: the Z.AI SDK endpoint and max_retries=3).
const (
	// DefaultBaseURL is the Z.AI OpenAI-compatible API root used when
	// ZAIConfig.BaseURL is empty.
	DefaultBaseURL = "https://api.z.ai/api/paas/v4"
	// EnvAPIKey names the environment variable consulted when ZAIConfig.APIKey
	// is empty (Python parity: the SDK reads it at client construction).
	EnvAPIKey = "ZAI_API_KEY"
	// DefaultMaxRetries is the transport retry count (Python parity).
	DefaultMaxRetries = 3
	// chatPath is the Chat-Completions endpoint under BaseURL.
	chatPath = "/chat/completions"
)

// ZAIConfig configures one Z.AI model (Python parity: the ZAIConfig dataclass).
// Construct with NewZAI — the only way to set the model — and assign the
// exported fields. A config is immutable once built: Create and Clone never
// mutate the receiver, so one value is safe to share across goroutines.
//
// AGO-DEVIATION: the Python dataclass carries the model as the public field
// `model`; AGo holds it privately behind the Model() accessor because the
// config.ModelConfig contract requires a Model() method and Go forbids a
// field and a method sharing one name.
type ZAIConfig struct {
	// APIKey authenticates requests. Empty falls back to EnvAPIKey at Create()
	// time; neither set → Create() wraps config.ErrNotProvided.
	APIKey string
	// BaseURL overrides the endpoint root. Empty → DefaultBaseURL.
	BaseURL string

	// MaxRetries counts transport retries per call; 0 = single attempt (Go zero
	// value — NewZAI applies the Python default of 3).
	MaxRetries int

	Temperature *float64 // nil = unset, not sent
	TopP        *float64 // nil = unset, not sent
	MaxTokens   *int64   // nil = unset, not sent

	Streaming bool

	model string
}

// NewZAI returns a config for model with the Python defaults: the Z.AI base
// URL and 3 transport retries.
func NewZAI(model string) *ZAIConfig {
	return &ZAIConfig{
		model:      model,
		BaseURL:    DefaultBaseURL,
		MaxRetries: DefaultMaxRetries,
	}
}

// Provider reports the vendor (ModelConfig contract).
func (c *ZAIConfig) Provider() config.Provider { return config.ProviderZAI }

// Model returns the model identifier sent to the API (ModelConfig contract).
func (c *ZAIConfig) Model() string { return c.model }

// Create implements config.ModelConfig: it validates the required values and
// binds them into a fresh client (Python parity: fresh client per create()).
// The API key falls back to EnvAPIKey when the field is empty; a missing model
// or key is an error wrapping config.ErrNotProvided.
func (c *ZAIConfig) Create() (config.LLMClient, error) {
	params, err := c.params()
	if err != nil {
		return nil, err
	}
	return &chatClient{params: params}, nil
}

// params renders the shared-engine parameters for this config.
func (c *ZAIConfig) params() (config.CompatParams, error) {
	if c.model == "" {
		return config.CompatParams{}, fmt.Errorf("%w: zai: model", config.ErrNotProvided)
	}
	key := c.APIKey
	if key == "" {
		key = os.Getenv(EnvAPIKey)
	}
	if key == "" {
		return config.CompatParams{}, fmt.Errorf("%w: zai: api key (set %s)", config.ErrNotProvided, EnvAPIKey)
	}
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	return config.CompatParams{
		APIKey:      key,
		BaseURL:     base,
		Path:        chatPath,
		Model:       c.model,
		Provider:    config.ProviderZAI,
		Temperature: c.Temperature,
		TopP:        c.TopP,
		MaxTokens:   c.MaxTokens,
		Streaming:   c.Streaming,
		MaxRetries:  c.MaxRetries,
	}, nil
}

// Clone implements config.ModelConfig (Python parity: dataclasses.replace): a
// copy with overrides applied, receiver untouched. Keys are Go field
// names, matched case-insensitively; an unknown key or a wrongly typed value
// is an error. A pointer field
// also accepts its value type (wrapped) and nil (cleared).
func (c *ZAIConfig) Clone(overrides map[string]any) (config.ModelConfig, error) {
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
		case "maxretries":
			err = setInt(&out.MaxRetries, k, v)
		case "temperature":
			err = setF64(&out.Temperature, k, v)
		case "topp":
			err = setF64(&out.TopP, k, v)
		case "maxtokens":
			err = setI64(&out.MaxTokens, k, v)
		case "streaming":
			err = setBool(&out.Streaming, k, v)
		default:
			err = fmt.Errorf("config/zai: unknown config field %q", k)
		}
		if err != nil {
			return nil, err
		}
	}
	return &out, nil
}
