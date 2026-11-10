package searchtools

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestFirecrawlSearch asserts the minimal search request (SDK defaults included)
// and the result shaping for plain hits.
func TestFirecrawlSearch(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotBody string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBody = readBody(t, r)
		respondJSON(t, w, `{"success":true,"data":{
			"web":[
				{"url":"https://ago.example/","title":"AGo","description":"the port","position":1},
				{"url":"https://two.example/"}
			],
			"news":[{"url":"https://n.example/","title":"News","snippet":"s","date":"2026-01-01","position":2}]
		}}`)
	})
	tk, err := NewFirecrawlToolkit("fc-secret", WithFirecrawlBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewFirecrawlToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "firecrawl_search", `{"query":"ago port"}`)

	if gotMethod != http.MethodPost || gotPath != "/v2/search" {
		t.Fatalf("request = %s %s, want POST /v2/search", gotMethod, gotPath)
	}
	if gotAuth != "Bearer fc-secret" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	sameJSON(t, gotBody, `{"limit":5,"origin":"ag2","query":"ago port","timeout":300000}`)
	want := `{"query":"ago port",` +
		`"web":[{"url":"https://ago.example/","title":"AGo","description":"the port","position":1,"truncated":false},` +
		`{"url":"https://two.example/","truncated":false}],` +
		`"news":[{"url":"https://n.example/","title":"News","snippet":"s","date":"2026-01-01","position":2,"truncated":false}]}`
	sameJSON(t, firstDataJSON(t, ev), want)
}

// TestFirecrawlSearchFiltersAndScrape asserts every filter's wire key and the
// scrapeOptions block that scrape_results adds.
func TestFirecrawlSearchFiltersAndScrape(t *testing.T) {
	var gotBody string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody = readBody(t, r)
		respondJSON(t, w, `{"success":true,"data":{"web":[
			{"url":"https://page.example/","metadata":{"url":"https://page.example/","title":"P","description":"D"},
			 "markdown":"abcdefghij"}
		]}}`)
	})
	tk, err := NewFirecrawlToolkit("k", WithFirecrawlBaseURL(srv.URL),
		WithFirecrawlLimit(3),
		WithFirecrawlSources("web"),
		WithFirecrawlCategories("github"),
		WithFirecrawlDomains([]string{"a.example"}, nil),
		WithFirecrawlTBS("qdr:d"),
		WithFirecrawlLocation("Germany"),
		WithFirecrawlCountry("DE"),
		WithFirecrawlIgnoreInvalidURLs(true),
		WithFirecrawlTimeout(5000),
		WithFirecrawlScrapeResults(true),
		WithFirecrawlMaxCharacters(4),
	)
	if err != nil {
		t.Fatalf("NewFirecrawlToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "firecrawl_search", `{"query":"q"}`)
	wantBody := `{
		"categories":[{"type":"github"}],
		"country":"DE",
		"ignoreInvalidURLs":true,
		"includeDomains":["a.example"],
		"limit":3,
		"location":"Germany",
		"origin":"ag2",
		"query":"q",
		"scrapeOptions":{
			"blockAds":true,"fastMode":false,"formats":["markdown"],
			"maxAge":14400000,"mobile":false,"onlyMainContent":true,
			"removeBase64Images":true,"skipTlsVerification":true,"storeInCache":true},
		"sources":[{"type":"web"}],
		"tbs":"qdr:d",
		"timeout":5000
	}`
	sameJSON(t, gotBody, wantBody)
	// A scraped entry carries the rank, its page metadata and the cut markdown.
	want := `{"query":"q","web":[{` +
		`"url":"https://page.example/","title":"P","description":"D","position":1,` +
		`"markdown":"abcd","truncated":true}],"news":[]}`
	sameJSON(t, firstDataJSON(t, ev), want)
}

// TestFirecrawlScrape asserts the exact scrape request and result payload,
// including link truncation.
func TestFirecrawlScrape(t *testing.T) {
	var gotPath, gotBody string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody = readBody(t, r)
		respondJSON(t, w, `{"success":true,"data":{
			"markdown":"# Page",
			"html":"<h1>Page</h1>",
			"links":["https://a.example/","https://b.example/"],
			"metadata":{"url":"https://page.example/final","title":"Page","description":"D",
				"language":"en","statusCode":200}
		}}`)
	})
	tk, err := NewFirecrawlToolkit("k", WithFirecrawlBaseURL(srv.URL),
		WithFirecrawlFormats("markdown", "html", "links"),
		WithFirecrawlPageHeaders(map[string]string{"X-Test": "1"}),
		WithFirecrawlMaxCharacters(1000),
	)
	if err != nil {
		t.Fatalf("NewFirecrawlToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "firecrawl_scrape", `{"url":"https://page.example/"}`)
	if gotPath != "/v2/scrape" {
		t.Fatalf("path = %s, want /v2/scrape", gotPath)
	}
	wantBody := `{
		"blockAds":true,"fastMode":false,"formats":["markdown","html","links"],
		"headers":{"X-Test":"1"},"maxAge":14400000,"mobile":false,
		"onlyMainContent":true,"origin":"ag2","removeBase64Images":true,
		"skipTlsVerification":true,"storeInCache":true,
		"url":"https://page.example/"
	}`
	sameJSON(t, gotBody, wantBody)
	want := `{"url":"https://page.example/final","title":"Page","description":"D","language":"en",` +
		`"status_code":200,"markdown":"# Page","html":"<h1>Page</h1>",` +
		`"links":["https://a.example/","https://b.example/"],"truncated":false}`
	sameJSON(t, firstDataJSON(t, ev), want)
}

// TestFirecrawlScrapeLinkTruncation asserts the combined-length link cap.
func TestFirecrawlScrapeLinkTruncation(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		respondJSON(t, w, `{"success":true,"data":{"links":["https://a.example/","https://b.example/"],
			"metadata":{"url":"https://page.example/"}}}`)
	})
	tk, err := NewFirecrawlToolkit("k", WithFirecrawlBaseURL(srv.URL), WithFirecrawlMaxCharacters(20))
	if err != nil {
		t.Fatalf("NewFirecrawlToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "firecrawl_scrape", `{"url":"https://page.example/"}`)
	sameJSON(t, firstDataJSON(t, ev),
		`{"url":"https://page.example/","links":["https://a.example/"],"truncated":true}`)
}

// TestFirecrawlScrapeRejectsUnsafeURL asserts the model-facing error object.
func TestFirecrawlScrapeRejectsUnsafeURL(t *testing.T) {
	requests := 0
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		respondJSON(t, w, `{"success":true,"data":{}}`)
	})
	tk, err := NewFirecrawlToolkit("k", WithFirecrawlBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewFirecrawlToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "firecrawl_scrape", `{"url":"ftp://x"}`)
	sameJSON(t, firstDataJSON(t, ev),
		`{"error":"Only http/https URLs are supported; rejected: 'ftp://x'"}`)
	if requests != 0 {
		t.Fatalf("requests = %d, want none", requests)
	}
}

