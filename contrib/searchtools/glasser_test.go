package searchtools

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestGlasserWebSearch asserts the exact request and the result envelope.
func TestGlasserWebSearch(t *testing.T) {
	var (
		gotMethod, gotPath, gotAuth, gotUA, gotIdem, gotBody string
	)
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		gotIdem = r.Header.Get("Idempotency-Key")
		gotBody = readBody(t, r)
		respondJSON(t, w, `{"id":"run-1","status":"DONE","result_status":"done","provider":"p",
			"charge_usd":"0.01","output":{"rows":1},"run_url":"https://api.glasser.ai/v1/runs/run-1"}`)
	})
	tk, err := NewGlasserToolkit("gl-secret", WithGlasserBaseURL(srv.URL),
		WithGlasserProvider("google"), WithGlasserLimit(3), WithGlasserTaskID("task-1"))
	if err != nil {
		t.Fatalf("NewGlasserToolkit: %v", err)
	}
	_, cx := newTestContext()
	cx = cx.WithToolCallID("call-1")
	ev := callTool(t, tk, cx, "glasser_web_search", `{"query":"ago port"}`)

	if gotMethod != http.MethodPost || gotPath != "/v1/solutions/gtm/web_research" {
		t.Fatalf("request = %s %s, want POST /v1/solutions/gtm/web_research", gotMethod, gotPath)
	}
	if gotAuth != "Bearer gl-secret" || gotUA != "ag2-glasser-extension" {
		t.Fatalf("headers = auth %q, ua %q", gotAuth, gotUA)
	}
	// The same stream and tool call always derive the same idempotency key, and a
	// different call derives another one.
	first, err := idempotencyKey(cx, "web_research")
	if err != nil {
		t.Fatalf("idempotencyKey: %v", err)
	}
	again, _ := idempotencyKey(cx, "web_research")
	other, _ := idempotencyKey(cx.WithToolCallID("call-2"), "web_research")
	if first != again || first == other {
		t.Fatalf("idempotency keys = %q, %q, %q", first, again, other)
	}
	if len(gotIdem) != 36 || strings.Count(gotIdem, "-") != 4 {
		t.Fatalf("idempotency key = %q, want a UUID", gotIdem)
	}
	sameJSON(t, gotBody, `{"action":"search","query":"ago port","provider":"google","limit":3,"task_id":"task-1"}`)
	want := `{"id":"run-1","status":"DONE","result_status":"done","provider":"p","charge_usd":"0.01",` +
		`"output":{"rows":1},"run_url":"https://api.glasser.ai/v1/runs/run-1","truncated":false}`
	sameJSON(t, firstDataJSON(t, ev), want)
}

