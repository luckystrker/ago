package zai

import (
	"context"
	"fmt"

	"ago/config"
	agocontext "ago/context"
	"ago/events"
)

// chatClient is the LLMClient that ZAIConfig.Create returns: the shared
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
	_ config.ModelConfig = (*ZAIConfig)(nil)
	_ config.LLMClient   = (*chatClient)(nil)
)

// --- Clone field setters -------------------------------------------------------
// Each setter enforces the config's field type; a pointer field additionally
// accepts its value type (wrapped) and nil (cleared, Python None parity).

func setString(dst *string, name string, v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("config/zai: override %q: got %T, want string", name, v)
	}
	*dst = s
	return nil
}

func setInt(dst *int, name string, v any) error {
	n, ok := v.(int)
	if !ok {
		return fmt.Errorf("config/zai: override %q: got %T, want int", name, v)
	}
	*dst = n
	return nil
}

func setBool(dst *bool, name string, v any) error {
	b, ok := v.(bool)
	if !ok {
		return fmt.Errorf("config/zai: override %q: got %T, want bool", name, v)
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
		return fmt.Errorf("config/zai: override %q: got %T, want float64", name, v)
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
		return fmt.Errorf("config/zai: override %q: got %T, want int64", name, v)
	}
	return nil
}
