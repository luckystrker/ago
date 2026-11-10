package network

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestChannelManifestWireNames(t *testing.T) {
	m := ChannelManifest{
		Type:              TypeDiscussion,
		ParticipantSchema: &ParticipantSchema{Min: 2, Max: 5, Roles: []string{"initiator", "participant"}},
		KnobsSchema:       map[string]string{"ordering": "str"},
		DefaultViewPolicy: ViewNamedWindowedSummary,
		Labels:            map[string]string{LabelIntent: "plan the launch"},
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(data, &got)
	if got["default_view_policy"] != "named_windowed_summary" {
		t.Errorf("default_view_policy: %v", got["default_view_policy"])
	}
	if !reflect.DeepEqual(got["knobs_schema"], map[string]any{"ordering": "str"}) {
		t.Errorf("knobs_schema: %v", got["knobs_schema"])
	}
	if !reflect.DeepEqual(got["labels"], map[string]any{"intent": "plan the launch"}) {
		t.Errorf("labels: %v", got["labels"])
	}
	// Python ParticipantSchema field names: min / max / roles.
	if !reflect.DeepEqual(got["participant_schema"], map[string]any{
		"min": float64(2), "max": float64(5), "roles": []any{"initiator", "participant"},
	}) {
		t.Errorf("participant_schema: %v", got["participant_schema"])
	}
	var back ChannelManifest
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, m) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", back, m)
	}
	// A bare manifest keeps the old wire shape.
	bare, _ := json.Marshal(ChannelManifest{Type: TypeConsulting})
	if string(bare) != `{"type":"consulting"}` {
		t.Fatalf("bare manifest wire shape changed: %s", bare)
	}
}

func TestParticipantSchemaAllows(t *testing.T) {
	two := ParticipantSchema{Min: 2, Max: 2}
	open := ParticipantSchema{Min: 2}
	for n, want := range map[int]bool{0: false, 1: false, 2: true, 3: false} {
		if two.Allows(n) != want {
			t.Errorf("2..2 allows %d: want %v", n, want)
		}
	}
	for n, want := range map[int]bool{1: false, 2: true, 50: true} {
		if open.Allows(n) != want {
			t.Errorf("2.. allows %d: want %v", n, want)
		}
	}
}

