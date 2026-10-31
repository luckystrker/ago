package knowledgeredis

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	miniredis "github.com/alicebob/miniredis/v2"

	"github.com/luckystrker/ago/knowledge"
)

func newTestStore(t *testing.T, opts ...Option) (*Store, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return NewStore(rdb, opts...), mr
}

func TestWriteReadRoundTrip(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.Write("/memory/working.md", []byte("# state\nline two")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := s.Read("/memory/working.md")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "# state\nline two" {
		t.Errorf("read = %q, want %q", got, "# state\nline two")
	}

	// Normalization: equivalent spellings reach the same key.
	if err := s.Write("notes/todo.md", []byte("buy milk")); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, p := range []string{"/notes/todo.md", "notes/todo.md", "//notes//todo.md", "/notes/todo.md/"} {
		got, err := s.Read(p)
		if err != nil {
			t.Fatalf("read %q: %v", p, err)
		}
		if string(got) != "buy milk" {
			t.Errorf("read %q = %q, want %q", p, got, "buy milk")
		}
	}

	// Write replaces: shorter content leaves nothing behind.
	if err := s.Write("/notes/todo.md", []byte("x")); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got, _ = s.Read("/notes/todo.md"); string(got) != "x" {
		t.Errorf("after overwrite read = %q, want %q", got, "x")
	}
}

func TestReadMissing(t *testing.T) {
	s, _ := newTestStore(t)
	if _, err := s.Read("/nope.txt"); !errors.Is(err, knowledge.ErrNotFound) {
		t.Fatalf("read missing: err = %v, want knowledge.ErrNotFound", err)
	}
}

func TestList(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.Write("/abx.txt", []byte("x_100%")); err != nil {
		t.Fatalf("write /abx.txt: %v", err)
	}
	for p, c := range map[string]string{
		"/a/b/c.txt": "c",
		"/a/d.txt":   "d",
		"/top.txt":   "t",
	} {
		if err := s.Write(p, []byte(c)); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	cases := []struct {
		dir  string
		want []string
	}{
		{"/", []string{"a/", "abx.txt", "top.txt"}},
		{"/a", []string{"b/", "d.txt"}},
		{"/a/b/", []string{"c.txt"}},
		{"/missing", []string{}},
		// Prefix matching is literal over the index: a sibling sharing a literal
		// prefix (no separator) is not a child, and shell metacharacters in the
		// stored path are just bytes (never wildcards).
		{"/ab", []string{}},
		{"x_1", []string{}},
	}
	for _, tc := range cases {
		got, err := s.List(tc.dir)
		if err != nil {
			t.Fatalf("list %q: %v", tc.dir, err)
		}
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("list %q = %v, want %v", tc.dir, got, tc.want)
		}
	}
}

func TestDelete(t *testing.T) {
	s, _ := newTestStore(t)
	for _, p := range []string{"/a/b/c.txt", "/a/d.txt", "/top.txt"} {
		if err := s.Write(p, []byte("x")); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	if err := s.Delete("/a"); err != nil {
		t.Fatalf("delete /a: %v", err)
	}
	if s.Exists("/a/b/c.txt") || s.Exists("/a/d.txt") || s.Exists("/a") {
		t.Errorf("delete /a left content behind")
	}
	if !s.Exists("/top.txt") {
		t.Errorf("delete /a removed unrelated /top.txt")
	}
	// Deleting a missing path is a no-op, deleting a leaf file works.
	if err := s.Delete("/missing"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
	if err := s.Delete("/top.txt"); err != nil {
		t.Fatalf("delete /top.txt: %v", err)
	}
	if s.Exists("/top.txt") {
		t.Errorf("/top.txt survived delete")
	}
	// The index must not leak deleted paths: List goes back to empty.
	got, err := s.List("/")
	if err != nil || len(got) != 0 {
		t.Errorf("list / after deletes = %v (err %v), want empty", got, err)
	}
}

func TestExists(t *testing.T) {
	s, _ := newTestStore(t)
	if s.Exists("/a/b/c.txt") {
		t.Errorf("empty store reports /a/b/c.txt")
	}
	// Append creates the key (Python parity: APPEND on a missing key creates it).
	if err := s.Append("/a/b/c.txt", []byte("")); err != nil {
		t.Fatalf("empty append: %v", err)
	}
	if !s.Exists("/a/b/c.txt") {
		t.Errorf("Exists(/a/b/c.txt) = false after append")
	}
	for _, p := range []string{"/a/b", "/a", "/"} {
		if !s.Exists(p) {
			t.Errorf("Exists(%q) = false, want true", p)
		}
	}
	if s.Exists("/a/b/other.txt") {
		t.Errorf("Exists(/a/b/other.txt) = true, want false")
	}
}

func TestAppendAndReadRange(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.Append("/log.jsonl", nil); err != nil {
		t.Fatalf("empty append: %v", err)
	}
	if !s.Exists("/log.jsonl") {
		t.Fatalf("append of empty payload did not create the key")
	}
	if err := s.Append("/log.jsonl", []byte("первая\n")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.Append("/log.jsonl", []byte("вторая\n")); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, err := s.Read("/log.jsonl")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if want := "первая\nвторая\n"; string(got) != want {
		t.Errorf("read = %q, want %q", got, want)
	}

	// ReadRange works on byte offsets (Python parity), so the range cases use
	// ASCII payloads. Head, tail, bounded middle, past-end and missing file.
	if err := s.Append("/bytes.txt", []byte("first\nsecond\n")); err != nil {
		t.Fatalf("append bytes.txt: %v", err)
	}
	cases := []struct {
		name string
		path string
		off  int
		n    int
		want string
	}{
		{"from start bounded", "/bytes.txt", 0, 3, "fir"},
		{"rest of file (n<=0)", "/bytes.txt", 6, 0, "second\n"},
		{"middle", "/bytes.txt", 6, 2, "se"},
		{"whole file (n<=0)", "/bytes.txt", 0, -1, "first\nsecond\n"},
		{"offset at end", "/bytes.txt", len("first\nsecond\n"), 5, ""},
		{"offset past end", "/bytes.txt", len("first\nsecond\n") + 10, 5, ""},
		{"utf-8 whole via log", "/log.jsonl", 0, 0, string(got)},
	}
	for _, tc := range cases {
		part, err := s.ReadRange(tc.path, tc.off, tc.n)
		if err != nil {
			t.Fatalf("read_range %s: %v", tc.name, err)
		}
		if string(part) != tc.want {
			t.Errorf("read_range %s = %q, want %q", tc.name, part, tc.want)
		}
	}
	if part, err := s.ReadRange("/missing.txt", 0, 5); err != nil || part != nil {
		t.Errorf("read_range missing = (%v, %v), want (nil, nil)", part, err)
	}
	if _, err := s.ReadRange("/log.jsonl", -1, 5); err == nil {
		t.Errorf("read_range negative offset: want error, got nil")
	}
}

func TestOnChange(t *testing.T) {
	s, _ := newTestStore(t)
	var mu sync.Mutex
	var got []string
	s.OnChange(func(path string) {
		mu.Lock()
		got = append(got, path)
		mu.Unlock()
	})
	if err := s.Write("/a/b.txt", []byte("1")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Append("/a/b.txt", []byte("2")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.Write("/a/c.txt", []byte("3")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Delete("/a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"/a/b.txt", "/a/b.txt", "/a/c.txt", "/a/b.txt", "/a/c.txt"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("notifications = %v, want %v", got, want)
	}
}

func TestPrefixOption(t *testing.T) {
	mr := miniredis.RunT(t)
	newRDB := func() *redis.Client {
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { rdb.Close() })
		return rdb
	}
	s := NewStore(newRDB(), WithPrefix("custom:kn"))
	if err := s.Write("/k", []byte("v")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := mr.Get("custom:kn:/k"); err != nil {
		t.Errorf("content key not under custom prefix: %v", err)
	}
	if !mr.Exists("custom:kn:__index") {
		t.Errorf("index key not under custom prefix")
	}
	// Trailing colons are trimmed (Python rstrip(":") parity).
	s2 := NewStore(newRDB(), WithPrefix("other::"))
	if err := s2.Write("/k2", []byte("v2")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := mr.Get("other:/k2"); err != nil {
		t.Errorf("content key = %v, want other:/k2", err)
	}
}

// The Python test_redis_append scenario: appends from independent store
// instances (separate clients) over one server are atomic via the Lua script —
// every entry lands, none is lost.
func TestAppendAtomicAcrossClients(t *testing.T) {
	mr := miniredis.RunT(t)
	const connections = 20
	var stores []*Store
	var clients []*redis.Client
	for range connections {
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		clients = append(clients, rdb)
		stores = append(stores, NewStore(rdb))
	}
	t.Cleanup(func() {
		for _, c := range clients {
			c.Close()
		}
	})

	if err := stores[0].Write("/log.jsonl", []byte("")); err != nil {
		t.Fatalf("write: %v", err)
	}
	lines := make([]string, 0, connections)
	var wg sync.WaitGroup
	for i, s := range stores {
		line := fmt.Sprintf("entry-%d-雪\n", i)
		lines = append(lines, line)
		wg.Add(1)
		go func(s *Store, line string) {
			defer wg.Done()
			if err := s.Append("/log.jsonl", []byte(line)); err != nil {
				t.Errorf("append: %v", err)
			}
		}(s, line)
	}
	wg.Wait()

	got, err := stores[0].Read("/log.jsonl")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	parts := strings.SplitAfter(string(got), "\n")
	parts = parts[:len(parts)-1] // drop the empty tail after the final "\n"
	if len(parts) != connections {
		t.Fatalf("log holds %d lines, want %d: %q", len(parts), connections, got)
	}
	// Appends land in completion order; compare as multisets.
	want := append([]string(nil), lines...)
	sort.Strings(want)
	sort.Strings(parts)
	if strings.Join(parts, "") != strings.Join(want, "") {
		t.Errorf("appended content mismatch: got %q, want %q", got, strings.Join(want, ""))
	}
}

// Concurrent mixed traffic on one store: no lost writes, appends interleave at
// whole-call granularity.
func TestConcurrentWriteRead(t *testing.T) {
	s, _ := newTestStore(t)
	const workers = 16
	const rounds = 10
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for r := range rounds {
				// Per-worker paths: concurrent writers to one path race by
				// design, so write→read pairs must not share keys.
				path := fmt.Sprintf("/w%d/batch-%d.txt", w, r)
				payload := []byte(fmt.Sprintf("worker %d round %d", w, r))
				if err := s.Write(path, payload); err != nil {
					t.Errorf("write %s: %v", path, err)
					return
				}
				got, err := s.Read(path)
				if err != nil || string(got) != string(payload) {
					t.Errorf("read %s = (%q, %v), want (%q, nil)", path, got, err, payload)
					return
				}
				if err := s.Append(fmt.Sprintf("/shared/log-%d.jsonl", w%3), payload); err != nil {
					t.Errorf("append: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	for g := range 3 {
		data, err := s.Read(fmt.Sprintf("/shared/log-%d.jsonl", g))
		if err != nil {
			t.Fatalf("read shared log %d: %v", g, err)
		}
		var wantLen int
		for w := 0; w < workers; w++ {
			if w%3 == g {
				wantLen += rounds * len(fmt.Sprintf("worker %d round %d", w, rounds-1))
			}
		}
		if len(data) != wantLen {
			t.Errorf("shared log %d holds %d bytes, want %d (lost appends?)", g, len(data), wantLen)
		}
	}
}

func TestTimeoutDefaultAndOption(t *testing.T) {
	s, _ := newTestStore(t)
	if s.timeout != DefaultTimeout {
		t.Errorf("default timeout = %s, want %s", s.timeout, DefaultTimeout)
	}
	s2, _ := newTestStore(t, WithTimeout(5*time.Second))
	if s2.timeout != 5*time.Second {
		t.Errorf("WithTimeout(5s) = %s, want 5s", s2.timeout)
	}
	// Zero or negative restores the default instead of removing the deadline.
	for _, d := range []time.Duration{0, -time.Second} {
		s3, _ := newTestStore(t, WithTimeout(d))
		if s3.timeout != DefaultTimeout {
			t.Errorf("WithTimeout(%s) = %s, want default %s", d, s3.timeout, DefaultTimeout)
		}
	}
}

// startHungRedis listens on loopback, accepts connections and never speaks the
// Redis protocol — a hung server. Held connections are closed on stop.
func startHungRedis(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	stop := make(chan struct{})
	t.Cleanup(func() {
		ln.Close()
		close(stop)
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				<-stop
				c.Close()
			}(conn)
		}
	}()
	return ln.Addr().String()
}

// L-26: every Redis call must run under its own deadline. The client is built
// the way go-redis documents for blocking setups: client-level read/write
// timeouts off (ReadTimeout -1 → 0 = no socket timeout) plus
// ContextTimeoutEnabled, so the per-call context deadline is the only bound —
// without it a hung server blocks the caller forever. Calls must fail fast
// with the context error instead.
func TestHungRedisDeadline(t *testing.T) {
	addr := startHungRedis(t)
	rdb := redis.NewClient(&redis.Options{
		Addr:                  addr,
		DialTimeout:           5 * time.Second,
		ReadTimeout:           -1, // client-level timeouts off: the deadline under test is the store's
		WriteTimeout:          -1,
		ContextTimeoutEnabled: true, // go-redis applies the ctx deadline to the socket only with this on
	})
	t.Cleanup(func() { rdb.Close() })
	s := NewStore(rdb, WithTimeout(200*time.Millisecond))

	start := time.Now()
	err := s.Write("/hung.txt", []byte("x"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Write on hung redis: err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Write took %s to fail; deadline was 200ms", elapsed)
	}

	start = time.Now()
	if _, err := s.Read("/hung.txt"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Read on hung redis: err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Read took %s to fail; deadline was 200ms", elapsed)
	}
}
