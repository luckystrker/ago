package network

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// turnRig is a hub on a controllable clock with n auto-acking agents and an
// audit sink; sweeps are driven by hand.
type turnRig struct {
	t     *testing.T
	clock *fakeClock
	h     *Hub
	audit *SliceAuditLog
	rec   *recorder
	ids   []string
}

func newTurnRig(t *testing.T, n int) *turnRig {
	t.Helper()
	clock := newFakeClock()
	h, audit := sweepHub(t, clock)
	r := &turnRig{t: t, clock: clock, h: h, audit: audit, rec: newRecorder()}
	for i := 0; i < n; i++ {
		p, err := h.Register(Passport{Name: fmt.Sprintf("agent%d", i)}, nil)
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		r.rec.attach(h, p.AgentID, true)
		r.ids = append(r.ids, p.AgentID)
	}
	return r
}

// discussion opens a round-robin discussion over every agent; expectations
// nil takes the adapter defaults.
func (r *turnRig) discussion(exps []Expectation) string {
	r.t.Helper()
	ch, err := r.h.CreateChannel(ChannelManifest{Type: TypeDiscussion, Expectations: exps}, r.ids)
	if err != nil {
		r.t.Fatalf("create discussion: %v", err)
	}
	return ch.ID
}

// workflow opens a round-robin workflow over every agent.
func (r *turnRig) workflow(exps []Expectation) string {
	r.t.Helper()
	graph := TransitionGraph{
		Initial:     r.ids[0],
		Transitions: []Transition{{When: Always{}, To: &RoundRobinTarget{}}},
	}
	ch, err := r.h.CreateChannel(ChannelManifest{
		Type:         TypeWorkflow,
		Knobs:        map[string]any{"graph": graph.ToDict()},
		Expectations: exps,
	}, r.ids)
	if err != nil {
		r.t.Fatalf("create workflow: %v", err)
	}
	return ch.ID
}

// speak makes the protocol-expected speaker post one substantive envelope.
func (r *turnRig) speak(channelID string) {
	r.t.Helper()
	exp, err := r.h.ExpectedNext(channelID)
	if err != nil || exp == nil {
		r.t.Fatalf("no expected speaker: %v %v", exp, err)
	}
	ch, _ := r.h.Channel(channelID)
	var env Envelope
	if ch.Manifest.Type == TypeWorkflow {
		env = NewPacketEnvelope(channelID, exp.AgentID, "", "turn",
			WithPacketRouting(map[string]any{"kind": "text"}))
	} else {
		env = NewTextEnvelope(channelID, exp.AgentID, "", "turn")
	}
	if _, err := r.h.Send(env); err != nil {
		r.t.Fatalf("speak as %s: %v", exp.AgentID, err)
	}
}

func (r *turnRig) fires(kind string) []AuditEntry {
	var out []AuditEntry
	for _, e := range r.audit.Entries() {
		if e.Kind == AuditExpectationViolated && e.Expectation == kind {
			out = append(out, e)
		}
	}
	return out
}

// violationEnvelopes lists the EnvExpectationViolated envelopes in the WAL.
func (r *turnRig) violationEnvelopes(channelID string) []Envelope {
	envs, _ := r.h.ReadWAL(channelID)
	var out []Envelope
	for _, e := range envs {
		if e.EventType == EnvExpectationViolated {
			out = append(out, e)
		}
	}
	return out
}

func turnWithin(onViolation string, seconds int) Expectation {
	return Expectation{Kind: "turn_within", OnViolation: onViolation, Params: map[string]any{"seconds": seconds}}
}

// A stalled workflow: the turn goes to B, B stays silent past the window.
func TestTurnWithinStalledWorkflow(t *testing.T) {
	r := newTurnRig(t, 3)
	a, b, c := r.ids[0], r.ids[1], r.ids[2]
	id := r.workflow(nil) // adapter defaults: turn_within 120s warn, 600s auto_close

	r.speak(id) // A speaks; the turn passes to B
	if exp, _ := r.h.ExpectedNext(id); exp == nil || exp.AgentID != b {
		t.Fatalf("expected speaker %v, want %s", exp, b)
	}

	r.clock.Advance(119 * time.Second)
	r.h.SweepExpectations()
	if got := r.fires("turn_within"); len(got) != 0 {
		t.Fatalf("fired inside the window: %v", got)
	}

	r.clock.Advance(2 * time.Second) // 121s
	r.h.SweepExpectations()
	got := r.fires("turn_within")
	if len(got) != 1 || len(got[0].Violators) != 1 || got[0].Violators[0] != b || got[0].OnViolation != "warn" {
		t.Fatalf("want one turn_within/warn violation on B, got %+v", got)
	}

	// warn reaches only the violator.
	envs := r.violationEnvelopes(id)
	if len(envs) != 1 || envs[0].Audience != b {
		t.Fatalf("warn envelopes %+v, want one addressed to B", envs)
	}
	count := func(agent string) (n int) {
		for _, e := range r.rec.forAgent(agent) {
			if e.EventType == EnvExpectationViolated {
				n++
			}
		}
		return n
	}
	if count(b) != 1 || count(a) != 0 || count(c) != 0 {
		t.Fatalf("EV_EXPECTATION_VIOLATED deliveries a=%d b=%d c=%d, want 0/1/0", count(a), count(b), count(c))
	}
	if ch, _ := r.h.Channel(id); ch.State != ChannelActive {
		t.Fatalf("warn must not close the channel: %s", ch.State)
	}

	// Same turn, same window: no second fire.
	r.h.SweepExpectations()
	if got := r.fires("turn_within"); len(got) != 1 {
		t.Fatalf("re-fired on the same turn: %d", len(got))
	}

	// The 600s expectation closes the channel by manifest.
	r.clock.Advance(480 * time.Second) // 601s since the turn
	r.h.SweepExpectations()
	closed := waitForChannel(t, r.h, id, ChannelClosed)
	if closed.CloseReason != "expectation_violated:turn_within" {
		t.Fatalf("close reason %q", closed.CloseReason)
	}
}

