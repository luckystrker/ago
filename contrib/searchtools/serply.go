package searchtools

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/tools"
)

// serplyUserAgent is sent because Serply sits behind Cloudflare, which blocks
// httpx's default `python-httpx/*` User-Agent (Python parity).
const serplyUserAgent = "ag2-serply-extension"

// serplyNewsMaxResults caps a Google News answer, which ignores the num
// parameter and returns its whole feed (Python parity: _NEWS_MAX_RESULTS).
const serplyNewsMaxResults = 10

// SerplyWebResult is one web hit (Python parity: serply.SerplyWebResult).
type SerplyWebResult struct {
	Title       string `json:"title"`
	Link        string `json:"link"`
	Description string `json:"description"`
	// AGO-DEVIATION: absent optional values omit their wire key (omitempty)
	// where Python's dataclass emits an explicit null.
	Position *int `json:"position,omitempty"`
}

// SerplyWebSearchResponse is the web tool result (Python parity:
// serply.SerplyWebSearchResponse).
type SerplyWebSearchResponse struct {
	Query   string            `json:"query"`
	Results []SerplyWebResult `json:"results"`
}

// SerplyNewsResult is one news hit (Python parity: serply.SerplyNewsResult).
type SerplyNewsResult struct {
	Title     string `json:"title"`
	Link      string `json:"link"`
	Published string `json:"published"`
	Source    string `json:"source"`
}

// SerplyNewsSearchResponse is the news tool result (Python parity:
// serply.SerplyNewsSearchResponse).
type SerplyNewsSearchResponse struct {
	Query   string             `json:"query"`
	Results []SerplyNewsResult `json:"results"`
}

// SerplyScholarResult is one academic hit (Python parity:
// serply.SerplyScholarResult).
type SerplyScholarResult struct {
	Title     string `json:"title"`
	Link      string `json:"link"`
	Authors   string `json:"authors"`
	PDFLink   string `json:"pdf_link"`
	Citations *int   `json:"citations,omitempty"`
}

// SerplyScholarSearchResponse is the scholar tool result (Python parity:
// serply.SerplyScholarSearchResponse).
type SerplyScholarSearchResponse struct {
	Query   string                `json:"query"`
	Results []SerplyScholarResult `json:"results"`
}

// SerplySearchToolkit searches Google web, news and scholar results through the
// Serply REST API (Python parity: ag2/extensions/tools/search/serply.py
// SerplySearchToolkit). Passing the toolkit registers all three tools.
//
// A key is required. A SerplySearchToolkit is immutable after construction and
// safe for concurrent use.
type SerplySearchToolkit struct {
	toolbox
	key     apiCredential
	baseURL string
	client  *http.Client
	mw      []middleware.Middleware
	params  *paramSet
}

var _ tools.Tool = (*SerplySearchToolkit)(nil)

// SerplyOption configures NewSerplySearchToolkit.
type SerplyOption func(*SerplySearchToolkit) error

// WithSerplyBaseURL overrides the API origin (default https://api.serply.io).
func WithSerplyBaseURL(base string) SerplyOption {
	return func(t *SerplySearchToolkit) error {
		if base == "" {
			return errEmptyOption("serply", "base URL")
		}
		t.baseURL = strings.TrimRight(base, "/")
		return nil
	}
}

// WithSerplyHTTPClient overrides the HTTP client.
func WithSerplyHTTPClient(c *http.Client) SerplyOption {
	return func(t *SerplySearchToolkit) error {
		if c == nil {
			return errNilOption("serply", "http client")
		}
		t.client = c
		return nil
	}
}

// WithSerplyAPIKeyVariable reads the key from the named conversation variable.
func WithSerplyAPIKeyVariable(name string) SerplyOption {
	return func(t *SerplySearchToolkit) error {
		if name == "" {
			return errEmptyOption("serply", "key variable")
		}
		t.key.variable = name
		return nil
	}
}

// WithSerplyMiddleware adds tool-scoped middleware to all three members.
func WithSerplyMiddleware(mw ...middleware.Middleware) SerplyOption {
	return func(t *SerplySearchToolkit) error {
		if err := checkMiddleware("serply", mw); err != nil {
			return err
		}
		t.mw = append(t.mw, mw...)
		return nil
	}
}

// WithSerplyNum sets the default result count (Python parity: num).
func WithSerplyNum(n int) SerplyOption {
	return func(t *SerplySearchToolkit) error {
		if n <= 0 {
			return errOptionValue("serply", "num", "must be > 0")
		}
		t.params.set("num", n)
		return nil
	}
}

// WithSerplyLocale sets the default Google country and interface language
// (Python parity: gl / hl).
func WithSerplyLocale(gl, hl string) SerplyOption {
	return func(t *SerplySearchToolkit) error {
		if gl != "" {
			t.params.set("gl", gl)
		}
		if hl != "" {
			t.params.set("hl", hl)
		}
		return nil
	}
}

