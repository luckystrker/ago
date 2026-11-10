package searchtools

import (
	stdctx "context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestHTTPErrorRendersBodyWhenNoMessage asserts the provider payload reaches the
// message when the provider supplies no dedicated message field.
func TestHTTPErrorRendersBodyWhenNoMessage(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		respondJSON(t, w, `{"error":"card declined"}`)
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
	if !strings.Contains(err.Error(), "card declined") {
		t.Fatalf("error = %q, want the response body in it", err)
	}
}

// TestRequestShape asserts the transport contract shared by every provider: no
// stray "?" on a query-less request, a JSON content type on a body, and an
// explicit nil context treated as Background.
func TestRequestShape(t *testing.T) {
	var (
		gotPath, gotQuery, gotContentType string
		gotForceQuery                     bool
		gotBody                           string
	)
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotForceQuery = r.URL.ForceQuery
		gotContentType = r.Header.Get("Content-Type")
		gotBody = readBody(t, r)
		respondJSON(t, w, `{"ok":true}`)
	})

	// A nil context must not panic and must not lose the request.
	if _, err := (httpRequest{
		provider: "test",
		method:   http.MethodPost,
		url:      srv.URL + "/body",
		body:     map[string]any{"a": 1},
	}).do(nil); err != nil {
		t.Fatalf("do with a nil context: %v", err)
	}
	if gotPath != "/body" || gotQuery != "" || gotForceQuery {
		t.Fatalf("request = %s?%s (force=%v), want no query string", gotPath, gotQuery, gotForceQuery)
	}
	if gotContentType != "application/json" {
		t.Fatalf("content type = %q, want application/json", gotContentType)
	}
	if gotBody != `{"a":1}` {
		t.Fatalf("body = %s", gotBody)
	}

	// A query-less GET keeps the endpoint untouched too.
	if _, err := (httpRequest{
		provider: "test",
		method:   http.MethodGet,
		url:      srv.URL + "/plain",
	}).do(stdctx.Background()); err != nil {
		t.Fatalf("do: %v", err)
	}
	if gotPath != "/plain" || gotQuery != "" || gotForceQuery {
		t.Fatalf("request = %s?%s (force=%v), want no query string", gotPath, gotQuery, gotForceQuery)
	}
	if gotContentType != "" {
		t.Fatalf("content type = %q, want none without a body", gotContentType)
	}
}

// TestStatusBoundaries asserts the accepted status window: 2xx passes, 3xx is a
// typed error (providers answer redirects instead of following them).
func TestStatusBoundaries(t *testing.T) {
	for _, status := range []int{http.StatusOK, 201, 299} {
		srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{}`)
		})
		if _, err := (httpRequest{provider: "test", method: http.MethodGet, url: srv.URL}).do(stdctx.Background()); err != nil {
			t.Fatalf("status %d: %v, want it accepted", status, err)
		}
	}
	for _, status := range []int{300, http.StatusNotModified, http.StatusInternalServerError} {
		srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{}`)
		})
		_, err := (httpRequest{provider: "test", method: http.MethodGet, url: srv.URL}).do(stdctx.Background())
		if !errors.Is(err, ErrHTTPStatus) {
			t.Fatalf("status %d: error = %v, want ErrHTTPStatus", status, err)
		}
	}
}

// TestTurnContextCancelsRequest asserts the conversation's turn context reaches
// the HTTP request, so a cancelled turn stops the provider call.
func TestTurnContextCancelsRequest(t *testing.T) {
	requests := 0
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		respondJSON(t, w, `{"tweets":[]}`)
	})
	tk, err := NewXquikSearchToolkit("k", WithXquikBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewXquikSearchToolkit: %v", err)
	}
	_, cx := newTestContext()
	turn, cancel := stdctx.WithCancel(stdctx.Background())
	cancel()
	cx.SetTurnContext(turn)
	_, err = tk.search(callContext(cx), cx, "q")
	if !errors.Is(err, stdctx.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want the cancelled call to stop before the server", requests)
	}
}
