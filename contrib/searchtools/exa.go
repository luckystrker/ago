package searchtools

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/tools"
)

// exaDefaultChars is the text budget the Exa SDK asks for when the caller names
// no content options (exa-py DEFAULT_MAX_CHARACTERS).
const exaDefaultChars = 10000

// exaTimeout is the request bound exa-py gives its async HTTP client (600 s); a
// deep search can run far past the 60 s DefaultTimeout.
const exaTimeout = 600 * time.Second

// apiKey is one provider credential: the constructor value, the name of a
// conversation variable overriding it per call, and the environment fallback the
// Python SDK reads.
type apiCredential struct {
	key      string
	variable string
	envVar   string
}

// resolve returns the key for one call.
func (k apiCredential) resolve(cx *agocontext.Context) (string, error) {
	if k.variable != "" {
		v, ok := cx.Variable(k.variable)
		if !ok {
			return "", errVariableMissing(k.variable)
		}
		s, ok := v.(string)
		if !ok {
			return "", errVariableType(k.variable, v)
		}
		if s == "" {
			return "", errKeyEmpty(k.envVar)
		}
		return s, nil
	}
	if k.key != "" {
		return k.key, nil
	}
	if k.envVar != "" {
		if s := os.Getenv(k.envVar); s != "" {
			return s, nil
		}
	}
	return "", errKeyEmpty(k.envVar)
}

// errKeyEmpty reports a missing credential, naming the environment variable when
// the provider has one.
func errKeyEmpty(envVar string) error {
	if envVar == "" {
		return ErrMissingKey
	}
	return errMissingKeyEnv(envVar)
}

// ExaSearchResult is one hit (Python parity: exa.ExaSearchResult).
type ExaSearchResult struct {
	Title string `json:"title"`
	URL   string `json:"url"`
	// AGO-DEVIATION: absent optional values omit their wire key (omitempty)
	// where Python's dataclass emits an explicit null (same call as Tavily).
	Score         *float64 `json:"score,omitempty"`
	PublishedDate *string  `json:"published_date,omitempty"`
	Author        *string  `json:"author,omitempty"`
	Text          *string  `json:"text,omitempty"`
}

// ExaSearchResponse is the search/answer wrapper (Python parity:
// exa.ExaSearchResponse).
type ExaSearchResponse struct {
	Query   string            `json:"query"`
	Results []ExaSearchResult `json:"results"`
}

// ExaContentResult is one fetched page (Python parity: exa.ExaContentResult).
type ExaContentResult struct {
	URL           string  `json:"url"`
	Title         string  `json:"title"`
	Text          string  `json:"text"`
	Author        *string `json:"author,omitempty"`
	PublishedDate *string `json:"published_date,omitempty"`
}

// ExaAnswerCitation is one source of an answer (Python parity:
// exa.ExaAnswerCitation).
type ExaAnswerCitation struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

// ExaAnswerResult is an AI answer with its sources (Python parity:
// exa.ExaAnswerResult).
type ExaAnswerResult struct {
	Answer    string              `json:"answer"`
	Citations []ExaAnswerCitation `json:"citations"`
}

// ExaToolkit exposes Exa's neural search engine as four tools sharing one
// configuration (Python parity: ag2/extensions/tools/search/exa.py ExaToolkit).
//
// The API key comes from the constructor, the EXA_API_KEY environment variable
// the Python SDK reads, or a conversation variable (WithExaAPIKeyVariable). An
// ExaToolkit is immutable after construction and safe for concurrent use.
type ExaToolkit struct {
	toolbox
	key     apiCredential
	baseURL string
	client  *http.Client
	mw      []middleware.Middleware
	params  *paramSet
}

var _ tools.Tool = (*ExaToolkit)(nil)

// ExaOption configures NewExaToolkit.
type ExaOption func(*ExaToolkit) error

// WithExaBaseURL overrides the API origin (default https://api.exa.ai) — the
// test seam for fake servers.
func WithExaBaseURL(base string) ExaOption {
	return func(t *ExaToolkit) error {
		if base == "" {
			return errEmptyOption("exa", "base URL")
		}
		t.baseURL = strings.TrimRight(base, "/")
		return nil
	}
}

// WithExaHTTPClient overrides the HTTP client.
func WithExaHTTPClient(c *http.Client) ExaOption {
	return func(t *ExaToolkit) error {
		if c == nil {
			return errNilOption("exa", "http client")
		}
		t.client = c
		return nil
	}
}

