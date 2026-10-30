package knowledgesqlite

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/luckystrker/ago/knowledge"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "knowledge.sqlite"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestWriteReadRoundTrip(t *testing.T) {
	s := newTestStore(t)
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
	s := newTestStore(t)
	if _, err := s.Read("/nope.txt"); !errors.Is(err, knowledge.ErrNotFound) {
		t.Fatalf("read missing: err = %v, want knowledge.ErrNotFound", err)
	}
}

func TestList(t *testing.T) {
	s := newTestStore(t)
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
		// Prefix matching must be literal: a sibling sharing a literal prefix
		// (no separator) is not a child, and LIKE metacharacters are just bytes.
		{"/ab", []string{}},
	}
	if err := s.Write("/abx.txt", []byte("x_100%")); err != nil {
		t.Fatalf("write /abx.txt: %v", err)
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
	s := newTestStore(t)
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
}

func TestExists(t *testing.T) {
	s := newTestStore(t)
	if s.Exists("/a/b/c.txt") {
		t.Errorf("empty store reports /a/b/c.txt")
	}
	if err := s.Write("/a/b/c.txt", []byte("c")); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, p := range []string{"/a/b/c.txt", "/a/b", "/a", "/"} {
		if !s.Exists(p) {
			t.Errorf("Exists(%q) = false, want true", p)
		}
	}
	if s.Exists("/a/b/other.txt") {
		t.Errorf("Exists(/a/b/other.txt) = true, want false")
	}
}

func TestAppendAndReadRange(t *testing.T) {
	s := newTestStore(t)
	// An empty append still creates the key (Python parity).
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

	// ReadRange works on byte offsets (Python parity: read_range slices the
	// encoded bytes), so the range cases use ASCII payloads. Head, tail,
	// bounded middle, past-end and missing file.
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
	s := newTestStore(t)
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

// Two instances over one database file must see each other's committed writes.
func TestPersistenceAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	first, err := NewStore(filepath.Join(dir, "knowledge.sqlite"))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	defer first.Close()
	second, err := NewStore(filepath.Join(dir, "knowledge.sqlite"))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	defer second.Close()

	if err := first.Write("/shared/state", []byte("v1")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	got, err := second.Read("/shared/state")
	if err != nil || string(got) != "v1" {
		t.Errorf("second read = (%q, %v), want (v1, nil)", got, err)
	}
	if err := second.Write("/shared/state", []byte("v2")); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if got, _ = first.Read("/shared/state"); string(got) != "v2" {
		t.Errorf("first read = %q, want %q", got, "v2")
	}
}

// The Python sqlite suite's core scenario: appends from two independent store
// instances on one database never lose entries, because each append reads the
// prior content only under the database's writer lock.
func TestAppendAtomicAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	var stores []*Store
	for i := range 8 {
		s, err := NewStore(filepath.Join(dir, "knowledge.sqlite"))
		if err != nil {
			t.Fatalf("store %d: %v", i, err)
		}
		defer s.Close()
		stores = append(stores, s)
	}
	if err := stores[0].Write("/log.jsonl", []byte("")); err != nil {
		t.Fatalf("write: %v", err)
	}

	lines := make([]string, 0, len(stores))
	var wg sync.WaitGroup
	for i, s := range stores {
		line := fmt.Sprintf("line-%d\n", i)
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
	parts = parts[:len(parts)-1]
	sort.Strings(parts)
	if strings.Join(parts, "") != strings.Join(lines, "") {
		t.Errorf("content = %q, want every line exactly once", got)
	}
}

// Concurrent mixed traffic on one instance: no lost writes, reads always
// consistent (run with -race for the memory-model check).
func TestConcurrentWriteRead(t *testing.T) {
	s := newTestStore(t)
	const workers = 16
	const rounds = 10
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for r := range rounds {
				// Per-worker paths: concurrent writers to one path race by
				// design (DiskStore semantics), so write→read pairs must not
				// share keys across workers.
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

	// Every shared log holds exactly rounds*len(payloads-per-worker-group) bytes.
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
