package anthropic

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/tools"
)

// respondWith answers every request with a fixed JSON message body.
func respondWith(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}
}

// callOnce performs one headless call and returns the captured request body.
func callOnce(t *testing.T, cfg func(*AnthropicConfig)) map[string]any {
	t.Helper()
	srv, reqs := captureServer(t, respondWith(textResponse))
	cl := newTestClient(srv, cfg)
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("got %d requests", len(*reqs))
	}
	return (*reqs)[0]
}

// --- item 1: extended thinking -------------------------------------------------

func TestCallThinkingModes(t *testing.T) {
	cases := []struct {
		name  string
		set   func(*AnthropicConfig)
		want  map[string]any
		noKey bool
	}{
		{
			name: "enabled",
			set: func(c *AnthropicConfig) {
				c.Thinking = &ThinkingConfig{Type: ThinkingEnabled, BudgetTokens: 2048}
			},
			want: map[string]any{"type": "enabled", "budget_tokens": float64(2048)},
		},
		{
			name: "adaptive",
			set: func(c *AnthropicConfig) {
				c.Thinking = &ThinkingConfig{Type: ThinkingAdaptive}
			},
			want: map[string]any{"type": "adaptive"},
		},
		{
			name: "disabled",
			set: func(c *AnthropicConfig) {
				c.Thinking = &ThinkingConfig{Type: ThinkingDisabled}
			},
			want: map[string]any{"type": "disabled"},
		},
		{
			name:  "unset",
			set:   nil,
			noKey: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := callOnce(t, tc.set)
			if tc.noKey {
				if v, ok := body["thinking"]; ok {
					t.Fatalf("thinking must be absent, got %v", v)
				}
				return
			}
			got, ok := body["thinking"].(map[string]any)
			if !ok {
				t.Fatalf("thinking = %T %v", body["thinking"], body["thinking"])
			}
			if len(got) != len(tc.want) {
				t.Fatalf("thinking = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("thinking[%q] = %v, want %v", k, got[k], v)
				}
			}
		})
	}
}

// Thinking samples at temperature 1 only: a set temperature/top_p must not reach
// the wire, which is also what AG2 does (its API version drops the sampling
// parameters entirely).
func TestThinkingDropsSamplingParams(t *testing.T) {
	sampling := func(c *AnthropicConfig) {
		c.Temperature = f64ptr(0.3)
		c.TopP = f64ptr(0.8)
	}
	for _, typ := range []ThinkingType{ThinkingEnabled, ThinkingAdaptive} {
		t.Run(string(typ), func(t *testing.T) {
			body := callOnce(t, func(c *AnthropicConfig) {
				sampling(c)
				c.Thinking = &ThinkingConfig{Type: typ, BudgetTokens: 2048}
			})
			if v, ok := body["temperature"]; ok {
				t.Fatalf("temperature = %v, want it dropped with thinking on", v)
			}
			if v, ok := body["top_p"]; ok {
				t.Fatalf("top_p = %v, want it dropped with thinking on", v)
			}
		})
	}
	t.Run("disabled keeps them", func(t *testing.T) {
		body := callOnce(t, func(c *AnthropicConfig) {
			sampling(c)
			c.Thinking = &ThinkingConfig{Type: ThinkingDisabled}
		})
		if body["temperature"] != 0.3 || body["top_p"] != 0.8 {
			t.Fatalf("sampling = %v/%v, want them sent with thinking disabled", body["temperature"], body["top_p"])
		}
	})
}

// --- item 3: default headers and beta flags ------------------------------------

