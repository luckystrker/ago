package zai

// Tests for the Z.AI extras added for D-008/D-021: the thinking toggle,
// reasoning_effort, the vendor extras, and the token-cache opt-out with the
// source-channel header.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/ago/config"
)

// wantTokenTTLMillis is the documented Z.AI token lifetime (zai SDK:
// API_TOKEN_TTL_SECONDS = 210), pinned as a number so the constant itself is
// under test.
const wantTokenTTLMillis int64 = 210_000

func TestThinkingAndVendorExtrasOnWire(t *testing.T) {
	cases := map[string]struct {
		set  func(*ZAIConfig)
		key  string
		want any
	}{
		"thinking_on": {
			func(c *ZAIConfig) { c.Thinking = boolPtr(true) },
			"thinking", map[string]any{"type": "enabled"},
		},
		"thinking_off": {
			func(c *ZAIConfig) { c.Thinking = boolPtr(false) },
			"thinking", map[string]any{"type": "disabled"},
		},
		"reasoning_effort": {
			func(c *ZAIConfig) { c.ReasoningEffort = "high" },
			"reasoning_effort", "high",
		},
		"sensitive_word_check": {
			func(c *ZAIConfig) { c.SensitiveWordCheck = map[string]any{"type": "all"} },
			"sensitive_word_check", map[string]any{"type": "all"},
		},
		"watermark_enabled_false": {
			func(c *ZAIConfig) { c.WatermarkEnabled = boolPtr(false) },
			"watermark_enabled", false,
		},
		"tool_stream": {
			func(c *ZAIConfig) { c.ToolStream = boolPtr(true) },
			"tool_stream", true,
		},
		"extra": {
			func(c *ZAIConfig) { c.Extra = map[string]any{"geex": "on"} },
			"extra", map[string]any{"geex": "on"},
		},
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

	// Unset extras stay off the wire.
	srv, _, _, _, captured := captureServer(t, http.StatusOK, zaiCompletionBody)
	cl := zaiClient(t, srv.URL, nil)
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatal(err)
	}
	body := requestJSON(t, *captured)
	for _, k := range []string{"thinking", "reasoning_effort", "sensitive_word_check", "watermark_enabled", "tool_stream", "extra"} {
		if _, ok := body[k]; ok {
			t.Errorf("unset extra %q was sent", k)
		}
	}
}

// TestTypedThinkingWinsOverExtraBody: an extra_body "thinking" key loses to the
// typed toggle, like every other typed Z.AI field.
func TestTypedThinkingWinsOverExtraBody(t *testing.T) {
	srv, _, _, _, captured := captureServer(t, http.StatusOK, zaiCompletionBody)
	cl := zaiClient(t, srv.URL, func(c *ZAIConfig) {
		c.Thinking = boolPtr(true)
		c.ExtraBody = map[string]any{"thinking": map[string]any{"type": "disabled"}}
	})
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := requestJSON(t, *captured)["thinking"]; !reflect.DeepEqual(got, map[string]any{"type": "enabled"}) {
		t.Fatalf("typed thinking must win: %#v", got)
	}
}

// TestTokenCacheDefaultKeepsAPIKey: the Python default (disable_token_cache =
// true) sends the API key as the bearer token and identifies the client
// through x-source-channel.
func TestTokenCacheDefaultKeepsAPIKey(t *testing.T) {
	srv, _, _, header, _ := captureServer(t, http.StatusOK, zaiCompletionBody)
	cl := zaiClient(t, srv.URL, nil)
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := (*header).Get("Authorization"); got != "Bearer test-key" {
		t.Fatalf("authorization: %q", got)
	}
	if got := (*header).Get("x-source-channel"); got != DefaultSourceChannel {
		t.Fatalf("x-source-channel: %q, want %q", got, DefaultSourceChannel)
	}
}

// TestSourceChannelOverride: the caller can name its own channel.
func TestSourceChannelOverride(t *testing.T) {
	srv, _, _, header, _ := captureServer(t, http.StatusOK, zaiCompletionBody)
	cl := zaiClient(t, srv.URL, func(c *ZAIConfig) { c.SourceChannel = "gateway-7" })
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := (*header).Get("x-source-channel"); got != "gateway-7" {
		t.Fatalf("x-source-channel: %q", got)
	}
}

