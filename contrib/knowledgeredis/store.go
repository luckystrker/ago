// Package knowledgeredis provides a Redis-backed knowledge.Store.
//
// Optional module (D-048/D-059): unlike the core, this module takes an external
// dependency — github.com/redis/go-redis/v9. Parity with Python AG2
// ag2/knowledge/redis.py: each virtual path becomes a Redis string under a
// configurable key prefix (default "ag2:knowledge"); a companion sorted set at
// "{prefix}__index" keeps a (path, version) ranking so List, Exists and Delete
// work from one index snapshot instead of key-space scans.
//
// Append runs as one server-side Lua script over the content, index and version
// counter keys, so it is atomic across connections — independent clients cannot
// interleave appends and lose entries (the exact scenario the Python
// test_redis_append.py suite pins). The keys do not share a cluster slot, so —
// like Python — append requires a single Redis node.
//
// AGO-DEVIATION: Python's on_change attaches a PollingChangeWatcher (default
// 500 ms) and exposes list_versions_under for it. AGo's Store interface
// prescribes in-process OnChange callbacks (like knowledge.DiskStore), and
// version snapshots are not part of the AGo protocol surface, so neither is
// ported; consumers needing cross-process detection poll.
//
// The client is injected (NewStore), never constructed here: the caller owns
// connection pooling and lifecycle. Safe for concurrent use.
package knowledgeredis

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/luckystrker/ago/knowledge"
)

// Compile-time proof the store satisfies the core protocol.
var _ knowledge.Store = (*Store)(nil)

// DefaultPrefix is the Redis key prefix (Python RedisKnowledgeStore default).
const DefaultPrefix = "ag2:knowledge"

// DefaultTimeout bounds every Redis call (a multi-command mutation — Write,
// Delete — shares one deadline for the whole mutation). A deadline here is the
// store's own guarantee: the injected client may come with client-level
// timeouts disabled (a common setup for blocking commands), and without a
// per-call deadline one hung Redis would block the caller forever. Caveat
// inherited from go-redis v9: the deadline reaches the socket only when the
// injected client has ContextTimeoutEnabled set (or its own read/write
// timeouts, the default 3s); a client with both disabled ignores any context
// deadline.
const DefaultTimeout = 30 * time.Second

// appendScript appends the payload and bumps the version/index in one server-side
// step, so independent connections cannot interleave (Python _APPEND_SCRIPT).
// KEYS: content, index, version counter. ARGV: payload, path. Returns the content
// size after the append.
const appendScript = `
local size = redis.call('APPEND', KEYS[1], ARGV[1])
local version = redis.call('INCR', KEYS[3])
redis.call('ZADD', KEYS[2], version, ARGV[2])
return size
`

// Store is a knowledge.Store backed by Redis.
type Store struct {
	rdb        redis.UniversalClient
	keyPrefix  string
	indexKey   string
	versionKey string
	append     *redis.Script
	// writeMu serializes multi-command mutations within this instance (Python
	// asyncio.Lock parity); the Lua-script append needs no such lock.
	writeMu sync.Mutex
	ch      watchers
	// The core Store protocol methods carry no context (AGO-DEVIATION,
	// interface parity constraint: Python's protocol is likewise context-free),
	// so every Redis call runs under a fresh context derived from base with the
	// per-call deadline timeout.
	base    context.Context
	timeout time.Duration
}

// Option configures a Store.
type Option func(*Store)

// WithPrefix overrides the Redis key prefix (default DefaultPrefix).
func WithPrefix(prefix string) Option {
	return func(s *Store) { s.keyPrefix = prefix }
}

// WithTimeout overrides the per-call Redis deadline (default DefaultTimeout).
// A zero or negative duration restores the default.
func WithTimeout(d time.Duration) Option {
	return func(s *Store) {
		if d > 0 {
			s.timeout = d
		}
	}
}

// NewStore builds a store over an injected client. The caller keeps ownership:
// closing the client (and flushing its keys) is the caller's job.
func NewStore(rdb redis.UniversalClient, opts ...Option) *Store {
	s := &Store{
		rdb:       rdb,
		keyPrefix: DefaultPrefix,
		base:      context.Background(),
		timeout:   DefaultTimeout,
	}
	for _, opt := range opts {
		opt(s)
	}
	s.keyPrefix = strings.TrimRight(s.keyPrefix, ":") // Python rstrip(":") parity
	s.indexKey = s.keyPrefix + ":__index"
	s.versionKey = s.keyPrefix + ":__version_counter"
	s.append = redis.NewScript(appendScript)
	return s
}

// callCtx returns the context for one Redis call (or one multi-command mutation
// like Write/Delete): base with the deadline attached. Each call gets its own
// cancel so timers are never leaked across calls.
func (s *Store) callCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(s.base, s.timeout)
}

// key maps a virtual path to its Redis string key: "{prefix}:{path}".
func (s *Store) key(path string) string { return s.keyPrefix + ":" + path }

