package network

import "path"

// Rule is the per-(hub, agent) access + limits policy. Defaults are
// permissive (allow everyone, no caps); apps tighten by passing a non-zero
// rule at registration. Both halves are enforced at the hub, never the
// client.
//
// AGO-DEVIATION vs Python's Rule: the brief pins a flat four-field shape —
// channel-type access, rate limits, inbox policy and default TTLs are not
// ported.
type Rule struct {
	// InboundGlobs globs over sender *names*; a recipient accepts envelopes
	// only from matching senders. Empty = accept nobody (use DefaultRule).
	InboundGlobs []string `json:"inbound_globs,omitempty"`
	// OutboundGlobs globs over recipient *names*; the sender may address
	// only matching recipients.
	OutboundGlobs []string `json:"outbound_globs,omitempty"`
	// MaxDelegationDepth caps Envelope.Depth on send; 0 disables the cap.
	MaxDelegationDepth int `json:"max_delegation_depth,omitempty"`
	// MaxConcurrentChannels caps the channels one creator may keep
	// non-terminal; 0 disables the cap.
	MaxConcurrentChannels int `json:"max_concurrent_channels,omitempty"`
}

// DefaultRule is the permissive rule handed out when none is given: every
// glob is "*", no caps.
func DefaultRule() Rule {
	return Rule{
		InboundGlobs:  []string{"*"},
		OutboundGlobs: []string{"*"},
	}
}

// Clone deep-copies the glob slices so a stored rule never aliases the
// caller's memory.
func (r Rule) Clone() Rule {
	out := r
	if len(r.InboundGlobs) > 0 {
		out.InboundGlobs = append([]string(nil), r.InboundGlobs...)
	}
	if len(r.OutboundGlobs) > 0 {
		out.OutboundGlobs = append([]string(nil), r.OutboundGlobs...)
	}
	return out
}

// Match reports whether s matches any glob in globs. Patterns use
// path.Match syntax; an empty pattern list matches nothing (a zero Rule
// denies everything — pairs with DefaultRule for the permissive default).
func Match(globs []string, s string) bool {
	for _, g := range globs {
		if ok, err := path.Match(g, s); err == nil && ok {
			return true
		}
	}
	return false
}
