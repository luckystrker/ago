package network

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/ago/knowledge"
	"github.com/luckystrker/ago/task"
)

// Property test for wave-4 invariant 5 (spec/tla/HubRestart.tla) and 3b
// (HubCore NoDoubleAccept / IndexFaithful): random sequences of registry,
// channel, post, retried-post and ack operations with crashes at random write
// boundaries and restarts between operations. After every restart:
//
//   - the hub equals the store (independent parse of the files): registry,
//     rules, resumes, capability index, cursors, nacks, channels, WAL, index;
//   - nothing acknowledged is lost and nothing deleted resurrects;
//   - hydrating again changes nothing; the WAL only ever grows by appending;
//   - a post retried under one (channel, sender, causation) key is in the WAL
//     exactly once, whatever crash or restart separated the attempts.
//
// A failing sequence prints its seed; AGO_PROP_SEED=<n> reruns just that one.

const (
	propSequences = 2500
	propBaseSeed  = 20261111000
)

func TestHubPersistenceProperty(t *testing.T) {
	if s := os.Getenv("AGO_PROP_SEED"); s != "" {
		seed, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		runPropSequence(t, seed)
		return
	}
	propStats = map[string]int{}
	// AGO_PROP_SEQUENCES shortens the run (the mutation harness evaluates
	// dozens of mutants); the gate runs the full propSequences.
	sequences := propSequences
	if v, err := strconv.Atoi(os.Getenv("AGO_PROP_SEQUENCES")); err == nil && v > 0 {
		sequences = v
	}
	for i := 0; i < sequences; i++ {
		seed := int64(propBaseSeed + i)
		if !runPropSequence(t, seed) {
			return
		}
	}
	t.Logf("%d sequences, seeds %d..%d; coverage %v", sequences, propBaseSeed, propBaseSeed+sequences-1, propStats)
}

// --- model of the acknowledged state ----------------------------------------------------

type mAgent struct {
	name, passport, rule, resume string
	resumeVer                    int
	cursors                      map[string]string
	nacks                        int
}

type mChan struct {
	id           string
	parts        [2]string
	acked        []string          // envelope ids whose Send returned success
	keys         map[string]string // "sender|cause" → first accepted envelope id
	closedAcked  bool
	walBytes     []byte // WAL file content seen at the last check (append-only witness)
	causeCounter int
}

// intent describes the operation in flight when a crash hits.
type intent struct {
	kind     string
	agentID  string
	name     string
	ruleJSON string
	channel  string
	envelope string
}

type propRun struct {
	t       *testing.T
	seed    int64
	rng     *rand.Rand
	inner   *knowledge.MemoryStore
	cs      *crashStore
	clock   func() time.Time
	h       *Hub
	agents  map[string]*mAgent
	deleted map[string]bool
	// ackedDeleted holds the agents whose Unregister returned success: their
	// files must be gone from the store at once, before any hydrate can sweep
	// leftovers away.
	ackedDeleted map[string]bool
	chans        map[string]*mChan
	trace        []string
	restarts     int
}

// propStats counts what the random schedules reached (printed on success so
// a schedule generator that stops crashing anything is noticed).
var propStats map[string]int

func newPropHub(store knowledge.Store, clock func() time.Time) *Hub {
	h := NewHub(WithHubStore(store), WithAckTimeout(20*time.Millisecond), WithHubClock(clock))
	h.RegisterAdapter(ConsultingAdapter{}, "1")
	h.RegisterAdapter(ConversationAdapter{}, "1")
	h.RegisterAdapter(DiscussionAdapter{}, "1")
	h.RegisterAdapter(WorkflowAdapter{}, "1")
	return h
}

func runPropSequence(t *testing.T, seed int64) bool {
	t.Helper()
	inner := knowledge.NewMemoryStore()
	cs := newCrashStore(inner)
	r := &propRun{
		t: t, seed: seed, rng: rand.New(rand.NewSource(seed)),
		inner: inner, cs: cs, clock: testClock(),
		agents: map[string]*mAgent{}, deleted: map[string]bool{}, ackedDeleted: map[string]bool{}, chans: map[string]*mChan{},
	}
	h, err := r.openHub()
	if err != nil {
		return r.fail("initial open: %v", err)
	}
	r.h = h
	steps := 20 + r.rng.Intn(40)
	for i := 0; i < steps; i++ {
		if !r.step() {
			return false
		}
	}
	return r.restart(nil, "final") // every sequence ends with a verified restart
}

