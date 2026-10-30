package knowledge

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// TestMemoryStoreVersionsUnder checks the snapshot semantics VersionsUnder must
// give the polling watcher: scoping under the prefix, a bumped version per
// write, and deletions dropping out of the snapshot.
func TestMemoryStoreVersionsUnder(t *testing.T) {
	s := NewMemoryStore()
	if err := s.Write("/log/a.jsonl", []byte("1")); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := s.Write("/log/b.jsonl", []byte("2")); err != nil {
		t.Fatalf("write b: %v", err)
	}
	if err := s.Write("/memory/working.md", []byte("state")); err != nil {
		t.Fatalf("write working: %v", err)
	}

	all, err := s.VersionsUnder("/")
	if err != nil {
		t.Fatalf("versions under /: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("root snapshot has %d paths, want 3: %v", len(all), all)
	}

	logs, err := s.VersionsUnder("/log/")
	if err != nil {
		t.Fatalf("versions under /log/: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("log snapshot has %d paths, want 2: %v", len(logs), logs)
	}
	for _, p := range []string{"/log/a.jsonl", "/log/b.jsonl"} {
		if _, ok := logs[p]; !ok {
			t.Errorf("snapshot missing %s", p)
		}
	}
	if _, ok := logs["/memory/working.md"]; ok {
		t.Errorf("log snapshot leaked /memory/working.md")
	}

	// A rewrite bumps only the rewritten path's version, and the version
	// changes as a string.
	before := logs["/log/a.jsonl"]
	if _, err := strconv.Atoi(before); err != nil {
		t.Fatalf("version %q is not numeric: %v", before, err)
	}
	if err := s.Write("/log/a.jsonl", []byte("1 rewritten")); err != nil {
		t.Fatalf("rewrite a: %v", err)
	}
	after, err := s.VersionsUnder("/log/")
	if err != nil {
		t.Fatalf("re-read versions: %v", err)
	}
	if after["/log/a.jsonl"] == before {
		t.Errorf("version did not bump on rewrite: %q", after["/log/a.jsonl"])
	}
	if after["/log/b.jsonl"] != logs["/log/b.jsonl"] {
		t.Errorf("untouched path's version changed: %q -> %q", logs["/log/b.jsonl"], after["/log/b.jsonl"])
	}

	// Deletion removes the entry, so a diff sees the path disappear.
	if err := s.Delete("/log/b.jsonl"); err != nil {
		t.Fatalf("delete b: %v", err)
	}
	after, err = s.VersionsUnder("/log/")
	if err != nil {
		t.Fatalf("versions after delete: %v", err)
	}
	if _, ok := after["/log/b.jsonl"]; ok {
		t.Errorf("deleted path still in snapshot")
	}
}

// waitFor polls ch once per interval, failing the test when the value does not
// arrive within the budget.
func waitFor(t *testing.T, ch <-chan string, budget time.Duration) string {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(budget):
		t.Fatalf("no callback within %s", budget)
		return ""
	}
}

func assertNoFire(t *testing.T, ch <-chan string, quiet time.Duration, what string) {
	t.Helper()
	select {
	case got := <-ch:
		t.Fatalf("%s fired for %q (expected silence)", what, got)
	case <-time.After(quiet):
	}
}

// TestPollingWatcherFiresOnChange checks the end-to-end diff loop: mutations
// after Start fire the callback once per changed path, pre-Start state stays
// silent, deletions fire, and a foreign prefix does not.
func TestPollingWatcherFiresOnChange(t *testing.T) {
	s := NewMemoryStore()
	// Pre-Start state: present in the initial snapshot, never fires.
	if err := s.Write("/log/old.jsonl", []byte("old")); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	fired := make(chan string, 16)
	w := NewPollingWatcher(s, "/log", func(path string) { fired <- path }, MinPollInterval)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer w.Close()

	assertNoFire(t, fired, 5*w.interval, "initial snapshot")

	if err := s.Write("/log/new.jsonl", []byte("hello")); err != nil {
		t.Fatalf("write new: %v", err)
	}
	if got := waitFor(t, fired, 2*time.Second); got != "/log/new.jsonl" {
		t.Fatalf("write fired %q, want /log/new.jsonl", got)
	}

	if err := s.Write("/log/old.jsonl", []byte("updated")); err != nil {
		t.Fatalf("rewrite old: %v", err)
	}
	if got := waitFor(t, fired, 2*time.Second); got != "/log/old.jsonl" {
		t.Fatalf("rewrite fired %q, want /log/old.jsonl", got)
	}

	if err := s.Delete("/log/new.jsonl"); err != nil {
		t.Fatalf("delete new: %v", err)
	}
	if got := waitFor(t, fired, 2*time.Second); got != "/log/new.jsonl" {
		t.Fatalf("delete fired %q, want /log/new.jsonl", got)
	}

	// Outside the watched prefix: silent.
	if err := s.Write("/memory/working.md", []byte("state")); err != nil {
		t.Fatalf("write outside prefix: %v", err)
	}
	assertNoFire(t, fired, 5*w.interval, "foreign prefix")
}

// TestPollingWatcherCloseStops checks that Close stops delivery even while the
// store keeps changing, and that Close is idempotent.
func TestPollingWatcherCloseStops(t *testing.T) {
	s := NewMemoryStore()
	fired := make(chan string, 16)
	w := NewPollingWatcher(s, "/", func(path string) { fired <- path }, MinPollInterval)
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	w.Close()
	w.Close() // idempotent

	// Drain anything in flight from before the close, then require silence.
	for {
		select {
		case <-fired:
			continue
		case <-time.After(2 * w.interval):
		}
		break
	}
	if err := s.Write("/after-close.jsonl", []byte("x")); err != nil {
		t.Fatalf("write after close: %v", err)
	}
	assertNoFire(t, fired, 5*w.interval, "watcher after Close")
}

// TestPollingWatcherContextCancel checks that cancelling Start's context stops
// the watcher (the ctx is the watcher's lifetime).
func TestPollingWatcherContextCancel(t *testing.T) {
	s := NewMemoryStore()
	fired := make(chan string, 16)
	w := NewPollingWatcher(s, "/", func(path string) { fired <- path }, MinPollInterval)
	ctx, cancel := context.WithCancel(context.Background())
	if err := w.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	cancel()
	select {
	case <-w.done:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher outlived its context")
	}
}

// TestPollingWatcherRecoversPanic checks that a panicking callback does not
// kill the watcher (Python parity: _safe_fire).
func TestPollingWatcherRecoversPanic(t *testing.T) {
	s := NewMemoryStore()
	panicThenReport := make(chan string, 1)
	w := NewPollingWatcher(s, "/", func(path string) {
		select {
		case panicThenReport <- path:
		default:
		}
		panic("bad callback")
	}, MinPollInterval)
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer w.Close()

	if err := s.Write("/boom.jsonl", []byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := waitFor(t, panicThenReport, 2*time.Second); got != "/boom.jsonl" {
		t.Fatalf("callback saw %q, want /boom.jsonl", got)
	}
	// The watcher is still alive: a second change also reaches the callback.
	if err := s.Write("/again.jsonl", []byte("y")); err != nil {
		t.Fatalf("second write: %v", err)
	}
	// Both writes may land in the buffered channel; just require the second
	// path to appear.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case got := <-panicThenReport:
			if got == "/again.jsonl" {
				return
			}
		case <-deadline:
			t.Fatal("watcher died after a panicking callback")
		}
	}
}

// TestPollingWatcherIntervalDefaults checks the Python interval rules: a
// non-positive interval falls back to 500ms, a positive one clamps to 50ms.
func TestPollingWatcherIntervalDefaults(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want time.Duration
	}{
		{in: 0, want: DefaultPollInterval},
		{in: -time.Second, want: DefaultPollInterval},
		{in: time.Millisecond, want: MinPollInterval},
		{in: MinPollInterval, want: MinPollInterval},
		{in: 250 * time.Millisecond, want: 250 * time.Millisecond},
	}
	for _, tc := range cases {
		w := NewPollingWatcher(NewMemoryStore(), "/", func(string) {}, tc.in)
		if w.interval != tc.want {
			t.Errorf("interval %s -> %s, want %s", tc.in, w.interval, tc.want)
		}
	}
}
