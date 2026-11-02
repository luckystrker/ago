package stream

import (
	"errors"
	"math/rand"
	"reflect"
	"sync"
	"testing"
	"testing/quick"

	"github.com/luckystrker/ago/events"
)

// countingStorage counts GetHistory reads: query construction must be lazy and
// every terminal must take its own fresh snapshot (Python parity: the
// RecordedStorage laziness test).
type countingStorage struct {
	*MemoryStorage
	mu    sync.Mutex
	reads int
}

func (c *countingStorage) GetHistory(id string) ([]events.Event, error) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	return c.MemoryStorage.GetHistory(id)
}

func (c *countingStorage) readsSoFar() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

// failingStorage fails every read (error propagation parity).
type failingStorage struct{ err error }

func (f *failingStorage) SaveEvent(string, events.Event) error      { return nil }
func (f *failingStorage) GetHistory(string) ([]events.Event, error) { return nil, f.err }
func (f *failingStorage) SetHistory(string, []events.Event) error   { return nil }
func (f *failingStorage) DropHistory(string) error                  { return nil }

func textInput(s string) *events.TextInput { return &events.TextInput{Content: s} }

// querySeed builds a transcript covering user/service requests, drains,
// responses and plain inputs.
func querySeed() []events.Event {
	return []events.Event{
		textInput("old"),
		&events.ModelRequest{Origin: events.OriginUser, Parts: []events.Input{textInput("q1")}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: "r1"}},
		&events.DrainedModelRequest{ModelRequest: events.ModelRequest{Origin: events.OriginService}},
		textInput("mid"),
		&events.ModelRequest{Origin: events.OriginUser, Parts: []events.Input{textInput("q2")}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: "r2"}},
	}
}

func TestHistoryQueryPublicationOrderWithRepeats(t *testing.T) {
	h := NewHistory("q", NewMemoryStorage())
	if err := h.Append([]events.Event{textInput("a"), textInput("b"), textInput("a")}...); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, err := h.Where(events.TypeOf[events.TextInput]()).All()
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 matches with the repeat kept, got %d", len(got))
	}
	for i, want := range []string{"a", "b", "a"} {
		ti, ok := events.AsValue(got[i]).(events.TextInput)
		if !ok || ti.Content != want {
			t.Fatalf("got[%d] = %#v, want TextInput %q (order must follow publication)", i, got[i], want)
		}
	}
	first, ok, err := h.Where(events.TypeOf[events.TextInput]()).First()
	if err != nil || !ok || events.AsValue(first).(events.TextInput).Content != "a" {
		t.Fatalf("first: %v %v %#v", ok, err, first)
	}
	last, ok, err := h.Where(events.TypeOf[events.TextInput]()).Last()
	if err != nil || !ok || events.AsValue(last).(events.TextInput).Content != "a" {
		t.Fatalf("last: %v %v %#v", ok, err, last)
	}
}

// TestHistoryQueryTopLevelOccurrences pins that a batch ToolCallsEvent is one
// occurrence and is never unfolded into its calls (Python parity:
// test_history_queries_keep_top_level_occurrences).
func TestHistoryQueryTopLevelOccurrences(t *testing.T) {
	h := NewHistory("q", NewMemoryStorage())
	batch := events.ToolCallsEvent{Calls: []events.ToolCallEvent{
		events.NewToolCallEvent("a"), events.NewToolCallEvent("b"),
	}}
	if err := h.Append([]events.Event{&batch}...); err != nil {
		t.Fatalf("append: %v", err)
	}
	calls, err := h.Where(events.TypeOf[events.ToolCallEvent]()).All()
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("batch must not unfold into calls, got %d", len(calls))
	}
	n, err := h.Where(events.TypeOf[events.ToolCallsEvent]()).Count()
	if err != nil || n != 1 {
		t.Fatalf("batch count = %d err=%v, want 1", n, err)
	}
}

