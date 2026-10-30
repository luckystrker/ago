package knowledge

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Polling defaults (Python parity: polling.py — interval_s=0.5, floor 0.05).
const (
	// DefaultPollInterval is used when NewPollingWatcher gets a non-positive
	// interval.
	DefaultPollInterval = 500 * time.Millisecond
	// MinPollInterval is the shortest interval the watcher accepts.
	MinPollInterval = 50 * time.Millisecond
)

// VersionSource is the backend capability PollingWatcher diffs: a snapshot of
// {path: version} for every key under prefix. Versions are opaque comparable
// strings — an mtime, a monotonic counter, anything whose inequality marks a
// change; a path missing from the snapshot counts as deleted (Python parity:
// list_versions_under in polling.py/base.py).
type VersionSource interface {
	VersionsUnder(prefix string) (map[string]string, error)
}

// ChangeFunc receives one changed path. It runs on the watcher's polling
// goroutine: blocking it delays further polls, and a panic is recovered so a
// bad callback never kills the watcher (Python parity: _safe_fire).
type ChangeFunc func(path string)

// PollingWatcher polls a VersionSource for changes under one path prefix and
// dispatches each changed path to a callback. It backs stores that cannot
// observe changes natively. Semantics (Python parity: PollingChangeWatcher):
//
//   - Start takes the initial snapshot, so mutations before Start never fire;
//   - each tick diffs the current snapshot against the last-seen one and fires
//     once per changed path — new, rewritten, and removed paths all fire;
//   - a failed snapshot read skips the tick; a panicking callback is dropped;
//   - Close stops delivery; cancelling Start's context does the same.
type PollingWatcher struct {
	src      VersionSource
	prefix   string
	onChange ChangeFunc
	interval time.Duration

	mu      sync.Mutex
	last    map[string]string
	started bool
	closed  bool
	cancel  context.CancelFunc
	done    chan struct{}
}

// NewPollingWatcher returns a watcher firing onChange for every path changed
// under prefix. A non-positive interval means DefaultPollInterval; shorter
// intervals clamp to MinPollInterval.
func NewPollingWatcher(src VersionSource, prefix string, onChange ChangeFunc, interval time.Duration) *PollingWatcher {
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	if interval < MinPollInterval {
		interval = MinPollInterval
	}
	return &PollingWatcher{
		src:      src,
		prefix:   normalizePath(prefix),
		onChange: onChange,
		interval: interval,
	}
}

// Start takes the initial snapshot and launches the polling goroutine. It is
// idempotent: later calls return nil without effect. Cancelling ctx stops the
// watcher, as does Close.
func (w *PollingWatcher) Start(ctx context.Context) error {
	w.mu.Lock()
	if w.started {
		w.mu.Unlock()
		return nil
	}
	snapshot, err := w.src.VersionsUnder(w.prefix)
	if err != nil {
		w.mu.Unlock()
		return err
	}
	w.last = snapshot
	w.started = true
	pctx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	w.done = make(chan struct{})
	w.mu.Unlock()

	go w.loop(pctx)
	return nil
}

// loop polls until ctx is cancelled or Close wins. The watcher lives as long
// as the caller's scope: cancelling Start's context stops polling, as does
// Close.
func (w *PollingWatcher) loop(ctx context.Context) {
	defer close(w.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.interval):
		}
		if w.isClosed() {
			return
		}
		current, err := w.src.VersionsUnder(w.prefix)
		if err != nil {
			continue // a failed poll is skipped, not fatal (Python parity)
		}
		for _, path := range sortedPaths(current) {
			if prior, ok := w.snapshot()[path]; !ok || prior != current[path] {
				w.safeFire(path)
			}
		}
		for _, path := range sortedPaths(w.snapshot()) {
			if _, ok := current[path]; !ok {
				w.safeFire(path) // removed paths fire too (Python parity)
			}
		}
		w.setSnapshot(current)
	}
}

// Close stops the watcher and waits for the polling goroutine to exit.
// Idempotent; safe before Start (no-op).
func (w *PollingWatcher) Close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	cancel, done := w.cancel, w.done
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (w *PollingWatcher) isClosed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed
}

func (w *PollingWatcher) snapshot() map[string]string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.last
}

func (w *PollingWatcher) setSnapshot(s map[string]string) {
	w.mu.Lock()
	w.last = s
	w.mu.Unlock()
}

// safeFire invokes onChange, recovering a panic so callbacks never crash the
// watcher (Python parity: _safe_fire swallows callback exceptions).
func (w *PollingWatcher) safeFire(path string) {
	defer func() { _ = recover() }()
	w.onChange(path)
}

// sortedPaths returns the snapshot's paths in stable order, so callbacks fire
// deterministically (Python dict order is insertion order; Go map order is not).
func sortedPaths(snapshot map[string]string) []string {
	out := make([]string, 0, len(snapshot))
	for p := range snapshot {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