// WithExaAPIKeyVariable reads the key from the named conversation variable
// instead of the constructor value (a missing variable is a call-time error).
func WithExaAPIKeyVariable(name string) ExaOption {
	return func(t *ExaToolkit) error {
		if name == "" {
			return errEmptyOption("exa", "key variable")
		}
		t.key.variable = name
		return nil
	}
}

// WithExaMiddleware adds tool-scoped middleware to all four members, the Go
// spelling of the Python toolkit's middleware= kwarg.
func WithExaMiddleware(mw ...middleware.Middleware) ExaOption {
	return func(t *ExaToolkit) error {
		if err := checkMiddleware("exa", mw); err != nil {
			return err
		}
		t.mw = append(t.mw, mw...)
		return nil
	}
}

// WithExaNumResults sets the default result count (Python parity: num_results;
// the API default is 10).
func WithExaNumResults(n int) ExaOption {
	return func(t *ExaToolkit) error {
		if n <= 0 {
			return errOptionValue("exa", "num_results", "must be > 0")
		}
		t.params.set("num_results", n)
		return nil
	}
}

// WithExaMaxCharacters caps each result's text (Python parity: max_characters).
func WithExaMaxCharacters(n int) ExaOption {
	return func(t *ExaToolkit) error {
		if n <= 0 {
			return errOptionValue("exa", "max_characters", "must be > 0")
		}
		t.params.set("max_characters", n)
		return nil
	}
}

// WithExaSearchType sets the search algorithm (Python parity: search_type).
func WithExaSearchType(v string) ExaOption {
	return func(t *ExaToolkit) error {
		switch v {
		case "auto", "neural", "fast", "deep-lite", "deep", "deep-reasoning", "instant", "lite":
		default:
			return errOptionValue("exa", "search_type", "is not a known search type")
		}
		t.params.set("search_type", v)
		return nil
	}
}

// WithExaCategory narrows results to one content category (Python parity:
// category).
func WithExaCategory(v string) ExaOption {
	return func(t *ExaToolkit) error {
		switch v {
		case "company", "research paper", "news", "pdf", "personal site", "financial report", "people":
		default:
			return errOptionValue("exa", "category", "is not a known category")
		}
		t.params.set("category", v)
		return nil
	}
}

// WithExaIncludeDomains restricts search to these domains (Python parity:
// include_domains).
func WithExaIncludeDomains(domains ...string) ExaOption {
	return func(t *ExaToolkit) error {
		t.params.set("include_domains", append([]string{}, domains...))
		return nil
	}
}

// WithExaExcludeDomains omits results from these domains (Python parity:
// exclude_domains).
func WithExaExcludeDomains(domains ...string) ExaOption {
	return func(t *ExaToolkit) error {
		t.params.set("exclude_domains", append([]string{}, domains...))
		return nil
	}
}

// WithExaPublishedDates bounds publication dates (Python parity:
// start_published_date / end_published_date, ISO 8601).
func WithExaPublishedDates(start, end string) ExaOption {
	return func(t *ExaToolkit) error {
		if start != "" {
			t.params.set("start_published_date", start)
		}
		if end != "" {
			t.params.set("end_published_date", end)
		}
		return nil
	}
}

// WithExaCrawlDates bounds crawl dates (Python parity: start_crawl_date /
// end_crawl_date, ISO 8601).
func WithExaCrawlDates(start, end string) ExaOption {
	return func(t *ExaToolkit) error {
		if start != "" {
			t.params.set("start_crawl_date", start)
		}
		if end != "" {
			t.params.set("end_crawl_date", end)
		}
		return nil
	}
}

// WithExaLivecrawl selects the live-crawl policy for result text (Python parity:
// livecrawl).
func WithExaLivecrawl(v string) ExaOption {
	return func(t *ExaToolkit) error {
		switch v {
		case "never", "fallback", "always", "preferred":
		default:
			return errOptionValue("exa", "livecrawl", "is not a known livecrawl policy")
		}
		t.params.set("livecrawl", v)
		return nil
	}
}

// WithExaExcludeSourceDomain omits the source domain from find-similar results
// (Python parity: exclude_source_domain).
func WithExaExcludeSourceDomain(v bool) ExaOption {
	return func(t *ExaToolkit) error {
		t.params.set("exclude_source_domain", v)
		return nil
	}
}

