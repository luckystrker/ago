package network

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Audit record kinds the hub itself emits. The set is open — tenants may
// append entries with their own kinds (Python parity: AuditLog's open kind
// set).
const (
	AuditExpectationViolated = "expectation_violated"
)

// AuditEntry is one hub-crossing event not visible on a per-channel WAL:
// expectation violations today; a JSON object per line in the file sink.
type AuditEntry struct {
	At          time.Time      `json:"at"`
	Kind        string         `json:"kind"`
	ChannelID   string         `json:"channel_id,omitempty"`
	Expectation string         `json:"expectation,omitempty"`
	Violators   []string       `json:"violators,omitempty"`
	Detail      map[string]any `json:"detail,omitempty"`
}

// AuditSink receives the hub's audit entries. Implementations must be safe
// for concurrent use and must not block on the hub's hot path for long.
type AuditSink interface {
	Append(entry AuditEntry) error
}

// FileAuditLog is a JSONL audit sink: one entry per line, appended under a
// mutex (Python parity: AuditLog over audit.jsonl).
type FileAuditLog struct {
	mu   sync.Mutex
	path string
}

// NewFileAuditLog creates a JSONL audit log at path (created on first
// append; parent directories are made as needed).
func NewFileAuditLog(path string) *FileAuditLog { return &FileAuditLog{path: path} }

// Append implements AuditSink.
func (l *FileAuditLog) Append(entry AuditEntry) error {
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if dir := filepath.Dir(l.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// SliceAuditLog is an in-memory audit sink (tests and ephemeral hubs).
type SliceAuditLog struct {
	mu      sync.Mutex
	entries []AuditEntry
}

// NewSliceAuditLog creates an empty in-memory audit sink.
func NewSliceAuditLog() *SliceAuditLog { return &SliceAuditLog{} }

// Append implements AuditSink.
func (l *SliceAuditLog) Append(entry AuditEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, entry)
	return nil
}

// Entries returns a copy of the recorded entries in append order.
func (l *SliceAuditLog) Entries() []AuditEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]AuditEntry(nil), l.entries...)
}

// auditLocked appends one audit entry through the hub sink, swallowing a
// backend failure: auditing must never break the flow it observes. The
// caller stamps At from the hub clock.
func (h *Hub) auditEntry(kind, channelID string, mutate func(*AuditEntry)) {
	if h.audit == nil {
		return
	}
	entry := AuditEntry{At: h.now(), Kind: kind, ChannelID: channelID}
	if mutate != nil {
		mutate(&entry)
	}
	_ = h.audit.Append(entry)
}
