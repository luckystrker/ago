package searchtools

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/luckystrker/ago/tools"
)

// TestExaSearch asserts the exact request the search tool sends and the exact
// payload the model receives.
func TestExaSearch(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotKey    string
		gotIntegr string
		gotBody   string
	)
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotIntegr = r.Header.Get("x-exa-integration")
		gotBody = readBody(t, r)
		respondJSON(t, w, `{"results":[
			{"title":"AGo","url":"https://ago.example/","score":0.5,"publishedDate":"2026-01-02","author":"Ann","text":"port"},
			{"title":null,"url":"https://ago2.example/"}
		]}`)
	})

	tk, err := NewExaToolkit("exa-secret", WithExaBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewExaToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "exa_search", `{"query":"ago port"}`)

	if gotMethod != http.MethodPost || gotPath != "/search" {
		t.Fatalf("request = %s %s, want POST /search", gotMethod, gotPath)
	}
	if gotKey != "exa-secret" || gotIntegr != "ag2" {
		t.Fatalf("headers = x-api-key %q, x-exa-integration %q", gotKey, gotIntegr)
	}
	wantBody := `{"contents":{"text":{"maxCharacters":10000}},"query":"ago port"}`
	if gotBody != wantBody {
		t.Fatalf("body = %s, want %s", gotBody, wantBody)
	}
	want := `{"query":"ago port","results":[` +
		`{"title":"AGo","url":"https://ago.example/","score":0.5,"published_date":"2026-01-02","author":"Ann","text":"port"},` +
		`{"title":"","url":"https://ago2.example/"}]}`
	sameJSON(t, firstDataJSON(t, ev), want)
}

// TestExaSearchFilters asserts that every optional filter reaches the request
// under its camelCase wire key.
func TestExaSearchFilters(t *testing.T) {
	var gotBody string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody = readBody(t, r)
		respondJSON(t, w, `{"results":[]}`)
	})
	tk, err := NewExaToolkit("k",
		WithExaBaseURL(srv.URL),
		WithExaNumResults(7),
		WithExaSearchType("deep"),
		WithExaCategory("news"),
		WithExaIncludeDomains("a.example", "b.example"),
		WithExaExcludeDomains("c.example"),
		WithExaPublishedDates("2026-01-01", "2026-02-01"),
		WithExaCrawlDates("2026-01-03", "2026-01-04"),
		WithExaLivecrawl("always"),
		WithExaMaxCharacters(500),
	)
	if err != nil {
		t.Fatalf("NewExaToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "exa_search", `{"query":"q"}`)
	if res := firstDataJSON(t, ev); res != `{"query":"q","results":[]}` {
		t.Fatalf("result = %s, want an empty result list", res)
	}
	want := `{"contents":{"livecrawl":"always","text":{"maxCharacters":500}},` +
		`"excludeDomains":["c.example"],"includeDomains":["a.example","b.example"],` +
		`"numResults":7,"query":"q","type":"deep","category":"news",` +
		`"startPublishedDate":"2026-01-01","endPublishedDate":"2026-02-01",` +
		`"startCrawlDate":"2026-01-03","endCrawlDate":"2026-01-04"}`
	sameJSON(t, gotBody, want)
}

// TestExaFindSimilar asserts the /findSimilar request and the bare result list.
func TestExaFindSimilar(t *testing.T) {
	var gotPath, gotBody string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody = readBody(t, r)
		respondJSON(t, w, `{"results":[{"title":"Near","url":"https://near.example/"}]}`)
	})
	tk, err := NewExaToolkit("k", WithExaBaseURL(srv.URL),
		WithExaNumResults(3), WithExaExcludeSourceDomain(true),
		WithExaIncludeDomains("near.example"))
	if err != nil {
		t.Fatalf("NewExaToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "exa_find_similar", `{"url":"https://src.example/"}`)
	if gotPath != "/findSimilar" {
		t.Fatalf("path = %s, want /findSimilar", gotPath)
	}
	wantBody := `{"contents":{"text":{"maxCharacters":10000}},"excludeSourceDomain":true,` +
		`"includeDomains":["near.example"],"numResults":3,"url":"https://src.example/"}`
	if gotBody != wantBody {
		t.Fatalf("body = %s, want %s", gotBody, wantBody)
	}
	if got := firstDataJSON(t, ev); got != `[{"title":"Near","url":"https://near.example/"}]` {
		t.Fatalf("result = %s, want a bare result list", got)
	}
}

// TestExaGetContents asserts the /contents request and the page payload.
func TestExaGetContents(t *testing.T) {
	var gotPath, gotBody string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody = readBody(t, r)
		respondJSON(t, w, `{"results":[
			{"url":"https://a.example/","title":"A","text":"body","author":"Ann","publishedDate":"2026-03-04"},
			{"url":"https://b.example/","title":null,"text":null}
		]}`)
	})
	tk, err := NewExaToolkit("k", WithExaBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewExaToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "exa_get_contents", `{"urls":["https://a.example/","https://b.example/"]}`)
	if gotPath != "/contents" || gotBody != `{"text":true,"urls":["https://a.example/","https://b.example/"]}` {
		t.Fatalf("request = %s %s", gotPath, gotBody)
	}
	want := `[{"url":"https://a.example/","title":"A","text":"body","author":"Ann","published_date":"2026-03-04"},` +
		`{"url":"https://b.example/","title":"","text":""}]`
	sameJSON(t, firstDataJSON(t, ev), want)
}

