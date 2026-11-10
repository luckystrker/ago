package searchtools

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/tools"
)

const (
	// firecrawlOrigin tags every request payload (Python parity: the SDK's
	// origin field, set to "ag2" by the extension).
	firecrawlOrigin = "ag2"
	// firecrawlSearchLimit and firecrawlSearchTimeout are the SDK's SearchRequest
	// defaults; they are always sent (Python parity: exclude_none over non-None
	// defaults).
	firecrawlSearchLimit   = 5
	firecrawlSearchTimeout = 300000
	// firecrawlDefaultMaxAge is the cache age the SDK accepts by default, in
	// milliseconds (4 hours).
	firecrawlDefaultMaxAge = 14400000
)

// FirecrawlSearchResult is one web hit (Python parity:
// firecrawl.FirecrawlSearchResult).
type FirecrawlSearchResult struct {
	URL string `json:"url"`
	// AGO-DEVIATION: absent optional values omit their wire key (omitempty)
	// where Python's dataclass emits an explicit null.
	Title       *string `json:"title,omitempty"`
	Description *string `json:"description,omitempty"`
	Position    *int    `json:"position,omitempty"`
	Markdown    *string `json:"markdown,omitempty"`
	Truncated   bool    `json:"truncated"`
}

// FirecrawlNewsResult is one news hit (Python parity:
// firecrawl.FirecrawlNewsResult).
type FirecrawlNewsResult struct {
	URL       string  `json:"url"`
	Title     *string `json:"title,omitempty"`
	Snippet   *string `json:"snippet,omitempty"`
	Date      *string `json:"date,omitempty"`
	Position  *int    `json:"position,omitempty"`
	Markdown  *string `json:"markdown,omitempty"`
	Truncated bool    `json:"truncated"`
}

// FirecrawlSearchResponse is the search tool result (Python parity:
// firecrawl.FirecrawlSearchResponse).
type FirecrawlSearchResponse struct {
	Query string                  `json:"query"`
	Web   []FirecrawlSearchResult `json:"web"`
	News  []FirecrawlNewsResult   `json:"news"`
}

// FirecrawlScrapeResult is one scraped page (Python parity:
// firecrawl.FirecrawlScrapeResult).
type FirecrawlScrapeResult struct {
	URL         string   `json:"url"`
	Title       *string  `json:"title,omitempty"`
	Description *string  `json:"description,omitempty"`
	Language    *string  `json:"language,omitempty"`
	StatusCode  *int     `json:"status_code,omitempty"`
	Markdown    *string  `json:"markdown,omitempty"`
	HTML        *string  `json:"html,omitempty"`
	Links       []string `json:"links"`
	Truncated   bool     `json:"truncated"`
}

// FirecrawlToolkit exposes Firecrawl Search and Scrape as agent tools (Python
// parity: ag2/extensions/tools/search/firecrawl.py FirecrawlToolkit).
//
// The key comes from the constructor, FIRECRAWL_API_KEY, or a conversation
// variable. A FirecrawlToolkit is immutable after construction and safe for
// concurrent use.
type FirecrawlToolkit struct {
	toolbox
	key     apiCredential
	baseURL string
	client  *http.Client
	mw      []middleware.Middleware
	params  *paramSet
}

var _ tools.Tool = (*FirecrawlToolkit)(nil)

// FirecrawlOption configures NewFirecrawlToolkit.
type FirecrawlOption func(*FirecrawlToolkit) error

// WithFirecrawlBaseURL overrides the API origin (default
// https://api.firecrawl.dev).
func WithFirecrawlBaseURL(base string) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		if base == "" {
			return errEmptyOption("firecrawl", "base URL")
		}
		t.baseURL = strings.TrimRight(base, "/")
		return nil
	}
}

// WithFirecrawlHTTPClient overrides the HTTP client.
func WithFirecrawlHTTPClient(c *http.Client) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		if c == nil {
			return errNilOption("firecrawl", "http client")
		}
		t.client = c
		return nil
	}
}

