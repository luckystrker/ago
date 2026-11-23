package knowledge

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// joinProbe bounds the check that Close is still blocked. It outlasts any
// scheduling delay of a Close that has already decided to return, while a
// joining Close cannot return at all until the in-flight callback ends.
const joinProbe = 200 * time.Millisecond

// versionSource is a VersionSource driven by the test: it hands back whatever
// snapshot the test last stored. Unlike MemoryStore it cannot change under the
// watcher's feet, so one poll delivers exactly the intended set of changed
// paths and no write races the tick.
type versionSource struct {
	mu       sync.Mutex
	snapshot map[string]string
}

func newVersionSource() *versionSource {
	return &versionSource{snapshot: map[string]string{}}
}

func (s *versionSource) VersionsUnder(string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.snapshot))
	for path, version := range s.snapshot {
		out[path] = version
	}
	return out, nil
}

func (s *versionSource) set(snapshot map[string]string) {
	s.mu.Lock()
	s.snapshot = make(map[string]string, len(snapshot))
	for path, version := range snapshot {
		s.snapshot[path] = version
	}
	s.mu.Unlock()
}

// waitClosed blocks until Close has taken effect on w. Close does not
// acknowledge through a channel and a callback can hold the polling goroutine
// inside a dispatch batch, so the flag itself is observed rather than the
// goroutine's exit.
func waitClosed(t *testing.T, w *PollingWatcher) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !w.isClosed() {
		if time.Now().After(deadline) {
			t.Fatal("Close did not take effect within 2s")
		}
		runtime.Gosched()
	}
}

// drainStrings returns everything buffered on ch without blocking.
func drainStrings(ch <-chan string) []string {
	var out []string
	for {
		select {
		case s := <-ch:
			out = append(out, s)
		default:
			return out
		}
	}
}

// TestPollingWatcherCloseStopsBatchDispatch checks that a Close landing while
// the watcher is mid-batch suppresses the rest of that batch: the delivery
// already inside the callback completes, the remaining changed paths never
// reach it (Python parity: _safe_fire returns when the watcher is closed).
func TestPollingWatcherCloseStopsBatchDispatch(t *testing.T) {
	src := newVersionSource()
	src.set(map[string]string{
		"/log/a.jsonl": "v1",
		"/log/b.jsonl": "v1",
		"/log/c.jsonl": "v1",
	})

	fired := make(chan string, 16)
	inFirst := make(chan struct{})
	release := make(chan struct{})
	var first sync.Once
	w := NewPollingWatcher(src, "/log", func(path string) {
		select {
		case fired <- path:
		default:
		}
		first.Do(func() {
			close(inFirst)
			<-release // hold the polling goroutine inside the batch
		})
	}, MinPollInterval)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer w.Close()

	// One poll now sees all three rewrites: the next dispatch is a batch of three.
	src.set(map[string]string{
		"/log/a.jsonl": "v2",
		"/log/b.jsonl": "v2",
		"/log/c.jsonl": "v2",
	})

	select {
	case <-inFirst:
	case <-time.After(2 * time.Second):
		t.Fatal("no callback within 2s")
	}

	closedByCaller := make(chan struct{})
	go func() {
		defer close(closedByCaller)
		w.Close()
	}()
	waitClosed(t, w) // Close has taken effect while the batch is still in flight
	close(release)   // let the in-flight delivery return

	select {
	case <-closedByCaller:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return: the polling goroutine is stuck")
	}

	got := drainStrings(fired)
	if len(got) != 1 || got[0] != "/log/a.jsonl" {
		t.Fatalf("after Close mid-batch the callback saw %d deliveries %v, want 1 [/log/a.jsonl]", len(got), got)
	}
	assertNoFire(t, fired, 5*w.interval, "watcher after Close mid-batch")
}

// goroutineDump returns every goroutine's stack, so a hang can be diagnosed
// from the failure message instead of a bare timeout.
func goroutineDump() string {
	buf := make([]byte, 1<<16)
	return string(buf[:runtime.Stack(buf, true)])
}

