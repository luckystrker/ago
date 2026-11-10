package policies

import (
	"strings"
	"testing"

	"github.com/luckystrker/ago/events"
)

// TestTokenBudgetPolicyCharsPerToken pins chars_per_token: the same history that
// fits under the default estimate exceeds the budget under a lower one.
func TestTokenBudgetPolicyCharsPerToken(t *testing.T) {
	long := &events.TextInput{Content: strings.Repeat("a", 20)} // 5 tokens default, 20 at cpt=1
	short := &events.TextInput{Content: "bbbb"}                 // 1 vs 4
	in := []events.Event{long, short}

	t.Run("default estimate fits", func(t *testing.T) {
		_, got, err := NewTokenBudgetPolicy(10).Apply(nil, in, nil)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("kept %d events, want both under the default estimate", len(got))
		}
	})

	t.Run("one char per token cuts", func(t *testing.T) {
		_, got, err := NewTokenBudgetPolicy(10, WithCharsPerToken(1)).Apply(nil, in, nil)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("kept %d events (%v), want only the newest", len(got), eventTypesOf(got))
		}
		if txt, ok := got[0].(*events.TextInput); !ok || txt.Content != "bbbb" {
			t.Fatalf("kept %+v, want the newest input", got[0])
		}
	})

	t.Run("non-positive keeps the default", func(t *testing.T) {
		_, got, err := NewTokenBudgetPolicy(10, WithCharsPerToken(0)).Apply(nil, in, nil)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("kept %d events, want the default estimate", len(got))
		}
	})
}
