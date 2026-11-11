package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/luckystrker/ago/internal/httpx"
)

// APIError is the typed failure every HTTP provider client returns for a
// non-2xx answer, once the transport retries are spent. It is returned
// wrapped or bare: extract it with errors.As and branch on StatusCode or on
// the classification sentinels with errors.Is.
//
//	var apiErr *config.APIError
//	if errors.As(err, &apiErr) && apiErr.Retryable { ... }
//	if errors.Is(err, config.ErrRateLimited) { ... }
//
// Python parity: the provider SDKs raise typed status exceptions
// (RateLimitError, AuthenticationError, ...). AG2 adds no wrapper of its own,
// so the sentinels below mirror the SDK classes by status code.
type APIError struct {
	// Provider names the vendor that answered.
	Provider Provider
	// Op names the failing call for the message ("anthropic: messages API");
	// it may be empty.
	Op string
	// StatusCode is the HTTP status of the final attempt.
	StatusCode int
	// Type is the vendor's error type when the body carries one
	// ("invalid_request_error"); empty otherwise.
	Type string
	// Message is the vendor's error message when the body carries one; empty
	// when the body is not a recognized error envelope.
	Message string
	// Body is the first bytes of the raw response body.
	Body string
	// Retryable reports whether the status is one the transport retries
	// (408, 409, 429, 5xx). A caller that wraps the model call in its own
	// retry loop can use it to decide.
	Retryable bool
	// Header carries the response headers when the client read them (the
	// streaming paths do); nil otherwise.
	Header http.Header
}

// Classification sentinels, matched by APIError.Is through the status code
// (the OpenAI/Anthropic SDK exception classes). ErrAPI matches every APIError.
var (
	ErrAPI              = errors.New("config: API error")
	ErrBadRequest       = errors.New("config: API bad request (400)")
	ErrAuthentication   = errors.New("config: API authentication failed (401)")
	ErrPermissionDenied = errors.New("config: API permission denied (403)")
	ErrNotFound         = errors.New("config: API resource not found (404)")
	ErrConflict         = errors.New("config: API conflict (409)")
	ErrUnprocessable    = errors.New("config: API unprocessable entity (422)")
	ErrRateLimited      = errors.New("config: API rate limited (429)")
	ErrServerError      = errors.New("config: API server error (5xx)")
)

// maxErrorBody caps the raw body kept on an APIError.
const maxErrorBody = 512

// NewAPIError builds the typed error for a non-2xx answer. op prefixes the
// message ("config: chat completions"); header may be nil.
func NewAPIError(provider Provider, op string, status int, header http.Header, body []byte) *APIError {
	e := &APIError{
		Provider:   provider,
		Op:         op,
		StatusCode: status,
		Retryable:  httpx.RetryableStatus(status),
		Header:     header,
	}
	e.Body = strings.TrimSpace(string(body))
	if len(e.Body) > maxErrorBody {
		e.Body = e.Body[:maxErrorBody] + "…"
	}
	e.Type, e.Message = parseErrorEnvelope(body)
	return e
}

// WrapStatusError converts a streaming transport failure into an *APIError
// when err carries an httpx.StatusError; any other error is returned as is.
// Provider clients that read their body through httpx.PostJSONStream call it
// on the stream's error.
func WrapStatusError(provider Provider, op string, err error) error {
	var se *httpx.StatusError
	if errors.As(err, &se) {
		return NewAPIError(provider, op, se.StatusCode, se.Header, se.Body)
	}
	return err
}

// Error renders "<op> returned <status>: <message or body>".
func (e *APIError) Error() string {
	detail := e.Body
	if e.Message != "" {
		detail = e.Message
		if e.Type != "" {
			detail = e.Type + ": " + e.Message
		}
	}
	return fmt.Sprintf("%sreturned %d: %s", opPrefix(e.Op), e.StatusCode, detail)
}

func opPrefix(op string) string {
	if op == "" {
		return ""
	}
	return op + " "
}

// Is matches the classification sentinels by status code.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrAPI:
		return true
	case ErrBadRequest:
		return e.StatusCode == http.StatusBadRequest
	case ErrAuthentication:
		return e.StatusCode == http.StatusUnauthorized
	case ErrPermissionDenied:
		return e.StatusCode == http.StatusForbidden
	case ErrNotFound:
		return e.StatusCode == http.StatusNotFound
	case ErrConflict:
		return e.StatusCode == http.StatusConflict
	case ErrUnprocessable:
		return e.StatusCode == http.StatusUnprocessableEntity
	case ErrRateLimited:
		return e.StatusCode == http.StatusTooManyRequests
	case ErrServerError:
		return e.StatusCode >= 500
	}
	return false
}

// parseErrorEnvelope extracts the vendor's error type and message from the
// common envelopes: {"error": {"type", "message"}} (OpenAI, Anthropic, Gemini,
// Z.AI), {"error": "text"} (Ollama), {"message": "text"} (Mistral) and
// {"detail": "text"}. An unrecognized body yields empty strings.
func parseErrorEnvelope(body []byte) (typ, msg string) {
	var env struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Detail  string          `json:"detail"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return "", ""
	}
	if len(env.Error) > 0 {
		var text string
		if json.Unmarshal(env.Error, &text) == nil && text != "" {
			return "", text
		}
		var obj struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if json.Unmarshal(env.Error, &obj) == nil && obj.Message != "" {
			return obj.Type, obj.Message
		}
	}
	if env.Message != "" {
		return "", env.Message
	}
	return "", env.Detail
}