// The first turn is timed from the channel opening, not from a prior envelope.
func TestTurnWithinFirstTurnStartsAtChannelOpening(t *testing.T) {
	r := newTurnRig(t, 2)
	id := r.discussion([]Expectation{turnWithin("warn", 120)})
	r.clock.Advance(119 * time.Second)
	r.h.SweepExpectations()
	if got := r.fires("turn_within"); len(got) != 0 {
		t.Fatalf("fired before the window: %v", got)
	}
	r.clock.Advance(2 * time.Second)
	r.h.SweepExpectations()
	got := r.fires("turn_within")
	if len(got) != 1 || got[0].Violators[0] != r.ids[0] {
		t.Fatalf("want the creator flagged on the opening turn, got %+v", got)
	}
	_ = id
}

// The window of a later turn starts at the envelope that handed it over, not
// at the channel's opening.
func TestTurnWithinLaterTurnStartsAtHandoverEnvelope(t *testing.T) {
	r := newTurnRig(t, 2)
	id := r.discussion([]Expectation{turnWithin("warn", 120)})
	r.clock.Advance(100 * time.Second)
	r.speak(id) // A speaks at +100s; B's window opens now
	r.clock.Advance(50 * time.Second)
	r.h.SweepExpectations() // +150s since opening, only 50s into B's turn
	if got := r.fires("turn_within"); len(got) != 0 {
		t.Fatalf("B's window must run from A's envelope: %v", got)
	}
	r.clock.Advance(71 * time.Second)
	r.h.SweepExpectations()
	if got := r.fires("turn_within"); len(got) != 1 || got[0].Violators[0] != r.ids[1] {
		t.Fatalf("want B flagged, got %+v", got)
	}
}

// The same participant violates on turn 1 and again on turn 3: two fires;
// a repeat sweep on one turn adds none.
func TestTurnWithinDedupIsPerTurn(t *testing.T) {
	r := newTurnRig(t, 2)
	b := r.ids[1]
	id := r.discussion([]Expectation{turnWithin("audit", 60)})

	r.speak(id) // A; turn 1 is B's
	r.clock.Advance(61 * time.Second)
	r.h.SweepExpectations()
	r.h.SweepExpectations()
	if got := r.fires("turn_within"); len(got) != 1 {
		t.Fatalf("turn 1: %d fires, want 1", len(got))
	}
	r.speak(id) // B late; turn 2 is A's
	r.speak(id) // A; turn 3 is B's
	r.clock.Advance(61 * time.Second)
	r.h.SweepExpectations()
	r.h.SweepExpectations()
	got := r.fires("turn_within")
	if len(got) != 2 || got[0].Violators[0] != b || got[1].Violators[0] != b {
		t.Fatalf("want two fires on B (turns 1 and 3), got %+v", got)
	}
}

// A violation whose on_violation names no handler is audited, not dropped.
func TestUnknownOnViolationIsAudited(t *testing.T) {
	r := newTurnRig(t, 2)
	id := r.discussion([]Expectation{turnWithin("hide", 600)})
	r.clock.Advance(601 * time.Second)
	r.h.SweepExpectations()
	got := r.fires("turn_within")
	if len(got) != 1 || got[0].OnViolation != "hide" {
		t.Fatalf("want one audited turn_within/hide fire, got %+v", got)
	}
	if n := len(r.violationEnvelopes(id)); n != 0 {
		t.Fatalf("an unhandled violation must send nothing, got %d envelopes", n)
	}
	if ch, _ := r.h.Channel(id); ch.State != ChannelActive {
		t.Fatalf("an unhandled violation must not close the channel: %s", ch.State)
	}
	r.h.SweepExpectations()
	if got := r.fires("turn_within"); len(got) != 1 {
		t.Fatalf("re-audited: %d", len(got))
	}
}

