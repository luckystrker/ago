package network

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		globs []string
		s     string
		want  bool
	}{
		{[]string{"*"}, "anything", true},
		{[]string{"agent-*"}, "agent-1", true},
		{[]string{"agent-*"}, "human-1", false},
		{[]string{"a", "b*"}, "bob", true},
		{[]string{"a", "b*"}, "carol", false},
		{nil, "anything", false}, // empty pattern list matches nothing
	}
	for _, tc := range cases {
		if got := Match(tc.globs, tc.s); got != tc.want {
			t.Errorf("Match(%v, %q) = %v, want %v", tc.globs, tc.s, got, tc.want)
		}
	}
}

func TestDefaultRule(t *testing.T) {
	r := DefaultRule()
	if !Match(r.InboundGlobs, "anyone") || !Match(r.OutboundGlobs, "anyone") {
		t.Fatal("default rule must be permissive")
	}
	if r.MaxDelegationDepth != 0 || r.MaxConcurrentChannels != 0 {
		t.Fatal("default rule must have no caps")
	}
}

func TestRuleClone(t *testing.T) {
	r := Rule{InboundGlobs: []string{"a"}, OutboundGlobs: []string{"b"}, MaxDelegationDepth: 3}
	c := r.Clone()
	c.InboundGlobs[0] = "changed"
	if r.InboundGlobs[0] != "a" {
		t.Fatal("Clone must deep-copy the glob slices")
	}
	if c.MaxDelegationDepth != 3 {
		t.Fatal("clone lost the cap")
	}
}
