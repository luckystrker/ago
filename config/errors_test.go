package config

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/luckystrker/ago/internal/httpx"
)

func TestNewAPIErrorParsesEnvelopes(t *testing.T) {
	for name, tc := range map[string]struct {
		body      string
		typ, msg  string
		wantError string
	}{
		"openai":    {`{"error":{"type":"invalid_request_error","message":"bad field"}}`, "invalid_request_error", "bad field", "op returned 400: invalid_request_error: bad field"},
		"ollama":    {`{"error":"model not found"}`, "", "model not found", "op returned 400: model not found"},
		"mistral":   {`{"message":"Unauthorized"}`, "", "Unauthorized", "op returned 400: Unauthorized"},
		"plaintext": {`gateway exploded`, "", "", "op returned 400: gateway exploded"},
	} {
		t.Run(name, func(t *testing.T) {
			e := NewAPIError(ProviderOpenAI, "op", 400, nil, []byte(tc.body))
			if e.Type != tc.typ || e.Message != tc.msg {
				t.Fatalf("type/message = %q/%q, want %q/%q", e.Type, e.Message, tc.typ, tc.msg)
			}
			if e.Error() != tc.wantError {
				t.Fatalf("Error() = %q, want %q", e.Error(), tc.wantError)
			}
			if e.Body != tc.body {
				t.Fatalf("Body = %q", e.Body)
			}
		})
	}
}

func TestAPIErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		status    int
		sentinel  error
		retryable bool
	}{
		{400, ErrBadRequest, false},
		{401, ErrAuthentication, false},
		{403, ErrPermissionDenied, false},
		{404, ErrNotFound, false},
		{409, ErrConflict, true},
		{422, ErrUnprocessable, false},
		{429, ErrRateLimited, true},
		{500, ErrServerError, true},
		{503, ErrServerError, true},
	} {
		e := NewAPIError(ProviderAnthropic, "", tc.status, nil, nil)
		wrapped := fmt.Errorf("turn failed: %w", e)
		if !errors.Is(wrapped, tc.sentinel) || !errors.Is(wrapped, ErrAPI) {
			t.Errorf("status %d: errors.Is must match %v and ErrAPI", tc.status, tc.sentinel)
		}
		if e.Retryable != tc.retryable {
			t.Errorf("status %d: Retryable = %v, want %v", tc.status, e.Retryable, tc.retryable)
		}
		var got *APIError
		if !errors.As(wrapped, &got) || got.StatusCode != tc.status || got.Provider != ProviderAnthropic {
			t.Errorf("status %d: errors.As failed: %+v", tc.status, got)
		}
	}
	if errors.Is(NewAPIError(ProviderOpenAI, "", 401, nil, nil), ErrRateLimited) {
		t.Error("a 401 must not match ErrRateLimited")
	}
}

func TestWrapStatusError(t *testing.T) {
	h := http.Header{"X-Request-Id": {"req-1"}}
	err := WrapStatusError(ProviderMistral, "mistral: chat", fmt.Errorf("outer: %w",
		&httpx.StatusError{StatusCode: 429, Status: "429 Too Many Requests", Header: h, Body: []byte(`{"message":"slow"}`)}))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 429 || !apiErr.Retryable || apiErr.Header.Get("X-Request-Id") != "req-1" {
		t.Fatalf("not converted: %v", err)
	}
	plain := errors.New("connection reset")
	if WrapStatusError(ProviderMistral, "", plain) != plain {
		t.Fatal("a non-status error must pass through unchanged")
	}
}

// TestNewAPIErrorBodyBoundary: a body of exactly 512 bytes is kept whole, one
// byte more is cut with an ellipsis.
func TestNewAPIErrorBodyBoundary(t *testing.T) {
	exact := strings.Repeat("a", 512)
	if got := NewAPIError(ProviderGemini, "", 500, nil, []byte(exact)).Body; got != exact {
		t.Fatalf("a 512-byte body must pass through unchanged, got %d bytes", len(got))
	}
	long := strings.Repeat("b", 513)
	if got := NewAPIError(ProviderGemini, "", 500, nil, []byte(long)).Body; got != long[:512]+"…" {
		t.Fatalf("a 513-byte body must truncate to 512 + ellipsis, got %d bytes", len(got))
	}
}
