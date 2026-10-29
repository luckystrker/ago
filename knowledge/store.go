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

// watchers is the shared in-process change-notification list behind OnChange.
type watchers struct {
	mu  sync.Mutex
	fns []func(path string)
}

func (w *watchers) add(fn func(path string)) {
	if fn == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.fns = append(w.fns, fn)
}

// notify invokes every watcher with changed. Callbacks run in the caller's
// goroutine after the caller released its own locks, so a callback may read the
// store it was registered on (just not a LockedStore wrapping it).
func (w *watchers) notify(changed string) {
	w.mu.Lock()
	fns := make([]func(path string), len(w.fns))
	copy(fns, w.fns)
	w.mu.Unlock()
	for _, fn := range fns {
		fn(changed)
	}
}
