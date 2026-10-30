package knowledge

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/luckystrker/ago/events"
)

type backend struct {
	name string
	new  func() Store
}

func allBackends(t *testing.T) []backend {
	t.Helper()
	return []backend{
		{"MemoryStore", func() Store { return NewMemoryStore() }},
		{"DiskStore", func() Store { return NewDiskStore(t.TempDir()) }},
	}
}

func TestStoreWriteReadRoundTrip(t *testing.T) {
	for _, be := range allBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			s := be.new()
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

			// Normalization: same content is visible under equivalent spellings.
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

			// Write replaces; Write overwrites with shorter content entirely.
			if err := s.Write("/notes/todo.md", []byte("x")); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, _ = s.Read("/notes/todo.md")
			if string(got) != "x" {
				t.Errorf("after overwrite read = %q, want %q", got, "x")
			}
		})
	}
}

func TestStoreReadMissing(t *testing.T) {
	for _, be := range allBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			s := be.new()
			_, err := s.Read("/nope.txt")
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("read missing: err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestStoreList(t *testing.T) {
	for _, be := range allBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			s := be.new()
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
				{"/", []string{"a/", "top.txt"}},
				{"/a", []string{"b/", "d.txt"}},
				{"/a/b/", []string{"c.txt"}},
				{"/missing", []string{}},
			}
			for _, tc := range cases {
				got, err := s.List(tc.dir)
				if err != nil {
					t.Fatalf("list %q: %v", tc.dir, err)
				}
				if len(got) != len(tc.want) {
					t.Fatalf("list %q = %v, want %v", tc.dir, got, tc.want)
				}
				for i := range got {
					if got[i] != tc.want[i] {
						t.Fatalf("list %q = %v, want %v", tc.dir, got, tc.want)
					}
				}
			}
		})
	}
}

func TestStoreExists(t *testing.T) {
	for _, be := range allBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			s := be.new()
			if s.Exists("/a/b.txt") {
				t.Error("missing file reported as existing")
			}
			if err := s.Write("/a/b.txt", []byte("x")); err != nil {
				t.Fatalf("write: %v", err)
			}
			if !s.Exists("/a/b.txt") {
				t.Error("file reported as missing")
			}
			if !s.Exists("/a") || !s.Exists("/a/") || !s.Exists("/") {
				t.Error("implicit directory reported as missing")
			}
			if err := s.Delete("/a/b.txt"); err != nil {
				t.Fatalf("delete: %v", err)
			}
			if s.Exists("/a/b.txt") {
				t.Error("deleted file reported as existing")
			}
		})
	}
}

func TestStoreAppendAndReadRange(t *testing.T) {
	for _, be := range allBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			s := be.new()
			if err := s.Append("/logs/app.jsonl", []byte("aaaa\n")); err != nil {
				t.Fatalf("append: %v", err)
			}
			if err := s.Append("/logs/app.jsonl", []byte("bbbbb\n")); err != nil {
				t.Fatalf("append: %v", err)
			}
			got, err := s.Read("/logs/app.jsonl")
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if want := "aaaa\nbbbbb\n"; string(got) != want {
				t.Errorf("read = %q, want %q", got, want)
			}

			cases := []struct {
				off, n int
				want   string
			}{
				{0, 5, "aaaa\n"},          // exact prefix
				{2, 3, "aa\n"},            // middle slice
				{5, 0, "bbbbb\n"},         // n <= 0: to EOF
				{5, -1, "bbbbb\n"},        // n < 0: to EOF
				{11, 4, ""},               // offset at EOF: empty
				{99, 4, ""},               // offset past EOF: empty
				{0, 999, "aaaa\nbbbbb\n"}, // n larger than file: clamped
			}
			for _, tc := range cases {
				got, err := s.ReadRange("/logs/app.jsonl", tc.off, tc.n)
				if err != nil {
					t.Fatalf("read_range(%d,%d): %v", tc.off, tc.n, err)
				}
				if string(got) != tc.want {
					t.Errorf("read_range(%d,%d) = %q, want %q", tc.off, tc.n, got, tc.want)
				}
			}

			// Missing file: empty result, no error (Python read_range parity).
			got, err = s.ReadRange("/logs/missing.jsonl", 0, 10)
			if err != nil || len(got) != 0 {
				t.Errorf("read_range missing = (%q, %v), want empty/nil", got, err)
			}

			// Negative offset is a caller bug: error.
			if _, err := s.ReadRange("/logs/app.jsonl", -1, 0); err == nil {
				t.Error("negative offset: want error, got nil")
			}
		})
	}
}

