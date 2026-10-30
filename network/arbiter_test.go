package network

import (
	"errors"
	"testing"
)

// recordingArbiter embeds the no-op base and records which gates the hub
// consulted, with an all-or-nothing allow switch.
type recordingArbiter struct {
	BaseArbiter
	allow    bool
	redirect string
	visited  map[string]int
	resolved [][]string
}

func newRecordingArbiter(allow bool) *recordingArbiter {
	return &recordingArbiter{allow: allow, visited: map[string]int{}}
}

func (a *recordingArbiter) note(gate string) Decision {
	a.visited[gate]++
	if a.allow {
		return Allow()
	}
	return Deny("denied by test arbiter")
}

func (a *recordingArbiter) AuthorizeSend(env Envelope, sender Passport, _ Rule, recipients []Passport) Decision {
	return a.note("send")
}

func (a *recordingArbiter) AuthorizeDispatch(env Envelope, sender Passport, recipient Passport, _ Rule) Decision {
	return a.note("dispatch")
}

func (a *recordingArbiter) AuthorizeChannelOpen(manifest ChannelManifest, creator Passport, _ Rule, invitees []Passport, _ []Rule, active int) Decision {
	return a.note("channel_open")
}

// redirect is the replacement audience the federation hook returns (set
// by the test to a registered stand-in for a local proxy id).
func (a *recordingArbiter) ResolveUnknownAudience(env Envelope, unknownIDs []string) []string {
	a.resolved = append(a.resolved, append([]string(nil), unknownIDs...))
	return []string{a.redirect}
}

// denyDispatchTo denies dispatch to one recipient name, delegates the rest
// to the rule-based behavior.
type denyDispatchTo struct {
	RuleBasedArbiter
	name string
}

func (d *denyDispatchTo) AuthorizeDispatch(env Envelope, sender Passport, recipient Passport, rule Rule) Decision {
	if recipient.Name == d.name {
		return Deny("blocked by test")
	}
	return d.RuleBasedArbiter.AuthorizeDispatch(env, sender, recipient, rule)
}

func TestArbiterDefaultRuleSendPasses(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)
	alice, err := hc.Register("alice")
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()
	bob, err := hc.Register("bob")
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()

	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Send(NewTextEnvelope(ch.ID, alice.AgentID(), "", "hi")); err != nil {
		t.Fatalf("send with default rule must pass: %v", err)
	}
}

func TestArbiterCustomDenyGatesOpen(t *testing.T) {
	arb := newRecordingArbiter(false)
	h := newTestHub(t, WithHubArbiter(arb))
	hc := NewHubClient(h)
	alice, _ := hc.Register("alice")
	defer alice.Close()

	if _, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}); err == nil {
		t.Fatal("denying arbiter must block channel open")
	}
	if arb.visited["channel_open"] == 0 {
		t.Fatal("hub never consulted AuthorizeChannelOpen")
	}
}

func TestArbiterDenySend(t *testing.T) {
	arb := newRecordingArbiter(false)
	h := newTestHub(t)
	hc := NewHubClient(h)
	alice, _ := hc.Register("alice")
	defer alice.Close()
	bob, _ := hc.Register("bob")
	defer bob.Close()
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.arbiter = arb
	h.mu.Unlock()
	if _, err := h.Send(NewTextEnvelope(ch.ID, alice.AgentID(), "", "blocked")); err == nil {
		t.Fatal("denying arbiter must block sends")
	}
	if arb.visited["send"] == 0 {
		t.Fatal("hub never consulted AuthorizeSend")
	}
}