// TestTokenCacheEnabledMintsSignedJWT: disable_token_cache=false produces the
// HS256 token of the Python SDK — id in the claims, the part after the dot as
// the signing key, a 210 s lifetime.
func TestTokenCacheEnabledMintsSignedJWT(t *testing.T) {
	srv, _, _, header, _ := captureServer(t, http.StatusOK, zaiCompletionBody)
	cl := zaiClient(t, srv.URL, func(c *ZAIConfig) {
		c.APIKey = "kid01.secret-part"
		c.DisableTokenCache = boolPtr(false)
	})
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatal(err)
	}
	auth := (*header).Get("Authorization")
	token, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok {
		t.Fatalf("authorization: %q", auth)
	}
	pieces := strings.Split(token, ".")
	if len(pieces) != 3 {
		t.Fatalf("a JWT has three dot-separated parts: %q", token)
	}

	head := decodeSegment(t, pieces[0])
	if head["alg"] != "HS256" || head["sign_type"] != "SIGN" {
		t.Fatalf("header: %#v", head)
	}
	claims := decodeSegment(t, pieces[1])
	if claims["api_key"] != "kid01" {
		t.Fatalf("claims: %#v", claims)
	}
	exp, ts := int64(claims["exp"].(float64)), int64(claims["timestamp"].(float64))
	// The lifetime is pinned as a number: 210 s is the documented SDK TTL.
	if exp-ts != wantTokenTTLMillis {
		t.Fatalf("token lifetime = %d ms, want %d", exp-ts, wantTokenTTLMillis)
	}

	mac := hmac.New(sha256.New, []byte("secret-part"))
	mac.Write([]byte(pieces[0] + "." + pieces[1]))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if pieces[2] != want {
		t.Fatal("signature does not verify with the secret part of the key")
	}
}

// TestTokenCacheEnabledRequiresIDDotSecret: a key without the "<id>.<secret>"
// form cannot be signed and fails at Create, not on the wire.
func TestTokenCacheEnabledRequiresIDDotSecret(t *testing.T) {
	cfg := NewZAI("glm-4.7")
	cfg.BaseURL, cfg.APIKey, cfg.MaxRetries = "http://unused", "no-dot-here", 0
	cfg.DisableTokenCache = boolPtr(false)
	if _, err := cfg.Create(); !errors.Is(err, config.ErrNotProvided) {
		t.Fatalf("want ErrNotProvided, got %v", err)
	}
}

// TestSourceChannelUserHeaderWins: an ExtraHeaders entry of that name beats the
// typed field, whatever its spelling (net/http canonicalizes on the wire).
func TestSourceChannelUserHeaderWins(t *testing.T) {
	for _, key := range []string{"x-source-channel", "X-Source-Channel"} {
		t.Run(key, func(t *testing.T) {
			srv, _, _, header, _ := captureServer(t, http.StatusOK, zaiCompletionBody)
			cl := zaiClient(t, srv.URL, func(c *ZAIConfig) {
				c.SourceChannel = "typed-channel"
				c.ExtraHeaders = map[string]string{key: "user-channel"}
			})
			if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
				t.Fatal(err)
			}
			if got := (*header).Get("x-source-channel"); got != "user-channel" {
				t.Fatalf("x-source-channel = %q, want the user's value", got)
			}
		})
	}
}

// TestTokenMintedPerRequest: the bearer is minted on every call, so a turn that
// outlives the 210 s lifetime never sends an expired token (Python parity: the
// SDK mints per request and caches for 180 s).
func TestTokenMintedPerRequest(t *testing.T) {
	old := nowFunc
	base := time.UnixMilli(1_700_000_000_000)
	var elapsed time.Duration
	nowFunc = func() time.Time { return base.Add(elapsed) }
	t.Cleanup(func() { nowFunc = old })

	srv, _, _, header, _ := captureServer(t, http.StatusOK, zaiCompletionBody)
	cl := zaiClient(t, srv.URL, func(c *ZAIConfig) {
		c.APIKey = "kid01.secret-part"
		c.DisableTokenCache = boolPtr(false)
	})

	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	first := bearerToken(t, (*header).Get("Authorization"))
	firstClaims := decodeSegment(t, strings.Split(first, ".")[1])
	firstExp := int64(firstClaims["exp"].(float64))

	elapsed = 200 * time.Second
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatalf("second call: %v", err)
	}
	second := bearerToken(t, (*header).Get("Authorization"))
	secondClaims := decodeSegment(t, strings.Split(second, ".")[1])
	secondExp := int64(secondClaims["exp"].(float64))

	if first == second {
		t.Fatal("the second request reused the first token")
	}
	if got := secondExp - firstExp; got != 200*1000 {
		t.Fatalf("the second token's exp moved by %d ms, want 200000 (fresh token)", got)
	}
	if got := secondExp - int64(secondClaims["timestamp"].(float64)); got != wantTokenTTLMillis {
		t.Fatalf("the second token lives %d ms, want %d", got, wantTokenTTLMillis)
	}
}

