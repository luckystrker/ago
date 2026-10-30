package network

import (
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/ago/events"
)

// viewFixture builds a deterministic WAL: alice (self) and bob exchange
// three rounds of text on channel ch1, addressed broadcasts.
func viewFixture() []Envelope {
	at := func(min int) time.Time { return time.Unix(1700000000, int64(min)*int64(time.Minute)) }
	mk := func(id, sender, text string, min int) Envelope {
		env := NewTextEnvelope("ch1", sender, "", text)
		env.ID = id
		env.CreatedAt = at(min)
		return env
	}
	return []Envelope{
		mk("e1", "alice", "opening question", 0),
		mk("e2", "bob", "first answer", 1),
		mk("e3", "alice", "follow-up", 2),
		mk("e4", "bob", "second answer", 3),
		// Protocol bookkeeping: never projects.
		{ID: "e5", ChannelID: "ch1", SenderID: "bob", EventType: EnvContextSet, CreatedAt: at(4)},
	}
}

func nameAliceBob(id string) string {
	switch id {
	case "alice":
		return "Alice"
	case "bob":
		return "Bob"
	}
	return id
}

func optsFor(participant string) ViewOptions {
	return ViewOptions{NameFor: nameAliceBob}
}

// eventTypeList reduces a projection to "<type>:<content>" lines for the
// golden comparisons.
func eventTypeList(t *testing.T, evs []events.Event) []string {
	t.Helper()
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		switch e := ev.(type) {
		case events.ModelRequest:
			var texts []string
			for _, p := range e.Parts {
				if ti, ok := p.(*events.TextInput); ok {
					texts = append(texts, ti.Content)
				} else {
					texts = append(texts, "<?non-text>")
				}
			}
			out = append(out, "ModelRequest["+strings.Join(texts, " | ")+"]")
		case events.ModelMessage:
			out = append(out, "ModelMessage["+e.Content+"]")
		case *events.CompactionSummary:
			out = append(out, "CompactionSummary["+e.Content+"]")
		default:
			t.Fatalf("unexpected projection event %T", ev)
		}
	}
	return out
}

func TestFullTranscriptGolden(t *testing.T) {
	got := eventTypeList(t, FullTranscript{}.Project(viewFixture(), "alice", optsFor("alice")))
	want := []string{
		"ModelMessage[opening question]",
		"ModelRequest[first answer]",
		"ModelMessage[follow-up]",
		"ModelRequest[second answer]",
	}
	if len(got) != len(want) {
		t.Fatalf("projection length %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestNamedTranscriptGolden(t *testing.T) {
	got := eventTypeList(t, NamedTranscript{}.Project(viewFixture(), "alice", optsFor("alice")))
	want := []string{
		"ModelMessage[opening question]",
		"ModelRequest[[Bob]: first answer]",
		"ModelMessage[follow-up]",
		"ModelRequest[[Bob]: second answer]",
	}
	if len(got) != len(want) {
		t.Fatalf("projection length %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestWindowedSummaryGolden(t *testing.T) {
	view, err := NewWindowedSummary(2)
	if err != nil {
		t.Fatal(err)
	}
	got := eventTypeList(t, view.Project(viewFixture(), "alice", optsFor("alice")))
	// Elided: e1 (self) + e2 (bob) → unnamed window lists raw ids.
	want := []string{
		"CompactionSummary[Earlier in this channel: 2 messages from alice, bob.]",
		"ModelMessage[follow-up]",
		"ModelRequest[second answer]",
	}
	if len(got) != len(want) {
		t.Fatalf("projection length %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestNamedWindowedSummaryGolden(t *testing.T) {
	view, err := NewNamedWindowedSummary(2)
	if err != nil {
		t.Fatal(err)
	}
	got := eventTypeList(t, view.Project(viewFixture(), "alice", optsFor("alice")))
	want := []string{
		"CompactionSummary[Earlier in this channel: 2 messages from Alice, Bob.]",
		"ModelMessage[follow-up]",
		"ModelRequest[[Bob]: second answer]",
	}
	if len(got) != len(want) {
		t.Fatalf("projection length %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestWindowedSummaryNoElision(t *testing.T) {
	view, err := NewWindowedSummary(10)
	if err != nil {
		t.Fatal(err)
	}
	got := eventTypeList(t, view.Project(viewFixture(), "alice", ViewOptions{}))
	if len(got) != 4 || got[0] != "ModelMessage[opening question]" {
		t.Fatalf("short WAL must project verbatim with no head summary, got %v", got)
	}
}

func TestWindowedSummaryRejectsZero(t *testing.T) {
	if _, err := NewWindowedSummary(0); err == nil {
		t.Fatal("NewWindowedSummary(0) must fail")
	}
	if _, err := NewNamedWindowedSummary(0); err == nil {
		t.Fatal("NewNamedWindowedSummary(0) must fail")
	}
}

func TestViewDeterministic(t *testing.T) {
	wal := viewFixture()
	a := FullTranscript{}.Project(wal, "bob", ViewOptions{})
	b := FullTranscript{}.Project(wal, "bob", ViewOptions{})
	if len(a) != len(b) {
		t.Fatalf("lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if eventTypeList(t, a[i:][:1])[0] != eventTypeList(t, b[i:][:1])[0] {
			t.Fatalf("projection is not deterministic at %d", i)
		}
	}
}

func TestDefaultRenderEnvelopePacket(t *testing.T) {
	env := NewPacketEnvelope("ch1", "bob", "", "do the thing", WithHandoff(&Handoff{Target: "alice", Reason: "needs review"}))
	parts := DefaultRenderEnvelope(env)
	if len(parts) != 2 {
		t.Fatalf("packet renders 2 parts, got %d", len(parts))
	}
	ti0 := parts[0].(*events.TextInput)
	ti1 := parts[1].(*events.TextInput)
	if ti0.Content != "[Handed off via handoff] needs review" || ti1.Content != "do the thing" {
		t.Fatalf("unexpected parts: %q, %q", ti0.Content, ti1.Content)
	}
	// Non-substantive event types render nothing.
	if parts := DefaultRenderEnvelope(Envelope{EventType: EnvChannelOpened}); parts != nil {
		t.Fatalf("protocol envelope must not project, got %v", parts)
	}
}

func TestVisibleFilteringInView(t *testing.T) {
	wal := viewFixture()
	private := NewTextEnvelope("ch1", "bob", "alice", "private aside")
	private.ID = "e9"
	wal = append(wal, private)

	// Bob must not see his own addressed-away line as an inbound request.
	got := eventTypeList(t, FullTranscript{}.Project(wal, "bob", ViewOptions{}))
	for _, line := range got {
		if strings.Contains(line, "private aside") && strings.HasPrefix(line, "ModelRequest") {
			t.Fatalf("bob sees alice-addressed envelope as inbound: %v", got)
		}
	}
	// Alice sees it.
	got = eventTypeList(t, FullTranscript{}.Project(wal, "alice", ViewOptions{}))
	found := false
	for _, line := range got {
		if line == "ModelRequest[private aside]" {
			found = true
		}
	}
	if !found {
		t.Fatalf("alice must see the addressed envelope, got %v", got)
	}
}