func (r *propRun) fail(format string, args ...any) bool {
	r.t.Helper()
	r.t.Errorf("seed %d (rerun: AGO_PROP_SEED=%d go test ./network -run TestHubPersistenceProperty)\n  %s\ntrace:\n    %s",
		r.seed, r.seed, fmt.Sprintf(format, args...), strings.Join(r.trace, "\n    "))
	return false
}

func (r *propRun) logf(format string, args ...any) {
	r.trace = append(r.trace, fmt.Sprintf(format, args...))
}

var propNames = []string{"a", "b", "c", "d"}

func (r *propRun) pickAgentID() (string, bool) {
	ids := make([]string, 0, len(r.agents))
	for id := range r.agents {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return "", false
	}
	sort.Strings(ids)
	return ids[r.rng.Intn(len(ids))], true
}

func (r *propRun) pickChan() (*mChan, bool) {
	ids := make([]string, 0, len(r.chans))
	for id := range r.chans {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, false
	}
	sort.Strings(ids)
	return r.chans[ids[r.rng.Intn(len(ids))]], true
}

var propRules = []Rule{
	{InboundGlobs: []string{"*"}, OutboundGlobs: []string{"*"}},
	{InboundGlobs: []string{"*"}, OutboundGlobs: []string{"*"}, ChannelTTLDefault: "2h", MaxDelegationDepth: 4},
	{InboundGlobs: []string{"a*", "b*"}, OutboundGlobs: []string{"*"}},
	{InboundGlobs: []string{"zzz"}, OutboundGlobs: []string{"zzz"}},
	{InboundGlobs: []string{"*"}, OutboundGlobs: []string{"*"}, Inbox: InboxBlock{MaxPending: 3}},
}

// step runs one random operation (maybe under an armed crash) and, when the
// store crashed or the dice say so, restarts and checks.
func (r *propRun) step() bool {
	armed := r.rng.Intn(4) == 0
	if armed {
		r.cs.arm(r.rng.Intn(5), r.rng.Intn(2) == 0)
	}
	in := r.op()
	crashed := r.cs.didCrash()
	r.cs.disarm()
	if crashed {
		return r.restart(&in, "crash")
	}
	if r.rng.Intn(7) == 0 {
		return r.restart(nil, "restart")
	}
	return true
}