// The adapter defaults carry both turn_within entries of the reference.
func TestDiscussionAndWorkflowDefaultManifestsCarryTurnWithin(t *testing.T) {
	cases := []struct {
		name string
		m    ChannelManifest
		want []Expectation
	}{
		{"discussion", DiscussionAdapter{}.DefaultManifest(), []Expectation{turnWithin("warn", 120), turnWithin("hide", 600)}},
		{"workflow", WorkflowAdapter{}.DefaultManifest(), []Expectation{turnWithin("warn", 120), turnWithin("auto_close", 600)}},
	}
	for _, c := range cases {
		if len(c.m.Expectations) != len(c.want) {
			t.Fatalf("%s: %d expectations, want %d", c.name, len(c.m.Expectations), len(c.want))
		}
		for i, w := range c.want {
			g := c.m.Expectations[i]
			if g.Kind != w.Kind || g.OnViolation != w.OnViolation || expectationParamSeconds(g, 0) != expectationParamSeconds(w, 0) {
				t.Fatalf("%s[%d] = %+v, want %+v", c.name, i, g, w)
			}
		}
	}
}

// Invariants over random turn/sweep sequences (warn@60s), on a discussion for
// even seeds and a workflow for odd ones:
//
//	(I1) a (violator, turn) pair fires at most once;
//	(I2) nothing fires before the turn's window has run out;
//	(I3) the warning is addressed to the violator alone.
//
// Liveness rides along: a sweep past the window with no fire yet must fire.
// On a workflow any participant may also post a context update mid-turn; it
// is traffic on the channel but does not hand the turn over, so it must not
// restart the window.
func TestTurnWithinInvariantsRandomised(t *testing.T) {
	const window = 60
	for seed := int64(1); seed <= 25; seed++ {
		rng := rand.New(rand.NewSource(seed))
		r := newTurnRig(t, 3)
		workflow := seed%2 == 1
		var id string
		if workflow {
			id = r.workflow([]Expectation{turnWithin("warn", window)})
		} else {
			id = r.discussion([]Expectation{turnWithin("warn", window)})
		}

		turn := 0                  // number of substantive envelopes folded
		turnStart := r.clock.Now() // when the current turn's window opened
		fired := map[string]int{}  // "agent|turn" → fires
		seen := 0                  // audit entries already attributed
		envsSeen := 0              // violation envelopes already checked
		for step := 0; step < 80; step++ {
			switch k := rng.Intn(4); {
			case k == 0:
				r.clock.Advance(time.Duration(rng.Intn(90)) * time.Second)
			case k == 1:
				r.speak(id)
				turn++
				turnStart = r.clock.Now()
			case k == 3 && workflow:
				who := r.ids[rng.Intn(len(r.ids))]
				if _, err := r.h.Send(NewContextSetEnvelope(id, who, map[string]any{"k": step})); err != nil {
					t.Fatalf("seed %d: context set: %v", seed, err)
				}
			default:
				exp, _ := r.h.ExpectedNext(id)
				before := len(r.fires("turn_within"))
				r.h.SweepExpectations()
				after := r.fires("turn_within")
				elapsed := r.clock.Now().Sub(turnStart)
				key := fmt.Sprintf("%s|%d", exp.AgentID, turn)
				for _, e := range after[seen:] {
					if e.Violators[0] != exp.AgentID {
						t.Fatalf("seed %d: fired on %s, expected speaker is %s", seed, e.Violators[0], exp.AgentID)
					}
					fired[key]++ // (I1) checked below
					if elapsed < window*time.Second {
						t.Fatalf("seed %d: (I2) fired after %v, window %ds", seed, elapsed, window)
					}
				}
				seen = len(after)
				if fired[key] > 1 {
					t.Fatalf("seed %d: (I1) %s fired %d times", seed, key, fired[key])
				}
				if elapsed >= window*time.Second && fired[key] == 0 {
					t.Fatalf("seed %d: past the window (%v) yet nothing fired (%d→%d)", seed, elapsed, before, len(after))
				}
				// (I3)
				envs := r.violationEnvelopes(id)
				for _, e := range envs[envsSeen:] {
					if e.Audience != exp.AgentID {
						t.Fatalf("seed %d: (I3) warning addressed to %q, violator is %s", seed, e.Audience, exp.AgentID)
					}
				}
				envsSeen = len(envs)
			}
		}
		for _, agent := range r.ids {
			for _, e := range r.rec.forAgent(agent) {
				if e.EventType == EnvExpectationViolated && !strings.Contains(string(e.Payload), agent) {
					t.Fatalf("seed %d: (I3) %s received a warning about someone else: %s", seed, agent, e.Payload)
				}
			}
		}
	}
}
