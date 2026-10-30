package network

import "fmt"

// Decision is the arbiter's verdict: Allow or Deny (Python parity:
// arbiter.py Allow/Deny dataclasses collapsed into one struct). A Deny
// carries a human-readable Reason and the sentinel class the hub surfaces
// to the caller (Err; nil → ErrAccessDenied).
type Decision struct {
	Allowed bool
	Reason  string
	Err     error
}

// Allow permits the action.
func Allow() Decision { return Decision{Allowed: true} }

// Deny rejects the action. errClass selects the sentinel the hub wraps
// (nil → ErrAccessDenied, Python's default AccessDeniedError).
func Deny(reason string, errClass ...error) Decision {
	var err error = ErrAccessDenied
	if len(errClass) > 0 && errClass[0] != nil {
		err = errClass[0]
	}
	return Decision{Allowed: false, Reason: reason, Err: err}
}

func (d Decision) err() error {
	return fmt.Errorf("%w: %s", d.Err, d.Reason)
}

// HubArbiter is the decision-making seam the hub consults before
// committing register / channel-open / send / dispatch decisions (Python
// parity: arbiter.py HubArbiter Protocol). Distinct from passive
// observation — the arbiter is the gatekeeper. Implementations must be
// pure decisions: inspect the inputs, never mutate hub state.
//
// Only one arbiter is active per hub (WithHubArbiter; the default is
// RuleBasedArbiter). AGO-DEVIATION vs Python: methods are synchronous —
// the hub holds its lock across the consultations, so an arbiter must not
// block (Python's async methods run on the hub's event loop under the same
// constraint).
type HubArbiter interface {
	// AuthorizeSend gates Send before the WAL append: outbound access
	// plus delegation depth over the explicit audience (broadcasts skip —
	// protocol broadcasts address the creator too).
	AuthorizeSend(env Envelope, sender Passport, senderRule Rule, recipients []Passport) Decision
	// AuthorizeInbox gates a send per-recipient against inbox capacity.
	// AGO-DEVIATION: the hub never consults this gate in the current
	// subset — delivery inboxes live client-side (a bounded channel whose
	// overflow drops) and the hub keeps no pending counter, so there is no
	// count to consult with; the AGo Rule carries no inbox block either.
	// The method exists so tenant arbiters keep the Python surface.
	AuthorizeInbox(env Envelope, recipient Passport, recipientRule Rule, currentPending int) Decision
	// AuthorizeDispatch gates per-recipient dispatch (inbound access). A
	// Deny silently skips this recipient — the rest of the audience still
	// receives.
	AuthorizeDispatch(env Envelope, sender Passport, recipient Passport, recipientRule Rule) Decision
	// AuthorizeChannelOpen gates CreateChannel before any persistence.
	AuthorizeChannelOpen(manifest ChannelManifest, creator Passport, creatorRule Rule, invitees []Passport, inviteeRules []Rule, activeCreatorChannels int) Decision
	// AuthorizeRegister gates Hub.register. The reference hub never
	// consults it (Python parity — the Protocol method has no call site in
	// hub core); it exists for tenant hubs layered on the seam.
	AuthorizeRegister(passport Passport, resume Resume, rule Rule) Decision
	// ResolveUnknownAudience is the federation hook for audience members
	// the hub doesn't know. Returning nil drops the unknown ids silently
	// (single-hub behavior); returning a list delivers to those ids
	// instead — typically the local proxy id of a remote peer.
	ResolveUnknownAudience(env Envelope, unknownIDs []string) []string
}

// BaseArbiter is the no-op base: every gate allows, unknown audiences are
// dropped. Embed it and override only the gates you care about.
type BaseArbiter struct{}

// AuthorizeSend implements HubArbiter.
func (BaseArbiter) AuthorizeSend(Envelope, Passport, Rule, []Passport) Decision { return Allow() }

// AuthorizeInbox implements HubArbiter.
func (BaseArbiter) AuthorizeInbox(Envelope, Passport, Rule, int) Decision { return Allow() }

// AuthorizeDispatch implements HubArbiter.
func (BaseArbiter) AuthorizeDispatch(Envelope, Passport, Passport, Rule) Decision { return Allow() }

// AuthorizeChannelOpen implements HubArbiter.
func (BaseArbiter) AuthorizeChannelOpen(ChannelManifest, Passport, Rule, []Passport, []Rule, int) Decision {
	return Allow()
}

// AuthorizeRegister implements HubArbiter.
func (BaseArbiter) AuthorizeRegister(Passport, Resume, Rule) Decision { return Allow() }

// ResolveUnknownAudience implements HubArbiter.
func (BaseArbiter) ResolveUnknownAudience(Envelope, []string) []string { return nil }

