// Package typesafe holds the TypeSafe model configuration. TypeSafe is not a
// chat model — one typed question per call, tools rejected — and its Python
// client rides the typesafe_sdk, which is out of the AGo scope for now, so
// this is a stub provider (D-008): the config exists with the brief's fields
// and Create() reports config.ErrNotImplemented until a client is ported.
//
// Parity with Python AG2 ag2/config/typesafe/config.py (TypeSafeConfig). See
// docs/specs/config-llm.md §1.
package typesafe

import (
	"fmt"
	"strings"
	"time"

	"ago/config"
)

// DefaultBooleanThreshold is the score cutoff deciding boolean questions
// (Python parity: boolean_threshold=0.5).
const DefaultBooleanThreshold = 0.5

// TypeSafeConfig configures one TypeSafe evaluation model (Python parity: the
// TypeSafeConfig dataclass). Construct with NewTypeSafe — the only way to set
// the model — and assign the exported fields. Immutable once built; safe for
// concurrent use.
//
// AGO-DEVIATION: the Python dataclass carries the model as the public field
// `model` with a default taken from typesafe_sdk.constants.DEFAULT_MODEL; that
// SDK is absent from the Python clone, so the value is unknown and AGo
// requires the model through NewTypeSafe instead. It is also held privately
// behind the Model() accessor because the config.ModelConfig contract
// requires a Model() method and Go forbids a field and a method sharing one
// name.
type TypeSafeConfig struct {
	// APIKey authenticates requests.
	APIKey string
	// BaseURL overrides the service endpoint; empty lets the future client
	// pick the SDK default.
	BaseURL string
	// Timeout caps one call (0 = no cap).
	Timeout time.Duration
	// BooleanThreshold is the score cutoff for boolean questions; NewTypeSafe
	// applies DefaultBooleanThreshold.
	BooleanThreshold float64
	// Criteria maps criterion names to their descriptions (Python parity:
	// criteria Mapping[str, str]).
	Criteria map[string]string

	model string
}

// NewTypeSafe returns a config for model with the TypeSafe defaults
// (boolean_threshold=0.5).
func NewTypeSafe(model string) *TypeSafeConfig {
	return &TypeSafeConfig{
		model:            model,
		BooleanThreshold: DefaultBooleanThreshold,
	}
}

// Provider reports the vendor (ModelConfig contract).
func (c *TypeSafeConfig) Provider() config.Provider { return config.ProviderTypeSafe }

// Model returns the model identifier sent to the API (ModelConfig contract).
func (c *TypeSafeConfig) Model() string { return c.model }

// Create implements config.ModelConfig. There is no TypeSafe client yet: a
// missing model still wraps config.ErrNotProvided (the ModelConfig contract),
// and a valid config wraps config.ErrNotImplemented naming the provider.
func (c *TypeSafeConfig) Create() (config.LLMClient, error) {
	if c.model == "" {
		return nil, fmt.Errorf("%w: typesafe: model", config.ErrNotProvided)
	}
	return nil, fmt.Errorf("%w: provider %q has no client yet", config.ErrNotImplemented, config.ProviderTypeSafe)
}

// Clone implements config.ModelConfig (Python parity: dataclasses.replace): a
// copy with overrides applied, receiver untouched. Keys are Go field
// names, matched case-insensitively; an unknown key or a wrongly typed value
// is an error. Map values are
// copied, so the clone never aliases the receiver's memory.
func (c *TypeSafeConfig) Clone(overrides map[string]any) (config.ModelConfig, error) {
	out := *c
	out.Criteria = cloneCriteria(c.Criteria)
	for k, v := range overrides {
		var err error
		switch strings.ToLower(k) {
		case "model":
			err = setString(&out.model, k, v)
		case "apikey":
			err = setString(&out.APIKey, k, v)
		case "baseurl":
			err = setString(&out.BaseURL, k, v)
		case "timeout":
			err = setDuration(&out.Timeout, k, v)
		case "booleanthreshold":
			err = setF64(&out.BooleanThreshold, k, v)
		case "criteria":
			err = setCriteria(&out.Criteria, k, v)
		default:
			err = fmt.Errorf("config/typesafe: unknown config field %q", k)
		}
		if err != nil {
			return nil, err
		}
	}
	return &out, nil
}

// Compile-time contract check.
var _ config.ModelConfig = (*TypeSafeConfig)(nil)

// --- Clone field setters -------------------------------------------------------
// Each setter enforces the config's field type; a pointer field additionally
// accepts its value type (wrapped) and nil (cleared, Python None parity).

func setString(dst *string, name string, v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("config/typesafe: override %q: got %T, want string", name, v)
	}
	*dst = s
	return nil
}

func setF64(dst *float64, name string, v any) error {
	n, ok := v.(float64)
	if !ok {
		return fmt.Errorf("config/typesafe: override %q: got %T, want float64", name, v)
	}
	*dst = n
	return nil
}

func setDuration(dst *time.Duration, name string, v any) error {
	d, ok := v.(time.Duration)
	if !ok {
		return fmt.Errorf("config/typesafe: override %q: got %T, want time.Duration", name, v)
	}
	*dst = d
	return nil
}

func setCriteria(dst *map[string]string, name string, v any) error {
	switch m := v.(type) {
	case nil:
		*dst = nil
	case map[string]string:
		*dst = cloneCriteria(m)
	default:
		return fmt.Errorf("config/typesafe: override %q: got %T, want map[string]string", name, v)
	}
	return nil
}

func cloneCriteria(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