func (r *propRun) op() intent {
	h := r.h
	n := r.rng.Intn(100)
	switch {
	case n < 14: // register
		name := propNames[r.rng.Intn(len(propNames))]
		rule := propRules[r.rng.Intn(len(propRules))]
		r.logf("register %s", name)
		p, err := h.Register(Passport{Name: name}, &rule)
		if err == nil {
			ackInvites(h, p.AgentID)
			r.agents[p.AgentID] = r.snapshotAgent(p.AgentID)
		}
		return intent{kind: "register", name: name}
	case n < 21: // unregister
		id, ok := r.pickAgentID()
		if !ok {
			return intent{}
		}
		r.logf("unregister %s", r.agents[id].name)
		if err := h.Unregister(id); err == nil {
			delete(r.agents, id)
			r.deleted[id] = true
			// A crash inside the best-effort directory delete still lets
			// Unregister report success (the passport, its commit point, is gone):
			// leftovers are only a bug when the process did not crash.
			if !r.cs.didCrash() {
				r.ackedDeleted[id] = true
			}
		}
		return intent{kind: "unregister", agentID: id}
	case n < 28: // set rule
		id, ok := r.pickAgentID()
		if !ok {
			return intent{}
		}
		rule := propRules[r.rng.Intn(len(propRules))]
		r.logf("setrule %s %s", r.agents[id].name, mustJSONString(rule))
		if err := h.SetRule(id, rule); err == nil {
			r.agents[id] = r.snapshotAgent(id)
		}
		return intent{kind: "setrule", agentID: id, ruleJSON: mustJSONString(rule.Clone())}
	case n < 36: // set resume
		id, ok := r.pickAgentID()
		if !ok {
			return intent{}
		}
		caps := []string{"c" + strconv.Itoa(r.rng.Intn(3)), "c" + strconv.Itoa(r.rng.Intn(3))}
		r.logf("setresume %s %v", r.agents[id].name, caps)
		if err := h.SetResume(Resume{AgentID: id, Capabilities: caps, Summary: "s"}); err == nil {
			r.agents[id] = r.snapshotAgent(id)
		}
		return intent{kind: "resume", agentID: id}
	case n < 42: // observation
		id, ok := r.pickAgentID()
		if !ok {
			return intent{}
		}
		capab := "c" + strconv.Itoa(r.rng.Intn(3))
		tid := "t" + strconv.Itoa(r.rng.Intn(4))
		r.logf("observe %s %s %s", r.agents[id].name, capab, tid)
		if err := h.RecordObservation(id, capab, task.StateCompleted, time.Duration(r.rng.Intn(50))*time.Millisecond, tid); err == nil {
			r.agents[id] = r.snapshotAgent(id)
		}
		return intent{kind: "resume", agentID: id}
	case n < 45: // example
		id, ok := r.pickAgentID()
		if !ok {
			return intent{}
		}
		r.logf("example %s", r.agents[id].name)
		if err := h.AddResumeExample(id, ResumeExample{Title: "e"}); err == nil {
			r.agents[id] = r.snapshotAgent(id)
		}
		return intent{kind: "resume", agentID: id}
	case n < 52: // open channel
		a, ok1 := r.pickAgentID()
		b, ok2 := r.pickAgentID()
		if !ok1 || !ok2 || a == b {
			return intent{}
		}
		r.logf("open %s %s", r.agents[a].name, r.agents[b].name)
		ch, err := h.CreateChannel(ChannelManifest{Type: TypeConversation}, []string{a, b})
		if err == nil {
			r.chans[ch.ID] = &mChan{id: ch.ID, parts: [2]string{a, b}, keys: map[string]string{}}
			r.acceptWAL(ch.ID)
		}
		return intent{kind: "open"}
	case n < 54: // close channel
		c, ok := r.pickChan()
		if !ok {
			return intent{}
		}
		r.logf("close %s", short(c.id))
		if err := h.CloseChannel(c.id, "done"); err == nil && !r.cs.didCrash() {
			c.closedAcked = true
			r.acceptWAL(c.id)
		}
		return intent{kind: "close", channel: c.id}
	case n < 78: // post (new key, retried key, or no key)
		c, ok := r.pickChan()
		if !ok {
			return intent{}
		}
		sender := c.parts[r.rng.Intn(2)]
		cause := ""
		switch x := r.rng.Intn(10); {
		case x < 4:
			c.causeCounter++
			cause = "k" + strconv.Itoa(c.causeCounter)
		case x < 9:
			cause = "k" + strconv.Itoa(1+r.rng.Intn(c.causeCounter+1)) // often a retry
		}
		env := NewTextEnvelope(c.id, sender, "", "t")
		env.CausationID = cause
		r.logf("post %s by %s cause=%q", short(c.id), r.name(sender), cause)
		got, err := h.Send(env)
		if err == nil {
			if got.SenderID != sender || got.CausationID != cause {
				r.fail("post by %s with cause %q was answered with the envelope of %s / cause %q", r.name(sender), cause, r.name(got.SenderID), got.CausationID)
			}
			key := sender + "|" + cause
			if cause != "" {
				if first, seen := c.keys[key]; seen && first != got.ID {
					r.fail("retried post %q accepted again: first %s, now %s", key, first, got.ID)
				} else if !seen {
					c.keys[key] = got.ID
				}
			}
			c.acked = append(c.acked, got.ID)
		}
		return intent{kind: "post", channel: c.id}
	case n < 90: // ack
		id, ok := r.pickAgentID()
		c, ok2 := r.pickChan()
		if !ok || !ok2 {
			return intent{}
		}
		envID := "0000"
		if log, err := h.ReadWAL(c.id); err == nil && len(log) > 0 && r.rng.Intn(5) > 0 {
			envID = log[r.rng.Intn(len(log))].ID
		}
		r.logf("ack %s %s %s", r.agents[id].name, short(c.id), short(envID))
		if err := h.AdvanceInboxCursor(id, c.id, envID); err == nil {
			r.agents[id] = r.snapshotAgent(id)
		}
		return intent{kind: "ack", agentID: id, channel: c.id, envelope: envID}
	case n < 95: // nack
		id, ok := r.pickAgentID()
		c, ok2 := r.pickChan()
		if !ok || !ok2 {
			return intent{}
		}
		r.logf("nack %s %s", r.agents[id].name, short(c.id))
		if err := h.RecordNack(id, c.id, "e", "r"); err == nil {
			r.agents[id] = r.snapshotAgent(id)
		}
		return intent{kind: "nack", agentID: id}
	default: // hydrate again on the live hub
		r.logf("hydrate (live)")
		before := stateFingerprint(h)
		if err := h.Hydrate(); err != nil {
			r.fail("live hydrate: %v", err)
		} else if after := stateFingerprint(h); after != before {
			r.fail("live hydrate changed state\n  before %s\n  after  %s", before, after)
		}
		return intent{}
	}
}