// WithFirecrawlAPIKeyVariable reads the key from the named conversation variable.
func WithFirecrawlAPIKeyVariable(name string) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		if name == "" {
			return errEmptyOption("firecrawl", "key variable")
		}
		t.key.variable = name
		return nil
	}
}

// WithFirecrawlMiddleware adds tool-scoped middleware to both members.
func WithFirecrawlMiddleware(mw ...middleware.Middleware) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		if err := checkMiddleware("firecrawl", mw); err != nil {
			return err
		}
		t.mw = append(t.mw, mw...)
		return nil
	}
}

// WithFirecrawlLimit caps results per source (Python parity: limit; Firecrawl's
// own default is 5).
func WithFirecrawlLimit(n int) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		if n <= 0 || n > 100 {
			return errOptionValue("firecrawl", "limit", "must be between 1 and 100")
		}
		t.params.set("limit", n)
		return nil
	}
}

// WithFirecrawlSources selects the result types ("web", "news"; Python parity:
// sources).
func WithFirecrawlSources(sources ...string) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		for _, s := range sources {
			switch s {
			case "web", "news", "images", "alexandria":
			default:
				return errOptionValue("firecrawl", "sources", "contains an unknown source type")
			}
		}
		t.params.set("sources", append([]string{}, sources...))
		return nil
	}
}

// WithFirecrawlCategories narrows web results ("github", "pdf"; Python parity:
// categories).
func WithFirecrawlCategories(categories ...string) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		for _, c := range categories {
			switch c {
			case "github", "pdf", "research", "developer", "gov":
			default:
				return errOptionValue("firecrawl", "categories", "contains an unknown category")
			}
		}
		t.params.set("categories", append([]string{}, categories...))
		return nil
	}
}

// WithFirecrawlDomains restricts results to include or exclude domains (Python
// parity: include_domains / exclude_domains; they are mutually exclusive).
func WithFirecrawlDomains(include, exclude []string) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		if len(include) > 0 && len(exclude) > 0 {
			return fmt.Errorf("%w: firecrawl: include_domains and exclude_domains cannot both be set", ErrRejected)
		}
		if len(include) > 0 {
			t.params.set("include_domains", append([]string{}, include...))
		}
		if len(exclude) > 0 {
			t.params.set("exclude_domains", append([]string{}, exclude...))
		}
		return nil
	}
}

// WithFirecrawlTBS sets the web time filter, for example "qdr:d" (Python
// parity: tbs).
func WithFirecrawlTBS(v string) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		t.params.set("tbs", v)
		return nil
	}
}

// WithFirecrawlLocation sets the search location ("Germany"; Python parity:
// location).
func WithFirecrawlLocation(v string) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		t.params.set("location", v)
		return nil
	}
}

// WithFirecrawlCountry sets the ISO country code ("US"; Python parity: country).
func WithFirecrawlCountry(v string) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		t.params.set("country", v)
		return nil
	}
}

// WithFirecrawlIgnoreInvalidURLs drops results Firecrawl cannot scrape (Python
// parity: ignore_invalid_urls).
func WithFirecrawlIgnoreInvalidURLs(v bool) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		t.params.set("ignore_invalid_urls", v)
		return nil
	}
}

// WithFirecrawlTimeout sets the Firecrawl API timeout in milliseconds (Python
// parity: timeout).
func WithFirecrawlTimeout(ms int) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		if ms <= 0 || ms > 300000 {
			return errOptionValue("firecrawl", "timeout", "must be between 1 and 300000 ms")
		}
		t.params.set("timeout", ms)
		return nil
	}
}

// WithFirecrawlScrapeResults makes each search result carry its page content as
// markdown (Python parity: scrape_results).
func WithFirecrawlScrapeResults(v bool) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		t.params.set("scrape_results", v)
		return nil
	}
}

// WithFirecrawlScrapeOnlyMainContent drops headers, navigation and footers from
// search-scraped pages (Python parity: scrape_only_main_content).
func WithFirecrawlScrapeOnlyMainContent(v bool) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		t.params.set("scrape_only_main_content", v)
		return nil
	}
}

