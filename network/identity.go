package network

import (
	"encoding/json"
	"time"
)

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

// CostProfile carries optional billing and routing hints. Nothing in the hub
// validates them; peers listings may sort on them. Wire names match Python.
type CostProfile struct {
	InputPerMTok  float64 `json:"input_per_mtok,omitempty"`
	OutputPerMTok float64 `json:"output_per_mtok,omitempty"`
	LatencyTier   string  `json:"latency_tier,omitempty"` // "fast" | "balanced" | "deep"
}

// Passport is the identity record for one registration. AgentID, Version and
// CreatedAt are hub-stamped at registration; a passport is immutable after
// that — changes go through unregister + re-register and yield a fresh id.
//
// The billing and routing facts (Owner, Provider, Model, Cost, Region) use
// the Python field names on the wire. Decoding tolerates the Python
// defaults (null, and "" for an unstamped created_at).
type Passport struct {
	Kind      PassportKind   `json:"kind,omitempty"` // "" ≡ agent
	AgentID   string         `json:"agent_id"`       // hub-stamped; empty on the sender side
	Name      string         `json:"name"`           // human/LLM-facing address; unique per hub
	Auth      map[string]any `json:"auth,omitempty"` // {"scheme": ..., "claim": {...}}; see AuthRegistry
	Owner     string         `json:"owner,omitempty"`
	Provider  string         `json:"provider,omitempty"` // "anthropic" | "openai" | ...
	Model     string         `json:"model,omitempty"`
	Cost      *CostProfile   `json:"cost,omitempty"`
	Region    string         `json:"region,omitempty"`
	Version   int            `json:"version,omitempty"`   // hub-stamped; 1 on first registration
	CreatedAt time.Time      `json:"created_at,omitzero"` // hub-stamped
}

// EffectiveKind resolves the declared kind, defaulting to agent.
func (p Passport) EffectiveKind() PassportKind { return p.Kind.Effective() }

// UnmarshalJSON decodes a passport, accepting the Python encoding of an
// unstamped created_at ("" or null).
func (p *Passport) UnmarshalJSON(data []byte) error {
	type plain Passport
	aux := struct {
		*plain
		CreatedAt json.RawMessage `json:"created_at"`
	}{plain: (*plain)(p)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	t, err := parseLooseTime(aux.CreatedAt)
	if err != nil {
		return err
	}
	p.CreatedAt = t
	return nil
}

// parseLooseTime decodes an RFC 3339 timestamp, mapping absent, null and ""
// (Python's unstamped default) to the zero time.
func parseLooseTime(raw json.RawMessage) (time.Time, error) {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == `""` {
		return time.Time{}, nil
	}
	var t time.Time
	err := json.Unmarshal(raw, &t)
	return t, err
}

// ResumeExample is one example task in a resume.
type ResumeExample struct {
	Title     string `json:"title"`
	Outcome   string `json:"outcome,omitempty"` // "completed" | "failed" | free-form
	TaskID    string `json:"task_id,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
	When      string `json:"when,omitempty"` // RFC 3339; stamped by AddResumeExample when empty
	Note      string `json:"note,omitempty"`
}

// ObservedStat is the hub-derived per-capability track record, updated on
// terminal task events (Hub.RecordObservation).
type ObservedStat struct {
	N            int  `json:"n"`
	Completed    int  `json:"completed"`
	Failed       int  `json:"failed"`
	Expired      int  `json:"expired"`
	P50LatencyMS *int `json:"p50_latency_ms,omitempty"` // latest sample (single-sample stand-in); nil = never measured
}

// Resume is the mutable capability claim attached to a registration plus the
// hub-observed track record. Tenant code sets Capabilities, Domains, Summary
// and Examples; the hub owns Observed, Version and LastUpdated.
//
// Capabilities travels as "capabilities"; decoding also accepts Python's
// "claimed_capabilities".
type Resume struct {
	AgentID      string                  `json:"agent_id"`
	Capabilities []string                `json:"capabilities,omitempty"`
	Domains      []string                `json:"domains,omitempty"`
	Summary      string                  `json:"summary,omitempty"` // one line, searchable by peers find
	Examples     []ResumeExample         `json:"examples,omitempty"`
	Observed     map[string]ObservedStat `json:"observed,omitempty"`
	Version      int                     `json:"version,omitempty"`     // hub-bumped on every mutation
	LastUpdated  time.Time               `json:"last_updated,omitzero"` // hub-stamped on every mutation
}

// UnmarshalJSON decodes a resume, accepting Python's claimed_capabilities
// alias and its "" last_updated default.
func (r *Resume) UnmarshalJSON(data []byte) error {
	type plain Resume
	aux := struct {
		*plain
		Claimed     []string        `json:"claimed_capabilities"`
		LastUpdated json.RawMessage `json:"last_updated"`
	}{plain: (*plain)(r)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if len(r.Capabilities) == 0 && len(aux.Claimed) > 0 {
		r.Capabilities = aux.Claimed
	}
	t, err := parseLooseTime(aux.LastUpdated)
	if err != nil {
		return err
	}
	r.LastUpdated = t
	return nil
}

// Clone deep-copies the slices and maps so a stored resume never aliases the
// caller's memory.
func (r Resume) Clone() Resume {
	out := r
	out.Capabilities = append([]string(nil), r.Capabilities...)
	out.Domains = append([]string(nil), r.Domains...)
	out.Examples = append([]ResumeExample(nil), r.Examples...)
	if r.Observed != nil {
		out.Observed = make(map[string]ObservedStat, len(r.Observed))
		for k, v := range r.Observed {
			if v.P50LatencyMS != nil {
				ms := *v.P50LatencyMS
				v.P50LatencyMS = &ms
			}
			out.Observed[k] = v
		}
	}
	return out
}

// allCapabilities lists claimed and observed capability tags (the set the
// hub's capability index covers).
func (r Resume) allCapabilities() map[string]struct{} {
	set := make(map[string]struct{}, len(r.Capabilities)+len(r.Observed))
	for _, c := range r.Capabilities {
		set[c] = struct{}{}
	}
	for c := range r.Observed {
		set[c] = struct{}{}
	}
	return set
}
