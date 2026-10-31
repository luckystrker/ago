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
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/luckystrker/ago/internal/sse"
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
	// MaxResponseBytes caps the buffered response body read by PostJSON
	// (0 → DefaultMaxResponseBytes, negative → no cap). A hostile or broken
	// endpoint must not be able to exhaust memory through an endless body.
	// Streaming responses are consumed incrementally and are not affected.
	MaxResponseBytes int64
	// Redirect selects the redirect policy for the one-shot client built when
	// HTTP is nil. A caller-supplied HTTP client keeps its own CheckRedirect —
	// attach Redirect.CheckRedirect() to it for the same behavior.
	Redirect RedirectPolicy
}

// NewClient returns a client with SDK-default retry behavior
// (4 retries — Python parity: OpenAI DEFAULT_MAX_RETRIES), the default
// response-body cap and the credential-stripping redirect policy.
func NewClient() *Client {
	return &Client{MaxRetries: 4}
}

// DefaultMaxResponseBytes caps a buffered (non-streaming) response body read
// by this package when the client sets no limit: 8 MiB, the repo-wide norm
// for HTTP payloads. Streaming bodies are consumed incrementally by the SSE
// scanner and are not capped here.
const DefaultMaxResponseBytes int64 = 8 << 20

// ErrResponseTooLarge reports a buffered response body over the configured
// MaxResponseBytes; match with errors.Is.
var ErrResponseTooLarge = errors.New("httpx: response body exceeds the size limit")

// RedirectPolicy selects how requests follow HTTP redirects across host
// boundaries. Provider credentials travel in custom headers (Authorization,
// x-api-key, x-goog-api-key, arbitrary per-config headers), and the Go
// stdlib strips only Authorization, Www-Authenticate, Cookie and Cookie2 on
// a host change — the rest would reach whatever host a redirect names.
//
// AGO-DEVIATION: Python provider SDKs sit on httpx, which does not follow
// redirects by default; the Go stdlib does. AGo keeps following redirects
// (gateways legitimately bounce requests, e.g. http→https on the same host)
// but strips credentials at the host boundary and offers a strict no-cross-
// host mode.
type RedirectPolicy int

const (
	// RedirectStripCredentials follows cross-host redirects but drops every
	// request header outside safelistedHeaders on the hop, so credentials
	// cannot reach a different host: an endpoint answering 307 → its own
	// domain must not receive the API key.
	RedirectStripCredentials RedirectPolicy = iota
	// RedirectSameHost blocks any redirect whose target host name or port
	// differs from the original request's; the request fails instead.
	RedirectSameHost
)

// AGO-DEVIATION: Python provider SDKs sit on httpx, which does not follow
// redirects at all by default; the Go stdlib does. AGo keeps following
// redirects (gateways legitimately bounce requests, e.g. http→https) but
// strips credentials at the host boundary and keeps an explicit strict mode.
var _ = struct{}{}

// safelistedHeaders may cross a host boundary: they carry no credentials.
// Mirrors the Fetch spec's CORS-safelisted request headers plus the fields
// the transport itself manages; Content-Type must survive so a 307/308
// re-POST stays parseable on the receiving side.
var safelistedHeaders = map[string]bool{
	"Accept":           true,
	"Accept-Encoding":  true,
	"Accept-Language":  true,
	"Content-Language": true,
	"Content-Length":   true,
	"Content-Type":     true,
	"Range":            true,
	"User-Agent":       true,
}

// maxRedirectHops bounds the redirect chain. A custom CheckRedirect replaces
// the stdlib default checker, including its built-in stop, so the policy must
// enforce its own.
const maxRedirectHops = 10

// CheckRedirect returns the policy as an http.Client CheckRedirect function;
// assign it to a custom client for the same behavior.
func (p RedirectPolicy) CheckRedirect() func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) == 0 {
			return nil
		}
		if len(via) >= maxRedirectHops {
			return fmt.Errorf("httpx: stopped after %d redirects", maxRedirectHops)
		}
		if sameAuthority(via[0].URL, req.URL) {
			return nil
		}
		if p == RedirectSameHost {
			return fmt.Errorf("httpx: redirect from %s to %s changes the host", via[0].URL.Host, req.URL.Host)
		}
		for k := range req.Header {
			if !safelistedHeaders[http.CanonicalHeaderKey(k)] {
				req.Header.Del(k)
			}
		}
		return nil
	}
}

// sameAuthority reports whether two URLs target the same host name and port;
// implicit ports resolve by scheme, so a scheme upgrade on the same host
// (http→https) is not a host change.
func sameAuthority(a, b *url.URL) bool {
	return strings.EqualFold(a.Hostname(), b.Hostname()) && effectivePort(a) == effectivePort(b)
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch u.Scheme {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
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
// application/json unless the caller overrides it, accept governs the Accept
// header. stream must reflect the caller's path: a default client carries
// Timeout only for non-streaming requests — a stream may be long-lived and
// http.Client.Timeout would kill the whole body read (cancel via ctx).
func (c *Client) do(ctx context.Context, url string, headers map[string]string, payload []byte, accept string, stream bool) (*http.Response, error) {
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
		client = &http.Client{CheckRedirect: c.Redirect.CheckRedirect()}
		if !stream {
			client.Timeout = c.Timeout
		}
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
		resp, err := c.do(ctx, url, headers, payload, "application/json", false)
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
		data, readErr := c.readBody(resp.Body)
		resp.Body.Close()
		if errors.Is(readErr, ErrResponseTooLarge) {
			// A completed answer over the cap is a fact about the server, not
			// a transient failure — retrying cannot shrink it.
			return resp.StatusCode, nil, readErr
		}
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
		resp, err := c.do(ctx, url, headers, payload, "text/event-stream", true)
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

// readBody reads a buffered response body up to the client's cap; a body over
// the cap fails with ErrResponseTooLarge instead of exhausting memory.
func (c *Client) readBody(r io.Reader) ([]byte, error) {
	limit := c.MaxResponseBytes
	if limit == 0 {
		limit = DefaultMaxResponseBytes
	}
	if limit < 0 {
		return io.ReadAll(r)
	}
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, ErrResponseTooLarge
	}
	return data, nil
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
