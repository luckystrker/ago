package task

import "sync"

// CheckpointStore persists owner-defined resume state keyed by task id. The
// framework never inspects the payload — the owner picks what to checkpoint,
// when, and how to interpret it on resume. Read returns (nil, nil) when no
// checkpoint has been written for the given id.
type CheckpointStore interface {
	Write(taskID string, state []byte) error
	Read(taskID string) ([]byte, error)
}

// MemoryCheckpointStore keeps checkpoints in process memory. Safe for
// concurrent use; stored bytes are copied on both write and read so callers
// cannot alias the store's state.
type MemoryCheckpointStore struct {
	mu     sync.Mutex
	states map[string][]byte
}

// NewMemoryCheckpointStore returns an empty in-memory checkpoint store.
func NewMemoryCheckpointStore() *MemoryCheckpointStore {
	return &MemoryCheckpointStore{states: map[string][]byte{}}
}

// Write stores a copy of state under taskID, replacing any previous checkpoint.
func (m *MemoryCheckpointStore) Write(taskID string, state []byte) error {
	cp := make([]byte, len(state))
	copy(cp, state)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[taskID] = cp
	return nil
}

// Read returns a copy of the stored state, or (nil, nil) when absent.
func (m *MemoryCheckpointStore) Read(taskID string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, ok := m.states[taskID]
	if !ok {
		return nil, nil
	}
	out := make([]byte, len(stored))
	copy(out, stored)
	return out, nil
}
