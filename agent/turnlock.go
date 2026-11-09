package agent

import "sync"

// turnLock is the per-stream mutex serializing concurrent turns. refs counts
// runs that acquired it (including ones still waiting to Lock), so the entry is
// deleted only when nobody can still be holding or waiting on it.
type turnLock struct {
	mu   sync.Mutex
	refs int
}

// turnLocks is process-wide by stream/conversation id, not per Agent: two
// Agent instances sharing one WithStreamBase publish on the same event bus, so
// they must serialize there too (upstream: the lock lives on the stream).
// Entries are reference-counted and dropped when the last turn finishes.
var turnLocks = struct {
	sync.Mutex
	m map[string]*turnLock
}{m: map[string]*turnLock{}}

// acquireTurnLock takes the lock slot for a stream: an existing entry is
// shared, a missing one is created. The refcount is taken under the registry
// mutex before the caller can block on the mutex, so a release that shrinks the
// count to zero can safely delete the entry — no waiter can be left blocking on
// a mutex the map no longer knows.
func acquireTurnLock(streamID string) *turnLock {
	turnLocks.Lock()
	defer turnLocks.Unlock()
	l, ok := turnLocks.m[streamID]
	if !ok {
		l = &turnLock{}
		turnLocks.m[streamID] = l
	}
	l.refs++
	return l
}

// releaseTurnLock unlocks the stream mutex and drops the run's slot. The entry
// is deleted only when this was the last reference: the mutex is released first,
// so a run arriving between the unlock and the map cleanup finds the same entry
// and keeps serializing instead of racing on a fresh one.
func releaseTurnLock(streamID string, l *turnLock) {
	l.mu.Unlock()
	turnLocks.Lock()
	defer turnLocks.Unlock()
	l.refs--
	if l.refs == 0 && turnLocks.m[streamID] == l {
		delete(turnLocks.m, streamID)
	}
}

// turnLockCount reports live lock entries (tests and diagnostics).
func turnLockCount() int {
	turnLocks.Lock()
	defer turnLocks.Unlock()
	return len(turnLocks.m)
}
