// Package xai holds the xAI (Grok) model configuration. The Python client
// rides the xai gRPC SDK; AGo has no gRPC transport in scope, so this is a
// stub provider (D-008): the config exists with the brief's fields and
// Create() reports config.ErrNotImplemented until a client is ported.
//
// Parity with Python AG2 ag2/config/xai/config.py (XAIConfig). See
// docs/specs/config-llm.md §1.
package xai

import (
	"fmt"
	"strings"

	"ago/config"
)

// DefaultAPIHost is the xAI API host (Python parity: XAI_DEFAULT_API_HOST).
const DefaultAPIHost = "api.x.ai"

// XAIConfig configures one xAI model (Python parity: the XAIConfig dataclass;
// the gRPC-only extras — metadata, channel_options, server-side conversation —
// are out of scope). Construct with NewXAI — the only way to set the model —
// and assign the exported fields. Immutable once built; safe for concurrent
// use.
//
// AGO-DEVIATION: the Python dataclass carries the model as the public field
// `model`; AGo holds it privately behind the Model() accessor because the
// config.ModelConfig contract requires a Model() method and Go forbids a
// field and a method sharing one name.
type XAIConfig struct {
	// APIKey authenticates requests (the Python config documents no env
	// fallback; the key stays explicit).
	APIKey string
	// APIHost is the API endpoint host. Empty on a direct literal — NewXAI
	// applies DefaultAPIHost.
	APIHost string
	// MaxTokens caps the completion; nil = unset.
	MaxTokens *int64
	// ReasoningEffort selects the reasoning level (Python parity:
	// reasoning_effort).
	ReasoningEffort string

	model string
}

// NewXAI returns a config for model with the xAI defaults (API host).
func NewXAI(model string) *XAIConfig {
	return &XAIConfig{
		model:   model,
		APIHost: DefaultAPIHost,
	}
}

// Provider reports the vendor (ModelConfig contract).
func (c *XAIConfig) Provider() config.Provider { return config.ProviderXAI }

// Model returns the model identifier sent to the API (ModelConfig contract).
func (c *XAIConfig) Model() string { return c.model }

// Create implements config.ModelConfig. There is no xAI client yet: a missing
// model still wraps config.ErrNotProvided (the ModelConfig contract), and a
// valid config wraps config.ErrNotImplemented naming the provider.
func (c *XAIConfig) Create() (config.LLMClient, error) {
	if c.model == "" {
		return nil, fmt.Errorf("%w: xai: model", config.ErrNotProvided)
	}
	return nil, fmt.Errorf("%w: provider %q has no client yet", config.ErrNotImplemented, config.ProviderXAI)
}

// Clone implements config.ModelConfig (Python parity: dataclasses.replace): a
// copy with overrides applied, receiver untouched. Keys are Go field
// names, matched case-insensitively; an unknown key or a wrongly typed value
// is an error. A pointer field
// also accepts its value type (wrapped) and nil (cleared).
func (c *XAIConfig) Clone(overrides map[string]any) (config.ModelConfig, error) {
	out := *c
	for k, v := range overrides {
		var err error
		switch strings.ToLower(k) {
		case "model":
			err = setString(&out.model, k, v)
		case "apikey":
			err = setString(&out.APIKey, k, v)
		case "apihost":
			err = setString(&out.APIHost, k, v)
		case "maxtokens":
			err = setI64(&out.MaxTokens, k, v)
		case "reasoningeffort":
			err = setString(&out.ReasoningEffort, k, v)
		default:
			err = fmt.Errorf("config/xai: unknown config field %q", k)
		}
		if err != nil {
			return nil, err
		}
	}
	return &out, nil
}

// Compile-time contract check.
var _ config.ModelConfig = (*XAIConfig)(nil)

// --- Clone field setters -------------------------------------------------------
// Each setter enforces the config's field type; a pointer field additionally
// accepts its value type (wrapped) and nil (cleared, Python None parity).

func setString(dst *string, name string, v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("config/xai: override %q: got %T, want string", name, v)
	}
	*dst = s
	return nil
}

func setI64(dst **int64, name string, v any) error {
	switch n := v.(type) {
	case nil:
		*dst = nil
	case *int64:
		*dst = n
	case int64:
		*dst = &n
	case int:
		n64 := int64(n)
		*dst = &n64
	default:
		return fmt.Errorf("config/xai: override %q: got %T, want int64", name, v)
	}
	return nil
}
