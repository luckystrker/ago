package searchtools

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/luckystrker/ago/tools"
)

// TestSerplyWeb asserts the exact web request and its result payload.
func TestSerplyWeb(t *testing.T) {
	var (
		gotPath, gotQuery, gotKey, gotUA, gotLoc string
	)
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotKey = r.Header.Get("X-Api-Key")
		gotUA = r.Header.Get("User-Agent")
		gotLoc = r.Header.Get("X-Proxy-Location")
		respondJSON(t, w, `{"results":[
			{"title":"AGo","link":"https://ago.example/","description":"the port","position":1},
			{"title":"No position","link":"https://ago2.example/"}
		]}`)
	})
	tk, err := NewSerplySearchToolkit("sp-secret",
		WithSerplyBaseURL(srv.URL),
		WithSerplyNum(5),
		WithSerplyLocale("us", "en"),
		WithSerplyProxyLocation("DE"),
	)
	if err != nil {
		t.Fatalf("NewSerplySearchToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "serply_web_search", `{"query":"ago port"}`)

	if gotPath != "/v1/search/" {
		t.Fatalf("path = %s, want /v1/search/", gotPath)
	}
	if gotQuery != "gl=us&hl=en&num=5&q=ago+port" {
		t.Fatalf("query = %s", gotQuery)
	}
	if gotKey != "sp-secret" || gotUA != "ag2-serply-extension" || gotLoc != "DE" {
		t.Fatalf("headers = key %q, ua %q, location %q", gotKey, gotUA, gotLoc)
	}
	want := `{"query":"ago port","results":[` +
		`{"title":"AGo","link":"https://ago.example/","description":"the port","position":1},` +
		`{"title":"No position","link":"https://ago2.example/","description":""}]}`
	sameJSON(t, firstDataJSON(t, ev), want)
}

// TestSerplyNews asserts the news request (no num on the wire) and the
// client-side truncation.
func TestSerplyNews(t *testing.T) {
	var gotPath, gotQuery string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		respondJSON(t, w, `{"entries":[
			{"title":"One","link":"https://n1.example/","published":"2026-01-01","source":{"title":"Wire"}},
			{"title":"Two","link":"https://n2.example/"},
			{"title":"Three","link":"https://n3.example/"}
		]}`)
	})
	tk, err := NewSerplySearchToolkit("k", WithSerplyBaseURL(srv.URL), WithSerplyNum(2))
	if err != nil {
		t.Fatalf("NewSerplySearchToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "serply_news_search", `{"query":"ago"}`)
	if gotPath != "/v1/news/" || gotQuery != "q=ago" {
		t.Fatalf("request = %s?%s, want /v1/news/?q=ago", gotPath, gotQuery)
	}
	want := `{"query":"ago","results":[` +
		`{"title":"One","link":"https://n1.example/","published":"2026-01-01","source":"Wire"},` +
		`{"title":"Two","link":"https://n2.example/","published":"","source":""}]}`
	sameJSON(t, firstDataJSON(t, ev), want)
}

// TestSerplyNewsDefaultLimit asserts a tenth-entry cap when num is unset.
func TestSerplyNewsDefaultLimit(t *testing.T) {
	entries := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		entries = append(entries, `{"title":"t","link":"https://l.example/"}`)
	}
	body := `{"entries":[` + strings.Join(entries, ",") + `]}`
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		respondJSON(t, w, body)
	})
	tk, err := NewSerplySearchToolkit("k", WithSerplyBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewSerplySearchToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "serply_news_search", `{"query":"q"}`)
	var got SerplyNewsSearchResponse
	if err := json.Unmarshal([]byte(firstDataJSON(t, ev)), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Results) != 10 {
		t.Fatalf("results = %d, want the 10-entry cap", len(got.Results))
	}
}

// TestSerplyScholar asserts the scholar request and its nested field mapping.
func TestSerplyScholar(t *testing.T) {
	var gotPath string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		respondJSON(t, w, `{"articles":[
			{"title":"Paper","link":"https://p.example/","author":{"names":"A. Author"},
			 "doc":{"link":"https://p.example/p.pdf"},"extras":{"citations":{"count":42}}},
			{"title":"Bare","link":"https://p2.example/"}
		]}`)
	})
	tk, err := NewSerplySearchToolkit("k", WithSerplyBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewSerplySearchToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "serply_scholar_search", `{"query":"porting"}`)
	if gotPath != "/v1/scholar/" {
		t.Fatalf("path = %s", gotPath)
	}
	want := `{"query":"porting","results":[` +
		`{"title":"Paper","link":"https://p.example/","authors":"A. Author",` +
		`"pdf_link":"https://p.example/p.pdf","citations":42},` +
		`{"title":"Bare","link":"https://p2.example/","authors":"","pdf_link":""}]}`
	sameJSON(t, firstDataJSON(t, ev), want)
}

// TestSerplyHTTPError asserts a non-2xx answer becomes the typed HTTP error.
func TestSerplyHTTPError(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		respondJSON(t, w, `{"error":"denied"}`)
	})
	tk, err := NewSerplySearchToolkit("k", WithSerplyBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewSerplySearchToolkit: %v", err)
	}
	_, cx := newTestContext()
	_, err = tk.web(callContext(cx), cx, "q")
	if !errors.Is(err, ErrHTTPStatus) {
		t.Fatalf("error = %v, want ErrHTTPStatus", err)
	}
}

// TestSerplyMissingKey asserts the constructor rejects an empty key.
func TestSerplyMissingKey(t *testing.T) {
	if _, err := NewSerplySearchToolkit(""); !errors.Is(err, ErrMissingKey) {
		t.Fatalf("error = %v, want ErrMissingKey", err)
	}
}

// TestSerplyKeyVariable asserts the conversation-variable key seam.
func TestSerplyKeyVariable(t *testing.T) {
	var gotKey string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
		respondJSON(t, w, `{"results":[]}`)
	})
	tk, err := NewSerplySearchToolkit("", WithSerplyBaseURL(srv.URL), WithSerplyAPIKeyVariable("sp_key"))
	if err != nil {
		t.Fatalf("NewSerplySearchToolkit: %v", err)
	}
	_, cx := newTestContext()
	if _, err := tk.web(callContext(cx), cx, "q"); !errors.Is(err, tools.ErrNotFound) {
		t.Fatalf("error = %v, want a missing-variable error", err)
	}
	if err := cx.SetVariable("sp_key", "var-key"); err != nil {
		t.Fatalf("SetVariable: %v", err)
	}
	if _, err := tk.web(callContext(cx), cx, "q"); err != nil {
		t.Fatalf("web: %v", err)
	}
	if gotKey != "var-key" {
		t.Fatalf("key = %q", gotKey)
	}
}

// TestSerplyMembers asserts the toolkit registers the three Python tool names.
func TestSerplyMembers(t *testing.T) {
	tk, err := NewSerplySearchToolkit("k")
	if err != nil {
		t.Fatalf("NewSerplySearchToolkit: %v", err)
	}
	got := ""
	for _, member := range tk.Tools() {
		got += member.Name() + ","
	}
	if tk.Name() != "serply_search_toolkit" ||
		got != "serply_web_search,serply_news_search,serply_scholar_search," {
		t.Fatalf("toolkit = %s with %s", tk.Name(), got)
	}
}
