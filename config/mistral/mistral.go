// Package mistral wires Mistral's chat-completions API into the AGo config
// contract. Mistral speaks an OpenAI Chat-Completions dialect, so its client
// is a thin binding of MistralConfig onto the shared engine
// config.RunChatCompat.
//
// Parity with Python AG2 ag2/config/mistral/config.py (MistralConfig). The
// Mistral-specific extras (guardrails, prediction, prompt_mode, logprobs,
// Files API) are out of the wave-1 scope (D-008); the sampled fields follow
// the porting brief. See docs/specs/config-llm.md §1.
package mistral

import (
	"fmt"
	"os"
	"strings"

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

	Temperature *float64 // nil = unset, not sent
	TopP        *float64 // nil = unset, not sent
	MaxTokens   *int64   // nil = unset, not sent

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
		APIKey:      key,
		BaseURL:     base,
		Path:        chatPath,
		Model:       c.model,
		Provider:    config.ProviderMistral,
		Temperature: c.Temperature,
		TopP:        c.TopP,
		MaxTokens:   c.MaxTokens,
		Streaming:   c.Streaming,
	}
	if c.SafePrompt {
		// Wire field of the Mistral chat API; omitted when false (see the
		// field comment).
		p.Extra = map[string]any{"safe_prompt": true}
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
		default:
			err = fmt.Errorf("config/mistral: unknown config field %q", k)
		}
		if err != nil {
			return nil, err
		}
	}
	return &out, nil
}
