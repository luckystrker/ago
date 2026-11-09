package toolpacks

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luckystrker/ago/tools"
)

func TestDuckDuckGoSearch(t *testing.T) {
	var gotForm map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		gotForm = map[string]string{
			"q":  r.FormValue("q"),
			"kl": r.FormValue("kl"),
			"kp": r.FormValue("kp"),
		}
		_, _ = io.WriteString(w, `<html><body>
<div class="result">
  <h2 class="result__title"><a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fa&amp;rut=zz">Example &amp; <b>Title</b></a></h2>
  <a class="result__snippet" href="#">A <b>snippet</b> here</a>
</div>
<div class="result">
  <h2><a class="result__a" href="https://direct.example.com/">Direct Link</a></h2>
  <a class="result__snippet">Second snippet</a>
</div>
</body></html>`)
	}))
	defer srv.Close()

	d, err := NewDuckDuckGoSearch(WithDDGBaseURL(srv.URL), WithDDGMaxResults(5), WithDDGRegion("de-de"))
	if err != nil {
		t.Fatalf("NewDuckDuckGoSearch: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, d, cx, d.Name(), `{"query":"ago port"}`)
	if gotForm["q"] != "ago port" || gotForm["kl"] != "de-de" || gotForm["kp"] != "-1" {
		t.Fatalf("form = %v, want the query, region and moderate safesearch", gotForm)
	}
	var resp DDGSearchResponse
	if err := json.Unmarshal([]byte(firstDataJSON(t, ev)), &resp); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("results = %+v, want 2", resp.Results)
	}
	if resp.Results[0].Title != "Example & Title" {
		t.Fatalf("title = %q, want the stripped unescaped text", resp.Results[0].Title)
	}
	if resp.Results[0].Href != "https://example.com/a" {
		t.Fatalf("href = %q, want the unwrapped redirect target", resp.Results[0].Href)
	}
	if resp.Results[0].Body != "A snippet here" {
		t.Fatalf("body = %q", resp.Results[0].Body)
	}
	if resp.Results[1].Href != "https://direct.example.com/" {
		t.Fatalf("href = %q, want the plain URL passthrough", resp.Results[1].Href)
	}
}

// (firstDataJSON, the shared helper, lives in toolpacks_test.go.)

