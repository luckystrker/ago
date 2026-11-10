package network

import (
	"reflect"
	"testing"
	"time"

	"github.com/luckystrker/ago/task"
)

func TestQueryChannelsFilters(t *testing.T) {
	clock := newFakeClock()
	h := newTestHub(t, WithHubClock(clock.Now))
	ids := registerAgents(t, h, "alice", "bob", "carol")
	open := func(a, b string) string {
		clock.Advance(time.Second) // distinct creation times
		ch, err := h.CreateChannel(ChannelManifest{Type: TypeConversation}, []string{a, b})
		if err != nil {
			t.Fatal(err)
		}
		return ch.ID
	}
	ab, bc, ac := open(ids[0], ids[1]), open(ids[1], ids[2]), open(ids[0], ids[2])
	if err := h.CloseChannel(bc, "done"); err != nil {
		t.Fatal(err)
	}
	chIDs := func(q ChannelQuery) []string {
		var out []string
		for _, ch := range h.QueryChannels(q) {
			out = append(out, ch.ID)
		}
		return out
	}
	if got := chIDs(ChannelQuery{}); !reflect.DeepEqual(got, []string{ab, bc, ac}) {
		t.Fatalf("all, oldest first: %v", got)
	}
	if got := chIDs(ChannelQuery{AgentID: ids[0]}); !reflect.DeepEqual(got, []string{ab, ac}) {
		t.Fatalf("by agent: %v", got)
	}
	if got := chIDs(ChannelQuery{State: ChannelClosed}); !reflect.DeepEqual(got, []string{bc}) {
		t.Fatalf("by state: %v", got)
	}
	if got := chIDs(ChannelQuery{AgentID: ids[2], State: ChannelActive}); !reflect.DeepEqual(got, []string{ac}) {
		t.Fatalf("agent + state: %v", got)
	}
	if got := chIDs(ChannelQuery{Limit: 2}); !reflect.DeepEqual(got, []string{ab, bc}) {
		t.Fatalf("limit: %v", got)
	}
	// Results are snapshots.
	snap := h.QueryChannels(ChannelQuery{AgentID: ids[0]})[0]
	snap.State = ChannelExpired
	snap.Participants[0].AgentID = "x"
	if got, _ := h.Channel(ab); got.State != ChannelActive || got.Participants[0].AgentID != ids[0] {
		t.Fatal("QueryChannels leaked a live handle")
	}
}

func TestReadWALRange(t *testing.T) {
	h := newTestHub(t)
	alice, _, ch, _ := rawConversation(t, h, nil, nil)
	for _, m := range []string{"a", "b", "c", "d"} {
		if err := say(h, ch, alice, m); err != nil {
			t.Fatal(err)
		}
	}
	full, _ := h.ReadWAL(ch)
	n := len(full) // invite, acks, opened + 4 texts
	texts := func(es []Envelope) string {
		s := ""
		for _, e := range es {
			if e.EventType == EnvText {
				s += e.Text()
			}
		}
		return s
	}
	cases := []struct {
		since, until int
		want         int
	}{
		{0, -1, n}, {-5, -1, n}, {n - 2, -1, 2}, {1, 3, 2}, {0, 0, 0}, {3, 3, 0}, {4, 2, 0}, {n, -1, 0}, {n - 1, 999, 1},
	}
	for _, tc := range cases {
		got, err := h.ReadWALRange(ch, tc.since, tc.until)
		if err != nil || len(got) != tc.want {
			t.Errorf("[%d,%d): %d envelopes (err %v), want %d", tc.since, tc.until, len(got), err, tc.want)
		}
	}
	if got, _ := h.ReadWALRange(ch, n-3, -1); texts(got) != "bcd" {
		t.Errorf("tail texts: %q", texts(got))
	}
	if _, err := h.ReadWALRange("ghost", 0, -1); err == nil {
		t.Error("unknown channel must fail")
	}
}