// WithExaVariable binds a toolkit default to a conversation variable resolved at
// call time, the Go spelling of the Python factories' `Variable` kwargs. param
// names are the Python factory parameter names ("num_results", "category", ...).
func WithExaVariable(param, variable string) ExaOption {
	return func(t *ExaToolkit) error {
		if param == "" || variable == "" {
			return errOptionValue("exa", "variable", "needs both a parameter and a variable name")
		}
		t.params.bind(param, variable)
		return nil
	}
}

// NewExaToolkit builds the exa_toolkit. An empty apiKey is accepted: the key is
// then read from EXA_API_KEY at call time, or from the key variable.
func NewExaToolkit(apiKey string, opts ...ExaOption) (*ExaToolkit, error) {
	t := &ExaToolkit{
		key:     apiCredential{key: apiKey, envVar: "EXA_API_KEY"},
		baseURL: "https://api.exa.ai",
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
	similar, err := t.FindSimilar()
	if err != nil {
		return nil, err
	}
	contents, err := t.GetContents()
	if err != nil {
		return nil, err
	}
	answer, err := t.Answer()
	if err != nil {
		return nil, err
	}
	tk, err := tools.NewToolkit("exa_toolkit", nil, search, similar, contents, answer)
	if err != nil {
		return nil, err
	}
	t.tk = tk
	return t, nil
}

// clone copies the toolkit with per-factory overrides applied, so a factory
// option never mutates the toolkit defaults.
func (t *ExaToolkit) clone(opts ...ExaOption) (*ExaToolkit, error) {
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

// Search builds the exa_search tool (Python parity: ExaToolkit.search).
func (t *ExaToolkit) Search(opts ...ExaOption) (*tools.FunctionTool, error) {
	c, err := t.clone(opts...)
	if err != nil {
		return nil, err
	}
	type exaSearchArgs struct {
		Query string `json:"query" agschema:"description=The search query string.,required"`
	}
	fn := func(args exaSearchArgs, cx *agocontext.Context) (ExaSearchResponse, error) {
		return c.search(callContext(cx), cx, args.Query)
	}
	return tools.NewFunctionTool("exa_search",
		"Search the web using Exa's neural search engine. "+
			"Returns ranked results with titles, URLs, relevance scores, and optional text content.",
		fn, tools.WithMiddleware(c.mw...))
}

// FindSimilar builds the exa_find_similar tool (Python parity:
// ExaToolkit.find_similar).
func (t *ExaToolkit) FindSimilar(opts ...ExaOption) (*tools.FunctionTool, error) {
	c, err := t.clone(opts...)
	if err != nil {
		return nil, err
	}
	type exaSimilarArgs struct {
		URL string `json:"url" agschema:"description=The URL to find similar pages for.,required"`
	}
	fn := func(args exaSimilarArgs, cx *agocontext.Context) ([]ExaSearchResult, error) {
		return c.findSimilar(callContext(cx), cx, args.URL)
	}
	return tools.NewFunctionTool("exa_find_similar",
		"Find web pages similar to a given URL. Useful for discovering "+
			"related content, competitors, or alternative sources.",
		fn, tools.WithMiddleware(c.mw...))
}

// GetContents builds the exa_get_contents tool (Python parity:
// ExaToolkit.get_contents).
func (t *ExaToolkit) GetContents(opts ...ExaOption) (*tools.FunctionTool, error) {
	c, err := t.clone(opts...)
	if err != nil {
		return nil, err
	}
	type exaContentsArgs struct {
		URLs []string `json:"urls" agschema:"description=URLs to fetch content for.,required"`
	}
	fn := func(args exaContentsArgs, cx *agocontext.Context) ([]ExaContentResult, error) {
		return c.getContents(callContext(cx), cx, args.URLs)
	}
	return tools.NewFunctionTool("exa_get_contents",
		"Get the full text content of specific URLs. Useful when you already "+
			"know which pages you need and want to read them.",
		fn, tools.WithMiddleware(c.mw...))
}

// Answer builds the exa_answer tool (Python parity: ExaToolkit.answer).
func (t *ExaToolkit) Answer(opts ...ExaOption) (*tools.FunctionTool, error) {
	c, err := t.clone(opts...)
	if err != nil {
		return nil, err
	}
	type exaAnswerArgs struct {
		Query string `json:"query" agschema:"description=The question to answer.,required"`
	}
	fn := func(args exaAnswerArgs, cx *agocontext.Context) (ExaAnswerResult, error) {
		return c.answer(callContext(cx), cx, args.Query)
	}
	return tools.NewFunctionTool("exa_answer",
		"Generate an AI-powered answer to a question with citations from web sources.",
		fn, tools.WithMiddleware(c.mw...))
}

// headers composes the Exa request headers. AGO-DEVIATION: exa-py sends its own
// User-Agent; this port identifies itself as ago and keeps the integration tag
// the Python extension adds.
func (t *ExaToolkit) headers(key string) map[string]string {
	return map[string]string{
		"x-api-key":         key,
		"x-exa-integration": "ag2",
		"User-Agent":        "ago-searchtools",
	}
}

// contents composes the `contents` request object: an explicit text budget
// and/or livecrawl policy, or the SDK's default text budget.
func (t *ExaToolkit) contents(cx *agocontext.Context) (map[string]any, error) {
	contents := map[string]any{}
	maxChars, ok, err := t.params.intParam(cx, "max_characters")
	if err != nil {
		return nil, err
	}
	if ok {
		contents["text"] = map[string]any{"maxCharacters": maxChars}
	}
	livecrawl, ok, err := t.params.stringParam(cx, "livecrawl")
	if err != nil {
		return nil, err
	}
	if ok {
		contents["livecrawl"] = livecrawl
	}
	if len(contents) == 0 {
		contents["text"] = map[string]any{"maxCharacters": exaDefaultChars}
	}
	return contents, nil
}

// exaResults decodes the shared `results` array of /search and /findSimilar.
func decodeExaResults(data []byte) ([]ExaSearchResult, error) {
	var raw struct {
		Results []struct {
			Title         *string  `json:"title"`
			URL           string   `json:"url"`
			Score         *float64 `json:"score"`
			PublishedDate *string  `json:"publishedDate"`
			Author        *string  `json:"author"`
			Text          *string  `json:"text"`
		} `json:"results"`
	}
	if err := decodeJSON("exa", data, &raw); err != nil {
		return nil, err
	}
	results := make([]ExaSearchResult, 0, len(raw.Results))
	for _, r := range raw.Results {
		res := ExaSearchResult{
			URL:           r.URL,
			Score:         r.Score,
			PublishedDate: r.PublishedDate,
			Author:        r.Author,
			Text:          r.Text,
		}
		if r.Title != nil {
			res.Title = *r.Title
		}
		results = append(results, res)
	}
	return results, nil
}

// search posts one /search request (Python parity: exa.search).
func (t *ExaToolkit) search(ctx context.Context, cx *agocontext.Context, query string) (ExaSearchResponse, error) {
	key, err := t.key.resolve(cx)
	if err != nil {
		return ExaSearchResponse{}, err
	}
	body := map[string]any{"query": query}
	if err := t.searchFilters(cx, body); err != nil {
		return ExaSearchResponse{}, err
	}
	contents, err := t.contents(cx)
	if err != nil {
		return ExaSearchResponse{}, err
	}
	body["contents"] = contents
	data, err := httpRequest{
		provider: "exa",
		client:   t.client,
		timeout:  exaTimeout,
		method:   http.MethodPost,
		url:      t.baseURL + "/search",
		header:   t.headers(key),
		body:     body,
	}.do(ctx)
	if err != nil {
		return ExaSearchResponse{}, err
	}
	results, err := decodeExaResults(data)
	if err != nil {
		return ExaSearchResponse{}, err
	}
	return ExaSearchResponse{Query: query, Results: results}, nil
}

// searchFilters fills the optional /search filters (Python parity: the kwargs
// dict of ExaToolkit.search, camelCased by the SDK).
func (t *ExaToolkit) searchFilters(cx *agocontext.Context, body map[string]any) error {
	ints := map[string]string{"num_results": "numResults"}
	strings := map[string]string{
		"search_type":          "type",
		"category":             "category",
		"start_published_date": "startPublishedDate",
		"end_published_date":   "endPublishedDate",
		"start_crawl_date":     "startCrawlDate",
		"end_crawl_date":       "endCrawlDate",
	}
	for paramName, wire := range ints {
		v, ok, err := t.params.intParam(cx, paramName)
		if err != nil {
			return err
		}
		if ok {
			body[wire] = v
		}
	}
	for paramName, wire := range strings {
		v, ok, err := t.params.stringParam(cx, paramName)
		if err != nil {
			return err
		}
		if ok {
			body[wire] = v
		}
	}
	lists := map[string]string{"include_domains": "includeDomains", "exclude_domains": "excludeDomains"}
	for paramName, wire := range lists {
		v, ok, err := t.params.stringsParam(cx, paramName)
		if err != nil {
			return err
		}
		if ok {
			body[wire] = v
		}
	}
	return nil
}

// findSimilar posts one /findSimilar request (Python parity:
// exa.find_similar).
func (t *ExaToolkit) findSimilar(ctx context.Context, cx *agocontext.Context, target string) ([]ExaSearchResult, error) {
	key, err := t.key.resolve(cx)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"url": target}
	for paramName, wire := range map[string]string{"num_results": "numResults"} {
		v, ok, err := t.params.intParam(cx, paramName)
		if err != nil {
			return nil, err
		}
		if ok {
			body[wire] = v
		}
	}
	for paramName, wire := range map[string]string{
		"include_domains": "includeDomains",
		"exclude_domains": "excludeDomains",
	} {
		v, ok, err := t.params.stringsParam(cx, paramName)
		if err != nil {
			return nil, err
		}
		if ok {
			body[wire] = v
		}
	}
	if v, ok, err := t.params.boolParam(cx, "exclude_source_domain"); err != nil {
		return nil, err
	} else if ok {
		body["excludeSourceDomain"] = v
	}
	// Python parity: the SDK fills in the default contents when the caller names
	// none, and the extension never names any.
	body["contents"] = map[string]any{
		"text": map[string]any{"maxCharacters": exaDefaultChars},
	}
	data, err := httpRequest{
		provider: "exa",
		client:   t.client,
		timeout:  exaTimeout,
		method:   http.MethodPost,
		url:      t.baseURL + "/findSimilar",
		header:   t.headers(key),
		body:     body,
	}.do(ctx)
	if err != nil {
		return nil, err
	}
	return decodeExaResults(data)
}

// getContents posts one /contents request (Python parity: exa.get_contents).
func (t *ExaToolkit) getContents(ctx context.Context, cx *agocontext.Context, urls []string) ([]ExaContentResult, error) {
	key, err := t.key.resolve(cx)
	if err != nil {
		return nil, err
	}
	data, err := httpRequest{
		provider: "exa",
		client:   t.client,
		timeout:  exaTimeout,
		method:   http.MethodPost,
		url:      t.baseURL + "/contents",
		header:   t.headers(key),
		body:     map[string]any{"urls": urls, "text": true},
	}.do(ctx)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Results []struct {
			URL           string  `json:"url"`
			Title         *string `json:"title"`
			Text          *string `json:"text"`
			Author        *string `json:"author"`
			PublishedDate *string `json:"publishedDate"`
		} `json:"results"`
	}
	if err := decodeJSON("exa", data, &raw); err != nil {
		return nil, err
	}
	results := make([]ExaContentResult, 0, len(raw.Results))
	for _, r := range raw.Results {
		res := ExaContentResult{URL: r.URL, Author: r.Author, PublishedDate: r.PublishedDate}
		if r.Title != nil {
			res.Title = *r.Title
		}
		if r.Text != nil {
			res.Text = *r.Text
		}
		results = append(results, res)
	}
	return results, nil
}

