package searchtools

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// step is one scripted answer: a status and body, or a transport error.
type step struct {
	status int
	body   string
	header http.Header
	err    error
	// bodyErr makes reading the response body fail with this error.
	bodyErr error
}

// scriptedTransport plays steps in order and repeats the last one; it records
// every request it saw.
type scriptedTransport struct {
	t      *testing.T
	steps  []step
	got    []recordedRequest
	onCall func(n int) // called with the 1-based request number before answering
}

func (s *scriptedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body string
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			s.t.Fatalf("read request body: %v", err)
		}
		body = string(b)
	}
	s.got = append(s.got, recordedRequest{method: req.Method, url: req.URL.String(), body: body})
	if s.onCall != nil {
		s.onCall(len(s.got))
	}
	st := s.steps[min(len(s.got), len(s.steps))-1]
	if st.err != nil {
		return nil, st.err
	}
	header := st.header
	if header == nil {
		header = http.Header{}
	}
	var respBody io.Reader = strings.NewReader(st.body)
	if st.bodyErr != nil {
		respBody = io.MultiReader(strings.NewReader("partial"), errReader{st.bodyErr})
	}
	return &http.Response{
		StatusCode: st.status,
		Header:     header,
		Body:       io.NopCloser(respBody),
		Request:    req,
	}, nil
}

// errReader fails every read with its error.
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// bodies returns the request bodies in order.
func (s *scriptedTransport) bodies() []string {
	out := make([]string, len(s.got))
	for i, g := range s.got {
		out[i] = g.body
	}
	return out
}

// fakeSleep replaces the backoff sleep with a recorder for the test's length.
func fakeSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	old := sleep
	sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return ctx.Err()
	}
	t.Cleanup(func() { sleep = old })
	return &waits
}

func seconds(v ...float64) []time.Duration {
	out := make([]time.Duration, len(v))
	for i, s := range v {
		out[i] = time.Duration(s * float64(time.Second))
	}
	return out
}

func okFetch() step {
	return step{status: 200, body: `{"results":[],"errors":[]}`}
}

const tinyfishOK = `{"query":"q","results":[],"total_results":7}`

func newTinyFishWith(t *testing.T, rt http.RoundTripper, opts ...TinyFishOption) *TinyFishSearchToolkit {
	t.Helper()
	all := append([]TinyFishOption{WithTinyFishHTTPClient(&http.Client{Transport: rt})}, opts...)
	tk, err := NewTinyFishSearchToolkit("k", all...)
	if err != nil {
		t.Fatalf("NewTinyFishSearchToolkit: %v", err)
	}
	return tk
}

func newFirecrawlWith(t *testing.T, rt http.RoundTripper, opts ...FirecrawlOption) *FirecrawlToolkit {
	t.Helper()
	all := append([]FirecrawlOption{WithFirecrawlHTTPClient(&http.Client{Transport: rt})}, opts...)
	tk, err := NewFirecrawlToolkit("k", all...)
	if err != nil {
		t.Fatalf("NewFirecrawlToolkit: %v", err)
	}
	return tk
}

// TestTinyFishRetriesRetryableStatuses asserts the SDK loop: two retries (three
// tries) on 408, 429 and every 5xx, with 0.5 s then 1 s of backoff, and the
// success of the third try is what the tool returns.
func TestTinyFishRetriesRetryableStatuses(t *testing.T) {
	for _, code := range []int{408, 429, 500, 502, 503, 599} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			waits := fakeSleep(t)
			rt := &scriptedTransport{t: t, steps: []step{
				{status: code, body: `{"error":{"message":"nope"}}`},
				{status: code},
				{status: 200, body: tinyfishOK},
			}}
			tk := newTinyFishWith(t, rt)
			_, cx := newTestContext()
			res, err := tk.search(callContext(cx), cx, "q")
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if res.TotalResults != 7 || len(rt.got) != 3 {
				t.Fatalf("total = %d after %d requests, want 7 after 3", res.TotalResults, len(rt.got))
			}
			if !reflect.DeepEqual(*waits, seconds(0.5, 1)) {
				t.Fatalf("backoff = %v, want 0.5s then 1s", *waits)
			}
		})
	}
}