func TestTavilySearch(t *testing.T) {
	var (
		gotAuth   string
		gotBody   map[string]any
		gotMethod string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{
			"query": "ago port",
			"answer": "It is a Go port.",
			"results": [
				{"title": "AGo", "url": "https://ago.example/", "content": "The port.", "score": 0.91},
				{"title": "No score", "url": "https://ago2.example/", "content": "Also relevant."}
			],
			"images": ["https://img.example/1.png", {"url": "https://img.example/2.png"}]
		}`))
	}))
	defer srv.Close()

	tv, err := NewTavilySearch("tvly-secret",
		WithTavilyBaseURL(srv.URL), WithTavilyMaxResults(7), WithTavilyIncludeAnswer(true))
	if err != nil {
		t.Fatalf("NewTavilySearch: %v", err)
	}
	if _, err := NewTavilySearch(""); err == nil {
		t.Fatal("an empty key without the variable option must be rejected")
	}
	_, cx := newTestContext()
	ev := callTool(t, tv, cx, tv.Name(), `{"query":"ago port"}`)
	if gotMethod != http.MethodPost || gotAuth != "Bearer tvly-secret" {
		t.Fatalf("request = %s with auth %q", gotMethod, gotAuth)
	}
	if gotBody["query"] != "ago port" || gotBody["max_results"] != float64(7) || gotBody["include_answer"] != true {
		t.Fatalf("payload = %v", gotBody)
	}
	var resp TavilySearchResponse
	if err := json.Unmarshal([]byte(firstDataJSON(t, ev)), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Query != "ago port" || resp.Answer == nil || *resp.Answer != "It is a Go port." {
		t.Fatalf("response = %+v", resp)
	}
	if len(resp.Results) != 2 || resp.Results[0].Score == nil || *resp.Results[0].Score != 0.91 {
		t.Fatalf("results = %+v", resp.Results)
	}
	if len(resp.Results[1].Title) == 0 || resp.Results[1].Score != nil {
		t.Fatalf("second result = %+v, want an omitted score", resp.Results[1])
	}
	if len(resp.Images) != 2 || resp.Images[0] != "https://img.example/1.png" || resp.Images[1] != "https://img.example/2.png" {
		t.Fatalf("images = %v, want both shapes coerced", resp.Images)
	}
}

// TestTavilySearchOptionalResultFields pins the API's optional raw_content and
// favicon on their way to the model (upstream d503c8ddc8): present fields reach
// the tool result under their snake_case API names, absent ones are omitted.
func TestTavilySearchOptionalResultFields(t *testing.T) {
	strp := func(s string) *string { return &s }
	tests := []struct {
		name     string
		body     string
		wantRaw  *string
		wantIcon *string
	}{
		{
			name: "fields present",
			body: `{"query":"q","results":[
				{"title":"T","url":"https://a.example/","content":"c","score":0.8,
				 "raw_content":"# Full text","favicon":"https://a.example/favicon.ico"}
			]}`,
			wantRaw:  strp("# Full text"),
			wantIcon: strp("https://a.example/favicon.ico"),
		},
		{
			name: "fields absent",
			body: `{"query":"q","results":[
				{"title":"T","url":"https://a.example/","content":"c","score":0.8}
			]}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			tv, err := NewTavilySearch("tvly-secret", WithTavilyBaseURL(srv.URL))
			if err != nil {
				t.Fatalf("NewTavilySearch: %v", err)
			}
			_, cx := newTestContext()
			ev := callTool(t, tv, cx, tv.Name(), `{"query":"q"}`)
			data := firstDataJSON(t, ev)

			var resp TavilySearchResponse
			if err := json.Unmarshal([]byte(data), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(resp.Results) != 1 {
				t.Fatalf("results = %+v, want 1", resp.Results)
			}
			got := resp.Results[0]
			if !sameOptionalString(got.RawContent, tc.wantRaw) {
				t.Fatalf("raw_content = %v, want %v", got.RawContent, tc.wantRaw)
			}
			if !sameOptionalString(got.Favicon, tc.wantIcon) {
				t.Fatalf("favicon = %v, want %v", got.Favicon, tc.wantIcon)
			}

			var wire map[string]any
			if err := json.Unmarshal([]byte(data), &wire); err != nil {
				t.Fatalf("decode wire: %v", err)
			}
			first, ok := wire["results"].([]any)[0].(map[string]any)
			if !ok {
				t.Fatalf("results[0] shape = %T", wire["results"].([]any)[0])
			}
			for key, want := range map[string]*string{"raw_content": tc.wantRaw, "favicon": tc.wantIcon} {
				got, present := first[key]
				if want == nil {
					if present {
						t.Fatalf("%s = %v, want the key omitted", key, got)
					}
					continue
				}
				if !present || got != *want {
					t.Fatalf("%s = %v (present %v), want %q", key, got, present, *want)
				}
			}
		})
	}
}

func sameOptionalString(got, want *string) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return *got == *want
}

// tavilySearchBody drives one Tavily call against a fake API returning body and
// returns the model-facing response plus its wire JSON.
func tavilySearchBody(t *testing.T, body string) (TavilySearchResponse, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	tv, err := NewTavilySearch("tvly-secret", WithTavilyBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewTavilySearch: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tv, cx, tv.Name(), `{"query":"asked query"}`)
	data := firstDataJSON(t, ev)
	var resp TavilySearchResponse
	if err := json.Unmarshal([]byte(data), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp, data
}

// TestTavilySearchOptionalResultFieldEdges covers what presence alone leaves
// open: null decodes to nil and is dropped, empty strings are real values and
// survive, and one result's optional fields do not leak into another.
func TestTavilySearchOptionalResultFieldEdges(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []map[string]any // optional keys expected per result; absent means dropped
	}{
		{
			name: "null fields are dropped",
			body: `{"query":"q","results":[{"title":"T","url":"u","content":"c",
				"raw_content":null,"favicon":null}]}`,
			want: []map[string]any{{}},
		},
		{
			name: "empty fields are kept",
			body: `{"query":"q","results":[{"title":"T","url":"u","content":"c",
				"raw_content":"","favicon":""}]}`,
			want: []map[string]any{{"raw_content": "", "favicon": ""}},
		},
		{
			name: "results decode independently",
			body: `{"query":"q","results":[
				{"title":"A","url":"a","content":"c","raw_content":"full","favicon":"icon"},
				{"title":"B","url":"b","content":"c"}]}`,
			want: []map[string]any{{"raw_content": "full", "favicon": "icon"}, {}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, data := tavilySearchBody(t, tc.body)
			if len(resp.Results) != len(tc.want) {
				t.Fatalf("results = %d, want %d", len(resp.Results), len(tc.want))
			}
			var wire struct {
				Results []map[string]any `json:"results"`
			}
			if err := json.Unmarshal([]byte(data), &wire); err != nil {
				t.Fatalf("decode wire: %v", err)
			}
			if len(wire.Results) != len(tc.want) {
				t.Fatalf("wire results = %d, want %d", len(wire.Results), len(tc.want))
			}
			for i, want := range tc.want {
				for key, wantVal := range want {
					got, present := wire.Results[i][key]
					if !present || got != wantVal {
						t.Fatalf("results[%d].%s = %v (present %v), want %q", i, key, got, present, wantVal)
					}
				}
				for _, key := range []string{"raw_content", "favicon"} {
					if _, expected := want[key]; expected {
						continue
					}
					if got, present := wire.Results[i][key]; present {
						t.Fatalf("results[%d].%s = %v, want the key omitted", i, key, got)
					}
				}
			}
		})
	}
}