// answer posts one /answer request (Python parity: exa.answer).
func (t *ExaToolkit) answer(ctx context.Context, cx *agocontext.Context, query string) (ExaAnswerResult, error) {
	key, err := t.key.resolve(cx)
	if err != nil {
		return ExaAnswerResult{}, err
	}
	data, err := httpRequest{
		provider: "exa",
		client:   t.client,
		timeout:  exaTimeout,
		method:   http.MethodPost,
		url:      t.baseURL + "/answer",
		header:   t.headers(key),
		// exa-py's answer() keeps its stream=False default in the options dict, so the
		// SDK body always carries "stream":false next to the query and text flag.
		body: map[string]any{"query": query, "stream": false, "text": true},
	}.do(ctx)
	if err != nil {
		return ExaAnswerResult{}, err
	}
	var raw struct {
		Answer    string `json:"answer"`
		Citations []struct {
			URL   string  `json:"url"`
			Title *string `json:"title"`
			Text  *string `json:"text"`
		} `json:"citations"`
	}
	if err := decodeJSON("exa", data, &raw); err != nil {
		return ExaAnswerResult{}, err
	}
	out := ExaAnswerResult{Answer: raw.Answer, Citations: []ExaAnswerCitation{}}
	for _, c := range raw.Citations {
		cit := ExaAnswerCitation{URL: c.URL}
		if c.Title != nil {
			cit.Title = *c.Title
		}
		if c.Text != nil {
			cit.Text = *c.Text
		}
		out.Citations = append(out.Citations, cit)
	}
	return out, nil
}