// TestTinyFishRetriesTransportErrors asserts a connection failure is a retried
// failure, as httpx.RequestError is in the SDK.
func TestTinyFishRetriesTransportErrors(t *testing.T) {
	waits := fakeSleep(t)
	rt := &scriptedTransport{t: t, steps: []step{
		{err: errors.New("connection reset")},
		{status: 200, body: tinyfishOK},
	}}
	tk := newTinyFishWith(t, rt)
	_, cx := newTestContext()
	if _, err := tk.search(callContext(cx), cx, "q"); err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(rt.got) != 2 || !reflect.DeepEqual(*waits, seconds(0.5)) {
		t.Fatalf("requests = %d, backoff = %v", len(rt.got), *waits)
	}
}

// TestTinyFishGivesUpWithTheLastTypedError asserts that when every try fails the
// caller gets the last *HTTPError, after exactly three tries and two waits.
func TestTinyFishGivesUpWithTheLastTypedError(t *testing.T) {
	waits := fakeSleep(t)
	rt := &scriptedTransport{t: t, steps: []step{{status: 503}, {status: 429}, {status: 500, body: "last"}}}
	tk := newTinyFishWith(t, rt)
	_, cx := newTestContext()
	_, err := tk.search(callContext(cx), cx, "q")
	var httpErr *HTTPError
	if !errors.Is(err, ErrHTTPStatus) || !errors.As(err, &httpErr) || httpErr.StatusCode != 500 || httpErr.Body != "last" {
		t.Fatalf("error = %v, want the last HTTPError (500)", err)
	}
	if len(rt.got) != 3 || len(*waits) != 2 {
		t.Fatalf("requests = %d, waits = %v, want 3 and 2", len(rt.got), *waits)
	}
}

// TestTinyFishDoesNotRetryOtherStatuses asserts client errors fail at once.
func TestTinyFishDoesNotRetryOtherStatuses(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404, 409, 422} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			waits := fakeSleep(t)
			rt := &scriptedTransport{t: t, steps: []step{{status: code}}}
			tk := newTinyFishWith(t, rt)
			_, cx := newTestContext()
			_, err := tk.search(callContext(cx), cx, "q")
			if !errors.Is(err, ErrHTTPStatus) {
				t.Fatalf("error = %v, want ErrHTTPStatus", err)
			}
			if len(rt.got) != 1 || len(*waits) != 0 {
				t.Fatalf("requests = %d, waits = %v, want one request and no wait", len(rt.got), *waits)
			}
		})
	}
}

// TestTinyFishMaxRetries asserts the option scales the loop and the backoff
// doubles up to the 8 s cap.
func TestTinyFishMaxRetries(t *testing.T) {
	cases := []struct {
		retries  int
		requests int
		waits    []time.Duration
	}{
		{0, 1, nil},
		{1, 2, seconds(0.5)},
		{6, 7, seconds(0.5, 1, 2, 4, 8, 8)},
	}
	for _, c := range cases {
		waits := fakeSleep(t)
		rt := &scriptedTransport{t: t, steps: []step{{status: 500}}}
		tk := newTinyFishWith(t, rt, WithTinyFishMaxRetries(c.retries))
		_, cx := newTestContext()
		if _, err := tk.search(callContext(cx), cx, "q"); !errors.Is(err, ErrHTTPStatus) {
			t.Fatalf("retries=%d: error = %v", c.retries, err)
		}
		if len(rt.got) != c.requests || !reflect.DeepEqual(*waits, c.waits) {
			t.Fatalf("retries=%d: requests = %d, waits = %v; want %d and %v",
				c.retries, len(rt.got), *waits, c.requests, c.waits)
		}
	}
	if _, err := NewTinyFishSearchToolkit("k", WithTinyFishMaxRetries(-1)); err == nil {
		t.Fatal("a negative max_retries must be rejected")
	}
}

// TestTinyFishFetchRetryResendsBody asserts a retried POST carries the same body.
func TestTinyFishFetchRetryResendsBody(t *testing.T) {
	fakeSleep(t)
	rt := &scriptedTransport{t: t, steps: []step{{status: 503}, okFetch()}}
	tk := newTinyFishWith(t, rt)
	_, cx := newTestContext()
	callTool(t, tk, cx, "tinyfish_fetch", `{"urls":["https://a.example/"]}`)
	bodies := rt.bodies()
	if len(bodies) != 2 || bodies[0] == "" || bodies[0] != bodies[1] {
		t.Fatalf("bodies = %q, want the same non-empty body twice", bodies)
	}
}