func TestHistoryQueryWhereChainsANDWithoutMutation(t *testing.T) {
	h := NewHistory("q", NewMemoryStorage())
	if err := h.Append([]events.Event{textInput("x"), textInput("y"), textInput("x")}...); err != nil {
		t.Fatalf("append: %v", err)
	}
	q1 := h.Where(events.TypeOf[events.TextInput]())
	q2 := q1.Where(events.Equal(func(ev events.Event) string {
		if ti, ok := events.AsValue(ev).(events.TextInput); ok {
			return ti.Content
		}
		return ""
	}, "x"))
	broad, err := q1.All()
	if err != nil {
		t.Fatalf("q1 all: %v", err)
	}
	if len(broad) != 3 {
		t.Fatalf("q1 = %d matches, want 3 (narrower query must not mutate its source)", len(broad))
	}
	narrow, err := q2.All()
	if err != nil {
		t.Fatalf("q2 all: %v", err)
	}
	if len(narrow) != 2 {
		t.Fatalf("q2 = %d matches, want 2 (AND combination)", len(narrow))
	}
	// nil condition matches everything and does not narrow.
	all, err := q1.Where(nil).All()
	if err != nil {
		t.Fatalf("nil where: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("nil condition must match all, got %d", len(all))
	}
	// A nil start matches everything too.
	everything, err := h.Where(nil).All()
	if err != nil || len(everything) != 3 {
		t.Fatalf("Where(nil) from history: %d %v", len(everything), err)
	}
}

func TestHistoryQueryLazyAndFreshSnapshots(t *testing.T) {
	cs := &countingStorage{MemoryStorage: NewMemoryStorage()}
	h := NewHistory("q", cs)
	if err := h.Append([]events.Event{
		&events.ModelRequest{Origin: events.OriginUser},
		textInput("a"),
		textInput("b"),
	}...); err != nil {
		t.Fatalf("append: %v", err)
	}
	q := h.Where(events.TypeOf[events.TextInput]()).SinceLastUserInput()
	before := cs.readsSoFar() // Append above already read once; construction must add nothing
	if reads := cs.readsSoFar(); reads != before {
		t.Fatalf("query construction read storage %d times, want 0", reads-before)
	}
	if _, err := q.All(); err != nil {
		t.Fatalf("all: %v", err)
	}
	if _, _, err := q.First(); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, _, err := q.Last(); err != nil {
		t.Fatalf("last: %v", err)
	}
	if _, err := q.Count(); err != nil {
		t.Fatalf("count: %v", err)
	}
	// Four terminals, four fresh reads.
	if reads := cs.readsSoFar(); reads != before+4 {
		t.Fatalf("terminals read storage %d times, want 4 (fresh snapshot per terminal)", reads-before)
	}
	// The snapshot is fresh: mutating a returned slice cannot affect later reads.
	got, err := q.All()
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	got[0] = nil
	again, err := q.All()
	if err != nil {
		t.Fatalf("all again: %v", err)
	}
	if len(again) != 2 || again[0] == nil {
		t.Fatalf("fresh snapshot corrupted: len=%d first=%#v", len(again), again[0])
	}
}

func TestHistoryQueryEmptyHistory(t *testing.T) {
	h := NewHistory("q", NewMemoryStorage())
	got, err := h.Where(events.TypeOf[events.TextInput]()).All()
	if err != nil || len(got) != 0 {
		t.Fatalf("all = %d %v, want empty", len(got), err)
	}
	if _, ok, err := h.Where(nil).First(); ok || err != nil {
		t.Fatalf("first = %v %v, want ok=false", ok, err)
	}
	if _, ok, err := h.Where(nil).Last(); ok || err != nil {
		t.Fatalf("last = %v %v, want ok=false", ok, err)
	}
	if n, err := h.Where(nil).Count(); n != 0 || err != nil {
		t.Fatalf("count = %d %v, want 0", n, err)
	}
}

func TestHistoryQueryStorageErrorPropagates(t *testing.T) {
	want := errors.New("storage down")
	h := NewHistory("q", &failingStorage{err: want})
	if _, err := h.Where(nil).All(); !errors.Is(err, want) {
		t.Fatalf("all error = %v, want %v", err, want)
	}
	if _, _, err := h.Where(nil).First(); !errors.Is(err, want) {
		t.Fatalf("first error = %v, want %v", err, want)
	}
	if _, _, err := h.Where(nil).Last(); !errors.Is(err, want) {
		t.Fatalf("last error = %v, want %v", err, want)
	}
	if _, err := h.Where(nil).Count(); !errors.Is(err, want) {
		t.Fatalf("count error = %v, want %v", err, want)
	}
}

// TestHistoryQueryScopeLastUserInput pins the since-last-user-input semantics:
// the boundary is the last user-origin request (ModelRequest or
// DrainedModelRequest), the boundary itself is excluded, the cut runs before
// any filter, and a missing boundary yields an empty result.
func TestHistoryQueryScopeLastUserInput(t *testing.T) {
	h := NewHistory("q", NewMemoryStorage())
	if err := h.Append(querySeed()...); err != nil {
		t.Fatalf("append: %v", err)
	}
	q := h.Where(events.TypeOf[events.ModelResponse]()).SinceLastUserInput()
	got, err := q.All()
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	// Only "r2" survives: the cut lands before the filter, so "r1" — matching
	// the filter but published before the boundary — is out of scope too.
	if len(got) != 1 || events.AsValue(got[0]).(events.ModelResponse).Content() != "r2" {
		t.Fatalf("scoped responses = %#v, want [r2]", got)
	}
	// The boundary itself is excluded: no requests after the last user input.
	reqs, err := h.Where(events.TypeOf[events.ModelRequest]()).SinceLastUserInput().All()
	if err != nil || len(reqs) != 0 {
		t.Fatalf("scoped requests = %d %v, want 0 (boundary excluded)", len(reqs), err)
	}
	first, ok, err := h.Where(nil).SinceLastUserInput().First()
	if err != nil || !ok || events.AsValue(first).(events.ModelResponse).Content() != "r2" {
		t.Fatalf("scoped first = %v %v %#v, want r2", ok, err, first)
	}
	if n, err := h.Where(nil).SinceLastUserInput().Count(); n != 1 || err != nil {
		t.Fatalf("scoped count = %d %v, want 1", n, err)
	}
}

// TestHistoryQueryScopeBoundaryForms pins which request shapes open an interval:
// both ModelRequest and DrainedModelRequest count when their origin is user
// (embedding is not inheritance — each form is asserted), and no other event
// does.
func TestHistoryQueryScopeBoundaryForms(t *testing.T) {
	tests := []struct {
		name string
		seed []events.Event
		want []string // contents of the scoped TextInputs in order (nil = none)
	}{
		{
			"model request opens",
			[]events.Event{
				&events.ModelRequest{Origin: events.OriginUser},
				textInput("after"),
				&events.ModelResponse{},
			},
			[]string{"after"},
		},
		{
			"drained request opens",
			[]events.Event{
				&events.DrainedModelRequest{ModelRequest: events.ModelRequest{Origin: events.OriginUser}},
				textInput("after"),
			},
			[]string{"after"},
		},
		{
			"service drained does not open",
			[]events.Event{
				&events.ModelRequest{Origin: events.OriginUser},
				&events.ModelResponse{},
				&events.DrainedModelRequest{ModelRequest: events.ModelRequest{Origin: events.OriginService}},
				textInput("after"),
			},
			[]string{"after"},
		},
		{
			// Legacy records without origin: a request counts as user, a drained
			// merge counts as service (Python field defaults). The "mid" input
			// between the user request and the legacy drain is what discriminates:
			// a drain wrongly opening an interval would drop it from the scope.
			"legacy drained does not open",
			[]events.Event{
				&events.ModelRequest{Origin: events.OriginUser},
				textInput("mid"),
				&events.DrainedModelRequest{},
				textInput("after"),
			},
			[]string{"mid", "after"},
		},
		{
			// Legacy record without origin: the request itself counts as user
			// (the Python field default — the only ""→user fallback).
			"legacy request opens",
			[]events.Event{
				&events.ModelRequest{},
				textInput("after"),
			},
			[]string{"after"},
		},
		{
			// The latest boundary wins: the scope restarts at the second request.
			"latest boundary wins",
			[]events.Event{
				&events.ModelRequest{Origin: events.OriginUser},
				textInput("first-interval"),
				&events.ModelRequest{Origin: events.OriginUser},
				textInput("second-interval"),
			},
			[]string{"second-interval"},
		},
		{
			"no boundary, empty scope",
			[]events.Event{textInput("lone"), &events.ModelResponse{}},
			nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHistory("q", NewMemoryStorage())
			if err := h.Append(tc.seed...); err != nil {
				t.Fatalf("append: %v", err)
			}
			got, err := h.Where(events.TypeOf[events.TextInput]()).SinceLastUserInput().All()
			if err != nil {
				t.Fatalf("all: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("scoped inputs = %d events, want %d", len(got), len(tc.want))
			}
			for i, want := range tc.want {
				ti, ok := events.AsValue(got[i]).(events.TextInput)
				if !ok || ti.Content != want {
					t.Fatalf("scoped input[%d] = %#v, want %q", i, got[i], want)
				}
			}
		})
	}
}

// TestHistoryQueryScopeBoundaryEvicted pins retention interaction: when the
// boundary left the retained transcript (cap eviction or Replace), the scope is
// empty — no saved boundary, empty result (Python parity).
func TestHistoryQueryScopeBoundaryEvicted(t *testing.T) {
	h := NewHistory("q", NewMemoryStorage())
	h.SetCap(2)
	if err := h.Append([]events.Event{
		&events.ModelRequest{Origin: events.OriginUser},
		textInput("1"),
		textInput("2"),
	}...); err != nil {
		t.Fatalf("append: %v", err)
	}
	// The cap evicted the boundary request: [1, 2] remain.
	got, err := h.Where(nil).SinceLastUserInput().All()
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("evicted boundary must yield an empty scope, got %d events", len(got))
	}
	// Same through an explicit Replace of the transcript.
	if err := h.Replace([]events.Event{textInput("1"), textInput("2")}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got, err := h.Where(nil).SinceLastUserInput().All(); err != nil || len(got) != 0 {
		t.Fatalf("replaced transcript without boundary: %d %v", len(got), err)
	}
}

// --- generative property (testing/quick) ------------------------------------

// quickQueryOrigins enumerates every origin state a request can carry,
// including the legacy empty ones the boundary rule reads through.
var quickQueryOrigins = []events.RequestOrigin{"", events.OriginUser, events.OriginService}

var quickQueryContents = []string{"ab", "xyz", "odd", ""}

// quickQueryEvent draws one transcript event from a small vocabulary covering
// plain inputs, both request shapes in all origin states, and responses.
func quickQueryEvent(r *rand.Rand) events.Event {
	switch r.Intn(4) {
	case 0:
		return &events.TextInput{Content: quickQueryContents[r.Intn(len(quickQueryContents))]}
	case 1:
		return &events.ModelRequest{Origin: quickQueryOrigins[r.Intn(len(quickQueryOrigins))]}
	case 2:
		return &events.DrainedModelRequest{ModelRequest: events.ModelRequest{Origin: quickQueryOrigins[r.Intn(len(quickQueryOrigins))]}}
	default:
		return &events.ModelResponse{Message: &events.ModelMessage{Content: "m"}}
	}
}

// quickQueryPred is the predicate vocabulary the generator picks from:
// always-true and always-false bracket the cases, the content predicates
// discriminate on TextInput payloads, and the request predicate covers both
// request shapes.
func quickQueryPred(i int) events.Condition {
	switch i % 5 {
	case 0:
		return func(events.Event) bool { return true }
	case 1:
		return func(events.Event) bool { return false }
	case 2:
		return events.TypeOf[events.TextInput]()
	case 3:
		return func(ev events.Event) bool {
			ti, ok := events.AsValue(ev).(events.TextInput)
			return ok && len(ti.Content)%2 == 0
		}
	default:
		return func(ev events.Event) bool {
			switch events.AsValue(ev).(type) {
			case events.ModelRequest, events.DrainedModelRequest:
				return true
			}
			return false
		}
	}
}

// quickRefFilter applies cond keeping order: the reference implementation of
// "a query is the subsequence of the transcript its predicate accepts".
func quickRefFilter(evs []events.Event, cond events.Condition) []events.Event {
	out := make([]events.Event, 0, len(evs))
	for _, ev := range evs {
		if cond == nil || cond(ev) {
			out = append(out, ev)
		}
	}
	return out
}

// quickTranscriptCase generates a random transcript plus two predicate
// indices.
type quickTranscriptCase struct {
	events []events.Event
	a, b   int
}

func (quickTranscriptCase) Generate(r *rand.Rand, size int) reflect.Value {
	c := quickTranscriptCase{a: r.Intn(5), b: r.Intn(5)}
	for n := r.Intn(11); n > 0; n-- {
		c.events = append(c.events, quickQueryEvent(r))
	}
	return reflect.ValueOf(c)
}

// TestQuickQueryFilterComposition sweeps random transcripts against the
// predicate vocabulary and pins three algebraic invariants of HistoryQuery at
// once, all through the public API:
//
//  1. an unfiltered query returns exactly the appended transcript in
//     publication order;
//  2. Where(a) is exactly the subsequence the transcript's own order gives a
//     (a filter is an order-preserving subsequence, never a reordering);
//  3. combinators distribute over the filter: Where(a).Where(b) is Where(a)
//     filtered again by b, and Where(a).SinceLastUserInput() is the cut
//     transcript filtered by a — the scope runs before any filter.
func TestQuickQueryFilterComposition(t *testing.T) {
	prop := func(c quickTranscriptCase) bool {
		h := NewHistory("q", NewMemoryStorage())
		if err := h.Append(c.events...); err != nil {
			return false
		}
		all, err := h.Where(nil).All()
		if err != nil || len(all) != len(c.events) {
			return false
		}
		for i := range all {
			if !reflect.DeepEqual(all[i], c.events[i]) {
				return false
			}
		}
		a, b := quickQueryPred(c.a), quickQueryPred(c.b)
		gotA, err := h.Where(a).All()
		if err != nil || !reflect.DeepEqual(gotA, quickRefFilter(c.events, a)) {
			return false
		}
		gotAB, err := h.Where(a).Where(b).All()
		if err != nil || !reflect.DeepEqual(gotAB, quickRefFilter(quickRefFilter(c.events, a), b)) {
			return false
		}
		scoped, err := h.Where(nil).SinceLastUserInput().All()
		if err != nil {
			return false
		}
		gotAS, err := h.Where(a).SinceLastUserInput().All()
		if err != nil || !reflect.DeepEqual(gotAS, quickRefFilter(scoped, a)) {
			return false
		}
		return true
	}
	cfg := &quick.Config{MaxCount: 500, Rand: rand.New(rand.NewSource(7))}
	if err := quick.Check(prop, cfg); err != nil {
		t.Fatalf("query composition property failed: %v", err)
	}
}
