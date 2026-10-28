package zai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ago/config"
	agocontext "ago/context"
	"ago/events"
	"ago/stream"
)

func f64ptr(v float64) *float64 { return &v }
func i64ptr(v int64) *int64     { return &v }

// --- defaults ------------------------------------------------------------------

func TestDefaults(t *testing.T) {
	cfg := NewZAI("glm-4.7")
	if cfg.Model() != "glm-4.7" {
		t.Fatalf("bad model: %q", cfg.Model())
	}
	if cfg.BaseURL != DefaultBaseURL {
		t.Fatalf("base URL default not applied: %q", cfg.BaseURL)
	}
	if cfg.MaxRetries != DefaultMaxRetries {
		t.Fatalf("max retries default not applied: %d", cfg.MaxRetries)
	}
	if cfg.Temperature != nil || cfg.TopP != nil || cfg.MaxTokens != nil {
		t.Fatalf("sampling fields must be unset by default: %+v", cfg)
	}
	if cfg.Streaming {
		t.Fatal("streaming must default to false")
	}
	if cfg.Provider() != config.ProviderZAI || string(cfg.Provider()) != "zai" {
		t.Fatalf("bad provider: %q", cfg.Provider())
	}
}

// --- Create: validation and env fallback ----------------------------------------

func TestCreateMissingModel(t *testing.T) {
	if _, err := (&ZAIConfig{APIKey: "k"}).Create(); !errors.Is(err, config.ErrNotProvided) {
		t.Fatalf("want ErrNotProvided for a missing model, got %v", err)
	}
}

func TestCreateMissingKey(t *testing.T) {
	t.Setenv(EnvAPIKey, "")
	if _, err := NewZAI("glm-4.7").Create(); !errors.Is(err, config.ErrNotProvided) {
		t.Fatalf("want ErrNotProvided for a missing key, got %v", err)
	}
}

func TestCreateKeyFromEnv(t *testing.T) {
	t.Setenv(EnvAPIKey, "env-key")
	client, err := NewZAI("glm-4.7").Create()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client == nil {
		t.Fatal("Create must return a client")
	}
}

// TestParamsFallbacks checks the parameter rendering: an empty BaseURL falls
// back to the default (Python parity: base_url=None → SDK default) and the
// provider/path/identity fields are stamped.
func TestParamsFallbacks(t *testing.T) {
	t.Setenv(EnvAPIKey, "env-key")
	cfg := NewZAI("glm-4.7")
	cfg.BaseURL = ""
	cfg.MaxRetries = 0
	p, err := cfg.params()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.BaseURL != DefaultBaseURL {
		t.Fatalf("empty BaseURL must fall back to the default, got %q", p.BaseURL)
	}
	if p.Path != "/chat/completions" {
		t.Fatalf("bad path: %q", p.Path)
	}
	if p.Provider != config.ProviderZAI || p.Model != "glm-4.7" {
		t.Fatalf("identity not stamped: %+v", p)
	}
}

// --- Clone -----------------------------------------------------------------------

