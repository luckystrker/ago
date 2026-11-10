package network

import (
	"bytes"
	"errors"
	"testing"

	"github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/task"
)

// checkpointStoreContract is the contract task.CheckpointStore promises (the
// task package's MemoryCheckpointStore defines it): absent reads are
// (nil, nil), writes replace, and both directions copy.
func checkpointStoreContract(t *testing.T, store task.CheckpointStore) {
	t.Helper()
	if got, err := store.Read("none"); got != nil || err != nil {
		t.Fatalf("absent read: %v %v", got, err)
	}
	src := []byte("v1")
	if err := store.Write("t", src); err != nil {
		t.Fatal(err)
	}
	src[0] = 'X' // caller mutates after Write
	got, err := store.Read("t")
	if err != nil || !bytes.Equal(got, []byte("v1")) {
		t.Fatalf("write must copy: %q %v", got, err)
	}
	got[0] = 'Y' // caller mutates the read result
	if again, _ := store.Read("t"); !bytes.Equal(again, []byte("v1")) {
		t.Fatalf("read must copy: %q", again)
	}
	_ = store.Write("t", []byte("v2"))
	if got, _ := store.Read("t"); !bytes.Equal(got, []byte("v2")) {
		t.Fatalf("last write wins: %q", got)
	}
	_ = store.Write("empty", []byte{})
	if got, err := store.Read("empty"); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("an empty checkpoint is not an absent one: %#v %v", got, err)
	}
	if got, _ := store.Read("other"); got != nil {
		t.Fatalf("keys are independent: %q", got)
	}
}

func TestHubBackedCheckpointStoreContract(t *testing.T) {
	t.Run("hub-backed unclaimed", func(t *testing.T) {
		checkpointStoreContract(t, NewHubBackedCheckpointStore(NewHub(), ""))
	})
	t.Run("hub-backed claimed", func(t *testing.T) {
		checkpointStoreContract(t, NewHubBackedCheckpointStore(NewHub(), "agent-1"))
	})
	t.Run("reference memory store", func(t *testing.T) {
		checkpointStoreContract(t, task.NewMemoryCheckpointStore())
	})
}

func TestCheckpointFirstWriterClaim(t *testing.T) {
	h := NewHub()
	if err := h.CheckpointTask("alice", "t", []byte("a1")); err != nil {
		t.Fatal(err)
	}
	// The claimant keeps writing and reading; strangers are refused both ways.
	if err := h.CheckpointTask("alice", "t", []byte("a2")); err != nil {
		t.Fatalf("claimant rewrite: %v", err)
	}
	if err := h.CheckpointTask("mallory", "t", []byte("evil")); !errors.Is(err, ErrCheckpointDenied) || !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("stranger write: %v", err)
	}
	if _, err := h.ReadTaskCheckpoint("mallory", "t"); !errors.Is(err, ErrCheckpointDenied) {
		t.Fatalf("stranger read: %v", err)
	}
	if got, err := h.ReadTaskCheckpoint("alice", "t"); err != nil || string(got) != "a2" {
		t.Fatalf("claimant read: %q %v", got, err)
	}
	// The refused write left the checkpoint alone; the trusted in-process
	// path (no identity) is neither claiming nor checked.
	if got, _ := h.ReadTaskCheckpoint("", "t"); string(got) != "a2" {
		t.Fatalf("in-process read: %q", got)
	}
	if err := h.CheckpointTask("", "t", []byte("local")); err != nil {
		t.Fatalf("in-process write: %v", err)
	}
	if got, _ := h.ReadTaskCheckpoint("alice", "t"); string(got) != "local" {
		t.Fatalf("after in-process write: %q", got)
	}
	// An in-process write must not erase the claim: strangers stay locked
	// out, and so does the restarted hub that replays the WAL.
	if err := h.CheckpointTask("mallory", "t", []byte("evil")); !errors.Is(err, ErrCheckpointDenied) {
		t.Fatalf("claim erased by an in-process write: %v", err)
	}
	if _, err := h.ReadTaskCheckpoint("mallory", "t"); !errors.Is(err, ErrCheckpointDenied) {
		t.Fatalf("claim erased for readers: %v", err)
	}
	// An unclaimed checkpoint is first claimed by the next identified writer.
	_ = h.CheckpointTask("", "open", []byte("x"))
	if _, err := h.ReadTaskCheckpoint("anyone", "open"); err != nil {
		t.Fatalf("unclaimed read: %v", err)
	}
	_ = h.CheckpointTask("bob", "open", []byte("y"))
	if err := h.CheckpointTask("carol", "open", []byte("z")); !errors.Is(err, ErrCheckpointDenied) {
		t.Fatalf("claimed by the first identified writer: %v", err)
	}
}