// TestMintAuthTokenRejectsHalfKeys: both halves are mandatory — "id." (empty
// secret) and ".secret" (empty id) are refused.
func TestMintAuthTokenRejectsHalfKeys(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	for _, key := range []string{"id.", ".secret", "no-dot-here", "."} {
		if _, err := mintAuthToken(key, now); !errors.Is(err, config.ErrNotProvided) {
			t.Fatalf("mintAuthToken(%q) = %v, want ErrNotProvided", key, err)
		}
	}

	// The same validation holds at the config boundary.
	cfg := NewZAI("glm-4.7")
	cfg.BaseURL, cfg.APIKey, cfg.MaxRetries = "http://unused", "id.", 0
	cfg.DisableTokenCache = boolPtr(false)
	if _, err := cfg.Create(); !errors.Is(err, config.ErrNotProvided) {
		t.Fatalf("Create with half a key: want ErrNotProvided, got %v", err)
	}
}

func bearerToken(t *testing.T, auth string) string {
	t.Helper()
	token, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok {
		t.Fatalf("authorization: %q", auth)
	}
	return token
}

func decodeSegment(t *testing.T, seg string) map[string]any {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatalf("decoding token segment %q: %v", seg, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("segment %q is not JSON: %v", seg, err)
	}
	return m
}

// TestMintAuthTokenTTL: the lifetime is measured from the passed clock, so the
// claim arithmetic is pinned without depending on wall time.
func TestMintAuthTokenTTL(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	token, err := mintAuthToken("id.secret", now)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	claims := decodeSegment(t, strings.Split(token, ".")[1])
	if int64(claims["timestamp"].(float64)) != now.UnixMilli() ||
		int64(claims["exp"].(float64)) != now.UnixMilli()+wantTokenTTLMillis {
		t.Fatalf("claims: %#v", claims)
	}
}

// TestCloneVendorKnobs: every new field survives Clone and clears on nil.
func TestCloneVendorKnobs(t *testing.T) {
	cfg := NewZAI("glm-4.7")
	cp, err := cfg.Clone(map[string]any{
		"disable_token_cache":  false,
		"source_channel":       "gateway-7",
		"thinking":             true,
		"reasoning_effort":     "medium",
		"sensitive_word_check": map[string]any{"type": "all"},
		"watermark_enabled":    false,
		"tool_stream":          true,
		"extra":                map[string]any{"geex": "on"},
	})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	got := cp.(*ZAIConfig)
	if got.DisableTokenCache == nil || *got.DisableTokenCache ||
		got.SourceChannel != "gateway-7" ||
		got.Thinking == nil || !*got.Thinking ||
		got.ReasoningEffort != "medium" ||
		got.WatermarkEnabled == nil || *got.WatermarkEnabled ||
		got.ToolStream == nil || !*got.ToolStream {
		t.Fatalf("overrides lost: %+v", got)
	}
	if !reflect.DeepEqual(got.SensitiveWordCheck, map[string]any{"type": "all"}) ||
		!reflect.DeepEqual(got.Extra, map[string]any{"geex": "on"}) {
		t.Fatalf("untyped overrides lost: %+v", got)
	}
	if cfg.Thinking != nil || cfg.DisableTokenCache != nil {
		t.Fatal("original mutated")
	}

	cleared, err := got.Clone(map[string]any{
		"disable_token_cache": nil, "thinking": nil, "watermark_enabled": nil, "tool_stream": nil,
	})
	if err != nil {
		t.Fatalf("clone nil: %v", err)
	}
	got2 := cleared.(*ZAIConfig)
	if got2.DisableTokenCache != nil || got2.Thinking != nil || got2.WatermarkEnabled != nil || got2.ToolStream != nil {
		t.Fatalf("nil overrides must clear: %+v", got2)
	}
	if _, err := cfg.Clone(map[string]any{"thinking": "yes"}); err == nil {
		t.Fatal("wrongly typed override must fail")
	}
}