func TestStoreDelete(t *testing.T) {
	for _, be := range allBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			s := be.new()
			for _, p := range []string{"/d/x.txt", "/d/sub/y.txt", "/keep.txt"} {
				if err := s.Write(p, []byte("x")); err != nil {
					t.Fatalf("write %s: %v", p, err)
				}
			}

			// File delete.
			if err := s.Delete("/d/x.txt"); err != nil {
				t.Fatalf("delete file: %v", err)
			}
			if s.Exists("/d/x.txt") {
				t.Error("file survived delete")
			}
			if !s.Exists("/d/sub/y.txt") {
				t.Error("sibling lost by file delete")
			}

			// Directory delete removes the subtree.
			if err := s.Delete("/d"); err != nil {
				t.Fatalf("delete dir: %v", err)
			}
			if s.Exists("/d") || s.Exists("/d/sub/y.txt") {
				t.Error("subtree survived dir delete")
			}
			if !s.Exists("/keep.txt") {
				t.Error("delete removed unrelated file")
			}

			// Deleting a missing path is a no-op.
			if err := s.Delete("/d"); err != nil {
				t.Errorf("delete missing: %v", err)
			}
			if err := s.Delete("/never/was/here.txt"); err != nil {
				t.Errorf("delete missing nested: %v", err)
			}
		})
	}
}

func TestStoreOnChange(t *testing.T) {
	for _, be := range allBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			s := be.new()
			var mu sync.Mutex
			var got []string
			s.OnChange(func(path string) {
				mu.Lock()
				defer mu.Unlock()
				got = append(got, path)
			})

			if err := s.Write("/x/a.txt", []byte("1")); err != nil {
				t.Fatalf("write: %v", err)
			}
			if err := s.Append("/x/a.txt", []byte("2")); err != nil {
				t.Fatalf("append: %v", err)
			}
			if err := s.Delete("/x/a.txt"); err != nil {
				t.Fatalf("delete: %v", err)
			}
			if _, err := s.Read("/x/a.txt"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("read after delete: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			// Reads never notify; the three mutations do.
			if len(got) != 3 {
				t.Fatalf("notifications = %v, want three entries", got)
			}
			for i, want := range []string{"/x/a.txt", "/x/a.txt", "/x/a.txt"} {
				if got[i] != want {
					t.Errorf("notification %d = %q, want %q", i, got[i], want)
				}
			}
		})
	}
}

func TestDiskStoreTraversal(t *testing.T) {
	root := t.TempDir()
	s := NewDiskStore(root)

	paths := []string{"/../evil.txt", "/a/../../evil.txt", "/sub/../../../evil.txt"}
	for _, p := range paths {
		if err := s.Write(p, []byte("evil")); !errors.Is(err, ErrTraversal) {
			t.Errorf("write %q: err = %v, want ErrTraversal", p, err)
		}
		if err := s.Append(p, []byte("evil")); !errors.Is(err, ErrTraversal) {
			t.Errorf("append %q: err = %v, want ErrTraversal", p, err)
		}
		if _, err := s.Read(p); !errors.Is(err, ErrTraversal) {
			t.Errorf("read %q: err = %v, want ErrTraversal", p, err)
		}
		if err := s.Delete(p); !errors.Is(err, ErrTraversal) {
			t.Errorf("delete %q: err = %v, want ErrTraversal", p, err)
		}
		if s.Exists(p) {
			t.Errorf("exists %q = true, want false", p)
		}
	}

	// Nothing landed outside the root.
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "evil.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("escape file exists outside root (stat err = %v)", err)
	}
}

