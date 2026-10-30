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
