package xai

import (
	"errors"
	"strings"
	"testing"

	"github.com/luckystrker/ago/config"
)

func TestDefaults(t *testing.T) {
	cfg := NewXAI("grok-4")
	if cfg.Model() != "grok-4" {
		t.Fatalf("bad model: %q", cfg.Model())
	}
	if cfg.APIHost != DefaultAPIHost {
		t.Fatalf("API host default not applied: %q", cfg.APIHost)
	}
	if cfg.MaxTokens != nil || cfg.ReasoningEffort != "" || cfg.APIKey != "" {
		t.Fatalf("optional fields must be unset by default: %+v", cfg)
	}
	if cfg.Provider() != config.ProviderXAI || string(cfg.Provider()) != "xai" {
		t.Fatalf("bad provider: %q", cfg.Provider())
	}
}

// TestCreateNotImplemented pins the stub contract: a valid config reports
// config.ErrNotImplemented naming the provider.
func TestCreateNotImplemented(t *testing.T) {
	_, err := NewXAI("grok-4").Create()
	if !errors.Is(err, config.ErrNotImplemented) {
		t.Fatalf("want ErrNotImplemented, got %v", err)
	}
	if !strings.Contains(err.Error(), "xai") {
		t.Fatalf("the error must name the provider, got %v", err)
	}
}

func TestCreateMissingModel(t *testing.T) {
	// The ModelConfig contract (missing model → ErrNotProvided) holds before
	// the stub error.
	if _, err := (&XAIConfig{APIHost: DefaultAPIHost}).Create(); !errors.Is(err, config.ErrNotProvided) {
		t.Fatalf("want ErrNotProvided for a missing model, got %v", err)
	}
}

func TestCloneOverrides(t *testing.T) {
	cfg := NewXAI("grok-4")
	cloned, err := cfg.Clone(map[string]any{
		"Model":           "grok-5",
		"apiKey":          "k",
		"APIHost":         "api.example.com",
		"MaxTokens":       int64(1024),
		"ReasoningEffort": "high",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, ok := cloned.(*XAIConfig)
	if !ok {
		t.Fatalf("Clone must return *XAIConfig, got %T", cloned)
	}
	if got.Model() != "grok-5" || got.APIKey != "k" || got.APIHost != "api.example.com" || got.ReasoningEffort != "high" {
		t.Fatalf("overrides not applied: %+v", got)
	}
	if got.MaxTokens == nil || *got.MaxTokens != 1024 {
		t.Fatalf("int64 override not applied: %+v", got.MaxTokens)
	}
	if cfg.Model() != "grok-4" || cfg.APIKey != "" {
		t.Fatalf("Clone mutated the receiver: %+v", cfg)
	}
	if cloned.Provider() != config.ProviderXAI {
		t.Fatalf("clone lost the provider: %q", cloned.Provider())
	}

	// Errors: unknown key, wrong type.
	if _, err := cfg.Clone(map[string]any{"Bogus": 1}); err == nil || !strings.Contains(err.Error(), "Bogus") {
		t.Fatalf("unknown key must fail naming the key, got %v", err)
	}
	if _, err := cfg.Clone(map[string]any{"Model": 42}); err == nil {
		t.Fatal("wrongly typed override must fail")
	}
}