func (r *propRun) name(id string) string {
	if a, ok := r.agents[id]; ok {
		return a.name
	}
	return short(id)
}

func short(s string) string {
	if len(s) > 6 {
		return s[:6]
	}
	return s
}

// snapshotAgent captures the hub's in-memory view of an agent: after an
// acknowledged operation this IS the acknowledged state.
func (r *propRun) snapshotAgent(id string) *mAgent {
	h := r.h
	h.mu.Lock()
	defer h.mu.Unlock()
	a := &mAgent{name: h.passports[id].Name, passport: mustJSONString(h.passports[id]), rule: mustJSONString(h.rules[id]),
		cursors: map[string]string{}}
	if res, ok := h.resumes[id]; ok {
		a.resume, a.resumeVer = mustJSONString(res), res.Version
	}
	for c, e := range h.ext.cursors[id] {
		a.cursors[c] = e
	}
	a.nacks = len(h.ext.nacks[id])
	return a
}

func (r *propRun) acceptWAL(chID string) {
	if c, ok := r.chans[chID]; ok {
		body, _ := r.inner.Read("/channels/" + chID + "/wal.jsonl")
		c.walBytes = body
	}
}

// openHub starts a hub the way a process would: half of the time through the
// production entry point OpenHub, otherwise NewHub + Hydrate. Both must
// restore the registry from the store. Background sweepers are off.
func (r *propRun) openHub() (*Hub, error) {
	var h *Hub
	if r.rng.Intn(2) == 0 {
		r.logf("-- open via OpenHub")
		var err error
		h, err = OpenHub(WithHubStore(r.cs), WithAckTimeout(20*time.Millisecond), WithHubClock(r.clock),
			WithTTLSweepInterval(0), WithExpectationSweepInterval(0))
		if err != nil {
			return nil, err
		}
	} else {
		r.logf("-- open via NewHub+Hydrate")
		h = newPropHub(r.cs, r.clock)
		if err := h.Hydrate(); err != nil {
			return nil, err
		}
	}
	for _, p := range h.Registrations() {
		ackInvites(h, p.AgentID)
	}
	return h, nil
}

// --- restart and verification -------------------------------------------------------------

// restart kills the process (the old hub is abandoned), optionally crashes
// the first hydrate attempt, hydrates for real and verifies every property.
func (r *propRun) restart(in *intent, why string) bool {
	r.t.Helper()
	r.restarts++
	r.logf("-- %s (in flight: %+v)", why, in)
	if propStats != nil {
		propStats["restart"]++
		if why == "crash" {
			propStats["crash:"+in.kind]++
		}
	}
	r.cs.disarm()
	// Before any hydrate runs: an acknowledged unregister left nothing behind
	// (hydrate would otherwise mask a half-done delete).
	for id := range r.ackedDeleted {
		if r.inner.Exists("/agents/" + id) {
			return r.fail("acknowledged unregister of %s left files in the store (seen before hydrate)", short(id))
		}
	}
	if r.rng.Intn(3) == 0 { // crash inside hydrate, then hydrate again
		r.cs.arm(r.rng.Intn(3), r.rng.Intn(2) == 0)
		_, _ = r.openHub()
		r.cs.disarm()
		r.logf("-- a hydrate attempt was cut short, hydrating again")
	}
	h, err := r.openHub()
	if err != nil {
		return r.fail("open after %s: %v", why, err)
	}
	r.h = h
	if !r.verify(in) {
		return false
	}
	// hydrate twice == hydrate once
	once := stateFingerprint(h)
	if err := h.Hydrate(); err != nil {
		return r.fail("second hydrate: %v", err)
	}
	if twice := stateFingerprint(h); twice != once {
		return r.fail("hydrate twice differs\n  once  %s\n  twice %s", once, twice)
	}
	return true
}