// Read returns the content at path, or knowledge.ErrNotFound.
func (s *Store) Read(path string) ([]byte, error) {
	norm := normalizePath(path)
	ctx, cancel := s.callCtx()
	defer cancel()
	data, err := s.rdb.Get(ctx, s.key(norm)).Bytes()
	if err == redis.Nil {
		return nil, notFoundErr(path)
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Write stores data at path (directories are implicit) and notifies watchers.
// Version bump, content write and index update are three commands like Python's
// write, serialized here by the instance write lock and sharing one deadline
// (the mutation is atomic from the caller's perspective).
func (s *Store) Write(path string, data []byte) error {
	norm := normalizePath(path)
	ctx, cancel := s.callCtx()
	defer cancel()
	s.writeMu.Lock()
	version, err := s.rdb.Incr(ctx, s.versionKey).Result()
	if err == nil {
		err = s.rdb.Set(ctx, s.key(norm), data, 0).Err()
	}
	if err == nil {
		err = s.rdb.ZAdd(ctx, s.indexKey, redis.Z{Score: float64(version), Member: norm}).Err()
	}
	s.writeMu.Unlock()
	if err != nil {
		return err
	}
	s.ch.notify(norm)
	return nil
}

// indexSnapshot returns the (path → version) pairs of the whole index. The
// caller supplies the mutation-scoped context so a multi-command mutation reads
// the index under the same deadline it writes under.
func (s *Store) indexSnapshot(ctx context.Context) (map[string]int64, error) {
	zs, err := s.rdb.ZRangeWithScores(ctx, s.indexKey, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(zs))
	for _, z := range zs {
		out[z.Member.(string)] = int64(z.Score)
	}
	return out, nil
}

// List returns the sorted immediate children of dir: plain names for files,
// names ending in "/" for directories. A directory with no files under it lists
// as empty (never an error), Python parity.
func (s *Store) List(dir string) ([]string, error) {
	prefix := dirPrefix(dir)
	ctx, cancel := s.callCtx()
	defer cancel()
	snapshot, err := s.indexSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	children := map[string]struct{}{}
	for p := range snapshot {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		remainder := p[len(prefix):]
		if i := strings.IndexByte(remainder, '/'); i >= 0 {
			children[remainder[:i+1]] = struct{}{}
		} else {
			children[remainder] = struct{}{}
		}
	}
	out := make([]string, 0, len(children))
	for name := range children {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// Delete removes path and everything under it, notifying once per removed path.
// Deleting a missing path is a no-op. The snapshot read and the key/index
// removals share one deadline: one mutation from the caller's perspective.
func (s *Store) Delete(path string) error {
	norm := normalizePath(path)
	prefix := strings.TrimSuffix(norm, "/") + "/"
	var affected []string
	ctx, cancel := s.callCtx()
	defer cancel()
	s.writeMu.Lock()
	snapshot, err := s.indexSnapshot(ctx)
	if err == nil {
		for p := range snapshot {
			if p == norm || strings.HasPrefix(p, prefix) {
				affected = append(affected, p)
			}
		}
		sort.Strings(affected)
		if len(affected) > 0 {
			keys := make([]string, len(affected))
			for i, p := range affected {
				keys[i] = s.key(p)
			}
			if err = s.rdb.Del(ctx, keys...).Err(); err == nil {
				err = s.rdb.ZRem(ctx, s.indexKey, s.toMembers(affected)).Err()
			}
		}
	}
	s.writeMu.Unlock()
	if err != nil {
		return err
	}
	for _, changed := range affected {
		s.ch.notify(changed)
	}
	return nil
}

func (s *Store) toMembers(paths []string) []interface{} {
	out := make([]interface{}, len(paths))
	for i, p := range paths {
		out[i] = p
	}
	return out
}

// Exists reports whether path is stored directly or has anything under it.
func (s *Store) Exists(path string) bool {
	norm := normalizePath(path)
	ctx, cancel := s.callCtx()
	defer cancel()
	n, err := s.rdb.Exists(ctx, s.key(norm)).Result()
	if err == nil && n > 0 {
		return true
	}
	prefix := strings.TrimSuffix(norm, "/") + "/"
	snapshot, err := s.indexSnapshot(ctx)
	if err != nil {
		return false
	}
	for p := range snapshot {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// Append appends data to the file at path, creating it if missing. The
// read-modify-write is one server-side Lua script (Python _APPEND_SCRIPT
// parity), so appends from independent clients on one Redis node cannot
// interleave and lose entries.
func (s *Store) Append(path string, data []byte) error {
	norm := normalizePath(path)
	// The offset Python returns (size after minus payload) is not part of the
	// AGo Store protocol — the script's atomicity is what matters here.
	ctx, cancel := s.callCtx()
	defer cancel()
	_, err := s.append.Run(ctx, s.rdb, []string{s.key(norm), s.indexKey, s.versionKey}, data, norm).Result()
	if err != nil {
		return err
	}
	s.ch.notify(norm)
	return nil
}

// ReadRange returns up to n bytes of the file at path starting at byte offset
// off; n <= 0 reads to end of file. A missing file, or an offset at/after its
// end, yields (nil, nil) (Python read_range returns "" in both cases).
func (s *Store) ReadRange(path string, off, n int) ([]byte, error) {
	if off < 0 {
		return nil, &rangeError{path: path, off: off}
	}
	norm := normalizePath(path)
	ctx, cancel := s.callCtx()
	defer cancel()
	data, err := s.rdb.Get(ctx, s.key(norm)).Bytes()
	if err == redis.Nil {
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
// the virtual path. In-process only: mutations made by other processes or store
// instances are not observed.
func (s *Store) OnChange(fn func(path string)) {
	s.ch.add(fn)
}