// TestTinyFishCancelDuringBackoffStopsRetrying asserts a cancelled context ends
// the wait at once: no sleeping out the backoff, no further request.
func TestTinyFishCancelDuringBackoffStopsRetrying(t *testing.T) {
	rt := &scriptedTransport{t: t, steps: []step{{status: 503}}}
	tk := newTinyFishWith(t, rt)
	_, cx := newTestContext()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	start := time.Now()
	_, err := tk.search(ctx, cx, "q")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Fatalf("returned after %s, want it to leave the 500 ms backoff on cancel", elapsed)
	}
	if len(rt.got) != 1 {
		t.Fatalf("requests = %d, want 1", len(rt.got))
	}
}

// TestTinyFishCancelDuringRequestIsNotRetried asserts a transport error caused
// by the caller's own cancellation is final.
func TestTinyFishCancelDuringRequestIsNotRetried(t *testing.T) {
	waits := fakeSleep(t)
	ctx, cancel := context.WithCancel(context.Background())
	rt := &scriptedTransport{t: t, steps: []step{{err: context.Canceled}}}
	rt.onCall = func(int) { cancel() }
	tk := newTinyFishWith(t, rt)
	_, cx := newTestContext()
	_, err := tk.search(ctx, cx, "q")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if len(rt.got) != 1 || len(*waits) != 0 {
		t.Fatalf("requests = %d, waits = %v, want one request and no wait", len(rt.got), *waits)
	}
}

// TestFirecrawlRetriesOnlyBadGateway asserts the SDK's POST loop: three tries in
// total, repeated on 502 only, with 0.5 s then 1 s of backoff.
func TestFirecrawlRetriesOnlyBadGateway(t *testing.T) {
	waits := fakeSleep(t)
	rt := &scriptedTransport{t: t, steps: []step{
		{status: 502}, {status: 502}, {status: 200, body: `{"success":true,"data":{"web":[{"url":"https://a.example/"}]}}`},
	}}
	tk := newFirecrawlWith(t, rt)
	_, cx := newTestContext()
	res, err := tk.search(callContext(cx), cx, "q")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Web) != 1 || len(rt.got) != 3 {
		t.Fatalf("web = %d after %d requests, want 1 after 3", len(res.Web), len(rt.got))
	}
	if !reflect.DeepEqual(*waits, seconds(0.5, 1)) {
		t.Fatalf("backoff = %v, want 0.5s then 1s", *waits)
	}
	if bodies := rt.bodies(); bodies[0] == "" || bodies[0] != bodies[2] {
		t.Fatalf("a retried request must resend its body, got %q", bodies)
	}
}

// TestFirecrawlGivesUpWithTheBadGatewayError asserts that three 502s end in the
// typed error of the last answer, after two waits (the SDK returns the final
// 502 response without sleeping again).
func TestFirecrawlGivesUpWithTheBadGatewayError(t *testing.T) {
	waits := fakeSleep(t)
	rt := &scriptedTransport{t: t, steps: []step{{status: 502, body: "bad gateway"}}}
	tk := newFirecrawlWith(t, rt)
	_, cx := newTestContext()
	_, err := tk.search(callContext(cx), cx, "q")
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != 502 || httpErr.Body != "bad gateway" {
		t.Fatalf("error = %v, want the 502 HTTPError", err)
	}
	if len(rt.got) != 3 || len(*waits) != 2 {
		t.Fatalf("requests = %d, waits = %v, want 3 and 2", len(rt.got), *waits)
	}
}

// TestFirecrawlDoesNotRetryOtherStatuses asserts only 502 is retried: other 5xx,
// 429 and 408 (without the resume signal) are returned at once.
func TestFirecrawlDoesNotRetryOtherStatuses(t *testing.T) {
	for _, code := range []int{400, 401, 408, 429, 500, 503, 504} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			waits := fakeSleep(t)
			rt := &scriptedTransport{t: t, steps: []step{{status: code}}}
			tk := newFirecrawlWith(t, rt)
			_, cx := newTestContext()
			_, err := tk.search(callContext(cx), cx, "q")
			if !errors.Is(err, ErrHTTPStatus) {
				t.Fatalf("error = %v, want ErrHTTPStatus", err)
			}
			if len(rt.got) != 1 || len(*waits) != 0 {
				t.Fatalf("requests = %d, waits = %v, want one request and no wait", len(rt.got), *waits)
			}
		})
	}
}

