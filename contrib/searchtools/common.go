package searchtools

import (
	"bytes"
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
)

// DefaultTimeout bounds one provider request; the Python extensions default to
// timeout=60 seconds.
const DefaultTimeout = 60 * time.Second

// maxResponseBytes caps a buffered provider response (the repo-wide 8 MiB norm).
const maxResponseBytes = 8 << 20

// Sentinel errors; every provider error wraps one of them.
var (
	// ErrHTTPStatus reports a non-2xx answer from a provider; match with
	// errors.Is and read the *HTTPError for the status, body and message.
	ErrHTTPStatus = errors.New("searchtools: unexpected HTTP status")
	// ErrAPIResponse reports a 2xx answer the provider itself marked as failed.
	ErrAPIResponse = errors.New("searchtools: API reported failure")
	// ErrMissingKey reports a toolkit built without an API key and without a key
	// variable (Python parity: "api_key is required").
	ErrMissingKey = errors.New("searchtools: API key is not set")
	// ErrRejected reports a request the tool refused to send: an unsupported URL
	// scheme or a conflicting filter pair.
	ErrRejected = errors.New("searchtools: request rejected")
)

// HTTPError is a provider response outside the 2xx range. Message carries the
// provider's own error text when it supplies one; Body keeps the raw payload.
//
// AGO-DEVIATION: the Python extensions surface httpx.HTTPStatusError with its
// own message; this port raises one typed error per provider failure so callers
// can match errors.Is/As, with the status, the request and the body-attached
// provider message.
type HTTPError struct {
	Provider   string
	Method     string
	URL        string
	StatusCode int
	Body       string
	Message    string
	RetryAfter string
}

// Error renders the status plus the provider's own detail, so the model sees
// what went wrong instead of a bare status code.
func (e *HTTPError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "searchtools: %s: %s %s: HTTP %d", e.Provider, e.Method, e.URL, e.StatusCode)
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	} else if body := strings.TrimSpace(e.Body); body != "" {
		b.WriteString(": ")
		b.WriteString(body)
	}
	if e.RetryAfter != "" {
		fmt.Fprintf(&b, " (Retry-After: %s)", e.RetryAfter)
	}
	return b.String()
}

// Unwrap exposes ErrHTTPStatus for errors.Is.
func (e *HTTPError) Unwrap() error { return ErrHTTPStatus }

// httpRequest is one provider call. A nil body sends no payload; query and
// header may be nil.
type httpRequest struct {
	provider string
	client   *http.Client
	method   string
	url      string
	query    url.Values
	header   map[string]string
	body     any
	// timeout bounds each attempt when client is nil; zero means DefaultTimeout.
	timeout time.Duration
	// retry is the provider SDK's retry policy; the zero value sends one attempt.
	retry retryPolicy
}

// retryPolicy mirrors the retry loop a provider SDK wraps around its HTTP calls.
type retryPolicy struct {
	// attempts is the total number of tries (the first one included); values
	// below one mean a single try.
	attempts int
	// status reports whether an HTTP status is worth another try; nil retries none.
	status func(code int) bool
	// transport retries connection, timeout and body-read failures.
	transport bool
	// backoff is the wait after the failed-th try (1-based).
	backoff func(failed int) time.Duration
}

// doublingBackoff is 0.5 s * 2^(failed-1), never above limit: the wait both
// SDKs derive from their attempt number.
func doublingBackoff(failed int, limit time.Duration) time.Duration {
	// The shift is bounded so a large attempt number cannot overflow a Duration.
	return min(500*time.Millisecond<<min(failed-1, 20), limit)
}

// defaultHTTPClient sends requests of a request without its own client; tests
// replace it to observe the effective per-attempt deadline.
var defaultHTTPClient = http.DefaultClient

// sleep waits for d or until ctx ends (sleepContext); tests replace it to run
// backoff instantly.
var sleep = sleepContext

// do sends the request and returns the response body, retrying as the request's
// policy allows. A non-2xx status becomes an *HTTPError; transport and read
// failures wrap their cause. The last failure is returned once the attempts run
// out, and a cancelled ctx ends the call without further tries.
func (r httpRequest) do(ctx stdctx.Context) ([]byte, error) {
	endpoint := r.url
	if len(r.query) > 0 {
		endpoint += "?" + r.query.Encode()
	}
	var payload []byte
	if r.body != nil {
		b, err := json.Marshal(r.body)
		if err != nil {
			return nil, fmt.Errorf("searchtools: %s: encode request: %w", r.provider, err)
		}
		payload = b
	}
	if ctx == nil {
		ctx = stdctx.Background()
	}
	for failed := 1; ; failed++ {
		data, retryable, err := r.attempt(ctx, endpoint, payload)
		if err == nil {
			return data, nil
		}
		if !retryable || failed >= r.retry.attempts {
			return nil, err
		}
		if serr := sleep(ctx, r.retry.backoff(failed)); serr != nil {
			return nil, fmt.Errorf("searchtools: %s: request failed: %w", r.provider, serr)
		}
	}
}

