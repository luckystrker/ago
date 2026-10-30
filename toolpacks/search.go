package toolpacks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
)

// httpTimeout bounds one search request (each tool owns a client unless the
// caller supplies one).
const httpTimeout = 30 * time.Second

// doJSON sends a JSON request and decodes the response body into out. A non-2xx
// status surfaces the body (trimmed) so the model sees the API's own message.
func doJSON(client *http.Client, method, endpoint string, headers map[string]string, body any, out any) error {
	client = searchClient(client)
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("toolpacks: encode request: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, endpoint, reader)
	if err != nil {
		return fmt.Errorf("toolpacks: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("toolpacks: request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("toolpacks: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("toolpacks: %s %s: HTTP %d: %s", method, endpoint, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("toolpacks: decode response: %w", err)
	}
	return nil
}

// searchClient returns the caller's client or a package default.
func searchClient(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: httpTimeout}
}

// --- DuckDuckGo ---------------------------------------------------------------

// DDGSearchResult is one hit (Python parity: search.duckduckgo.SearchResult).
type DDGSearchResult struct {
	Title string `json:"title"`
	Href  string `json:"href"`
	Body  string `json:"body"`
}

// DDGSearchResponse is the tool result (Python parity:
// search.duckduckgo.SearchResponse — snake_case JSON wire keys).
type DDGSearchResponse struct {
	Query   string            `json:"query"`
	Results []DDGSearchResult `json:"results"`
}

// DuckDuckGoSearch searches the web through the DuckDuckGo HTML endpoint
// (Python parity: ag2/tools/search/duckduckgo.py DuckDuckGoSearchTool; the ddgs
// SDK is replaced by a direct HTML fetch with a regex parser — the porting
// brief's simplified variant, see docs/DECISIONS.md).
//
// A DuckDuckGoSearch is safe for concurrent use.
type DuckDuckGoSearch struct {
	tool       *tools.FunctionTool
	maxResults int
	region     string
	safesearch string
	baseURL    string
	client     *http.Client
}

var _ tools.Tool = (*DuckDuckGoSearch)(nil)

// DDGOption configures NewDuckDuckGoSearch.
type DDGOption func(*DuckDuckGoSearch) error

// WithDDGMaxResults caps the parsed results (Python parity: max_results, default 5).
func WithDDGMaxResults(n int) DDGOption {
	return func(d *DuckDuckGoSearch) error {
		if n <= 0 {
			return errors.New("toolpacks: ddg max_results must be > 0")
		}
		d.maxResults = n
		return nil
	}
}

// WithDDGRegion sets the search region (Python parity: region, default "us-en").
func WithDDGRegion(region string) DDGOption {
	return func(d *DuckDuckGoSearch) error {
		if region == "" {
			return errors.New("toolpacks: ddg region must not be empty")
		}
		d.region = region
		return nil
	}
}

// WithDDGSafeSearch sets the filter level: "on", "moderate" (default) or "off"
// (Python parity: safesearch).
func WithDDGSafeSearch(level string) DDGOption {
	return func(d *DuckDuckGoSearch) error {
		switch strings.ToLower(level) {
		case "on", "moderate", "off":
			d.safesearch = strings.ToLower(level)
		default:
			return fmt.Errorf("toolpacks: ddg safesearch must be on, moderate or off, got %q", level)
		}
		return nil
	}
}

// WithDDGBaseURL overrides the endpoint origin (default
// https://html.duckduckgo.com) — the test seam for fake servers.
func WithDDGBaseURL(base string) DDGOption {
	return func(d *DuckDuckGoSearch) error {
		if base == "" {
			return errors.New("toolpacks: ddg base URL must not be empty")
		}
		d.baseURL = strings.TrimRight(base, "/")
		return nil
	}
}

// WithDDGHTTPClient overrides the HTTP client (timeouts, transport, tests).
func WithDDGHTTPClient(c *http.Client) DDGOption {
	return func(d *DuckDuckGoSearch) error {
		if c == nil {
			return errors.New("toolpacks: ddg http client must not be nil")
		}
		d.client = c
		return nil
	}
}

// NewDuckDuckGoSearch builds the duckduckgo_search tool.
func NewDuckDuckGoSearch(opts ...DDGOption) (*DuckDuckGoSearch, error) {
	d := &DuckDuckGoSearch{
		maxResults: 5,
		region:     "us-en",
		safesearch: "moderate",
		baseURL:    "https://html.duckduckgo.com",
	}
	for _, opt := range opts {
		if err := opt(d); err != nil {
			return nil, err
		}
	}
	type ddgArgs struct {
		Query string `json:"query" agschema:"description=The search query string,required"`
	}
	fn := func(args ddgArgs) (DDGSearchResponse, error) {
		return d.search(args.Query)
	}
	ft, err := tools.NewFunctionTool("duckduckgo_search",
		"Search the web using DuckDuckGo. Returns titles, URLs, and snippets for each result.",
		fn)
	if err != nil {
		return nil, fmt.Errorf("toolpacks: ddg search: %w", err)
	}
	d.tool = ft
	return d, nil
}

// search fetches the HTML result page and parses it.
func (d *DuckDuckGoSearch) search(query string) (DDGSearchResponse, error) {
	form := url.Values{
		"q":  {query},
		"kl": {d.region},
		"kp": {ddgSafeSearchCode(d.safesearch)},
	}
	resp, err := searchClient(d.client).PostForm(d.baseURL+"/html", form)
	if err != nil {
		return DDGSearchResponse{}, fmt.Errorf("toolpacks: ddg request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return DDGSearchResponse{}, fmt.Errorf("toolpacks: ddg read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return DDGSearchResponse{}, fmt.Errorf("toolpacks: ddg: HTTP %d", resp.StatusCode)
	}
	out := DDGSearchResponse{Query: query, Results: []DDGSearchResult{}}
	snippets := ddgSnippetRe.FindAllStringSubmatch(string(body), -1)
	for i, m := range ddgResultRe.FindAllStringSubmatch(string(body), d.maxResults) {
		bodyText := ""
		if i < len(snippets) {
			bodyText = stripHTML(snippets[i][1])
		}
		out.Results = append(out.Results, DDGSearchResult{
			Title: stripHTML(m[2]),
			Href:  unwrapDDGHref(m[1]),
			Body:  bodyText,
		})
	}
	return out, nil
}

// ddgResultRe matches one result link on the HTML page.
var ddgResultRe = regexp.MustCompile(`(?s)class="result__a"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)

// ddgSnippetRe matches one result snippet; the search method pairs snippets with
// results by index.
var ddgSnippetRe = regexp.MustCompile(`(?s)class="result__snippet"[^>]*>(.*?)</a>`)

// ddgSafeSearchCode maps the level name to the endpoint's kp parameter.
func ddgSafeSearchCode(level string) string {
	switch level {
	case "on":
		return "1"
	case "off":
		return "-2"
	default:
		return "-1"
	}
}

// unwrapDDGHref resolves the redirect wrapper hrefs the HTML endpoint emits
// (//duckduckgo.com/l/?uddg=<encoded>&rut=…) back to the target URL; anything
// else passes through unchanged.
func unwrapDDGHref(href string) string {
	href = html.UnescapeString(href)
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	if u.Query().Get("uddg") == "" {
		return href
	}
	if target, err := url.QueryUnescape(u.Query().Get("uddg")); err == nil {
		return target
	}
	return href
}

// stripHTML removes tags and unescapes entities from a parsed HTML fragment.
func stripHTML(s string) string {
	s = regexp.MustCompile(`(?s)<[^>]*>`).ReplaceAllString(s, "")
	return strings.TrimSpace(html.UnescapeString(s))
}

// Name returns the tool name.
func (d *DuckDuckGoSearch) Name() string { return d.tool.Name() }

// DeclaredInCode reports a program-declared tool (framework contract).
func (d *DuckDuckGoSearch) DeclaredInCode() bool { return d.tool.DeclaredInCode() }

// Schemas declares the single search function.
func (d *DuckDuckGoSearch) Schemas(cx *agocontext.Context) ([]any, error) { return d.tool.Schemas(cx) }

// Register subscribes the search on the conversation stream.
func (d *DuckDuckGoSearch) Register(cx *agocontext.Context, mw []middleware.Middleware) ([]stream.SubID, error) {
	return d.tool.Register(cx, mw)
}

// --- Tavily -------------------------------------------------------------------

// TavilySearchResult is one hit (Python parity: search.tavily.SearchResult).
type TavilySearchResult struct {
	Title   string   `json:"title"`
	URL     string   `json:"url"`
	Content string   `json:"content"`
	Score   *float64 `json:"score,omitempty"`
}

// TavilySearchResponse is the tool result (Python parity: search.tavily.
// SearchResponse).
type TavilySearchResponse struct {
	Query   string               `json:"query"`
	Results []TavilySearchResult `json:"results"`
	Answer  *string              `json:"answer,omitempty"`
	Images  []string             `json:"images,omitempty"`
}

// TavilySearch searches through the Tavily REST API (Python parity:
// ag2/tools/search/tavily.py TavilySearchTool — the parameter subset the porting
// brief carries; per-call Variable overrides of the optional parameters are
// deferred, see docs/DECISIONS.md).
//
// The API key comes from the constructor; a conversation variable named
// "tavily_api_key" overrides it per conversation. The key never appears in the
// tool schema (Python parity: it is not a model-supplied parameter).
type TavilySearch struct {
	tool     *tools.FunctionTool
	apiKey   string
	baseURL  string
	client   *http.Client
	options  map[string]any
	variable bool // override the key from the conversation variable
}

var _ tools.Tool = (*TavilySearch)(nil)

// TavilyOption configures NewTavilySearch.
type TavilyOption func(*TavilySearch) error

// WithTavilyBaseURL overrides the API origin (default https://api.tavily.com) —
// the test seam for fake servers.
func WithTavilyBaseURL(base string) TavilyOption {
	return func(t *TavilySearch) error {
		if base == "" {
			return errors.New("toolpacks: tavily base URL must not be empty")
		}
		t.baseURL = strings.TrimRight(base, "/")
		return nil
	}
}

// WithTavilyHTTPClient overrides the HTTP client.
func WithTavilyHTTPClient(c *http.Client) TavilyOption {
	return func(t *TavilySearch) error {
		if c == nil {
			return errors.New("toolpacks: tavily http client must not be nil")
		}
		t.client = c
		return nil
	}
}

// WithTavilyMaxResults sets the default result count (Python parity:
// max_results, default 5).
func WithTavilyMaxResults(n int) TavilyOption {
	return func(t *TavilySearch) error {
		if n <= 0 {
			return errors.New("toolpacks: tavily max_results must be > 0")
		}
		t.options["max_results"] = n
		return nil
	}
}

// WithTavilySearchDepth sets the depth ("basic" or "advanced"; Python parity:
// search_depth).
func WithTavilySearchDepth(depth string) TavilyOption {
	return func(t *TavilySearch) error {
		if depth != "basic" && depth != "advanced" {
			return fmt.Errorf("toolpacks: tavily search_depth must be basic or advanced, got %q", depth)
		}
		t.options["search_depth"] = depth
		return nil
	}
}

// WithTavilyTopic sets the topic ("general", "news" or "finance"; Python parity:
// topic).
func WithTavilyTopic(topic string) TavilyOption {
	return func(t *TavilySearch) error {
		switch topic {
		case "general", "news", "finance":
		default:
			return fmt.Errorf("toolpacks: tavily topic must be general, news or finance, got %q", topic)
		}
		t.options["topic"] = topic
		return nil
	}
}

// WithTavilyIncludeAnswer asks the API for an LLM-generated answer (Python
// parity: include_answer).
func WithTavilyIncludeAnswer(v bool) TavilyOption {
	return func(t *TavilySearch) error { t.options["include_answer"] = v; return nil }
}

// WithTavilyAPIKeyVariable reads the key from the conversation variable
// "tavily_api_key" instead of the constructor argument (a missing variable is a
// call-time error).
func WithTavilyAPIKeyVariable() TavilyOption {
	return func(t *TavilySearch) error { t.variable = true; return nil }
}

// NewTavilySearch builds the tavily_search tool. An empty apiKey is accepted
// only together with WithTavilyAPIKeyVariable.
func NewTavilySearch(apiKey string, opts ...TavilyOption) (*TavilySearch, error) {
	t := &TavilySearch{
		apiKey:  apiKey,
		baseURL: "https://api.tavily.com",
		options: map[string]any{},
	}
	for _, opt := range opts {
		if err := opt(t); err != nil {
			return nil, err
		}
	}
	if t.apiKey == "" && !t.variable {
		return nil, errors.New("toolpacks: tavily: api key is empty (pass a key or WithTavilyAPIKeyVariable)")
	}
	type tavilyArgs struct {
		Query string `json:"query" agschema:"description=The search query string,required"`
	}
	fn := func(args tavilyArgs, cx *agocontext.Context) (TavilySearchResponse, error) {
		key := t.apiKey
		if t.variable {
			v, ok := cx.Variable("tavily_api_key")
			if !ok {
				return TavilySearchResponse{}, fmt.Errorf("%w: tool %q: variable \"tavily_api_key\" is not provided",
					tools.ErrNotFound, "tavily_search")
			}
			s, ok := v.(string)
			if !ok {
				return TavilySearchResponse{}, fmt.Errorf("toolpacks: tavily: variable \"tavily_api_key\" must be a string, got %T", v)
			}
			key = s
		}
		return t.search(key, args.Query)
	}
	ft, err := tools.NewFunctionTool("tavily_search",
		"Search the web using Tavily. Returns titles, URLs, snippets, relevance scores, "+
			"and optional LLM-generated answer, raw content, and images.",
		fn)
	if err != nil {
		return nil, fmt.Errorf("toolpacks: tavily search: %w", err)
	}
	t.tool = ft
	return t, nil
}

// search performs one API call.
func (t *TavilySearch) search(key, query string) (TavilySearchResponse, error) {
	payload := map[string]any{"query": query}
	for k, v := range t.options {
		payload[k] = v
	}
	var raw struct {
		Query   string  `json:"query"`
		Answer  *string `json:"answer"`
		Results []struct {
			Title   string   `json:"title"`
			URL     string   `json:"url"`
			Content string   `json:"content"`
			Score   *float64 `json:"score"`
		} `json:"results"`
		Images []json.RawMessage `json:"images"`
	}
	if err := doJSON(t.client, http.MethodPost, t.baseURL+"/search",
		map[string]string{"Authorization": "Bearer " + key}, payload, &raw); err != nil {
		return TavilySearchResponse{}, err
	}
	out := TavilySearchResponse{Query: raw.Query, Results: []TavilySearchResult{}, Images: []string{}}
	if out.Query == "" {
		out.Query = query
	}
	out.Answer = raw.Answer
	for _, r := range raw.Results {
		out.Results = append(out.Results, TavilySearchResult(r))
	}
	for _, img := range raw.Images {
		if s, ok := imageRef(img); ok {
			out.Images = append(out.Images, s)
		}
	}
	return out, nil
}

// imageRef coerces an image entry — a bare URL string or an object carrying
// "url" (both shapes occur across API versions) — to its URL.
func imageRef(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	var obj struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.URL != "" {
		return obj.URL, true
	}
	return "", false
}

// Name returns the tool name.
func (t *TavilySearch) Name() string { return t.tool.Name() }

// DeclaredInCode reports a program-declared tool (framework contract).
func (t *TavilySearch) DeclaredInCode() bool { return t.tool.DeclaredInCode() }

// Schemas declares the single search function.
func (t *TavilySearch) Schemas(cx *agocontext.Context) ([]any, error) { return t.tool.Schemas(cx) }

// Register subscribes the search on the conversation stream.
func (t *TavilySearch) Register(cx *agocontext.Context, mw []middleware.Middleware) ([]stream.SubID, error) {
	return t.tool.Register(cx, mw)
}

// --- Perplexity ---------------------------------------------------------------

// PerplexitySearchResult is one hit (Python parity:
// search.perplexity.PerplexitySearchResult).
type PerplexitySearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
	Date    string `json:"date,omitempty"`
}

// PerplexityImage is one image reference (Python parity:
// PerplexityImageMeta).
type PerplexityImage struct {
	ImageURL  string  `json:"image_url"`
	OriginURL *string `json:"origin_url,omitempty"`
	Title     *string `json:"title,omitempty"`
}

// PerplexitySearchResponse is the shared result shape of both tools (Python
// parity: PerplexitySearchResponse).
type PerplexitySearchResponse struct {
	Query     string                   `json:"query"`
	Results   []PerplexitySearchResult `json:"results"`
	Content   *string                  `json:"content,omitempty"`
	Citations []string                 `json:"citations,omitempty"`
	Images    []PerplexityImage        `json:"images,omitempty"`
}

// PerplexityBaseURL is the default API origin.
const PerplexityBaseURL = "https://api.perplexity.ai"

// perplexityCommon carries the shared client wiring of both tools (Python
// parity: PerplexitySearchToolkit — one client, two endpoints).
type perplexityCommon struct {
	apiKey   string
	baseURL  string
	client   *http.Client
	variable bool
}

func (p *perplexityCommon) headers(key string) map[string]string {
	// Python parity: the integration header accompanies every call; there is no
	// library version constant to compose it from, so the package identity is
	// sent verbatim.
	return map[string]string{
		"Authorization":      "Bearer " + key,
		"X-Pplx-Integration": "ag2/ago",
	}
}

func (p *perplexityCommon) resolveKey(cx *agocontext.Context) (string, error) {
	if !p.variable {
		return p.apiKey, nil
	}
	v, ok := cx.Variable("perplexity_api_key")
	if !ok {
		return "", fmt.Errorf("%w: variable \"perplexity_api_key\" is not provided", tools.ErrNotFound)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("toolpacks: perplexity: variable \"perplexity_api_key\" must be a string, got %T", v)
	}
	return s, nil
}

// PerplexitySearchToolkit is the two-tool pack over the Perplexity search APIs
// (Python parity: ag2/tools/search/perplexity.py PerplexitySearchToolkit):
// perplexity_search (raw web search) and perplexity_answer (LLM answer with
// citations). Passing the toolkit to an agent registers both; the individual
// members are reachable through Tools().
type PerplexitySearchToolkit struct {
	common *perplexityCommon
	tk     *tools.Toolkit
}

var _ tools.Tool = (*PerplexitySearchToolkit)(nil)

// PerplexityOption configures NewPerplexitySearch.
type PerplexityOption func(*perplexityCommon) error

// WithPerplexityBaseURL overrides the API origin (default
// https://api.perplexity.ai) — the test seam for fake servers.
func WithPerplexityBaseURL(base string) PerplexityOption {
	return func(p *perplexityCommon) error {
		if base == "" {
			return errors.New("toolpacks: perplexity base URL must not be empty")
		}
		p.baseURL = strings.TrimRight(base, "/")
		return nil
	}
}

// WithPerplexityHTTPClient overrides the HTTP client.
func WithPerplexityHTTPClient(c *http.Client) PerplexityOption {
	return func(p *perplexityCommon) error {
		if c == nil {
			return errors.New("toolpacks: perplexity http client must not be nil")
		}
		p.client = c
		return nil
	}
}

// WithPerplexityAPIKeyVariable reads the key from the conversation variable
// "perplexity_api_key" instead of the constructor argument.
func WithPerplexityAPIKeyVariable() PerplexityOption {
	return func(p *perplexityCommon) error { p.variable = true; return nil }
}

// NewPerplexitySearch builds the perplexity toolkit. An empty apiKey is
// accepted only together with WithPerplexityAPIKeyVariable.
func NewPerplexitySearch(apiKey string, opts ...PerplexityOption) (*PerplexitySearchToolkit, error) {
	common := &perplexityCommon{apiKey: apiKey, baseURL: PerplexityBaseURL}
	for _, opt := range opts {
		if err := opt(common); err != nil {
			return nil, err
		}
	}
	if common.apiKey == "" && !common.variable {
		return nil, errors.New("toolpacks: perplexity: api key is empty (pass a key or WithPerplexityAPIKeyVariable)")
	}
	search, err := newPerplexitySearchTool(common)
	if err != nil {
		return nil, err
	}
	answer, err := newPerplexityAnswerTool(common)
	if err != nil {
		return nil, err
	}
	tk, err := tools.NewToolkit("perplexity_toolkit", nil, search, answer)
	if err != nil {
		return nil, err
	}
	return &PerplexitySearchToolkit{common: common, tk: tk}, nil
}

// newPerplexitySearchTool builds perplexity_search (Search API parity: the
// search() factory).
func newPerplexitySearchTool(p *perplexityCommon) (*tools.FunctionTool, error) {
	type pplxArgs struct {
		Query      string `json:"query" agschema:"description=The search query string,required"`
		MaxResults *int   `json:"max_results" agschema:"description=How many results to return"`
	}
	fn := func(args pplxArgs, cx *agocontext.Context) (PerplexitySearchResponse, error) {
		key, err := p.resolveKey(cx)
		if err != nil {
			return PerplexitySearchResponse{}, err
		}
		payload := map[string]any{"query": args.Query}
		if args.MaxResults != nil {
			payload["max_results"] = *args.MaxResults
		}
		var raw struct {
			Results []struct {
				Title   string  `json:"title"`
				URL     string  `json:"url"`
				Snippet *string `json:"snippet"`
				Date    *string `json:"date"`
			} `json:"results"`
		}
		if err := doJSON(p.client, http.MethodPost, p.baseURL+"/search", p.headers(key), payload, &raw); err != nil {
			return PerplexitySearchResponse{}, err
		}
		out := PerplexitySearchResponse{Query: args.Query, Results: []PerplexitySearchResult{}}
		for _, r := range raw.Results {
			res := PerplexitySearchResult{Title: r.Title, URL: r.URL}
			if r.Snippet != nil {
				res.Snippet = *r.Snippet
			}
			if r.Date != nil {
				res.Date = *r.Date
			}
			out.Results = append(out.Results, res)
		}
		return out, nil
	}
	return tools.NewFunctionTool("perplexity_search",
		"Search the web with the Perplexity Search API. Returns ranked results "+
			"with title, URL, snippet, and date.",
		fn)
}

// newPerplexityAnswerTool builds perplexity_answer (Python parity: the answer()
// factory — Sonar chat completions with web_search_options; the optional
// filters of the Python factory are deferred).
func newPerplexityAnswerTool(p *perplexityCommon) (*tools.FunctionTool, error) {
	type pplxArgs struct {
		Query string `json:"query" agschema:"description=The question to answer,required"`
	}
	fn := func(args pplxArgs, cx *agocontext.Context) (PerplexitySearchResponse, error) {
		key, err := p.resolveKey(cx)
		if err != nil {
			return PerplexitySearchResponse{}, err
		}
		payload := map[string]any{
			"model": "sonar",
			"messages": []map[string]string{
				{"role": "system", "content": "Be precise and concise."},
				{"role": "user", "content": args.Query},
			},
			"max_tokens":         1000,
			"web_search_options": map[string]any{"search_context_size": "high"},
		}
		var raw struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			SearchResults []struct {
				Title   string  `json:"title"`
				URL     string  `json:"url"`
				Snippet *string `json:"snippet"`
				Date    *string `json:"date"`
			} `json:"search_results"`
			Citations []string `json:"citations"`
			Images    []struct {
				ImageURL  string  `json:"image_url"`
				OriginURL *string `json:"origin_url"`
				Title     *string `json:"title"`
			} `json:"images"`
		}
		if err := doJSON(p.client, http.MethodPost, p.baseURL+"/chat/completions", p.headers(key), payload, &raw); err != nil {
			return PerplexitySearchResponse{}, err
		}
		out := PerplexitySearchResponse{Query: args.Query, Results: []PerplexitySearchResult{}}
		if len(raw.Choices) > 0 {
			content := raw.Choices[0].Message.Content
			out.Content = &content
		}
		for _, r := range raw.SearchResults {
			res := PerplexitySearchResult{Title: r.Title, URL: r.URL}
			if r.Snippet != nil {
				res.Snippet = *r.Snippet
			}
			if r.Date != nil {
				res.Date = *r.Date
			}
			out.Results = append(out.Results, res)
		}
		out.Citations = append([]string{}, raw.Citations...)
		for _, img := range raw.Images {
			if img.ImageURL == "" {
				continue
			}
			out.Images = append(out.Images, PerplexityImage{ImageURL: img.ImageURL, OriginURL: img.OriginURL, Title: img.Title})
		}
		return out, nil
	}
	return tools.NewFunctionTool("perplexity_answer",
		"Ask Perplexity AI for an LLM-generated answer with web citations. "+
			"Useful for conversational questions, in-depth research, and analysis. "+
			"Returns content text plus ranked search results and citations.",
		fn)
}

// Name returns the toolkit name.
func (p *PerplexitySearchToolkit) Name() string { return p.tk.Name() }

// DeclaredInCode reports a program-declared tool (framework contract).
func (p *PerplexitySearchToolkit) DeclaredInCode() bool { return p.tk.DeclaredInCode() }

// Tools returns the members: perplexity_search, then perplexity_answer.
func (p *PerplexitySearchToolkit) Tools() []tools.Tool { return p.tk.Tools() }

// Schemas concatenates the members' schemas.
func (p *PerplexitySearchToolkit) Schemas(cx *agocontext.Context) ([]any, error) {
	return p.tk.Schemas(cx)
}

// Register subscribes both tools on the conversation stream.
func (p *PerplexitySearchToolkit) Register(cx *agocontext.Context, mw []middleware.Middleware) ([]stream.SubID, error) {
	return p.tk.Register(cx, mw)
}
