package searchtools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/tools"
)

// glasserDefaultMaxCharacters caps the content fields of one result (Python
// parity: max_characters default 10000).
const glasserDefaultMaxCharacters = 10000

// glasserUserAgent identifies the port to Glasser (Python parity:
// "ag2-glasser-extension").
const glasserUserAgent = "ag2-glasser-extension"

// GlasserResult is one Glasser outcome, with content bounded separately from
// billing and retrieval fields (Python parity: glasser.GlasserResult).
type GlasserResult struct {
	ID           *string `json:"id,omitempty"`
	Status       *string `json:"status,omitempty"`
	ResultStatus *string `json:"result_status,omitempty"`
	Provider     *string `json:"provider,omitempty"`
	ChargeUSD    *string `json:"charge_usd,omitempty"`
	Output       any     `json:"output,omitempty"`
	Error        any     `json:"error,omitempty"`
	Failure      any     `json:"failure,omitempty"`
	ProviderResp any     `json:"provider_response,omitempty"`
	Runs         any     `json:"runs,omitempty"`
	RunURL       *string `json:"run_url,omitempty"`
	ResultURL    *string `json:"result_url,omitempty"`
	Truncated    bool    `json:"truncated"`
}

// GlasserToolkit exposes six narrow tools over Glasser's GTM Solution API
// (Python parity: ag2/extensions/tools/search/glasser.py GlasserToolkit).
//
// Every call spends money (pay-per-call billing), so enabling it is an owner
// decision. A GlasserToolkit is immutable after construction and safe for
// concurrent use.
type GlasserToolkit struct {
	toolbox
	key          apiCredential
	baseURL      string // with a trailing slash, Python parity
	client       *http.Client
	mw           []middleware.Middleware
	params       *paramSet
	waitTimeout  time.Duration
	pollInterval time.Duration
}

var _ tools.Tool = (*GlasserToolkit)(nil)

// GlasserOption configures NewGlasserToolkit.
type GlasserOption func(*GlasserToolkit) error

// WithGlasserBaseURL overrides the API root (default https://api.glasser.ai).
func WithGlasserBaseURL(base string) GlasserOption {
	return func(t *GlasserToolkit) error {
		u, err := url.Parse(base)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errOptionValue("glasser", "base_url",
				"must be an HTTP(S) API root without credentials, query or fragment")
		}
		t.baseURL = strings.TrimRight(base, "/") + "/"
		return nil
	}
}

// WithGlasserHTTPClient overrides the HTTP client.
func WithGlasserHTTPClient(c *http.Client) GlasserOption {
	return func(t *GlasserToolkit) error {
		if c == nil {
			return errNilOption("glasser", "http client")
		}
		t.client = c
		return nil
	}
}

// WithGlasserAPIKeyVariable reads the key from the named conversation variable.
func WithGlasserAPIKeyVariable(name string) GlasserOption {
	return func(t *GlasserToolkit) error {
		if name == "" {
			return errEmptyOption("glasser", "key variable")
		}
		t.key.variable = name
		return nil
	}
}

// WithGlasserMiddleware adds tool-scoped middleware to all six members.
func WithGlasserMiddleware(mw ...middleware.Middleware) GlasserOption {
	return func(t *GlasserToolkit) error {
		if err := checkMiddleware("glasser", mw); err != nil {
			return err
		}
		t.mw = append(t.mw, mw...)
		return nil
	}
}

// WithGlasserTimeout bounds one HTTP request (Python parity: timeout, seconds).
func WithGlasserTimeout(d time.Duration) GlasserOption {
	return func(t *GlasserToolkit) error {
		if d <= 0 {
			return errOptionValue("glasser", "timeout", "must be finite and positive")
		}
		t.client = &http.Client{Timeout: d, CheckRedirect: noRedirect}
		return nil
	}
}

// WithGlasserWaitTimeout bounds the polling wait (Python parity: wait_timeout;
// zero disables polling).
func WithGlasserWaitTimeout(d time.Duration) GlasserOption {
	return func(t *GlasserToolkit) error {
		if d < 0 {
			return errOptionValue("glasser", "wait_timeout", "must be finite and non-negative")
		}
		t.waitTimeout = d
		return nil
	}
}

