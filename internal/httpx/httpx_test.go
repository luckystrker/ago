package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func shortBackoff(t *testing.T) {
	t.Helper()
	oldBase, oldMax := retryBaseDelay, maxRetryDelay
	retryBaseDelay = time.Millisecond
	maxRetryDelay = 2 * time.Millisecond
	t.Cleanup(func() { retryBaseDelay, maxRetryDelay = oldBase, oldMax })
}

func TestPostJSONSuccess(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	c := NewClient()
	status, data, err := c.PostJSON(context.Background(), srv.URL, map[string]string{"Authorization": "Bearer k"}, map[string]any{"q": 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d", status)
	}
	if gotAuth != "Bearer k" {
		t.Fatalf("Authorization header not sent: %q", gotAuth)
	}
	if gotCT != "application/json" {
		t.Fatalf("Content-Type not set: %q", gotCT)
	}
	if gotBody["q"] != float64(1) {
		t.Fatalf("request body not delivered: %+v", gotBody)
	}
	var resp map[string]any
	if err := json.Unmarshal(data, &resp); err != nil || resp["ok"] != true {
		t.Fatalf("bad response data: %s (%v)", data, err)
	}
}

func TestPostJSONRetriesOn500ThenSucceeds(t *testing.T) {
	shortBackoff(t)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "boom")
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	c := &Client{MaxRetries: 2}
	status, _, err := c.PostJSON(context.Background(), srv.URL, nil, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("want 200 after retry, got %d", status)
	}
	if calls != 2 {
		t.Fatalf("want 2 calls, got %d", calls)
	}
}

func TestPostJSONHonorsRetryAfter(t *testing.T) {
	shortBackoff(t)
	var calls int32
	start := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()

	// Retry-After is capped at maxRetryAfter; shrink the cap for the test.
	oldCap := maxRetryAfter
	maxRetryAfter = 30 * time.Millisecond
	t.Cleanup(func() { maxRetryAfter = oldCap })

	c := &Client{MaxRetries: 1}
	if _, _, err := c.PostJSON(context.Background(), srv.URL, nil, map[string]any{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if waited := time.Since(start); waited < 30*time.Millisecond {
		t.Fatalf("client ignored Retry-After, retried after %v", waited)
	}
}

func TestPostJSONNoRetryOn4xx(t *testing.T) {
	shortBackoff(t)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"bad request"}`)
	}))
	defer srv.Close()

	c := &Client{MaxRetries: 3}
	status, data, err := c.PostJSON(context.Background(), srv.URL, nil, map[string]any{})
	if err != nil {
		t.Fatalf("completed HTTP response must not be an error: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", status)
	}
	if !strings.Contains(string(data), "bad request") {
		t.Fatalf("body not returned: %s", data)
	}
	if calls != 1 {
		t.Fatalf("4xx must not retry, got %d calls", calls)
	}
}

func TestPostJSONRetriesExhaustedOn429(t *testing.T) {
	shortBackoff(t)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := &Client{MaxRetries: 2}
	status, _, err := c.PostJSON(context.Background(), srv.URL, nil, map[string]any{})
	if err != nil {
		t.Fatalf("the final HTTP response must be returned, not an error: %v", err)
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d", status)
	}
	if calls != 3 { // 1 initial + 2 retries
		t.Fatalf("want 3 calls, got %d", calls)
	}
}

func TestPostJSONRetriesOnNetworkError(t *testing.T) {
	shortBackoff(t)
	var calls int32
	// The handler resets the connection on the first call, then succeeds.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			panic(http.ErrAbortHandler)
		}
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()

	c := &Client{MaxRetries: 1}
	if _, _, err := c.PostJSON(context.Background(), srv.URL, nil, map[string]any{}); err != nil {
		t.Fatalf("network error should be retried: %v", err)
	}
	if calls != 2 {
		t.Fatalf("want 2 calls, got %d", calls)
	}
}

func TestPostJSONContextCanceled(t *testing.T) {
	shortBackoff(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &Client{MaxRetries: 3}
	if _, _, err := c.PostJSON(ctx, srv.URL, nil, map[string]any{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestPostJSONStreamDeliversDataChunks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("want Accept text/event-stream, got %q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"n\":1}\n\ndata: {\"n\":2}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	var chunks []string
	c := NewClient()
	err := c.PostJSONStream(context.Background(), srv.URL, nil, map[string]any{}, func(data []byte) error {
		chunks = append(chunks, string(data))
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{`{"n":1}`, `{"n":2}`, `[DONE]`}
	if len(chunks) != len(want) {
		t.Fatalf("want %d chunks, got %d: %v", len(want), len(chunks), chunks)
	}
	for i, w := range want {
		if chunks[i] != w {
			t.Fatalf("chunk %d: want %q, got %q", i, w, chunks[i])
		}
	}
}

func TestPostJSONStreamRetriesBeforeStreamOpens(t *testing.T) {
	shortBackoff(t)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: ok\n\n")
	}))
	defer srv.Close()

	var chunks []string
	c := &Client{MaxRetries: 1}
	if err := c.PostJSONStream(context.Background(), srv.URL, nil, map[string]any{}, func(data []byte) error {
		chunks = append(chunks, string(data))
		return nil
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 || chunks[0] != "ok" {
		t.Fatalf("bad chunks: %v", chunks)
	}
}

func TestPostJSONStreamNon2xxIsError(t *testing.T) {
	shortBackoff(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"bad key"}`)
	}))
	defer srv.Close()

	c := NewClient()
	err := c.PostJSONStream(context.Background(), srv.URL, nil, map[string]any{}, func([]byte) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "bad key") {
		t.Fatalf("want an error carrying status and body, got %v", err)
	}
}

