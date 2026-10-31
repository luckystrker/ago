// Package streamredis provides a Redis-backed stream.Storage: the durable
// transcript of one or more conversation streams.
//
// Optional module (D-048/D-059): unlike the core, this module takes an external
// dependency — github.com/redis/go-redis/v9. Parity with Python AG2
// ag2/streams/redis/storage.py: one stream id maps to one Redis key
// ("{prefix}:{stream_id}", prefix default "ag2:stream"); the transcript is a
// Redis list of serialized events — SaveEvent RPUSHes, GetHistory reads the
// whole list, SetHistory rewrites it inside one transactional pipeline
// (DEL + RPUSH...), DropHistory deletes the key.
//
// Events are serialized with events.MarshalEvent / UnmarshalEvent (the "@type"
// registry round trip). AGO-DEVIATION: Python also offers a pickle serializer
// preserving exact Python types; there is no Go counterpart — the Go wire format
// is the "@type"-discriminated JSON only (and Python's pickle is unsafe against
// anyone with write access to the Redis, which its own docs warn about).
//
// The client is injected (NewStorage), never constructed here: the caller owns
// connection pooling and lifecycle. Safe for concurrent use; concurrent
// SaveEvent calls on one stream keep every event, in arrival order.
package streamredis

import (
	"context"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
)

// Compile-time proof the storage satisfies the core protocol.
var _ stream.Storage = (*Storage)(nil)

// DefaultPrefix is the Redis key prefix (Python RedisStorage default).
const DefaultPrefix = "ag2:stream"

// DefaultTimeout bounds every Redis call. A deadline here is the storage's own
// guarantee: the injected client may come with client-level timeouts disabled
// (a common setup for blocking commands), and without a per-call deadline one
// hung Redis would block the caller forever. Caveat inherited from go-redis
// v9: the deadline reaches the socket only when the injected client has
// ContextTimeoutEnabled set (or its own read/write timeouts, the default 3s);
// a client with both disabled ignores any context deadline.
const DefaultTimeout = 30 * time.Second

// Storage persists stream transcripts in Redis.
type Storage struct {
	rdb    redis.UniversalClient
	prefix string
	// The core Storage protocol methods carry no context (AGO-DEVIATION,
	// interface parity constraint: Python's protocol is likewise context-free),
	// so every Redis call runs under a fresh context derived from base with the
	// per-call deadline timeout.
	base    context.Context
	timeout time.Duration
}

// Option configures a Storage.
type Option func(*Storage)

// WithPrefix overrides the Redis key prefix (default DefaultPrefix).
func WithPrefix(prefix string) Option {
	return func(s *Storage) { s.prefix = prefix }
}

// WithTimeout overrides the per-call Redis deadline (default DefaultTimeout).
// A zero or negative duration restores the default.
func WithTimeout(d time.Duration) Option {
	return func(s *Storage) {
		if d > 0 {
			s.timeout = d
		}
	}
}

// NewStorage builds a storage over an injected client. The caller keeps
// ownership: closing the client is the caller's job.
func NewStorage(rdb redis.UniversalClient, opts ...Option) *Storage {
	s := &Storage{
		rdb:     rdb,
		prefix:  DefaultPrefix,
		base:    context.Background(),
		timeout: DefaultTimeout,
	}
	for _, opt := range opts {
		opt(s)
	}
	s.prefix = strings.TrimRight(s.prefix, ":")
	return s
}

// callCtx returns the context for one Redis call (or one transactional
// pipeline): base with the deadline attached. Each call gets its own cancel so
// timers are never leaked across calls.
func (s *Storage) callCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(s.base, s.timeout)
}

// key maps a stream id to its Redis list key: "{prefix}:{stream_id}".
func (s *Storage) key(streamID string) string { return s.prefix + ":" + streamID }

// SaveEvent appends one serialized event to the stream's list.
func (s *Storage) SaveEvent(streamID string, ev events.Event) error {
	data, err := events.MarshalEvent(ev)
	if err != nil {
		return err
	}
	ctx, cancel := s.callCtx()
	defer cancel()
	return s.rdb.RPush(ctx, s.key(streamID), data).Err()
}

// GetHistory returns the whole transcript; an unknown stream id yields an empty
// slice (LRANGE over a missing key is empty, memory-storage parity).
func (s *Storage) GetHistory(streamID string) ([]events.Event, error) {
	ctx, cancel := s.callCtx()
	defer cancel()
	raw, err := s.rdb.LRange(ctx, s.key(streamID), 0, -1).Result()
	if err != nil {
		return nil, err
	}
	out := make([]events.Event, 0, len(raw))
	for _, item := range raw {
		ev, err := events.UnmarshalEvent([]byte(item))
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, nil
}

// SetHistory atomically replaces the transcript: events are marshaled before any
// Redis command is issued, then DEL + RPUSH... run in one transactional pipeline,
// so a reader never observes a half-written list. The whole pipeline shares one
// deadline: it is a single replacement from the caller's perspective.
func (s *Storage) SetHistory(streamID string, evs []events.Event) error {
	payload := make([][]byte, len(evs))
	for i, ev := range evs {
		data, err := events.MarshalEvent(ev)
		if err != nil {
			return err
		}
		payload[i] = data
	}
	ctx, cancel := s.callCtx()
	defer cancel()
	key := s.key(streamID)
	pipe := s.rdb.TxPipeline()
	pipe.Del(ctx, key)
	for _, data := range payload {
		pipe.RPush(ctx, key, data)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// DropHistory removes the transcript; dropping an unknown stream id is a no-op.
func (s *Storage) DropHistory(streamID string) error {
	ctx, cancel := s.callCtx()
	defer cancel()
	return s.rdb.Del(ctx, s.key(streamID)).Err()
}
