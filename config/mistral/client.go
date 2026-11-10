package mistral

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// chatClient is the LLMClient that MistralConfig.Create returns: the shared
// OpenAI-compatible chat engine bound to one validated parameter set.
// Immutable; safe for concurrent use.
type chatClient struct {
	params config.CompatParams
}

// Call performs one model call (config.LLMClient).
func (c *chatClient) Call(ctx context.Context, messages []events.Event, conv *agocontext.Context, opts config.CallOptions) (*events.ModelResponse, error) {
	return config.RunChatCompat(ctx, c.params, messages, conv, opts)
}

// Compile-time contract checks.
var (
	_ config.ModelConfig = (*MistralConfig)(nil)
	_ config.LLMClient   = (*chatClient)(nil)
)

// --- Clone field setters -------------------------------------------------------
// Each setter enforces the config's field type; a pointer field additionally
// accepts its value type (wrapped) and nil (cleared, Python None parity).

func setString(dst *string, name string, v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("config/mistral: override %q: got %T, want string", name, v)
	}
	*dst = s
	return nil
}

func setBool(dst *bool, name string, v any) error {
	b, ok := v.(bool)
	if !ok {
		return fmt.Errorf("config/mistral: override %q: got %T, want bool", name, v)
	}
	*dst = b
	return nil
}

func setF64(dst **float64, name string, v any) error {
	switch n := v.(type) {
	case nil:
		*dst = nil
	case *float64:
		*dst = n
	case float64:
		*dst = &n
	default:
		return fmt.Errorf("config/mistral: override %q: got %T, want float64", name, v)
	}
	return nil
}

func setI64(dst **int64, name string, v any) error {
	switch n := v.(type) {
	case nil:
		*dst = nil
	case *int64:
		*dst = n
	case int64:
		*dst = &n
	case int:
		n64 := int64(n)
		*dst = &n64
	default:
		return fmt.Errorf("config/mistral: override %q: got %T, want int64", name, v)
	}
	return nil
}

func setDuration(dst *time.Duration, name string, v any) error {
	d, ok := v.(time.Duration)
	if !ok {
		return fmt.Errorf("config/mistral: override %q: got %T, want time.Duration", name, v)
	}
	*dst = d
	return nil
}

func setHTTPClient(dst **http.Client, name string, v any) error {
	switch h := v.(type) {
	case nil:
		*dst = nil
	case *http.Client:
		*dst = h
	default:
		return fmt.Errorf("config/mistral: override %q: got %T, want *http.Client", name, v)
	}
	return nil
}

func setBoolPtr(dst **bool, name string, v any) error {
	switch b := v.(type) {
	case nil:
		*dst = nil
	case *bool:
		*dst = b
	case bool:
		*dst = &b
	default:
		return fmt.Errorf("config/mistral: override %q: got %T, want bool", name, v)
	}
	return nil
}

func setStrings(dst *[]string, name string, v any) error {
	switch s := v.(type) {
	case nil:
		*dst = nil
	case []string:
		*dst = s
	default:
		return fmt.Errorf("config/mistral: override %q: got %T, want []string", name, v)
	}
	return nil
}

func setStringMap(dst *map[string]string, name string, v any) error {
	switch m := v.(type) {
	case nil:
		*dst = nil
	case map[string]string:
		*dst = m
	default:
		return fmt.Errorf("config/mistral: override %q: got %T, want map[string]string", name, v)
	}
	return nil
}

func setAnyMap(dst *map[string]any, name string, v any) error {
	switch m := v.(type) {
	case nil:
		*dst = nil
	case map[string]any:
		*dst = m
	default:
		return fmt.Errorf("config/mistral: override %q: got %T, want map[string]any", name, v)
	}
	return nil
}

// setAnyMaps accepts a list of objects (the guardrails block); each entry is
// copied so a cloned config never shares the caller's maps.
func setAnyMaps(dst *[]map[string]any, name string, v any) error {
	var src []map[string]any
	switch m := v.(type) {
	case nil:
		*dst = nil
		return nil
	case []map[string]any:
		src = m
	case []any:
		src = make([]map[string]any, 0, len(m))
		for _, entry := range m {
			obj, ok := entry.(map[string]any)
			if !ok {
				return fmt.Errorf("config/mistral: override %q: got element %T, want map[string]any", name, entry)
			}
			src = append(src, obj)
		}
	default:
		return fmt.Errorf("config/mistral: override %q: got %T, want []map[string]any", name, v)
	}
	out := make([]map[string]any, len(src))
	for i, obj := range src {
		cp := make(map[string]any, len(obj))
		for k, val := range obj {
			cp[k] = val
		}
		out[i] = cp
	}
	*dst = out
	return nil
}
