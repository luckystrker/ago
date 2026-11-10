package searchtools

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/luckystrker/ago/middleware"
)

// TestFirecrawlMiddleware asserts a nil entry is refused and a valid one is
// attached to both members.
func TestFirecrawlMiddleware(t *testing.T) {
	if _, err := NewFirecrawlToolkit("k", WithFirecrawlMiddleware(nil)); err == nil {
		t.Fatal("a nil middleware must be rejected")
	}
	var calls int32
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		respondJSON(t, w, `{"success":true,"data":{"web":[]}}`)
	})
	tk, err := NewFirecrawlToolkit("k", WithFirecrawlBaseURL(srv.URL),
		WithFirecrawlMiddleware(countingMiddleware{calls: &calls}))
	if err != nil {
		t.Fatalf("NewFirecrawlToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "firecrawl_search", `{"query":"q"}`)
	if got := firstDataJSON(t, ev); got == "" {
		t.Fatal("empty result")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("middleware calls = %d, want exactly 1", got)
	}
}

// TestFirecrawlDomainsReachTheRequest asserts each domain filter is sent alone,
// without an empty counterpart key.
func TestFirecrawlDomainsReachTheRequest(t *testing.T) {
	cases := []struct {
		name string
		opts []FirecrawlOption
		body string
	}{
		{
			name: "include only",
			opts: []FirecrawlOption{WithFirecrawlDomains([]string{"a.example"}, nil)},
			body: `{"includeDomains":["a.example"],"limit":5,"origin":"ag2","query":"q","timeout":300000}`,
		},
		{
			name: "exclude only",
			opts: []FirecrawlOption{WithFirecrawlDomains(nil, []string{"b.example"})},
			body: `{"excludeDomains":["b.example"],"limit":5,"origin":"ag2","query":"q","timeout":300000}`,
		},
		{
			name: "tags include only",
			opts: []FirecrawlOption{WithFirecrawlTags([]string{"main"}, nil), WithFirecrawlFormats("markdown")},
			body: "",
		},
		{
			name: "tags exclude only",
			opts: []FirecrawlOption{WithFirecrawlTags(nil, []string{"nav"}), WithFirecrawlFormats("markdown")},
			body: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody string
			srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				gotBody = readBody(t, r)
				respondJSON(t, w, `{"success":true,"data":{"web":[]}}`)
			})
			opts := append([]FirecrawlOption{WithFirecrawlBaseURL(srv.URL)}, tc.opts...)
			tk, err := NewFirecrawlToolkit("k", opts...)
			if err != nil {
				t.Fatalf("NewFirecrawlToolkit: %v", err)
			}
			_, cx := newTestContext()
			if tc.body == "" {
				if _, err := tk.scrape(callContext(cx), cx, "https://page.example/"); err != nil {
					t.Fatalf("scrape: %v", err)
				}
				if tc.name == "tags include only" {
					sameJSON(t, gotBody, `{"blockAds":true,"fastMode":false,"formats":["markdown"],
						"includeTags":["main"],"maxAge":14400000,"mobile":false,"onlyMainContent":true,
						"origin":"ag2","removeBase64Images":true,"skipTlsVerification":true,
						"storeInCache":true,"url":"https://page.example/"}`)
				} else {
					sameJSON(t, gotBody, `{"blockAds":true,"excludeTags":["nav"],"fastMode":false,
						"formats":["markdown"],"maxAge":14400000,"mobile":false,"onlyMainContent":true,
						"origin":"ag2","removeBase64Images":true,"skipTlsVerification":true,
						"storeInCache":true,"url":"https://page.example/"}`)
				}
				return
			}
			if _, err := tk.search(callContext(cx), cx, "q"); err != nil {
				t.Fatalf("search: %v", err)
			}
			sameJSON(t, gotBody, tc.body)
		})
	}
}

// TestFirecrawlRuntimeDomainFilters asserts the call-time check: only the
// conflicting pair is refused, a single filter passes.
func TestFirecrawlRuntimeDomainFilters(t *testing.T) {
	var gotBody string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody = readBody(t, r)
		respondJSON(t, w, `{"success":true,"data":{"web":[]}}`)
	})
	tk, err := NewFirecrawlToolkit("k", WithFirecrawlBaseURL(srv.URL),
		WithFirecrawlVariable("include_domains", "inc"))
	if err != nil {
		t.Fatalf("NewFirecrawlToolkit: %v", err)
	}
	_, cx := newTestContext()
	if err := cx.SetVariable("inc", []string{"a.example"}); err != nil {
		t.Fatalf("SetVariable: %v", err)
	}
	if _, err := tk.search(callContext(cx), cx, "q"); err != nil {
		t.Fatalf("search with one filter: %v", err)
	}
	sameJSON(t, gotBody, `{"includeDomains":["a.example"],"limit":5,"origin":"ag2","query":"q","timeout":300000}`)
}

