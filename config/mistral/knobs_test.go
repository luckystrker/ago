package mistral

// Tests for the vendor extras added for D-021: guardrails, prediction,
// prompt_mode and logprobs.

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/luckystrker/ago/config"
)

func TestVendorExtrasOnWire(t *testing.T) {
	guardrails := []map[string]any{{"model": "moderation-llm-v2", "block_on_error": true}}
	cases := map[string]struct {
		set  func(*MistralConfig)
		key  string
		want any
	}{
		"guardrails": {
			func(c *MistralConfig) { c.Guardrails = guardrails },
			"guardrails", []any{map[string]any{"model": "moderation-llm-v2", "block_on_error": true}},
		},
		"prediction": {
			func(c *MistralConfig) { c.Prediction = map[string]any{"type": "content", "content": "hint"} },
			"prediction", map[string]any{"type": "content", "content": "hint"},
		},
		"prompt_mode": {
			func(c *MistralConfig) { c.PromptMode = "reasoning" },
			"prompt_mode", "reasoning",
		},
		"logprobs_true": {
			func(c *MistralConfig) { c.Logprobs = boolPtr(true) },
			"logprobs", true,
		},
		"logprobs_false": {
			// An explicit false is a setting, not an omission (Python parity:
			// logprobs: bool | None).
			func(c *MistralConfig) { c.Logprobs = boolPtr(false) },
			"logprobs", false,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _, _, _, captured := captureServer(t, http.StatusOK, mistralCompletionBody)
			cl := mistralClient(t, srv.URL, tc.set)
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
	srv, _, _, _, captured := captureServer(t, http.StatusOK, mistralCompletionBody)
	cl := mistralClient(t, srv.URL, nil)
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatal(err)
	}
	body := requestJSON(t, *captured)
	for _, k := range []string{"guardrails", "prediction", "prompt_mode", "logprobs"} {
		if _, ok := body[k]; ok {
			t.Errorf("unset extra %q was sent", k)
		}
	}
}

func TestCloneVendorExtras(t *testing.T) {
	cfg := NewMistral("mistral-large-latest")
	cp, err := cfg.Clone(map[string]any{
		"guardrails":  []map[string]any{{"model": "moderation-llm-v2"}},
		"prediction":  map[string]any{"type": "content"},
		"prompt_mode": "reasoning",
		"logprobs":    true,
	})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	got := cp.(*MistralConfig)
	if len(got.Guardrails) != 1 || got.Guardrails[0]["model"] != "moderation-llm-v2" ||
		!reflect.DeepEqual(got.Prediction, map[string]any{"type": "content"}) ||
		got.PromptMode != "reasoning" || got.Logprobs == nil || !*got.Logprobs {
		t.Fatalf("overrides lost: %+v", got)
	}
	if cfg.PromptMode != "" || cfg.Logprobs != nil || cfg.Guardrails != nil {
		t.Fatal("original mutated")
	}
	// The guardrail entries are copies: editing one must not reach the clone.
	got.Guardrails[0]["model"] = "other"
	if again, _ := cp.Clone(nil); again.(*MistralConfig).Guardrails[0]["model"] != "other" {
		t.Fatal("clone state is not stable")
	}
	if cfg.Guardrails != nil {
		t.Fatal("receiver must own no guardrail entries")
	}

	// nil clears; a slice of interface entries is accepted too.
	cleared, err := got.Clone(map[string]any{
		"guardrails": nil, "prediction": nil, "prompt_mode": nil, "logprobs": nil,
	})
	if err != nil {
		t.Fatalf("clone nil: %v", err)
	}
	got2 := cleared.(*MistralConfig)
	if got2.Guardrails != nil || got2.Prediction != nil || got2.PromptMode != "" || got2.Logprobs != nil {
		t.Fatalf("nil overrides must clear: %+v", got2)
	}
}

func TestCloneVendorExtrasValidation(t *testing.T) {
	cfg := NewMistral("mistral-large-latest")
	for key, bad := range map[string]any{
		"guardrails":  "all",
		"prediction":  []any{"x"},
		"logprobs":    "yes",
		"prompt_mode": 3,
	} {
		if _, err := cfg.Clone(map[string]any{key: bad}); err == nil {
			t.Fatalf("wrongly typed override %q must fail", key)
		}
	}
	// prompt_mode is Literal["reasoning"] in Python: any other value is a bug.
	if _, err := cfg.Clone(map[string]any{"prompt_mode": "chat"}); err == nil {
		t.Fatal("an unsupported prompt_mode must fail")
	}
}