func TestPostJSONStreamOnEventErrorStops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: a\n\ndata: b\n\n")
	}))
	defer srv.Close()

	myErr := errors.New("stop here")
	c := NewClient()
	var seen int32
	err := c.PostJSONStream(context.Background(), srv.URL, nil, map[string]any{}, func([]byte) error {
		if atomic.AddInt32(&seen, 1) == 2 {
			return myErr
		}
		return nil
	})
	if !errors.Is(err, myErr) {
		t.Fatalf("want the onEvent error unwrapped, got %v", err)
	}
	if seen != 2 {
		t.Fatalf("stream should stop at the second event, saw %d", seen)
	}
}

func TestPostJSONStreamIgnoresTimeout(t *testing.T) {
	// Timeout caps only non-streaming requests: a long generation streamed
	// past the cap must arrive whole (http.Client.Timeout would otherwise kill
	// the whole body read mid-stream).
	const events = 10
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		flusher.Flush()
		for i := 0; i < events; i++ {
			fmt.Fprintf(w, "data: {\"n\":%d}\n\n", i)
			flusher.Flush()
			time.Sleep(30 * time.Millisecond) // ~300ms total, well past Timeout
		}
	}))
	defer srv.Close()

	var got []int
	c := &Client{Timeout: 50 * time.Millisecond}
	err := c.PostJSONStream(context.Background(), srv.URL, nil, map[string]any{}, func(data []byte) error {
		var m struct {
			N int `json:"n"`
		}
		if err := json.Unmarshal(data, &m); err != nil {
			return err
		}
		got = append(got, m.N)
		return nil
	})
	if err != nil {
		t.Fatalf("stream must survive the client Timeout, got: %v", err)
	}
	if len(got) != events {
		t.Fatalf("want all %d events delivered, got %d: %v", events, len(got), got)
	}
}

func TestPostJSONTimeoutCapsNonStreaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	c := &Client{Timeout: 30 * time.Millisecond} // single try: MaxRetries 0
	if _, _, err := c.PostJSON(context.Background(), srv.URL, nil, map[string]any{}); err == nil {
		t.Fatal("the client Timeout must still cap a non-streaming request")
	}
}

// --- redirect policy ------------------------------------------------------------

func TestPostJSONStripsHeadersOnCrossHostRedirect(t *testing.T) {
	var gotAuth, gotAPIKey, gotGoogKey, gotCT, gotBody string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		gotGoogKey = r.Header.Get("x-goog-api-key")
		gotCT = r.Header.Get("Content-Type")
		gotBody = string(b)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer target.Close()

	// A 307 preserves method and body; a different port is a different
	// authority, which is what the policy must treat as cross-host.
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/echo", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	c := NewClient()
	status, data, err := c.PostJSON(context.Background(), source.URL, map[string]string{
		"Authorization":  "Bearer secret-bearer",
		"x-api-key":      "secret-anthropic",
		"x-goog-api-key": "secret-gemini",
	}, map[string]any{"q": 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK || !strings.Contains(string(data), "ok") {
		t.Fatalf("want the redirected answer, got status %d data %s", status, data)
	}
	if gotAuth != "" || gotAPIKey != "" || gotGoogKey != "" {
		t.Fatalf("credentials leaked to the redirected host: auth=%q x-api-key=%q x-goog-api-key=%q",
			gotAuth, gotAPIKey, gotGoogKey)
	}
	if gotCT != "application/json" {
		t.Fatalf("Content-Type must survive a 307 re-POST, got %q", gotCT)
	}
	if !strings.Contains(gotBody, `"q"`) {
		t.Fatalf("307 must re-send the request body, got %q", gotBody)
	}
}

func TestPostJSONSameHostRedirectKeepsHeaders(t *testing.T) {
	var gotAuth, gotAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/final" {
			http.Redirect(w, r, "/final", http.StatusTemporaryRedirect)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	c := NewClient()
	status, _, err := c.PostJSON(context.Background(), srv.URL, map[string]string{
		"Authorization": "Bearer k",
		"x-api-key":     "anthropic-key",
	}, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("want 200 after the same-host hop, got %d", status)
	}
	if gotAuth != "Bearer k" || gotAPIKey != "anthropic-key" {
		t.Fatalf("same-host redirect must keep credentials, got auth=%q x-api-key=%q", gotAuth, gotAPIKey)
	}
}

func TestPostJSONSameHostPolicyBlocksCrossHostRedirect(t *testing.T) {
	var targetHits int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&targetHits, 1)
		fmt.Fprint(w, `{}`)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	c := &Client{Redirect: RedirectSameHost}
	_, _, err := c.PostJSON(context.Background(), source.URL, nil, map[string]any{})
	if err == nil {
		t.Fatal("a cross-host redirect must fail under RedirectSameHost")
	}
	if !strings.Contains(err.Error(), "changes the host") {
		t.Fatalf("err = %v, want the host-change message", err)
	}
	if hits := atomic.LoadInt32(&targetHits); hits != 0 {
		t.Fatalf("the redirected host must not be contacted, got %d hits", hits)
	}
}

func TestPostJSONStopsAfterMaxRedirectHops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path+"x", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	c := &Client{} // MaxRetries 0: the transport error surfaces immediately
	_, _, err := c.PostJSON(context.Background(), srv.URL, nil, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("err = %v, want the hop-limit error", err)
	}
}