// RuleBasedArbiter is the default arbiter: it enforces the per-agent Rule
// exactly as the hub did inline before the seam existed. Pure function of
// the inputs; holds no state.
type RuleBasedArbiter struct {
	BaseArbiter
}

// AuthorizeSend implements HubArbiter: outbound globs over recipient names
// plus the delegation-depth cap (0 disables).
func (RuleBasedArbiter) AuthorizeSend(env Envelope, sender Passport, senderRule Rule, recipients []Passport) Decision {
	for _, recipient := range recipients {
		if recipient.AgentID == sender.AgentID {
			continue // self-routing is always allowed
		}
		if !Match(senderRule.OutboundGlobs, recipient.Name) {
			return Deny(fmt.Sprintf("sender %q may not send to %q", sender.Name, recipient.Name))
		}
	}
	if cap := senderRule.MaxDelegationDepth; cap > 0 && env.Depth > cap {
		return Deny(fmt.Sprintf("sender %q depth %d > %d", sender.Name, env.Depth, cap), ErrDepthExceeded)
	}
	return Allow()
}

// AuthorizeInbox implements HubArbiter. The AGo Rule shape has no inbox
// block, so the rule-based gate has nothing to compare against and always
// allows (see the interface's AGO-DEVIATION note).
func (RuleBasedArbiter) AuthorizeInbox(Envelope, Passport, Rule, int) Decision { return Allow() }

// AuthorizeDispatch implements HubArbiter: the recipient's inbound globs
// over the sender's name.
func (RuleBasedArbiter) AuthorizeDispatch(_ Envelope, sender Passport, recipient Passport, recipientRule Rule) Decision {
	if !Match(recipientRule.InboundGlobs, sender.Name) {
		return Deny(fmt.Sprintf("recipient %q blocks inbound from %q", recipient.Name, sender.Name))
	}
	return Allow()
}

// AuthorizeChannelOpen implements HubArbiter: each invitee's inbound globs
// against the creator's name (a pre-flight for the invite handshake —
// dispatch silently filters blocked senders, so without it an invite to a
// blocking invitee would hang until the ack timeout), then the creator's
// channel concurrency cap.
func (RuleBasedArbiter) AuthorizeChannelOpen(_ ChannelManifest, creator Passport, creatorRule Rule, invitees []Passport, inviteeRules []Rule, activeCreatorChannels int) Decision {
	for i, invitee := range invitees {
		if invitee.AgentID == creator.AgentID {
			continue
		}
		var inviteeRule Rule
		if i < len(inviteeRules) {
			inviteeRule = inviteeRules[i]
		}
		if !Match(inviteeRule.InboundGlobs, creator.Name) {
			return Deny(fmt.Sprintf("invitee %q does not accept inbound from %q", invitee.Name, creator.Name))
		}
	}
	if cap := creatorRule.MaxConcurrentChannels; cap > 0 && activeCreatorChannels >= cap {
		return Deny(fmt.Sprintf("creator %q exceeded max_concurrent_channels (%d >= %d)",
			creator.Name, activeCreatorChannels, cap))
	}
	return Allow()
}

// ResolveUnknownAudience implements HubArbiter: drop unknown ids silently.
func (RuleBasedArbiter) ResolveUnknownAudience(Envelope, []string) []string { return nil }

// interface conformance guards.
var (
	_ HubArbiter = BaseArbiter{}
	_ HubArbiter = RuleBasedArbiter{}
)

// denyErr builds the error a Deny surfaces through Send / CreateChannel.
// A Deny built without an explicit sentinel class defaults to
// ErrAccessDenied.
func denyErr(d Decision) error {
	if d.Err == nil {
		return fmt.Errorf("%w: %s", ErrAccessDenied, d.Reason)
	}
	return d.err()
}

// resolveUnknownLocked consults the arbiter's federation hook for audience
// ids no passport covers. Runs under the hub lock; nil replacement keeps
// the drop-silently behavior.
func (h *Hub) resolveUnknownLocked(env Envelope, ids []string) []string {
	var unknown []string
	seen := map[string]bool{}
	for _, id := range ids {
		if id == env.SenderID {
			continue
		}
		if _, ok := h.passports[id]; !ok && !seen[id] {
			unknown = append(unknown, id)
			seen[id] = true
		}
	}
	if len(unknown) == 0 {
		return ids
	}
	replacement := h.arbiter.ResolveUnknownAudience(env, unknown)
	if replacement == nil {
		return ids
	}
	out := make([]string, 0, len(ids)+len(replacement))
	for _, id := range ids {
		if !seen[id] {
			out = append(out, id)
		}
	}
	out = append(out, replacement...)
	return out
}
