package searchtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/tools"
)

const (
	// keenableEnvKey is the environment variable that only raises the keyless
	// rate limits (Python parity: KEENABLE_API_KEY).
	keenableEnvKey = "KEENABLE_API_KEY"
	// keenableAppTitle attributes traffic; Keenable rejects keyless calls
	// without it (Python parity: _APP_TITLE).
	keenableAppTitle = "ag2"
	// keenableDefaultSnippet caps each result's snippet: Keenable returns 5-8k
	// characters of page text per result when left unset.
	keenableDefaultSnippet = 1000
)

// KeenableSearchResult is one hit (Python parity:
// keenable.KeenableSearchResult).
type KeenableSearchResult struct {
	Title      string `json:"title"`
	URL        string `json:"url"`
	Snippet    string `json:"snippet"`
	AcquiredAt string `json:"acquired_at"`
}

// KeenableSearchResponse is the search tool result (Python parity:
// keenable.KeenableSearchResponse).
type KeenableSearchResponse struct {
	Query   string                 `json:"query"`
	Results []KeenableSearchResult `json:"results"`
}

// KeenableFetchResult is one fetched page (Python parity:
// keenable.KeenableFetchResult).
type KeenableFetchResult struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	Content     string `json:"content"`
	Author      string `json:"author"`
	PublishedAt string `json:"published_at"`
}

// KeenableSearchToolkit exposes Keenable web search and page fetch as agent
// tools (Python parity: ag2/extensions/tools/search/keenable.py
// KeenableSearchToolkit).
//
// No key is needed: without one the keyless /public endpoints are called with
// lower rate limits; KEENABLE_API_KEY only raises them. A
// KeenableSearchToolkit is immutable after construction and safe for concurrent
// use.
type KeenableSearchToolkit struct {
	toolbox
	key     apiCredential
	baseURL string
	client  *http.Client
	mw      []middleware.Middleware
	params  *paramSet
}

var _ tools.Tool = (*KeenableSearchToolkit)(nil)

// KeenableOption configures NewKeenableSearchToolkit.
type KeenableOption func(*KeenableSearchToolkit) error

// WithKeenableBaseURL overrides the API origin (default
// https://api.keenable.ai).
func WithKeenableBaseURL(base string) KeenableOption {
	return func(t *KeenableSearchToolkit) error {
		if base == "" {
			return errEmptyOption("keenable", "base URL")
		}
		t.baseURL = strings.TrimRight(base, "/")
		return nil
	}
}

// WithKeenableHTTPClient overrides the HTTP client.
func WithKeenableHTTPClient(c *http.Client) KeenableOption {
	return func(t *KeenableSearchToolkit) error {
		if c == nil {
			return errNilOption("keenable", "http client")
		}
		t.client = c
		return nil
	}
}

// WithKeenableAPIKeyVariable reads the key from the named conversation variable
// instead of the constructor value.
func WithKeenableAPIKeyVariable(name string) KeenableOption {
	return func(t *KeenableSearchToolkit) error {
		if name == "" {
			return errEmptyOption("keenable", "key variable")
		}
		t.key.variable = name
		return nil
	}
}

// WithKeenableMiddleware adds tool-scoped middleware to both members.
func WithKeenableMiddleware(mw ...middleware.Middleware) KeenableOption {
	return func(t *KeenableSearchToolkit) error {
		if err := checkMiddleware("keenable", mw); err != nil {
			return err
		}
		t.mw = append(t.mw, mw...)
		return nil
	}
}

// WithKeenableMaxResults sets the default result count (Python parity:
// max_results, 1-50, API default 10).
func WithKeenableMaxResults(n int) KeenableOption {
	return func(t *KeenableSearchToolkit) error {
		if n < 1 || n > 50 {
			return errOptionValue("keenable", "max_results", "must be between 1 and 50")
		}
		t.params.set("max_results", n)
		return nil
	}
}

// WithKeenableSite restricts results to one domain (Python parity: site).
func WithKeenableSite(domain string) KeenableOption {
	return func(t *KeenableSearchToolkit) error {
		t.params.set("site", domain)
		return nil
	}
}

// WithKeenablePublishedAfter keeps pages published after a YYYY-MM-DD date
// (Python parity: published_after).
func WithKeenablePublishedAfter(date string) KeenableOption {
	return func(t *KeenableSearchToolkit) error {
		t.params.set("published_after", date)
		return nil
	}
}

