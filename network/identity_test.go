package network

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/luckystrker/ago/task"
)

// The JSON fixtures below are the verbatim output of Python's
// Passport.to_dict / Resume.to_dict (ag2/network/identity.py, json.dumps).

const pythonPassportJSON = `{"agent_id": "a1", "auth": {"audience": null, "claim": {}, "issuer": null, "key_fingerprint": null, "scheme": "none"}, "cost": {"input_per_mtok": 1.5, "latency_tier": "fast", "output_per_mtok": 7.5}, "created_at": "2026-11-09T10:00:00Z", "hub_id": null, "kind": null, "model": "claude", "name": "alice", "owner": "acme", "provider": "anthropic", "region": "eu", "version": 3}`

const pythonUnstampedPassportJSON = `{"agent_id": null, "auth": {"audience": null, "claim": {}, "issuer": null, "key_fingerprint": null, "scheme": "none"}, "cost": null, "created_at": "", "hub_id": null, "kind": null, "model": null, "name": "bob", "owner": "", "provider": null, "region": null, "version": 1}`

const pythonResumeJSON = `{"claimed_capabilities": ["math"], "domains": ["finance"], "examples": [{"channel_id": null, "note": "n", "outcome": "completed", "task_id": "t1", "title": "t", "when": "2026-11-09T10:00:00Z"}], "last_updated": "2026-11-09T10:00:00Z", "observed": {"math": {"completed": 2, "expired": 0, "failed": 1, "n": 3, "p50_latency_ms": 120}}, "summary": "adds numbers", "version": 4}`

const pythonEmptyResumeJSON = `{"claimed_capabilities": [], "domains": [], "examples": [], "last_updated": "", "observed": {}, "summary": "", "version": 1}`