// attempt sends the request once. retryable says whether the failure is one the
// retry policy covers.
func (r httpRequest) attempt(ctx stdctx.Context, endpoint string, payload []byte) (data []byte, retryable bool, err error) {
	client := r.client
	if client == nil {
		client = defaultHTTPClient
		limit := r.timeout
		if limit <= 0 {
			limit = DefaultTimeout
		}
		// AGO-DEVIATION: the SDKs retry an httpx timeout like any transport error
		// (up to 3 tries); here ctx is the per-attempt deadline context, so a timed
		// out attempt reads as a finished context and is not retried below. One call
		// is thus bounded by a single timeout instead of up to 3 x 600 s plus backoff.
		var cancel stdctx.CancelFunc
		ctx, cancel = stdctx.WithTimeout(ctx, limit)
		defer cancel()
	}
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, endpoint, reader)
	if err != nil {
		return nil, false, fmt.Errorf("searchtools: %s: build request: %w", r.provider, err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range r.header {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, r.retry.transport && ctx.Err() == nil,
			fmt.Errorf("searchtools: %s: request failed: %w", r.provider, err)
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, r.retry.transport && ctx.Err() == nil,
			fmt.Errorf("searchtools: %s: read response: %w", r.provider, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, r.retry.status != nil && r.retry.status(resp.StatusCode), &HTTPError{
			Provider:   r.provider,
			Method:     r.method,
			URL:        endpoint,
			StatusCode: resp.StatusCode,
			Body:       string(data),
			RetryAfter: resp.Header.Get("Retry-After"),
		}
	}
	return data, false, nil
}

// decodeJSON unmarshals a response body into out.
func decodeJSON(provider string, data []byte, out any) error {
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("searchtools: %s: decode response: %w", provider, err)
	}
	return nil
}

// callContext is the cancellation scope of one tool call. The framework stamps
// the turn context on the conversation context; a bare context falls back to
// Background.
func callContext(cx *agocontext.Context) stdctx.Context {
	if cx != nil {
		if tc := cx.TurnContext(); tc != nil {
			return tc
		}
	}
	return stdctx.Background()
}

// paramSet holds the per-tool option values: a fixed value per parameter name,
// or the name of a conversation variable resolved at call time (Python parity:
// the `Variable` kwargs of the extension factories).
type paramSet struct {
	fixed map[string]any
	vars  map[string]string
}

func newParamSet() *paramSet {
	return &paramSet{fixed: map[string]any{}, vars: map[string]string{}}
}

// clone copies the set so a factory can apply per-tool overrides without
// touching the toolkit defaults.
func (p *paramSet) clone() *paramSet {
	c := newParamSet()
	for k, v := range p.fixed {
		c.fixed[k] = v
	}
	for k, v := range p.vars {
		c.vars[k] = v
	}
	return c
}

// set fixes a parameter value.
func (p *paramSet) set(name string, v any) { p.fixed[name] = v }

// bind makes a parameter resolve from the named conversation variable.
func (p *paramSet) bind(name, variable string) {
	p.vars[name] = variable
	delete(p.fixed, name)
}

// get resolves one parameter. ok is false when neither a fixed value nor a
// variable is configured. A missing conversation variable wraps
// tools.ErrNotFound, matching the built-in search toolkits.
func (p *paramSet) get(cx *agocontext.Context, name string) (any, bool, error) {
	if variable, ok := p.vars[name]; ok {
		v, ok := cx.Variable(variable)
		if !ok {
			return nil, false, fmt.Errorf("%w: variable %q is not provided", tools.ErrNotFound, variable)
		}
		return v, true, nil
	}
	v, ok := p.fixed[name]
	return v, ok, nil
}

// intParam resolves an integer parameter.
func (p *paramSet) intParam(cx *agocontext.Context, name string) (int, bool, error) {
	v, ok, err := p.get(cx, name)
	if err != nil || !ok {
		return 0, false, err
	}
	n, ok := v.(int)
	if !ok {
		return 0, false, fmt.Errorf("searchtools: parameter %q must be an int, got %T", name, v)
	}
	return n, true, nil
}

// stringParam resolves a string parameter.
func (p *paramSet) stringParam(cx *agocontext.Context, name string) (string, bool, error) {
	v, ok, err := p.get(cx, name)
	if err != nil || !ok {
		return "", false, err
	}
	s, ok := v.(string)
	if !ok {
		return "", false, fmt.Errorf("searchtools: parameter %q must be a string, got %T", name, v)
	}
	return s, true, nil
}

// boolParam resolves a boolean parameter.
func (p *paramSet) boolParam(cx *agocontext.Context, name string) (bool, bool, error) {
	v, ok, err := p.get(cx, name)
	if err != nil || !ok {
		return false, false, err
	}
	b, ok := v.(bool)
	if !ok {
		return false, false, fmt.Errorf("searchtools: parameter %q must be a bool, got %T", name, v)
	}
	return b, true, nil
}

// stringsParam resolves a string-list parameter.
func (p *paramSet) stringsParam(cx *agocontext.Context, name string) ([]string, bool, error) {
	v, ok, err := p.get(cx, name)
	if err != nil || !ok {
		return nil, false, err
	}
	s, ok := v.([]string)
	if !ok {
		return nil, false, fmt.Errorf("searchtools: parameter %q must be []string, got %T", name, v)
	}
	return s, true, nil
}

// toolbox is the shared registration surface of every toolkit: the compiled
// toolkit and the middleware it carries.
type toolbox struct {
	tk *tools.Toolkit
}

// Name returns the toolkit name.
func (b *toolbox) Name() string { return b.tk.Name() }

// DeclaredInCode reports a program-declared tool (framework contract).
func (b *toolbox) DeclaredInCode() bool { return b.tk.DeclaredInCode() }

// Tools returns the members in toolkit order.
func (b *toolbox) Tools() []tools.Tool { return b.tk.Tools() }

// Schemas concatenates the members' schemas.
func (b *toolbox) Schemas(cx *agocontext.Context) ([]any, error) { return b.tk.Schemas(cx) }

// Register subscribes every member on the conversation stream.
func (b *toolbox) Register(cx *agocontext.Context, mw []middleware.Middleware) ([]stream.SubID, error) {
	return b.tk.Register(cx, mw)
}

// truncate cuts text to max characters (max < 0 means no cut) and reports
// whether it was cut. A character is one Unicode code point, the unit Python's
// text[:max] slices by; byte slicing would cut a multibyte rune in half.
func truncate(text string, max int) (string, bool) {
	if max < 0 || runeLen(text) <= max {
		return text, false
	}
	runes := []rune(text)
	return string(runes[:max]), true
}

// truncateLinks keeps leading links while their combined character length fits
// max; a negative max keeps everything (Python parity: firecrawl
// _truncate_links, which sums len() — characters, not bytes).
func truncateLinks(links []string, max int) ([]string, bool) {
	if len(links) == 0 {
		return []string{}, false
	}
	if max < 0 {
		return append([]string{}, links...), false
	}
	kept := []string{}
	used := 0
	for _, link := range links {
		used += runeLen(link)
		if used > max {
			return kept, true
		}
		kept = append(kept, link)
	}
	return kept, false
}

// runeLen counts Unicode code points, the unit the Python extensions cut and sum
// text by.
func runeLen(s string) int { return utf8.RuneCountInString(s) }

// pythonListRepr renders a string slice the way Python's repr does, so a
// rejection message reads the same in both ports.
func pythonListRepr(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = "'" + strings.ReplaceAll(item, "'", `\'`) + "'"
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// errMissingKeyEnv reports a credential absent from both the constructor and the
// provider's environment variable.
func errMissingKeyEnv(envVar string) error {
	if envVar == "" {
		return ErrMissingKey
	}
	return fmt.Errorf("%w: pass a key or set %s", ErrMissingKey, envVar)
}

// errVariableMissing reports a conversation variable an option bound but the
// conversation does not carry.
func errVariableMissing(name string) error {
	return fmt.Errorf("%w: variable %q is not provided", tools.ErrNotFound, name)
}

// errVariableType reports a conversation variable of the wrong Go type.
func errVariableType(name string, v any) error {
	return fmt.Errorf("searchtools: variable %q must be a string, got %T", name, v)
}

// errEmptyOption reports an option that needs a non-empty value.
func errEmptyOption(provider, name string) error {
	return fmt.Errorf("searchtools: %s: %s must not be empty", provider, name)
}

// errNilOption reports an option that needs a non-nil value.
func errNilOption(provider, name string) error {
	return fmt.Errorf("searchtools: %s: %s must not be nil", provider, name)
}

// errOptionValue reports an option value outside its accepted range.
func errOptionValue(provider, name, reason string) error {
	return fmt.Errorf("searchtools: %s: %s %s", provider, name, reason)
}

// checkMiddleware rejects nil middleware entries before they reach a tool.
func checkMiddleware(provider string, mw []middleware.Middleware) error {
	for _, m := range mw {
		if m == nil {
			return fmt.Errorf("searchtools: %s: middleware must not be nil", provider)
		}
	}
	return nil
}
