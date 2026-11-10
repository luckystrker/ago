package searchtools

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestTinyFishSearch asserts the exact search request and result payload.
func TestTinyFishSearch(t *testing.T) {
	var gotMethod, gotPath, gotQuery, gotKey, gotOrigin, gotAccept, gotUA string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotKey = r.Header.Get("X-API-Key")
		gotOrigin = r.Header.Get("X-TF-Request-Origin")
		gotAccept = r.Header.Get("Accept")
		gotUA = r.Header.Get("User-Agent")
		respondJSON(t, w, `{"query":"ago port","total_results":2,"results":[
			{"position":1,"site_name":"ago.example","title":"AGo","snippet":"the port","url":"https://ago.example/"},
			{"position":2,"site_name":"two.example","title":"Two","snippet":"s","url":"https://two.example/"}
		]}`)
	})
	tk, err := NewTinyFishSearchToolkit("tf-secret",
		WithTinyFishBaseURL(srv.URL),
		WithTinyFishSearchDefaults("US", "en"))
	if err != nil {
		t.Fatalf("NewTinyFishSearchToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "tinyfish_search", `{"query":"ago port"}`)

	if gotMethod != http.MethodGet || gotPath != "/v1/search" {
		t.Fatalf("request = %s %s, want GET /v1/search", gotMethod, gotPath)
	}
	if gotQuery != "language=en&location=US&query=ago+port" {
		t.Fatalf("query = %s", gotQuery)
	}
	if gotKey != "tf-secret" || gotOrigin != "tinyfish-python" || gotAccept != "application/json" ||
		gotUA != "ago-searchtools" {
		t.Fatalf("headers = %q %q %q %q", gotKey, gotOrigin, gotAccept, gotUA)
	}
	want := `{"query":"ago port","total_results":2,"results":[` +
		`{"position":1,"site_name":"ago.example","title":"AGo","snippet":"the port","url":"https://ago.example/"},` +
		`{"position":2,"site_name":"two.example","title":"Two","snippet":"s","url":"https://two.example/"}]}`
	sameJSON(t, firstDataJSON(t, ev), want)
}

// TestTinyFishFetch asserts the exact fetch request and result payload.
func TestTinyFishFetch(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotBody = readBody(t, r)
		respondJSON(t, w, `{
			"results":[{"url":"https://a.example/","final_url":"https://a.example/home","title":"A",
				"description":"d","language":"en","author":"Ann","published_date":"2026-01-01",
				"text":"# Page","format":"markdown","links":["https://l.example/"],
				"image_links":["https://i.example/x.png"]}],
			"errors":[{"url":"https://b.example/","error":"timeout"}]
		}`)
	})
	tk, err := NewTinyFishSearchToolkit("k",
		WithTinyFishBaseURL(srv.URL),
		WithTinyFishFetchDefaults("markdown", true, false))
	if err != nil {
		t.Fatalf("NewTinyFishSearchToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "tinyfish_fetch", `{"urls":["https://a.example/","https://b.example/"]}`)
	if gotMethod != http.MethodPost || gotPath != "/v1/fetch" {
		t.Fatalf("request = %s %s", gotMethod, gotPath)
	}
	wantBody := `{"api_integration":"ag2","format":"markdown","image_links":false,"links":true,` +
		`"urls":["https://a.example/","https://b.example/"]}`
	sameJSON(t, gotBody, wantBody)
	want := `{"results":[{"url":"https://a.example/","format":"markdown",` +
		`"final_url":"https://a.example/home","title":"A","description":"d","language":"en",` +
		`"author":"Ann","published_date":"2026-01-01","text":"# Page",` +
		`"links":["https://l.example/"],"image_links":["https://i.example/x.png"]}],` +
		`"errors":[{"url":"https://b.example/","error":"timeout"}]}`
	sameJSON(t, firstDataJSON(t, ev), want)
}

// TestTinyFishFetchRejectsUnsafeURLs asserts the model-facing error object for a
// non-http scheme, without any request leaving the client.
func TestTinyFishFetchRejectsUnsafeURLs(t *testing.T) {
	requests := 0
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		respondJSON(t, w, `{}`)
	})
	tk, err := NewTinyFishSearchToolkit("k", WithTinyFishBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewTinyFishSearchToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "tinyfish_fetch", `{"urls":["ftp://x","https://ok.example/"]}`)
	sameJSON(t, firstDataJSON(t, ev),
		`{"error":"Only http/https URLs are supported; rejected: ['ftp://x']"}`)
	if requests != 0 {
		t.Fatalf("requests = %d, want none", requests)
	}
}

// TestTinyFishFetchRejectsEmptyAndOversizedBatches asserts the SDK's 1-10 range.
func TestTinyFishFetchRejectsEmptyAndOversizedBatches(t *testing.T) {
	tk, err := NewTinyFishSearchToolkit("k")
	if err != nil {
		t.Fatalf("NewTinyFishSearchToolkit: %v", err)
	}
	_, cx := newTestContext()
	if _, err := tk.fetch(callContext(cx), cx, nil); !errors.Is(err, ErrRejected) {
		t.Fatalf("error = %v, want ErrRejected", err)
	}
	urls := make([]string, 11)
	for i := range urls {
		urls[i] = "https://x.example/"
	}
	if _, err := tk.fetch(callContext(cx), cx, urls); !errors.Is(err, ErrRejected) {
		t.Fatalf("error = %v, want ErrRejected", err)
	}
}