// TestExaAnswer asserts the /answer request and the answer payload.
func TestExaAnswer(t *testing.T) {
	var gotPath, gotBody string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody = readBody(t, r)
		respondJSON(t, w, `{"answer":"Paris","citations":[
			{"url":"https://fr.example/","title":"France","text":"capital"},
			{"url":"https://x.example/","title":null,"text":null}
		]}`)
	})
	tk, err := NewExaToolkit("k", WithExaBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewExaToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "exa_answer", `{"query":"capital of France?"}`)
	if gotPath != "/answer" || gotBody != `{"query":"capital of France?","text":true}` {
		t.Fatalf("request = %s %s", gotPath, gotBody)
	}
	want := `{"answer":"Paris","citations":[` +
		`{"url":"https://fr.example/","title":"France","text":"capital"},` +
		`{"url":"https://x.example/","title":"","text":""}]}`
	sameJSON(t, firstDataJSON(t, ev), want)
}

// TestExaToolkitMembers asserts the toolkit registers all four tools under the
// Python names.
func TestExaToolkitMembers(t *testing.T) {
	tk, err := NewExaToolkit("k")
	if err != nil {
		t.Fatalf("NewExaToolkit: %v", err)
	}
	names := []string{}
	for _, member := range tk.Tools() {
		names = append(names, member.Name())
	}
	want := "exa_search,exa_find_similar,exa_get_contents,exa_answer"
	if strings.Join(names, ",") != want || tk.Name() != "exa_toolkit" {
		t.Fatalf("toolkit = %s with %v, want %s", tk.Name(), names, want)
	}
}

// TestExaHTTPError asserts a non-2xx answer surfaces as the typed HTTP error.
func TestExaHTTPError(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		respondJSON(t, w, `{"error":"bad key"}`)
	})
	tk, err := NewExaToolkit("k", WithExaBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewExaToolkit: %v", err)
	}
	_, cx := newTestContext()
	_, err = tk.search(callContext(cx), cx, "q")
	if !errors.Is(err, ErrHTTPStatus) {
		t.Fatalf("error = %v, want ErrHTTPStatus", err)
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("error = %v, want an HTTPError with status 401", err)
	}
	// The model-facing path renders the same status.
	ev := callTool(t, tk, cx, "exa_search", `{"query":"q"}`)
	if msg := toolErrorText(t, ev); !strings.Contains(msg, "HTTP 401") {
		t.Fatalf("tool error = %q, want the status in the message", msg)
	}
}

// TestExaMissingKey asserts an absent key is reported before any request.
func TestExaMissingKey(t *testing.T) {
	t.Setenv("EXA_API_KEY", "")
	tk, err := NewExaToolkit("")
	if err != nil {
		t.Fatalf("NewExaToolkit: %v", err)
	}
	_, cx := newTestContext()
	_, err = tk.search(callContext(cx), cx, "q")
	if !errors.Is(err, ErrMissingKey) || !strings.Contains(err.Error(), "EXA_API_KEY") {
		t.Fatalf("error = %v, want ErrMissingKey naming EXA_API_KEY", err)
	}
}

// TestExaKeyVariableAndParamVariable asserts the conversation-variable seams:
// the key and a per-call default.
func TestExaKeyVariableAndParamVariable(t *testing.T) {
	var gotKey, gotBody string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotBody = readBody(t, r)
		respondJSON(t, w, `{"results":[]}`)
	})
	tk, err := NewExaToolkit("", WithExaBaseURL(srv.URL),
		WithExaAPIKeyVariable("exa_key"),
		WithExaVariable("num_results", "exa_num"))
	if err != nil {
		t.Fatalf("NewExaToolkit: %v", err)
	}
	_, cx := newTestContext()
	_, err = tk.search(callContext(cx), cx, "q")
	if !errors.Is(err, tools.ErrNotFound) {
		t.Fatalf("error = %v, want a missing-variable error", err)
	}
	if gotKey != "" {
		t.Fatalf("a request went out without the key: %q", gotKey)
	}
	if err := cx.SetVariable("exa_key", "from-var"); err != nil {
		t.Fatalf("SetVariable: %v", err)
	}
	if err := cx.SetVariable("exa_num", 4); err != nil {
		t.Fatalf("SetVariable: %v", err)
	}
	ev := callTool(t, tk, cx, "exa_search", `{"query":"q"}`)
	if gotKey != "from-var" {
		t.Fatalf("key = %q, want the variable value", gotKey)
	}
	want := `{"contents":{"text":{"maxCharacters":10000}},"numResults":4,"query":"q"}`
	if gotBody != want {
		t.Fatalf("body = %s, want %s", gotBody, want)
	}
	if got := firstDataJSON(t, ev); got != `{"query":"q","results":[]}` {
		t.Fatalf("result = %s", got)
	}
}

// TestExaOptionValidation rejects nonsense at construction.
func TestExaOptionValidation(t *testing.T) {
	cases := map[string]ExaOption{
		"empty base":    WithExaBaseURL(""),
		"nil client":    WithExaHTTPClient(nil),
		"num results":   WithExaNumResults(0),
		"search type":   WithExaSearchType("telepathic"),
		"category":      WithExaCategory("poetry"),
		"livecrawl":     WithExaLivecrawl("sometimes"),
		"empty var":     WithExaVariable("", "x"),
		"empty key var": WithExaAPIKeyVariable(""),
	}
	for name, opt := range cases {
		if _, err := NewExaToolkit("k", opt); err == nil {
			t.Fatalf("%s: option must be rejected", name)
		}
	}
}

// TestExaToolkitIsTool asserts the toolkit satisfies the framework contract.
func TestExaToolkitIsTool(t *testing.T) {
	tk, err := NewExaToolkit("k")
	if err != nil {
		t.Fatalf("NewExaToolkit: %v", err)
	}
	_, cx := newTestContext()
	schemas, err := tk.Schemas(cx)
	if err != nil {
		t.Fatalf("Schemas: %v", err)
	}
	if len(schemas) != 4 {
		t.Fatalf("schemas = %d, want 4", len(schemas))
	}
}