func TestCheckpointDurableThroughWAL(t *testing.T) {
	wal := NewMemoryWAL()
	h := NewHub(WithHubWAL(wal))
	_ = h.CheckpointTask("alice", "t", []byte("one"))
	_ = h.CheckpointTask("alice", "t", []byte("two"))
	_ = h.CheckpointTask("", "plain", []byte("p"))

	h2 := NewHub(WithHubWAL(wal))
	if err := h2.Replay(); err != nil {
		t.Fatal(err)
	}
	if got, _ := h2.ReadTaskCheckpoint("alice", "t"); string(got) != "two" {
		t.Fatalf("restored checkpoint: %q", got)
	}
	// Ownership survives the restart too.
	if err := h2.CheckpointTask("mallory", "t", []byte("x")); !errors.Is(err, ErrCheckpointDenied) {
		t.Fatalf("claim lost across replay: %v", err)
	}
	if got, _ := h2.ReadTaskCheckpoint("", "plain"); string(got) != "p" {
		t.Fatalf("unclaimed checkpoint: %q", got)
	}
}

type failingWAL struct{}

func (failingWAL) Append(WALRecord) error        { return errors.New("disk full") }
func (failingWAL) ReadAll() ([]WALRecord, error) { return nil, nil }

func TestCheckpointFailsWhenWALRefuses(t *testing.T) {
	h := NewHub(WithHubWAL(&failingWAL{}))
	if err := h.CheckpointTask("alice", "t", []byte("x")); err == nil {
		t.Fatal("a checkpoint the WAL refused must not report success")
	}
	if got, _ := h.ReadTaskCheckpoint("", "t"); got != nil {
		t.Fatalf("refused write is visible: %q", got)
	}
}

// The store plugs into the task package's own checkpoint-and-resume flow.
func TestHubBackedStoreDrivesTaskResume(t *testing.T) {
	h := NewHub()
	store := NewHubBackedCheckpointStore(h, "")
	c := context.New(stream.NewMemoryStream())
	first, err := task.Start(c, "agent", task.Spec{Title: "first"}, task.WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Checkpoint([]byte("progress=7")); err != nil {
		t.Fatal(err)
	}
	got, err := h.ReadTaskCheckpoint("", first.TaskID())
	if err != nil || string(got) != "progress=7" {
		t.Fatalf("the task's checkpoint did not reach the hub: %q %v", got, err)
	}
	second, err := task.Start(c, "agent", task.Spec{Title: "second"},
		task.WithCheckpointStore(store), task.WithResumeFrom(first.TaskID()))
	if err != nil {
		t.Fatal(err)
	}
	if string(second.ResumedState()) != "progress=7" {
		t.Fatalf("resume through the hub store: %q", second.ResumedState())
	}
}

func TestInProcessWriteKeepsClaimAcrossReplay(t *testing.T) {
	wal := NewMemoryWAL()
	h := NewHub(WithHubWAL(wal))
	_ = h.CheckpointTask("alice", "t", []byte("one"))
	_ = h.CheckpointTask("", "t", []byte("local"))
	h2 := NewHub(WithHubWAL(wal))
	if err := h2.Replay(); err != nil {
		t.Fatal(err)
	}
	if err := h2.CheckpointTask("mallory", "t", []byte("x")); !errors.Is(err, ErrCheckpointDenied) {
		t.Fatalf("claim lost when the last WAL record was an in-process write: %v", err)
	}
}