// TestTinyFishHTTPError asserts a non-2xx answer becomes the typed HTTP error.
func TestTinyFishHTTPError(t *testing.T) {
	fakeSleep(t) // the 502 is retried; do not wait out the SDK backoff
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		respondJSON(t, w, `{"error":"upstream"}`)
	})
	tk, err := NewTinyFishSearchToolkit("k", WithTinyFishBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewTinyFishSearchToolkit: %v", err)
	}
	_, cx := newTestContext()
	_, err = tk.search(callContext(cx), cx, "q")
	if !errors.Is(err, ErrHTTPStatus) {
		t.Fatalf("error = %v, want ErrHTTPStatus", err)
	}
}

// TestTinyFishMissingKey asserts the call fails without a key.
func TestTinyFishMissingKey(t *testing.T) {
	t.Setenv("TINYFISH_API_KEY", "")
	tk, err := NewTinyFishSearchToolkit("")
	if err != nil {
		t.Fatalf("NewTinyFishSearchToolkit: %v", err)
	}
	_, cx := newTestContext()
	_, err = tk.search(callContext(cx), cx, "q")
	if !errors.Is(err, ErrMissingKey) || !strings.Contains(err.Error(), "TINYFISH_API_KEY") {
		t.Fatalf("error = %v, want ErrMissingKey naming TINYFISH_API_KEY", err)
	}
}

// TestTinyFishMembers asserts the toolkit name and both Python tool names.
func TestTinyFishMembers(t *testing.T) {
	tk, err := NewTinyFishSearchToolkit("k")
	if err != nil {
		t.Fatalf("NewTinyFishSearchToolkit: %v", err)
	}
	if tk.Name() != "tinyfish_search_toolkit" || len(tk.Tools()) != 2 ||
		tk.Tools()[0].Name() != "tinyfish_search" || tk.Tools()[1].Name() != "tinyfish_fetch" {
		t.Fatalf("toolkit = %s with %v", tk.Name(), tk.Tools())
	}
}

// TestTinyFishOptionValidation asserts which option arguments are rejected and
// which valid ones are accepted, at construction and on the per-tool factories.
func TestTinyFishOptionValidation(t *testing.T) {
	bad := map[string]TinyFishOption{
		"empty base":         WithTinyFishBaseURL(""),
		"nil client":         WithTinyFishHTTPClient(nil),
		"empty key variable": WithTinyFishAPIKeyVariable(""),
		"nil middleware":     WithTinyFishMiddleware(nil),
		"empty param":        WithTinyFishVariable("", "v"),
		"empty variable":     WithTinyFishVariable("format", ""),
		"both empty":         WithTinyFishVariable("", ""),
		"negative retries":   WithTinyFishMaxRetries(-1),
	}
	for name, opt := range bad {
		if _, err := NewTinyFishSearchToolkit("k", opt); err == nil {
			t.Errorf("%s: option must be rejected at construction", name)
		}
		tk, err := NewTinyFishSearchToolkit("k")
		if err != nil {
			t.Fatalf("NewTinyFishSearchToolkit: %v", err)
		}
		if _, err := tk.Search(opt); err == nil {
			t.Errorf("%s: option must be rejected by the Search factory", name)
		}
		if _, err := tk.Fetch(opt); err == nil {
			t.Errorf("%s: option must be rejected by the Fetch factory", name)
		}
	}
	good := []TinyFishOption{
		WithTinyFishBaseURL("https://x.example"),
		WithTinyFishHTTPClient(&http.Client{}),
		WithTinyFishAPIKeyVariable("key"),
		WithTinyFishMiddleware(countingMiddleware{}),
		WithTinyFishVariable("format", "fmt_var"),
		WithTinyFishMaxRetries(0),
	}
	for i, opt := range good {
		if _, err := NewTinyFishSearchToolkit("k", opt); err != nil {
			t.Errorf("valid option #%d rejected: %v", i, err)
		}
	}
}

// TestTinyFishFetchAcceptsTenURLs asserts the upper bound of the SDK's 1-10 range
// is inclusive.
func TestTinyFishFetchAcceptsTenURLs(t *testing.T) {
	requests := 0
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		respondJSON(t, w, `{"results":[],"errors":[]}`)
	})
	tk, err := NewTinyFishSearchToolkit("k", WithTinyFishBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewTinyFishSearchToolkit: %v", err)
	}
	_, cx := newTestContext()
	urls := make([]string, 10)
	for i := range urls {
		urls[i] = "https://x.example/"
	}
	if _, err := tk.fetch(callContext(cx), cx, urls); err != nil || requests != 1 {
		t.Fatalf("fetch of 10 urls: err = %v, requests = %d", err, requests)
	}
}
