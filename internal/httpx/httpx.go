// Package httpx is the JSON-over-HTTP transport shared by the LLM provider
// clients: one POST helper for plain JSON endpoints and one for SSE streams,
// both with transport-level retries (429/5xx and network errors) honoring
// Retry-After. Parity with the retry behavior of the Python provider SDKs
// (OpenAI max_retries=4, Anthropic 2, Z.AI 3 — configured per provider).
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"ago/internal/sse"
)

// Client posts JSON bodies and reads JSON/SSE responses. The zero value is
// usable but performs no retries. Safe for concurrent use.
type Client struct {
	// HTTP performs the requests. When nil, PostJSON builds a one-shot client
	// with Timeout applied; PostJSONStream never applies a total timeout (a
	// stream may be long-lived; cancel via ctx).
	HTTP *http.Client
	// MaxRetries is the number of retries after the first attempt (0 = single try).
	MaxRetries int
	// Timeout caps a non-streaming request when HTTP is nil (0 = no cap).
	Timeout time.Duration
}

// NewClient returns a client with SDK-default retry behavior
// (4 retries — Python parity: OpenAI DEFAULT_MAX_RETRIES) and no timeout.
func NewClient() *Client {
	return &Client{MaxRetries: 4}
}

// retryBaseDelay and maxRetryDelay shape the exponential backoff. Vars so tests
// can shorten the waits (same seam as the middleware Retry builtin).
var (
	retryBaseDelay = 500 * time.Millisecond
	maxRetryDelay  = 8 * time.Second
	// maxRetryAfter caps a server-sent Retry-After so a broken server cannot
	// stall a turn indefinitely.
	maxRetryAfter = time.Minute
)

func backoff(attempt int) time.Duration {
	d := retryBaseDelay << attempt
	if d > maxRetryDelay || d <= 0 {
		return maxRetryDelay
	}
	return d
}

// retryAfter parses the Retry-After header (delay-seconds or HTTP-date).
func retryAfter(h http.Header) (time.Duration, bool) {
	v := h.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		d := time.Duration(secs * float64(time.Second))
		if d < 0 {
			d = 0
		}
		if d > maxRetryAfter {
			d = maxRetryAfter
		}
		return d, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		if d > maxRetryAfter {
			d = maxRetryAfter
		}
		return d, true
	}
	return 0, false
}

func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// sleepCtx waits d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// do performs one POST of payload. headers may be nil; Content-Type is set to
// application/json unless the caller overrides it, accept governs the Accept header.
func (c *Client) do(ctx context.Context, url string, headers map[string]string, payload []byte, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("httpx: bad request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: c.Timeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// PostJSON posts body as JSON and returns the status code and the raw response
// body of the final attempt. A completed HTTP response is never an error — the
// caller inspects the status; the error reports transport failure after retries
// or ctx cancellation.
func (c *Client) PostJSON(ctx context.Context, url string, headers map[string]string, body any) (int, []byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, nil, fmt.Errorf("httpx: encoding request body: %w", err)
	}
	for attempt := 0; ; attempt++ {
		resp, err := c.do(ctx, url, headers, payload, "application/json")
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return 0, nil, ctxErr
			}
			if attempt < c.MaxRetries {
				if sleepCtx(ctx, backoff(attempt)) == nil {
					continue
				}
				return 0, nil, ctx.Err()
			}
			return 0, nil, fmt.Errorf("httpx: request failed after %d attempt(s): %w", attempt+1, err)
		}
		data, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			if attempt < c.MaxRetries && ctx.Err() == nil && sleepCtx(ctx, backoff(attempt)) == nil {
				continue // body read interrupted: retry with a fresh attempt
			}
			return resp.StatusCode, nil, fmt.Errorf("httpx: reading response: %w", readErr)
		}
		if retryableStatus(resp.StatusCode) && attempt < c.MaxRetries {
			delay, ok := retryAfter(resp.Header)
			if !ok {
				delay = backoff(attempt)
			}
			if sleepCtx(ctx, delay) == nil {
				continue
			}
			return resp.StatusCode, data, ctx.Err()
		}
		return resp.StatusCode, data, nil
	}
}

// PostJSONStream posts body as JSON and reads the response as an SSE stream,
// passing every data chunk to onEvent (including a "data: [DONE]" sentinel —
// the caller filters it). Retries cover connection failures and 429/5xx before
// the stream opens; once events flow, a failure is terminal. A non-2xx final
// response becomes an error carrying the status and a body snippet. An error
// returned by onEvent stops the stream and is returned unwrapped.
func (c *Client) PostJSONStream(ctx context.Context, url string, headers map[string]string, body any, onEvent func(data []byte) error) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("httpx: encoding request body: %w", err)
	}
	for attempt := 0; ; attempt++ {
		resp, err := c.do(ctx, url, headers, payload, "text/event-stream")
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if attempt < c.MaxRetries {
				if sleepCtx(ctx, backoff(attempt)) == nil {
					continue
				}
				return ctx.Err()
			}
			return fmt.Errorf("httpx: stream request failed after %d attempt(s): %w", attempt+1, err)
		}
		if retryableStatus(resp.StatusCode) && attempt < c.MaxRetries {
			delay, ok := retryAfter(resp.Header)
			if !ok {
				delay = backoff(attempt)
			}
			drainAndClose(resp.Body)
			if sleepCtx(ctx, delay) == nil {
				continue
			}
			return ctx.Err()
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			return fmt.Errorf("httpx: stream request returned %s: %s", resp.Status, snippet(data))
		}

		// The stream is live: no retries past this point (events were delivered).
		sc := sse.NewScanner(resp.Body)
		for sc.Next() {
			_, data := sc.Event()
			trimmed := bytes.TrimSpace(data)
			if len(trimmed) == 0 {
				continue
			}
			if err := onEvent(trimmed); err != nil {
				resp.Body.Close()
				return err
			}
		}
		streamErr := sc.Err()
		resp.Body.Close()
		if streamErr != nil && !errors.Is(streamErr, io.EOF) {
			return fmt.Errorf("httpx: reading stream: %w", streamErr)
		}
		return nil
	}
}

func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 4096))
	body.Close()
}

// snippet renders the first bytes of an error body for an error message.
func snippet(b []byte) string {
	const max = 512
	s := string(bytes.TrimSpace(b))
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