func TestQueryAgents(t *testing.T) {
	h := newTestHub(t)
	reg := func(name string, kind PassportKind) string {
		p, err := h.Register(Passport{Name: name, Kind: kind}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return p.AgentID
	}
	zed, amy, hum := reg("zed", ""), reg("amy", KindAgent), reg("hana", KindHuman)
	_ = h.SetResume(Resume{AgentID: zed, Capabilities: []string{"math"}, Summary: "Adds numbers FAST"})
	_ = h.SetResume(Resume{AgentID: amy, Summary: "writes poems"})
	_ = h.RecordObservation(amy, "math", task.StateCompleted, -1, "t1") // observed counts
	names := func(q AgentQuery) []string {
		var out []string
		for _, p := range h.QueryAgents(q) {
			out = append(out, p.Name)
		}
		return out
	}
	if got := names(AgentQuery{}); !reflect.DeepEqual(got, []string{"zed", "amy", "hana"}) {
		t.Fatalf("registration order: %v", got)
	}
	if got := names(AgentQuery{SortBy: SortByName}); !reflect.DeepEqual(got, []string{"amy", "hana", "zed"}) {
		t.Fatalf("by name: %v", got)
	}
	if got := names(AgentQuery{Kind: KindAgent}); !reflect.DeepEqual(got, []string{"zed", "amy"}) {
		t.Fatalf("kind agent also matches the empty alias: %v", got)
	}
	if got := names(AgentQuery{Kind: KindHuman}); !reflect.DeepEqual(got, []string{"hana"}) {
		t.Fatalf("kind human: %v", got)
	}
	if got := names(AgentQuery{Capability: "math", SortBy: SortByName}); !reflect.DeepEqual(got, []string{"amy", "zed"}) {
		t.Fatalf("claimed or observed capability: %v", got)
	}
	if got := names(AgentQuery{Query: "fast"}); !reflect.DeepEqual(got, []string{"zed"}) {
		t.Fatalf("summary substring, case-insensitive: %v", got)
	}
	if got := names(AgentQuery{Query: "hana"}); len(got) != 0 {
		t.Fatalf("query matches the summary only: %v", got)
	}
	if got := names(AgentQuery{Limit: 1}); !reflect.DeepEqual(got, []string{"zed"}) {
		t.Fatalf("limit: %v", got)
	}
	_ = hum
}

func TestCanSendProbesTurnWithoutMutating(t *testing.T) {
	h := newTestHub(t)
	ids := registerAgents(t, h, "alice", "bob", "carol")
	a, b, c := ids[0], ids[1], ids[2]

	cons, err := h.CreateChannel(ChannelManifest{Type: TypeConsulting}, []string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	walLen := func(ch string) int { w, _ := h.ReadWAL(ch); return len(w) }
	before := walLen(cons.ID)
	if !h.CanSend(cons.ID, a, "") || h.CanSend(cons.ID, b, "") {
		t.Fatal("consulting: only the initiator may open")
	}
	if h.CanSend(cons.ID, c, "") {
		t.Fatal("a non-participant can never send")
	}
	if walLen(cons.ID) != before {
		t.Fatal("the probe recorded an envelope")
	}
	// Probing twice must not have advanced the 1Q1R state either.
	if !h.CanSend(cons.ID, a, EnvText) {
		t.Fatal("probe consumed the initiator's turn")
	}
	if _, err := h.Send(NewTextEnvelope(cons.ID, a, "", "q")); err != nil {
		t.Fatal(err)
	}
	if h.CanSend(cons.ID, a, "") || !h.CanSend(cons.ID, b, "") {
		t.Fatal("consulting: after the question it is the respondent's turn")
	}

	disc, _ := h.CreateChannel(ChannelManifest{Type: TypeDiscussion}, ids)
	if !h.CanSend(disc.ID, a, "") || h.CanSend(disc.ID, b, "") {
		t.Fatal("discussion: the rotation starts with the creator")
	}
	_, _ = h.Send(NewTextEnvelope(disc.ID, a, "", "x"))
	if !h.CanSend(disc.ID, b, "") || h.CanSend(disc.ID, a, "") {
		t.Fatal("discussion: next in rotation")
	}

	conv, _ := h.CreateChannel(ChannelManifest{Type: TypeConversation}, []string{b, c})
	if !h.CanSend(conv.ID, b, "") || !h.CanSend(conv.ID, c, "") || h.CanSend(conv.ID, a, "") {
		t.Fatal("conversation: any participant, nobody else")
	}

	graph, _ := NewSequenceGraph([]string{a, b})
	wf, _ := h.CreateChannel(ChannelManifest{Type: TypeWorkflow, Knobs: map[string]any{"graph": graph.ToDict()}}, []string{a, b})
	if !h.CanSend(wf.ID, a, "") || h.CanSend(wf.ID, b, "") {
		t.Fatal("workflow: the graph's initial speaker")
	}
	if !h.CanSend(wf.ID, b, EnvContextSet) {
		t.Fatal("workflow: context updates are loose")
	}

	if h.CanSend("ghost", a, "") {
		t.Fatal("unknown channel")
	}
	_ = h.CloseChannel(conv.ID, "x")
	if h.CanSend(conv.ID, b, "") {
		t.Fatal("closed channel")
	}
}

func TestFindEnvelopeByCausation(t *testing.T) {
	h := newTestHub(t)
	alice, bob, ch, _ := rawConversation(t, h, nil, nil)
	q, err := h.Send(NewTextEnvelope(ch, alice, "", "question"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h.FindEnvelopeByCausation(ch, bob, q.ID); ok {
		t.Fatal("nothing replied yet")
	}
	reply := NewTextEnvelope(ch, bob, "", "answer")
	reply.CausationID = q.ID
	sent, err := h.Send(reply)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := h.FindEnvelopeByCausation(ch, bob, q.ID)
	if !ok || got.ID != sent.ID || got.Text() != "answer" {
		t.Fatalf("lookup: %+v %v", got, ok)
	}
	// The key is (channel, sender, causation): other senders / causations miss.
	if _, ok := h.FindEnvelopeByCausation(ch, alice, q.ID); ok {
		t.Fatal("wrong sender hit")
	}
	if _, ok := h.FindEnvelopeByCausation(ch, bob, "other"); ok {
		t.Fatal("wrong causation hit")
	}
	if _, ok := h.FindEnvelopeByCausation("other-channel", bob, q.ID); ok {
		t.Fatal("wrong channel hit")
	}
	// A retry posted under the same causation: the latest reply wins.
	retry := NewTextEnvelope(ch, bob, "", "answer again")
	retry.CausationID = q.ID
	sent2, _ := h.Send(retry)
	if got, _ := h.FindEnvelopeByCausation(ch, bob, q.ID); got.ID != sent2.ID {
		t.Fatalf("latest wins: %s vs %s", got.ID, sent2.ID)
	}
	// The index is dropped when the channel closes.
	_ = h.CloseChannel(ch, "done")
	if _, ok := h.FindEnvelopeByCausation(ch, bob, q.ID); ok {
		t.Fatal("index must be pruned with the channel")
	}
}

func TestCausationIndexRebuiltOnReplay(t *testing.T) {
	wal := NewMemoryWAL()
	h := newTestHub(t, WithHubWAL(wal))
	alice, bob, ch, _ := rawConversation(t, h, nil, nil)
	q, _ := h.Send(NewTextEnvelope(ch, alice, "", "question"))
	reply := NewTextEnvelope(ch, bob, "", "answer")
	reply.CausationID = q.ID
	sent, _ := h.Send(reply)

	h2 := NewHub(WithHubWAL(wal))
	h2.RegisterDefaultAdapters()
	if err := h2.Replay(); err != nil {
		t.Fatal(err)
	}
	got, ok := h2.FindEnvelopeByCausation(ch, bob, q.ID)
	if !ok || got.ID != sent.ID {
		t.Fatalf("index after replay: %+v %v", got, ok)
	}
}

// noProbe is a free-form adapter that does not implement SendProber.
type noProbe struct{}

func (noProbe) Type() string                         { return "bare" }
func (noProbe) ValidateCreate(ChannelManifest) error { return nil }
func (noProbe) InitialState() any                    { return nil }
func (noProbe) OnEnvelope(*ChannelHandle, Envelope) (Routing, error) {
	return Routing{}, nil
}

func TestCanSendWithoutProberChecksChannelAndMembership(t *testing.T) {
	h := newTestHub(t)
	h.RegisterAdapter(noProbe{}, "1")
	ids := registerAgents(t, h, "alice", "bob", "carol")
	ch, err := h.CreateChannel(ChannelManifest{Type: "bare"}, ids[:2])
	if err != nil {
		t.Fatal(err)
	}
	if _, isProber := ChannelAdapter(noProbe{}).(SendProber); isProber {
		t.Fatal("fixture must not implement SendProber")
	}
	if !h.CanSend(ch.ID, ids[0], "") || !h.CanSend(ch.ID, ids[1], "") {
		t.Fatal("participants of an adapter with no turn rules may send")
	}
	if h.CanSend(ch.ID, ids[2], "") {
		t.Fatal("a non-participant may not, whatever the adapter says")
	}
}
