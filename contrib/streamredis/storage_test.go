package streamredis

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	miniredis "github.com/alicebob/miniredis/v2"

	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
)

func newTestStorage(t *testing.T, opts ...Option) (*Storage, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return NewStorage(rdb, opts...), mr
}

func sampleEvents() []events.Event {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	return []events.Event{
		&events.TextInput{Base: events.Base{CreatedAt: now}, Content: "привет мир"},
		&events.ModelRequest{
			Base:  events.Base{CreatedAt: now.Add(time.Second)},
			Parts: []events.Input{&events.TextInput{Content: "part one"}, &events.DataInput{Data: map[string]any{"k": float64(1)}}},
		},
		&events.BinaryInput{Base: events.Base{CreatedAt: now.Add(2 * time.Second)}, Data: []byte{0, 1, 255}, MediaType: "image/png", Kind: events.BinaryImage},
		&events.MessageEnqueued{Base: events.Base{CreatedAt: now.Add(3 * time.Second)}},
	}
}

func TestSaveGetRoundTrip(t *testing.T) {
	s, _ := newTestStorage(t)
	seed := sampleEvents()
	for i, ev := range seed {
		if err := s.SaveEvent("stream-1", ev); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	got, err := s.GetHistory("stream-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != len(seed) {
		t.Fatalf("history holds %d events, want %d", len(got), len(seed))
	}
	for i := range seed {
		if got[i].EventType() != seed[i].EventType() {
			t.Fatalf("event %d type = %s, want %s", i, got[i].EventType(), seed[i].EventType())
		}
		if !reflect.DeepEqual(got[i], seed[i]) {
			t.Errorf("event %d = %#v, want %#v", i, got[i], seed[i])
		}
	}
}

func TestGetHistoryUnknownStreamEmpty(t *testing.T) {
	s, _ := newTestStorage(t)
	got, err := s.GetHistory("no-such-stream")
	if err != nil {
		t.Fatalf("get unknown: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("get unknown = %v, want empty non-nil slice", got)
	}
}

func TestSetHistoryReplaces(t *testing.T) {
	s, _ := newTestStorage(t)
	for _, ev := range sampleEvents() {
		if err := s.SaveEvent("stream-2", ev); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	replacement := []events.Event{&events.TextInput{Content: "rewritten"}}
	if err := s.SetHistory("stream-2", replacement); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := s.GetHistory("stream-2")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 1 || got[0].EventType() != "TextInput" {
		t.Fatalf("history = %v, want one TextInput", got)
	}
	if got[0].(*events.TextInput).Content != "rewritten" {
		t.Errorf("content = %q, want rewritten", got[0].(*events.TextInput).Content)
	}

	// Setting an empty history clears the transcript entirely.
	if err := s.SetHistory("stream-2", nil); err != nil {
		t.Fatalf("set empty: %v", err)
	}
	if got, _ = s.GetHistory("stream-2"); len(got) != 0 {
		t.Errorf("after empty set history = %v, want empty", got)
	}
}

func TestDropHistory(t *testing.T) {
	s, _ := newTestStorage(t)
	if err := s.SaveEvent("stream-3", &events.TextInput{Content: "x"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.DropHistory("stream-3"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if got, _ := s.GetHistory("stream-3"); len(got) != 0 {
		t.Errorf("after drop history = %v, want empty", got)
	}
	// Dropping an unknown stream id is a no-op.
	if err := s.DropHistory("no-such-stream"); err != nil {
		t.Errorf("drop unknown: %v", err)
	}
}

func TestPrefixOption(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	s := NewStorage(rdb, WithPrefix("custom::"))
	if err := s.SaveEvent("sid", &events.TextInput{Content: "v"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	items, err := mr.List("custom:sid")
	if err != nil || len(items) != 1 {
		t.Errorf("list custom:sid holds %d items (err %v), want 1 (trailing colons must be trimmed)", len(items), err)
	}
}

// Events saved concurrently all land in the list (RPUSH serializes server-side).
func TestConcurrentSaveEvent(t *testing.T) {
	s, _ := newTestStorage(t)
	const workers = 16
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for r := range 5 {
				ev := &events.TextInput{Content: fmt.Sprintf("w%d-r%d", w, r)}
				if err := s.SaveEvent("shared", ev); err != nil {
					t.Errorf("save: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	got, err := s.GetHistory("shared")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != workers*5 {
		t.Errorf("history holds %d events, want %d (lost saves?)", len(got), workers*5)
	}
	seen := map[string]int{}
	for _, ev := range got {
		seen[ev.(*events.TextInput).Content]++
	}
	for w := range workers {
		for r := range 5 {
			key := fmt.Sprintf("w%d-r%d", w, r)
			if seen[key] != 1 {
				t.Errorf("event %q seen %d times, want 1", key, seen[key])
			}
		}
	}
}

// The storage drops into the core stream machinery unchanged: a stream built
// over it via LoadHistory persists through History().Replace and reads back
// through a second storage handle over the same Redis.
func TestIntegratesWithCoreStreamHistory(t *testing.T) {
	s, mr := newTestStorage(t)
	seed := sampleEvents()[:2]
	ms, err := stream.LoadHistory("resume-me", s, seed)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	h := ms.History()
	got, err := h.GetEvents()
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(got) != 2 || got[0].EventType() != "TextInput" || got[1].EventType() != "ModelRequest" {
		t.Fatalf("seeded history = %v, want [TextInput ModelRequest]", got)
	}
	// Append through the stream, then re-read through a fresh storage handle on
	// the same Redis: the transcript must round-trip beyond the process.
	if err := h.Append(&events.MessageEnqueued{}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	fresh := NewStorage(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	again, err := fresh.GetHistory("resume-me")
	if err != nil {
		t.Fatalf("fresh get: %v", err)
	}
	if len(again) != 3 || again[2].EventType() != "MessageEnqueued" {
		t.Errorf("fresh history = %v, want 3 events ending MessageEnqueued", again)
	}
}
