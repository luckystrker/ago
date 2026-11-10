package network

import "testing"

func TestParseHubURN(t *testing.T) {
	cases := []struct {
		in            string
		wantHub       string
		wantAgent     string
		whatItIsAbout string
	}{
		{"hub://h1/agent-7", "h1", "agent-7", "a canonical URN"},
		{"agent-7", "", "agent-7", "a bare agent id passes through unchanged"},
		{"hub://h1", "", "hub://h1", "a URN with no agent half is not a URN"},
		{"hub://h1/", "", "hub://h1/", "an empty agent half is not a URN"},
		{"hub:///agent-7", "", "hub:///agent-7", "an empty hub id is not a URN"},
		{"https://h1/agent-7", "", "https://h1/agent-7", "another scheme is not a hub URN"},
		{"", "", "", "the empty string is not a URN"},
	}
	for _, tc := range cases {
		hubID, agentID := ParseHubURN(tc.in)
		if hubID != tc.wantHub || agentID != tc.wantAgent {
			t.Errorf("ParseHubURN(%q) = (%q, %q), want (%q, %q) — %s",
				tc.in, hubID, agentID, tc.wantHub, tc.wantAgent, tc.whatItIsAbout)
		}
	}
}

// The parse is idempotent on the agent half: feeding it back yields it again,
// which is what lets a caller parse whatever address it was handed.
func TestParseHubURNAgentHalfIsIdempotent(t *testing.T) {
	_, agentID := ParseHubURN("hub://remote-hub/bob")
	if hubID, again := ParseHubURN(agentID); hubID != "" || again != "bob" {
		t.Fatalf("re-parsing %q = (%q, %q), want (\"\", \"bob\")", agentID, hubID, again)
	}
}

func TestHubURNIsTheInverseOfParse(t *testing.T) {
	const hubID, agentID = "h-42", "carol"
	hub, agent := ParseHubURN(HubURN(hubID, agentID))
	if hub != hubID || agent != agentID {
		t.Fatalf("round trip = (%q, %q)", hub, agent)
	}
	if got, want := HubURN(hubID, agentID), "hub://h-42/carol"; got != want {
		t.Fatalf("HubURN = %q, want %q", got, want)
	}
}
