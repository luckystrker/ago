package zai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/tools"
)

func boolPtr(v bool) *bool { return &v }

func zaiClient(t *testing.T, srvURL string, mutate func(*ZAIConfig)) config.LLMClient {
	t.Helper()
	cfg := NewZAI("glm-4.7")
	cfg.BaseURL, cfg.APIKey, cfg.MaxRetries = srvURL, "test-key", 0
	if mutate != nil {
		mutate(cfg)
	}
	cl, err := cfg.Create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return cl
}

// TestRequestKnobsOnWire: every Z.AI knob reaches the body under its wire name.
func TestRequestKnobsOnWire(t *testing.T) {
	cases := map[string]struct {
		set  func(*ZAIConfig)
		key  string
		want any
	}{
		"stop":        {func(c *ZAIConfig) { c.Stop = []string{"END"} }, "stop", []any{"END"}},
		"seed":        {func(c *ZAIConfig) { c.Seed = i64ptr(5) }, "seed", float64(5)},
		"tool_choice": {func(c *ZAIConfig) { c.ToolChoice = "auto" }, "tool_choice", "auto"},
		"user_id":     {func(c *ZAIConfig) { c.UserID = "u-1" }, "user_id", "u-1"},
		"request_id":  {func(c *ZAIConfig) { c.RequestID = "r-1" }, "request_id", "r-1"},
		"do_sample":   {func(c *ZAIConfig) { c.DoSample = boolPtr(false) }, "do_sample", false},
		"meta":        {func(c *ZAIConfig) { c.Meta = map[string]string{"k": "v"} }, "meta", map[string]any{"k": "v"}},
		"extra_body":  {func(c *ZAIConfig) { c.ExtraBody = map[string]any{"thinking": map[string]any{"type": "enabled"}} }, "thinking", map[string]any{"type": "enabled"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _, _, _, captured := captureServer(t, http.StatusOK, zaiCompletionBody)
			cl := zaiClient(t, srv.URL, tc.set)
			if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
				t.Fatalf("call: %v", err)
			}
			got, ok := requestJSON(t, *captured)[tc.key]
			if !ok || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("body[%q] = %#v, want %#v", tc.key, got, tc.want)
			}
		})
	}

	// Unset knobs stay off the wire.
	srv, _, _, _, captured := captureServer(t, http.StatusOK, zaiCompletionBody)
	cl := zaiClient(t, srv.URL, nil)
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatal(err)
	}
	body := requestJSON(t, *captured)
	for _, k := range []string{"stop", "seed", "tool_choice", "user_id", "request_id", "do_sample", "meta"} {
		if _, ok := body[k]; ok {
			t.Errorf("unset knob %q was sent", k)
		}
	}
}

// TestTypedFieldsWinOverExtraBody: Python parity (_merge_extra_body) — the
// typed options overwrite a same-named extra_body key.
func TestTypedFieldsWinOverExtraBody(t *testing.T) {
	srv, _, _, _, captured := captureServer(t, http.StatusOK, zaiCompletionBody)
	cl := zaiClient(t, srv.URL, func(c *ZAIConfig) {
		c.Seed = i64ptr(1)
		c.Temperature = f64ptr(0.2)
		c.ExtraBody = map[string]any{"seed": 99, "temperature": 0.9, "other": "kept"}
	})
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatal(err)
	}
	body := requestJSON(t, *captured)
	if body["seed"] != float64(1) || body["temperature"] != 0.2 || body["other"] != "kept" {
		t.Fatalf("typed fields must win, untyped extras must stay: %+v", body)
	}
}

func TestExtraHeadersOnWire(t *testing.T) {
	srv, _, _, header, _ := captureServer(t, http.StatusOK, zaiCompletionBody)
	cl := zaiClient(t, srv.URL, func(c *ZAIConfig) { c.ExtraHeaders = map[string]string{"X-Trace": "t-9"} })
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatal(err)
	}
	if (*header).Get("X-Trace") != "t-9" || (*header).Get("Authorization") != "Bearer test-key" {
		t.Fatalf("headers: %v", *header)
	}
}

// TestTimeoutCapsRequest: the missing Z.AI request timeout now cuts off a
// server that never answers.
func TestTimeoutCapsRequest(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	cl := zaiClient(t, srv.URL, func(c *ZAIConfig) { c.Timeout = 50 * time.Millisecond })
	start := time.Now()
	_, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
	if err == nil {
		t.Fatal("a hung server must fail once Timeout elapses")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("call outlived the timeout by far: %v", elapsed)
	}
}

type countingTransport struct{ n atomic.Int32 }

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return http.DefaultTransport.RoundTrip(r)
}

func TestInjectedHTTPClientChatAndStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "text/event-stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"id\":\"x\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(zaiCompletionBody))
	}))
	t.Cleanup(srv.Close)
	rt := &countingTransport{}
	hc := &http.Client{Transport: rt}
	for _, streaming := range []bool{false, true} {
		cl := zaiClient(t, srv.URL, func(c *ZAIConfig) { c.HTTPClient, c.Streaming = hc, streaming })
		if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
			t.Fatalf("streaming=%v: %v", streaming, err)
		}
	}
	if rt.n.Load() != 2 {
		t.Fatalf("injected client saw %d requests, want 2", rt.n.Load())
	}
}

func TestAPIErrorTyped(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		srv, _, _, _, _ := captureServer(t, http.StatusTooManyRequests, `{"error":{"code":"1302","message":"rate limit"}}`)
		cl := zaiClient(t, srv.URL, func(c *ZAIConfig) { c.Streaming = streaming })
		_, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
		var apiErr *config.APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("streaming=%v: want *config.APIError, got %T: %v", streaming, err, err)
		}
		if apiErr.Provider != config.ProviderZAI || apiErr.StatusCode != 429 || !apiErr.Retryable || apiErr.Message != "rate limit" {
			t.Fatalf("streaming=%v: %+v", streaming, apiErr)
		}
		if !errors.Is(err, config.ErrRateLimited) {
			t.Fatalf("streaming=%v: errors.Is(ErrRateLimited) failed", streaming)
		}
	}
}

// TestRetrievalToolMapped: the retrieval schema reaches the wire in Z.AI's
// shape; the template is sent only when set.
func TestRetrievalToolMapped(t *testing.T) {
	srv, _, _, _, captured := captureServer(t, http.StatusOK, zaiCompletionBody)
	cl := zaiClient(t, srv.URL, nil)
	retrieval, _ := tools.NewRetrieval("kb-7", tools.WithRetrievalPromptTemplate("Use {{knowledge}}")).Schemas(nil)
	plain, _ := tools.NewRetrieval("kb-8").Schemas(nil)
	opts := config.CallOptions{Tools: []any{retrieval[0], plain[0]}}
	if _, err := cl.Call(context.Background(), nil, nil, opts); err != nil {
		t.Fatalf("call: %v", err)
	}
	wire, _ := requestJSON(t, *captured)["tools"].([]any)
	want := []any{
		map[string]any{"type": "retrieval", "retrieval": map[string]any{"knowledge_id": "kb-7", "prompt_template": "Use {{knowledge}}"}},
		map[string]any{"type": "retrieval", "retrieval": map[string]any{"knowledge_id": "kb-8"}},
	}
	if !reflect.DeepEqual(wire, want) {
		t.Fatalf("tools = %#v, want %#v", wire, want)
	}
}

// TestOtherHostedToolStillRejected: the hook handles retrieval only.
func TestOtherHostedToolStillRejected(t *testing.T) {
	srv, _, _, _, _ := captureServer(t, http.StatusOK, zaiCompletionBody)
	cl := zaiClient(t, srv.URL, nil)
	shell, _ := tools.NewShell().Schemas(nil)
	_, err := cl.Call(context.Background(), nil, nil, config.CallOptions{Tools: []any{shell[0]}})
	if !errors.Is(err, config.ErrUnsupportedTool) {
		t.Fatalf("want ErrUnsupportedTool, got %v", err)
	}
}

func TestCloneNewKnobs(t *testing.T) {
	hc := &http.Client{}
	cfg := NewZAI("glm-4.7")
	cp, err := cfg.Clone(map[string]any{
		"timeout": 3 * time.Second, "http_client": hc, "stop": []string{"x"}, "seed": 4, "tool_choice": "auto",
		"user_id": "u", "request_id": "r", "do_sample": true, "meta": map[string]string{"a": "b"},
		"extra_headers": map[string]string{"H": "v"}, "extra_body": map[string]any{"k": 1},
	})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	got := cp.(*ZAIConfig)
	if got.Timeout != 3*time.Second || got.HTTPClient != hc || got.Stop[0] != "x" || *got.Seed != 4 || got.ToolChoice != "auto" ||
		got.UserID != "u" || got.RequestID != "r" || !*got.DoSample || got.Meta["a"] != "b" || got.ExtraHeaders["H"] != "v" || got.ExtraBody["k"] != 1 {
		t.Fatalf("overrides lost: %+v", got)
	}
	if cfg.Timeout != 0 || cfg.Seed != nil {
		t.Fatal("original mutated")
	}
	if _, err := cfg.Clone(map[string]any{"timeout": 3}); err == nil {
		t.Fatal("wrongly typed override must fail")
	}
}
