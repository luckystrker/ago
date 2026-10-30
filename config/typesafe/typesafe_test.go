package typesafe

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/ago/config"
)

func TestDefaults(t *testing.T) {
	cfg := NewTypeSafe("ts-judge-v1")
	if cfg.Model() != "ts-judge-v1" {
		t.Fatalf("bad model: %q", cfg.Model())
	}
	if cfg.BooleanThreshold != DefaultBooleanThreshold {
		t.Fatalf("boolean threshold default not applied: %v", cfg.BooleanThreshold)
	}
	if cfg.APIKey != "" || cfg.BaseURL != "" || cfg.Timeout != 0 || cfg.Criteria != nil {
		t.Fatalf("optional fields must be unset by default: %+v", cfg)
	}
	if cfg.Provider() != config.ProviderTypeSafe || string(cfg.Provider()) != "typesafe" {
		t.Fatalf("bad provider: %q", cfg.Provider())
	}
}

// TestCreateNotImplemented pins the stub contract: a valid config reports
// config.ErrNotImplemented naming the provider.
func TestCreateNotImplemented(t *testing.T) {
	_, err := NewTypeSafe("ts-judge-v1").Create()
	if !errors.Is(err, config.ErrNotImplemented) {
		t.Fatalf("want ErrNotImplemented, got %v", err)
	}
	if !strings.Contains(err.Error(), "typesafe") {
		t.Fatalf("the error must name the provider, got %v", err)
	}
}

func TestCreateMissingModel(t *testing.T) {
	if _, err := (&TypeSafeConfig{BooleanThreshold: DefaultBooleanThreshold}).Create(); !errors.Is(err, config.ErrNotProvided) {
		t.Fatalf("want ErrNotProvided for a missing model, got %v", err)
	}
}

func TestCloneOverrides(t *testing.T) {
	cfg := NewTypeSafe("ts-judge-v1")
	cfg.Criteria = map[string]string{"accuracy": "answers the question"}

	cloned, err := cfg.Clone(map[string]any{
		"Model":            "ts-judge-v2",
		"APIKey":           "k",
		"BaseURL":          "https://typesafe.example",
		"timeout":          30 * time.Second,
		"BooleanThreshold": 0.75,
		"Criteria":         map[string]string{"clarity": "easy to follow"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, ok := cloned.(*TypeSafeConfig)
	if !ok {
		t.Fatalf("Clone must return *TypeSafeConfig, got %T", cloned)
	}
	if got.Model() != "ts-judge-v2" || got.APIKey != "k" || got.BaseURL != "https://typesafe.example" {
		t.Fatalf("overrides not applied: %+v", got)
	}
	if got.Timeout != 30*time.Second {
		t.Fatalf("timeout override not applied: %v", got.Timeout)
	}
	if got.BooleanThreshold != 0.75 {
		t.Fatalf("boolean threshold override not applied: %v", got.BooleanThreshold)
	}
	if len(got.Criteria) != 1 || got.Criteria["clarity"] != "easy to follow" {
		t.Fatalf("map override not applied: %+v", got.Criteria)
	}
	// The receiver stays untouched, including its map memory.
	if cfg.Model() != "ts-judge-v1" || len(cfg.Criteria) != 1 || cfg.Criteria["accuracy"] != "answers the question" {
		t.Fatalf("Clone mutated the receiver: %+v", cfg)
	}
	if cloned.Provider() != config.ProviderTypeSafe {
		t.Fatalf("clone lost the provider: %q", cloned.Provider())
	}

	// Errors: unknown key, wrong type.
	if _, err := cfg.Clone(map[string]any{"Bogus": 1}); err == nil || !strings.Contains(err.Error(), "Bogus") {
		t.Fatalf("unknown key must fail naming the key, got %v", err)
	}
	if _, err := cfg.Clone(map[string]any{"Timeout": "30s"}); err == nil {
		t.Fatal("wrongly typed override must fail")
	}
	if _, err := cfg.Clone(map[string]any{"Criteria": "accuracy"}); err == nil {
		t.Fatal("wrongly typed override must fail")
	}
}
