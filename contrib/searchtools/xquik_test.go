package searchtools

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/luckystrker/ago/tools"
)

// TestXquikTweetSearch asserts the exact request and result of the tweet search.
func TestXquikTweetSearch(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotQuery  string
		gotKey    string
	)
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotKey = r.Header.Get("x-api-key")
		respondJSON(t, w, `{
			"tweets":[{"id":"1","text":"hi"},{"id":"2","text":"there"}],
			"has_next_page":true,
			"next_cursor":"c2"
		}`)
	})
	tk, err := NewXquikSearchToolkit("xq-secret",
		WithXquikBaseURL(srv.URL),
		WithXquikQueryType("Latest"),
		WithXquikCursor("c1"),
		WithXquikTimeRange("2026-01-01", "2026-01-31"),
		WithXquikLimit(5),
	)
	if err != nil {
		t.Fatalf("NewXquikSearchToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "xquik_tweet_search", `{"query":"ago port"}`)

	if gotMethod != http.MethodGet || gotPath != "/api/v1/x/tweets/search" {
		t.Fatalf("request = %s %s", gotMethod, gotPath)
	}
	wantQuery := "cursor=c1&limit=5&q=ago+port&queryType=Latest&sinceTime=2026-01-01&untilTime=2026-01-31"
	if gotQuery != wantQuery {
		t.Fatalf("query = %s, want %s", gotQuery, wantQuery)
	}
	if gotKey != "xq-secret" {
		t.Fatalf("x-api-key = %q", gotKey)
	}
	want := `{"query":"ago port","tweets":[{"id":"1","text":"hi"},{"id":"2","text":"there"}],` +
		`"has_next_page":true,"next_cursor":"c2"}`
	sameJSON(t, firstDataJSON(t, ev), want)
}

// TestXquikTweetSearchDefaults asserts the minimal request: only q goes out, and
// a non-list tweets payload becomes an empty list with empty pagination.
func TestXquikTweetSearchDefaults(t *testing.T) {
	var gotQuery string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		respondJSON(t, w, `{"tweets":"not-a-list","has_next_page":1,"next_cursor":7}`)
	})
	tk, err := NewXquikSearchToolkit("k", WithXquikBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewXquikSearchToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "xquik_tweet_search", `{"query":"q"}`)
	if gotQuery != "q=q" {
		t.Fatalf("query = %q, want only q", gotQuery)
	}
	sameJSON(t, firstDataJSON(t, ev),
		`{"query":"q","tweets":[],"has_next_page":false,"next_cursor":""}`)
}

// TestXquikHTTPError asserts a non-2xx answer becomes the typed HTTP error.
func TestXquikHTTPError(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		respondJSON(t, w, `{"error":"slow down"}`)
	})
	tk, err := NewXquikSearchToolkit("k", WithXquikBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewXquikSearchToolkit: %v", err)
	}
	_, cx := newTestContext()
	_, err = tk.search(callContext(cx), cx, "q")
	if !errors.Is(err, ErrHTTPStatus) {
		t.Fatalf("error = %v, want ErrHTTPStatus", err)
	}
}

// TestXquikMissingKey asserts the constructor rejects an empty key.
func TestXquikMissingKey(t *testing.T) {
	if _, err := NewXquikSearchToolkit(""); !errors.Is(err, ErrMissingKey) {
		t.Fatalf("error = %v, want ErrMissingKey", err)
	}
}

// TestXquikKeyVariable asserts the conversation-variable key seam.
func TestXquikKeyVariable(t *testing.T) {
	var gotKey string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		respondJSON(t, w, `{"tweets":[]}`)
	})
	tk, err := NewXquikSearchToolkit("", WithXquikBaseURL(srv.URL), WithXquikAPIKeyVariable("xq_key"))
	if err != nil {
		t.Fatalf("NewXquikSearchToolkit: %v", err)
	}
	_, cx := newTestContext()
	_, err = tk.search(callContext(cx), cx, "q")
	if !errors.Is(err, tools.ErrNotFound) {
		t.Fatalf("error = %v, want a missing-variable error", err)
	}
	if err := cx.SetVariable("xq_key", "from-var"); err != nil {
		t.Fatalf("SetVariable: %v", err)
	}
	if _, err := tk.search(callContext(cx), cx, "q"); err != nil {
		t.Fatalf("search: %v", err)
	}
	if gotKey != "from-var" {
		t.Fatalf("x-api-key = %q, want the variable value", gotKey)
	}
}

// TestXquikMembers asserts the toolkit name and member.
func TestXquikMembers(t *testing.T) {
	tk, err := NewXquikSearchToolkit("k")
	if err != nil {
		t.Fatalf("NewXquikSearchToolkit: %v", err)
	}
	if tk.Name() != "xquik_search_toolkit" || len(tk.Tools()) != 1 ||
		tk.Tools()[0].Name() != "xquik_tweet_search" {
		t.Fatalf("toolkit = %s with %v", tk.Name(), tk.Tools())
	}
	if !strings.Contains(tk.Tools()[0].Name(), "tweet") {
		t.Fatalf("member = %s", tk.Tools()[0].Name())
	}
}
