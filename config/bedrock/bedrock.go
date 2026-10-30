// Package bedrock holds the Amazon Bedrock model configuration (Converse
// API). The Python client rides aiobotocore — AWS SigV4 signing plus the
// botocore credentials chain — which is out of the AGo scope for now, so this
// is a stub provider (D-008): the config exists with the brief's fields and
// Create() reports config.ErrNotImplemented until a client is ported.
//
// Parity with Python AG2 ag2/config/bedrock/config.py (BedrockConfig). See
// docs/specs/config-llm.md §1.
package bedrock

import (
	"fmt"
	"strings"

	"github.com/luckystrker/ago/config"
)

// BedrockConfig configures one Bedrock model (Python parity: the
// BedrockConfig dataclass; model is a Bedrock model id or inference-profile
// ARN). Construct with NewBedrock — the only way to set the model — and
// assign the exported fields. Immutable once built; safe for concurrent use.
//
// Credentials follow botocore's resolution chain (explicit keys, then
// profile_name, then environment/shared config/instance roles); AGo carries
// them verbatim for the future client.
//
// AGO-DEVIATION: the Python dataclass carries the model as the public field
// `model`; AGo holds it privately behind the Model() accessor because the
// config.ModelConfig contract requires a Model() method and Go forbids a
// field and a method sharing one name.
type BedrockConfig struct {
	// Static credentials (optional; the SDK chain resolves them otherwise).
	AWSAccessKeyID     string
	AWSSecretAccessKey string
	AWSSessionToken    string
	// ProfileName selects a shared-config profile.
	ProfileName string
	// RegionName is the AWS region to call.
	RegionName string
	// EndpointURL overrides the service endpoint.
	EndpointURL string

	MaxTokens   *int64   // nil = unset, not sent
	Temperature *float64 // nil = unset, not sent
	TopP        *float64 // nil = unset, not sent
	// StopSequences end the completion when the model emits one.
	StopSequences []string

	model string
}

// NewBedrock returns a config for model. Bedrock has no non-zero defaults in
// the Python config, so only the model is set.
func NewBedrock(model string) *BedrockConfig {
	return &BedrockConfig{model: model}
}

// Provider reports the vendor (ModelConfig contract).
func (c *BedrockConfig) Provider() config.Provider { return config.ProviderBedrock }

// Model returns the model identifier sent to the API (ModelConfig contract).
func (c *BedrockConfig) Model() string { return c.model }

// Create implements config.ModelConfig. There is no Bedrock client yet: a
// missing model still wraps config.ErrNotProvided (the ModelConfig contract),
// and a valid config wraps config.ErrNotImplemented naming the provider.
func (c *BedrockConfig) Create() (config.LLMClient, error) {
	if c.model == "" {
		return nil, fmt.Errorf("%w: bedrock: model", config.ErrNotProvided)
	}
	return nil, fmt.Errorf("%w: provider %q has no client yet", config.ErrNotImplemented, config.ProviderBedrock)
}

// Clone implements config.ModelConfig (Python parity: dataclasses.replace): a
// copy with overrides applied, receiver untouched. Keys are Go field
// names, matched case-insensitively; an unknown key or a wrongly typed value
// is an error. A pointer field
// also accepts its value type (wrapped) and nil (cleared). Slice values are
// copied, so the clone never aliases the receiver's memory.
func (c *BedrockConfig) Clone(overrides map[string]any) (config.ModelConfig, error) {
	out := *c
	out.StopSequences = cloneStrings(c.StopSequences)
	for k, v := range overrides {
		var err error
		switch strings.ToLower(k) {
		case "model":
			err = setString(&out.model, k, v)
		case "awsaccesskeyid":
			err = setString(&out.AWSAccessKeyID, k, v)
		case "awssecretaccesskey":
			err = setString(&out.AWSSecretAccessKey, k, v)
		case "awssessiontoken":
			err = setString(&out.AWSSessionToken, k, v)
		case "profilename":
			err = setString(&out.ProfileName, k, v)
		case "regionname":
			err = setString(&out.RegionName, k, v)
		case "endpointurl":
			err = setString(&out.EndpointURL, k, v)
		case "maxtokens":
			err = setI64(&out.MaxTokens, k, v)
		case "temperature":
			err = setF64(&out.Temperature, k, v)
		case "topp":
			err = setF64(&out.TopP, k, v)
		case "stopsequences":
			err = setStrings(&out.StopSequences, k, v)
		default:
			err = fmt.Errorf("config/bedrock: unknown config field %q", k)
		}
		if err != nil {
			return nil, err
		}
	}
	return &out, nil
}

// Compile-time contract check.
var _ config.ModelConfig = (*BedrockConfig)(nil)

// --- Clone field setters -------------------------------------------------------
// Each setter enforces the config's field type; a pointer field additionally
// accepts its value type (wrapped) and nil (cleared, Python None parity).

func setString(dst *string, name string, v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("config/bedrock: override %q: got %T, want string", name, v)
	}
	*dst = s
	return nil
}

func setF64(dst **float64, name string, v any) error {
	switch n := v.(type) {
	case nil:
		*dst = nil
	case *float64:
		*dst = n
	case float64:
		*dst = &n
	default:
		return fmt.Errorf("config/bedrock: override %q: got %T, want float64", name, v)
	}
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
		return fmt.Errorf("config/bedrock: override %q: got %T, want int64", name, v)
	}
	return nil
}

func setStrings(dst *[]string, name string, v any) error {
	s, ok := v.([]string)
	if !ok {
		return fmt.Errorf("config/bedrock: override %q: got %T, want []string", name, v)
	}
	*dst = cloneStrings(s)
	return nil
}

func cloneStrings(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}