func TestDiskStoreFileLayout(t *testing.T) {
	root := t.TempDir()
	s := NewDiskStore(root)

	if err := s.Write("/x/y.txt", []byte("on disk")); err != nil {
		t.Fatalf("write: %v", err)
	}
	onDisk, err := os.ReadFile(filepath.Join(root, "x", "y.txt"))
	if err != nil {
		t.Fatalf("read physical file: %v", err)
	}
	if string(onDisk) != "on disk" {
		t.Errorf("physical content = %q, want %q", onDisk, "on disk")
	}

	// Append creates missing files on disk.
	if err := s.Append("/fresh/log.txt", []byte("first")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "fresh", "log.txt")); err != nil {
		t.Errorf("appended file missing on disk: %v", err)
	}

	// Deleting a file prunes now-empty parent directories up to the root.
	if err := s.Delete("/x/y.txt"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "x")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("empty parent dir survived (stat err = %v)", err)
	}
}

func TestLockedStorePassthrough(t *testing.T) {
	s := NewLockedStore(NewMemoryStore())
	if err := s.Write("/a/b.txt", []byte("one")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Append("/a/b.txt", []byte("two")); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, err := s.Read("/a/b.txt")
	if err != nil || string(got) != "onetwo" {
		t.Fatalf("read = (%q, %v), want onetwo", got, err)
	}
	kids, err := s.List("/a")
	if err != nil || len(kids) != 1 || kids[0] != "b.txt" {
		t.Fatalf("list = (%v, %v), want [b.txt]", kids, err)
	}
	if !s.Exists("/a/b.txt") {
		t.Error("exists = false, want true")
	}
	rng, err := s.ReadRange("/a/b.txt", 3, 0)
	if err != nil || string(rng) != "two" {
		t.Fatalf("read_range = (%q, %v), want two", rng, err)
	}
	changes := make(chan string, 1)
	s.OnChange(func(path string) { changes <- path })
	if err := s.Delete("/a/b.txt"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := <-changes; got != "/a/b.txt" {
		t.Errorf("notification = %q, want /a/b.txt", got)
	}
}

// TestLockedStoreConcurrentAppends proves the wrapper serializes writers: the
// final content is exactly the concatenation of every append, never a lost
// update. Run with -race for the memory-model check.
func TestLockedStoreConcurrentAppends(t *testing.T) {
	const writers, perWriter = 8, 50
	payload := bytes.Repeat([]byte("ab"), 32) // 64 bytes

	s := NewLockedStore(NewMemoryStore())
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				if err := s.Append("/logs/w.jsonl", payload); err != nil {
					t.Errorf("append: %v", err)
					return
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				if _, err := s.Read("/logs/w.jsonl"); err != nil && !errors.Is(err, ErrNotFound) {
					t.Errorf("read: %v", err)
					return
				}
				s.Exists("/logs/w.jsonl")
				s.List("/logs")
			}
		}()
	}
	wg.Wait()

	got, err := s.Read("/logs/w.jsonl")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if want := writers * perWriter * len(payload); len(got) != want {
		t.Fatalf("content length = %d, want %d (appends lost)", len(got), want)
	}
	for off := 0; off < len(got); off += len(payload) {
		if !bytes.Equal(got[off:off+len(payload)], payload) {
			t.Fatalf("content corrupted at offset %d", off)
		}
	}
}