// WithFirecrawlScrapeMaxAge accepts cached search-scraped pages up to this many
// milliseconds old; 0 always fetches fresh (Python parity: scrape_max_age).
func WithFirecrawlScrapeMaxAge(ms int) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		if ms < 0 {
			return errOptionValue("firecrawl", "scrape_max_age", "must not be negative")
		}
		t.params.set("scrape_max_age", ms)
		return nil
	}
}

// WithFirecrawlMaxCharacters caps the page content each tool returns: markdown
// and HTML each, and the combined length of links (Python parity:
// max_characters).
func WithFirecrawlMaxCharacters(n int) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		if n < 0 {
			return errOptionValue("firecrawl", "max_characters", "must not be negative")
		}
		t.params.set("max_characters", n)
		return nil
	}
}

// WithFirecrawlFormats sets the scrape output formats (Python parity: formats;
// default ["markdown"]).
func WithFirecrawlFormats(formats ...string) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		for _, f := range formats {
			switch f {
			case "markdown", "html", "links":
			default:
				return errOptionValue("firecrawl", "formats", `must be "markdown", "html" or "links"`)
			}
		}
		t.params.set("formats", append([]string{}, formats...))
		return nil
	}
}

// WithFirecrawlOnlyMainContent drops headers, navigation and footers (Python
// parity: only_main_content).
func WithFirecrawlOnlyMainContent(v bool) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		t.params.set("only_main_content", v)
		return nil
	}
}

// WithFirecrawlTags keeps or removes HTML tags, classes or ids (Python parity:
// include_tags / exclude_tags).
func WithFirecrawlTags(include, exclude []string) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		if len(include) > 0 {
			t.params.set("include_tags", append([]string{}, include...))
		}
		if len(exclude) > 0 {
			t.params.set("exclude_tags", append([]string{}, exclude...))
		}
		return nil
	}
}

// WithFirecrawlPageHeaders sends extra HTTP headers with the page request
// (Python parity: headers).
func WithFirecrawlPageHeaders(headers map[string]string) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		copied := map[string]string{}
		for k, v := range headers {
			copied[k] = v
		}
		t.params.set("headers", copied)
		return nil
	}
}

// WithFirecrawlWaitFor waits this many milliseconds for the page to render
// (Python parity: wait_for).
func WithFirecrawlWaitFor(ms int) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		if ms < 0 {
			return errOptionValue("firecrawl", "wait_for", "must not be negative")
		}
		t.params.set("wait_for", ms)
		return nil
	}
}

// WithFirecrawlMobile emulates a mobile device (Python parity: mobile).
func WithFirecrawlMobile(v bool) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		t.params.set("mobile", v)
		return nil
	}
}

// WithFirecrawlProxy selects the proxy type (Python parity: proxy).
func WithFirecrawlProxy(v string) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		switch v {
		case "basic", "stealth", "enhanced", "auto":
		default:
			return errOptionValue("firecrawl", "proxy", `must be "basic", "stealth", "enhanced" or "auto"`)
		}
		t.params.set("proxy", v)
		return nil
	}
}

// WithFirecrawlBlockAds blocks ads and cookie pop-ups (Python parity: block_ads;
// Firecrawl defaults to true).
func WithFirecrawlBlockAds(v bool) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		t.params.set("block_ads", v)
		return nil
	}
}

// WithFirecrawlMaxAge accepts a cached copy up to this many milliseconds old
// (Python parity: max_age; Firecrawl defaults to 2 days).
func WithFirecrawlMaxAge(ms int) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		if ms < 0 {
			return errOptionValue("firecrawl", "max_age", "must not be negative")
		}
		t.params.set("max_age", ms)
		return nil
	}
}

// WithFirecrawlStoreInCache allows Firecrawl to cache the page (Python parity:
// store_in_cache).
func WithFirecrawlStoreInCache(v bool) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		t.params.set("store_in_cache", v)
		return nil
	}
}

// WithFirecrawlVariable binds a default to a conversation variable.
func WithFirecrawlVariable(param, variable string) FirecrawlOption {
	return func(t *FirecrawlToolkit) error {
		if param == "" || variable == "" {
			return errOptionValue("firecrawl", "variable", "needs both a parameter and a variable name")
		}
		t.params.bind(param, variable)
		return nil
	}
}