func TestDefaultHeadersAndBetaMerge(t *testing.T) {
	var gotBeta, gotCustom []string
	srv, reqs := captureServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotBeta = append(gotBeta, r.Header.Get("anthropic-beta"))
		gotCustom = append(gotCustom, r.Header.Get("x-custom"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, textResponse)
	})
	cl := newTestClient(srv, func(c *AnthropicConfig) {
		c.DefaultHeaders = map[string]string{
			"Anthropic-Beta": "fast-mode-2026-02-01",
			"X-Custom":       "custom-value",
		}
	})
	mcp := tools.MCPServerToolSchema{ServerURL: "https://mcp.example.com/x", ServerLabel: "x"}

	// Call 1: no framework beta, so the caller's value goes out unchanged.
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{Tools: []any{mcp}}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	// Call 2: the MCP connector adds its own beta; the caller's flag must survive.
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{Tools: []any{mcp}}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(*reqs) != 2 {
		t.Fatalf("got %d requests", len(*reqs))
	}
	if gotCustom[0] != "custom-value" {
		t.Fatalf("default header X-Custom = %q", gotCustom[0])
	}
	// Both calls carry the MCP tool, so both carry the union.
	for i, beta := range gotBeta {
		want := "fast-mode-2026-02-01," + mcpClientBeta
		if beta != want {
			t.Fatalf("request %d anthropic-beta = %q, want %q", i, beta, want)
		}
	}
}

func TestNoBetaHeaderWithoutBetas(t *testing.T) {
	var gotBeta string
	srv, _ := captureServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotBeta = r.Header.Get("anthropic-beta")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, textResponse)
	})
	cl := newTestClient(srv, nil)
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if gotBeta != "" {
		t.Fatalf("anthropic-beta = %q, want no header", gotBeta)
	}
}

// --- item 4: metadata / service_tier / speed ------------------------------------

func TestMetadataServiceTierAndSpeed(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		body := callOnce(t, func(c *AnthropicConfig) {
			c.Metadata = map[string]string{"user_id": "user-42"}
			c.ServiceTier = "standard_only"
			c.Speed = "fast"
		})
		meta, ok := body["metadata"].(map[string]any)
		if !ok || meta["user_id"] != "user-42" || len(meta) != 1 {
			t.Fatalf("metadata = %v", body["metadata"])
		}
		if body["service_tier"] != "standard_only" {
			t.Fatalf("service_tier = %v", body["service_tier"])
		}
		if body["speed"] != "fast" {
			t.Fatalf("speed = %v", body["speed"])
		}
	})
	t.Run("unset", func(t *testing.T) {
		body := callOnce(t, nil)
		for _, key := range []string{"metadata", "service_tier", "speed"} {
			if v, ok := body[key]; ok {
				t.Fatalf("%s = %v, want it absent", key, v)
			}
		}
	})
	t.Run("empty metadata map is sent", func(t *testing.T) {
		// Python's metadata field is tri-state: None omits it, {} is a value.
		body := callOnce(t, func(c *AnthropicConfig) { c.Metadata = map[string]string{} })
		meta, ok := body["metadata"].(map[string]any)
		if !ok || len(meta) != 0 {
			t.Fatalf("metadata = %v, want an empty object", body["metadata"])
		}
	})
}

// --- item 6: cache diagnostics --------------------------------------------------

const diagnosticsResponse = `{
  "id": "msg_diag",
  "model": "claude-sonnet-4",
  "content": [{"type": "text", "text": "Hi"}],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 1, "output_tokens": 1},
  "diagnostics": {"cache_miss_reason": {"type": "tools_changed", "cache_missed_input_tokens": 1792}}
}`