// TestFirecrawlRetriesTransportErrors asserts connection failures use the same
// loop (httpx.HTTPError) and exhaustion surfaces the last cause.
func TestFirecrawlRetriesTransportErrors(t *testing.T) {
	waits := fakeSleep(t)
	cause := errors.New("connection reset")
	rt := &scriptedTransport{t: t, steps: []step{{err: cause}}}
	tk := newFirecrawlWith(t, rt)
	_, cx := newTestContext()
	_, err := tk.search(callContext(cx), cx, "q")
	if !errors.Is(err, cause) {
		t.Fatalf("error = %v, want the transport cause", err)
	}
	if len(rt.got) != 3 || !reflect.DeepEqual(*waits, seconds(0.5, 1)) {
		t.Fatalf("requests = %d, waits = %v, want 3 and 0.5s,1s", len(rt.got), *waits)
	}
}

// TestFirecrawlMaxRetries asserts the option counts total tries, like the SDK's
// max(1, retries), and the backoff is capped.
func TestFirecrawlMaxRetries(t *testing.T) {
	cases := []struct {
		tries    int
		requests int
		waits    []time.Duration
	}{
		{0, 1, nil},
		{1, 1, nil},
		{2, 2, seconds(0.5)},
		{9, 9, seconds(0.5, 1, 2, 4, 8, 16, 32, 60)},
	}
	for _, c := range cases {
		waits := fakeSleep(t)
		rt := &scriptedTransport{t: t, steps: []step{{status: 502}}}
		tk := newFirecrawlWith(t, rt, WithFirecrawlMaxRetries(c.tries))
		_, cx := newTestContext()
		if _, err := tk.search(callContext(cx), cx, "q"); !errors.Is(err, ErrHTTPStatus) {
			t.Fatalf("tries=%d: error = %v", c.tries, err)
		}
		if len(rt.got) != c.requests || !reflect.DeepEqual(*waits, c.waits) {
			t.Fatalf("tries=%d: requests = %d, waits = %v; want %d and %v",
				c.tries, len(rt.got), *waits, c.requests, c.waits)
		}
	}
	if _, err := NewFirecrawlToolkit("k", WithFirecrawlMaxRetries(-1)); err == nil {
		t.Fatal("a negative max_retries must be rejected")
	}
}

// TestFirecrawlCancelDuringBackoffStopsRetrying asserts cancellation ends the
// 502 backoff immediately.
func TestFirecrawlCancelDuringBackoffStopsRetrying(t *testing.T) {
	rt := &scriptedTransport{t: t, steps: []step{{status: 502}}}
	tk := newFirecrawlWith(t, rt)
	_, cx := newTestContext()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	start := time.Now()
	_, err := tk.search(ctx, cx, "q")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Fatalf("returned after %s, want it to leave the 500 ms backoff on cancel", elapsed)
	}
	if len(rt.got) != 1 {
		t.Fatalf("requests = %d, want 1", len(rt.got))
	}
}

const scrapeOK = `{"success":true,"data":{"markdown":"done","metadata":{"url":"https://p.example/"}}}`

func processing(extra string) step {
	return step{status: 408, body: `{"success":false,"code":"SCRAPE_TIMEOUT","details":{"state":"processing_continues"` + extra + `}}`}
}