// WithSerplyProxyLocation sets the region Serply searches from, sent as the
// X-Proxy-Location header (Python parity: proxy_location).
func WithSerplyProxyLocation(v string) SerplyOption {
	return func(t *SerplySearchToolkit) error {
		switch v {
		case "EU", "CA", "US", "IE", "GB", "FR", "DE", "SE", "IN", "JP", "KR", "SG", "AU", "BR":
		default:
			return errOptionValue("serply", "proxy_location", "is not a known region")
		}
		t.params.set("proxy_location", v)
		return nil
	}
}

// WithSerplyVariable binds a default to a conversation variable.
func WithSerplyVariable(param, variable string) SerplyOption {
	return func(t *SerplySearchToolkit) error {
		if param == "" || variable == "" {
			return errOptionValue("serply", "variable", "needs both a parameter and a variable name")
		}
		t.params.bind(param, variable)
		return nil
	}
}

// NewSerplySearchToolkit builds the serply_search_toolkit (Python parity: it
// raises when api_key is empty).
func NewSerplySearchToolkit(apiKey string, opts ...SerplyOption) (*SerplySearchToolkit, error) {
	t := &SerplySearchToolkit{
		key:     apiCredential{key: apiKey},
		baseURL: "https://api.serply.io",
		params:  newParamSet(),
	}
	for _, opt := range opts {
		if err := opt(t); err != nil {
			return nil, err
		}
	}
	if t.key.key == "" && t.key.variable == "" {
		return nil, ErrMissingKey
	}
	web, err := t.Web()
	if err != nil {
		return nil, err
	}
	news, err := t.News()
	if err != nil {
		return nil, err
	}
	scholar, err := t.Scholar()
	if err != nil {
		return nil, err
	}
	tk, err := tools.NewToolkit("serply_search_toolkit", nil, web, news, scholar)
	if err != nil {
		return nil, err
	}
	t.tk = tk
	return t, nil
}

