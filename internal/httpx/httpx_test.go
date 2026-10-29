package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