// TestFirecrawlScrapeResumesProcessingDocuments asserts the auto-resume loop: a
// 408 SCRAPE_TIMEOUT/processing_continues answer is re-requested after the
// server's hint clamped to 5 s..10 min, the Retry-After header, or 60 s.
func TestFirecrawlScrapeResumesProcessingDocuments(t *testing.T) {
	retryAfter := processing("")
	retryAfter.header = http.Header{"Retry-After": {"12"}}
	badHeader := processing("")
	badHeader.header = http.Header{"Retry-After": {"soon"}}
	cases := []struct {
		name string
		step step
		want time.Duration
	}{
		{"body hint", processing(`,"retryAfterSeconds":7`), 7 * time.Second},
		{"hint below the floor", processing(`,"retryAfterSeconds":1`), 5 * time.Second},
		{"hint above the ceiling", processing(`,"retryAfterSeconds":99999`), 10 * time.Minute},
		{"boolean hint is not a number", processing(`,"retryAfterSeconds":true`), 60 * time.Second},
		{"header fallback", retryAfter, 12 * time.Second},
		{"unusable header", badHeader, 60 * time.Second},
		{"no hint", processing(""), 60 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			waits := fakeSleep(t)
			rt := &scriptedTransport{t: t, steps: []step{c.step, {status: 200, body: scrapeOK}}}
			tk := newFirecrawlWith(t, rt)
			_, cx := newTestContext()
			out, err := tk.scrape(callContext(cx), cx, "https://p.example/")
			if err != nil {
				t.Fatalf("scrape: %v", err)
			}
			if len(rt.got) != 2 || !reflect.DeepEqual(*waits, []time.Duration{c.want}) {
				t.Fatalf("requests = %d, waits = %v, want 2 and [%s]", len(rt.got), *waits, c.want)
			}
			if rt.got[0].body != rt.got[1].body {
				t.Fatalf("the resume must resend the same request, got %q", rt.bodies())
			}
			if res, ok := out.(FirecrawlScrapeResult); !ok || res.Markdown == nil || *res.Markdown != "done" {
				t.Fatalf("result = %#v, want the finished document", out)
			}
		})
	}
}

// TestFirecrawlScrapeResumeBounds asserts at most five resumes and 20 minutes of
// total waiting, after which the 408 is returned.
func TestFirecrawlScrapeResumeBounds(t *testing.T) {
	t.Run("five resumes", func(t *testing.T) {
		waits := fakeSleep(t)
		rt := &scriptedTransport{t: t, steps: []step{processing(`,"retryAfterSeconds":10`)}}
		tk := newFirecrawlWith(t, rt)
		_, cx := newTestContext()
		_, err := tk.scrape(callContext(cx), cx, "https://p.example/")
		var httpErr *HTTPError
		if !errors.As(err, &httpErr) || httpErr.StatusCode != 408 {
			t.Fatalf("error = %v, want the 408 HTTPError", err)
		}
		if len(rt.got) != 6 || len(*waits) != 5 {
			t.Fatalf("requests = %d, waits = %d, want 6 and 5", len(rt.got), len(*waits))
		}
	})
	t.Run("total wait", func(t *testing.T) {
		waits := fakeSleep(t)
		rt := &scriptedTransport{t: t, steps: []step{processing(`,"retryAfterSeconds":600`)}}
		tk := newFirecrawlWith(t, rt)
		_, cx := newTestContext()
		if _, err := tk.scrape(callContext(cx), cx, "https://p.example/"); !errors.Is(err, ErrHTTPStatus) {
			t.Fatalf("error = %v, want ErrHTTPStatus", err)
		}
		// 600 s + 600 s fits the 20 minute budget exactly; a third wait does not.
		if len(rt.got) != 3 || !reflect.DeepEqual(*waits, []time.Duration{10 * time.Minute, 10 * time.Minute}) {
			t.Fatalf("requests = %d, waits = %v, want 3 and two 10 minute waits", len(rt.got), *waits)
		}
	})
}

// TestFirecrawlScrapeDoesNotResumeOtherTimeouts asserts the resume needs the
// full signal: status 408, code SCRAPE_TIMEOUT and state processing_continues.
func TestFirecrawlScrapeDoesNotResumeOtherTimeouts(t *testing.T) {
	cases := map[string]step{
		"other code":    {status: 408, body: `{"code":"OTHER","details":{"state":"processing_continues"}}`},
		"other state":   {status: 408, body: `{"code":"SCRAPE_TIMEOUT","details":{"state":"failed"}}`},
		"no details":    {status: 408, body: `{"code":"SCRAPE_TIMEOUT"}`},
		"not json":      {status: 408, body: `timeout`},
		"other status":  {status: 504, body: `{"code":"SCRAPE_TIMEOUT","details":{"state":"processing_continues"}}`},
		"array payload": {status: 408, body: `[]`},
	}
	for name, st := range cases {
		t.Run(name, func(t *testing.T) {
			waits := fakeSleep(t)
			rt := &scriptedTransport{t: t, steps: []step{st}}
			tk := newFirecrawlWith(t, rt)
			_, cx := newTestContext()
			if _, err := tk.scrape(callContext(cx), cx, "https://p.example/"); !errors.Is(err, ErrHTTPStatus) {
				t.Fatalf("error = %v, want ErrHTTPStatus", err)
			}
			if len(rt.got) != 1 || len(*waits) != 0 {
				t.Fatalf("requests = %d, waits = %v, want one request and no wait", len(rt.got), *waits)
			}
		})
	}
}

