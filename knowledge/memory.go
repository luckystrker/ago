package knowledge

import (
	"sort"
	"strings"
	"sync"
)

// MemoryStore is the in-process Store: development default and test backend.
//
// Backed by a flat map keyed by normalized virtual path; directories are inferred
// from stored paths via prefix matching. Safe for concurrent use.
type MemoryStore struct {
	mu   sync.RWMutex
	data map[string][]byte
	ch   watchers
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{data: map[string][]byte{}}
}

// Read returns the content at path, or ErrNotFound.
func (m *MemoryStore) Read(path string) ([]byte, error) {
	m.mu.RLock()
	data, ok := m.data[normalizePath(path)]
	m.mu.RUnlock()
	if !ok {
		return nil, notFoundErr(path)
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out, nil
}

// Write stores data at path (directories are implicit) and notifies watchers.
func (m *MemoryStore) Write(path string, data []byte) error {
	norm := normalizePath(path)
	stored := make([]byte, len(data))
	copy(stored, data)
	m.mu.Lock()
	m.data[norm] = stored
	m.mu.Unlock()
	m.ch.notify(norm)
	return nil
}

// List returns the sorted immediate children of dir. Child directories carry a
// trailing "/". A directory with no files under it lists as empty.
func (m *MemoryStore) List(dir string) ([]string, error) {
	prefix := dirPrefix(dir)
	m.mu.RLock()
	children := map[string]struct{}{}
	for key := range m.data {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		remainder := key[len(prefix):]
		if remainder == "" {
			continue
		}
		if i := strings.IndexByte(remainder, '/'); i >= 0 {
			children[remainder[:i+1]] = struct{}{}
		} else {
			children[remainder] = struct{}{}
		}
	}
	m.mu.RUnlock()
	out := make([]string, 0, len(children))
	for name := range children {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// Delete removes path and everything under it, notifying once per removed path.
// Deleting a missing path is a no-op.
func (m *MemoryStore) Delete(path string) error {
	norm := normalizePath(path)
	prefix := strings.TrimSuffix(norm, "/") + "/"
	m.mu.Lock()
	var affected []string
	if _, ok := m.data[norm]; ok {
		delete(m.data, norm)
		affected = append(affected, norm)
	}
	for key := range m.data {
		if strings.HasPrefix(key, prefix) {
			delete(m.data, key)
			affected = append(affected, key)
		}
	}
	m.mu.Unlock()
	for _, changed := range affected {
		m.ch.notify(changed)
	}
	return nil
}

// Exists reports whether path is stored directly or has anything under it.
func (m *MemoryStore) Exists(path string) bool {
	norm := normalizePath(path)
	prefix := strings.TrimSuffix(norm, "/") + "/"
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.data[norm]; ok {
		return true
	}
	for key := range m.data {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// Append appends data to the file at path under the store lock (appends from
// concurrent goroutines interleave at whole-call granularity, never mid-call).
func (m *MemoryStore) Append(path string, data []byte) error {
	norm := normalizePath(path)
	m.mu.Lock()
	combined := make([]byte, 0, len(m.data[norm])+len(data))
	combined = append(combined, m.data[norm]...)
	combined = append(combined, data...)
	m.data[norm] = combined
	m.mu.Unlock()
	m.ch.notify(norm)
	return nil
}

// ReadRange returns bytes [off, off+n) of the file at path; n <= 0 reads to end
// of file. A missing file, or an offset at/after its end, yields (nil, nil)
// (Python read_range returns "" in both cases).
func (m *MemoryStore) ReadRange(path string, off, n int) ([]byte, error) {
	if off < 0 {
		return nil, &rangeError{path: path, off: off}
	}
	norm := normalizePath(path)
	m.mu.RLock()
	var out []byte
	if data, ok := m.data[norm]; ok && off < len(data) {
		end := len(data)
		if n > 0 && off+n < end {
			end = off + n
		}
		out = make([]byte, end-off)
		copy(out, data[off:end])
	}
	m.mu.RUnlock()
	return out, nil
}

// OnChange registers a change callback (Write, Append, Delete all notify).
func (m *MemoryStore) OnChange(fn func(path string)) {
	m.ch.add(fn)
}