// WithGlasserPollInterval sets the delay between polls (Python parity:
// poll_interval, default one second).
func WithGlasserPollInterval(d time.Duration) GlasserOption {
	return func(t *GlasserToolkit) error {
		if d <= 0 {
			return errOptionValue("glasser", "poll_interval", "must be finite and positive")
		}
		t.pollInterval = d
		return nil
	}
}

// WithGlasserProvider fixes the upstream provider (Python parity: provider).
func WithGlasserProvider(v string) GlasserOption {
	return func(t *GlasserToolkit) error {
		t.params.set("provider", v)
		return nil
	}
}

// WithGlasserLimit caps rows per result (Python parity: limit).
func WithGlasserLimit(n int) GlasserOption {
	return func(t *GlasserToolkit) error {
		if n <= 0 {
			return errOptionValue("glasser", "limit", "must be > 0")
		}
		t.params.set("limit", n)
		return nil
	}
}

// WithGlasserCountry sets the country hint (Python parity: country).
func WithGlasserCountry(v string) GlasserOption {
	return func(t *GlasserToolkit) error {
		t.params.set("country", v)
		return nil
	}
}

// WithGlasserTaskID attaches a caller task id (Python parity: task_id).
func WithGlasserTaskID(v string) GlasserOption {
	return func(t *GlasserToolkit) error {
		t.params.set("task_id", v)
		return nil
	}
}

// WithGlasserMetadata attaches caller metadata (Python parity: metadata).
func WithGlasserMetadata(m map[string]string) GlasserOption {
	return func(t *GlasserToolkit) error {
		copied := map[string]string{}
		for k, v := range m {
			copied[k] = v
		}
		t.params.set("metadata", copied)
		return nil
	}
}

// WithGlasserMaxCharacters bounds the content fields of one result (Python
// parity: max_characters, default 10000).
func WithGlasserMaxCharacters(n int) GlasserOption {
	return func(t *GlasserToolkit) error {
		if n < 0 {
			return errOptionValue("glasser", "max_characters", "must be non-negative")
		}
		t.params.set("max_characters", n)
		return nil
	}
}

// WithGlasserVariable binds a default to a conversation variable.
func WithGlasserVariable(param, variable string) GlasserOption {
	return func(t *GlasserToolkit) error {
		if param == "" || variable == "" {
			return errOptionValue("glasser", "variable", "needs both a parameter and a variable name")
		}
		t.params.bind(param, variable)
		return nil
	}
}