func TestArbiterDenyDispatchSkipsRecipientOnly(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)
	alice, _ := hc.Register("alice")
	defer alice.Close()
	bob, _ := hc.Register("bob")
	defer bob.Close()
	carol, _ := hc.Register("carol")
	defer carol.Close()
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}

	var aliceGot, bobGot int
	_ = h.Attach(alice.AgentID(), func(env Envelope) { aliceGot++ })
	_ = h.Attach(bob.AgentID(), func(env Envelope) { bobGot++ })

	h.mu.Lock()
	h.arbiter = &denyDispatchTo{name: "bob"}
	h.mu.Unlock()

	// The close broadcast addresses both participants (creator included);
	// only bob is denied — alice still receives.
	if err := h.CloseChannel(ch.ID, "test done"); err != nil {
		t.Fatal(err)
	}
	if aliceGot == 0 {
		t.Fatal("alice must still receive the broadcast")
	}
	if bobGot != 0 {
		t.Fatal("bob must be silently skipped by the dispatch deny")
	}
}

func TestArbiterResolveUnknownAudience(t *testing.T) {
	arb := newRecordingArbiter(true)
	h := newTestHub(t, WithHubArbiter(arb))
	hc := NewHubClient(h)
	alice, _ := hc.Register("alice")
	defer alice.Close()
	bob, _ := hc.Register("bob")
	defer bob.Close()
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	arb.redirect = bob.AgentID()

	var bobGot int
	_ = h.Attach(bob.AgentID(), func(env Envelope) { bobGot++ })

	// "proxy-remote" is registered nowhere; the arbiter reroutes the
	// unknown audience id onto bob (stand-in for a local federation proxy).
	env := NewTextEnvelope(ch.ID, alice.AgentID(), "proxy-remote", "for the remote peer")
	if _, err := h.Send(env); err != nil {
		t.Fatalf("send with unknown audience must succeed: %v", err)
	}
	if len(arb.resolved) != 1 || len(arb.resolved[0]) != 1 || arb.resolved[0][0] != "proxy-remote" {
		t.Fatalf("resolve hook not consulted: %v", arb.resolved)
	}
	if bobGot != 1 {
		t.Fatalf("rerouted envelope must reach the replacement id, got %d", bobGot)
	}
}

func TestArbiterUnknownAudienceDroppedByDefault(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)
	alice, _ := hc.Register("alice")
	defer alice.Close()
	bob, _ := hc.Register("bob")
	defer bob.Close()
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	env := NewTextEnvelope(ch.ID, alice.AgentID(), "ghost", "nobody home")
	if _, err := h.Send(env); err != nil {
		t.Fatalf("default arbiter must drop unknown audiences silently: %v", err)
	}
}

func TestArbiterDepthCapSurfacesSentinel(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)
	deep, err := hc.Register("deep", WithAgentRule(Rule{
		InboundGlobs:       []string{"*"},
		OutboundGlobs:      []string{"*"},
		MaxDelegationDepth: 2,
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer deep.Close()
	peer, err := hc.Register("peer")
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	ch, err := deep.CreateChannel(ChannelManifest{Type: TypeConversation}, peer.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	env := NewTextEnvelope(ch.ID, deep.AgentID(), "", "too deep")
	env.Depth = 3
	if _, err := h.Send(env); !errors.Is(err, ErrDepthExceeded) {
		t.Fatalf("want ErrDepthExceeded, got %v", err)
	}
}

func TestArbiterAuthorizeRegisterSeamOnly(t *testing.T) {
	// Python parity: the hub core never calls authorize_register; the gate
	// exists on the seam for tenant hubs. The embedded BaseArbiter answers
	// Allow; the hub keeps registering regardless of any override.
	arb := newRecordingArbiter(false)
	// The embedded BaseArbiter answers Allow by default (register is not
	// overridden on the recording arbiter).
	if d := arb.AuthorizeRegister(Passport{Name: "x"}, Resume{}, DefaultRule()); !d.Allowed {
		t.Fatal("BaseArbiter default must allow register")
	}
	// The hub never consults the gate: registration works even on a hub
	// wired with an all-denying arbiter.
	h := newTestHub(t, WithHubArbiter(arb))
	if _, err := h.Register(Passport{Name: "x"}, nil); err != nil {
		t.Fatalf("hub must not consult authorize_register: %v", err)
	}
	if arb.visited["register"] != 0 {
		t.Fatalf("authorize_register was consulted: %v", arb.visited)
	}
}