func TestPassportDecodesPythonJSON(t *testing.T) {
	var p Passport
	if err := json.Unmarshal([]byte(pythonPassportJSON), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	if p.AgentID != "a1" || p.Name != "alice" || p.Owner != "acme" || p.Provider != "anthropic" ||
		p.Model != "claude" || p.Region != "eu" || p.Version != 3 || !p.CreatedAt.Equal(want) {
		t.Fatalf("metadata fields: %+v", p)
	}
	if p.Cost == nil || p.Cost.InputPerMTok != 1.5 || p.Cost.OutputPerMTok != 7.5 || p.Cost.LatencyTier != "fast" {
		t.Fatalf("cost: %+v", p.Cost)
	}
	if p.EffectiveKind() != KindAgent {
		t.Fatalf("kind null must resolve to agent: %q", p.Kind)
	}
	if scheme, _ := authSchemeAndClaim(p); scheme != AuthSchemeNone {
		t.Fatalf("python auth block scheme: %q", scheme)
	}

	// An unstamped Python passport has created_at "" and nulls.
	var bare Passport
	if err := json.Unmarshal([]byte(pythonUnstampedPassportJSON), &bare); err != nil {
		t.Fatalf("decode unstamped: %v", err)
	}
	if !bare.CreatedAt.IsZero() || bare.Name != "bob" || bare.Cost != nil || bare.Provider != "" {
		t.Fatalf("unstamped: %+v", bare)
	}
}

func TestPassportEncodesPythonFieldNames(t *testing.T) {
	p := Passport{
		AgentID: "a1", Name: "alice", Owner: "acme", Provider: "anthropic", Model: "claude",
		Region: "eu", Version: 3, CreatedAt: time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC),
		Cost: &CostProfile{InputPerMTok: 1.5, OutputPerMTok: 7.5, LatencyTier: "fast"},
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got, want map[string]any
	_ = json.Unmarshal(data, &got)
	_ = json.Unmarshal([]byte(pythonPassportJSON), &want)
	for _, key := range []string{"agent_id", "name", "owner", "provider", "model", "region", "version", "created_at", "cost"} {
		if !reflect.DeepEqual(got[key], want[key]) {
			t.Errorf("%s: go %v, python %v", key, got[key], want[key])
		}
	}
	// Unset metadata stays off the wire (including the zero time).
	bare, _ := json.Marshal(Passport{Name: "x"})
	var bareMap map[string]any
	_ = json.Unmarshal(bare, &bareMap)
	for _, key := range []string{"owner", "provider", "model", "region", "cost", "version", "created_at"} {
		if _, ok := bareMap[key]; ok {
			t.Errorf("%s must be omitted when unset: %s", key, bare)
		}
	}
}

func TestResumeDecodesPythonJSON(t *testing.T) {
	var r Resume
	if err := json.Unmarshal([]byte(pythonResumeJSON), &r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(r.Capabilities, []string{"math"}) || !reflect.DeepEqual(r.Domains, []string{"finance"}) || r.Summary != "adds numbers" {
		t.Fatalf("claims: %+v", r)
	}
	if len(r.Examples) != 1 || r.Examples[0] != (ResumeExample{Title: "t", Outcome: "completed", TaskID: "t1", When: "2026-11-09T10:00:00Z", Note: "n"}) {
		t.Fatalf("examples: %+v", r.Examples)
	}
	stat := r.Observed["math"]
	if stat.N != 3 || stat.Completed != 2 || stat.Failed != 1 || stat.Expired != 0 || stat.P50LatencyMS == nil || *stat.P50LatencyMS != 120 {
		t.Fatalf("observed: %+v", stat)
	}
	if r.Version != 4 || !r.LastUpdated.Equal(time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("version/last_updated: %d %v", r.Version, r.LastUpdated)
	}

	var empty Resume
	if err := json.Unmarshal([]byte(pythonEmptyResumeJSON), &empty); err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	if !empty.LastUpdated.IsZero() || empty.Version != 1 || len(empty.Capabilities) != 0 {
		t.Fatalf("empty: %+v", empty)
	}
}

func TestResumeEncodesPythonFieldNames(t *testing.T) {
	ms := 120
	r := Resume{
		AgentID: "a1", Capabilities: []string{"math"}, Domains: []string{"finance"}, Summary: "adds numbers",
		Examples:    []ResumeExample{{Title: "t", Outcome: "completed", TaskID: "t1", When: "2026-11-09T10:00:00Z", Note: "n"}},
		Observed:    map[string]ObservedStat{"math": {N: 3, Completed: 2, Failed: 1, P50LatencyMS: &ms}},
		Version:     4,
		LastUpdated: time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC),
	}
	data, _ := json.Marshal(r)
	var got, want map[string]any
	_ = json.Unmarshal(data, &got)
	_ = json.Unmarshal([]byte(pythonResumeJSON), &want)
	for _, key := range []string{"domains", "summary", "observed", "version", "last_updated"} {
		if !reflect.DeepEqual(got[key], want[key]) {
			t.Errorf("%s: go %v, python %v", key, got[key], want[key])
		}
	}
	// Examples differ from Python only by omitted empties (channel_id null).
	ex := got["examples"].([]any)[0].(map[string]any)
	for _, key := range []string{"title", "outcome", "task_id", "when", "note"} {
		if ex[key] != want["examples"].([]any)[0].(map[string]any)[key] {
			t.Errorf("example %s: %v", key, ex[key])
		}
	}
	if _, ok := got["capabilities"]; !ok {
		t.Errorf("capabilities key missing: %s", data)
	}
}

func TestHubRegisterStampsPassportMetadata(t *testing.T) {
	clock := newFakeClock()
	h := newTestHub(t, WithHubClock(clock.Now))
	p, err := h.Register(Passport{Name: "alice", Owner: "acme", Provider: "anthropic", Model: "claude", Region: "eu"}, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if p.Version != 1 || !p.CreatedAt.Equal(clock.Now()) {
		t.Fatalf("hub must stamp version and created_at: %+v", p)
	}
	got, _ := h.Passport("alice")
	if got.Owner != "acme" || got.Provider != "anthropic" || got.Model != "claude" || got.Region != "eu" {
		t.Fatalf("stored passport lost metadata: %+v", got)
	}
	// A caller-chosen version is kept.
	q, _ := h.Register(Passport{Name: "bob", Version: 7}, nil)
	if q.Version != 7 {
		t.Fatalf("explicit version: %d", q.Version)
	}
}

func TestHubSetResumeStampsVersionAndIndexesCapabilities(t *testing.T) {
	clock := newFakeClock()
	h := newTestHub(t, WithHubClock(clock.Now))
	a, b := registerTwo(t, h)

	if err := h.SetResume(Resume{AgentID: a, Capabilities: []string{"math", "poetry"}, Summary: "numbers"}); err != nil {
		t.Fatal(err)
	}
	first, _ := h.ResumeOf(a)
	if first.Version != 1 || !first.LastUpdated.Equal(clock.Now()) || first.Summary != "numbers" {
		t.Fatalf("first resume: %+v", first)
	}
	clock.Advance(time.Minute)
	if err := h.SetResume(Resume{AgentID: a, Capabilities: []string{"math", "chess"}}); err != nil {
		t.Fatal(err)
	}
	second, _ := h.ResumeOf(a)
	if second.Version != 2 || !second.LastUpdated.Equal(clock.Now()) {
		t.Fatalf("second resume: %+v", second)
	}

	if got := h.AgentsWithCapability("math"); !reflect.DeepEqual(got, []string{a}) {
		t.Fatalf("math index: %v", got)
	}
	if got := h.AgentsWithCapability("chess"); !reflect.DeepEqual(got, []string{a}) {
		t.Fatalf("chess index after set: %v", got)
	}
	if got := h.AgentsWithCapability("poetry"); len(got) != 0 {
		t.Fatalf("removed claim must leave the index: %v", got)
	}

	_ = h.SetResume(Resume{AgentID: b, Capabilities: []string{"math"}})
	want := []string{a, b}
	if want[0] > want[1] {
		want[0], want[1] = want[1], want[0]
	}
	if got := h.AgentsWithCapability("math"); !reflect.DeepEqual(got, want) {
		t.Fatalf("two agents on math: %v want %v", got, want)
	}
	if err := h.Unregister(a); err != nil {
		t.Fatal(err)
	}
	if got := h.AgentsWithCapability("math"); !reflect.DeepEqual(got, []string{b}) {
		t.Fatalf("unregistered agent must leave the index: %v", got)
	}
	if got := h.AgentsWithCapability("chess"); len(got) != 0 {
		t.Fatalf("chess after unregister: %v", got)
	}
}

func TestHubResumeOfReturnsIndependentCopy(t *testing.T) {
	h := newTestHub(t)
	a, _ := registerTwo(t, h)
	_ = h.SetResume(Resume{AgentID: a, Capabilities: []string{"math"}})
	got, _ := h.ResumeOf(a)
	got.Capabilities[0] = "mutated"
	again, _ := h.ResumeOf(a)
	if again.Capabilities[0] != "math" {
		t.Fatalf("stored resume aliased through ResumeOf: %v", again.Capabilities)
	}
}

func TestHubRecordObservation(t *testing.T) {
	clock := newFakeClock()
	h := newTestHub(t, WithHubClock(clock.Now))
	a, _ := registerTwo(t, h)

	// No resume yet: one is created on demand and the capability indexed.
	if err := h.RecordObservation(a, "grinding", task.StateCompleted, 1500*time.Millisecond, "t-1"); err != nil {
		t.Fatal(err)
	}
	r, _ := h.ResumeOf(a)
	stat := r.Observed["grinding"]
	if stat.N != 1 || stat.Completed != 1 || stat.P50LatencyMS == nil || *stat.P50LatencyMS != 1500 {
		t.Fatalf("after completed: %+v", stat)
	}
	if r.Version != 2 || !r.LastUpdated.Equal(clock.Now()) {
		t.Fatalf("version/last_updated: %+v", r)
	}
	if got := h.AgentsWithCapability("grinding"); !reflect.DeepEqual(got, []string{a}) {
		t.Fatalf("observed capability must be indexed: %v", got)
	}

	// Failed / expired / cancelled; cancelled counts toward n only.
	// A zero latency is a measurement, not "unmeasured".
	_ = h.RecordObservation(a, "instant", task.StateCompleted, 0, "t-0")
	if z, _ := h.ResumeOf(a); z.Observed["instant"].P50LatencyMS == nil || *z.Observed["instant"].P50LatencyMS != 0 {
		t.Fatalf("zero latency must be recorded: %+v", z.Observed["instant"])
	}
	_ = h.RecordObservation(a, "grinding", task.StateFailed, -1, "t-2")
	_ = h.RecordObservation(a, "grinding", task.StateExpired, -1, "t-3")
	_ = h.RecordObservation(a, "grinding", task.StateCancelled, -1, "t-4")
	r, _ = h.ResumeOf(a)
	stat = r.Observed["grinding"]
	if stat.N != 4 || stat.Completed != 1 || stat.Failed != 1 || stat.Expired != 1 {
		t.Fatalf("counters: %+v", stat)
	}
	if *stat.P50LatencyMS != 1500 {
		t.Fatalf("unmeasured latency must not overwrite: %d", *stat.P50LatencyMS)
	}

	// The same task counts once; a non-terminal outcome is ignored.
	_ = h.RecordObservation(a, "grinding", task.StateCompleted, time.Second, "t-1")
	_ = h.RecordObservation(a, "grinding", task.StateRunning, time.Second, "t-5")
	r, _ = h.ResumeOf(a)
	if r.Observed["grinding"].N != 4 {
		t.Fatalf("dedupe/non-terminal leaked: %+v", r.Observed["grinding"])
	}

	if err := h.RecordObservation("ghost", "x", task.StateCompleted, 0, "t"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown owner: %v", err)
	}
}

func TestHubAddResumeExample(t *testing.T) {
	clock := newFakeClock()
	h := newTestHub(t, WithHubClock(clock.Now))
	hc := NewHubClient(h)
	alice, err := hc.Register("alice", WithAgentResume(Resume{Capabilities: []string{"math"}}))
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()

	if err := alice.AddExample(ResumeExample{Title: "sum", Outcome: "completed"}); err != nil {
		t.Fatal(err)
	}
	// A concurrent observation is not clobbered by the next example.
	_ = h.RecordObservation(alice.AgentID(), "math", task.StateCompleted, -1, "t-1")
	if err := alice.AddExample(ResumeExample{Title: "product", When: "2026-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	r, _ := h.ResumeOf(alice.AgentID())
	if len(r.Examples) != 2 || r.Examples[0].Title != "sum" || r.Examples[1].Title != "product" {
		t.Fatalf("examples: %+v", r.Examples)
	}
	if r.Examples[0].When != clock.Now().UTC().Format(time.RFC3339) || r.Examples[1].When != "2026-01-01T00:00:00Z" {
		t.Fatalf("when stamping: %+v", r.Examples)
	}
	if r.Observed["math"].N != 1 || len(r.Capabilities) != 1 {
		t.Fatalf("add example clobbered the resume: %+v", r)
	}
	if r.Version != 4 { // set(1) → add(2) → observation(3) → add(4)
		t.Fatalf("version bumps: %d", r.Version)
	}
	if err := h.AddResumeExample("ghost", ResumeExample{Title: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown agent: %v", err)
	}
}

func TestResumeCloneIsDeep(t *testing.T) {
	ms := 7
	orig := Resume{
		Capabilities: []string{"a"}, Domains: []string{"d"}, Examples: []ResumeExample{{Title: "e"}},
		Observed: map[string]ObservedStat{
			"measured":   {N: 1, P50LatencyMS: &ms},
			"unmeasured": {N: 2}, // nil latency must clone without panicking
		},
	}
	cp := orig.Clone()
	if cp.Observed["unmeasured"].P50LatencyMS != nil || cp.Observed["unmeasured"].N != 2 {
		t.Fatalf("unmeasured stat: %+v", cp.Observed["unmeasured"])
	}
	ms = 99 // mutate through the original's pointer
	orig.Capabilities[0], orig.Domains[0], orig.Examples[0].Title = "x", "x", "x"
	orig.Observed["measured"] = ObservedStat{N: 100}
	if got := *cp.Observed["measured"].P50LatencyMS; got != 7 {
		t.Fatalf("latency pointer shared with the original: %d", got)
	}
	if cp.Capabilities[0] != "a" || cp.Domains[0] != "d" || cp.Examples[0].Title != "e" || cp.Observed["measured"].N != 1 {
		t.Fatalf("clone aliases the original: %+v", cp)
	}
}
