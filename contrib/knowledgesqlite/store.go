// Package knowledgesqlite provides a SQLite-backed knowledge.Store.
//
// Optional module (D-048/D-059): unlike the core, this module takes an external
// dependency — modernc.org/sqlite, a pure-Go SQLite driver (no cgo), so the build
// needs no C toolchain. Parity with Python AG2 ag2/knowledge/sqlite.py: every
// virtual path is a row in a single files table; prefix matching uses
// substr(path, 1, ?) = ? rather than LIKE (LIKE treats "_" and "%" as wildcards
// and folds ASCII case, while the other stores compare prefixes literally and
// case-sensitively — Python pins the same predicate).
//
// AGO-DEVIATION: the table is files(path, data, updated_at) per the AGo port
// spec, not Python's entries(path, content, version) — the version column existed
// only to feed Python's PollingChangeWatcher, and AGo's Store.OnChange contract is
// in-process callbacks (like knowledge.DiskStore), which need no versioning.
// Directories are implicit (MemoryStore semantics), so there is no dirs table.
//
// AGO-DEVIATION: Python's on_change attaches a PollingChangeWatcher (default
// 500 ms) that can observe mutations from other processes. AGo's Store interface
// prescribes in-process OnChange callbacks, so this store notifies only mutations
// made through this instance; consumers needing cross-process detection poll.
//
// Safe for concurrent use: database/sql serializes access to pooled connections,
// in-process mutations serialize on an internal mutex (Python asyncio.Lock
// parity), and cross-process/cross-instance appends serialize inside SQLite —
// the append transaction starts with BEGIN IMMEDIATE, so the prior content is
// read only after this connection holds the database's writer lock and no
// interleaved append can be lost.
package knowledgesqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" database/sql driver

	"github.com/luckystrker/ago/knowledge"
)

// Compile-time proof the store satisfies the core protocol.
var _ knowledge.Store = (*Store)(nil)

// schema keeps every virtual path in one flat table. updated_at is metadata only
// (unix seconds); identity and matching are path-only, like the other backends.
const schema = `
CREATE TABLE IF NOT EXISTS files (
    path       TEXT PRIMARY KEY,
    data       BLOB NOT NULL,
    updated_at INTEGER NOT NULL
)`

// Store is a knowledge.Store backed by a SQLite database file.
type Store struct {
	db *sql.DB
	// writeMu serializes Write/Append/Delete within this instance (Python
	// serializes them on an asyncio.Lock); cross-instance ordering is SQLite's
	// job. Held only across the database work — notifications fire after release.
	writeMu sync.Mutex
	ch      watchers
}

// NewStore opens (creating if missing) the SQLite database at dbPath and ensures
// the schema. Parent directories are created, Python _ensure_connected parity.
func NewStore(dbPath string) (*Store, error) {
	if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	// _txlock=immediate: every transaction starts with BEGIN IMMEDIATE, so the
	// append's read of the prior content happens under the writer lock (the
	// atomicity Python's _sync_append gets from an explicit BEGIN IMMEDIATE).
	// busy_timeout keeps concurrent writers (other instances, other processes)
	// waiting for SQLite's lock instead of failing with SQLITE_BUSY.
	dsn := "file:" + filepath.ToSlash(dbPath) + "?_txlock=immediate&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close releases the underlying database. Idempotent (database/sql contract).
func (s *Store) Close() error { return s.db.Close() }

// Read returns the content at path, or knowledge.ErrNotFound.
func (s *Store) Read(path string) ([]byte, error) {
	norm := normalizePath(path)
	var data []byte
	err := s.db.QueryRow(`SELECT data FROM files WHERE path = ?`, norm).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundErr(path)
	}
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out, nil
}

// Write stores data at path (directories are implicit) and notifies watchers.
func (s *Store) Write(path string, data []byte) error {
	norm := normalizePath(path)
	s.writeMu.Lock()
	_, err := s.db.Exec(`INSERT INTO files (path, data, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at`,
		norm, data, time.Now().Unix())
	s.writeMu.Unlock()
	if err != nil {
		return err
	}
	s.ch.notify(norm)
	return nil
}