// TestFirecrawlSearchDoesNotResume asserts the resume loop belongs to scrape only.
func TestFirecrawlSearchDoesNotResume(t *testing.T) {
	waits := fakeSleep(t)
	rt := &scriptedTransport{t: t, steps: []step{processing(`,"retryAfterSeconds":10`)}}
	tk := newFirecrawlWith(t, rt)
	_, cx := newTestContext()
	if _, err := tk.search(callContext(cx), cx, "q"); !errors.Is(err, ErrHTTPStatus) {
		t.Fatalf("error = %v, want ErrHTTPStatus", err)
	}
	if len(rt.got) != 1 || len(*waits) != 0 {
		t.Fatalf("requests = %d, waits = %v", len(rt.got), *waits)
	}
}

// TestFirecrawlScrapeCancelDuringResumeWait asserts a cancelled context ends the
// resume wait with the context error.
func TestFirecrawlScrapeCancelDuringResumeWait(t *testing.T) {
	rt := &scriptedTransport{t: t, steps: []step{processing(`,"retryAfterSeconds":10`)}}
	tk := newFirecrawlWith(t, rt)
	_, cx := newTestContext()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	start := time.Now()
	_, err := tk.scrape(ctx, cx, "https://p.example/")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("returned after %s, want it to leave the 10 s wait on cancel", elapsed)
	}
	if len(rt.got) != 1 {
		t.Fatalf("requests = %d, want 1", len(rt.got))
	}
}

// TestRetryCoversBodyReadFailures asserts a response whose body breaks mid-read
// is a transport failure (httpx.ReadError): retried while the context lives,
// final once the caller has cancelled.
func TestRetryCoversBodyReadFailures(t *testing.T) {
	t.Run("retried", func(t *testing.T) {
		waits := fakeSleep(t)
		rt := &scriptedTransport{t: t, steps: []step{
			{status: 200, bodyErr: io.ErrUnexpectedEOF},
			{status: 200, body: tinyfishOK},
		}}
		tk := newTinyFishWith(t, rt)
		_, cx := newTestContext()
		if _, err := tk.search(callContext(cx), cx, "q"); err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(rt.got) != 2 || !reflect.DeepEqual(*waits, seconds(0.5)) {
			t.Fatalf("requests = %d, waits = %v", len(rt.got), *waits)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		waits := fakeSleep(t)
		ctx, cancel := context.WithCancel(context.Background())
		rt := &scriptedTransport{t: t, steps: []step{{status: 200, bodyErr: io.ErrUnexpectedEOF}}}
		rt.onCall = func(int) { cancel() }
		tk := newTinyFishWith(t, rt)
		_, cx := newTestContext()
		_, err := tk.search(ctx, cx, "q")
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("error = %v, want the read failure", err)
		}
		if len(rt.got) != 1 || len(*waits) != 0 {
			t.Fatalf("requests = %d, waits = %v, want one request and no wait", len(rt.got), *waits)
		}
	})
}

// TestDoublingBackoff pins the shared backoff curve and its cap.
func TestDoublingBackoff(t *testing.T) {
	cases := []struct {
		failed int
		limit  time.Duration
		want   time.Duration
	}{
		{1, 8 * time.Second, 500 * time.Millisecond},
		{2, 8 * time.Second, time.Second},
		{5, 8 * time.Second, 8 * time.Second},
		{6, 8 * time.Second, 8 * time.Second},
		{5, time.Minute, 8 * time.Second},
		{8, time.Minute, time.Minute},
		{1000, time.Minute, time.Minute},
	}
	for _, c := range cases {
		if got := doublingBackoff(c.failed, c.limit); got != c.want {
			t.Errorf("doublingBackoff(%d, %s) = %s, want %s", c.failed, c.limit, got, c.want)
		}
	}
}
