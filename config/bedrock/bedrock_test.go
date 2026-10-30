package bedrock

import (
	"errors"
	"strings"
	"testing"

	"github.com/luckystrker/ago/config"
)

func TestDefaults(t *testing.T) {
	cfg := NewBedrock("anthropic.claude-sonnet-4-5")
	if cfg.Model() != "anthropic.claude-sonnet-4-5" {
		t.Fatalf("bad model: %q", cfg.Model())
	}
	// Bedrock has no non-zero defaults: credentials and sampling stay unset.
	if cfg.AWSAccessKeyID != "" || cfg.AWSSecretAccessKey != "" || cfg.AWSSessionToken != "" ||
		cfg.ProfileName != "" || cfg.RegionName != "" || cfg.EndpointURL != "" {
		t.Fatalf("credential fields must be unset by default: %+v", cfg)
	}
	if cfg.MaxTokens != nil || cfg.Temperature != nil || cfg.TopP != nil || cfg.StopSequences != nil {
		t.Fatalf("sampling fields must be unset by default: %+v", cfg)
	}
	if cfg.Provider() != config.ProviderBedrock || string(cfg.Provider()) != "bedrock" {
		t.Fatalf("bad provider: %q", cfg.Provider())
	}
}

// TestCreateNotImplemented pins the stub contract: a valid config reports
// config.ErrNotImplemented naming the provider.
func TestCreateNotImplemented(t *testing.T) {
	_, err := NewBedrock("anthropic.claude-sonnet-4-5").Create()
	if !errors.Is(err, config.ErrNotImplemented) {
		t.Fatalf("want ErrNotImplemented, got %v", err)
	}
	if !strings.Contains(err.Error(), "bedrock") {
		t.Fatalf("the error must name the provider, got %v", err)
	}
}

func TestCreateMissingModel(t *testing.T) {
	if _, err := (&BedrockConfig{}).Create(); !errors.Is(err, config.ErrNotProvided) {
		t.Fatalf("want ErrNotProvided for a missing model, got %v", err)
	}
}

func TestCloneOverrides(t *testing.T) {
	cfg := NewBedrock("anthropic.claude-sonnet-4-5")
	cfg.Temperature = f64ptr(0.2)
	cfg.StopSequences = []string{"END"}

	cloned, err := cfg.Clone(map[string]any{
		"Model":              "anthropic.claude-haiku-4-5",
		"AWSAccessKeyID":     "AKIA...",
		"AWSSecretAccessKey": "secret",
		"AWSSessionToken":    "token",
		"ProfileName":        "default",
		"regionname":         "us-east-1",
		"EndpointURL":        "https://vpce.example",
		"MaxTokens":          int64(2048),
		"TopP":               0.9,
		"StopSequences":      []string{"STOP"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, ok := cloned.(*BedrockConfig)
	if !ok {
		t.Fatalf("Clone must return *BedrockConfig, got %T", cloned)
	}
	if got.Model() != "anthropic.claude-haiku-4-5" || got.AWSAccessKeyID != "AKIA..." ||
		got.AWSSecretAccessKey != "secret" || got.AWSSessionToken != "token" ||
		got.ProfileName != "default" || got.RegionName != "us-east-1" || got.EndpointURL != "https://vpce.example" {
		t.Fatalf("overrides not applied: %+v", got)
	}
	if got.MaxTokens == nil || *got.MaxTokens != 2048 {
		t.Fatalf("int64 override not applied: %+v", got.MaxTokens)
	}
	if got.Temperature == nil || *got.Temperature != 0.2 || got.TopP == nil || *got.TopP != 0.9 {
		t.Fatalf("float64 overrides not applied: %+v", got)
	}
	if len(got.StopSequences) != 1 || got.StopSequences[0] != "STOP" {
		t.Fatalf("slice override not applied: %+v", got.StopSequences)
	}
	// The receiver stays untouched, including its slice memory.
	if cfg.Model() != "anthropic.claude-sonnet-4-5" || len(cfg.StopSequences) != 1 || cfg.StopSequences[0] != "END" {
		t.Fatalf("Clone mutated the receiver: %+v", cfg)
	}
	if cloned.Provider() != config.ProviderBedrock {
		t.Fatalf("clone lost the provider: %q", cloned.Provider())
	}

	// Errors: unknown key, wrong type.
	if _, err := cfg.Clone(map[string]any{"Bogus": 1}); err == nil || !strings.Contains(err.Error(), "Bogus") {
		t.Fatalf("unknown key must fail naming the key, got %v", err)
	}
	if _, err := cfg.Clone(map[string]any{"StopSequences": "END"}); err == nil {
		t.Fatal("wrongly typed override must fail")
	}
	if _, err := cfg.Clone(map[string]any{"Model": 42}); err == nil {
		t.Fatal("wrongly typed override must fail")
	}
}

func f64ptr(v float64) *float64 { return &v }
