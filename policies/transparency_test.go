package policies

import (
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/knowledge"
)

// TestSlidingWindowTransparency pins the injected note: Python's wording, with
// the retained and total conversational counts.
func TestSlidingWindowTransparency(t *testing.T) {
	evs := []events.Event{
		&events.TextInput{Content: "a"},
		&events.TextInput{Content: "b"},
		&events.TextInput{Content: "c"},
		&events.TextInput{Content: "d"},
	}
	prompts, retained, err := NewSlidingWindowPolicy(2, WithWindowTransparency(true)).Apply([]string{"sys"}, evs, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(retained) != 2 {
		t.Fatalf("retained = %d, want 2", len(retained))
	}
	want := "[sliding_window] Showing last 2 of 4 events."
	if len(prompts) != 2 || prompts[1] != want {
		t.Fatalf("prompts = %q, want %q appended", prompts, want)
	}

	// No trim, no note.
	prompts, _, err = NewSlidingWindowPolicy(10, WithWindowTransparency(true)).Apply([]string{"sys"}, evs, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(prompts) != 1 {
		t.Fatalf("prompts = %q, want no note when nothing is trimmed", prompts)
	}

	// Transparency off by default.
	prompts, _, err = NewSlidingWindowPolicy(2).Apply([]string{"sys"}, evs, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(prompts) != 1 {
		t.Fatalf("prompts = %q, want no note by default", prompts)
	}
}

// TestTokenBudgetTransparency pins the budget note.
func TestTokenBudgetTransparency(t *testing.T) {
	e1, _ := textIn("aaaaaaaa") // 2 tokens
	e2, _ := textIn("bb")       // 1 token
	e3, _ := textIn("cccccccc") // 2 tokens
	evs := []events.Event{e1, e2, e3}

	prompts, retained, err := NewTokenBudgetPolicy(3, WithBudgetTransparency(true)).Apply([]string{"sys"}, evs, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(retained) != 2 {
		t.Fatalf("retained = %d, want 2", len(retained))
	}
	want := "[token_budget] Showing 2 of 3 events (token budget)."
	if len(prompts) != 2 || prompts[1] != want {
		t.Fatalf("prompts = %q, want %q appended", prompts, want)
	}

	prompts, _, err = NewTokenBudgetPolicy(100, WithBudgetTransparency(true)).Apply([]string{"sys"}, evs, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(prompts) != 1 {
		t.Fatalf("prompts = %q, want no note when everything fits", prompts)
	}
}

// TestEpisodicTransparency pins the summary-count note and its off default.
func TestEpisodicTransparency(t *testing.T) {
	store := knowledge.NewMemoryStore()
	for name, summary := range map[string]string{"c1": "first", "c2": "second"} {
		if err := store.Write(ConversationsDir+name, []byte(summary)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	prompts, _, err := NewEpisodicMemoryPolicy(store).Apply([]string{"sys"}, nil, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(prompts) != 2 {
		t.Fatalf("prompts = %q, want the block with no note by default", prompts)
	}

	prompts, _, err = NewEpisodicMemoryPolicy(store, WithEpisodicTransparency(true)).Apply([]string{"sys"}, nil, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := "[episodic_memory] Injected 2 past conversation summaries."
	if len(prompts) != 3 || prompts[2] != want {
		t.Fatalf("prompts = %q, want %q appended", prompts, want)
	}
}

// customPolicy is an unknown policy used to pin the name fallback.
type customPolicy struct{}

func (customPolicy) Apply(prompts []string, evs []events.Event, _ *agocontext.Context) ([]string, []events.Event, error) {
	return prompts, evs, nil
}

// TestPolicyName pins the Python-compatible names, including pointer forms.
func TestPolicyName(t *testing.T) {
	window := NewSlidingWindowPolicy(2)
	budget := NewTokenBudgetPolicy(10)
	episodic := NewEpisodicMemoryPolicy(nil)
	memory := NewWorkingMemoryPolicy(nil)
	tests := []struct {
		name string
		p    AssemblyPolicy
		want string
	}{
		{"conversation", NewConversationPolicy(), "conversation"},
		{"window", window, "sliding_window"},
		{"window pointer", &window, "sliding_window"},
		{"budget", budget, "token_budget"},
		{"budget pointer", &budget, "token_budget"},
		{"episodic", episodic, "episodic_memory"},
		{"episodic pointer", &episodic, "episodic_memory"},
		{"working memory", memory, "working_memory"},
		{"working memory pointer", &memory, "working_memory"},
		{"alert", NewAlertPolicy(), "alert"},
		{"custom", customPolicy{}, "customPolicy"},
		{"nil", nil, "nil"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PolicyName(tt.p); got != tt.want {
				t.Fatalf("PolicyName = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestValidateOrder pins the Python ordering warnings.
func TestValidateOrder(t *testing.T) {
	t.Run("reduction before injection warns", func(t *testing.T) {
		warnings := ValidateOrder([]AssemblyPolicy{
			NewSlidingWindowPolicy(2),
			NewEpisodicMemoryPolicy(nil),
		})
		want := "Policy 'sliding_window' (index 0) runs before 'episodic_memory' (index 1). " +
			"Injection policies should generally run before reduction policies " +
			"so injected context is included in the reduction budget."
		if len(warnings) != 1 || warnings[0] != want {
			t.Fatalf("warnings = %q, want [%q]", warnings, want)
		}
	})
	t.Run("injection before reduction is clean", func(t *testing.T) {
		if warnings := ValidateOrder([]AssemblyPolicy{
			NewEpisodicMemoryPolicy(nil),
			NewSlidingWindowPolicy(2),
		}); len(warnings) != 0 {
			t.Fatalf("warnings = %q, want none", warnings)
		}
	})
	t.Run("every reduction before every later injection warns", func(t *testing.T) {
		warnings := ValidateOrder([]AssemblyPolicy{
			NewSlidingWindowPolicy(2),
			NewTokenBudgetPolicy(10),
			NewEpisodicMemoryPolicy(nil),
		})
		if len(warnings) != 2 {
			t.Fatalf("warnings = %q, want 2", warnings)
		}
	})
	t.Run("empty order is clean", func(t *testing.T) {
		if warnings := ValidateOrder(nil); len(warnings) != 0 {
			t.Fatalf("warnings = %q, want none", warnings)
		}
	})
	t.Run("alert counts as an injection", func(t *testing.T) {
		warnings := ValidateOrder([]AssemblyPolicy{NewTokenBudgetPolicy(10), NewAlertPolicy()})
		if len(warnings) != 1 {
			t.Fatalf("warnings = %q, want 1", warnings)
		}
	})
}
