package searchtools

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// recordedRequest is what a fake transport saw.
type recordedRequest struct {
	method string
	url    string
	header http.Header
	body   string
	// deadline is the request context's deadline (zero when it has none).
	deadline time.Time
}

// recordingTransport answers every request with one canned JSON body and keeps
// the request, so the default (non-overridden) hosts can be pinned offline.
type recordingTransport struct {
	t    *testing.T
	body string
	got  []recordedRequest
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body string
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			r.t.Fatalf("read request body: %v", err)
		}
		body = string(b)
	}
	deadline, _ := req.Context().Deadline()
	r.got = append(r.got, recordedRequest{
		method:   req.Method,
		url:      req.URL.String(),
		header:   req.Header.Clone(),
		body:     body,
		deadline: deadline,
	})
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(r.body)),
		Request:    req,
	}, nil
}

func (r *recordingTransport) only(t *testing.T) recordedRequest {
	t.Helper()
	if len(r.got) != 1 {
		t.Fatalf("requests = %d, want 1", len(r.got))
	}
	return r.got[0]
}

// TestExaDefaultEndpointAndHeaders pins the origin and the header set exa-py
// sends (x-api-key, JSON content type) for every tool.
func TestExaDefaultEndpointAndHeaders(t *testing.T) {
	cases := []struct {
		tool, args, path string
	}{
		{"exa_search", `{"query":"q"}`, "/search"},
		{"exa_find_similar", `{"url":"https://s.example/"}`, "/findSimilar"},
		{"exa_get_contents", `{"urls":["https://a.example/"]}`, "/contents"},
		{"exa_answer", `{"query":"q"}`, "/answer"},
	}
	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			rt := &recordingTransport{t: t, body: `{"results":[],"answer":"","citations":[]}`}
			tk, err := NewExaToolkit("exa-key", WithExaHTTPClient(&http.Client{Transport: rt}))
			if err != nil {
				t.Fatalf("NewExaToolkit: %v", err)
			}
			_, cx := newTestContext()
			callTool(t, tk, cx, c.tool, c.args)
			got := rt.only(t)
			if got.method != http.MethodPost || got.url != "https://api.exa.ai"+c.path {
				t.Fatalf("request = %s %s, want POST https://api.exa.ai%s", got.method, got.url, c.path)
			}
			if got.header.Get("x-api-key") != "exa-key" || got.header.Get("Content-Type") != "application/json" ||
				got.header.Get("x-exa-integration") != "ag2" || got.header.Get("User-Agent") != "ago-searchtools" {
				t.Fatalf("headers = %v", got.header)
			}
		})
	}
}

// TestFirecrawlDefaultEndpointAndHeaders pins the origin and the bearer auth of
// the Firecrawl SDK client.
func TestFirecrawlDefaultEndpointAndHeaders(t *testing.T) {
	cases := []struct {
		tool, args, path string
	}{
		{"firecrawl_search", `{"query":"q"}`, "/v2/search"},
		{"firecrawl_scrape", `{"url":"https://p.example/"}`, "/v2/scrape"},
	}
	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			rt := &recordingTransport{t: t, body: `{"success":true,"data":{}}`}
			tk, err := NewFirecrawlToolkit("fc-key", WithFirecrawlHTTPClient(&http.Client{Transport: rt}))
			if err != nil {
				t.Fatalf("NewFirecrawlToolkit: %v", err)
			}
			_, cx := newTestContext()
			callTool(t, tk, cx, c.tool, c.args)
			got := rt.only(t)
			if got.method != http.MethodPost || got.url != "https://api.firecrawl.dev"+c.path {
				t.Fatalf("request = %s %s, want POST https://api.firecrawl.dev%s", got.method, got.url, c.path)
			}
			if got.header.Get("Authorization") != "Bearer fc-key" ||
				got.header.Get("Content-Type") != "application/json" ||
				got.header.Get("User-Agent") != "ago-searchtools" {
				t.Fatalf("headers = %v", got.header)
			}
		})
	}
}

