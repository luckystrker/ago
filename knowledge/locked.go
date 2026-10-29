package knowledge

import "sync"

// LockedStore serializes access to another Store behind one read/write mutex:
// mutations (Write, Delete, Append) take the write lock, reads (Read, List,
// Exists, ReadRange) and OnChange take the read lock and may proceed in
// parallel. Safe for concurrent use.
//
// AGO-DEVIATION: Python locks writes per path with named async locks (30s TTL);
// one RWMutex is the Go equivalent without the lock-manager machinery, and it
// also closes the read-modify-write window (e.g. append-after-read) that
// per-path locks leave open.
type LockedStore struct {
	inner Store
	mu    sync.RWMutex
}

// NewLockedStore wraps inner for concurrent use.
func NewLockedStore(inner Store) *LockedStore {
	return &LockedStore{inner: inner}
}

// Read reads through the inner store under the read lock.
func (l *LockedStore) Read(path string) ([]byte, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.inner.Read(path)
}

// Write writes through the inner store under the write lock.
func (l *LockedStore) Write(path string, data []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.Write(path, data)
}

// List lists through the inner store under the read lock.
func (l *LockedStore) List(dir string) ([]string, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.inner.List(dir)
}

// Delete deletes through the inner store under the write lock.
func (l *LockedStore) Delete(path string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.Delete(path)
}

// Exists checks the inner store under the read lock.
func (l *LockedStore) Exists(path string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.inner.Exists(path)
}

// Append appends through the inner store under the write lock, so concurrent
// appends to one path interleave at whole-call granularity.
func (l *LockedStore) Append(path string, data []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.Append(path, data)
}

// ReadRange reads a range through the inner store under the read lock.
func (l *LockedStore) ReadRange(path string, off, n int) ([]byte, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.inner.ReadRange(path, off, n)
}

// OnChange registers the callback on the inner store. The callback runs while
// the write lock of this wrapper is held (notifications fire inside the inner
// mutation call) and must not call methods on this LockedStore — that would
// deadlock; it may call the inner store directly.
func (l *LockedStore) OnChange(fn func(path string)) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	l.inner.OnChange(fn)
}