// WithKeenableSnippetMaxLength sets the approximate snippet length in
// characters (Python parity: snippet_max_length, default 1000).
func WithKeenableSnippetMaxLength(n int) KeenableOption {
	return func(t *KeenableSearchToolkit) error {
		if n <= 0 {
			return errOptionValue("keenable", "snippet_max_length", "must be > 0")
		}
		t.params.set("snippet_max_length", n)
		return nil
	}
}

// WithKeenableVariable binds a default to a conversation variable.
func WithKeenableVariable(param, variable string) KeenableOption {
	return func(t *KeenableSearchToolkit) error {
		if param == "" || variable == "" {
			return errOptionValue("keenable", "variable", "needs both a parameter and a variable name")
		}
		t.params.bind(param, variable)
		return nil
	}
}

// NewKeenableSearchToolkit builds the keenable_search_toolkit.
func NewKeenableSearchToolkit(apiKey string, opts ...KeenableOption) (*KeenableSearchToolkit, error) {
	t := &KeenableSearchToolkit{
		key:     apiCredential{key: apiKey, envVar: keenableEnvKey},
		baseURL: "https://api.keenable.ai",
		params:  newParamSet(),
	}
	for _, opt := range opts {
		if err := opt(t); err != nil {
			return nil, err
		}
	}
	search, err := t.Search()
	if err != nil {
		return nil, err
	}
	fetch, err := t.Fetch()
	if err != nil {
		return nil, err
	}
	tk, err := tools.NewToolkit("keenable_search_toolkit", nil, search, fetch)
	if err != nil {
		return nil, err
	}
	t.tk = tk
	return t, nil
}

// clone copies the toolkit with per-factory overrides applied.
func (t *KeenableSearchToolkit) clone(opts ...KeenableOption) (*KeenableSearchToolkit, error) {
	c := *t
	c.params = t.params.clone()
	c.mw = append([]middleware.Middleware{}, t.mw...)
	for _, opt := range opts {
		if err := opt(&c); err != nil {
			return nil, err
		}
	}
	return &c, nil
}

// Search builds the keenable_search tool (Python parity:
// KeenableSearchToolkit.search).
func (t *KeenableSearchToolkit) Search(opts ...KeenableOption) (*tools.FunctionTool, error) {
	c, err := t.clone(opts...)
	if err != nil {
		return nil, err
	}
	type keenableArgs struct {
		Query string `json:"query" agschema:"description=The search query string.,required"`
	}
	fn := func(args keenableArgs, cx *agocontext.Context) (KeenableSearchResponse, error) {
		return c.search(callContext(cx), cx, args.Query)
	}
	return tools.NewFunctionTool("keenable_search",
		"Search the web using Keenable. Returns ranked results with titles, URLs, and a snippet of each page's text.",
		fn, tools.WithMiddleware(c.mw...))
}

// Fetch builds the keenable_fetch tool (Python parity:
// KeenableSearchToolkit.fetch).
func (t *KeenableSearchToolkit) Fetch(opts ...KeenableOption) (*tools.FunctionTool, error) {
	c, err := t.clone(opts...)
	if err != nil {
		return nil, err
	}
	type keenableArgs struct {
		URL string `json:"url" agschema:"description=The http or https URL of the page to fetch.,required"`
	}
	fn := func(args keenableArgs, cx *agocontext.Context) (KeenableFetchResult, error) {
		return c.fetch(callContext(cx), cx, args.URL)
	}
	return tools.NewFunctionTool("keenable_fetch",
		"Fetch a web page using Keenable. Returns the page's main content as markdown, "+
			"with its title, author, and publication date when known.",
		fn, tools.WithMiddleware(c.mw...))
}

// keyOrEmpty returns the key for one call. Unlike the other providers a missing
// key is not an error: Keenable's keyless endpoints answer at lower rate limits.
func (t *KeenableSearchToolkit) keyOrEmpty(cx *agocontext.Context) (string, error) {
	key, err := t.key.resolve(cx)
	if err == nil {
		return key, nil
	}
	if errors.Is(err, ErrMissingKey) {
		return "", nil
	}
	return "", err
}