// TestPollingWatcherCloseFromCallback checks that a callback may close its own
// watcher: Close returns instead of joining the polling goroutine it is
// currently running on, the rest of the batch stays undelivered, and the
// polling goroutine still exits as soon as the callback returns (Python
// parity: close() from a callback cancels the task it runs on and never awaits
// that same task to completion).
func TestPollingWatcherCloseFromCallback(t *testing.T) {
	src := newVersionSource()
	src.set(map[string]string{
		"/log/a.jsonl": "v1",
		"/log/b.jsonl": "v1",
		"/log/c.jsonl": "v1",
	})

	fired := make(chan string, 16)
	returned := make(chan struct{})
	var once sync.Once
	var w *PollingWatcher
	w = NewPollingWatcher(src, "/log", func(path string) {
		select {
		case fired <- path:
		default:
		}
		once.Do(func() {
			w.Close() // from the polling goroutine: must not wait for itself
			close(returned)
		})
	}, MinPollInterval)
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	// One poll now sees all three rewrites; the first delivery closes the watcher.
	src.set(map[string]string{
		"/log/a.jsonl": "v2",
		"/log/b.jsonl": "v2",
		"/log/c.jsonl": "v2",
	})

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatalf("Close from inside onChange did not return within 2s; goroutine dump:\n%s", goroutineDump())
	}

	if got := drainStrings(fired); len(got) != 1 || got[0] != "/log/a.jsonl" {
		t.Fatalf("after Close from its callback the callback saw %d deliveries %v, want 1 [/log/a.jsonl]", len(got), got)
	}
	select {
	case <-w.done:
	case <-time.After(2 * time.Second):
		t.Fatalf("polling goroutine did not exit after Close from its callback; goroutine dump:\n%s", goroutineDump())
	}
	assertNoFire(t, fired, 5*w.interval, "watcher closed from its callback")
}

// TestPollingWatcherCloseFromCallerWaitsForCallback pins the other half of the
// contract: Close from another goroutine joins the polling goroutine, so it
// cannot return while a callback is still running. Without that join a caller
// would see Close return and then a callback touching its state.
func TestPollingWatcherCloseFromCallerWaitsForCallback(t *testing.T) {
	src := newVersionSource()
	src.set(map[string]string{
		"/log/a.jsonl": "v1",
		"/log/b.jsonl": "v1",
	})

	fired := make(chan string, 16)
	inFirst := make(chan struct{})
	release := make(chan struct{})
	finish := make(chan struct{})
	var returned atomic.Bool
	var once sync.Once
	w := NewPollingWatcher(src, "/log", func(path string) {
		select {
		case fired <- path:
		default:
		}
		once.Do(func() {
			close(inFirst)
			<-release
			<-finish // the test decides when this delivery may end
			returned.Store(true)
		})
	}, MinPollInterval)
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	src.set(map[string]string{
		"/log/a.jsonl": "v2",
		"/log/b.jsonl": "v2",
	})

	select {
	case <-inFirst:
	case <-time.After(2 * time.Second):
		t.Fatal("no callback within 2s")
	}

	closedByCaller := make(chan struct{})
	go func() {
		defer close(closedByCaller)
		w.Close()
	}()
	waitClosed(t, w) // Close has taken effect and must now be joining
	close(release)   // the in-flight delivery still cannot end: finish is open

	// A Close that does not join returns here while the callback is still
	// running. The budget only bounds the proof of a negative; a joining Close
	// cannot return at all until finish opens below.
	select {
	case <-closedByCaller:
		t.Fatal("Close returned while the callback was still running: it did not join the polling goroutine")
	case <-time.After(2 * joinProbe):
	}

	close(finish)
	select {
	case <-closedByCaller:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after the callback finished")
	}
	if !returned.Load() {
		t.Fatal("Close returned before the callback set its returned flag")
	}
	if got := drainStrings(fired); len(got) != 1 || got[0] != "/log/a.jsonl" {
		t.Fatalf("after Close from another goroutine the callback saw %d deliveries %v, want 1 [/log/a.jsonl]", len(got), got)
	}
	assertNoFire(t, fired, 5*w.interval, "watcher closed from another goroutine")
}

// TestPollingWatcherCloseIdempotentSilent checks that Close is safe before
// Start and that repeated Close never panics or delivers a change made after
// it returned.
func TestPollingWatcherCloseIdempotentSilent(t *testing.T) {
	src := newVersionSource()
	src.set(map[string]string{"/log/a.jsonl": "v1"})

	fired := make(chan string, 16)
	w := NewPollingWatcher(src, "/log", func(path string) { fired <- path }, MinPollInterval)
	w.Close() // before Start: documented no-op, must not panic
	w.Close()

	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	w.Close()
	w.Close()

	src.set(map[string]string{"/log/a.jsonl": "v2"}) // change made after Close
	assertNoFire(t, fired, 5*w.interval, "closed watcher")
}