// TestGlasserBodies asserts the payload of the remaining five capabilities.
func TestGlasserBodies(t *testing.T) {
	cases := []struct {
		tool  string
		args  string
		path  string
		body  string
		error string
	}{
		{
			tool: "glasser_company_profile", args: `{"domain":"ago.example"}`,
			path: "/v1/solutions/gtm/company_intelligence",
			body: `{"action":"enrich","domain":"ago.example"}`,
		},
		{
			tool: "glasser_find_people",
			args: `{"job_titles":["CTO"],"locations":["Berlin"],"company_domain":"ago.example","keywords":"go"}`,
			path: "/v1/solutions/gtm/people_search",
			body: `{"action":"search","job_titles":["CTO"],"locations":["Berlin"],` +
				`"company_domain":"ago.example","keywords":"go"}`,
		},
		{
			tool: "glasser_keyword_overview", args: `{"keywords":["ago","port"]}`,
			path: "/v1/solutions/gtm/seo_research",
			body: `{"action":"keyword_overview","keywords":["ago","port"]}`,
		},
		{
			tool: "glasser_social_search", args: `{"platform":"reddit","query":"ago"}`,
			path: "/v1/solutions/gtm/social_research",
			body: `{"mode":"search","platform":"reddit","query":"ago"}`,
		},
		{
			tool: "glasser_stock_quote", args: `{"symbol":"AGO"}`,
			path: "/v1/solutions/gtm/market_data",
			body: `{"action":"stock_quote","symbol":"AGO"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			var gotPath, gotBody string
			srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotBody = readBody(t, r)
				respondJSON(t, w, `{"status":"DONE"}`)
			})
			tk, err := NewGlasserToolkit("k", WithGlasserBaseURL(srv.URL), WithGlasserWaitTimeout(0))
			if err != nil {
				t.Fatalf("NewGlasserToolkit: %v", err)
			}
			_, cx := newTestContext()
			ev := callTool(t, tk, cx, tc.tool, tc.args)
			if gotPath != tc.path {
				t.Fatalf("path = %s, want %s", gotPath, tc.path)
			}
			sameJSON(t, gotBody, tc.body)
			sameJSON(t, firstDataJSON(t, ev), `{"status":"DONE","truncated":false}`)
		})
	}
}

// TestGlasserInputGuards asserts the model-facing error objects for empty input.
func TestGlasserInputGuards(t *testing.T) {
	cases := []struct {
		tool string
		args string
		want string
	}{
		{"glasser_web_search", `{"query":"  "}`, "query must not be empty"},
		{"glasser_company_profile", `{"domain":""}`, "domain must not be empty"},
		{"glasser_find_people", `{"job_titles":[" "]}`, "supply job_titles, locations, company_domain or keywords"},
		{"glasser_keyword_overview", `{"keywords":[""]}`, "keywords must contain a non-empty value"},
		{"glasser_social_search", `{"platform":"reddit","query":""}`, "query must not be empty"},
		{"glasser_stock_quote", `{"symbol":" "}`, "symbol must not be empty"},
	}
	requests := 0
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		respondJSON(t, w, `{"status":"DONE"}`)
	})
	tk, err := NewGlasserToolkit("k", WithGlasserBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewGlasserToolkit: %v", err)
	}
	for _, tc := range cases {
		_, cx := newTestContext()
		ev := callTool(t, tk, cx, tc.tool, tc.args)
		sameJSON(t, firstDataJSON(t, ev), `{"error":"`+tc.want+`"}`)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want none: a guard must not reach the paid API", requests)
	}
}

// TestGlasserTruncation asserts the content budget across the four content
// fields, with the Python-compatible JSON rendering for non-string values.
func TestGlasserTruncation(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		respondJSON(t, w, `{"status":"DONE","output":{"a":1},"error":"boom","failure":"f",
			"provider_response":"pr","charge_usd":"0.02","result_url":"https://kept.example/"}`)
	})
	tk, err := NewGlasserToolkit("k", WithGlasserBaseURL(srv.URL), WithGlasserMaxCharacters(5))
	if err != nil {
		t.Fatalf("NewGlasserToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "glasser_stock_quote", `{"symbol":"A"}`)
	want := `{"status":"DONE","charge_usd":"0.02","output":"{\"a\":","error":"","failure":"",` +
		`"provider_response":"","result_url":"https://kept.example/","truncated":true}`
	sameJSON(t, firstDataJSON(t, ev), want)
}

// TestGlasserPollsPendingResult asserts the poll loop follows result_url until
// the result leaves the pending state.
func TestGlasserPollsPendingResult(t *testing.T) {
	posts, polls := 0, 0
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			posts++
			respondJSON(t, w, `{"status":"QUEUED","result_url":"`+fakeOrigin(r)+`/v1/solutions/gtm/web_research/result"}`)
		case r.URL.Path == "/v1/solutions/gtm/web_research/result":
			polls++
			respondJSON(t, w, `{"status":"DONE","output":"ok"}`)
		default:
			t.Errorf("unexpected poll path %s", r.URL.Path)
		}
	})
	tk, err := NewGlasserToolkit("k", WithGlasserBaseURL(srv.URL),
		WithGlasserPollInterval(time.Millisecond), WithGlasserWaitTimeout(time.Second))
	if err != nil {
		t.Fatalf("NewGlasserToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "glasser_web_search", `{"query":"q"}`)
	if posts != 1 || polls != 1 {
		t.Fatalf("posts = %d, polls = %d, want one each", posts, polls)
	}
	sameJSON(t, firstDataJSON(t, ev), `{"status":"DONE","output":"ok","truncated":false}`)
}

// TestGlasserPollsPeopleRun asserts the people-search run-id poll path.
func TestGlasserPollsPeopleRun(t *testing.T) {
	var polled string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			respondJSON(t, w, `{"status":"RUNNING","id":"ab12cd34-ef56"}`)
			return
		}
		polled = r.URL.Path
		respondJSON(t, w, `{"status":"DONE"}`)
	})
	tk, err := NewGlasserToolkit("k", WithGlasserBaseURL(srv.URL),
		WithGlasserPollInterval(time.Millisecond), WithGlasserWaitTimeout(time.Second))
	if err != nil {
		t.Fatalf("NewGlasserToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "glasser_find_people", `{"keywords":"go"}`)
	if polled != "/v1/runs/ab12cd34-ef56" {
		t.Fatalf("polled = %s, want the run endpoint (event %s)", polled, toolErrorText(t, ev))
	}
	sameJSON(t, firstDataJSON(t, ev), `{"status":"DONE","truncated":false}`)
}

// TestGlasserRejectsInvalidRunID asserts a pending people-search result without a
// usable run id is refused.
func TestGlasserRejectsInvalidRunID(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		respondJSON(t, w, `{"status":"QUEUED","id":"run-abc123"}`)
	})
	tk, err := NewGlasserToolkit("k", WithGlasserBaseURL(srv.URL),
		WithGlasserPollInterval(time.Millisecond), WithGlasserWaitTimeout(time.Second))
	if err != nil {
		t.Fatalf("NewGlasserToolkit: %v", err)
	}
	_, cx := newTestContext()
	_, err = tk.execute(callContext(cx), cx, "people_search", map[string]any{"keywords": "go"})
	if !errors.Is(err, ErrAPIResponse) {
		t.Fatalf("error = %v, want ErrAPIResponse", err)
	}
}

// TestGlasserRejectsForeignPollURL asserts a result_url off the configured
// origin is refused, so an answer cannot redirect the poll.
func TestGlasserRejectsForeignPollURL(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		respondJSON(t, w, `{"status":"QUEUED","result_url":"https://evil.example/v1/solutions/gtm/web_research/result"}`)
	})
	tk, err := NewGlasserToolkit("k", WithGlasserBaseURL(srv.URL),
		WithGlasserPollInterval(time.Millisecond), WithGlasserWaitTimeout(time.Second))
	if err != nil {
		t.Fatalf("NewGlasserToolkit: %v", err)
	}
	_, cx := newTestContext()
	_, err = tk.execute(callContext(cx), cx, "web_research", map[string]any{"query": "q"})
	if !errors.Is(err, ErrAPIResponse) {
		t.Fatalf("error = %v, want ErrAPIResponse", err)
	}
}

// TestGlasserHTTPError asserts a non-2xx answer becomes the typed HTTP error.
func TestGlasserHTTPError(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		respondJSON(t, w, `{"error":"no credits"}`)
	})
	tk, err := NewGlasserToolkit("k", WithGlasserBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewGlasserToolkit: %v", err)
	}
	_, cx := newTestContext()
	_, err = tk.execute(callContext(cx), cx, "market_data", map[string]any{"symbol": "A"})
	if !errors.Is(err, ErrHTTPStatus) {
		t.Fatalf("error = %v, want ErrHTTPStatus", err)
	}
}

// TestGlasserMissingKey asserts the constructor rejects an empty key.
func TestGlasserMissingKey(t *testing.T) {
	if _, err := NewGlasserToolkit("   "); !errors.Is(err, ErrMissingKey) {
		t.Fatalf("error = %v, want ErrMissingKey", err)
	}
}

// TestGlasserKeyVariable asserts the conversation-variable key seam.
func TestGlasserKeyVariable(t *testing.T) {
	var gotAuth string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		respondJSON(t, w, `{"status":"DONE"}`)
	})
	tk, err := NewGlasserToolkit("", WithGlasserBaseURL(srv.URL), WithGlasserAPIKeyVariable("gl_key"))
	if err != nil {
		t.Fatalf("NewGlasserToolkit: %v", err)
	}
	_, cx := newTestContext()
	if _, err := tk.execute(callContext(cx), cx, "market_data", map[string]any{"symbol": "A"}); err == nil {
		t.Fatal("a missing key variable must fail the call")
	}
	if err := cx.SetVariable("gl_key", "var-key"); err != nil {
		t.Fatalf("SetVariable: %v", err)
	}
	if _, err := tk.execute(callContext(cx), cx, "market_data", map[string]any{"symbol": "A"}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if gotAuth != "Bearer var-key" {
		t.Fatalf("authorization = %q", gotAuth)
	}
}

// TestGlasserMembers asserts the toolkit name and all six Python tool names.
func TestGlasserMembers(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("construction must not call the API")
	})
	tk, err := NewGlasserToolkit("k", WithGlasserBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewGlasserToolkit: %v", err)
	}
	got := []string{}
	for _, member := range tk.Tools() {
		got = append(got, member.Name())
	}
	want := "glasser_web_search,glasser_company_profile,glasser_find_people," +
		"glasser_keyword_overview,glasser_social_search,glasser_stock_quote"
	if tk.Name() != "glasser_toolkit" || strings.Join(got, ",") != want {
		t.Fatalf("toolkit = %s with %v", tk.Name(), got)
	}
}

// TestGlasserOptionValidation rejects nonsense at construction.
func TestGlasserOptionValidation(t *testing.T) {
	cases := map[string]GlasserOption{
		"empty base":   WithGlasserBaseURL(""),
		"creds base":   WithGlasserBaseURL("https://u:p@api.example"),
		"nil client":   WithGlasserHTTPClient(nil),
		"timeout":      WithGlasserTimeout(0),
		"wait":         WithGlasserWaitTimeout(-time.Second),
		"poll":         WithGlasserPollInterval(0),
		"limit":        WithGlasserLimit(0),
		"max chars":    WithGlasserMaxCharacters(-1),
		"empty var":    WithGlasserVariable("limit", ""),
		"empty keyvar": WithGlasserAPIKeyVariable(""),
	}
	for name, opt := range cases {
		if _, err := NewGlasserToolkit("k", opt); err == nil {
			t.Fatalf("%s: option must be rejected", name)
		}
	}
}

// fakeOrigin reconstructs the test server origin from a request.
func fakeOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// TestGlasserPollsRelativeResultURL asserts a relative result_url is resolved
// against the API base, as Python's httpx.URL.join does: the call is already
// paid, so refusing it would make the model pay again.
func TestGlasserPollsRelativeResultURL(t *testing.T) {
	var polled string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			respondJSON(t, w, `{"status":"QUEUED","result_url":"/v1/solutions/gtm/web_research/result"}`)
			return
		}
		polled = r.URL.Path
		respondJSON(t, w, `{"status":"DONE","output":"ok"}`)
	})
	tk, err := NewGlasserToolkit("k", WithGlasserBaseURL(srv.URL),
		WithGlasserPollInterval(time.Millisecond), WithGlasserWaitTimeout(time.Second))
	if err != nil {
		t.Fatalf("NewGlasserToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "glasser_web_search", `{"query":"q"}`)
	if polled != "/v1/solutions/gtm/web_research/result" {
		t.Fatalf("polled = %q, want the resolved relative path", polled)
	}
	sameJSON(t, firstDataJSON(t, ev), `{"status":"DONE","output":"ok","truncated":false}`)
}

// TestGlasserPollURLResolution asserts the resolution rules of a pending
// result_url: a relative path, a scheme-relative path and an absolute same-origin
// URL are accepted (Python parity: httpx.URL.join), another origin is refused.
func TestGlasserPollURLResolution(t *testing.T) {
	const base = "https://api.glasser.ai/"
	const path = "v1/solutions/gtm/web_research"
	accepted := []string{
		"/v1/solutions/gtm/web_research/result",
		"v1/solutions/gtm/web_research/result",
		"https://api.glasser.ai/v1/solutions/gtm/web_research/result",
		"https://api.glasser.ai:443/v1/solutions/gtm/web_research/result",
		"//api.glasser.ai/v1/solutions/gtm/web_research/result",
	}
	for _, resultURL := range accepted {
		got, err := glasserPollURL(base, path, "web_research", map[string]any{"result_url": resultURL})
		if err != nil {
			t.Fatalf("result_url %q: %v, want it accepted", resultURL, err)
		}
		if got == "" {
			t.Fatalf("result_url %q resolved to an empty URL", resultURL)
		}
	}
	refused := []string{
		"https://evil.example/v1/solutions/gtm/web_research/result",
		"http://api.glasser.ai/v1/solutions/gtm/web_research/result",
		"https://api.glasser.ai:8443/v1/solutions/gtm/web_research/result",
		"https://api.glasser.ai/v1/solutions/gtm/company_intelligence/result",
		"/v1/solutions/gtm/web_research/result#frag",
	}
	for _, resultURL := range refused {
		if _, err := glasserPollURL(base, path, "web_research", map[string]any{"result_url": resultURL}); !errors.Is(err, ErrAPIResponse) {
			t.Fatalf("result_url %q: error = %v, want ErrAPIResponse", resultURL, err)
		}
	}
}

// TestGlasserBaseURLValidation asserts every rejected base URL shape, so a typo
// cannot silently target another origin or carry credentials.
func TestGlasserBaseURLValidation(t *testing.T) {
	refused := []string{
		"",
		"api.glasser.ai",
		"ftp://api.glasser.ai",
		"https://",
		"https://u:p@api.glasser.ai",
		"https://api.glasser.ai?token=1",
		"https://api.glasser.ai#frag",
	}
	for _, base := range refused {
		if _, err := NewGlasserToolkit("k", WithGlasserBaseURL(base)); err == nil {
			t.Fatalf("base %q must be rejected", base)
		}
	}
	for _, base := range []string{"http://api.glasser.ai", "https://api.glasser.ai/"} {
		tk, err := NewGlasserToolkit("k", WithGlasserBaseURL(base))
		if err != nil {
			t.Fatalf("base %q: %v", base, err)
		}
		if tk.baseURL != strings.TrimRight(base, "/")+"/" {
			t.Fatalf("base %q became %q", base, tk.baseURL)
		}
	}
}

// TestGlasserFactoryOptionError asserts a factory hands its option error back,
// so a bad per-tool override never yields a silently defaulted tool.
func TestGlasserFactoryOptionError(t *testing.T) {
	tk, err := NewGlasserToolkit("k")
	if err != nil {
		t.Fatalf("NewGlasserToolkit: %v", err)
	}
	if _, err := tk.WebSearch(WithGlasserLimit(0)); err == nil {
		t.Fatal("a factory option error must surface")
	}
	if _, err := tk.WebSearch(WithGlasserLimit(2)); err != nil {
		t.Fatalf("a valid factory override must succeed: %v", err)
	}
}

// TestGlasserNilMiddleware asserts a nil middleware entry fails construction.
func TestGlasserNilMiddleware(t *testing.T) {
	if _, err := NewGlasserToolkit("k", WithGlasserMiddleware(nil)); err == nil {
		t.Fatal("a nil middleware must be rejected")
	}
}

// TestGlasserEternalPendingBoundedByWait asserts a result that never leaves the
// pending state is returned once the wait timeout passes, with its status and
// retrieval URL intact, instead of hanging.
func TestGlasserEternalPendingBoundedByWait(t *testing.T) {
	polls := 0
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			polls++
		}
		respondJSON(t, w, `{"status":"RUNNING","result_url":"/v1/solutions/gtm/web_research/result"}`)
	})
	tk, err := NewGlasserToolkit("k", WithGlasserBaseURL(srv.URL),
		WithGlasserPollInterval(5*time.Millisecond), WithGlasserWaitTimeout(40*time.Millisecond))
	if err != nil {
		t.Fatalf("NewGlasserToolkit: %v", err)
	}
	_, cx := newTestContext()
	started := time.Now()
	res, err := tk.execute(callContext(cx), cx, "web_research", map[string]any{"query": "q"})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("elapsed = %v, want the wait timeout to bound the poll", elapsed)
	}
	if polls == 0 {
		t.Fatal("the pending result must be polled at least once")
	}
	got, ok := res.(GlasserResult)
	if !ok {
		t.Fatalf("result = %T, want GlasserResult", res)
	}
	if got.Status == nil || *got.Status != "RUNNING" || got.ResultURL == nil {
		t.Fatalf("result = %+v, want the pending status and its result_url", got)
	}
}
