package policies

import (
	"fmt"
	"reflect"
)

// Policy names used by ValidateOrder. They match the Python policies' name
// attributes so an ordering warning reads the same in both ports.
const (
	PolicyNameConversation  = "conversation"
	PolicyNameSlidingWindow = "sliding_window"
	PolicyNameTokenBudget   = "token_budget"
	PolicyNameAlert         = "alert"
	PolicyNameWorkingMemory = "working_memory"
	PolicyNameEpisodic      = "episodic_memory"
)

// reductionPolicies and injectionPolicies are Python's ordering sets
// (assembly.py validate_order).
var (
	reductionPolicies = map[string]bool{
		PolicyNameSlidingWindow: true,
		PolicyNameTokenBudget:   true,
	}
	injectionPolicies = map[string]bool{
		PolicyNameEpisodic:      true,
		PolicyNameWorkingMemory: true,
		"topic_inbox":           true, // Python-only policy; kept so the sets match
		PolicyNameAlert:         true,
	}
)

// PolicyName returns the policy's name: the Python-compatible name for a
// builtin, or the Go type name for a custom policy.
func PolicyName(p AssemblyPolicy) string {
	if p == nil {
		return "nil"
	}
	switch p.(type) {
	case ConversationPolicy, *ConversationPolicy:
		return PolicyNameConversation
	case SlidingWindowPolicy, *SlidingWindowPolicy:
		return PolicyNameSlidingWindow
	case TokenBudgetPolicy, *TokenBudgetPolicy:
		return PolicyNameTokenBudget
	case *AlertPolicy:
		return PolicyNameAlert
	case WorkingMemoryPolicy, *WorkingMemoryPolicy:
		return PolicyNameWorkingMemory
	case EpisodicMemoryPolicy, *EpisodicMemoryPolicy:
		return PolicyNameEpisodic
	}
	t := reflect.TypeOf(p)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Name()
}

// ValidateOrder reports known-problematic policy orderings (Python parity:
// AssemblerMiddleware.validate_order). A reduction policy (window, budget) that
// runs before an injection policy trims context that the injection adds next,
// so the injected text escapes the budget. The returned warnings name both
// policies and their indexes; an empty result means the order looks sane.
func ValidateOrder(ps []AssemblyPolicy) []string {
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = PolicyName(p)
	}
	var warnings []string
	for i, name := range names {
		if !reductionPolicies[name] {
			continue
		}
		for j := i + 1; j < len(names); j++ {
			if !injectionPolicies[names[j]] {
				continue
			}
			warnings = append(warnings, fmt.Sprintf(
				"Policy '%s' (index %d) runs before '%s' (index %d). "+
					"Injection policies should generally run before reduction policies "+
					"so injected context is included in the reduction budget.",
				name, i, names[j], j))
		}
	}
	return warnings
}
