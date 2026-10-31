package knowledge

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Store files carry full conversation transcripts, so the tree must stay
// readable only by the owning user (audit L-7/L-22).
//
// AGO-DEVIATION: Python's disk backend creates files with default umask
// permissions (group/world-readable); AGo pins owner-only modes as a
// hardening measure from the security audit.
const (
	dirPerm  fs.FileMode = 0o700
	filePerm fs.FileMode = 0o600
)

// tmpPrefix marks in-flight temporaries of atomic writes inside the store
// tree; they are removed when the write completes or fails.
const tmpPrefix = ".ago-tmp-"

// ErrDeleteRoot is returned by Delete when asked to remove the store root
// itself. Wiping the whole store is a deliberate maintenance operation, not
// something a Store call with the root path may do (audit L-22).
var ErrDeleteRoot = errors.New("knowledge: refusing to delete the store root")

// DiskStore is a Store backed by the local filesystem under one root directory.
// Virtual paths map to real files under root; directories are created on write.
//
// Safe for concurrent use: the only mutable state is the watcher list (mutex
// guarded); file operations rely on the OS. Concurrent writers to the same path
// race exactly as they would on a filesystem — wrap in LockedStore when calls
// must serialize.
//
// AGO-DEVIATION: Python's DiskKnowledgeStore declares itself macOS/Linux-only and
// guards against filesystem-illegal characters; AGo passes path bytes through on
// every platform (store paths here are framework-controlled: /memory/..., /logs/...).
// AGO-DEVIATION: OnChange observes only mutations made through this store
// instance — the stdlib has no filesystem watcher, and external dependencies are
// forbidden (D-003); consumers needing external-change detection poll (Python
// ships the same fallback via NoopChangeSubscription).
type DiskStore struct {
	root string
	ch   watchers
}

// NewDiskStore returns a store rooted at root. A relative root resolves against
// the process working directory at call time.
func NewDiskStore(root string) *DiskStore {
	return &DiskStore{root: root}
}

// resolve maps a virtual path to an absolute filesystem path under root,
// refusing paths that would escape it ("..", absolute drive prefixes, symlinked
// components pointing outside, ...).
func (d *DiskStore) resolve(path string) (string, error) {
	rel := strings.TrimPrefix(normalizePath(path), "/")
	target := d.root
	if rel != "" {
		target = filepath.Join(d.root, filepath.FromSlash(rel))
	}
	absRoot, err := filepath.Abs(d.root)
	if err != nil {
		return "", err
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	// Join already cleans, so any ".." that survived normalization collapses
	// here; the relative walk back to root catches the escape.
	if err := checkWithin(absRoot, absTarget); err != nil {
		return "", traversalErr(path)
	}
	// Lexical containment is not enough once the tree exists: a symlink stored
	// under the root can point outside it and every I/O would follow it. Resolve
	// the symlinks of the longest existing prefix and re-check containment on
	// the real location; components that do not exist yet cannot be symlinks
	// (racing their creation afterwards is the caller's TOCTOU, not ours).
	//
	// AGO-DEVIATION: Python's disk backend has no symlink resolution; AGo
	// rejects paths whose existing components resolve outside the root
	// (audit L-22).
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Root not created yet: the lexical check above already bounds the
			// path and there is nothing to resolve.
			return absTarget, nil
		}
		return "", err
	}
	real, err := resolveExisting(absTarget)
	if err != nil {
		// Unresolvable path (e.g. a symlink loop): fail closed.
		return "", err
	}
	if err := checkWithin(realRoot, real); err != nil {
		return "", traversalErr(path)
	}
	return absTarget, nil
}

// checkWithin reports an error unless target is root itself or under it.
func checkWithin(root, target string) error {
	back, err := filepath.Rel(root, target)
	if err != nil || back == ".." || strings.HasPrefix(back, ".."+string(filepath.Separator)) {
		return fmt.Errorf("target outside root")
	}
	return nil
}

