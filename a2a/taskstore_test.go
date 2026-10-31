package a2a

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestMemoryTaskStoreSaveGetDelete(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryTaskStore()
	task := &Task{ID: "t-1", ContextID: "c-1", Status: TaskStatus{State: TaskStateWorking}}

	if err := store.Save(ctx, task); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.Get(ctx, "t-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != "t-1" || got.Status.State != TaskStateWorking {
		t.Fatalf("got %+v", got)
	}

	if err := store.Delete(ctx, "t-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, "t-1"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("Get after Delete: err = %v, want ErrTaskNotFound", err)
	}
	if err := store.Delete(ctx, "t-1"); err != nil {
		t.Fatalf("Delete of absent id: %v", err)
	}
}

func TestMemoryTaskStoreIsolatesCopies(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryTaskStore()
	original := &Task{ID: "t-1", History: []Message{*NewMessage(RoleUser, NewTextPart("original"))}}
	if err := store.Save(ctx, original); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Mutate the caller's struct and the returned copy: the store must not see it.
	original.Status = TaskStatus{State: TaskStateFailed}
	got, err := store.Get(ctx, "t-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.History[0].Parts = nil

	again, err := store.Get(ctx, "t-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if again.Status.State == TaskStateFailed {
		t.Fatal("store observed a mutation of the caller's struct")
	}
	if len(again.History) != 1 || len(again.History[0].Parts) != 1 {
		t.Fatal("store observed a mutation of a returned copy")
	}
}

func TestMemoryTaskStoreEmptyIDRejected(t *testing.T) {
	store := NewMemoryTaskStore()
	if err := store.Save(context.Background(), &Task{}); err == nil {
		t.Fatal("want error for empty id")
	}
}

func TestMemoryTaskStoreConcurrent(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryTaskStore()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := string(rune('a' + n%4))
			_ = store.Save(ctx, &Task{ID: id, Status: TaskStatus{State: TaskStateWorking}})
			_, _ = store.Get(ctx, id)
			_ = store.Delete(ctx, id)
		}(i)
	}
	wg.Wait()
}