func TestEventLogWriterRoundTrip(t *testing.T) {
	for _, be := range allBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			store := be.new()
			w := NewEventLogWriter(store)
			at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
			batch := []events.Event{
				events.TextInput{Base: events.Base{CreatedAt: at}, Content: "hello"},
				&events.ObserverAlert{
					Base:     events.Base{CreatedAt: at},
					Source:   "loop-detector",
					Severity: events.SeverityWarning,
					Message:  "n-gram repeat",
					Data:     map[string]any{"repeats": 3},
				},
				&events.HumanMessage{Base: events.Base{CreatedAt: at}, ParentID: "req-1", Content: "approved"},
				&events.HaltEvent{Base: events.Base{CreatedAt: at}, Reason: "fatal alert"},
			}
			for i, ev := range batch {
				if err := w.AppendEvent("s1", ev); err != nil {
					t.Fatalf("append event %d: %v", i, err)
				}
			}

			got, err := w.Replay("s1")
			if err != nil {
				t.Fatalf("replay: %v", err)
			}
			if len(got) != len(batch) {
				t.Fatalf("replay returned %d events, want %d", len(got), len(batch))
			}
			for i := range batch {
				want, err := events.MarshalEvent(batch[i])
				if err != nil {
					t.Fatalf("marshal original %d: %v", i, err)
				}
				have, err := events.MarshalEvent(got[i])
				if err != nil {
					t.Fatalf("marshal replayed %d: %v", i, err)
				}
				if !bytes.Equal(want, have) {
					t.Errorf("event %d round-trip mismatch\n want: %s\n got:  %s", i, want, have)
				}
			}
		})
	}
}

func TestEventLogWriterSkipsBrokenLines(t *testing.T) {
	store := NewMemoryStore()
	w := NewEventLogWriter(store)

	good1, err := events.MarshalEvent(events.TextInput{Content: "one"})
	if err != nil {
		t.Fatal(err)
	}
	good2, err := events.MarshalEvent(&events.HaltEvent{Reason: "two"})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	buf.Write(good1)
	buf.WriteString("\n")
	buf.WriteString("not json at all\n")                          // invalid JSON
	buf.WriteString("{}\n")                                       // no @type
	buf.WriteString(`{"@type":"TextInput","content":123}` + "\n") // decode failure
	buf.WriteString(`{"@type":"NoSuchEvent","x":1}` + "\n")       // unknown type -> UnknownEvent, kept
	buf.Write(good2)
	buf.WriteString("\n\n") // blank tail line

	if err := store.Write(LogPath("s"), buf.Bytes()); err != nil {
		t.Fatalf("seed log: %v", err)
	}
	got, err := w.Replay("s")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("replay returned %d events (%s), want 3", len(got), got)
	}
	if got[0].EventType() != "TextInput" || got[1].EventType() != "UnknownEvent" || got[2].EventType() != "HaltEvent" {
		t.Errorf("event types = [%s %s %s], want [TextInput UnknownEvent HaltEvent]",
			got[0].EventType(), got[1].EventType(), got[2].EventType())
	}
}

func TestEventLogWriterStreams(t *testing.T) {
	store := NewMemoryStore()
	w := NewEventLogWriter(store)

	if err := w.AppendEvent("beta", events.TextInput{Content: "b"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := w.AppendEvent("alpha", events.TextInput{Content: "a"}); err != nil {
		t.Fatalf("append: %v", err)
	}

	ids, err := w.Streams()
	if err != nil {
		t.Fatalf("streams: %v", err)
	}
	if len(ids) != 2 || ids[0] != "alpha" || ids[1] != "beta" {
		t.Fatalf("streams = %v, want [alpha beta]", ids)
	}

	entries, err := store.List("/" + LogsDir)
	if err != nil {
		t.Fatalf("list log dir: %v", err)
	}
	if len(entries) != 2 || entries[0] != "alpha.jsonl" || entries[1] != "beta.jsonl" {
		t.Fatalf("log dir = %v, want [alpha.jsonl beta.jsonl]", entries)
	}
}

func TestEventLogWriterReplayMissingStream(t *testing.T) {
	w := NewEventLogWriter(NewMemoryStore())
	got, err := w.Replay("never-started")
	if err != nil {
		t.Fatalf("replay missing stream: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("replay missing stream = %v, want empty", got)
	}
}