// TestFirecrawlSearchScrapeOptionsGate asserts scrapeOptions appear only when
// scrape_results is true, including the explicit-false case.
func TestFirecrawlSearchScrapeOptionsGate(t *testing.T) {
	for _, scrape := range []bool{true, false} {
		var gotBody string
		srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			gotBody = readBody(t, r)
			respondJSON(t, w, `{"success":true,"data":{"web":[]}}`)
		})
		tk, err := NewFirecrawlToolkit("k", WithFirecrawlBaseURL(srv.URL),
			WithFirecrawlScrapeResults(scrape))
		if err != nil {
			t.Fatalf("NewFirecrawlToolkit: %v", err)
		}
		_, cx := newTestContext()
		if _, err := tk.search(callContext(cx), cx, "q"); err != nil {
			t.Fatalf("search: %v", err)
		}
		got := map[string]any{}
		if err := json.Unmarshal([]byte(gotBody), &got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		_, present := got["scrapeOptions"]
		if present != scrape {
			t.Fatalf("scrape_results=%v: scrapeOptions present=%v, body=%s", scrape, present, gotBody)
		}
	}
}

// TestFirecrawlNewsDocumentRank asserts a scraped news entry takes its rank as
// the position, because a Document carries no search position.
func TestFirecrawlNewsDocumentRank(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		respondJSON(t, w, `{"success":true,"data":{"news":[
			{"title":"One","url":"https://n1.example/","snippet":"s","date":"2026-01-01","position":1},
			{"url":"https://n2.example/","markdown":"page","metadata":{"url":"https://n2.example/","title":"T"}}
		]}}`)
	})
	tk, err := NewFirecrawlToolkit("k", WithFirecrawlBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewFirecrawlToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "firecrawl_search", `{"query":"q"}`)
	sameJSON(t, firstDataJSON(t, ev), `{"query":"q","web":[],"news":[`+
		`{"url":"https://n1.example/","title":"One","snippet":"s","date":"2026-01-01","position":1,"truncated":false},`+
		`{"url":"https://n2.example/","title":"T","position":2,"markdown":"page","truncated":false}]}`)
}

// TestFirecrawlEmptyFormatsFallBackToMarkdown asserts an empty formats list keeps
// the ["markdown"] default (Python parity: `or ["markdown"]`).
func TestFirecrawlEmptyFormatsFallBackToMarkdown(t *testing.T) {
	var gotBody string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody = readBody(t, r)
		respondJSON(t, w, `{"success":true,"data":{"metadata":{"url":"https://page.example/"}}}`)
	})
	tk, err := NewFirecrawlToolkit("k", WithFirecrawlBaseURL(srv.URL), WithFirecrawlFormats())
	if err != nil {
		t.Fatalf("NewFirecrawlToolkit: %v", err)
	}
	_, cx := newTestContext()
	if _, err := tk.scrape(callContext(cx), cx, "https://page.example/"); err != nil {
		t.Fatalf("scrape: %v", err)
	}
	sameJSON(t, gotBody, `{"blockAds":true,"fastMode":false,"formats":["markdown"],
		"maxAge":14400000,"mobile":false,"onlyMainContent":true,"origin":"ag2",
		"removeBase64Images":true,"skipTlsVerification":true,"storeInCache":true,
		"url":"https://page.example/"}`)
}

// TestFirecrawlTruncationFlagCombinesFields asserts a cut in any one field marks
// the whole result truncated, whatever the other fields did.
func TestFirecrawlTruncationFlagCombinesFields(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{
			name: "every field cut",
			data: `{"markdown":"abcdef","html":"<p>ok</p>","links":["https://a.example/"]}`,
			want: `{"url":"https://page.example/","markdown":"abc","html":"<p>",` +
				`"links":[],"truncated":true}`,
		},
		{
			name: "only markdown cut",
			data: `{"markdown":"abcdef","html":"ab"}`,
			want: `{"url":"https://page.example/","markdown":"abc","html":"ab",` +
				`"links":[],"truncated":true}`,
		},
		{
			name: "only links cut",
			data: `{"markdown":"ab","html":"ab","links":["https://a.example/"]}`,
			want: `{"url":"https://page.example/","markdown":"ab","html":"ab",` +
				`"links":[],"truncated":true}`,
		},
		{
			name: "nothing cut",
			data: `{"markdown":"ab","html":"ab","links":["a"]}`,
			want: `{"url":"https://page.example/","markdown":"ab","html":"ab",` +
				`"links":["a"],"truncated":false}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := `{"success":true,"data":` +
				strings.TrimSuffix(tc.data, "}") + `,"metadata":{"url":"https://page.example/"}}}`
			srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				respondJSON(t, w, payload)
			})
			tk, err := NewFirecrawlToolkit("k", WithFirecrawlBaseURL(srv.URL), WithFirecrawlMaxCharacters(3))
			if err != nil {
				t.Fatalf("NewFirecrawlToolkit: %v", err)
			}
			_, cx := newTestContext()
			ev := callTool(t, tk, cx, "firecrawl_scrape", `{"url":"https://page.example/"}`)
			sameJSON(t, firstDataJSON(t, ev), tc.want)
		})
	}
}

// TestFirecrawlFactoryOptionError asserts a factory returns its option error.
func TestFirecrawlFactoryOptionError(t *testing.T) {
	tk, err := NewFirecrawlToolkit("k")
	if err != nil {
		t.Fatalf("NewFirecrawlToolkit: %v", err)
	}
	if _, err := tk.Search(WithFirecrawlLimit(0)); err == nil {
		t.Fatal("a factory option error must surface")
	}
	if _, err := tk.Search(WithFirecrawlLimit(2)); err != nil {
		t.Fatalf("a valid factory override must succeed: %v", err)
	}
}

// countingMiddleware satisfies the middleware contract in tests.
var _ middleware.Middleware = countingMiddleware{}