func TestMemoryTaskStoreUpdate(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryTaskStore()
	if err := store.Save(ctx, &Task{ID: "t-1", Status: TaskStatus{State: TaskStateWorking}}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// An absent id fails with ErrTaskNotFound and never invokes the mutate.
	called := false
	if _, err := store.Update(ctx, "absent", func(*Task) (*Task, error) {
		called = true
		return nil, nil
	}); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("Update absent: err = %v, want ErrTaskNotFound", err)
	}
	if called {
		t.Fatal("mutate ran for an absent id")
	}

	// The mutate edits the stored copy in place; the result lands in the
	// store and comes back as a fresh copy.
	returned, err := store.Update(ctx, "t-1", func(current *Task) (*Task, error) {
		if current.Status.State != TaskStateWorking {
			t.Fatalf("mutate saw state %q, want working", current.Status.State)
		}
		current.Status = TaskStatus{State: TaskStateCanceled}
		return current, nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if returned.Status.State != TaskStateCanceled {
		t.Fatalf("returned state = %q, want canceled", returned.Status.State)
	}
	returned.Status = TaskStatus{State: TaskStateWorking} // must not leak into the store
	got, err := store.Get(ctx, "t-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.State != TaskStateCanceled {
		t.Fatalf("stored state = %q, want canceled", got.Status.State)
	}

	// A mutate error aborts the write and comes back verbatim.
	abort := errors.New("abort")
	if _, err := store.Update(ctx, "t-1", func(*Task) (*Task, error) {
		return nil, abort
	}); !errors.Is(err, abort) {
		t.Fatalf("Update with mutate error: err = %v, want abort", err)
	}
	if got, err := store.Get(ctx, "t-1"); err != nil || got.Status.State != TaskStateCanceled {
		t.Fatalf("state after aborted Update = %v/%q, want unchanged canceled", err, got.Status.State)
	}
}

// TestMemoryTaskStoreUpdateConcurrent: overlapping Updates on one id
// serialize under the store lock, so every mutate sees the previous one's
// result and no history entry is lost.
func TestMemoryTaskStoreUpdateConcurrent(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryTaskStore()
	if err := store.Save(ctx, &Task{ID: "t-1"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	const writers = 16
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, err := store.Update(ctx, "t-1", func(current *Task) (*Task, error) {
				current.History = append(current.History,
					*NewMessage(RoleUser, NewTextPart(fmt.Sprintf("m%d", n))))
				return current, nil
			})
			if err != nil {
				t.Errorf("Update %d: %v", n, err)
			}
		}(i)
	}
	wg.Wait()
	got, err := store.Get(ctx, "t-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.History) != writers {
		t.Fatalf("history len = %d, want %d: concurrent Updates lost entries", len(got.History), writers)
	}
}

// TestMemoryTaskStoreMaxTasksEvictsOldest: beyond the cap, storing a new id
// evicts the oldest created record (FIFO) — re-saving an existing id evicts
// nothing, and Upsert takes part in the same eviction.
func TestMemoryTaskStoreMaxTasksEvictsOldest(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryTaskStore(WithMaxTasks(3))
	save := func(id string) {
		t.Helper()
		if err := store.Save(ctx, &Task{ID: id, Status: TaskStatus{State: TaskStateWorking}}); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
	}
	for i := 0; i < 5; i++ {
		save(fmt.Sprintf("t-%d", i))
	}
	for _, id := range []string{"t-0", "t-1"} {
		if _, err := store.Get(ctx, id); !errors.Is(err, ErrTaskNotFound) {
			t.Fatalf("Get %s: err = %v, want ErrTaskNotFound (evicted)", id, err)
		}
	}
	for _, id := range []string{"t-2", "t-3", "t-4"} {
		if _, err := store.Get(ctx, id); err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
	}

	// Saving an existing id at the cap replaces in place — no eviction.
	save("t-2")
	if _, err := store.Get(ctx, "t-3"); err != nil {
		t.Fatalf("Get t-3 after re-save: %v (re-save must not evict)", err)
	}

	// Upsert of a new id evicts the oldest live record (t-2), lazily skipping
	// the ids Delete/eviction already dropped.
	if _, err := store.Upsert(ctx, "t-9", func(current *Task) (*Task, error) {
		if current != nil {
			t.Fatal("Upsert saw a record for an absent id")
		}
		return &Task{ID: "t-9", Status: TaskStatus{State: TaskStateWorking}}, nil
	}); err != nil {
		t.Fatalf("Upsert t-9: %v", err)
	}
	if _, err := store.Get(ctx, "t-2"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("Get t-2: err = %v, want ErrTaskNotFound (evicted by upsert)", err)
	}
	for _, id := range []string{"t-3", "t-4", "t-9"} {
		if _, err := store.Get(ctx, id); err != nil {
			t.Fatalf("Get %s after upsert: %v", id, err)
		}
	}
}

// TestMemoryTaskStoreDefaultCap: the zero-option store is bounded at
// defaultMaxTasks; the first record is dropped once one more arrives.
func TestMemoryTaskStoreDefaultCap(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryTaskStore()
	for i := 0; i <= defaultMaxTasks; i++ {
		if err := store.Save(ctx, &Task{ID: fmt.Sprintf("t-%d", i)}); err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
	}
	if _, err := store.Get(ctx, "t-0"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("Get t-0: err = %v, want ErrTaskNotFound (default cap evicted it)", err)
	}
	if _, err := store.Get(ctx, fmt.Sprintf("t-%d", defaultMaxTasks)); err != nil {
		t.Fatalf("Get newest: %v", err)
	}
}

// TestMemoryTaskStoreUnlimited: WithMaxTasks(0) lifts the cap for deployments
// that bound memory some other way.
func TestMemoryTaskStoreUnlimited(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryTaskStore(WithMaxTasks(0))
	for i := 0; i < 5; i++ {
		if err := store.Save(ctx, &Task{ID: fmt.Sprintf("t-%d", i)}); err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
	}
	for i := 0; i < 5; i++ {
		if _, err := store.Get(ctx, fmt.Sprintf("t-%d", i)); err != nil {
			t.Fatalf("Get %d: %v", i, err)
		}
	}
}

// TestMemoryTaskStoreUpsert: a missing record is created (mutate receives
// nil), an existing one is updated under the same lock, and errors abort the
// write exactly as in Update.
func TestMemoryTaskStoreUpsert(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryTaskStore()

	if _, err := store.Upsert(ctx, "", func(*Task) (*Task, error) { return &Task{}, nil }); !errors.Is(err, ErrWire) {
		t.Fatalf("empty id: err = %v, want ErrWire", err)
	}
	if _, err := store.Upsert(ctx, "t-1", nil); !errors.Is(err, ErrWire) {
		t.Fatalf("nil mutate: err = %v, want ErrWire", err)
	}

	stored, err := store.Upsert(ctx, "t-1", func(current *Task) (*Task, error) {
		if current != nil {
			t.Fatal("create mutate received a record")
		}
		return &Task{ID: "t-1", Status: TaskStatus{State: TaskStateWorking}}, nil
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if stored.ID != "t-1" || stored.Status.State != TaskStateWorking {
		t.Fatalf("created = %+v", stored)
	}

	stored, err = store.Upsert(ctx, "t-1", func(current *Task) (*Task, error) {
		if current.Status.State != TaskStateWorking {
			t.Fatalf("update mutate saw %q, want working", current.Status.State)
		}
		current.Status = TaskStatus{State: TaskStateCanceled}
		return current, nil
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if stored.Status.State != TaskStateCanceled {
		t.Fatalf("updated = %+v", stored)
	}
	if got, err := store.Get(ctx, "t-1"); err != nil || got.Status.State != TaskStateCanceled {
		t.Fatalf("Get after upsert = %v/%+v", err, got)
	}

	// A mutate error aborts; a nil record from a creating mutate is refused.
	abort := errors.New("abort")
	if _, err := store.Upsert(ctx, "t-1", func(*Task) (*Task, error) { return nil, abort }); !errors.Is(err, abort) {
		t.Fatalf("mutate error: err = %v, want abort", err)
	}
	if _, err := store.Upsert(ctx, "t-2", func(*Task) (*Task, error) { return nil, nil }); !errors.Is(err, ErrWire) {
		t.Fatalf("nil record on create: err = %v, want ErrWire", err)
	}
	if _, err := store.Get(ctx, "t-2"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("aborted create left a record: err = %v", err)
	}

	// The caller's returned copy does not alias the stored record.
	stored.Status = TaskStatus{State: TaskStateWorking}
	if got, _ := store.Get(ctx, "t-1"); got.Status.State != TaskStateCanceled {
		t.Fatalf("returned copy aliases the store: %+v", got)
	}
}

// TestMemoryTaskStoreUpsertConcurrent: overlapping Upserts on one absent id
// serialize, so a create racing updates keeps every writer's entry — the
// first commit is as atomic as any continuation.
func TestMemoryTaskStoreUpsertConcurrent(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryTaskStore()
	const writers = 16
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, err := store.Upsert(ctx, "t-1", func(current *Task) (*Task, error) {
				if current == nil {
					return &Task{ID: "t-1", History: []Message{*NewMessage(RoleUser, NewTextPart(fmt.Sprintf("m%d", n)))}}, nil
				}
				current.History = append(current.History, *NewMessage(RoleUser, NewTextPart(fmt.Sprintf("m%d", n))))
				return current, nil
			})
			if err != nil {
				t.Errorf("Upsert %d: %v", n, err)
			}
		}(i)
	}
	wg.Wait()
	got, err := store.Get(ctx, "t-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.History) != writers {
		t.Fatalf("history len = %d, want %d (concurrent upserts lost entries)", len(got.History), writers)
	}
}
