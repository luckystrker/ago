package anthropic

import (
	"errors"
	"testing"
	"time"

	"github.com/luckystrker/ago/config"
)

func f64ptr(v float64) *float64 { return &v }
func i64ptr(v int64) *int64     { return &v }

func TestNewAnthropicDefaults(t *testing.T) {
	cfg := NewAnthropic("claude-sonnet")
	if cfg.Model() != "claude-sonnet" {
		t.Fatalf("Model() = %q", cfg.Model())
	}
	if cfg.Provider() != config.ProviderAnthropic {
		t.Fatalf("Provider() = %q", cfg.Provider())
	}
	if cfg.BaseURL != DefaultBaseURL {
		t.Fatalf("BaseURL = %q", cfg.BaseURL)
	}
	if cfg.MaxTokens != 4096 {
		t.Fatalf("MaxTokens = %d", cfg.MaxTokens)
	}
	if cfg.MaxRetries != 2 {
		t.Fatalf("MaxRetries = %d", cfg.MaxRetries)
	}
	if !cfg.PromptCaching {
		t.Fatal("PromptCaching must default to true")
	}
	if cfg.Streaming || cfg.Temperature != nil || cfg.TopP != nil || cfg.Timeout != 0 || len(cfg.StopSequences) != 0 {
		t.Fatalf("unexpected non-zero defaults: %+v", cfg)
	}
}

func TestCreateValidation(t *testing.T) {
	t.Setenv(APIKeyEnv, "")

	if _, err := NewAnthropic("").Create(); !errors.Is(err, config.ErrNotProvided) {
		t.Fatalf("missing model: want ErrNotProvided, got %v", err)
	}
	if _, err := NewAnthropic("claude-sonnet").Create(); !errors.Is(err, config.ErrNotProvided) {
		t.Fatalf("missing key: want ErrNotProvided, got %v", err)
	}
	// The env fallback satisfies Create and reaches the request headers.
	t.Setenv(APIKeyEnv, "env-key")
	cfg := NewAnthropic("claude-sonnet")
	cl, err := cfg.Create()
	if err != nil {
		t.Fatalf("Create with env key: %v", err)
	}
	if _, ok := cl.(config.LLMClient); !ok {
		t.Fatalf("Create returned %T", cl)
	}
}

func TestCreateDefaults(t *testing.T) {
	t.Setenv(APIKeyEnv, "env-key")
	// A hand-built config with zero values still gets the documented
	// MaxTokens/BaseURL defaults (the API requires max_tokens).
	cfg := &AnthropicConfig{model: "claude-sonnet", APIKey: "k"}
	cl, err := cfg.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	c := cl.(*Client)
	if c.maxTokens != DefaultMaxTokens {
		t.Fatalf("maxTokens = %d", c.maxTokens)
	}
	if c.baseURL != DefaultBaseURL {
		t.Fatalf("baseURL = %q", c.baseURL)
	}
	if c.promptCaching {
		t.Fatal("hand-built config must not invent PromptCaching=true")
	}
}

func TestClone(t *testing.T) {
	cfg := NewAnthropic("claude-sonnet")
	cfg.Temperature = f64ptr(0.5)

	cloned, err := cfg.Clone(map[string]any{
		"model":         "claude-opus",
		"MaxTokens":     int64(1024),
		"temperature":   0.9, // plain float64 wraps into *float64
		"StopSequences": []string{"END"},
		"PromptCaching": false,
		"Timeout":       3 * time.Second,
	})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	got := cloned.(*AnthropicConfig)
	if got.Model() != "claude-opus" {
		t.Fatalf("model = %q", got.Model())
	}
	if got.MaxTokens != 1024 {
		t.Fatalf("MaxTokens = %d", got.MaxTokens)
	}
	if got.Temperature == nil || *got.Temperature != 0.9 {
		t.Fatalf("Temperature = %v", got.Temperature)
	}
	if len(got.StopSequences) != 1 || got.StopSequences[0] != "END" {
		t.Fatalf("StopSequences = %v", got.StopSequences)
	}
	if got.PromptCaching {
		t.Fatal("PromptCaching override failed")
	}
	if got.Timeout != 3*time.Second {
		t.Fatalf("Timeout = %v", got.Timeout)
	}
	// The original is untouched.
	if cfg.Model() != "claude-sonnet" || *cfg.Temperature != 0.5 || cfg.PromptCaching != true {
		t.Fatalf("Clone mutated the original: %+v", cfg)
	}
	if cfg.StopSequences != nil {
		t.Fatalf("StopSequences leaked into the original: %v", cfg.StopSequences)
	}
	// nil clears a pointer field.
	cleared, err := cfg.Clone(map[string]any{"temperature": nil})
	if err != nil {
		t.Fatalf("Clone(nil): %v", err)
	}
	if cleared.(*AnthropicConfig).Temperature != nil {
		t.Fatal("nil override must clear Temperature")
	}
}

func TestCloneErrors(t *testing.T) {
	cfg := NewAnthropic("claude-sonnet")
	if _, err := cfg.Clone(map[string]any{"Nope": 1}); err == nil {
		t.Fatal("unknown override key must error")
	}
	if _, err := cfg.Clone(map[string]any{"MaxTokens": "big"}); err == nil {
		t.Fatal("mismatched override type must error")
	}
	if _, err := cfg.Clone(map[string]any{"Streaming": nil}); err == nil {
		t.Fatal("nil override on a non-pointer field must error")
	}
}
