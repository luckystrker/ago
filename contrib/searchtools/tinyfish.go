package searchtools

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/tools"
)

const (
	// tinyfishSearchHost is the Search product host; the API is served at the
	// root path (Python parity: tinyfish PRODUCT_BASE_URLS["search"]).
	tinyfishSearchHost = "https://api.search.tinyfish.ai/"
	// tinyfishFetchHost is the Fetch product host.
	tinyfishFetchHost = "https://api.fetch.tinyfish.ai/"
	// tinyfishIntegration is the api_integration tag the Python extension injects
	// into the fetch body (TF_API_INTEGRATION).
	tinyfishIntegration = "ag2"
	// tinyfishMaxURLs bounds one fetch call (Python parity: the SDK's 1-10 range).
	tinyfishMaxURLs = 10
)

// TinyFishSearchResult is one hit (Python parity:
// tinyfish.TinyFishSearchResult).
type TinyFishSearchResult struct {
	Position int    `json:"position"`
	SiteName string `json:"site_name"`
	Title    string `json:"title"`
	Snippet  string `json:"snippet"`
	URL      string `json:"url"`
}

// TinyFishSearchResponse is the search tool result (Python parity:
// tinyfish.TinyFishSearchResponse).
type TinyFishSearchResponse struct {
	Query        string                 `json:"query"`
	Results      []TinyFishSearchResult `json:"results"`
	TotalResults int                    `json:"total_results"`
}

// TinyFishFetchResult is one fetched page (Python parity:
// tinyfish.TinyFishFetchResult).
type TinyFishFetchResult struct {
	URL      string  `json:"url"`
	Format   string  `json:"format"`
	FinalURL *string `json:"final_url,omitempty"`
	Title    *string `json:"title,omitempty"`
	// AGO-DEVIATION: absent optional values omit their wire key (omitempty)
	// where Python's dataclass emits an explicit null.
	Description   *string `json:"description,omitempty"`
	Language      *string `json:"language,omitempty"`
	Author        *string `json:"author,omitempty"`
	PublishedDate *string `json:"published_date,omitempty"`
	// Text carries the page content in the requested format: a string for
	// markdown and html, an object for json.
	Text       any      `json:"text,omitempty"`
	Links      []string `json:"links"`
	ImageLinks []string `json:"image_links"`
}

// TinyFishFetchError is one URL the provider could not fetch (Python parity:
// tinyfish.TinyFishFetchError).
type TinyFishFetchError struct {
	URL   string `json:"url"`
	Error string `json:"error"`
}

// TinyFishFetchResponse is the fetch tool result (Python parity:
// tinyfish.TinyFishFetchResponse).
type TinyFishFetchResponse struct {
	Results []TinyFishFetchResult `json:"results"`
	Errors  []TinyFishFetchError  `json:"errors"`
}

// TinyFishSearchToolkit exposes TinyFish Search and Fetch as agent tools (Python
// parity: ag2/extensions/tools/search/tinyfish.py TinyFishSearchToolkit).
//
// The key comes from the constructor, TINYFISH_API_KEY, or a conversation
// variable. A TinyFishSearchToolkit is immutable after construction and safe for
// concurrent use.
type TinyFishSearchToolkit struct {
	toolbox
	key     apiCredential
	baseURL string // explicit base URL: both products resolve to /v1/<product>
	client  *http.Client
	mw      []middleware.Middleware
	params  *paramSet
}

var _ tools.Tool = (*TinyFishSearchToolkit)(nil)

// TinyFishOption configures NewTinyFishSearchToolkit.
type TinyFishOption func(*TinyFishSearchToolkit) error

// WithTinyFishBaseURL points both products at one origin (Python parity: the SDK
// base_url kwarg, which routes Search to /v1/search and Fetch to /v1/fetch) —
// the test seam for fake servers.
func WithTinyFishBaseURL(base string) TinyFishOption {
	return func(t *TinyFishSearchToolkit) error {
		if base == "" {
			return errEmptyOption("tinyfish", "base URL")
		}
		t.baseURL = strings.TrimRight(base, "/")
		return nil
	}
}

// WithTinyFishHTTPClient overrides the HTTP client.
func WithTinyFishHTTPClient(c *http.Client) TinyFishOption {
	return func(t *TinyFishSearchToolkit) error {
		if c == nil {
			return errNilOption("tinyfish", "http client")
		}
		t.client = c
		return nil
	}
}