func TestCheckRedirectAuthorityComparison(t *testing.T) {
	pol := RedirectStripCredentials.CheckRedirect()
	orig, _ := http.NewRequest(http.MethodPost, "http://api.example.com/v1", nil)

	// Scheme upgrade on the same name (implicit ports) is not a host change.
	upgraded, _ := http.NewRequest(http.MethodPost, "https://api.example.com/v1", nil)
	if err := pol(upgraded, []*http.Request{orig}); err != nil {
		t.Fatalf("scheme upgrade on the same host must pass, got %v", err)
	}

	// An explicit default port equals the implicit one.
	explicit, _ := http.NewRequest(http.MethodPost, "https://api.example.com:443/v1", nil)
	if err := pol(explicit, []*http.Request{orig}); err != nil {
		t.Fatalf("explicit default port must pass, got %v", err)
	}

	// A different port on the same name is a different authority.
	otherPort, _ := http.NewRequest(http.MethodPost, "http://api.example.com:8080/v1", nil)
	req := otherPort
	req.Header.Set("x-api-key", "secret")
	if err := pol(req, []*http.Request{orig}); err != nil {
		t.Fatalf("policy must follow the redirect, got %v", err)
	}
	if got := req.Header.Get("x-api-key"); got != "" {
		t.Fatalf("credential must be stripped at the authority boundary, got %q", got)
	}
}

// --- response body cap ----------------------------------------------------------

func TestPostJSONResponseLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", 64))
	}))
	defer srv.Close()

	c := &Client{MaxResponseBytes: 16}
	_, _, err := c.PostJSON(context.Background(), srv.URL, nil, map[string]any{})
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("err = %v, want ErrResponseTooLarge", err)
	}

	// A body exactly at the cap passes through whole.
	c2 := &Client{MaxResponseBytes: 64}
	status, data, err := c2.PostJSON(context.Background(), srv.URL, nil, map[string]any{})
	if err != nil || status != http.StatusOK || len(data) != 64 {
		t.Fatalf("cap-boundary body must be returned: status=%d len=%d err=%v", status, len(data), err)
	}

	// A negative cap removes the limit.
	c3 := &Client{MaxResponseBytes: -1, MaxRetries: 0}
	if _, data, err := c3.PostJSON(context.Background(), srv.URL, nil, map[string]any{}); err != nil || len(data) != 64 {
		t.Fatalf("negative cap must disable the limit: len=%d err=%v", len(data), err)
	}
}

func TestPostJSONDefaultResponseLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), int(DefaultMaxResponseBytes)+1))
	}))
	defer srv.Close()

	c := NewClient() // MaxResponseBytes 0 → DefaultMaxResponseBytes
	_, _, err := c.PostJSON(context.Background(), srv.URL, nil, map[string]any{})
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("err = %v, want ErrResponseTooLarge at the default cap", err)
	}
}

func TestPostJSONTooLargeIsNotRetried(t *testing.T) {
	shortBackoff(t)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		fmt.Fprint(w, strings.Repeat("x", 32))
	}))
	defer srv.Close()

	c := &Client{MaxRetries: 2, MaxResponseBytes: 8}
	if _, _, err := c.PostJSON(context.Background(), srv.URL, nil, map[string]any{}); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("err = %v, want ErrResponseTooLarge", err)
	}
	if calls != 1 {
		t.Fatalf("an oversized answer must not be retried, got %d calls", calls)
	}
}