// TestFirecrawlErrors asserts the typed HTTP error and the success=false path.
func TestFirecrawlErrors(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		respondJSON(t, w, `{"success":false,"error":"bad request"}`)
	})
	tk, err := NewFirecrawlToolkit("k", WithFirecrawlBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewFirecrawlToolkit: %v", err)
	}
	_, cx := newTestContext()
	_, err = tk.search(callContext(cx), cx, "q")
	if !errors.Is(err, ErrHTTPStatus) {
		t.Fatalf("error = %v, want ErrHTTPStatus", err)
	}

	accepted := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		respondJSON(t, w, `{"success":false,"error":"credits exhausted"}`)
	})
	tk2, err := NewFirecrawlToolkit("k", WithFirecrawlBaseURL(accepted.URL))
	if err != nil {
		t.Fatalf("NewFirecrawlToolkit: %v", err)
	}
	_, err = tk2.search(callContext(cx), cx, "q")
	if !errors.Is(err, ErrAPIResponse) || !strings.Contains(err.Error(), "credits exhausted") {
		t.Fatalf("error = %v, want ErrAPIResponse naming the provider error", err)
	}
}

// TestFirecrawlMissingKey asserts the call fails without a key.
func TestFirecrawlMissingKey(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEY", "")
	tk, err := NewFirecrawlToolkit("")
	if err != nil {
		t.Fatalf("NewFirecrawlToolkit: %v", err)
	}
	_, cx := newTestContext()
	_, err = tk.search(callContext(cx), cx, "q")
	if !errors.Is(err, ErrMissingKey) || !strings.Contains(err.Error(), "FIRECRAWL_API_KEY") {
		t.Fatalf("error = %v, want ErrMissingKey naming FIRECRAWL_API_KEY", err)
	}
}

// TestFirecrawlDomainFilters asserts include and exclude cannot be combined.
func TestFirecrawlDomainFilters(t *testing.T) {
	if _, err := NewFirecrawlToolkit("k", WithFirecrawlDomains([]string{"a"}, []string{"b"})); err == nil {
		t.Fatal("both domain filters must be rejected")
	}
	_, cx := newTestContext()
	if err := cx.SetVariable("inc", []string{"a"}); err != nil {
		t.Fatalf("SetVariable: %v", err)
	}
	if err := cx.SetVariable("exc", []string{"b"}); err != nil {
		t.Fatalf("SetVariable: %v", err)
	}
	both, err := NewFirecrawlToolkit("k",
		WithFirecrawlVariable("include_domains", "inc"),
		WithFirecrawlVariable("exclude_domains", "exc"))
	if err != nil {
		t.Fatalf("NewFirecrawlToolkit: %v", err)
	}
	if _, err := both.search(callContext(cx), cx, "q"); !errors.Is(err, ErrRejected) {
		t.Fatalf("error = %v, want ErrRejected at call time", err)
	}
}

// TestFirecrawlMembers asserts the toolkit name and both Python tool names.
func TestFirecrawlMembers(t *testing.T) {
	tk, err := NewFirecrawlToolkit("k")
	if err != nil {
		t.Fatalf("NewFirecrawlToolkit: %v", err)
	}
	if tk.Name() != "firecrawl_toolkit" || len(tk.Tools()) != 2 ||
		tk.Tools()[0].Name() != "firecrawl_search" || tk.Tools()[1].Name() != "firecrawl_scrape" {
		t.Fatalf("toolkit = %s with %v", tk.Name(), tk.Tools())
	}
}