// noRedirect keeps every response, including a 3xx, in the caller's hands
// (Python parity: follow_redirects=False).
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// NewGlasserToolkit builds the glasser_toolkit (Python parity: it raises when
// api_key is blank).
func NewGlasserToolkit(apiKey string, opts ...GlasserOption) (*GlasserToolkit, error) {
	t := &GlasserToolkit{
		key:          apiCredential{key: strings.TrimSpace(apiKey)},
		baseURL:      "https://api.glasser.ai/",
		client:       &http.Client{Timeout: DefaultTimeout, CheckRedirect: noRedirect},
		params:       newParamSet(),
		waitTimeout:  120 * time.Second,
		pollInterval: time.Second,
	}
	t.params.set("max_characters", glasserDefaultMaxCharacters)
	for _, opt := range opts {
		if err := opt(t); err != nil {
			return nil, err
		}
	}
	if t.key.key == "" && t.key.variable == "" {
		return nil, ErrMissingKey
	}
	members := make([]tools.Tool, 0, 6)
	for _, factory := range []func(...GlasserOption) (*tools.FunctionTool, error){
		t.WebSearch, t.CompanyProfile, t.FindPeople, t.KeywordOverview, t.SocialSearch, t.StockQuote,
	} {
		member, err := factory()
		if err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	tk, err := tools.NewToolkit("glasser_toolkit", nil, members...)
	if err != nil {
		return nil, err
	}
	t.tk = tk
	return t, nil
}

// clone copies the toolkit with per-factory overrides applied.
func (t *GlasserToolkit) clone(opts ...GlasserOption) (*GlasserToolkit, error) {
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

// WebSearch builds the glasser_web_search tool (Python parity:
// GlasserToolkit.web_search).
func (t *GlasserToolkit) WebSearch(opts ...GlasserOption) (*tools.FunctionTool, error) {
	c, err := t.clone(opts...)
	if err != nil {
		return nil, err
	}
	type glasserQueryArgs struct {
		Query string `json:"query" agschema:"description=The web search query string.,required"`
	}
	fn := func(args glasserQueryArgs, cx *agocontext.Context) (any, error) {
		if strings.TrimSpace(args.Query) == "" {
			return glasserRejected("query must not be empty"), nil
		}
		return c.execute(callContext(cx), cx, "web_research", map[string]any{
			"action": "search",
			"query":  args.Query,
		}, "provider", "limit", "country", "task_id", "metadata")
	}
	return tools.NewFunctionTool("glasser_web_search", "Search the web through Glasser.",
		fn, tools.WithMiddleware(c.mw...))
}

// CompanyProfile builds the glasser_company_profile tool (Python parity:
// GlasserToolkit.company_profile).
func (t *GlasserToolkit) CompanyProfile(opts ...GlasserOption) (*tools.FunctionTool, error) {
	c, err := t.clone(opts...)
	if err != nil {
		return nil, err
	}
	type glasserDomainArgs struct {
		Domain string `json:"domain" agschema:"description=The company website domain.,required"`
	}
	fn := func(args glasserDomainArgs, cx *agocontext.Context) (any, error) {
		if strings.TrimSpace(args.Domain) == "" {
			return glasserRejected("domain must not be empty"), nil
		}
		return c.execute(callContext(cx), cx, "company_intelligence", map[string]any{
			"action": "enrich",
			"domain": args.Domain,
		}, "provider", "limit", "country", "task_id", "metadata")
	}
	return tools.NewFunctionTool("glasser_company_profile",
		"Look up a company profile by domain through Glasser.",
		fn, tools.WithMiddleware(c.mw...))
}

// FindPeople builds the glasser_find_people tool (Python parity:
// GlasserToolkit.find_people).
func (t *GlasserToolkit) FindPeople(opts ...GlasserOption) (*tools.FunctionTool, error) {
	c, err := t.clone(opts...)
	if err != nil {
		return nil, err
	}
	type glasserPeopleArgs struct {
		JobTitles     []string `json:"job_titles" agschema:"description=Job titles to match."`
		Locations     []string `json:"locations" agschema:"description=Locations to match."`
		CompanyDomain *string  `json:"company_domain" agschema:"description=The company website domain."`
		Keywords      *string  `json:"keywords" agschema:"description=Keywords to match."`
	}
	fn := func(args glasserPeopleArgs, cx *agocontext.Context) (any, error) {
		if !anyNonBlank(args.JobTitles) && !anyNonBlank(args.Locations) &&
			!nonBlank(args.CompanyDomain) && !nonBlank(args.Keywords) {
			return glasserRejected("supply job_titles, locations, company_domain or keywords"), nil
		}
		body := map[string]any{"action": "search"}
		if args.JobTitles != nil {
			body["job_titles"] = args.JobTitles
		}
		if args.Locations != nil {
			body["locations"] = args.Locations
		}
		if args.CompanyDomain != nil {
			body["company_domain"] = *args.CompanyDomain
		}
		if args.Keywords != nil {
			body["keywords"] = *args.Keywords
		}
		return c.execute(callContext(cx), cx, "people_search", body,
			"provider", "limit", "task_id", "metadata")
	}
	return tools.NewFunctionTool("glasser_find_people",
		"Find people by job, location or company through Glasser.",
		fn, tools.WithMiddleware(c.mw...))
}

// KeywordOverview builds the glasser_keyword_overview tool (Python parity:
// GlasserToolkit.keyword_overview).
func (t *GlasserToolkit) KeywordOverview(opts ...GlasserOption) (*tools.FunctionTool, error) {
	c, err := t.clone(opts...)
	if err != nil {
		return nil, err
	}
	type glasserKeywordArgs struct {
		Keywords []string `json:"keywords" agschema:"description=Keywords to research.,required"`
	}
	fn := func(args glasserKeywordArgs, cx *agocontext.Context) (any, error) {
		if !anyNonBlank(args.Keywords) {
			return glasserRejected("keywords must contain a non-empty value"), nil
		}
		return c.execute(callContext(cx), cx, "seo_research", map[string]any{
			"action":   "keyword_overview",
			"keywords": args.Keywords,
		}, "provider", "limit", "country", "task_id", "metadata")
	}
	return tools.NewFunctionTool("glasser_keyword_overview",
		"Research SEO keywords through Glasser.",
		fn, tools.WithMiddleware(c.mw...))
}

// SocialSearch builds the glasser_social_search tool (Python parity:
// GlasserToolkit.social_search).
func (t *GlasserToolkit) SocialSearch(opts ...GlasserOption) (*tools.FunctionTool, error) {
	c, err := t.clone(opts...)
	if err != nil {
		return nil, err
	}
	type glasserSocialArgs struct {
		Platform string `json:"platform" agschema:"description=The social platform.,required,enum=reddit|x|youtube|tiktok|instagram|linkedin"`
		Query    string `json:"query" agschema:"description=The post search query.,required"`
	}
	fn := func(args glasserSocialArgs, cx *agocontext.Context) (any, error) {
		switch args.Platform {
		case "reddit", "x", "youtube", "tiktok", "instagram", "linkedin":
		default:
			return glasserRejected("platform must be one of reddit, x, youtube, tiktok, instagram, linkedin"), nil
		}
		if strings.TrimSpace(args.Query) == "" {
			return glasserRejected("query must not be empty"), nil
		}
		return c.execute(callContext(cx), cx, "social_research", map[string]any{
			"mode":     "search",
			"platform": args.Platform,
			"query":    args.Query,
		}, "provider", "metadata")
	}
	return tools.NewFunctionTool("glasser_social_search",
		"Search public social posts through Glasser.",
		fn, tools.WithMiddleware(c.mw...))
}

// StockQuote builds the glasser_stock_quote tool (Python parity:
// GlasserToolkit.stock_quote).
func (t *GlasserToolkit) StockQuote(opts ...GlasserOption) (*tools.FunctionTool, error) {
	c, err := t.clone(opts...)
	if err != nil {
		return nil, err
	}
	type glasserSymbolArgs struct {
		Symbol string `json:"symbol" agschema:"description=The stock ticker symbol.,required"`
	}
	fn := func(args glasserSymbolArgs, cx *agocontext.Context) (any, error) {
		if strings.TrimSpace(args.Symbol) == "" {
			return glasserRejected("symbol must not be empty"), nil
		}
		return c.execute(callContext(cx), cx, "market_data", map[string]any{
			"action": "stock_quote",
			"symbol": args.Symbol,
		}, "provider", "task_id", "metadata")
	}
	return tools.NewFunctionTool("glasser_stock_quote",
		"Get a stock quote through Glasser.",
		fn, tools.WithMiddleware(c.mw...))
}

// glasserRejected is the model-facing error object of the input guards.
func glasserRejected(message string) map[string]any {
	return map[string]any{"error": message}
}

// nonBlank reports a set, non-blank value.
func nonBlank(v *string) bool { return v != nil && strings.TrimSpace(*v) != "" }

// anyNonBlank reports at least one non-blank entry.
func anyNonBlank(values []string) bool {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

// headers composes the Glasser request headers.
func (t *GlasserToolkit) headers(key, idempotencyKey string) map[string]string {
	header := map[string]string{
		"Authorization": "Bearer " + key,
		"User-Agent":    glasserUserAgent,
	}
	if idempotencyKey != "" {
		header["Idempotency-Key"] = idempotencyKey
	}
	return header
}

// idempotencyKey scopes an LLM call ID to its stream, because different
// conversations can reuse call IDs (Python parity: uuid5 over
// "ag2-glasser:<stream>:<call>:<capability>", uuid4 when there is no call id).
func idempotencyKey(cx *agocontext.Context, capability string) (string, error) {
	if cx == nil || cx.ToolCallID == "" {
		return uuid4()
	}
	streamID := ""
	if cx.Stream != nil {
		streamID = cx.Stream.ID()
	}
	return uuid5(namespaceURL, fmt.Sprintf("ag2-glasser:%s:%s:%s", streamID, cx.ToolCallID, capability)), nil
}

// execute runs one capability call, polling a pending result until the wait
// timeout (Python parity: _GlasserClient.execute).
func (t *GlasserToolkit) execute(
	ctx context.Context,
	cx *agocontext.Context,
	capability string,
	body map[string]any,
	optional ...string,
) (any, error) {
	key, err := t.key.resolve(cx)
	if err != nil {
		return nil, err
	}
	maxChars := glasserDefaultMaxCharacters
	if v, ok, err := t.params.intParam(cx, "max_characters"); err != nil {
		return nil, err
	} else if ok {
		maxChars = v
	}
	for _, name := range optional {
		v, ok, err := t.params.get(cx, name)
		if err != nil {
			return nil, err
		}
		if ok {
			body[name] = v
		}
	}
	idKey, err := idempotencyKey(cx, capability)
	if err != nil {
		return nil, err
	}
	path := "v1/solutions/gtm/" + capability
	raw, err := t.object(ctx, http.MethodPost, t.baseURL+path, t.headers(key, idKey), body)
	if err != nil {
		return nil, err
	}
	if !glasserPending(raw) || t.waitTimeout == 0 {
		return glasserFromAPI(raw, maxChars), nil
	}
	pollURL, err := glasserPollURL(t.baseURL, path, capability, raw)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(t.waitTimeout)
	for glasserPending(raw) && time.Now().Before(deadline) {
		if err := sleepContext(ctx, t.pollInterval); err != nil {
			break
		}
		next, err := t.object(ctx, http.MethodGet, pollURL, t.headers(key, ""), nil)
		if err != nil {
			// Python parity: a failed poll keeps the last pending result and its
			// retrieval information.
			break
		}
		raw = next
	}
	return glasserFromAPI(raw, maxChars), nil
}

// object runs one request and decodes a JSON object, rejecting a non-object
// payload (Python parity: glasser._object).
func (t *GlasserToolkit) object(ctx context.Context, method, endpoint string, header map[string]string, body map[string]any) (map[string]any, error) {
	data, err := httpRequest{
		provider: "glasser",
		client:   t.client,
		method:   method,
		url:      endpoint,
		header:   header,
		body:     body,
	}.do(ctx)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%w: glasser: returned a non-object JSON response", ErrAPIResponse)
	}
	return raw, nil
}

// sleepContext waits for d unless the call context ends first.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// glasserPending reports whether a result is still being produced.
func glasserPending(raw map[string]any) bool {
	if status, _ := raw["status"].(string); status == "QUEUED" || status == "RUNNING" {
		return true
	}
	status, _ := raw["result_status"].(string)
	return status == "pending"
}

// glasserPollURL resolves where to poll a pending result (Python parity:
// glasser._poll_url). A relative result_url is resolved against the API base,
// exactly as httpx.URL.join does, and the outcome must point at this capability
// on the configured origin: an answer that names another host is refused, so a
// hostile payload cannot redirect the paid poll.
func glasserPollURL(base, path, capability string, raw map[string]any) (string, error) {
	if capability == "people_search" {
		id, _ := raw["id"].(string)
		if id == "" || !validRunID(id) {
			return "", fmt.Errorf("%w: glasser: returned an invalid pending Run id", ErrAPIResponse)
		}
		return base + "v1/runs/" + id, nil
	}
	resultURL, _ := raw["result_url"].(string)
	if resultURL == "" {
		return "", fmt.Errorf("%w: glasser: returned no result_url for a pending result", ErrAPIResponse)
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("%w: glasser: invalid API base URL", ErrAPIResponse)
	}
	rel, err := url.Parse(resultURL)
	if err != nil {
		return "", fmt.Errorf("%w: glasser: invalid result_url", ErrAPIResponse)
	}
	polled := baseURL.ResolveReference(rel)
	expected := baseURL.ResolveReference(&url.URL{Path: path + "/result"})
	if polled.Scheme != expected.Scheme || polled.Hostname() != expected.Hostname() ||
		effectivePort(polled) != effectivePort(expected) ||
		polled.Path != expected.Path || polled.User != nil || polled.Fragment != "" {
		return "", fmt.Errorf("%w: glasser: result_url must point to this capability on the configured API origin", ErrAPIResponse)
	}
	return polled.String(), nil
}

// effectivePort returns the URL's port, filling in the scheme default the way
// httpx.URL.port does, so http://x/p and http://x:80/p compare equal.
func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch u.Scheme {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}

// validRunID reports a hex run id with dashes only.
func validRunID(id string) bool {
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F', r == '-':
		default:
			return false
		}
	}
	return true
}