func (r *propRun) verify(in *intent) bool {
	if !r.verifyAgainstStore() {
		return false
	}
	return r.verifyAcknowledged(in) && r.verifyChannels(in)
}

// verifyAgainstStore: the hub equals what the store says (the model's
// MemoryEqualsPersisted + NoOrphanCursor), parsed without the hub's loader.
func (r *propRun) verifyAgainstStore() bool {
	h, store := r.h, r.inner
	h.mu.Lock()
	defer h.mu.Unlock()
	dirs, _ := store.List("/agents")
	seen := map[string]bool{}
	caps := map[string][]string{}
	for _, d := range dirs {
		id := strings.TrimSuffix(d, "/")
		seen[id] = true
		raw, err := store.Read("/agents/" + id + "/passport.json")
		if err != nil {
			return r.fail("orphan agent directory %s left after hydrate (no passport.json)", id)
		}
		var p Passport
		if err := json.Unmarshal(raw, &p); err != nil {
			return r.fail("passport %s: %v", id, err)
		}
		if got, ok := h.passports[id]; !ok || mustJSONString(got) != mustJSONString(p) {
			return r.fail("passport %s differs from store: hub %v", id, h.passports[id])
		}
		if h.byName[p.Name] != id {
			return r.fail("name index missing %s", p.Name)
		}
		rawRule, err := store.Read("/agents/" + id + "/rule.json")
		if err != nil {
			return r.fail("passport %s committed without rule.json", id)
		}
		var rule Rule
		_ = json.Unmarshal(rawRule, &rule)
		if mustJSONString(h.rules[id]) != mustJSONString(rule) {
			return r.fail("rule %s differs from store", id)
		}
		if rawRes, err := store.Read("/agents/" + id + "/resume.json"); err == nil {
			var res Resume
			_ = json.Unmarshal(rawRes, &res)
			if mustJSONString(h.resumes[id]) != mustJSONString(res) {
				return r.fail("resume %s differs from store", id)
			}
			for c := range res.allCapabilities() {
				caps[c] = append(caps[c], id)
			}
		} else if _, has := h.resumes[id]; has {
			return r.fail("resume %s invented by hydrate", id)
		}
		var cur map[string]string
		if rawCur, err := store.Read("/agents/" + id + "/inbox.cursors.json"); err == nil {
			_ = json.Unmarshal(rawCur, &cur)
		}
		for c, e := range cur {
			if h.ext.cursors[id][c] != e {
				return r.fail("cursor %s/%s: hub %q store %q", id, c, h.ext.cursors[id][c], e)
			}
		}
		if len(h.ext.cursors[id]) != len(cur) {
			return r.fail("cursors of %s: hub %v store %v", id, h.ext.cursors[id], cur)
		}
		nacks, _ := store.Read("/agents/" + id + "/inbox_nacks.jsonl")
		if want := bytes.Count(nacks, []byte{'\n'}); len(h.ext.nacks[id]) != want {
			return r.fail("nacks of %s: hub %d store %d", id, len(h.ext.nacks[id]), want)
		}
	}
	for id := range h.passports {
		if !seen[id] {
			return r.fail("hub holds agent %s the store does not have", id)
		}
	}
	for id := range h.ext.cursors {
		if !seen[id] {
			return r.fail("orphan cursor of %s in memory", id)
		}
	}
	// capability index: memory == derived from the persisted resumes == the cache file
	for c := range caps {
		sort.Strings(caps[c])
	}
	memCaps := map[string][]string{}
	for c, ids := range h.ext.capIndex {
		for id := range ids {
			memCaps[c] = append(memCaps[c], id)
		}
		sort.Strings(memCaps[c])
	}
	if mustJSONString(memCaps) != mustJSONString(caps) {
		return r.fail("capability index %v differs from the resumes in the store %v", memCaps, caps)
	}
	var cache map[string][]string
	if raw, err := store.Read("/registry/by_capability.json"); err == nil {
		_ = json.Unmarshal(raw, &cache)
	}
	if len(caps) > 0 || cache != nil {
		if mustJSONString(cache) != mustJSONString(caps) {
			return r.fail("by_capability.json %v stale after hydrate, want %v", cache, caps)
		}
	}
	return true
}

