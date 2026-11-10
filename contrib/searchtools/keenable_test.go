package searchtools

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestKeenableSearch asserts the exact keyed request and result payload.
func TestKeenableSearch(t *testing.T) {
	var (
		gotMethod, gotPath, gotTitle, gotKey, gotBody string
	)
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotTitle = r.Header.Get("X-Keenable-Title")
		gotKey = r.Header.Get("X-API-Key")
		gotBody = readBody(t, r)
		respondJSON(t, w, `{"results":[
			{"title":"AGo","url":"https://ago.example/","snippet":"text","acquired_at":"2026-01-01"},
			{"title":"Fallback","url":"https://b.example/","description":"desc","acquired_at":7}
		]}`)
	})
	tk, err := NewKeenableSearchToolkit("kn-secret",
		WithKeenableBaseURL(srv.URL),
		WithKeenableMaxResults(5),
		WithKeenableSite("docs.example"),
		WithKeenablePublishedAfter("2026-01-01"),
		WithKeenableSnippetMaxLength(700),
	)
	if err != nil {
		t.Fatalf("NewKeenableSearchToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "keenable_search", `{"query":"ago"}`)

	if gotMethod != http.MethodPost || gotPath != "/v1/search" {
		t.Fatalf("request = %s %s, want POST /v1/search", gotMethod, gotPath)
	}
	if gotTitle != "ag2" || gotKey != "kn-secret" {
		t.Fatalf("headers = title %q, key %q", gotTitle, gotKey)
	}
	wantBody := `{"max_results":5,"published_after":"2026-01-01","query":"ago",` +
		`"site":"docs.example","snippet_max_length":700}`
	sameJSON(t, gotBody, wantBody)
	want := `{"query":"ago","results":[` +
		`{"title":"AGo","url":"https://ago.example/","snippet":"text","acquired_at":"2026-01-01"},` +
		`{"title":"Fallback","url":"https://b.example/","snippet":"desc","acquired_at":""}]}`
	sameJSON(t, firstDataJSON(t, ev), want)
}

// TestKeenableKeyless asserts the /public path, the absent key header and the
// default 1000-character snippet cap.
func TestKeenableKeyless(t *testing.T) {
	t.Setenv("KEENABLE_API_KEY", "")
	var gotPath, gotKey, gotBody string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-API-Key")
		gotBody = readBody(t, r)
		respondJSON(t, w, `{"results":[]}`)
	})
	tk, err := NewKeenableSearchToolkit("", WithKeenableBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewKeenableSearchToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "keenable_search", `{"query":"q"}`)
	if gotPath != "/v1/search/public" || gotKey != "" {
		t.Fatalf("request = %s with key %q, want the public keyless endpoint", gotPath, gotKey)
	}
	sameJSON(t, gotBody, `{"query":"q","snippet_max_length":1000}`)
	sameJSON(t, firstDataJSON(t, ev), `{"query":"q","results":[]}`)
}

// TestKeenableEnvKey asserts the environment variable raises the key.
func TestKeenableEnvKey(t *testing.T) {
	t.Setenv("KEENABLE_API_KEY", "env-key")
	var gotPath, gotKey string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-API-Key")
		respondJSON(t, w, `{}`)
	})
	tk, err := NewKeenableSearchToolkit("", WithKeenableBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewKeenableSearchToolkit: %v", err)
	}
	_, cx := newTestContext()
	if _, err := tk.search(callContext(cx), cx, "q"); err != nil {
		t.Fatalf("search: %v", err)
	}
	if gotPath != "/v1/search" || gotKey != "env-key" {
		t.Fatalf("request = %s with key %q, want the keyed endpoint", gotPath, gotKey)
	}
}

// TestKeenableFetch asserts the GET request and the timestamp rendering.
func TestKeenableFetch(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		respondJSON(t, w, `{"title":"Page","content":"# Body","author":"Ann","published_at":1767225600}`)
	})
	tk, err := NewKeenableSearchToolkit("k", WithKeenableBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewKeenableSearchToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "keenable_fetch", `{"url":"https://page.example/a"}`)
	if gotMethod != http.MethodGet || gotPath != "/v1/fetch" || gotQuery != "url=https%3A%2F%2Fpage.example%2Fa" {
		t.Fatalf("request = %s %s?%s", gotMethod, gotPath, gotQuery)
	}
	// The response omits url, so the requested URL stands in for it.
	want := `{"url":"https://page.example/a","title":"Page","content":"# Body",` +
		`"author":"Ann","published_at":"2026-01-01T00:00:00+00:00"}`
	sameJSON(t, firstDataJSON(t, ev), want)
}

// TestKeenableHTTPError asserts a non-2xx answer becomes the typed error with
// Keenable's own message and the keyless rate-limit hint.
func TestKeenableHTTPError(t *testing.T) {
	t.Setenv("KEENABLE_API_KEY", "")
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		respondJSON(t, w, `{"message":"quota reached"}`)
	})
	tk, err := NewKeenableSearchToolkit("", WithKeenableBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewKeenableSearchToolkit: %v", err)
	}
	_, cx := newTestContext()
	_, err = tk.search(callContext(cx), cx, "q")
	if !errors.Is(err, ErrHTTPStatus) {
		t.Fatalf("error = %v, want ErrHTTPStatus", err)
	}
	for _, want := range []string{"429", "quota reached", "Retry-After: 30", "KEENABLE_API_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q in it", err, want)
		}
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("error = %v, want an HTTPError with status 429", err)
	}
}

// TestKeenableMalformedPayload asserts a non-object body decodes to an empty
// result instead of failing the call.
func TestKeenableMalformedPayload(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		respondJSON(t, w, `[1,2,3]`)
	})
	tk, err := NewKeenableSearchToolkit("k", WithKeenableBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewKeenableSearchToolkit: %v", err)
	}
	_, cx := newTestContext()
	res, err := tk.search(callContext(cx), cx, "q")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Results) != 0 {
		t.Fatalf("results = %+v, want none", res.Results)
	}
}

// TestKeenableOptionValidation rejects nonsense at construction.
func TestKeenableOptionValidation(t *testing.T) {
	cases := map[string]KeenableOption{
		"empty base":   WithKeenableBaseURL(""),
		"nil client":   WithKeenableHTTPClient(nil),
		"max results":  WithKeenableMaxResults(51),
		"snippet":      WithKeenableSnippetMaxLength(0),
		"empty var":    WithKeenableVariable("site", ""),
		"empty keyvar": WithKeenableAPIKeyVariable(""),
	}
	for name, opt := range cases {
		if _, err := NewKeenableSearchToolkit("k", opt); err == nil {
			t.Fatalf("%s: option must be rejected", name)
		}
	}
}

// TestKeenableMembers asserts the toolkit name and both Python tool names.
func TestKeenableMembers(t *testing.T) {
	tk, err := NewKeenableSearchToolkit("")
	if err != nil {
		t.Fatalf("NewKeenableSearchToolkit: %v", err)
	}
	if tk.Name() != "keenable_search_toolkit" || len(tk.Tools()) != 2 ||
		tk.Tools()[0].Name() != "keenable_search" || tk.Tools()[1].Name() != "keenable_fetch" {
		t.Fatalf("toolkit = %s with %v", tk.Name(), tk.Tools())
	}
}