// TestTavilySearchQueryFallback pins upstream's raw.get("query", query)
// (d503c8ddc8 tavily.py): an echoed query — even the empty string — is
// authoritative, and only a missing key falls back to the request query.
func TestTavilySearchQueryFallback(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"api echo wins", `{"query":"api query","results":[]}`, "api query"},
		{"empty echo wins", `{"query":"","results":[]}`, ""},
		{"missing echo falls back", `{"results":[]}`, "asked query"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, _ := tavilySearchBody(t, tc.body)
			if resp.Query != tc.want {
				t.Fatalf("query = %q, want %q", resp.Query, tc.want)
			}
		})
	}
}

// TestTavilySearchSkipsUndecodableImageObject pins the strict object shape: an
// image entry whose object form fails to decode is dropped rather than emitted
// with a partial URL, while well-formed entries around it survive.
func TestTavilySearchSkipsUndecodableImageObject(t *testing.T) {
	resp, _ := tavilySearchBody(t, `{"query":"q","results":[],"images":[
		"https://img.example/1.png",
		{"url":"https://partial.example/","url":123},
		{"url":"https://img.example/2.png"}]}`)
	want := []string{"https://img.example/1.png", "https://img.example/2.png"}
	if strings.Join(resp.Images, "|") != strings.Join(want, "|") {
		t.Fatalf("images = %v, want %v", resp.Images, want)
	}
}

// TestNewTavilySearchOptionValidation pins the constructor's argument guards.
func TestNewTavilySearchOptionValidation(t *testing.T) {
	tests := []struct {
		name string
		opts []TavilyOption
	}{
		{"nil http client", []TavilyOption{WithTavilyHTTPClient(nil)}},
		{"zero max results", []TavilyOption{WithTavilyMaxResults(0)}},
		{"negative max results", []TavilyOption{WithTavilyMaxResults(-1)}},
		{"unknown search depth", []TavilyOption{WithTavilySearchDepth("deep")}},
		{"empty base URL", []TavilyOption{WithTavilyBaseURL("")}},
		{"unknown topic", []TavilyOption{WithTavilyTopic("sports")}},
		{"unknown include_raw_content string", []TavilyOption{WithTavilyIncludeRawContent("html")}},
		{"wrong include_raw_content type", []TavilyOption{WithTavilyIncludeRawContent(1)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewTavilySearch("tvly-secret", tc.opts...); err == nil {
				t.Fatal("expected a constructor error")
			}
		})
	}
}

// TestTavilySearchIncludeFlags pins the request-side flags that make the API
// return raw_content/favicon at all (upstream d503c8ddc8, tavily.py:94,104):
// include_raw_content takes true/false or "markdown"/"text", include_favicon a
// bool, and an unset option stays off the request like upstream's None filter.
func TestTavilySearchIncludeFlags(t *testing.T) {
	tests := []struct {
		name  string
		opts  []TavilyOption
		want  map[string]any
		unset []string
	}{
		{
			name: "raw content true and favicon",
			opts: []TavilyOption{WithTavilyIncludeRawContent(true), WithTavilyIncludeFavicon(true)},
			want: map[string]any{"include_raw_content": true, "include_favicon": true},
		},
		{
			name: "raw content markdown",
			opts: []TavilyOption{WithTavilyIncludeRawContent("markdown"), WithTavilyIncludeFavicon(true)},
			want: map[string]any{"include_raw_content": "markdown", "include_favicon": true},
		},
		{
			name: "raw content text",
			opts: []TavilyOption{WithTavilyIncludeRawContent("text")},
			want: map[string]any{"include_raw_content": "text"},
		},
		{
			name: "explicit false is sent",
			opts: []TavilyOption{WithTavilyIncludeRawContent(false), WithTavilyIncludeFavicon(false)},
			want: map[string]any{"include_raw_content": false, "include_favicon": false},
		},
		{
			name:  "unset options are omitted",
			unset: []string{"include_raw_content", "include_favicon"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &body)
				_, _ = io.WriteString(w, `{"query":"q","results":[]}`)
			}))
			defer srv.Close()

			opts := append([]TavilyOption{WithTavilyBaseURL(srv.URL)}, tc.opts...)
			tv, err := NewTavilySearch("tvly-secret", opts...)
			if err != nil {
				t.Fatalf("NewTavilySearch: %v", err)
			}
			_, cx := newTestContext()
			ev := callTool(t, tv, cx, tv.Name(), `{"query":"q"}`)
			if ev == nil {
				t.Fatal("no event")
			}
			for key, want := range tc.want {
				got, present := body[key]
				if !present || got != want {
					t.Fatalf("payload[%s] = %v (present %v), want %v", key, got, present, want)
				}
			}
			for _, key := range tc.unset {
				if got, present := body[key]; present {
					t.Fatalf("payload[%s] = %v, want the key omitted", key, got)
				}
			}
		})
	}
}