// verifyAcknowledged: nothing acknowledged is lost, nothing deleted resurrects;
// the operation that was in flight at the crash may have landed or not, but
// atomically.
func (r *propRun) verifyAcknowledged(in *intent) bool {
	h := r.h
	h.mu.Lock()
	defer h.mu.Unlock()
	for id := range r.deleted {
		if _, ok := h.passports[id]; ok {
			return r.fail("deleted agent %s resurrected", id)
		}
		if _, ok := h.ext.cursors[id]; ok {
			return r.fail("deleted agent %s: cursor resurrected", id)
		}
		if r.inner.Exists("/agents/" + id) {
			return r.fail("deleted agent %s: files left in the store", id)
		}
	}
	snap := func(id string) *mAgent {
		a := &mAgent{name: h.passports[id].Name, passport: mustJSONString(h.passports[id]), rule: mustJSONString(h.rules[id]), cursors: map[string]string{}}
		if res, ok := h.resumes[id]; ok {
			a.resume, a.resumeVer = mustJSONString(res), res.Version
		}
		for c, e := range h.ext.cursors[id] {
			a.cursors[c] = e
		}
		a.nacks = len(h.ext.nacks[id])
		return a
	}
	for id, want := range r.agents {
		inflight := in != nil && in.agentID == id
		got, present := h.passports[id]
		if !present {
			if inflight && in.kind == "unregister" {
				delete(r.agents, id)
				r.deleted[id] = true
				continue
			}
			return r.fail("acknowledged registration of %s lost", want.name)
		}
		if mustJSONString(got) != want.passport {
			return r.fail("passport of %s changed", want.name)
		}
		have := snap(id)
		ok := true
		switch {
		case !inflight || in.kind == "unregister":
			ok = have.rule == want.rule && have.resume == want.resume && have.nacks == want.nacks && mustJSONString(have.cursors) == mustJSONString(want.cursors)
		case in.kind == "setrule":
			ok = (have.rule == want.rule || have.rule == in.ruleJSON) && have.resume == want.resume
		case in.kind == "resume":
			ok = have.rule == want.rule && (have.resume == want.resume || have.resumeVer == want.resumeVer+1 || (want.resumeVer == 0 && have.resumeVer >= 1))
		case in.kind == "ack":
			ok = have.rule == want.rule && have.resume == want.resume
			for c, e := range have.cursors {
				if e < want.cursors[c] {
					ok = false
				}
				if c != in.channel && e != want.cursors[c] {
					ok = false
				}
				if c == in.channel && e != want.cursors[c] && e != in.envelope {
					ok = false
				}
			}
			for c, e := range want.cursors {
				if have.cursors[c] < e {
					ok = false // a persisted cursor never decreases
				}
			}
		case in.kind == "nack":
			ok = have.nacks == want.nacks || have.nacks == want.nacks+1
		default:
			ok = have.rule == want.rule && have.resume == want.resume && have.nacks == want.nacks && mustJSONString(have.cursors) == mustJSONString(want.cursors)
		}
		if !ok {
			return r.fail("state of %s after restart is neither the acknowledged state nor the in-flight outcome\n  acknowledged %+v\n  restored     %+v\n  in flight    %+v", want.name, *want, *have, in)
		}
		r.agents[id] = have
	}
	for id, p := range h.passports {
		if _, known := r.agents[id]; known {
			continue
		}
		if in != nil && in.kind == "register" && in.name == p.Name {
			a := snap(id)
			r.agents[id] = a // the in-flight register landed: whole or not at all (verifyAgainstStore checked the rule file)
			continue
		}
		return r.fail("agent %s (%s) appeared from nowhere", p.Name, short(id))
	}
	return true
}

