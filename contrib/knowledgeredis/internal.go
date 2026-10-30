// Path semantics and change-notification plumbing shared by the contrib knowledge
// backends. The core package's helpers (knowledge.normalizePath, its watcher list)
// are unexported, so each optional module mirrors them here; the contracts are
// copied verbatim from knowledge/store.go so every backend behaves identically.

package knowledgeredis

import (
	"fmt"
	"strings"
	"sync"

	"github.com/luckystrker/ago/knowledge"
)

// normalizePath applies the core _normalize contract: ensure a leading "/",
// collapse "//" runs, strip trailing slashes (root stays "/"). Must stay in sync
// with knowledge.normalizePath — the two decide key identity together.
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

// notFoundErr wraps knowledge.ErrNotFound with the offending path, same shape the
// core stores use, so errors.Is(err, knowledge.ErrNotFound) works for every backend.
func notFoundErr(path string) error {
	return fmt.Errorf("%w: %s", knowledge.ErrNotFound, path)
}

// rangeError reports a negative offset passed to ReadRange; the message matches
// the core stores'.
type rangeError struct {
	path string
	off  int
}

func (e *rangeError) Error() string {
	return fmt.Sprintf("knowledge: negative offset %d reading %s", e.off, e.path)
}

// watchers is the in-process change-notification list behind OnChange (same
// contract as the core stores': callbacks run synchronously in the mutating
// goroutine).
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

// notify invokes every watcher with changed. The list is snapshotted under the
// lock and invoked outside it, so a callback may use the store it came from.
func (w *watchers) notify(changed string) {
	w.mu.Lock()
	fns := make([]func(path string), len(w.fns))
	copy(fns, w.fns)
	w.mu.Unlock()
	for _, fn := range fns {
		fn(changed)
	}
}