func TestCacheDiagnosticsRequestAndEvent(t *testing.T) {
	srv, reqs := captureServer(t, respondWith(diagnosticsResponse))
	conv, sent := newCaptureConv()
	cfg := NewAnthropic("claude-sonnet")
	cfg.APIKey = "test-key"
	cfg.BaseURL = srv.URL
	cfg.CacheDiagnostics = true
	cl, err := cfg.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := cl.Call(context.Background(), nil, conv, config.CallOptions{}); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if _, err := cl.Call(context.Background(), nil, conv, config.CallOptions{}); err != nil {
		t.Fatalf("call 2: %v", err)
	}
	if len(*reqs) != 2 {
		t.Fatalf("got %d requests", len(*reqs))
	}
	// The first call has nothing to compare against, the second names the first.
	first, _ := (*reqs)[0]["diagnostics"].(map[string]any)
	if v, ok := first["previous_message_id"]; !ok || v != nil {
		t.Fatalf("first diagnostics = %v, want previous_message_id null", first)
	}
	second, _ := (*reqs)[1]["diagnostics"].(map[string]any)
	if second["previous_message_id"] != "msg_diag" {
		t.Fatalf("second diagnostics = %v, want previous_message_id %q", second, "msg_diag")
	}

	var diags []CacheDiagnostics
	for _, ev := range *sent {
		if d, ok := events.AsValue(ev).(CacheDiagnostics); ok {
			diags = append(diags, d)
		}
	}
	if len(diags) != 2 {
		t.Fatalf("diagnostics events = %d, want 2", len(diags))
	}
	if diags[0].Reason != "tools_changed" || diags[0].CacheMissedInputTokens == nil ||
		*diags[0].CacheMissedInputTokens != 1792 {
		t.Fatalf("first diagnostics event = %+v", diags[0])
	}
	if diags[1].PreviousMessageID != "msg_diag" {
		t.Fatalf("second diagnostics compares against %q", diags[1].PreviousMessageID)
	}
}

func TestCacheDiagnosticsOffByDefault(t *testing.T) {
	body := callOnce(t, nil)
	if v, ok := body["diagnostics"]; ok {
		t.Fatalf("diagnostics = %v, want it absent without the knob", v)
	}
}

// A pending comparison (no cache_miss_reason) produces no event.
func TestCacheDiagnosticsPendingProducesNoEvent(t *testing.T) {
	srv, _ := captureServer(t, respondWith(`{"id":"msg_p","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","diagnostics":{}}`))
	conv, sent := newCaptureConv()
	cfg := NewAnthropic("claude-sonnet")
	cfg.APIKey = "test-key"
	cfg.BaseURL = srv.URL
	cfg.CacheDiagnostics = true
	cl, err := cfg.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := cl.Call(context.Background(), nil, conv, config.CallOptions{}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	for _, ev := range *sent {
		if _, ok := events.AsValue(ev).(CacheDiagnostics); ok {
			t.Fatalf("a pending comparison must not emit diagnostics: %+v", ev)
		}
	}
}

// --- item 5: per-input cache_control / citations --------------------------------

func TestInputVendorMetadata(t *testing.T) {
	image := func(meta map[string]any) events.Input {
		return &events.BinaryInput{Data: []byte{1}, MediaType: "image/png", Kind: events.BinaryImage, VendorMetadata: meta}
	}
	document := func(meta map[string]any) events.Input {
		return &events.BinaryInput{Data: []byte{1}, MediaType: "application/pdf", Kind: events.BinaryDocument, VendorMetadata: meta}
	}
	convert := func(t *testing.T, part events.Input) map[string]any {
		t.Helper()
		turns, err := ConvertMessages([]events.Event{&events.ModelRequest{Parts: []events.Input{part}}})
		if err != nil {
			t.Fatalf("ConvertMessages: %v", err)
		}
		return turns[0]["content"].([]map[string]any)[0]
	}

	t.Run("image cache_control", func(t *testing.T) {
		block := convert(t, image(map[string]any{"cache_control": map[string]any{"type": "ephemeral"}}))
		cc, ok := block["cache_control"].(map[string]any)
		if !ok || cc["type"] != "ephemeral" || len(cc) != 1 {
			t.Fatalf("cache_control = %v", block["cache_control"])
		}
	})
	t.Run("cache_control ttl passes through", func(t *testing.T) {
		block := convert(t, image(map[string]any{"cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"}}))
		cc, ok := block["cache_control"].(map[string]any)
		if !ok || cc["ttl"] != "1h" {
			t.Fatalf("cache_control = %v", block["cache_control"])
		}
	})
	t.Run("document citations", func(t *testing.T) {
		block := convert(t, document(map[string]any{"citations": map[string]any{"enabled": true}}))
		cit, ok := block["citations"].(map[string]any)
		if !ok || cit["enabled"] != true {
			t.Fatalf("citations = %v", block["citations"])
		}
	})
	t.Run("document cache_control and citations together", func(t *testing.T) {
		block := convert(t, document(map[string]any{
			"cache_control": map[string]any{"type": "ephemeral"},
			"citations":     map[string]any{"enabled": false},
		}))
		if _, ok := block["cache_control"]; !ok {
			t.Fatalf("cache_control missing: %v", block)
		}
		if cit, ok := block["citations"].(map[string]any); !ok || cit["enabled"] != false {
			t.Fatalf("citations = %v", block["citations"])
		}
	})
	t.Run("nil metadata reads as unset", func(t *testing.T) {
		block := convert(t, image(map[string]any{"cache_control": nil, "citations": nil}))
		for _, key := range []string{"cache_control", "citations"} {
			if v, ok := block[key]; ok {
				t.Fatalf("%s = %v, want it absent", key, v)
			}
		}
	})
	t.Run("image citations are refused", func(t *testing.T) {
		_, err := ConvertMessages([]events.Event{&events.ModelRequest{Parts: []events.Input{
			image(map[string]any{"citations": map[string]any{"enabled": true}}),
		}}})
		if !errors.Is(err, config.ErrUnsupportedInput) {
			t.Fatalf("want ErrUnsupportedInput, got %v", err)
		}
	})
	t.Run("invalid values are refused", func(t *testing.T) {
		cases := []map[string]any{
			{"cache_control": map[string]any{"type": "persistent"}},
			{"cache_control": map[string]any{"type": "ephemeral", "ttl": "24h"}},
			{"cache_control": map[string]any{"type": "ephemeral", "extra": 1}},
			{"cache_control": "ephemeral"},
			{"citations": map[string]any{"enabled": "yes"}},
			{"citations": map[string]any{"bogus": true}},
			{"citations": true},
		}
		for _, meta := range cases {
			if _, err := ConvertMessages([]events.Event{&events.ModelRequest{Parts: []events.Input{
				document(meta),
			}}}); !errors.Is(err, config.ErrUnsupportedInput) {
				t.Fatalf("metadata %v: want ErrUnsupportedInput, got %v", meta, err)
			}
		}
	})
}