// TestTinyFishDefaultProductHosts pins the per-product hosts the SDK uses when
// no base URL is configured: the API is served at the host root, not /v1/<product>.
func TestTinyFishDefaultProductHosts(t *testing.T) {
	rt := &recordingTransport{t: t, body: `{"query":"q","results":[],"total_results":0}`}
	tk, err := NewTinyFishSearchToolkit("tf-key", WithTinyFishHTTPClient(&http.Client{Transport: rt}))
	if err != nil {
		t.Fatalf("NewTinyFishSearchToolkit: %v", err)
	}
	_, cx := newTestContext()
	callTool(t, tk, cx, "tinyfish_search", `{"query":"q"}`)
	got := rt.only(t)
	if got.method != http.MethodGet || got.url != "https://api.search.tinyfish.ai/?query=q" {
		t.Fatalf("search request = %s %s", got.method, got.url)
	}
	if got.header.Get("X-API-Key") != "tf-key" || got.header.Get("Accept") != "application/json" ||
		got.header.Get("X-TF-Request-Origin") != "tinyfish-python" {
		t.Fatalf("search headers = %v", got.header)
	}

	rt.got = nil
	rt.body = `{"results":[],"errors":[]}`
	callTool(t, tk, cx, "tinyfish_fetch", `{"urls":["https://a.example/"]}`)
	got = rt.only(t)
	if got.method != http.MethodPost || got.url != "https://api.fetch.tinyfish.ai/" {
		t.Fatalf("fetch request = %s %s", got.method, got.url)
	}
	if got.header.Get("Content-Type") != "application/json" || got.header.Get("X-API-Key") != "tf-key" {
		t.Fatalf("fetch headers = %v", got.header)
	}
	sameJSON(t, got.body, `{"api_integration":"ag2","urls":["https://a.example/"]}`)
}

// useDefaultClient routes requests of toolkits built without their own HTTP
// client through rt for the length of the test.
func useDefaultClient(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	old := defaultHTTPClient
	defaultHTTPClient = &http.Client{Transport: rt}
	t.Cleanup(func() { defaultHTTPClient = old })
}

// TestRequestTimeoutDefaults pins the per-attempt bound each SDK gives its HTTP
// client, as the deadline the request carries: Exa and TinyFish allow 600 s (a
// deep search outlives the generic 60 s), Firecrawl is capped at 600 s, and a
// request without a configured bound keeps DefaultTimeout.
func TestRequestTimeoutDefaults(t *testing.T) {
	_, cx := newTestContext()
	cases := []struct {
		name string
		call func(t *testing.T, rt *recordingTransport)
		want time.Duration
	}{
		{"exa", func(t *testing.T, rt *recordingTransport) {
			tk, err := NewExaToolkit("k")
			if err != nil {
				t.Fatal(err)
			}
			callTool(t, tk, cx, "exa_search", `{"query":"q"}`)
		}, 600 * time.Second},
		{"tinyfish", func(t *testing.T, rt *recordingTransport) {
			tk, err := NewTinyFishSearchToolkit("k")
			if err != nil {
				t.Fatal(err)
			}
			callTool(t, tk, cx, "tinyfish_search", `{"query":"q"}`)
		}, 600 * time.Second},
		{"firecrawl", func(t *testing.T, rt *recordingTransport) {
			tk, err := NewFirecrawlToolkit("k")
			if err != nil {
				t.Fatal(err)
			}
			callTool(t, tk, cx, "firecrawl_search", `{"query":"q"}`)
		}, 600 * time.Second},
		{"unset", func(t *testing.T, rt *recordingTransport) {
			_, err := httpRequest{provider: "p", method: http.MethodGet, url: "https://p.example/"}.do(nil)
			if err != nil {
				t.Fatal(err)
			}
		}, DefaultTimeout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt := &recordingTransport{t: t, body: `{"success":true,"data":{},"results":[],"query":"q","total_results":0}`}
			useDefaultClient(t, rt)
			start := time.Now()
			c.call(t, rt)
			got := rt.only(t)
			if got.deadline.IsZero() {
				t.Fatal("request carries no deadline")
			}
			if left := got.deadline.Sub(start); left < c.want-time.Second || left > c.want+5*time.Second {
				t.Fatalf("deadline is %s after the call started, want about %s", left, c.want)
			}
		})
	}

	t.Run("caller client is not bounded further", func(t *testing.T) {
		rt := &recordingTransport{t: t, body: `{}`}
		_, err := httpRequest{
			provider: "p", method: http.MethodGet, url: "https://p.example/",
			client: &http.Client{Transport: rt}, timeout: exaTimeout,
		}.do(nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := rt.only(t); !got.deadline.IsZero() {
			t.Fatalf("deadline = %v, want none: the caller's client owns its bound", got.deadline)
		}
	})
}

