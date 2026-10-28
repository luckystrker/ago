// Package dashscope wires Alibaba DashScope (Qwen) models into the AGo config
// contract. DashScope offers an OpenAI-compatible endpoint, so its client is
// a thin binding of DashScopeConfig onto the shared engine
// config.RunChatCompat.
//
// Parity with Python AG2 ag2/config/dashscope/config.py (DashScopeConfig).
//
// AGO-DEVIATION: the Python config defaults to DASHSCOPE_INTL_BASE_URL
// ("https://dashscope-intl.aliyuncs.com/api/v1"), the DashScope-native REST
// root spoken through the dashscope SDK. AGo talks the OpenAI-compatible
// dialect (config.RunChatCompat), so the default is the compatible-mode root
// per the porting brief.
//
// See docs/specs/config-llm.md §1.
package dashscope

import (
	"fmt"
	"os"
	"strings"

	"ago/config"
)

// Defaults (per the porting brief).
const (
	// DefaultBaseURL is the DashScope OpenAI-compatible root used when
	// DashScopeConfig.BaseURL is empty.
	DefaultBaseURL = "https://dashscope-intl.aliyuncs.com/compatible-mode/v1"
	// EnvAPIKey names the environment variable consulted when
	// DashScopeConfig.APIKey is empty.
	EnvAPIKey = "DASHSCOPE_API_KEY"
	// chatPath is the Chat-Completions endpoint under BaseURL.
	chatPath = "/chat/completions"
)

// DashScopeConfig configures one DashScope model (Python parity: the
// DashScopeConfig dataclass). Construct with NewDashScope — the only way to
// set the model — and assign the exported fields. A config is immutable once
// built: Create and Clone never mutate the receiver, so one value is safe to
// share across goroutines.
//
// AGO-DEVIATION: the Python dataclass carries the model as the public field
// `model`; AGo holds it privately behind the Model() accessor because the
// config.ModelConfig contract requires a Model() method and Go forbids a
// field and a method sharing one name.
type DashScopeConfig struct {
	// APIKey authenticates requests. Empty falls back to EnvAPIKey at Create()
	// time; neither set → Create() wraps config.ErrNotProvided.
	APIKey string
	// BaseURL overrides the endpoint root. Empty → DefaultBaseURL.
	BaseURL string

	Temperature *float64 // nil = unset, not sent
	TopP        *float64 // nil = unset, not sent
	MaxTokens   *int64   // nil = unset, not sent

	Streaming bool

	model string
}

// NewDashScope returns a config for model with the DashScope defaults (base
// URL).
func NewDashScope(model string) *DashScopeConfig {
	return &DashScopeConfig{
		model:   model,
		BaseURL: DefaultBaseURL,
	}
}

// Provider reports the vendor (ModelConfig contract).
func (c *DashScopeConfig) Provider() config.Provider { return config.ProviderDashScope }

// Model returns the model identifier sent to the API (ModelConfig contract).
func (c *DashScopeConfig) Model() string { return c.model }

// Create implements config.ModelConfig: it validates the required values and
// binds them into a fresh client (Python parity: fresh client per create()).
// The API key falls back to EnvAPIKey when the field is empty; a missing model
// or key is an error wrapping config.ErrNotProvided.
func (c *DashScopeConfig) Create() (config.LLMClient, error) {
	params, err := c.params()
	if err != nil {
		return nil, err
	}
	return &chatClient{params: params}, nil
}

// params renders the shared-engine parameters for this config.
func (c *DashScopeConfig) params() (config.CompatParams, error) {
	if c.model == "" {
		return config.CompatParams{}, fmt.Errorf("%w: dashscope: model", config.ErrNotProvided)
	}
	key := c.APIKey
	if key == "" {
		key = os.Getenv(EnvAPIKey)
	}
	if key == "" {
		return config.CompatParams{}, fmt.Errorf("%w: dashscope: api key (set %s)", config.ErrNotProvided, EnvAPIKey)
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
		Provider:    config.ProviderDashScope,
		Temperature: c.Temperature,
		TopP:        c.TopP,
		MaxTokens:   c.MaxTokens,
		Streaming:   c.Streaming,
	}, nil
}

// Clone implements config.ModelConfig (Python parity: dataclasses.replace): a
// copy with overrides applied, receiver untouched. Keys are Go field
// names, matched case-insensitively; an unknown key or a wrongly typed value
// is an error. A pointer field
// also accepts its value type (wrapped) and nil (cleared).
func (c *DashScopeConfig) Clone(overrides map[string]any) (config.ModelConfig, error) {
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
		case "streaming":
			err = setBool(&out.Streaming, k, v)
		default:
			err = fmt.Errorf("config/dashscope: unknown config field %q", k)
		}
		if err != nil {
			return nil, err
		}
	}
	return &out, nil
}