// The input vendor metadata reaches the wire body, not just the mapper.
func TestRequestCarriesInputVendorMetadata(t *testing.T) {
	srv, reqs := captureServer(t, respondWith(textResponse))
	cl := newTestClient(srv, nil)
	_, err := cl.Call(context.Background(), []events.Event{&events.ModelRequest{Parts: []events.Input{
		&events.BinaryInput{
			Data:      []byte("%PDF-1"),
			MediaType: "application/pdf",
			Kind:      events.BinaryDocument,
			VendorMetadata: map[string]any{
				"cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"},
				"citations":     map[string]any{"enabled": true},
			},
		},
	}}}, nil, config.CallOptions{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	msgs := (*reqs)[0]["messages"].([]any)
	block := msgs[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	cc, ok := block["cache_control"].(map[string]any)
	if !ok || cc["type"] != "ephemeral" || cc["ttl"] != "1h" {
		t.Fatalf("cache_control on the wire = %v", block["cache_control"])
	}
	cit, ok := block["citations"].(map[string]any)
	if !ok || cit["enabled"] != true {
		t.Fatalf("citations on the wire = %v", block["citations"])
	}
}

// diagnosticsEvents collects the cache-miss reports one call put on the stream.
func diagnosticsEvents(sent []events.Event) []CacheDiagnostics {
	var out []CacheDiagnostics
	for _, ev := range sent {
		if d, ok := events.AsValue(ev).(CacheDiagnostics); ok {
			out = append(out, d)
		}
	}
	return out
}

// diagnosticsSSE builds one text stream; diag goes on the event named by at
// ("message_start" or "message_delta"), leaving it off elsewhere.
func diagnosticsSSE(at, diag string) string {
	startDiag, deltaDiag := "", ""
	switch at {
	case "message_start":
		startDiag = `,"diagnostics":` + diag
	case "message_delta":
		deltaDiag = `,"diagnostics":` + diag
	}
	return `event: message_start
data: {"type":"message_start","message":{"id":"msg_ss","model":"claude-sonnet-4","content":[],"usage":{"input_tokens":10,"output_tokens":1}` + startDiag + `}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}` + deltaDiag + `}

event: message_stop
data: {"type":"message_stop"}
`
}

// TestCacheDiagnosticsStreamChain: a streamed message_start diagnostic must
// become an event, and the next call on the same client must name the answer it
// compares against.
func TestCacheDiagnosticsStreamChain(t *testing.T) {
	diag := `{"cache_miss_reason":{"type":"messages_changed","cache_missed_input_tokens":42}}`
	srv, reqs := captureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, diagnosticsSSE("message_start", diag))
	})
	conv, sent := newCaptureConv()
	cfg := NewAnthropic("claude-sonnet")
	cfg.APIKey = "test-key"
	cfg.BaseURL = srv.URL
	cfg.Streaming = true
	cfg.CacheDiagnostics = true
	cl, err := cfg.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := cl.Call(context.Background(), nil, conv, config.CallOptions{}); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	diags := diagnosticsEvents(*sent)
	if len(diags) != 2 {
		t.Fatalf("diagnostics events = %d, want 2: %+v", len(diags), diags)
	}
	if diags[0].Reason != "messages_changed" || diags[0].CacheMissedInputTokens == nil ||
		*diags[0].CacheMissedInputTokens != 42 || diags[0].PreviousMessageID != "" {
		t.Fatalf("first diagnostics = %+v", diags[0])
	}
	if diags[1].PreviousMessageID != "msg_ss" {
		t.Fatalf("second diagnostics compares against %q, want msg_ss", diags[1].PreviousMessageID)
	}
	second, _ := (*reqs)[1]["diagnostics"].(map[string]any)
	if second["previous_message_id"] != "msg_ss" {
		t.Fatalf("second request diagnostics = %v", second)
	}
}