// glasserFromAPI shapes one raw result, bounding the content fields (Python
// parity: GlasserResult.from_api).
func glasserFromAPI(raw map[string]any, maxChars int) GlasserResult {
	out := GlasserResult{Truncated: false}
	if v, ok := raw["id"].(string); ok {
		out.ID = &v
	}
	if v, ok := raw["status"].(string); ok {
		out.Status = &v
	}
	if v, ok := raw["result_status"].(string); ok {
		out.ResultStatus = &v
	}
	if v, ok := raw["provider"].(string); ok {
		out.Provider = &v
	}
	switch charge := raw["charge_usd"].(type) {
	case string:
		out.ChargeUSD = &charge
	case float64:
		// AGO-DEVIATION: Python keeps whatever the API sends (its annotation says
		// str, but nothing validates it); the Go field is typed, so a numeric
		// charge is rendered as a string.
		rendered := fmt.Sprint(charge)
		out.ChargeUSD = &rendered
	}
	if v, ok := raw["run_url"].(string); ok {
		out.RunURL = &v
	}
	if v, ok := raw["result_url"].(string); ok {
		out.ResultURL = &v
	}
	out.Runs = raw["runs"]
	remaining := maxChars
	for _, field := range []string{"output", "error", "failure", "provider_response"} {
		value, next, cut := glasserContent(raw[field], remaining)
		switch field {
		case "output":
			out.Output = value
		case "error":
			out.Error = value
		case "failure":
			out.Failure = value
		default:
			out.ProviderResp = value
		}
		remaining = next
		out.Truncated = out.Truncated || cut
	}
	return out
}

// glasserContent bounds one content field: a value that fits stays as-is,
// anything longer becomes a string prefix (Python parity: glasser._content).
func glasserContent(value any, remaining int) (any, int, bool) {
	if value == nil {
		return nil, remaining, false
	}
	text, ok := value.(string)
	if !ok {
		encoded, err := pythonJSONDumps(value)
		if err != nil {
			return value, remaining, false
		}
		text = encoded
	}
	if remaining < 0 || runeLen(text) <= remaining {
		return value, remaining - runeLen(text), false
	}
	return string([]rune(text)[:remaining]), 0, true
}

// pythonJSONDumps renders a value the way Python's json.dumps does by default
// (a space after each "," and ":"), so a truncation boundary lands on the same
// character as the Python port.
func pythonJSONDumps(v any) (string, error) {
	compact, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	inString := false
	escaped := false
	for _, r := range string(compact) {
		switch {
		case escaped:
			escaped = false
		case r == '\\' && inString:
			escaped = true
		case r == '"':
			inString = !inString
		case (r == ',' || r == ':') && !inString:
			b.WriteRune(r)
			b.WriteRune(' ')
			continue
		}
		b.WriteRune(r)
	}
	return b.String(), nil
}
