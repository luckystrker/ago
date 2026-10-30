package a2a

import (
	"context"
	"errors"
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