// The same report may arrive on message_delta instead of message_start.
func TestCacheDiagnosticsStreamFromMessageDelta(t *testing.T) {
	diag := `{"cache_miss_reason":{"type":"tools_changed","cache_missed_input_tokens":7}}`
	srv, _ := captureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, diagnosticsSSE("message_delta", diag))
	})
	conv, sent := newCaptureConv()
	cfg := NewAnthropic("claude-sonnet")
	cfg.APIKey = "test-key"
	cfg.BaseURL = srv.URL
	cfg.Streaming = true
	cfg.CacheDiagnostics = true
	cl, err := cfg.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := cl.Call(context.Background(), nil, conv, config.CallOptions{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	diags := diagnosticsEvents(*sent)
	if len(diags) != 1 || diags[0].Reason != "tools_changed" ||
		diags[0].CacheMissedInputTokens == nil || *diags[0].CacheMissedInputTokens != 7 {
		t.Fatalf("diagnostics = %+v, want one tools_changed report", diags)
	}
}

// The knob asks for diagnostics; a response that carries none reports nothing.
func TestCacheDiagnosticsEnabledWithoutReport(t *testing.T) {
	srv, _ := captureServer(t, respondWith(textResponse))
	conv, sent := newCaptureConv()
	cfg := NewAnthropic("claude-sonnet")
	cfg.APIKey = "test-key"
	cfg.BaseURL = srv.URL
	cfg.CacheDiagnostics = true
	cl, err := cfg.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := cl.Call(context.Background(), nil, conv, config.CallOptions{}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if diags := diagnosticsEvents(*sent); len(diags) != 0 {
		t.Fatalf("diagnostics = %+v, want none for a response without a report", diags)
	}
}

// A report the caller did not ask for is not turned into an event.
func TestCacheDiagnosticsReportIgnoredWhenOff(t *testing.T) {
	srv, _ := captureServer(t, respondWith(diagnosticsResponse))
	conv, sent := newCaptureConv()
	cfg := NewAnthropic("claude-sonnet")
	cfg.APIKey = "test-key"
	cfg.BaseURL = srv.URL
	cl, err := cfg.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := cl.Call(context.Background(), nil, conv, config.CallOptions{}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if diags := diagnosticsEvents(*sent); len(diags) != 0 {
		t.Fatalf("diagnostics = %+v, want none with the knob off", diags)
	}
}
