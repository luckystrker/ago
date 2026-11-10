package middleware

import (
	"strings"
	"testing"

	"github.com/luckystrker/ago/events"
)

// TestTokenLimiterCharsPerToken pins the chars_per_token knob: with a lower
// estimate the same history costs more tokens, so the limiter cuts earlier.
func TestTokenLimiterCharsPerToken(t *testing.T) {
	long := textEv(strings.Repeat("a", 20))
	short := textEv("bbbb")
	evs := []events.Event{long, short}

	t.Run("default estimate fits", func(t *testing.T) {
		var saw []events.Event
		m := TokenLimiter(10)()
		if _, err := m.OnLLMCall(captureLLM(&saw), evs, newTestCtx(t)); err != nil {
			t.Fatalf("OnLLMCall: %v", err)
		}
		if len(saw) != 2 {
			t.Fatalf("kept %d events, want both under the default 4 chars/token", len(saw))
		}
	})

	t.Run("one char per token cuts", func(t *testing.T) {
		var saw []events.Event
		m := TokenLimiter(10, WithCharsPerToken(1))()
		if _, err := m.OnLLMCall(captureLLM(&saw), evs, newTestCtx(t)); err != nil {
			t.Fatalf("OnLLMCall: %v", err)
		}
		// 20 + 4 tokens > 10: only the newest event survives.
		if len(saw) != 1 {
			t.Fatalf("kept %d events, want only the newest", len(saw))
		}
		if got, ok := saw[0].(events.TextInput); !ok || got.Content != "bbbb" {
			t.Fatalf("kept %v, want the newest text", saw[0])
		}
	})

	t.Run("non-positive keeps the default", func(t *testing.T) {
		var saw []events.Event
		m := TokenLimiter(10, WithCharsPerToken(0))()
		if _, err := m.OnLLMCall(captureLLM(&saw), evs, newTestCtx(t)); err != nil {
			t.Fatalf("OnLLMCall: %v", err)
		}
		if len(saw) != 2 {
			t.Fatalf("kept %d events, want the default estimate", len(saw))
		}
	})
}
