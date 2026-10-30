package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// Package-level sentinels. Match with errors.Is.
var (
	// ErrTaskNotFound reports a task id absent from the server's task store;
	// the server answers JSON-RPC -32001 with it, and the client surfaces
	// that code back as this sentinel.
	ErrTaskNotFound = errors.New("a2a: task not found")
	// ErrTaskNotCancelable reports a cancel request against a task already in
	// a terminal state (JSON-RPC -32002 on the wire).
	ErrTaskNotCancelable = errors.New("a2a: task not cancelable")
	// ErrWire reports malformed A2A wire data: an unknown part kind, an
	// undecodable envelope, or a result of an unexpected shape.
	ErrWire = errors.New("a2a: malformed wire data")
	// ErrRPC reports a JSON-RPC error object from the remote side whose code
	// has no dedicated sentinel (task/cancel sentinels above).
	ErrRPC = errors.New("a2a: rpc error")
	// ErrTransport reports the HTTP exchange itself failing: unreachable
	// server, non-2xx status, truncated body.
	ErrTransport = errors.New("a2a: transport error")
	// ErrNoHandler reports a Server serving message/send with no Handle
	// configured in ServerConfig.
	ErrNoHandler = errors.New("a2a: no message handler configured")
)

// TaskStore persists Task records server-side so tasks/get and tasks/cancel
// can resolve them. Implementations must be safe for concurrent use: each
// call is atomic on its own. A read-modify-write cycle spanning a Get and a
// later Save is NOT — Get hands out a copy, Save replaces the record, so two
// overlapping cycles on one id silently lose whichever writes first. State
// transitions (a turn committing its outcome, tasks/cancel) must go through
// TaskUpdater when the store supports it; the server falls back to per-task
// locking otherwise.
type TaskStore interface {
	// Save stores task, replacing any previous record with the same ID.
	Save(ctx context.Context, task *Task) error
	// Get returns a copy of the stored task with the given ID;
	// ErrTaskNotFound when absent.
	Get(ctx context.Context, id string) (*Task, error)
	// Delete drops the record; deleting an absent id is not an error.
	Delete(ctx context.Context, id string) error
}

// TaskUpdater is the optional TaskStore extension for atomic state
// transitions. A turn's commit and a tasks/cancel are read-modify-write
// cycles whose read and write must not interleave with another writer of the
// same record (two continuations resuming one parked task, a cancel racing a
// finishing turn); a plain Get→Save pair cannot guarantee that. Stores that
// can make the whole cycle atomic under their own lock implement Update; the
// server detects them by assertion and otherwise serializes the cycle itself
// with a per-task mutex.
type TaskUpdater interface {
	// Update applies mutate to the stored record for id atomically: it
	// re-reads the current record under the store's write lock, hands a copy
	// to mutate, and replaces the stored record with what mutate returns —
	// no Save or Update on the same id can interleave between the read and
	// the write. The copy mutate receives is owned by it; editing it in
	// place and returning it is the intended use. Update returns the record
	// as now stored, by deep copy, or ErrTaskNotFound when absent; a mutate
	// error aborts the write and is returned verbatim.
	Update(ctx context.Context, id string, mutate func(stored *Task) (*Task, error)) (*Task, error)
}

// MemoryTaskStore is the in-memory TaskStore. Safe for concurrent use. Tasks
// are stored by deep copy and returned by deep copy, so callers never share
// mutable state with the store.
type MemoryTaskStore struct {
	mu    sync.RWMutex
	tasks map[string]*Task
}

var _ TaskUpdater = (*MemoryTaskStore)(nil)

// NewMemoryTaskStore builds an empty in-memory TaskStore (parity: the a2a-sdk
// InMemoryTaskStore that AG2's server defaults to).
func NewMemoryTaskStore() *MemoryTaskStore {
	return &MemoryTaskStore{tasks: make(map[string]*Task)}
}

// Save implements TaskStore.
func (s *MemoryTaskStore) Save(_ context.Context, task *Task) error {
	if task == nil || task.ID == "" {
		return fmt.Errorf("%w: task with empty id", ErrWire)
	}
	clone, err := cloneTask(task)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tasks == nil {
		s.tasks = make(map[string]*Task)
	}
	s.tasks[clone.ID] = clone
	return nil
}

// Get implements TaskStore.
func (s *MemoryTaskStore) Get(_ context.Context, id string) (*Task, error) {
	s.mu.RLock()
	stored, ok := s.tasks[id]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTaskNotFound, id)
	}
	return cloneTask(stored)
}

// Update implements TaskUpdater: the re-read, the mutate call and the write
// all happen under one Lock hold, so overlapping Updates on the same id
// serialize and every mutate sees the previous one's result.
func (s *MemoryTaskStore) Update(_ context.Context, id string, mutate func(stored *Task) (*Task, error)) (*Task, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: task with empty id", ErrWire)
	}
	if mutate == nil {
		return nil, fmt.Errorf("%w: nil mutate", ErrWire)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.tasks[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTaskNotFound, id)
	}
	current, err := cloneTask(stored)
	if err != nil {
		return nil, err
	}
	updated, err := mutate(current)
	if err != nil {
		return nil, err
	}
	if updated == nil {
		return nil, fmt.Errorf("%w: update of task %s produced no record", ErrWire, id)
	}
	clone, err := cloneTask(updated)
	if err != nil {
		return nil, err
	}
	if s.tasks == nil {
		s.tasks = make(map[string]*Task)
	}
	s.tasks[id] = clone
	// Return a fresh copy: the caller owns the result and must not share
	// mutable state with the stored record (the same invariant Get keeps).
	fresh, err := cloneTask(clone)
	if err != nil {
		return nil, err
	}
	return fresh, nil
}

// Delete implements TaskStore.
func (s *MemoryTaskStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, id)
	return nil
}

// cloneTask deep-copies a task through the wire encoding — the same shapes a
// client would see, parts decoded back into their concrete types.
func cloneTask(task *Task) (*Task, error) {
	data, err := json.Marshal(task)
	if err != nil {
		return nil, fmt.Errorf("%w: encode task: %v", ErrWire, err)
	}
	var clone Task
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil, fmt.Errorf("%w: decode task: %v", ErrWire, err)
	}
	return &clone, nil
}
