package network

import (
	"fmt"

	"github.com/luckystrker/ago/task"
)

// ErrCheckpointDenied means a task's checkpoint belongs to another writer.
// It wraps ErrAccessDenied.
var ErrCheckpointDenied = fmt.Errorf("%w: checkpoint held by another writer", ErrAccessDenied)

// taskCheckpoint is one task's stored resume snapshot and its claimed writer.
type taskCheckpoint struct {
	state  []byte
	writer string // "" = unclaimed (written without a writer identity)
}

// CheckpointRecord is the WAL form of a task checkpoint.
type CheckpointRecord struct {
	TaskID string `json:"task_id"`
	Writer string `json:"writer,omitempty"`
	State  []byte `json:"state"`
}

// CheckpointTask stores an owner-supplied resume snapshot for taskID,
// last-write-wins with no history; the payload is opaque. With a WAL
// attached the checkpoint is durable and Replay restores it; when the WAL
// refuses the append the call fails and the previous checkpoint stays.
//
// writerID claims the checkpoint: the first write with a non-empty writerID
// records it, and from then on only that writer may write or read it
// (ErrCheckpointDenied otherwise). An empty writerID is the trusted
// in-process path (Python's in-process hub call) and neither claims nor is
// checked.
//
// AGO-DEVIATION: Python's first-writer claim binds a wire connection's set of
// agents; single-host AGo binds one agent id.
func (h *Hub) CheckpointTask(writerID, taskID string, state []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	cur, exists := h.ext.checkpoints[taskID]
	if writerID != "" && cur.writer != "" && cur.writer != writerID {
		return fmt.Errorf("%w: task %s", ErrCheckpointDenied, taskID)
	}
	next := taskCheckpoint{state: cloneBytes(state), writer: cur.writer}
	if !exists || next.writer == "" {
		next.writer = writerID
	}
	if err := h.recordLocked(WALRecord{Kind: "checkpoint", Checkpoint: &CheckpointRecord{
		TaskID: taskID, Writer: next.writer, State: next.state,
	}}); err != nil {
		return fmt.Errorf("network: append checkpoint for task %s to wal: %w", taskID, err)
	}
	h.ext.checkpoints[taskID] = next
	return nil
}

// ReadTaskCheckpoint returns a copy of the last checkpoint of taskID, or
// (nil, nil) when none was written. A non-empty readerID must be the claimed
// writer (ErrCheckpointDenied otherwise); an unclaimed checkpoint and an
// empty readerID are open.
func (h *Hub) ReadTaskCheckpoint(readerID, taskID string) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cur, ok := h.ext.checkpoints[taskID]
	if !ok {
		return nil, nil
	}
	if readerID != "" && cur.writer != "" && cur.writer != readerID {
		return nil, fmt.Errorf("%w: task %s", ErrCheckpointDenied, taskID)
	}
	return cloneBytes(cur.state), nil
}

// HubBackedCheckpointStore persists task checkpoints through a Hub; it
// satisfies task.CheckpointStore, so it plugs into task.WithCheckpointStore.
// The zero writer is the trusted in-process path; NewHubBackedCheckpointStore
// with a writer id gives first-writer ownership.
type HubBackedCheckpointStore struct {
	hub    *Hub
	writer string
}

// NewHubBackedCheckpointStore binds a checkpoint store to hub. writerID ""
// is unclaimed; an agent id claims every task it checkpoints first.
func NewHubBackedCheckpointStore(hub *Hub, writerID string) *HubBackedCheckpointStore {
	return &HubBackedCheckpointStore{hub: hub, writer: writerID}
}

// Write implements task.CheckpointStore.
func (s *HubBackedCheckpointStore) Write(taskID string, state []byte) error {
	return s.hub.CheckpointTask(s.writer, taskID, state)
}

// Read implements task.CheckpointStore: (nil, nil) when absent.
func (s *HubBackedCheckpointStore) Read(taskID string) ([]byte, error) {
	return s.hub.ReadTaskCheckpoint(s.writer, taskID)
}

var _ task.CheckpointStore = (*HubBackedCheckpointStore)(nil)

// cloneBytes copies b, keeping an empty checkpoint non-nil so it reads back
// as present rather than absent.
func cloneBytes(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