// request runs one Keenable request, appending /public to keyless calls and
// carrying the provider's own error message (Python parity: keenable._request).
func (t *KeenableSearchToolkit) request(ctx context.Context, cx *agocontext.Context, method, path string, params url.Values, body map[string]any) (map[string]any, error) {
	key, err := t.keyOrEmpty(cx)
	if err != nil {
		return nil, err
	}
	header := map[string]string{"X-Keenable-Title": keenableAppTitle}
	if key == "" {
		path += "/public"
	} else {
		header["X-API-Key"] = key
	}
	data, err := httpRequest{
		provider: "keenable",
		client:   t.client,
		method:   method,
		url:      t.baseURL + path,
		query:    params,
		header:   header,
		body:     body,
	}.do(ctx)
	if err != nil {
		return nil, keenableError(err, key == "")
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		// Python parity: a payload that is not an object decodes to {}.
		return map[string]any{}, nil
	}
	return raw, nil
}

// keenableError folds Keenable's own error message and the keyless hint into
// the typed HTTP error.
func keenableError(err error, keyless bool) error {
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		return err
	}
	var payload map[string]any
	if json.Unmarshal([]byte(httpErr.Body), &payload) == nil {
		httpErr.Message, _ = payload["message"].(string)
	}
	if httpErr.StatusCode == http.StatusTooManyRequests && keyless {
		hint := fmt.Sprintf("Set %s to raise the keyless rate limit.", keenableEnvKey)
		if httpErr.Message == "" {
			httpErr.Message = hint
		} else {
			httpErr.Message += " " + hint
		}
	}
	return httpErr
}

// search runs one /v1/search request.
func (t *KeenableSearchToolkit) search(ctx context.Context, cx *agocontext.Context, query string) (KeenableSearchResponse, error) {
	body := map[string]any{"query": query}
	snippet, ok, err := t.params.intParam(cx, "snippet_max_length")
	if err != nil {
		return KeenableSearchResponse{}, err
	}
	if !ok {
		// Python parity: the 1000-character cap is always sent unless a variable
		// overrides it.
		snippet = keenableDefaultSnippet
	}
	body["snippet_max_length"] = snippet
	for _, name := range []string{"max_results", "site", "published_after"} {
		v, ok, err := t.params.get(cx, name)
		if err != nil {
			return KeenableSearchResponse{}, err
		}
		if ok {
			body[name] = v
		}
	}
	raw, err := t.request(ctx, cx, http.MethodPost, "/v1/search", nil, body)
	if err != nil {
		return KeenableSearchResponse{}, err
	}
	out := KeenableSearchResponse{Query: query, Results: []KeenableSearchResult{}}
	for _, item := range rawItems(raw, "results") {
		snippet := rawText(item, "snippet")
		if snippet == "" {
			snippet = rawText(item, "description")
		}
		out.Results = append(out.Results, KeenableSearchResult{
			Title:      rawText(item, "title"),
			URL:        rawText(item, "url"),
			Snippet:    snippet,
			AcquiredAt: rawText(item, "acquired_at"),
		})
	}
	return out, nil
}

// fetch runs one /v1/fetch request.
func (t *KeenableSearchToolkit) fetch(ctx context.Context, cx *agocontext.Context, target string) (KeenableFetchResult, error) {
	raw, err := t.request(ctx, cx, http.MethodGet, "/v1/fetch", url.Values{"url": {target}}, nil)
	if err != nil {
		return KeenableFetchResult{}, err
	}
	out := KeenableFetchResult{
		URL:         rawText(raw, "url"),
		Title:       rawText(raw, "title"),
		Content:     rawText(raw, "content"),
		Author:      rawText(raw, "author"),
		PublishedAt: keenableTimestamp(raw["published_at"]),
	}
	if out.URL == "" {
		out.URL = target
	}
	return out, nil
}

// rawItems returns the mappings inside the list at raw[key], skipping anything
// else (Python parity: keenable._items).
func rawItems(raw map[string]any, key string) []map[string]any {
	items, ok := raw[key].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// keenableTimestamp renders a Unix timestamp in seconds as an ISO 8601 UTC
// string and passes strings through (Python parity: keenable._timestamp, which
// renders with datetime.isoformat's "+00:00" offset).
func keenableTimestamp(v any) string {
	switch value := v.(type) {
	case string:
		return value
	case float64:
		return time.Unix(int64(value), 0).UTC().Format("2006-01-02T15:04:05+00:00")
	case int:
		return time.Unix(int64(value), 0).UTC().Format("2006-01-02T15:04:05+00:00")
	default:
		return ""
	}
}
