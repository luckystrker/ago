package mistral

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	cfg := NewMistral("mistral-large-latest")
	if cfg.Model() != "mistral-large-latest" {
		t.Fatalf("bad model: %q", cfg.Model())
	}
	if cfg.BaseURL != DefaultBaseURL {
		t.Fatalf("base URL default not applied: %q", cfg.BaseURL)
	}
	if cfg.Temperature != nil || cfg.TopP != nil || cfg.MaxTokens != nil {
		t.Fatalf("sampling fields must be unset by default: %+v", cfg)
	}
	if cfg.SafePrompt || cfg.Streaming {
		t.Fatal("safe_prompt and streaming must default to false")
	}
	if cfg.Provider() != config.ProviderMistral || string(cfg.Provider()) != "mistral" {
		t.Fatalf("bad provider: %q", cfg.Provider())
	}
}

// --- Create: validation and env fallback ----------------------------------------

func TestCreateMissingModel(t *testing.T) {
	if _, err := (&MistralConfig{APIKey: "k"}).Create(); !errors.Is(err, config.ErrNotProvided) {
		t.Fatalf("want ErrNotProvided for a missing model, got %v", err)
	}
}

func TestCreateMissingKey(t *testing.T) {
	t.Setenv(EnvAPIKey, "")
	if _, err := NewMistral("mistral-large-latest").Create(); !errors.Is(err, config.ErrNotProvided) {
		t.Fatalf("want ErrNotProvided for a missing key, got %v", err)
	}
}

func TestCreateKeyFromEnv(t *testing.T) {
	t.Setenv(EnvAPIKey, "env-key")
	client, err := NewMistral("mistral-large-latest").Create()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client == nil {
		t.Fatal("Create must return a client")
	}
}

func TestParamsFallbacks(t *testing.T) {
	t.Setenv(EnvAPIKey, "env-key")
	cfg := NewMistral("mistral-large-latest")
	cfg.BaseURL = ""
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
	if p.Provider != config.ProviderMistral || p.Model != "mistral-large-latest" {
		t.Fatalf("identity not stamped: %+v", p)
	}
	if p.Extra != nil {
		t.Fatalf("safe_prompt must be omitted when false: %+v", p.Extra)
	}
}

// TestParamsSafePrompt: the wire field travels only when the flag is set.
func TestParamsSafePrompt(t *testing.T) {
	t.Setenv(EnvAPIKey, "env-key")
	cfg := NewMistral("mistral-large-latest")
	cfg.SafePrompt = true
	p, err := cfg.params()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Extra["safe_prompt"] != true {
		t.Fatalf("safe_prompt=true not sent: %+v", p.Extra)
	}
}

// --- Clone -----------------------------------------------------------------------