// WithTinyFishAPIKeyVariable reads the key from the named conversation variable.
func WithTinyFishAPIKeyVariable(name string) TinyFishOption {
	return func(t *TinyFishSearchToolkit) error {
		if name == "" {
			return errEmptyOption("tinyfish", "key variable")
		}
		t.key.variable = name
		return nil
	}
}

// WithTinyFishMiddleware adds tool-scoped middleware to both members.
func WithTinyFishMiddleware(mw ...middleware.Middleware) TinyFishOption {
	return func(t *TinyFishSearchToolkit) error {
		if err := checkMiddleware("tinyfish", mw); err != nil {
			return err
		}
		t.mw = append(t.mw, mw...)
		return nil
	}
}

// WithTinyFishSearchDefaults sets the default location and language hints
// (Python parity: location / language).
func WithTinyFishSearchDefaults(location, language string) TinyFishOption {
	return func(t *TinyFishSearchToolkit) error {
		if location != "" {
			t.params.set("location", location)
		}
		if language != "" {
			t.params.set("language", language)
		}
		return nil
	}
}

// WithTinyFishFetchDefaults sets the default fetch options (Python parity:
// format, links, image_links).
func WithTinyFishFetchDefaults(format string, links, imageLinks bool) TinyFishOption {
	return func(t *TinyFishSearchToolkit) error {
		if format != "" {
			switch format {
			case "markdown", "html", "json":
			default:
				return errOptionValue("tinyfish", "format", `must be "markdown", "html" or "json"`)
			}
			t.params.set("format", format)
		}
		t.params.set("links", links)
		t.params.set("image_links", imageLinks)
		return nil
	}
}

// WithTinyFishVariable binds a default to a conversation variable.
func WithTinyFishVariable(param, variable string) TinyFishOption {
	return func(t *TinyFishSearchToolkit) error {
		if param == "" || variable == "" {
			return errOptionValue("tinyfish", "variable", "needs both a parameter and a variable name")
		}
		t.params.bind(param, variable)
		return nil
	}
}