func TestCloneOverrides(t *testing.T) {
	cfg := NewZAI("glm-4.7")
	cfg.Temperature = f64ptr(0.7)

	cloned, err := cfg.Clone(map[string]any{
		"model":       "glm-5",
		"Temperature": 0.2, // value type wraps into *float64
		"MaxTokens":   int64(512),
		"Streaming":   true,
		"MaxRetries":  1,
		"TopP":        nil, // clears
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, ok := cloned.(*ZAIConfig)
	if !ok {
		t.Fatalf("Clone must return *ZAIConfig, got %T", cloned)
	}
	if got.Model() != "glm-5" || !got.Streaming || got.MaxRetries != 1 {
		t.Fatalf("value overrides not applied: %+v", got)
	}
	if got.Temperature == nil || *got.Temperature != 0.2 {
		t.Fatalf("float64 override must wrap: %+v", got.Temperature)
	}
	if got.MaxTokens == nil || *got.MaxTokens != 512 {
		t.Fatalf("int64 override not applied: %+v", got.MaxTokens)
	}
	if got.TopP != nil {
		t.Fatalf("nil override must clear the field: %+v", got.TopP)
	}
	// The receiver stays untouched (dataclasses.replace parity).
	if cfg.Model() != "glm-4.7" || *cfg.Temperature != 0.7 {
		t.Fatalf("Clone mutated the receiver: %+v", cfg)
	}
	if cloned.Provider() != config.ProviderZAI {
		t.Fatalf("clone lost the provider: %q", cloned.Provider())
	}
}

func TestCloneErrors(t *testing.T) {
	cfg := NewZAI("glm-4.7")
	if _, err := cfg.Clone(map[string]any{"Bogus": 1}); err == nil || !strings.Contains(err.Error(), "Bogus") {
		t.Fatalf("unknown key must fail naming the key, got %v", err)
	}
	if _, err := cfg.Clone(map[string]any{"Model": 42}); err == nil {
		t.Fatal("wrongly typed override must fail")
	}
	if _, err := cfg.Clone(map[string]any{"Streaming": "yes"}); err == nil {
		t.Fatal("wrongly typed override must fail")
	}
	if _, err := cfg.Clone(map[string]any{"Temperature": "0.7"}); err == nil {
		t.Fatal("wrongly typed override must fail")
	}
}

// --- Call over HTTP ---------------------------------------------------------------

const zaiCompletionBody = `{
  "id": "chatcmpl-z1",
  "model": "glm-4.7-actual",
  "choices": [{"finish_reason": "stop", "message": {"content": "hello from zai"}}],
  "usage": {"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7}
}`

// captureServer records method, path, headers and body of one request and
// answers status with body.
func captureServer(t *testing.T, status int, body string) (*httptest.Server, *string, *string, *http.Header, *[]byte) {
	t.Helper()
	var (
		method   string
		path     string
		header   http.Header
		captured []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request: %v", err)
		}
		method, path, header, captured = r.Method, r.URL.Path, r.Header, b
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)
	return srv, &method, &path, &header, &captured
}

func requestJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("captured request is not JSON: %v", err)
	}
	return m
}

func TestCall(t *testing.T) {
	srv, method, path, header, captured := captureServer(t, http.StatusOK, zaiCompletionBody)

	cfg := NewZAI("glm-4.7")
	cfg.BaseURL = srv.URL
	cfg.APIKey = "test-key"
	cfg.Temperature = f64ptr(0.6)
	cfg.TopP = f64ptr(0.9)
	cfg.MaxTokens = i64ptr(128)
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	conv := agocontext.New(stream.NewMemoryStream())
	conv.AddPrompt("be brief")
	req := events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "hi"}}}

	resp, err := client.Call(context.Background(), []events.Event{&req}, conv, config.CallOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The wire request hits the chat-completions path with bearer auth.
	if *method != http.MethodPost || *path != "/chat/completions" {
		t.Fatalf("bad request target: %s %s", *method, *path)
	}
	if got := header.Get("Authorization"); got != "Bearer test-key" {
		t.Fatalf("bad authorization header: %q", got)
	}

	body := requestJSON(t, *captured)
	if body["model"] != "glm-4.7" {
		t.Fatalf("model not sent: %+v", body["model"])
	}
	if body["temperature"] != float64(0.6) || body["top_p"] != float64(0.9) || body["max_tokens"] != float64(128) {
		t.Fatalf("sampling params not sent: %+v", body)
	}
	msgs := body["messages"].([]any)
	if sys := msgs[0].(map[string]any); sys["content"] != "be brief" {
		t.Fatalf("system prompt not sent: %+v", sys)
	}

	// The answer normalizes into a ModelResponse stamped with the provider.
	if resp.Content() != "hello from zai" {
		t.Fatalf("bad content: %q", resp.Content())
	}
	if resp.Model != "glm-4.7-actual" || resp.ResponseID != "chatcmpl-z1" || resp.FinishReason != "stop" {
		t.Fatalf("bad tracing fields: %+v", resp)
	}
	if resp.Provider != "zai" {
		t.Fatalf("provider not stamped: %q", resp.Provider)
	}
	if resp.Usage.PromptTokens == nil || *resp.Usage.PromptTokens != 3 {
		t.Fatalf("usage not normalized: %+v", resp.Usage)
	}
}

func TestCallAPIError(t *testing.T) {
	srv, _, _, _, _ := captureServer(t, http.StatusUnauthorized, `{"error": "bad key"}`)
	cfg := NewZAI("glm-4.7")
	cfg.BaseURL = srv.URL
	cfg.APIKey = "test-key"
	cfg.MaxRetries = 0 // no retries: the test asserts the surfaced error
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, err = client.Call(context.Background(), nil, nil, config.CallOptions{})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want an error carrying the status, got %v", err)
	}
}