func TestCloneOverrides(t *testing.T) {
	cfg := NewMistral("mistral-large-latest")
	cfg.Temperature = f64ptr(0.3)

	cloned, err := cfg.Clone(map[string]any{
		"Model":       "mistral-small-latest",
		"MaxTokens":   int64(256),
		"Temperature": 0.9,
		"SafePrompt":  true,
		"Streaming":   true,
		"TopP":        nil,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, ok := cloned.(*MistralConfig)
	if !ok {
		t.Fatalf("Clone must return *MistralConfig, got %T", cloned)
	}
	if got.Model() != "mistral-small-latest" || !got.SafePrompt || !got.Streaming {
		t.Fatalf("value overrides not applied: %+v", got)
	}
	if got.MaxTokens == nil || *got.MaxTokens != 256 {
		t.Fatalf("int64 override not applied: %+v", got.MaxTokens)
	}
	if got.Temperature == nil || *got.Temperature != 0.9 {
		t.Fatalf("float64 override must wrap: %+v", got.Temperature)
	}
	if got.TopP != nil {
		t.Fatalf("nil override must clear the field: %+v", got.TopP)
	}
	if cfg.Model() != "mistral-large-latest" || cfg.SafePrompt || cfg.Streaming {
		t.Fatalf("Clone mutated the receiver: %+v", cfg)
	}
	if cloned.Provider() != config.ProviderMistral {
		t.Fatalf("clone lost the provider: %q", cloned.Provider())
	}
}

func TestCloneErrors(t *testing.T) {
	cfg := NewMistral("mistral-large-latest")
	if _, err := cfg.Clone(map[string]any{"Bogus": 1}); err == nil || !strings.Contains(err.Error(), "Bogus") {
		t.Fatalf("unknown key must fail naming the key, got %v", err)
	}
	if _, err := cfg.Clone(map[string]any{"Model": 42}); err == nil {
		t.Fatal("wrongly typed override must fail")
	}
	if _, err := cfg.Clone(map[string]any{"SafePrompt": 1}); err == nil {
		t.Fatal("wrongly typed override must fail")
	}
}

// --- Call over HTTP ---------------------------------------------------------------

const mistralCompletionBody = `{
  "id": "cmpl-m1",
  "model": "mistral-large-actual",
  "choices": [{"finish_reason": "stop", "message": {"content": "hello from mistral"}}],
  "usage": {"prompt_tokens": 5, "completion_tokens": 6, "total_tokens": 11}
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
	srv, method, path, header, captured := captureServer(t, http.StatusOK, mistralCompletionBody)

	cfg := NewMistral("mistral-large-latest")
	cfg.BaseURL = srv.URL
	cfg.APIKey = "test-key"
	cfg.Temperature = f64ptr(0.4)
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
	if body["model"] != "mistral-large-latest" {
		t.Fatalf("model not sent: %+v", body["model"])
	}
	if body["temperature"] != float64(0.4) {
		t.Fatalf("sampling params not sent: %+v", body)
	}
	if _, ok := body["safe_prompt"]; ok {
		t.Fatal("safe_prompt must be omitted when the flag is false")
	}
	msgs := body["messages"].([]any)
	if sys := msgs[0].(map[string]any); sys["content"] != "be brief" {
		t.Fatalf("system prompt not sent: %+v", sys)
	}

	if resp.Content() != "hello from mistral" {
		t.Fatalf("bad content: %q", resp.Content())
	}
	if resp.Model != "mistral-large-actual" || resp.ResponseID != "cmpl-m1" || resp.FinishReason != "stop" {
		t.Fatalf("bad tracing fields: %+v", resp)
	}
	if resp.Provider != "mistral" {
		t.Fatalf("provider not stamped: %q", resp.Provider)
	}
	if resp.Usage.TotalTokens == nil || *resp.Usage.TotalTokens != 11 {
		t.Fatalf("usage not normalized: %+v", resp.Usage)
	}
}

func TestCallAPIError(t *testing.T) {
	srv, _, _, _, _ := captureServer(t, http.StatusUnauthorized, `{"error": "bad key"}`)
	cfg := NewMistral("mistral-large-latest")
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

// --- media inputs (W1-B) ---------------------------------------------------------

// TestCallDocumentAndImageInputs pins the exact wire JSON of every Mistral
// input chunk: documents as document_url (inline data and URL), images as
// image_url (bare URL or URL + detail from vendor metadata).
func TestCallDocumentAndImageInputs(t *testing.T) {
	srv, _, _, _, captured := captureServer(t, http.StatusOK, mistralCompletionBody)
	cfg := NewMistral("mistral-large-latest")
	cfg.BaseURL = srv.URL
	cfg.APIKey = "test-key"
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	req := events.ModelRequest{Parts: []events.Input{
		&events.TextInput{Content: "look"},
		&events.BinaryInput{
			Data: []byte("hi"), MediaType: "application/pdf", Kind: events.BinaryDocument,
			VendorMetadata: map[string]any{"filename": "scan.pdf"},
		},
		&events.UrlInput{URL: "https://example.com/report.pdf", Kind: events.BinaryDocument},
		&events.BinaryInput{
			Data: []byte{1, 2, 3}, MediaType: "image/png", Kind: events.BinaryImage,
			VendorMetadata: map[string]any{"detail": "high"},
		},
		&events.UrlInput{URL: "https://example.com/cat.png", Kind: events.BinaryImage},
	}}
	if _, err := client.Call(context.Background(), []events.Event{&req}, nil, config.CallOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	body := requestJSON(t, *captured)
	content := body["messages"].([]any)[1].(map[string]any)["content"]
	want := []any{
		map[string]any{"type": "text", "text": "look"},
		map[string]any{
			"type":          "document_url",
			"document_url":  "data:application/pdf;base64,aGk=",
			"document_name": "scan.pdf",
		},
		map[string]any{"type": "document_url", "document_url": "https://example.com/report.pdf"},
		map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url":    "data:image/png;base64,AQID",
				"detail": "high",
			},
		},
		map[string]any{"type": "image_url", "image_url": "https://example.com/cat.png"},
	}
	if !reflect.DeepEqual(content, want) {
		t.Fatalf("wire content = %#v, want %#v", content, want)
	}
}

func TestCallDocumentWithoutFilename(t *testing.T) {
	srv, _, _, _, captured := captureServer(t, http.StatusOK, mistralCompletionBody)
	cfg := NewMistral("mistral-large-latest")
	cfg.BaseURL = srv.URL
	cfg.APIKey = "test-key"
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// An untyped binary is a document too; no vendor filename means no
	// document_name key at all.
	doc := events.BinaryInput{Data: []byte("x"), MediaType: "text/plain", Kind: events.BinaryBinary}
	req := events.ModelRequest{Parts: []events.Input{&doc}}
	if _, err := client.Call(context.Background(), []events.Event{&req}, nil, config.CallOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	content := requestJSON(t, *captured)["messages"].([]any)[1].(map[string]any)["content"]
	want := []any{map[string]any{
		"type":         "document_url",
		"document_url": "data:text/plain;base64,eA==",
	}}
	if !reflect.DeepEqual(content, want) {
		t.Fatalf("wire content = %#v, want %#v", content, want)
	}
}

func TestCallUnsupportedMediaInputs(t *testing.T) {
	srv, _, _, _, captured := captureServer(t, http.StatusOK, mistralCompletionBody)
	cfg := NewMistral("mistral-large-latest")
	cfg.BaseURL = srv.URL
	cfg.APIKey = "test-key"
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cases := []events.Input{
		&events.BinaryInput{Data: []byte{1}, MediaType: "audio/wav", Kind: events.BinaryAudio},
		&events.UrlInput{URL: "https://example.com/a.mp4", Kind: events.BinaryVideo},
		&events.FileIdInput{FileID: "file-1"},
		&events.BinaryInput{
			Data: []byte{1}, MediaType: "image/png", Kind: events.BinaryImage,
			VendorMetadata: map[string]any{"detail": 1},
		},
	}
	for _, part := range cases {
		req := events.ModelRequest{Parts: []events.Input{part}}
		_, err := client.Call(context.Background(), []events.Event{&req}, nil, config.CallOptions{})
		if !errors.Is(err, config.ErrUnsupportedInput) {
			t.Fatalf("%T: want ErrUnsupportedInput, got %v", part, err)
		}
	}
	if len(*captured) != 0 {
		t.Fatal("no request may reach the API on an unmappable part")
	}
}

// TestCallThinkChunksBecomeReasoning: a Mistral reasoning answer arrives as a
// content array; the thinking chunks become ModelReasoning and only the text
// chunks reach ModelMessage.
func TestCallThinkChunksBecomeReasoning(t *testing.T) {
	body := `{
  "id": "cmpl-m1",
  "model": "mistral-large-latest",
  "choices": [{
    "finish_reason": "stop",
    "message": {
      "content": [
        {"type": "thinking", "thinking": [{"type": "text", "text": "let me see"}]},
        {"type": "text", "text": "the answer"}
      ]
    }
  }]
}`
	srv, _, _, _, _ := captureServer(t, http.StatusOK, body)
	cfg := NewMistral("mistral-large-latest")
	cfg.BaseURL = srv.URL
	cfg.APIKey = "test-key"
	client, err := cfg.Create()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ms := stream.NewMemoryStream()
	var sent []events.Event
	ms.Subscribe(func(ev events.Event) error {
		sent = append(sent, ev)
		return nil
	}, stream.SubscribeOptions{})
	conv := agocontext.New(ms)

	resp, err := client.Call(context.Background(), nil, conv, config.CallOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content() != "the answer" {
		t.Fatalf("message content = %q, want %q", resp.Content(), "the answer")
	}
	var reasoning []string
	for _, ev := range sent {
		if r, ok := ev.(*events.ModelReasoning); ok {
			reasoning = append(reasoning, r.Content)
		}
	}
	if len(reasoning) != 1 || reasoning[0] != "let me see" {
		t.Fatalf("reasoning events = %v, want [let me see]", reasoning)
	}
}
