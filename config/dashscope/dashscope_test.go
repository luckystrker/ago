package dashscope

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

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
)

func f64ptr(v float64) *float64 { return &v }
func i64ptr(v int64) *int64     { return &v }

// --- defaults ------------------------------------------------------------------

func TestDefaults(t *testing.T) {
	cfg := NewDashScope("qwen-max")
	if cfg.Model() != "qwen-max" {
		t.Fatalf("bad model: %q", cfg.Model())
	}
	if cfg.BaseURL != DefaultBaseURL {
		t.Fatalf("base URL default not applied: %q", cfg.BaseURL)
	}
	if cfg.Temperature != nil || cfg.TopP != nil || cfg.MaxTokens != nil {
		t.Fatalf("sampling fields must be unset by default: %+v", cfg)
	}
	if cfg.Streaming {
		t.Fatal("streaming must default to false")
	}
	if cfg.Provider() != config.ProviderDashScope || string(cfg.Provider()) != "dashscope" {
		t.Fatalf("bad provider: %q", cfg.Provider())
	}
}

// --- Create: validation and env fallback ----------------------------------------

func TestCreateMissingModel(t *testing.T) {
	if _, err := (&DashScopeConfig{APIKey: "k"}).Create(); !errors.Is(err, config.ErrNotProvided) {
		t.Fatalf("want ErrNotProvided for a missing model, got %v", err)
	}
}

func TestCreateMissingKey(t *testing.T) {
	t.Setenv(EnvAPIKey, "")
	if _, err := NewDashScope("qwen-max").Create(); !errors.Is(err, config.ErrNotProvided) {
		t.Fatalf("want ErrNotProvided for a missing key, got %v", err)
	}
}

func TestCreateKeyFromEnv(t *testing.T) {
	t.Setenv(EnvAPIKey, "env-key")
	client, err := NewDashScope("qwen-max").Create()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client == nil {
		t.Fatal("Create must return a client")
	}
}

func TestParamsFallbacks(t *testing.T) {
	t.Setenv(EnvAPIKey, "env-key")
	cfg := NewDashScope("qwen-max")
	cfg.BaseURL = ""
	cfg.Streaming = true
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
	if !p.Streaming {
		t.Fatal("streaming flag not forwarded")
	}
	if p.Provider != config.ProviderDashScope || p.Model != "qwen-max" {
		t.Fatalf("identity not stamped: %+v", p)
	}
}

// --- Clone -----------------------------------------------------------------------

func TestCloneOverrides(t *testing.T) {
	cfg := NewDashScope("qwen-max")
	cfg.Temperature = f64ptr(0.7)

	cloned, err := cfg.Clone(map[string]any{
		"model":       "qwen-plus",
		"Temperature": 0.1,
		"MaxTokens":   int64(64),
		"Streaming":   true,
		"TopP":        nil,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, ok := cloned.(*DashScopeConfig)
	if !ok {
		t.Fatalf("Clone must return *DashScopeConfig, got %T", cloned)
	}
	if got.Model() != "qwen-plus" || !got.Streaming {
		t.Fatalf("value overrides not applied: %+v", got)
	}
	if got.Temperature == nil || *got.Temperature != 0.1 {
		t.Fatalf("float64 override must wrap: %+v", got.Temperature)
	}
	if got.MaxTokens == nil || *got.MaxTokens != 64 {
		t.Fatalf("int64 override not applied: %+v", got.MaxTokens)
	}
	if got.TopP != nil {
		t.Fatalf("nil override must clear the field: %+v", got.TopP)
	}
	if cfg.Model() != "qwen-max" || cfg.Streaming || *cfg.Temperature != 0.7 {
		t.Fatalf("Clone mutated the receiver: %+v", cfg)
	}
	if cloned.Provider() != config.ProviderDashScope {
		t.Fatalf("clone lost the provider: %q", cloned.Provider())
	}
}

func TestCloneErrors(t *testing.T) {
	cfg := NewDashScope("qwen-max")
	if _, err := cfg.Clone(map[string]any{"Bogus": 1}); err == nil || !strings.Contains(err.Error(), "Bogus") {
		t.Fatalf("unknown key must fail naming the key, got %v", err)
	}
	if _, err := cfg.Clone(map[string]any{"Model": 42}); err == nil {
		t.Fatal("wrongly typed override must fail")
	}
	if _, err := cfg.Clone(map[string]any{"Streaming": "yes"}); err == nil {
		t.Fatal("wrongly typed override must fail")
	}
}

// --- Call over HTTP ---------------------------------------------------------------

const dashscopeCompletionBody = `{
  "id": "cmpl-d1",
  "model": "qwen-max-actual",
  "choices": [{"finish_reason": "stop", "message": {"content": "hello from dashscope"}}],
  "usage": {"prompt_tokens": 2, "completion_tokens": 3, "total_tokens": 5}
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
	srv, method, path, header, captured := captureServer(t, http.StatusOK, dashscopeCompletionBody)

	cfg := NewDashScope("qwen-max")
	cfg.BaseURL = srv.URL
	cfg.APIKey = "test-key"
	cfg.MaxTokens = i64ptr(64)
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

	if *method != http.MethodPost || *path != "/chat/completions" {
		t.Fatalf("bad request target: %s %s", *method, *path)
	}
	if got := header.Get("Authorization"); got != "Bearer test-key" {
		t.Fatalf("bad authorization header: %q", got)
	}

	body := requestJSON(t, *captured)
	if body["model"] != "qwen-max" {
		t.Fatalf("model not sent: %+v", body["model"])
	}
	if body["max_tokens"] != float64(64) {
		t.Fatalf("max_tokens not sent: %+v", body)
	}
	msgs := body["messages"].([]any)
	if sys := msgs[0].(map[string]any); sys["content"] != "be brief" {
		t.Fatalf("system prompt not sent: %+v", sys)
	}

	if resp.Content() != "hello from dashscope" {
		t.Fatalf("bad content: %q", resp.Content())
	}
	if resp.Model != "qwen-max-actual" || resp.ResponseID != "cmpl-d1" || resp.FinishReason != "stop" {
		t.Fatalf("bad tracing fields: %+v", resp)
	}
	if resp.Provider != "dashscope" {
		t.Fatalf("provider not stamped: %q", resp.Provider)
	}
	if resp.Usage.CompletionTokens == nil || *resp.Usage.CompletionTokens != 3 {
		t.Fatalf("usage not normalized: %+v", resp.Usage)
	}
}

func TestCallAPIError(t *testing.T) {
	srv, _, _, _, _ := captureServer(t, http.StatusUnauthorized, `{"error": "bad key"}`)
	cfg := NewDashScope("qwen-max")
	cfg.BaseURL = srv.URL
	cfg.APIKey = "test-key"
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, err = client.Call(context.Background(), nil, nil, config.CallOptions{})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want an error carrying the status, got %v", err)
	}
}