func TestTavilySearchKeyVariable(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"query":"q","results":[]}`))
	}))
	defer srv.Close()

	tv, err := NewTavilySearch("", WithTavilyBaseURL(srv.URL), WithTavilyAPIKeyVariable())
	if err != nil {
		t.Fatalf("NewTavilySearch: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tv, cx, tv.Name(), `{"query":"q"}`)
	if !strings.Contains(resultPartText(t, ev), "not provided") {
		t.Fatalf("missing variable = %q, want a not-provided error", resultPartText(t, ev))
	}
	cx.SetVariable("tavily_api_key", "var-key")
	ev = callTool(t, tv, cx, tv.Name(), `{"query":"q"}`)
	if gotAuth != "Bearer var-key" {
		t.Fatalf("auth = %q, want the conversation variable to supply the key", gotAuth)
	}
}

func TestPerplexitySearchAndAnswer(t *testing.T) {
	var searchAuth, answerAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		searchAuth = r.Header.Get("Authorization")
		if r.Header.Get("X-Pplx-Integration") == "" {
			t.Error("search: the integration header is missing")
		}
		_, _ = w.Write([]byte(`{"results":[
			{"title":"Hit","url":"https://p.example/","snippet":"Snip","date":"2026-01-02"}
		]}`))
	})
	mux.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		answerAuth = r.Header.Get("Authorization")
		var payload map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &payload)
		if payload["model"] != "sonar" {
			t.Errorf("answer model = %v", payload["model"])
		}
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"content":"42 corridors."}}],
			"search_results":[{"title":"Ref","url":"https://ref.example/"}],
			"citations":["https://ref.example/"],
			"images":[{"image_url":"https://img.example/a.png","origin_url":"https://origin.example/"}]
		}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p, err := NewPerplexitySearch("pplx-secret", WithPerplexityBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewPerplexitySearch: %v", err)
	}
	if _, err := NewPerplexitySearch(""); err == nil {
		t.Fatal("an empty key without the variable option must be rejected")
	}
	if p.Name() != "perplexity_toolkit" {
		t.Fatalf("name = %q", p.Name())
	}
	members := p.Tools()
	if len(members) != 2 || members[0].Name() != "perplexity_search" || members[1].Name() != "perplexity_answer" {
		t.Fatalf("members = %v", memberNames(members))
	}

	_, cx := newTestContext()
	ev := callTool(t, p, cx, "perplexity_search", `{"query":"ago","max_results":3}`)
	if searchAuth != "Bearer pplx-secret" {
		t.Fatalf("search auth = %q", searchAuth)
	}
	var sresp PerplexitySearchResponse
	if err := json.Unmarshal([]byte(firstDataJSON(t, ev)), &sresp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sresp.Results) != 1 || sresp.Results[0].Title != "Hit" || sresp.Results[0].Snippet != "Snip" {
		t.Fatalf("search response = %+v", sresp)
	}

	ev = callTool(t, p, cx, "perplexity_answer", `{"query":"how many?"}`)
	if answerAuth != "Bearer pplx-secret" {
		t.Fatalf("answer auth = %q", answerAuth)
	}
	var aresp PerplexitySearchResponse
	if err := json.Unmarshal([]byte(firstDataJSON(t, ev)), &aresp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if aresp.Content == nil || *aresp.Content != "42 corridors." {
		t.Fatalf("answer content = %+v", aresp.Content)
	}
	if len(aresp.Citations) != 1 || len(aresp.Images) != 1 || aresp.Images[0].ImageURL != "https://img.example/a.png" {
		t.Fatalf("answer response = %+v", aresp)
	}
}

func memberNames(ts []tools.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name())
	}
	return out
}