// resolveExisting evaluates the symlinks of the longest existing prefix of
// path. The lexical remainder consists only of components that do not exist
// yet and therefore cannot redirect the path. When nothing exists, the path is
// returned unchanged.
func resolveExisting(path string) (string, error) {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	prefix := path
	for {
		parent := filepath.Dir(prefix)
		if parent == prefix {
			return path, nil // filesystem root reached: nothing exists to resolve
		}
		prefix = parent
		if real, err := filepath.EvalSymlinks(prefix); err == nil {
			return real, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
}

func (d *DiskStore) stat(target string) (fs.FileInfo, error) {
	info, err := os.Stat(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return info, nil
}

// Read returns the file content at path, or ErrNotFound when the path is missing
// or is a directory.
func (d *DiskStore) Read(path string) ([]byte, error) {
	target, err := d.resolve(path)
	if err != nil {
		return nil, err
	}
	info, err := d.stat(target)
	if err != nil {
		return nil, err
	}
	if info == nil || info.IsDir() {
		return nil, notFoundErr(path)
	}
	return os.ReadFile(target)
}

// Write replaces the file at path, creating parent directories, and notifies
// watchers with the virtual path. The replacement is atomic: data lands in a
// temporary file next to the target (same directory, so rename never crosses a
// filesystem) and is renamed over it — readers never see a half-written file,
// and a failed write leaves the previous content intact (audit L-22).
//
// AGO-DEVIATION: Python's disk backend writes in place; AGo writes
// temp+rename for crash-safe replacement.
func (d *DiskStore) Write(path string, data []byte) error {
	norm := normalizePath(path)
	target, err := d.resolve(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, tmpPrefix+"*")
	if err != nil {
		return err
	}
	// CreateTemp already grants 0600: transcripts stay owner-only.
	renamed := false
	defer func() {
		if !renamed {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return err
	}
	renamed = true
	d.ch.notify(norm)
	return nil
}

// List returns the sorted immediate children of dir: plain names for files,
// names ending in "/" for directories. A missing or non-directory path lists as
// empty (Python parity).
func (d *DiskStore) List(dir string) ([]string, error) {
	target, err := d.resolve(dir)
	if err != nil {
		return nil, err
	}
	info, err := d.stat(target)
	if err != nil || info == nil || !info.IsDir() {
		return []string{}, err
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name()+"/")
		} else {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// Delete removes the file (or directory tree) at path; a missing path is a
// no-op. Empty parent directories are pruned up to the root (Python parity).
// Deleting the store root ("/") is refused with ErrDeleteRoot — one careless
// call must not wipe every transcript the store holds (audit L-22).
//
// AGO-DEVIATION: Python's disk backend happily deletes its root directory;
// AGo refuses the root path.
func (d *DiskStore) Delete(path string) error {
	norm := normalizePath(path)
	if norm == "/" {
		return fmt.Errorf("%w: %s", ErrDeleteRoot, path)
	}
	target, err := d.resolve(path)
	if err != nil {
		return err
	}
	info, err := d.stat(target)
	if err != nil {
		return err
	}
	if info == nil {
		return nil
	}
	if info.IsDir() {
		if err := os.RemoveAll(target); err != nil {
			return err
		}
	} else if err := os.Remove(target); err != nil {
		return err
	}
	d.pruneEmptyParents(filepath.Dir(target))
	d.ch.notify(norm)
	return nil
}

// pruneEmptyParents removes now-empty directories from start up to (excluding)
// the store root, mirroring Python's disk backend.
func (d *DiskStore) pruneEmptyParents(start string) {
	absRoot, err := filepath.Abs(d.root)
	if err != nil {
		return
	}
	cur := start
	for {
		back, err := filepath.Rel(absRoot, cur)
		if err != nil || back == "." || back == ".." || strings.HasPrefix(back, ".."+string(filepath.Separator)) {
			return
		}
		if err := os.Remove(cur); err != nil {
			return // not empty (or not removable): stop
		}
		cur = filepath.Dir(cur)
	}
}

// Exists reports whether the path exists on disk. Paths escaping the root do
// not exist there.
func (d *DiskStore) Exists(path string) bool {
	target, err := d.resolve(path)
	if err != nil {
		return false
	}
	_, statErr := os.Stat(target)
	return statErr == nil
}

// Append appends data to the file at path (creating it and its parents), one
// O_APPEND write per call.
func (d *DiskStore) Append(path string, data []byte) error {
	norm := normalizePath(path)
	target, err := d.resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), dirPerm); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_APPEND, filePerm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	d.ch.notify(norm)
	return nil
}

// ReadRange returns up to n bytes of the file at path starting at byte offset
// off; n <= 0 reads to end of file. A missing file yields (nil, nil) (Python
// parity: read_range returns "" when the file does not exist).
func (d *DiskStore) ReadRange(path string, off, n int) ([]byte, error) {
	if off < 0 {
		return nil, &rangeError{path: path, off: off}
	}
	target, err := d.resolve(path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(int64(off), io.SeekStart); err != nil {
		return nil, err
	}
	if n > 0 {
		out := make([]byte, n)
		read, err := io.ReadFull(f, out)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return nil, err
		}
		return out[:read], nil
	}
	return io.ReadAll(f)
}

// OnChange registers a callback notified with the virtual path after every
// mutation performed through this store (external file changes are not observed).
func (d *DiskStore) OnChange(fn func(path string)) {
	d.ch.add(fn)
}
