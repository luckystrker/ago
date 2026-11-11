// Package knowledge is the virtual path-based store for agent knowledge and the
// append-only event log built on top of it.
//
// Parity with Python AG2 ag2/knowledge/ (base/memory/disk/locked/log). Paths use
// Unix conventions ("/dir/subdir/file.txt"); directories are implicit — writing
// "/a/b/c.txt" implies "/a/" and "/a/b/". List returns immediate children with
// directory names ending in "/". SQLite and Redis backends are not ported (they
// would need external dependencies; D-011).
package knowledge

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

var (
	// ErrNotFound is returned by Read when the path has no content.
	ErrNotFound = errors.New("knowledge: path not found")
	// ErrTraversal is returned when a path would escape the store root.
	ErrTraversal = errors.New("knowledge: path traversal blocked")
)

// notFoundErr wraps ErrNotFound with the offending path.
func notFoundErr(path string) error {
	return fmt.Errorf("%w: %s", ErrNotFound, path)
}

// rangeError reports a negative offset passed to ReadRange.
type rangeError struct {
	path string
	off  int
}

func (e *rangeError) Error() string {
	return fmt.Sprintf("knowledge: negative offset %d reading %s", e.off, e.path)
}

// traversalErr wraps ErrTraversal with the offending path.
func traversalErr(path string) error {
	return fmt.Errorf("%w: %s", ErrTraversal, path)
}

// Store is the virtual filesystem shared by agents, policies and the network hub.
//
// Read returns ErrNotFound for a missing path; ReadRange returns an empty result
// for a missing file (Python parity: read → None, read_range → ""). Delete of a
// missing path is a no-op. List of a missing or empty directory returns an empty
// slice, never an error.
type Store interface {
	// Read returns the content stored at path.
	Read(path string) ([]byte, error)
	// Write replaces the content at path, creating parent directories.
	Write(path string, data []byte) error
	// List returns the sorted immediate children of dir; directories end with "/".
	List(dir string) ([]string, error)
	// Delete removes path and, for a directory, everything under it. Missing paths
	// are a no-op.
	Delete(path string) error
	// Exists reports whether path exists (as file or as an ancestor of any file).
	Exists(path string) bool
	// Append appends data to the file at path, creating it (and parents) if missing.
	Append(path string, data []byte) error
	// ReadRange returns up to n bytes of the file at path starting at byte offset
	// off. n <= 0 reads to end of file. A missing file yields (nil, nil).
	ReadRange(path string, off, n int) ([]byte, error)
	// OnChange registers fn to be called with the store path after every mutation
	// (Write, Append, Delete). The callback runs synchronously in the mutating
	// goroutine and must not call methods on the store it came from (Python parity:
	// notifications fire inside the locked write there too — a re-entrant call
	// deadlocks).
	//
	// For a per-path subscription with a closable handle, see ChangeSubscriber
	// (implemented by every store in this package) and SubscribeTo.
	OnChange(fn func(path string))
}

// normalizePath applies the Python _normalize contract: ensure a leading "/",
// collapse "//" runs, strip trailing slashes (root stays "/").
func normalizePath(path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	for strings.Contains(path, "//") {
		path = strings.ReplaceAll(path, "//", "/")
	}
	if path != "/" {
		path = strings.TrimRight(path, "/")
	}
	return path
}

// dirPrefix turns a directory path into the key prefix matching everything under
// it: "/" → "/", "/log" → "/log/".
func dirPrefix(dir string) string {
	return strings.TrimSuffix(normalizePath(dir), "/") + "/"
}

// watchers is the shared in-process change-notification list behind OnChange and
// Subscribe: one registry of path-scoped subscriptions per store.
type watchers struct {
	mu   sync.Mutex
	next uint64
	// subs is a slice, not a map: callbacks must run in registration order, so a
	// store with two watchers sees them fire in the order they subscribed.
	subs []*watchEntry
}

// watchEntry is one subscription: the normalized path it watches and the
// callback. A "/" path matches every change.
type watchEntry struct {
	id   uint64
	path string
	fn   func(path string)
}

