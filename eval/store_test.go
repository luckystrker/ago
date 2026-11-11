package eval

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func storeRun(t *testing.T, dir string, opts ...RunOption) *RunResult {
	t.Helper()
	a := scriptedAgent(t, textResp("Paris!"))
	res, err := Run(context.Background(), a, Suite{
		{Name: "cap", Input: "capital?", Scorers: []Scorer{Correctness{Expected: "Paris", Mode: MatchContains}}},
	}, append([]RunOption{WithStoreDir(dir)}, opts...)...)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res
}

func TestStoreDirWritesRunByID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runs", "nested")
	res := storeRun(t, dir, WithLabel("baseline"))
	if res.RunID == "" || res.Label != "baseline" || res.CreatedAt.IsZero() {
		t.Fatalf("run identity: id=%q label=%q created=%v", res.RunID, res.Label, res.CreatedAt)
	}
	if _, err := os.Stat(filepath.Join(dir, res.RunID+".json")); err != nil {
		t.Fatalf("stored file: %v", err)
	}

	loaded, err := LoadRun(dir, res.RunID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if loaded.RunID != res.RunID || loaded.Label != "baseline" || !loaded.CreatedAt.Equal(res.CreatedAt) {
		t.Fatalf("identity drift: %+v vs %+v", loaded, res)
	}
	if loaded.PassRate != res.PassRate || len(loaded.Cases) != 1 || len(loaded.Cases[0].Trace.Events) == 0 {
		t.Fatalf("loaded run lost data: %+v", loaded)
	}
}

func TestStoreDirKeepsRunsApart(t *testing.T) {
	dir := t.TempDir()
	a, b := storeRun(t, dir), storeRun(t, dir)
	if a.RunID == b.RunID {
		t.Fatalf("two runs share id %q", a.RunID)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("store entries = %d, err %v; want 2", len(entries), err)
	}
}

func TestStoreRunRefusesUnsafeIDs(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"", ".", "..", "../escape", `a\b`, "a/b"} {
		if _, err := StoreRun(dir, &RunResult{RunID: id}); !errors.Is(err, ErrInvalidRunID) {
			t.Errorf("StoreRun(%q): err = %v, want ErrInvalidRunID", id, err)
		}
		if _, err := LoadRun(dir, id); !errors.Is(err, ErrInvalidRunID) {
			t.Errorf("LoadRun(%q): err = %v, want ErrInvalidRunID", id, err)
		}
	}
	if _, err := StoreRun(dir, nil); err == nil {
		t.Error("StoreRun(nil) succeeded")
	}
}

// TestStoreDirFailureKeepsResult: an unwritable store returns the graded run
// together with the error.
func TestStoreDirFailureKeepsResult(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := scriptedAgent(t, textResp("Paris"))
	res, err := Run(context.Background(), a, Suite{
		{Name: "cap", Input: "q", Scorers: []Scorer{Correctness{Expected: "Paris", Mode: MatchContains}}},
	}, WithStoreDir(filepath.Join(blocker, "runs"))) // a file is in the way of the directory
	if err == nil {
		t.Fatal("expected a store error")
	}
	if res == nil || len(res.Cases) != 1 || !res.Cases[0].Passed {
		t.Fatalf("graded result lost: %+v", res)
	}
}