// clone copies the toolkit with per-factory overrides applied.
func (t *SerplySearchToolkit) clone(opts ...SerplyOption) (*SerplySearchToolkit, error) {
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

// Web builds the serply_web_search tool (Python parity: SerplySearchToolkit.web).
func (t *SerplySearchToolkit) Web(opts ...SerplyOption) (*tools.FunctionTool, error) {
	c, err := t.clone(opts...)
	if err != nil {
		return nil, err
	}
	type serplyArgs struct {
		Query string `json:"query" agschema:"description=The web search query string.,required"`
	}
	fn := func(args serplyArgs, cx *agocontext.Context) (SerplyWebSearchResponse, error) {
		return c.web(callContext(cx), cx, args.Query)
	}
	return tools.NewFunctionTool("serply_web_search",
		"Search the web with Google through Serply. Returns ranked results with titles, snippets, and URLs.",
		fn, tools.WithMiddleware(c.mw...))
}

// News builds the serply_news_search tool (Python parity:
// SerplySearchToolkit.news).
func (t *SerplySearchToolkit) News(opts ...SerplyOption) (*tools.FunctionTool, error) {
	c, err := t.clone(opts...)
	if err != nil {
		return nil, err
	}
	type serplyArgs struct {
		Query string `json:"query" agschema:"description=The news search query string.,required"`
	}
	fn := func(args serplyArgs, cx *agocontext.Context) (SerplyNewsSearchResponse, error) {
		return c.news(callContext(cx), cx, args.Query)
	}
	return tools.NewFunctionTool("serply_news_search",
		"Search Google News through Serply. Returns recent articles with titles, sources, publish dates, and URLs.",
		fn, tools.WithMiddleware(c.mw...))
}

// Scholar builds the serply_scholar_search tool (Python parity:
// SerplySearchToolkit.scholar).
func (t *SerplySearchToolkit) Scholar(opts ...SerplyOption) (*tools.FunctionTool, error) {
	c, err := t.clone(opts...)
	if err != nil {
		return nil, err
	}
	type serplyArgs struct {
		Query string `json:"query" agschema:"description=The academic search query string.,required"`
	}
	fn := func(args serplyArgs, cx *agocontext.Context) (SerplyScholarSearchResponse, error) {
		return c.scholar(callContext(cx), cx, args.Query)
	}
	return tools.NewFunctionTool("serply_scholar_search",
		"Search Google Scholar through Serply. Returns academic articles with titles, authors, "+
			"citation counts, and URLs.",
		fn, tools.WithMiddleware(c.mw...))
}

// get runs one GET against the Serply REST API (Python parity: serply._get).
func (t *SerplySearchToolkit) get(ctx context.Context, cx *agocontext.Context, path string, withNum bool, query string) ([]byte, error) {
	key, err := t.key.resolve(cx)
	if err != nil {
		return nil, err
	}
	params := url.Values{"q": {query}}
	if withNum {
		if num, ok, err := t.params.intParam(cx, "num"); err != nil {
			return nil, err
		} else if ok {
			params.Set("num", strconv.Itoa(num))
		}
	}
	for _, name := range []string{"gl", "hl"} {
		v, ok, err := t.params.stringParam(cx, name)
		if err != nil {
			return nil, err
		}
		if ok {
			params.Set(name, v)
		}
	}
	header := map[string]string{
		"X-Api-Key":  key,
		"User-Agent": serplyUserAgent,
	}
	if loc, ok, err := t.params.stringParam(cx, "proxy_location"); err != nil {
		return nil, err
	} else if ok {
		header["X-Proxy-Location"] = loc
	}
	return httpRequest{
		provider: "serply",
		client:   t.client,
		method:   http.MethodGet,
		url:      t.baseURL + path,
		query:    params,
		header:   header,
	}.do(ctx)
}

// web runs one /v1/search/ request.
func (t *SerplySearchToolkit) web(ctx context.Context, cx *agocontext.Context, query string) (SerplyWebSearchResponse, error) {
	data, err := t.get(ctx, cx, "/v1/search/", true, query)
	if err != nil {
		return SerplyWebSearchResponse{}, err
	}
	var raw struct {
		Results []map[string]any `json:"results"`
	}
	if err := decodeJSON("serply", data, &raw); err != nil {
		return SerplyWebSearchResponse{}, err
	}
	out := SerplyWebSearchResponse{Query: query, Results: []SerplyWebResult{}}
	for _, item := range raw.Results {
		res := SerplyWebResult{
			Title:       rawText(item, "title"),
			Link:        rawText(item, "link"),
			Description: rawText(item, "description"),
		}
		if n, ok := rawInt(item["position"]); ok {
			res.Position = &n
		}
		out.Results = append(out.Results, res)
	}
	return out, nil
}

// news runs one /v1/news/ request, truncating the feed client-side.
func (t *SerplySearchToolkit) news(ctx context.Context, cx *agocontext.Context, query string) (SerplyNewsSearchResponse, error) {
	data, err := t.get(ctx, cx, "/v1/news/", false, query)
	if err != nil {
		return SerplyNewsSearchResponse{}, err
	}
	var raw struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := decodeJSON("serply", data, &raw); err != nil {
		return SerplyNewsSearchResponse{}, err
	}
	limit := serplyNewsMaxResults
	if num, ok, err := t.params.intParam(cx, "num"); err != nil {
		return SerplyNewsSearchResponse{}, err
	} else if ok && num > 0 {
		limit = num
	}
	out := SerplyNewsSearchResponse{Query: query, Results: []SerplyNewsResult{}}
	for _, item := range raw.Entries {
		if len(out.Results) >= limit {
			break
		}
		out.Results = append(out.Results, SerplyNewsResult{
			Title:     rawText(item, "title"),
			Link:      rawText(item, "link"),
			Published: rawText(item, "published"),
			Source:    rawText(nestedValue(item, "source"), "title"),
		})
	}
	return out, nil
}

// scholar runs one /v1/scholar/ request.
func (t *SerplySearchToolkit) scholar(ctx context.Context, cx *agocontext.Context, query string) (SerplyScholarSearchResponse, error) {
	data, err := t.get(ctx, cx, "/v1/scholar/", true, query)
	if err != nil {
		return SerplyScholarSearchResponse{}, err
	}
	var raw struct {
		Articles []map[string]any `json:"articles"`
	}
	if err := decodeJSON("serply", data, &raw); err != nil {
		return SerplyScholarSearchResponse{}, err
	}
	out := SerplyScholarSearchResponse{Query: query, Results: []SerplyScholarResult{}}
	for _, item := range raw.Articles {
		res := SerplyScholarResult{
			Title:   rawText(item, "title"),
			Link:    rawText(item, "link"),
			Authors: rawText(nestedValue(item, "author"), "names"),
			PDFLink: rawText(nestedValue(item, "doc"), "link"),
		}
		if n, ok := rawInt(nestedValue(item, "extras", "citations", "count")); ok {
			res.Citations = &n
		}
		out.Results = append(out.Results, res)
	}
	return out, nil
}

// rawText returns v[key] when v is a mapping holding a string there, else ""
// (Python parity: serply._text).
func rawText(v any, key string) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// rawInt returns v when it is a real integer, else ok=false (Python parity:
// serply._int, where a bool is not an integer).
func rawInt(v any) (int, bool) {
	n, ok := v.(float64)
	if !ok || n != float64(int(n)) {
		return 0, false
	}
	return int(n), true
}

// nestedValue walks keys through nested mappings, returning nil as soon as one
// is missing (Python parity: serply._nested).
func nestedValue(v any, keys ...string) any {
	for _, key := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[key]
	}
	return v
}