// TestFirecrawlScrapeMetadataWireKeys asserts the page metadata is read under the
// camelCase keys the API really sends (sourceURL, statusCode); the SDK maps them
// to snake_case and coerces a numeric string status to an int.
func TestFirecrawlScrapeMetadataWireKeys(t *testing.T) {
	cases := []struct {
		name     string
		metadata string
		want     string
	}{
		{
			name:     "api camelCase",
			metadata: `{"title":"P","sourceURL":"https://src.example/","statusCode":404}`,
			want:     `{"url":"https://src.example/","title":"P","status_code":404,"links":[],"truncated":false}`,
		},
		{
			name:     "numeric string status",
			metadata: `{"sourceURL":"https://src.example/","statusCode":"503"}`,
			want:     `{"url":"https://src.example/","status_code":503,"links":[],"truncated":false}`,
		},
		{
			name:     "url wins over sourceURL",
			metadata: `{"url":"https://final.example/","sourceURL":"https://src.example/","statusCode":200}`,
			want:     `{"url":"https://final.example/","status_code":200,"links":[],"truncated":false}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				respondJSON(t, w, `{"success":true,"data":{"metadata":`+c.metadata+`}}`)
			})
			tk, err := NewFirecrawlToolkit("k", WithFirecrawlBaseURL(srv.URL))
			if err != nil {
				t.Fatalf("NewFirecrawlToolkit: %v", err)
			}
			_, cx := newTestContext()
			ev := callTool(t, tk, cx, "firecrawl_scrape", `{"url":"https://asked.example/"}`)
			sameJSON(t, firstDataJSON(t, ev), c.want)
		})
	}
}

// TestFirecrawlSearchDocumentUsesSourceURL asserts a scraped search hit whose
// metadata names only sourceURL (the API spelling) keeps its URL.
func TestFirecrawlSearchDocumentUsesSourceURL(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		respondJSON(t, w, `{"success":true,"data":{"web":[
			{"markdown":"body","metadata":{"title":"T","sourceURL":"https://src.example/"}}
		]}}`)
	})
	tk, err := NewFirecrawlToolkit("k", WithFirecrawlBaseURL(srv.URL), WithFirecrawlScrapeResults(true))
	if err != nil {
		t.Fatalf("NewFirecrawlToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "firecrawl_search", `{"query":"q"}`)
	sameJSON(t, firstDataJSON(t, ev), `{"query":"q","web":[`+
		`{"url":"https://src.example/","title":"T","position":1,"markdown":"body","truncated":false}],"news":[]}`)
}

// TestFirecrawlMissingSuccessFlagIsFailure asserts a 200 body without
// "success":true fails the call, as the SDK's `not body.get("success")` does.
func TestFirecrawlMissingSuccessFlagIsFailure(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		respondJSON(t, w, `{"data":{"web":[{"url":"https://a.example/"}]}}`)
	})
	tk, err := NewFirecrawlToolkit("k", WithFirecrawlBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewFirecrawlToolkit: %v", err)
	}
	_, cx := newTestContext()
	if _, err := tk.search(callContext(cx), cx, "q"); !errors.Is(err, ErrAPIResponse) {
		t.Fatalf("search error = %v, want ErrAPIResponse", err)
	}
	if _, err := tk.scrape(callContext(cx), cx, "https://p.example/"); !errors.Is(err, ErrAPIResponse) {
		t.Fatalf("scrape error = %v, want ErrAPIResponse", err)
	}
}

// TestFirecrawlScrapeTrimsURL asserts the payload carries url.strip().
func TestFirecrawlScrapeTrimsURL(t *testing.T) {
	var gotBody string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody = readBody(t, r)
		respondJSON(t, w, `{"success":true,"data":{}}`)
	})
	tk, err := NewFirecrawlToolkit("k", WithFirecrawlBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewFirecrawlToolkit: %v", err)
	}
	_, cx := newTestContext()
	if _, err := tk.scrape(callContext(cx), cx, "https://page.example/  "); err != nil {
		t.Fatalf("scrape: %v", err)
	}
	if !strings.Contains(gotBody, `"url":"https://page.example/"`) {
		t.Fatalf("body = %s, want the stripped url", gotBody)
	}
}

// TestBlankQueryIsRejectedBeforeSending asserts the SDKs' client-side check: a
// blank query never reaches the network (Firecrawl: "Query cannot be empty";
// TinyFish: "query must be a non-empty string").
func TestBlankQueryIsRejectedBeforeSending(t *testing.T) {
	requests := 0
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		respondJSON(t, w, `{"success":true,"data":{},"results":[],"total_results":0}`)
	})
	_, cx := newTestContext()

	fc, err := NewFirecrawlToolkit("k", WithFirecrawlBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewFirecrawlToolkit: %v", err)
	}
	tf, err := NewTinyFishSearchToolkit("k", WithTinyFishBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewTinyFishSearchToolkit: %v", err)
	}
	for _, query := range []string{"", "   \t"} {
		if _, err := fc.search(callContext(cx), cx, query); !errors.Is(err, ErrRejected) {
			t.Errorf("firecrawl %q: error = %v, want ErrRejected", query, err)
		}
		if _, err := tf.search(callContext(cx), cx, query); !errors.Is(err, ErrRejected) {
			t.Errorf("tinyfish %q: error = %v, want ErrRejected", query, err)
		}
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want none", requests)
	}
}
