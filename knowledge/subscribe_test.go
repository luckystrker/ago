package knowledge

import (
	"testing"
)

// recordChanges collects the paths a subscription delivered.
type recordChanges struct {
	paths []string
}

func (r *recordChanges) fn(path string) { r.paths = append(r.paths, path) }

// TestSubscribeFiresForPathAndDescendants pins the matching rule: the subscribed
// path itself and everything under it, nothing else (Python parity:
// MemoryKnowledgeStore._notify).
func TestSubscribeFiresForPathAndDescendants(t *testing.T) {
	s := NewMemoryStore()
	got := &recordChanges{}
	sub := s.Subscribe("/notes", got.fn)
	defer sub.Close()

	if err := s.Write("/notes/a.txt", []byte("a")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Write("/notes/deep/b.txt", []byte("b")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Write("/notesx/c.txt", []byte("c")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Write("/other.txt", []byte("d")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Delete("/notes/a.txt"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	want := []string{"/notes/a.txt", "/notes/deep/b.txt", "/notes/a.txt"}
	if len(got.paths) != len(want) {
		t.Fatalf("delivered %v, want %v", got.paths, want)
	}
	for i := range want {
		if got.paths[i] != want[i] {
			t.Fatalf("delivered %v, want %v", got.paths, want)
		}
	}
}

// TestSubscribeCloseStopsDelivery pins the handle: after Close nothing reaches
// the callback, and closing twice is harmless.
func TestSubscribeCloseStopsDelivery(t *testing.T) {
	s := NewMemoryStore()
	got := &recordChanges{}
	sub := s.Subscribe("/log", got.fn)
	if err := s.Write("/log/one", []byte("1")); err != nil {
		t.Fatalf("write: %v", err)
	}
	sub.Close()
	sub.Close()
	if err := s.Write("/log/two", []byte("2")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(got.paths) != 1 || got.paths[0] != "/log/one" {
		t.Fatalf("delivered %v, want only the pre-Close change", got.paths)
	}

	// A nil handle is safe to close, and a nil callback yields a closable handle.
	var nilSub *ChangeSubscription
	nilSub.Close()
	s.Subscribe("/x", nil).Close()
}

// TestSubscribeIsIndependentOfOnChange pins that both registrations coexist:
// a global watcher keeps seeing everything after a scoped subscription closes.
func TestSubscribeIsIndependentOfOnChange(t *testing.T) {
	s := NewMemoryStore()
	var global []string
	s.OnChange(func(path string) { global = append(global, path) })
	scoped := &recordChanges{}
	sub := s.Subscribe("/a", scoped.fn)

	if err := s.Write("/a/1", []byte("1")); err != nil {
		t.Fatalf("write: %v", err)
	}
	sub.Close()
	if err := s.Write("/b/2", []byte("2")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(global) != 2 {
		t.Fatalf("global watcher saw %v, want both writes", global)
	}
	if len(scoped.paths) != 1 {
		t.Fatalf("scoped watcher saw %v, want only the /a write", scoped.paths)
	}
}

// TestChangeCallbacksRunInRegistrationOrder pins that notifications keep the
// order the callbacks were registered in, for OnChange and Subscribe alike (a
// map-backed registry fires them in random order). Repeat the round so a
// randomized order cannot pass by luck.
func TestChangeCallbacksRunInRegistrationOrder(t *testing.T) {
	for round := 0; round < 50; round++ {
		s := NewMemoryStore()
		var order []string
		s.OnChange(func(string) { order = append(order, "global-1") })
		scoped := s.Subscribe("/x", func(string) { order = append(order, "path") })
		s.OnChange(func(string) { order = append(order, "global-2") })
		s.Subscribe("/x", func(string) { order = append(order, "path-2") })

		if err := s.Write("/x/file", []byte("v")); err != nil {
			t.Fatalf("write: %v", err)
		}
		want := []string{"global-1", "path", "global-2", "path-2"}
		if len(order) != len(want) {
			t.Fatalf("round %d: order = %v, want %v", round, order, want)
		}
		for i := range want {
			if order[i] != want[i] {
				t.Fatalf("round %d: order = %v, want %v", round, order, want)
			}
		}

		// Closing one subscription leaves the others in their relative order.
		scoped.Close()
		order = nil
		if err := s.Write("/x/file", []byte("w")); err != nil {
			t.Fatalf("write: %v", err)
		}
		if len(order) != 3 || order[0] != "global-1" || order[1] != "global-2" || order[2] != "path-2" {
			t.Fatalf("round %d: order after Close = %v", round, order)
		}
	}
}

// TestSubscribeToFallsBackToOnChange pins the generic helper: a store that only
// has the store-wide callback still gets a path-scoped, closeable subscription.
func TestSubscribeToFallsBackToOnChange(t *testing.T) {
	inner := NewMemoryStore()
	s := noSubscriberStore{inner}
	got := &recordChanges{}
	sub := SubscribeTo(s, "/only", got.fn)

	if err := s.Write("/only/here", []byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Write("/elsewhere", []byte("y")); err != nil {
		t.Fatalf("write: %v", err)
	}
	sub.Close()
	if err := s.Write("/only/after", []byte("z")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(got.paths) != 1 || got.paths[0] != "/only/here" {
		t.Fatalf("delivered %v, want only the /only change before Close", got.paths)
	}
}

// noSubscriberStore hides Subscribe so the fallback path is exercised.
type noSubscriberStore struct{ Store }

// TestLockedStoreSubscribe pins the wrapper: it delegates to a store that can
// scope notifications, so the path filter still applies through the lock.
func TestLockedStoreSubscribe(t *testing.T) {
	s := NewLockedStore(NewMemoryStore())
	got := &recordChanges{}
	sub := s.Subscribe("/locked", got.fn)
	defer sub.Close()

	if err := s.Write("/locked/file", []byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Write("/free/file", []byte("y")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(got.paths) != 1 || got.paths[0] != "/locked/file" {
		t.Fatalf("delivered %v, want only the /locked change", got.paths)
	}
}

// TestDiskStoreSubscribe pins the same behaviour on the disk backend, which
// notifies from its write path after the file is on disk.
func TestDiskStoreSubscribe(t *testing.T) {
	dir := t.TempDir()
	s := NewDiskStore(dir)
	got := &recordChanges{}
	sub := s.Subscribe("/watched", got.fn)

	if err := s.Write("/watched/a.txt", []byte("a")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Write("/unwatched/b.txt", []byte("b")); err != nil {
		t.Fatalf("write: %v", err)
	}
	sub.Close()
	if err := s.Write("/watched/c.txt", []byte("c")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(got.paths) != 1 || got.paths[0] != "/watched/a.txt" {
		t.Fatalf("delivered %v, want only the pre-Close watched write", got.paths)
	}
}

// TestSubscribeFiresForTheExactPath pins the equality half of the matching rule:
// a subscription to one file fires when that very path changes, not only for
// changes under it.
func TestSubscribeFiresForTheExactPath(t *testing.T) {
	s := NewMemoryStore()
	got := &recordChanges{}
	sub := s.Subscribe("/log/events.jsonl", got.fn)
	defer sub.Close()

	if err := s.Write("/log/events.jsonl", []byte("a")); err != nil {
		t.Fatalf("write exact: %v", err)
	}
	if err := s.Append("/log/events.jsonl", []byte("b")); err != nil {
		t.Fatalf("append exact: %v", err)
	}
	if err := s.Write("/log/other.jsonl", []byte("c")); err != nil {
		t.Fatalf("write sibling: %v", err)
	}
	want := []string{"/log/events.jsonl", "/log/events.jsonl"}
	if len(got.paths) != len(want) {
		t.Fatalf("delivered %v, want %v", got.paths, want)
	}
	for i := range want {
		if got.paths[i] != want[i] {
			t.Fatalf("delivered %v, want %v", got.paths, want)
		}
	}
}