// add registers a store-wide callback: the OnChange form.
func (w *watchers) add(fn func(path string)) {
	w.subscribe("/", fn)
}

// subscribe registers fn for changes at path or anywhere under it and returns
// the closable handle. A nil fn is ignored and yields a handle that closes
// nothing, so a caller never has to branch on it.
func (w *watchers) subscribe(path string, fn func(path string)) *ChangeSubscription {
	sub := &ChangeSubscription{}
	if fn == nil {
		return sub
	}
	w.mu.Lock()
	w.next++
	id := w.next
	w.subs = append(w.subs, &watchEntry{id: id, path: normalizePath(path), fn: fn})
	w.mu.Unlock()
	sub.ch = w
	sub.id = id
	return sub
}

// remove drops one subscription; later notifications never reach it, and the
// remaining callbacks keep their relative order.
func (w *watchers) remove(id uint64) {
	w.mu.Lock()
	for i, e := range w.subs {
		if e.id == id {
			w.subs = append(w.subs[:i:i], w.subs[i+1:]...)
			break
		}
	}
	w.mu.Unlock()
}

// notify invokes every matching watcher with changed, in registration order.
// Callbacks run in the caller's goroutine after the caller released its own
// locks, so a callback may read the store it was registered on (just not a
// LockedStore wrapping it).
func (w *watchers) notify(changed string) {
	changed = normalizePath(changed)
	w.mu.Lock()
	fns := make([]func(string), 0, len(w.subs))
	for _, e := range w.subs {
		if watchMatches(e.path, changed) {
			fns = append(fns, e.fn)
		}
	}
	w.mu.Unlock()
	for _, fn := range fns {
		fn(changed)
	}
}

// watchMatches reports whether a change at changed belongs to a subscription on
// path: the path itself or anything under it (Python parity: MemoryKnowledgeStore
// _notify's equality-or-prefix rule).
func watchMatches(path, changed string) bool {
	if path == "/" || path == changed {
		return true
	}
	return strings.HasPrefix(changed, dirPrefix(path))
}

// ChangeSubscription is the closable handle Subscribe returns (Python parity:
// knowledge.base.ChangeSubscription). Closing stops delivery and unregisters the
// callback, so a subscription does not outlive its consumer. Safe for concurrent
// use; Close is idempotent and nil-safe.
type ChangeSubscription struct {
	once   sync.Once
	closed atomic.Bool
	ch     *watchers
	id     uint64
}

// Close stops receiving change notifications.
func (s *ChangeSubscription) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.closed.Store(true)
		if s.ch != nil {
			s.ch.remove(s.id)
		}
	})
}

// active reports whether the handle still delivers.
func (s *ChangeSubscription) active() bool { return s != nil && !s.closed.Load() }

// ChangeSubscriber is implemented by stores that scope change notifications to
// one path and hand back a closable handle (Python parity: KnowledgeStore
// .on_change). It is an optional extension of Store: a store that only offers
// the store-wide OnChange still works, callers fall back to filtering it.
type ChangeSubscriber interface {
	// Subscribe notifies fn for every change at path or under it. The handle's
	// Close stops delivery. Callbacks run synchronously in the mutating goroutine
	// and must not call methods on the store they came from (the same rule as
	// OnChange).
	Subscribe(path string, fn func(path string)) *ChangeSubscription
}

// SubscribeTo is the generic per-path subscription for any Store: it uses the
// store's own Subscribe when the store implements ChangeSubscriber and
// otherwise filters the store-wide OnChange callback. Either way the returned
// handle stops delivery on Close; only the underlying registration may outlive
// it on a store that cannot unregister.
func SubscribeTo(s Store, path string, fn func(path string)) *ChangeSubscription {
	if cs, ok := s.(ChangeSubscriber); ok {
		return cs.Subscribe(path, fn)
	}
	sub := &ChangeSubscription{}
	if fn == nil {
		return sub
	}
	norm := normalizePath(path)
	s.OnChange(func(changed string) {
		if !sub.active() || !watchMatches(norm, changed) {
			return
		}
		fn(changed)
	})
	return sub
}