// NewTinyFishSearchToolkit builds the tinyfish_search_toolkit. An empty apiKey
// is accepted: the key is then read from TINYFISH_API_KEY or the key variable.
func NewTinyFishSearchToolkit(apiKey string, opts ...TinyFishOption) (*TinyFishSearchToolkit, error) {
	t := &TinyFishSearchToolkit{
		key:    apiCredential{key: apiKey, envVar: "TINYFISH_API_KEY"},
		params: newParamSet(),
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
	tk, err := tools.NewToolkit("tinyfish_search_toolkit", nil, search, fetch)
	if err != nil {
		return nil, err
	}
	t.tk = tk
	return t, nil
}

// clone copies the toolkit with per-factory overrides applied.
func (t *TinyFishSearchToolkit) clone(opts ...TinyFishOption) (*TinyFishSearchToolkit, error) {
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

// Search builds the tinyfish_search tool (Python parity:
// TinyFishSearchToolkit.search).
func (t *TinyFishSearchToolkit) Search(opts ...TinyFishOption) (*tools.FunctionTool, error) {
	c, err := t.clone(opts...)
	if err != nil {
		return nil, err
	}
	type tinyfishArgs struct {
		Query string `json:"query" agschema:"description=The search query string.,required"`
	}
	fn := func(args tinyfishArgs, cx *agocontext.Context) (TinyFishSearchResponse, error) {
		return c.search(callContext(cx), cx, args.Query)
	}
	return tools.NewFunctionTool("tinyfish_search",
		"Search the web using TinyFish. Returns ranked results with titles, snippets, site names, and URLs.",
		fn, tools.WithMiddleware(c.mw...))
}

// Fetch builds the tinyfish_fetch tool (Python parity:
// TinyFishSearchToolkit.fetch).
func (t *TinyFishSearchToolkit) Fetch(opts ...TinyFishOption) (*tools.FunctionTool, error) {
	c, err := t.clone(opts...)
	if err != nil {
		return nil, err
	}
	type tinyfishArgs struct {
		URLs []string `json:"urls" agschema:"description=URLs to fetch content for. TinyFish supports 1-10 URLs.,required"`
	}
	fn := func(args tinyfishArgs, cx *agocontext.Context) (any, error) {
		return c.fetch(callContext(cx), cx, args.URLs)
	}
	return tools.NewFunctionTool("tinyfish_fetch",
		"Fetch and extract clean content from URLs using TinyFish. "+
			"Renders pages in a browser and returns extracted content plus per-URL errors.",
		fn, tools.WithMiddleware(c.mw...))
}

// productURL resolves one product endpoint (Python parity:
// tinyfish._BaseClient.product_url).
func (t *TinyFishSearchToolkit) productURL(product string) string {
	if t.baseURL != "" {
		return t.baseURL + "/v1/" + product
	}
	switch product {
	case "search":
		return tinyfishSearchHost
	default:
		return tinyfishFetchHost
	}
}

// headers composes the TinyFish request headers (Python parity: the SDK default
// headers). AGO-DEVIATION: the SDK sends its own User-Agent; this port
// identifies itself as ago.
func (t *TinyFishSearchToolkit) headers(key string) map[string]string {
	return map[string]string{
		"Accept":              "application/json",
		"X-API-Key":           key,
		"User-Agent":          "ago-searchtools",
		"X-TF-Request-Origin": "tinyfish-python",
	}
}

// search runs one Search request.
func (t *TinyFishSearchToolkit) search(ctx context.Context, cx *agocontext.Context, query string) (TinyFishSearchResponse, error) {
	key, err := t.key.resolve(cx)
	if err != nil {
		return TinyFishSearchResponse{}, err
	}
	params := url.Values{"query": {query}}
	for _, name := range []string{"location", "language"} {
		v, ok, err := t.params.stringParam(cx, name)
		if err != nil {
			return TinyFishSearchResponse{}, err
		}
		if ok {
			params.Set(name, v)
		}
	}
	data, err := httpRequest{
		provider: "tinyfish",
		client:   t.client,
		method:   http.MethodGet,
		url:      t.productURL("search"),
		query:    params,
		header:   t.headers(key),
	}.do(ctx)
	if err != nil {
		return TinyFishSearchResponse{}, err
	}
	var raw struct {
		Query        string                 `json:"query"`
		Results      []TinyFishSearchResult `json:"results"`
		TotalResults int                    `json:"total_results"`
	}
	if err := decodeJSON("tinyfish", data, &raw); err != nil {
		return TinyFishSearchResponse{}, err
	}
	out := TinyFishSearchResponse{
		Query:        raw.Query,
		Results:      raw.Results,
		TotalResults: raw.TotalResults,
	}
	if out.Results == nil {
		out.Results = []TinyFishSearchResult{}
	}
	return out, nil
}

// fetch runs one Fetch request.
func (t *TinyFishSearchToolkit) fetch(ctx context.Context, cx *agocontext.Context, urls []string) (any, error) {
	invalid := []string{}
	for _, u := range urls {
		if !safeURL(u) {
			invalid = append(invalid, u)
		}
	}
	if len(invalid) > 0 {
		// Python parity: the tool answers with an error object the model can act
		// on instead of failing the call.
		return map[string]any{
			"error": fmt.Sprintf("Only http/https URLs are supported; rejected: %s", pythonListRepr(invalid)),
		}, nil
	}
	if len(urls) == 0 || len(urls) > tinyfishMaxURLs {
		return nil, fmt.Errorf("%w: tinyfish: urls must hold between 1 and %d entries", ErrRejected, tinyfishMaxURLs)
	}
	key, err := t.key.resolve(cx)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"urls": urls, "api_integration": tinyfishIntegration}
	if format, ok, err := t.params.stringParam(cx, "format"); err != nil {
		return nil, err
	} else if ok {
		body["format"] = format
	}
	for _, name := range []string{"links", "image_links"} {
		v, ok, err := t.params.boolParam(cx, name)
		if err != nil {
			return nil, err
		}
		if ok {
			body[name] = v
		}
	}
	data, err := httpRequest{
		provider: "tinyfish",
		client:   t.client,
		method:   http.MethodPost,
		url:      t.productURL("fetch"),
		header:   t.headers(key),
		body:     body,
	}.do(ctx)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Results []TinyFishFetchResult `json:"results"`
		Errors  []TinyFishFetchError  `json:"errors"`
	}
	if err := decodeJSON("tinyfish", data, &raw); err != nil {
		return nil, err
	}
	out := TinyFishFetchResponse{Results: raw.Results, Errors: raw.Errors}
	if out.Results == nil {
		out.Results = []TinyFishFetchResult{}
	}
	if out.Errors == nil {
		out.Errors = []TinyFishFetchError{}
	}
	for i := range out.Results {
		if out.Results[i].Links == nil {
			out.Results[i].Links = []string{}
		}
		if out.Results[i].ImageLinks == nil {
			out.Results[i].ImageLinks = []string{}
		}
	}
	return out, nil
}

// safeURL reports whether the URL scheme is http or https (Python parity:
// tinyfish._safe_url).
func safeURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	return scheme == "http" || scheme == "https"
}
