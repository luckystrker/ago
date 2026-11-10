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

// XquikTweetSearchResponse is one page of X posts (Python parity:
// xquik.XquikTweetSearchResponse). Tweets keep the provider's own record shape.
type XquikTweetSearchResponse struct {
	Query       string `json:"query"`
	Tweets      []any  `json:"tweets"`
	HasNextPage bool   `json:"has_next_page"`
	NextCursor  string `json:"next_cursor"`
}

// XquikSearchToolkit searches public X posts through the Xquik REST API
// (Python parity: ag2/extensions/tools/search/xquik.py XquikSearchToolkit).
// Passing the toolkit to an agent registers xquik_tweet_search.
//
// A key is required. An XquikSearchToolkit is immutable after construction and
// safe for concurrent use.
type XquikSearchToolkit struct {
	toolbox
	key     apiCredential
	baseURL string
	client  *http.Client
	mw      []middleware.Middleware
	params  *paramSet
}

var _ tools.Tool = (*XquikSearchToolkit)(nil)

// XquikOption configures NewXquikSearchToolkit.
type XquikOption func(*XquikSearchToolkit) error

// WithXquikBaseURL overrides the API origin (default https://xquik.com).
func WithXquikBaseURL(base string) XquikOption {
	return func(t *XquikSearchToolkit) error {
		if base == "" {
			return errEmptyOption("xquik", "base URL")
		}
		t.baseURL = strings.TrimRight(base, "/")
		return nil
	}
}

// WithXquikHTTPClient overrides the HTTP client.
func WithXquikHTTPClient(c *http.Client) XquikOption {
	return func(t *XquikSearchToolkit) error {
		if c == nil {
			return errNilOption("xquik", "http client")
		}
		t.client = c
		return nil
	}
}

// WithXquikAPIKeyVariable reads the key from the named conversation variable
// instead of the constructor value.
func WithXquikAPIKeyVariable(name string) XquikOption {
	return func(t *XquikSearchToolkit) error {
		if name == "" {
			return errEmptyOption("xquik", "key variable")
		}
		t.key.variable = name
		return nil
	}
}

// WithXquikMiddleware adds tool-scoped middleware.
func WithXquikMiddleware(mw ...middleware.Middleware) XquikOption {
	return func(t *XquikSearchToolkit) error {
		if err := checkMiddleware("xquik", mw); err != nil {
			return err
		}
		t.mw = append(t.mw, mw...)
		return nil
	}
}

// WithXquikQueryType fixes the result ordering ("Latest" or "Top"; Python
// parity: query_type).
func WithXquikQueryType(v string) XquikOption {
	return func(t *XquikSearchToolkit) error {
		if v != "Latest" && v != "Top" {
			return errOptionValue("xquik", "query_type", `must be "Latest" or "Top"`)
		}
		t.params.set("query_type", v)
		return nil
	}
}

// WithXquikCursor continues from a previous page (Python parity: cursor).
func WithXquikCursor(v string) XquikOption {
	return func(t *XquikSearchToolkit) error {
		t.params.set("cursor", v)
		return nil
	}
}

// WithXquikTimeRange bounds post times (Python parity: since_time / until_time).
func WithXquikTimeRange(since, until string) XquikOption {
	return func(t *XquikSearchToolkit) error {
		if since != "" {
			t.params.set("since_time", since)
		}
		if until != "" {
			t.params.set("until_time", until)
		}
		return nil
	}
}

// WithXquikLimit caps the page size (Python parity: limit).
func WithXquikLimit(n int) XquikOption {
	return func(t *XquikSearchToolkit) error {
		if n <= 0 {
			return errOptionValue("xquik", "limit", "must be > 0")
		}
		t.params.set("limit", n)
		return nil
	}
}

// WithXquikVariable binds a default to a conversation variable.
func WithXquikVariable(param, variable string) XquikOption {
	return func(t *XquikSearchToolkit) error {
		if param == "" || variable == "" {
			return errOptionValue("xquik", "variable", "needs both a parameter and a variable name")
		}
		t.params.bind(param, variable)
		return nil
	}
}

// NewXquikSearchToolkit builds the xquik_search_toolkit (Python parity: it
// raises when api_key is empty).
func NewXquikSearchToolkit(apiKey string, opts ...XquikOption) (*XquikSearchToolkit, error) {
	t := &XquikSearchToolkit{
		key:     apiCredential{key: apiKey},
		baseURL: "https://xquik.com",
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
	tool, err := t.Search()
	if err != nil {
		return nil, err
	}
	tk, err := tools.NewToolkit("xquik_search_toolkit", nil, tool)
	if err != nil {
		return nil, err
	}
	t.tk = tk
	return t, nil
}

// Search builds the xquik_tweet_search tool (Python parity:
// XquikSearchToolkit.search).
func (t *XquikSearchToolkit) Search(opts ...XquikOption) (*tools.FunctionTool, error) {
	c := *t
	c.params = t.params.clone()
	c.mw = append([]middleware.Middleware{}, t.mw...)
	for _, opt := range opts {
		if err := opt(&c); err != nil {
			return nil, err
		}
	}
	type xquikArgs struct {
		Query string `json:"query" agschema:"description=The X search query string.,required"`
	}
	fn := func(args xquikArgs, cx *agocontext.Context) (XquikTweetSearchResponse, error) {
		return c.search(callContext(cx), cx, args.Query)
	}
	return tools.NewFunctionTool("xquik_tweet_search",
		"Search public X posts through Xquik. Returns tweet records and pagination metadata.",
		fn, tools.WithMiddleware(c.mw...))
}

// search runs one /api/v1/x/tweets/search request.
func (t *XquikSearchToolkit) search(ctx context.Context, cx *agocontext.Context, query string) (XquikTweetSearchResponse, error) {
	key, err := t.key.resolve(cx)
	if err != nil {
		return XquikTweetSearchResponse{}, err
	}
	params := url.Values{"q": {query}}
	wire := map[string]string{
		"query_type": "queryType",
		"cursor":     "cursor",
		"since_time": "sinceTime",
		"until_time": "untilTime",
	}
	for paramName, name := range wire {
		v, ok, err := t.params.stringParam(cx, paramName)
		if err != nil {
			return XquikTweetSearchResponse{}, err
		}
		if ok {
			params.Set(name, v)
		}
	}
	if limit, ok, err := t.params.intParam(cx, "limit"); err != nil {
		return XquikTweetSearchResponse{}, err
	} else if ok {
		params.Set("limit", strconv.Itoa(limit))
	}
	data, err := httpRequest{
		provider: "xquik",
		client:   t.client,
		method:   http.MethodGet,
		url:      t.baseURL + "/api/v1/x/tweets/search",
		query:    params,
		header:   map[string]string{"x-api-key": key},
	}.do(ctx)
	if err != nil {
		return XquikTweetSearchResponse{}, err
	}
	var raw struct {
		Tweets any `json:"tweets"`
		// Python parity: `raw.get("has_next_page") is True`, so only a real JSON
		// boolean counts.
		HasNextPage any `json:"has_next_page"`
		NextCursor  any `json:"next_cursor"`
	}
	if err := decodeJSON("xquik", data, &raw); err != nil {
		return XquikTweetSearchResponse{}, err
	}
	out := XquikTweetSearchResponse{
		Query:  query,
		Tweets: []any{},
	}
	if has, ok := raw.HasNextPage.(bool); ok {
		out.HasNextPage = has
	}
	if list, ok := raw.Tweets.([]any); ok {
		out.Tweets = list
	}
	if cursor, ok := raw.NextCursor.(string); ok {
		out.NextCursor = cursor
	}
	return out, nil
}
