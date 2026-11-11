package searchtools

import (
	"net/http"
	"testing"
)

// TestFirecrawlOptionBoundaries pins every numeric and enum validator to its
// accepted range: the values just inside the boundary must build, the values
// just outside must be refused.
func TestFirecrawlOptionBoundaries(t *testing.T) {
	accepted := map[string]FirecrawlOption{
		"limit low edge":        WithFirecrawlLimit(1),
		"limit high edge":       WithFirecrawlLimit(100),
		"timeout low edge":      WithFirecrawlTimeout(1),
		"timeout high edge":     WithFirecrawlTimeout(300000),
		"max_age zero":          WithFirecrawlMaxAge(0),
		"scrape_max_age zero":   WithFirecrawlScrapeMaxAge(0),
		"max_characters zero":   WithFirecrawlMaxCharacters(0),
		"wait_for zero":         WithFirecrawlWaitFor(0),
		"proxy auto":            WithFirecrawlProxy("auto"),
		"proxy stealth":         WithFirecrawlProxy("stealth"),
		"sources web":           WithFirecrawlSources("web"),
		"sources news":          WithFirecrawlSources("news"),
		"categories github":     WithFirecrawlCategories("github"),
		"categories pdf":        WithFirecrawlCategories("pdf"),
		"formats markdown":      WithFirecrawlFormats("markdown"),
		"formats links":         WithFirecrawlFormats("links"),
		"empty sources list":    WithFirecrawlSources(),
		"empty categories list": WithFirecrawlCategories(),
	}
	for name, opt := range accepted {
		if _, err := NewFirecrawlToolkit("k", opt); err != nil {
			t.Fatalf("%s: %v, want it accepted", name, err)
		}
	}

	rejected := map[string]FirecrawlOption{
		"limit zero":           WithFirecrawlLimit(0),
		"limit negative":       WithFirecrawlLimit(-1),
		"limit over":           WithFirecrawlLimit(101),
		"timeout zero":         WithFirecrawlTimeout(0),
		"timeout negative":     WithFirecrawlTimeout(-1),
		"timeout over":         WithFirecrawlTimeout(300001),
		"max_age negative":     WithFirecrawlMaxAge(-1),
		"scrape_max_age neg":   WithFirecrawlScrapeMaxAge(-1),
		"max_characters neg":   WithFirecrawlMaxCharacters(-1),
		"wait_for negative":    WithFirecrawlWaitFor(-1),
		"proxy unknown":        WithFirecrawlProxy("turbo"),
		"source unknown":       WithFirecrawlSources("videos"),
		"category unknown":     WithFirecrawlCategories("poetry"),
		"format unknown":       WithFirecrawlFormats("pdf-text"),
		"empty base":           WithFirecrawlBaseURL(""),
		"nil client":           WithFirecrawlHTTPClient(nil),
		"empty key variable":   WithFirecrawlAPIKeyVariable(""),
		"empty variable name":  WithFirecrawlVariable("limit", ""),
		"empty variable param": WithFirecrawlVariable("", "v"),
	}
	for name, opt := range rejected {
		if _, err := NewFirecrawlToolkit("k", opt); err == nil {
			t.Fatalf("%s: must be rejected", name)
		}
	}
}

// TestFirecrawlOptionValuesReachTheRequest asserts the accepted boundary values
// are actually sent, so a validator that clamps or drops them shows up.
func TestFirecrawlOptionValuesReachTheRequest(t *testing.T) {
	var gotBody string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody = readBody(t, r)
		respondJSON(t, w, `{"success":true,"data":{"web":[]}}`)
	})
	tk, err := NewFirecrawlToolkit("k", WithFirecrawlBaseURL(srv.URL),
		WithFirecrawlLimit(100), WithFirecrawlTimeout(300000))
	if err != nil {
		t.Fatalf("NewFirecrawlToolkit: %v", err)
	}
	_, cx := newTestContext()
	if _, err := tk.search(callContext(cx), cx, "q"); err != nil {
		t.Fatalf("search: %v", err)
	}
	sameJSON(t, gotBody, `{"limit":100,"origin":"ag2","query":"q","timeout":300000}`)
}