// The declared schema is the single place a generic client reads participant
// bounds from; it must not drift from what ValidateCreate enforces.
func TestAdapterSchemaMatchesValidateCreate(t *testing.T) {
	graph, err := NewRoundRobinGraph([]string{"p0", "p1", "p2", "p3"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	adapters := []interface {
		ChannelAdapter
		ManifestProvider
	}{ConsultingAdapter{}, ConversationAdapter{}, DiscussionAdapter{}, WorkflowAdapter{}}
	for _, a := range adapters {
		def := a.DefaultManifest()
		if def.Type != a.Type() || def.ParticipantSchema == nil || def.DefaultViewPolicy == "" {
			t.Fatalf("%s: incomplete default manifest %+v", a.Type(), def)
		}
		for n := 0; n <= 4; n++ {
			ids := make([]string, n)
			for i := range ids {
				ids[i] = "p" + string(rune('0'+i))
			}
			m := ChannelManifest{Type: a.Type(), Participants: ids, Knobs: map[string]any{"graph": graph.ToDict()}}
			err := a.ValidateCreate(m)
			if allowed := def.ParticipantSchema.Allows(n); allowed != (err == nil) {
				t.Errorf("%s with %d participants: schema allows=%v, ValidateCreate err=%v", a.Type(), n, allowed, err)
			}
		}
	}
}

func TestCreateChannelAppliesAdapterDefaults(t *testing.T) {
	h := newTestHub(t)
	a, b := registerTwo(t, h)
	rec := newRecorder()
	rec.attach(h, a, true)
	rec.attach(h, b, true)

	ch, err := h.CreateChannel(ChannelManifest{Type: TypeConsulting}, []string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	m := ch.Manifest
	if m.DefaultViewPolicy != ViewFullTranscript {
		t.Errorf("default view: %q", m.DefaultViewPolicy)
	}
	if m.ParticipantSchema == nil || !reflect.DeepEqual(*m.ParticipantSchema,
		ParticipantSchema{Min: 2, Max: 2, Roles: []string{"initiator", "respondent"}}) {
		t.Errorf("participant schema: %+v", m.ParticipantSchema)
	}
	if len(m.Expectations) != 2 || m.Expectations[0].Kind != "acks_within" || m.Expectations[1].Kind != "reply_within" ||
		m.Expectations[1].OnViolation != "auto_close" {
		t.Errorf("default expectations: %+v", m.Expectations)
	}

	// Channel() hands out copies: mutating one must not leak into the hub.
	view, _ := h.Channel(ch.ID)
	view.Manifest.ParticipantSchema.Roles[0] = "x"
	view.Manifest.Labels = nil
	again, _ := h.Channel(ch.ID)
	if again.Manifest.ParticipantSchema.Roles[0] != "initiator" {
		t.Error("Channel() leaked schema aliasing")
	}
}

func TestCreateChannelKeepsCallerManifestFields(t *testing.T) {
	h := newTestHub(t)
	a, b := registerTwo(t, h)
	rec := newRecorder()
	rec.attach(h, a, true)
	rec.attach(h, b, true)

	custom := []Expectation{{Kind: "max_silence", OnViolation: "audit", Params: map[string]any{"seconds": 5}}}
	ch, err := h.CreateChannel(ChannelManifest{
		Type: TypeConsulting, Expectations: custom, DefaultViewPolicy: ViewNamedTranscript,
	}, []string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ch.Manifest.Expectations, custom) || ch.Manifest.DefaultViewPolicy != ViewNamedTranscript {
		t.Fatalf("explicit fields must win: %+v", ch.Manifest)
	}

	// An explicit empty list opts out of the adapter's expectations.
	c2, b2 := registerThree(t, h)
	rec.attach(h, c2, true)
	rec.attach(h, b2, true)
	optOut, err := h.CreateChannel(ChannelManifest{Type: TypeConsulting, Expectations: []Expectation{}}, []string{c2, b2})
	if err != nil {
		t.Fatal(err)
	}
	if len(optOut.Manifest.Expectations) != 0 {
		t.Fatalf("empty non-nil Expectations must opt out: %+v", optOut.Manifest.Expectations)
	}
}

func registerThree(t *testing.T, h *Hub) (string, string) {
	t.Helper()
	c, err := h.Register(Passport{Name: "carol"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d, err := h.Register(Passport{Name: "dave"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c.AgentID, d.AgentID
}

// The default expectations are live, not decoration: a respondent who never
// answers gets the channel closed by the sweeper.
func TestConsultingDefaultExpectationClosesSilentPeer(t *testing.T) {
	clock := newFakeClock()
	h := newTestHub(t, WithHubClock(clock.Now))
	a, b := registerTwo(t, h)
	rec := newRecorder()
	rec.attach(h, a, true)
	rec.attach(h, b, true)
	ch, err := h.CreateChannel(ChannelManifest{Type: TypeConsulting}, []string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Send(NewTextEnvelope(ch.ID, a, b, "are you there?")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(599 * time.Second)
	h.SweepExpectations()
	if got, _ := h.Channel(ch.ID); got.State != ChannelActive {
		t.Fatalf("closed before the 600s default: %s", got.State)
	}
	clock.Advance(2 * time.Second)
	h.SweepExpectations()
	got, _ := h.Channel(ch.ID)
	if got.State != ChannelClosed || got.CloseReason != "expectation_violated:reply_within" {
		t.Fatalf("silent respondent: state %s reason %q", got.State, got.CloseReason)
	}
}

func TestChannelLabelsSetAtCreationAndPersisted(t *testing.T) {
	wal := NewMemoryWAL()
	h := newTestHub(t, WithHubWAL(wal))
	a, b := registerTwo(t, h)
	rec := newRecorder()
	rec.attach(h, a, true)
	rec.attach(h, b, true)

	labels := map[string]string{LabelIntent: "triage", "team": "support"}
	ch, err := h.CreateChannel(ChannelManifest{Type: TypeConversation, Labels: labels}, []string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ch.Manifest.Labels, labels) {
		t.Fatalf("labels: %v", ch.Manifest.Labels)
	}
	// Channel() hands out copies.
	view, _ := h.Channel(ch.ID)
	view.Manifest.Labels["team"] = "changed"
	if again, _ := h.Channel(ch.ID); again.Manifest.Labels["team"] != "support" {
		t.Fatal("Channel() aliased the stored labels")
	}

	h2 := NewHub(WithHubWAL(wal))
	h2.RegisterAdapter(ConversationAdapter{}, "1")
	if err := h2.Replay(); err != nil {
		t.Fatal(err)
	}
	replayed, err := h2.Channel(ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replayed.Manifest.Labels, labels) {
		t.Fatalf("labels lost across replay: %v", replayed.Manifest.Labels)
	}
}

// Every snapshot the hub hands out — Channel, ListChannels and the handle
// CreateChannel returns — owns its labels and schemas.
func TestChannelSnapshotsDoNotAliasHubMetadata(t *testing.T) {
	h := newTestHub(t)
	a, b := registerTwo(t, h)
	rec := newRecorder()
	rec.attach(h, a, true)
	rec.attach(h, b, true)
	created, err := h.CreateChannel(ChannelManifest{
		Type: TypeConsulting, Labels: map[string]string{"team": "support"},
		KnobsSchema: map[string]string{"k": "str"},
	}, []string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	vandalize := func(src string, ch *ChannelHandle) {
		ch.Manifest.Labels["team"] = src
		ch.Manifest.KnobsSchema["k"] = src
		ch.Manifest.ParticipantSchema.Roles[0] = src
		ch.Manifest.ParticipantSchema.Max = 99
	}
	check := func(after string) {
		t.Helper()
		got, _ := h.Channel(created.ID)
		if got.Manifest.Labels["team"] != "support" || got.Manifest.KnobsSchema["k"] != "str" ||
			got.Manifest.ParticipantSchema.Roles[0] != "initiator" || got.Manifest.ParticipantSchema.Max != 2 {
			t.Fatalf("mutating the %s snapshot reached the hub: %+v / %+v", after, got.Manifest, *got.Manifest.ParticipantSchema)
		}
	}
	vandalize("created", created)
	check("CreateChannel")
	for _, ch := range h.ListChannels() {
		vandalize("listed", ch)
	}
	check("ListChannels")
}