// NewFirecrawlToolkit builds the firecrawl_toolkit. An empty apiKey is accepted:
// the key is then read from FIRECRAWL_API_KEY or the key variable.
func NewFirecrawlToolkit(apiKey string, opts ...FirecrawlOption) (*FirecrawlToolkit, error) {
	t := &FirecrawlToolkit{
		key:     apiCredential{key: apiKey, envVar: "FIRECRAWL_API_KEY"},
		baseURL: "https://api.firecrawl.dev",
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
	scrape, err := t.Scrape()
	if err != nil {
		return nil, err
	}
	tk, err := tools.NewToolkit("firecrawl_toolkit", nil, search, scrape)
	if err != nil {
		return nil, err
	}
	t.tk = tk
	return t, nil
}

// clone copies the toolkit with per-factory overrides applied.
func (t *FirecrawlToolkit) clone(opts ...FirecrawlOption) (*FirecrawlToolkit, error) {
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

// Search builds the firecrawl_search tool (Python parity:
// FirecrawlToolkit.search).
func (t *FirecrawlToolkit) Search(opts ...FirecrawlOption) (*tools.FunctionTool, error) {
	c, err := t.clone(opts...)
	if err != nil {
		return nil, err
	}
	type firecrawlSearchArgs struct {
		Query string `json:"query" agschema:"description=The search query string.,required"`
	}
	fn := func(args firecrawlSearchArgs, cx *agocontext.Context) (FirecrawlSearchResponse, error) {
		return c.search(callContext(cx), cx, args.Query)
	}
	return tools.NewFunctionTool("firecrawl_search",
		"Search the web using Firecrawl. Returns ranked results with URLs, titles, and descriptions. "+
			"Results may also include the main content of each page as markdown.",
		fn, tools.WithMiddleware(c.mw...))
}

// Scrape builds the firecrawl_scrape tool (Python parity:
// FirecrawlToolkit.scrape).
func (t *FirecrawlToolkit) Scrape(opts ...FirecrawlOption) (*tools.FunctionTool, error) {
	c, err := t.clone(opts...)
	if err != nil {
		return nil, err
	}
	type firecrawlScrapeArgs struct {
		URL string `json:"url" agschema:"description=The http or https URL of the page to read.,required"`
	}
	fn := func(args firecrawlScrapeArgs, cx *agocontext.Context) (any, error) {
		return c.scrape(callContext(cx), cx, args.URL)
	}
	return tools.NewFunctionTool("firecrawl_scrape",
		"Read a single web page using Firecrawl. Returns the page content (markdown by default) "+
			"with the page title, description, and status code.",
		fn, tools.WithMiddleware(c.mw...))
}

// post sends one JSON request to the Firecrawl API with the bearer key.
func (t *FirecrawlToolkit) post(ctx context.Context, cx *agocontext.Context, path string, body map[string]any) ([]byte, error) {
	key, err := t.key.resolve(cx)
	if err != nil {
		return nil, err
	}
	body["origin"] = firecrawlOrigin
	data, err := httpRequest{
		provider: "firecrawl",
		client:   t.client,
		method:   http.MethodPost,
		url:      t.baseURL + path,
		header:   map[string]string{"Authorization": "Bearer " + key},
		body:     body,
	}.do(ctx)
	if err != nil {
		return nil, err
	}
	// A 200 with success=false is a failed call (Python parity: handle_response_error).
	var envelope struct {
		Success *bool  `json:"success"`
		Error   string `json:"error"`
	}
	if err := decodeJSON("firecrawl", data, &envelope); err != nil {
		return nil, err
	}
	if envelope.Success != nil && !*envelope.Success {
		return nil, fmt.Errorf("%w: firecrawl: %s", ErrAPIResponse, strings.TrimSpace(envelope.Error))
	}
	return data, nil
}

// firecrawlScrapeDefaults are the SDK defaults every scrape payload carries
// (Python parity: prepare_scrape_options).
func firecrawlScrapeDefaults(formats []string) map[string]any {
	return map[string]any{
		"formats":             formats,
		"onlyMainContent":     true,
		"mobile":              false,
		"skipTlsVerification": true,
		"removeBase64Images":  true,
		"fastMode":            false,
		"blockAds":            true,
		"maxAge":              firecrawlDefaultMaxAge,
		"storeInCache":        true,
	}
}

// scrapeOptions composes the scrape tool's payload options.
func (t *FirecrawlToolkit) scrapeOptions(cx *agocontext.Context, formats []string) (map[string]any, error) {
	opts := firecrawlScrapeDefaults(formats)
	for paramName, wire := range map[string]string{
		"only_main_content": "onlyMainContent",
		"wait_for":          "waitFor",
		"mobile":            "mobile",
		"proxy":             "proxy",
		"block_ads":         "blockAds",
		"max_age":           "maxAge",
		"store_in_cache":    "storeInCache",
		"timeout":           "timeout",
	} {
		v, ok, err := t.params.get(cx, paramName)
		if err != nil {
			return nil, err
		}
		if ok {
			opts[wire] = v
		}
	}
	for paramName, wire := range map[string]string{
		"include_tags": "includeTags",
		"exclude_tags": "excludeTags",
	} {
		v, ok, err := t.params.stringsParam(cx, paramName)
		if err != nil {
			return nil, err
		}
		if ok {
			opts[wire] = v
		}
	}
	if v, ok, err := t.params.get(cx, "headers"); err != nil {
		return nil, err
	} else if ok {
		opts["headers"] = v
	}
	return opts, nil
}

// searchScrapeOptions composes the options of a search-time scrape. Python
// parity: the extension passes only formats, only_main_content and max_age, and
// the SDK fills in the remaining defaults.
func (t *FirecrawlToolkit) searchScrapeOptions(cx *agocontext.Context) (map[string]any, error) {
	opts := firecrawlScrapeDefaults([]string{"markdown"})
	for paramName, wire := range map[string]string{
		"scrape_only_main_content": "onlyMainContent",
		"scrape_max_age":           "maxAge",
	} {
		v, ok, err := t.params.get(cx, paramName)
		if err != nil {
			return nil, err
		}
		if ok {
			opts[wire] = v
		}
	}
	return opts, nil
}

// search runs one /v2/search request.
func (t *FirecrawlToolkit) search(ctx context.Context, cx *agocontext.Context, query string) (FirecrawlSearchResponse, error) {
	body := map[string]any{"query": query, "limit": firecrawlSearchLimit, "timeout": firecrawlSearchTimeout}
	if v, ok, err := t.params.intParam(cx, "limit"); err != nil {
		return FirecrawlSearchResponse{}, err
	} else if ok {
		body["limit"] = v
	}
	if v, ok, err := t.params.intParam(cx, "timeout"); err != nil {
		return FirecrawlSearchResponse{}, err
	} else if ok {
		body["timeout"] = v
	}
	if v, ok, err := t.params.stringsParam(cx, "sources"); err != nil {
		return FirecrawlSearchResponse{}, err
	} else if ok {
		sources := make([]map[string]string, 0, len(v))
		for _, s := range v {
			sources = append(sources, map[string]string{"type": s})
		}
		body["sources"] = sources
	}
	if v, ok, err := t.params.stringsParam(cx, "categories"); err != nil {
		return FirecrawlSearchResponse{}, err
	} else if ok {
		categories := make([]map[string]string, 0, len(v))
		for _, c := range v {
			categories = append(categories, map[string]string{"type": c})
		}
		body["categories"] = categories
	}
	include, hasInclude, err := t.params.stringsParam(cx, "include_domains")
	if err != nil {
		return FirecrawlSearchResponse{}, err
	}
	exclude, hasExclude, err := t.params.stringsParam(cx, "exclude_domains")
	if err != nil {
		return FirecrawlSearchResponse{}, err
	}
	if len(include) > 0 && len(exclude) > 0 {
		return FirecrawlSearchResponse{}, fmt.Errorf(
			"%w: firecrawl: include_domains and exclude_domains cannot both be set", ErrRejected)
	}
	if hasInclude {
		body["includeDomains"] = include
	}
	if hasExclude {
		body["excludeDomains"] = exclude
	}
	for paramName, wire := range map[string]string{
		"tbs":      "tbs",
		"location": "location",
		"country":  "country",
	} {
		v, ok, err := t.params.stringParam(cx, paramName)
		if err != nil {
			return FirecrawlSearchResponse{}, err
		}
		if ok {
			body[wire] = v
		}
	}
	if v, ok, err := t.params.boolParam(cx, "ignore_invalid_urls"); err != nil {
		return FirecrawlSearchResponse{}, err
	} else if ok {
		body["ignoreInvalidURLs"] = v
	}
	if v, ok, err := t.params.boolParam(cx, "scrape_results"); err != nil {
		return FirecrawlSearchResponse{}, err
	} else if ok && v {
		opts, err := t.searchScrapeOptions(cx)
		if err != nil {
			return FirecrawlSearchResponse{}, err
		}
		body["scrapeOptions"] = opts
	}
	limitChars := -1
	if v, ok, err := t.params.intParam(cx, "max_characters"); err != nil {
		return FirecrawlSearchResponse{}, err
	} else if ok {
		limitChars = v
	}
	data, err := t.post(ctx, cx, "/v2/search", body)
	if err != nil {
		return FirecrawlSearchResponse{}, err
	}
	var raw struct {
		Data struct {
			Web  []any `json:"web"`
			News []any `json:"news"`
		} `json:"data"`
	}
	if err := decodeJSON("firecrawl", data, &raw); err != nil {
		return FirecrawlSearchResponse{}, err
	}
	out := FirecrawlSearchResponse{
		Query: query,
		Web:   []FirecrawlSearchResult{},
		News:  []FirecrawlNewsResult{},
	}
	for i, item := range raw.Data.Web {
		out.Web = append(out.Web, firecrawlWebResult(item, i+1, limitChars))
	}
	for i, item := range raw.Data.News {
		out.News = append(out.News, firecrawlNewsResult(item, i+1, limitChars))
	}
	return out, nil
}

// firecrawlDocumentKeys are the keys that mark a search entry as a scraped
// Document rather than a plain result (Python parity: _transform_array).
var firecrawlDocumentKeys = []string{
	"markdown", "html", "rawHtml", "links", "screenshot", "changeTracking", "summary", "json",
}

// isFirecrawlDocument reports whether a search entry is a scraped Document.
func isFirecrawlDocument(item map[string]any) bool {
	for _, key := range firecrawlDocumentKeys {
		if _, ok := item[key]; ok {
			return true
		}
	}
	return false
}

// firecrawlWebResult shapes one web entry.
func firecrawlWebResult(item any, rank, maxChars int) FirecrawlSearchResult {
	switch v := item.(type) {
	case string:
		return FirecrawlSearchResult{URL: v}
	case map[string]any:
		if isFirecrawlDocument(v) {
			url, title, description, markdown, cut := firecrawlPage(v, maxChars)
			position := rank
			return FirecrawlSearchResult{
				URL:         url,
				Title:       title,
				Description: description,
				Position:    &position,
				Markdown:    markdown,
				Truncated:   cut,
			}
		}
		res := FirecrawlSearchResult{
			URL: rawText(v, "url"),
		}
		if title, ok := v["title"].(string); ok {
			res.Title = &title
		}
		if description, ok := v["description"].(string); ok {
			res.Description = &description
		}
		if position, ok := rawInt(v["position"]); ok {
			res.Position = &position
		}
		return res
	default:
		return FirecrawlSearchResult{}
	}
}

// firecrawlNewsResult shapes one news entry.
func firecrawlNewsResult(item any, rank, maxChars int) FirecrawlNewsResult {
	switch v := item.(type) {
	case string:
		return FirecrawlNewsResult{URL: v}
	case map[string]any:
		if isFirecrawlDocument(v) {
			url, title, description, markdown, cut := firecrawlPage(v, maxChars)
			position := rank
			return FirecrawlNewsResult{
				URL:       url,
				Title:     title,
				Snippet:   description,
				Position:  &position,
				Markdown:  markdown,
				Truncated: cut,
			}
		}
		res := FirecrawlNewsResult{URL: rawText(v, "url")}
		if title, ok := v["title"].(string); ok {
			res.Title = &title
		}
		if snippet, ok := v["snippet"].(string); ok {
			res.Snippet = &snippet
		}
		if date, ok := v["date"].(string); ok {
			res.Date = &date
		}
		if position, ok := rawInt(v["position"]); ok {
			res.Position = &position
		}
		return res
	default:
		return FirecrawlNewsResult{}
	}
}

// firecrawlPage extracts a scraped Document's page fields, truncating its
// markdown (Python parity: _scraped_page).
func firecrawlPage(item map[string]any, maxChars int) (string, *string, *string, *string, bool) {
	metadata, _ := item["metadata"].(map[string]any)
	url := rawText(metadata, "url")
	if url == "" {
		url = rawText(metadata, "source_url")
	}
	var title, description, markdown *string
	if v, ok := metadata["title"].(string); ok {
		title = &v
	}
	if v, ok := metadata["description"].(string); ok {
		description = &v
	}
	if v, ok := item["markdown"].(string); ok {
		cut, truncated := truncate(v, maxChars)
		markdown = &cut
		return url, title, description, markdown, truncated
	}
	return url, title, description, markdown, false
}

// scrape runs one /v2/scrape request.
func (t *FirecrawlToolkit) scrape(ctx context.Context, cx *agocontext.Context, target string) (any, error) {
	if !safeURL(target) {
		// Python parity: the model gets an error object it can correct.
		return map[string]any{
			"error": fmt.Sprintf("Only http/https URLs are supported; rejected: '%s'", target),
		}, nil
	}
	formats := []string{"markdown"}
	if v, ok, err := t.params.stringsParam(cx, "formats"); err != nil {
		return nil, err
	} else if ok && len(v) > 0 {
		formats = v
	}
	opts, err := t.scrapeOptions(cx, formats)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"url": target}
	for k, v := range opts {
		body[k] = v
	}
	limitChars := -1
	if v, ok, err := t.params.intParam(cx, "max_characters"); err != nil {
		return nil, err
	} else if ok {
		limitChars = v
	}
	data, err := t.post(ctx, cx, "/v2/scrape", body)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Data map[string]any `json:"data"`
	}
	if err := decodeJSON("firecrawl", data, &raw); err != nil {
		return nil, err
	}
	metadata, _ := raw.Data["metadata"].(map[string]any)
	out := FirecrawlScrapeResult{
		URL:   rawText(metadata, "url"),
		Links: []string{},
	}
	if out.URL == "" {
		out.URL = rawText(metadata, "source_url")
	}
	if out.URL == "" {
		out.URL = target
	}
	if v, ok := metadata["title"].(string); ok {
		out.Title = &v
	}
	if v, ok := metadata["description"].(string); ok {
		out.Description = &v
	}
	if v, ok := metadata["language"].(string); ok {
		out.Language = &v
	}
	if v, ok := rawInt(metadata["status_code"]); ok {
		out.StatusCode = &v
	}
	if v, ok := raw.Data["markdown"].(string); ok {
		cut, truncated := truncate(v, limitChars)
		out.Markdown = &cut
		out.Truncated = out.Truncated || truncated
	}
	if v, ok := raw.Data["html"].(string); ok {
		cut, truncated := truncate(v, limitChars)
		out.HTML = &cut
		out.Truncated = out.Truncated || truncated
	}
	if links := firecrawlLinks(raw.Data["links"]); links != nil {
		kept, truncated := truncateLinks(links, limitChars)
		out.Links = kept
		out.Truncated = out.Truncated || truncated
	}
	return out, nil
}

// firecrawlLinks decodes a scraped page's links list.
func firecrawlLinks(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	links := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			links = append(links, s)
		}
	}
	return links
}