// List returns the sorted immediate children of dir: plain names for files,
// names ending in "/" for directories. A directory with no files under it lists
// as empty (never an error), Python parity.
func (s *Store) List(dir string) ([]string, error) {
	prefix := dirPrefix(dir)
	rows, err := s.db.Query(`SELECT path FROM files WHERE substr(path, 1, ?) = ?`, len(prefix), prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	children := map[string]struct{}{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		remainder := p[len(prefix):]
		if remainder == "" {
			continue
		}
		if i := strings.IndexByte(remainder, '/'); i >= 0 {
			children[remainder[:i+1]] = struct{}{}
		} else {
			children[remainder] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(children))
	for name := range children {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// Delete removes path and everything under it, notifying once per removed path.
// Deleting a missing path is a no-op.
func (s *Store) Delete(path string) error {
	norm := normalizePath(path)
	prefix := strings.TrimSuffix(norm, "/") + "/"
	s.writeMu.Lock()
	affected, err := s.affectedPaths(norm, prefix)
	if err == nil && len(affected) > 0 {
		_, err = s.db.Exec(`DELETE FROM files WHERE path = ? OR substr(path, 1, ?) = ?`,
			norm, len(prefix), prefix)
	}
	s.writeMu.Unlock()
	if err != nil {
		return err
	}
	sort.Strings(affected)
	for _, changed := range affected {
		s.ch.notify(changed)
	}
	return nil
}

// affectedPaths lists the exact path plus everything under it, inside the caller's
// writeMu scope (Delete consumes this together with the matching DELETE).
func (s *Store) affectedPaths(norm, prefix string) ([]string, error) {
	rows, err := s.db.Query(`SELECT path FROM files WHERE path = ? OR substr(path, 1, ?) = ?`,
		norm, len(prefix), prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Exists reports whether path is stored directly or has anything under it.
func (s *Store) Exists(path string) bool {
	norm := normalizePath(path)
	prefix := strings.TrimSuffix(norm, "/") + "/"
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM files WHERE path = ? LIMIT 1`, norm).Scan(&one)
	if err == nil {
		return true
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false
	}
	err = s.db.QueryRow(`SELECT 1 FROM files WHERE substr(path, 1, ?) = ? LIMIT 1`,
		len(prefix), prefix).Scan(&one)
	return err == nil
}

// Append appends data to the file at path, creating it (and its parent directories,
// implicitly) if missing. The read-modify-write runs in one BEGIN IMMEDIATE
// transaction, so appends from concurrent goroutines, other store instances and
// other processes interleave at whole-call granularity and none is lost.
func (s *Store) Append(path string, data []byte) error {
	norm := normalizePath(path)
	s.writeMu.Lock()
	err := s.appendLocked(norm, data)
	s.writeMu.Unlock()
	if err != nil {
		return err
	}
	s.ch.notify(norm)
	return nil
}

func (s *Store) appendLocked(norm string, data []byte) error {
	// An empty append still materializes the key (Python parity: append of ""
	// makes the path exist).
	tx, err := s.db.BeginTx(context.Background(), nil) // BEGIN IMMEDIATE via _txlock
	if err != nil {
		return err
	}
	var existing []byte
	err = tx.QueryRow(`SELECT data FROM files WHERE path = ?`, norm).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		tx.Rollback()
		return err
	}
	combined := make([]byte, 0, len(existing)+len(data))
	combined = append(combined, existing...)
	combined = append(combined, data...)
	if _, err := tx.Exec(`INSERT INTO files (path, data, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at`,
		norm, combined, time.Now().Unix()); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ReadRange returns up to n bytes of the file at path starting at byte offset
// off; n <= 0 reads to end of file. A missing file, or an offset at/after its
// end, yields (nil, nil) (Python read_range returns "" in both cases).
func (s *Store) ReadRange(path string, off, n int) ([]byte, error) {
	if off < 0 {
		return nil, &rangeError{path: path, off: off}
	}
	norm := normalizePath(path)
	var data []byte
	err := s.db.QueryRow(`SELECT data FROM files WHERE path = ?`, norm).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if off >= len(data) {
		return nil, nil
	}
	end := len(data)
	if n > 0 && off+n < end {
		end = off + n
	}
	out := make([]byte, end-off)
	copy(out, data[off:end])
	return out, nil
}

// OnChange registers a change callback (Write, Append, Delete all notify) with
// the virtual path. In-process only: mutations made through other store
// instances or processes are not observed.
func (s *Store) OnChange(fn func(path string)) {
	s.ch.add(fn)
}
