package network

// PassportKind discriminates participant types.
type PassportKind string

const (
	KindAgent       PassportKind = "agent"
	KindHuman       PassportKind = "human"
	KindRemoteAgent PassportKind = "remote_agent"
)

// Valid reports whether k is one of the known kinds ("" counts as agent for
// back-compatibility with pre-kind registrations).
func (k PassportKind) Valid() bool {
	switch k {
	case "", KindAgent, KindHuman, KindRemoteAgent:
		return true
	}
	return false
}

// Effective resolves "" to the default kind (agent).
func (k PassportKind) Effective() PassportKind {
	if k == "" {
		return KindAgent
	}
	return k
}

// Passport is the identity record for one registration. AgentID is
// hub-stamped at registration; a passport is immutable after that — changes
// go through unregister + re-register and yield a fresh id.
type Passport struct {
	Kind    PassportKind   `json:"kind,omitempty"` // "" ≡ agent
	AgentID string         `json:"agent_id"`       // hub-stamped; empty on the sender side
	Name    string         `json:"name"`           // human/LLM-facing address; unique per hub
	Auth    map[string]any `json:"auth,omitempty"` // scheme + claims; validated by nothing in this subset
}

// EffectiveKind resolves the declared kind, defaulting to agent.
func (p Passport) EffectiveKind() PassportKind { return p.Kind.Effective() }

// Resume is the mutable capability claim attached to a registration. The hub
// stores it; capability discovery is out of this subset's scope.
type Resume struct {
	AgentID      string   `json:"agent_id"`
	Capabilities []string `json:"capabilities,omitempty"`
}