// verifyChannels: the WAL is append-only, holds every acknowledged envelope
// in accept order, holds each (sender, causation) key at most once, and the
// causation index equals the keys of the WAL of an open channel.
func (r *propRun) verifyChannels(in *intent) bool {
	h, store := r.h, r.inner
	h.mu.Lock()
	defer h.mu.Unlock()
	dirs, _ := store.List("/channels")
	onDisk := map[string]bool{}
	for _, d := range dirs {
		id := strings.TrimSuffix(d, "/")
		onDisk[id] = true
		raw, err := store.Read("/channels/" + id + "/metadata.json")
		if err != nil {
			continue // a WAL line without metadata is not a channel
		}
		var snap ChannelSnapshot
		_ = json.Unmarshal(raw, &snap)
		ch, ok := h.channels[id]
		if !ok || ch.State != snap.State {
			return r.fail("channel %s: hub %v, store state %s", short(id), ch, snap.State)
		}
		walRaw, _ := store.Read("/channels/" + id + "/wal.jsonl")
		var ids []string
		keys := map[string]int{}
		for _, line := range bytes.Split(walRaw, []byte{'\n'}) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var e Envelope
			if err := json.Unmarshal(line, &e); err != nil {
				return r.fail("channel %s: corrupt WAL line: %v", short(id), err)
			}
			ids = append(ids, e.ID)
			if e.CausationID != "" {
				keys[e.SenderID+"|"+e.CausationID]++
			}
		}
		if !sort.StringsAreSorted(ids) {
			return r.fail("channel %s: WAL out of accept order", short(id))
		}
		for k, n := range keys {
			// A post retried under one key was accepted once. The invite acks
			// (sender = invitee, causation = invite) are keyed the same way.
			if n != 1 {
				return r.fail("channel %s: key %s accepted %d times", short(id), k, n)
			}
		}
		var mem []string
		for _, e := range h.logs[id] {
			mem = append(mem, e.ID)
		}
		if strings.Join(mem, ",") != strings.Join(ids, ",") {
			return r.fail("channel %s: in-memory log %v != WAL %v", short(id), mem, ids)
		}
		// index = keys of the WAL (open channel) / empty (terminal channel)
		idx := h.ext.causation[id]
		if snap.State.Terminal() {
			if len(idx) != 0 {
				return r.fail("channel %s: causation index kept for a terminal channel", short(id))
			}
		} else if len(idx) != len(keys) {
			return r.fail("channel %s: causation index has %d keys, WAL has %d", short(id), len(idx), len(keys))
		}
		if c, ok := r.chans[id]; ok {
			if !bytes.HasPrefix(walRaw, c.walBytes) {
				return r.fail("channel %s: WAL was not append-only", short(id))
			}
			c.walBytes = walRaw
			have := map[string]bool{}
			for _, x := range ids {
				have[x] = true
			}
			for _, x := range c.acked {
				if !have[x] {
					return r.fail("channel %s: acknowledged envelope %s lost", short(id), short(x))
				}
			}
			if c.closedAcked && !snap.State.Terminal() {
				return r.fail("channel %s: acknowledged close lost (state %s)", short(id), snap.State)
			}
		} else {
			// a channel whose create crashed midway: adopt it
			r.chans[id] = &mChan{id: id, parts: [2]string{}, keys: map[string]string{}, walBytes: walRaw}
			r.chans[id].acked = nil
		}
	}
	for id := range h.channels {
		if !onDisk[id] {
			return r.fail("hub holds channel %s the store does not have", short(id))
		}
	}
	for id, c := range r.chans {
		if !onDisk[id] {
			if len(c.acked) > 0 || c.closedAcked || c.parts != [2]string{} {
				return r.fail("acknowledged channel %s lost", short(id))
			}
			delete(r.chans, id)
		}
	}
	// adopted channels have no participant pair: drop them from the op pool
	for id, c := range r.chans {
		if c.parts == [2]string{} {
			delete(r.chans, id)
		}
	}
	return true
}
