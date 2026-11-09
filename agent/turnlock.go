package agent

import (
	"context"
	"sync"
)

// turnLock is the per-stream semaphore serializing concurrent turns. refs
// counts runs that acquired a slot (including one still waiting), so the entry
// is deleted only when nobody can still be holding or waiting on it.
//
// The semaphore is channel-based rather than sync.Mutex so a waiting turn can
// honor its context: upstream wraps the per-stream lock in the turn scope, and
// cancellation while queued must not strand the caller.
type turnLock struct {
	sem  chan struct{} // buffered 1; a token means the stream is owned
	refs int
}

func newTurnLock() *turnLock { return &turnLock{sem: make(chan struct{}, 1)} }

// acquire waits for the stream slot or the context deadline.
func (l *turnLock) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case l.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// release returns the stream slot.
func (l *turnLock) release() { <-l.sem }

// turnLocks is process-wide by stream/conversation id, not per Agent: two
// Agent instances sharing one WithStreamBase publish on the same event bus, so
// they must serialize there too (upstream: the lock lives on the stream).
var turnLocks = struct {
	sync.Mutex
	m map[string]*turnLock
}{m: map[string]*turnLock{}}

// turnLockFor takes a reference on the entry for a stream, creating it if
// needed. The refcount is taken under the registry mutex before the caller can
// block on the semaphore, so a release that shrinks the count to zero can
// safely delete the entry.
func turnLockFor(streamID string) *turnLock {
	turnLocks.Lock()
	defer turnLocks.Unlock()
	l, ok := turnLocks.m[streamID]
	if !ok {
		l = newTurnLock()
		turnLocks.m[streamID] = l
	}
	l.refs++
	return l
}

// acquireTurnLock takes the slot for a stream, honoring ctx while queued. A
// canceled wait drops its reference: the entry must not leak a run that never
// entered the turn scope.
func acquireTurnLock(ctx context.Context, streamID string) (*turnLock, error) {
	l := turnLockFor(streamID)
	if err := l.acquire(ctx); err != nil {
		dropTurnLockRef(streamID, l)
		return nil, err
	}
	return l, nil
}

// releaseTurnLock returns the stream slot and drops the run's reference. The
// slot is released first, so a run arriving between the release and the map
// cleanup finds the same entry and keeps serializing instead of racing on a
// fresh one.
func releaseTurnLock(streamID string, l *turnLock) {
	l.release()
	dropTurnLockRef(streamID, l)
}

// dropTurnLockRef decrements the reference without touching the semaphore. It
// is used after a canceled acquire (the slot was never taken) and by release.
func dropTurnLockRef(streamID string, l *turnLock) {
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
