package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// plainStore hides MemoryTaskStore behind the bare TaskStore surface: the
// wrapper has no Update, so the server's TaskUpdater assertion cannot match
// and every state transition goes through the per-task-mutex fallback.
type plainStore struct{ inner TaskStore }

func (p plainStore) Save(ctx context.Context, task *Task) error { return p.inner.Save(ctx, task) }
func (p plainStore) Get(ctx context.Context, id string) (*Task, error) {
	return p.inner.Get(ctx, id)
}
func (p plainStore) Delete(ctx context.Context, id string) error {
	return p.inner.Delete(ctx, id)
}

var _ TaskStore = plainStore{}

// TestServerConcurrentContinuationsKeepHistory drives the reviewed race: two
// continuations resume one parked task at the same time. TaskStore.Get hands
// out copies and Save replaces the record, so an unserialized
// read-modify-write loses whichever turn saves second — its history entry and
// status vanish. The commit is atomic and history-merging, so both turns'
// messages stay in the stored history. Runs against both store kinds: the
// TaskUpdater path (MemoryTaskStore) and the per-task-mutex fallback
// (plainStore).
func TestServerConcurrentContinuationsKeepHistory(t *testing.T) {
	t.Run("memory-updater", func(t *testing.T) {
		testConcurrentContinuations(t, NewMemoryTaskStore())
	})
	t.Run("plain-store", func(t *testing.T) {
		testConcurrentContinuations(t, plainStore{inner: NewMemoryTaskStore()})
	})
}

func testConcurrentContinuations(t *testing.T, store TaskStore) {
	t.Helper()
	ctx := context.Background()

	// The results handler releases a turn only once BOTH turns have loaded
	// the task and reached the handler, so the two read-modify-write cycles
	// provably overlap: both reads happen before either write.
	var arrivals sync.WaitGroup
	arrivals.Add(2)
	srv := NewServer(ServerConfig{
		Card:  testCard(false),
		Store: store,
		HandleClientTools: func(_ context.Context, _ *Message, _ []ToolSchema) (*Message, []ToolCall, error) {
			return nil, []ToolCall{{ID: "c1", Name: "calc"}}, nil
		},
		HandleToolResults: func(_ context.Context, _ *Message, _ []ToolResultEntry) (*Message, error) {
			arrivals.Done()
			arrivals.Wait()
			return NewMessage(RoleAgent, NewTextPart("done")), nil
		},
	})
	h := srv.Handler()

	// Park.
	parkBody, err := json.Marshal(map[string]any{
		"message": AttachClientTools(NewMessage(RoleUser, NewTextPart("open")), []ToolSchema{{Name: "calc"}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	env := postRPC(t, h, rpcBody(MethodMessageSend, string(parkBody), `"1"`))
	if env.Error != nil {
		t.Fatalf("park: %v", env.Error)
	}
	var parked Task
	if err := json.Unmarshal(env.Result, &parked); err != nil {
		t.Fatalf("decode parked task: %v (%s)", err, env.Result)
	}
	if parked.Status.State != TaskStateInputRequired {
		t.Fatalf("parked state = %q, want input-required", parked.Status.State)
	}

	// Resume twice, concurrently, with distinguishable result texts.
	var sends sync.WaitGroup
	for n, text := range []string{"r1", "r2"} {
		sends.Add(1)
		go func(n int, text string) {
			defer sends.Done()
			msg := BuildToolResultMessage(parked.ID, parked.ContextID,
				[]ToolResultEntry{{ID: "c1", Name: "calc", Content: text}}, nil)
			body, err := json.Marshal(map[string]any{"message": msg})
			if err != nil {
				t.Error(err)
				return
			}
			resumeEnv := postRPC(t, h, rpcBody(MethodMessageSend, string(body), fmt.Sprintf(`"%d"`, n+10)))
			if resumeEnv.Error != nil {
				t.Errorf("resume %d: %v", n, resumeEnv.Error)
			}
		}(n, text)
	}
	sends.Wait()

	stored, err := store.Get(ctx, parked.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	// The opening message plus BOTH continuations: an unserialized Save
	// would keep only one continuation's message.
	if len(stored.History) != 3 {
		t.Fatalf("history len = %d, want 3 (opening + both continuations)", len(stored.History))
	}
	if textOfMessage(&stored.History[0]) != "open" {
		t.Fatalf("opening history message = %q, want open", textOfMessage(&stored.History[0]))
	}
	// The two resume messages carry tool-result data parts; both results
	// must have survived with their own content.
	contents := map[string]int{}
	for _, m := range stored.History[1:] {
		results, err := ParseToolResults(&m)
		if err != nil || len(results) != 1 {
			t.Fatalf("resume history message %+v: results = %v/%v, want one entry", m, results, err)
		}
		contents[results[0].Content]++
	}
	for _, want := range []string{"r1", "r2"} {
		if contents[want] != 1 {
			t.Fatalf("resume contents = %v, want each of r1/r2 exactly once", contents)
		}
	}
	if stored.Status.State != TaskStateCompleted {
		t.Fatalf("state = %q, want completed", stored.Status.State)
	}
}

// TestServerCancelDuringTurnKeepsCanceled drives the cancel-vs-send race: a
// parked task's resume turn is still running when tasks/cancel lands. The
// cancel wins — the finished turn's commit is refused and the task stays
// canceled; it must not resurrect as completed. message/send answers the
// refused turn with the stored Task record.
func TestServerCancelDuringTurnKeepsCanceled(t *testing.T) {
	t.Run("memory-updater", func(t *testing.T) {
		testCancelDuringTurn(t, NewMemoryTaskStore())
	})
	t.Run("plain-store", func(t *testing.T) {
		testCancelDuringTurn(t, plainStore{inner: NewMemoryTaskStore()})
	})
}

func testCancelDuringTurn(t *testing.T, store TaskStore) {
	t.Helper()
	ctx := context.Background()
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := NewServer(ServerConfig{
		Card:  testCard(false),
		Store: store,
		HandleClientTools: func(_ context.Context, _ *Message, _ []ToolSchema) (*Message, []ToolCall, error) {
			return nil, []ToolCall{{ID: "c1", Name: "calc"}}, nil
		},
		HandleToolResults: func(_ context.Context, _ *Message, _ []ToolResultEntry) (*Message, error) {
			started <- struct{}{}
			<-release // held until the cancel has landed
			return NewMessage(RoleAgent, NewTextPart("late")), nil
		},
	})
	h := srv.Handler()

	// Park.
	parkBody, err := json.Marshal(map[string]any{
		"message": AttachClientTools(NewMessage(RoleUser, NewTextPart("open")), []ToolSchema{{Name: "calc"}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	env := postRPC(t, h, rpcBody(MethodMessageSend, string(parkBody), `"1"`))
	if env.Error != nil {
		t.Fatalf("park: %v", env.Error)
	}
	var parked Task
	if err := json.Unmarshal(env.Result, &parked); err != nil {
		t.Fatalf("decode parked task: %v (%s)", err, env.Result)
	}

	// Resume in the background; the turn parks inside the handler.
	resume := BuildToolResultMessage(parked.ID, parked.ContextID,
		[]ToolResultEntry{{ID: "c1", Name: "calc", Content: "r1"}}, nil)
	resumeBody, err := json.Marshal(map[string]any{"message": resume})
	if err != nil {
		t.Fatal(err)
	}
	var sendWg sync.WaitGroup
	sendWg.Add(1)
	var sendEnv rpcResponse
	go func() {
		defer sendWg.Done()
		sendEnv = postRPC(t, h, rpcBody(MethodMessageSend, string(resumeBody), `"2"`))
	}()
	<-started

	// Cancel while the resume turn is mid-flight.
	cancelBody, err := json.Marshal(map[string]any{"id": parked.ID})
	if err != nil {
		t.Fatal(err)
	}
	cancelEnv := postRPC(t, h, rpcBody(MethodTasksCancel, string(cancelBody), `"3"`))
	if cancelEnv.Error != nil {
		t.Fatalf("cancel mid-turn: %v", cancelEnv.Error)
	}
	var canceled Task
	if err := json.Unmarshal(cancelEnv.Result, &canceled); err != nil {
		t.Fatalf("decode canceled task: %v (%s)", err, cancelEnv.Result)
	}
	if canceled.Status.State != TaskStateCanceled {
		t.Fatalf("canceled state = %q, want canceled", canceled.Status.State)
	}

	close(release)
	sendWg.Wait()

	// The finished turn must not have overwritten the cancel.
	stored, err := store.Get(ctx, parked.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if stored.Status.State != TaskStateCanceled {
		t.Fatalf("state after finished turn = %q, want canceled (task resurrected)", stored.Status.State)
	}
	// The refused turn's outcome is dropped wholesale: no history entry.
	if len(stored.History) != 1 {
		t.Fatalf("history len = %d, want 1 (refused turn leaves the record untouched)", len(stored.History))
	}

	// message/send answers the refused turn with the stored Task record.
	if sendEnv.Error != nil {
		t.Fatalf("refused resume: %v", sendEnv.Error)
	}
	var refusedTask Task
	if err := json.Unmarshal(sendEnv.Result, &refusedTask); err != nil {
		t.Fatalf("refused resume result is not a Task: %v (%s)", err, sendEnv.Result)
	}
	if refusedTask.Status.State != TaskStateCanceled {
		t.Fatalf("refused resume task state = %q, want canceled", refusedTask.Status.State)
	}

	// The canceled record is terminal: a further cancel answers -32002.
	cancelEnv = postRPC(t, h, rpcBody(MethodTasksCancel, string(cancelBody), `"4"`))
	if cancelEnv.Error == nil || cancelEnv.Error.Code != codeTaskNotCancelable {
		t.Fatalf("second cancel = %+v, want code %d", cancelEnv.Error, codeTaskNotCancelable)
	}
}

// TestServerStreamRefusedAfterCancel is the message/stream twin: a cancel
// lands while a streamed resume turn is mid-flight, and the final event
// carries the stored canceled status instead of the dropped turn's reply.
func TestServerStreamRefusedAfterCancel(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := NewServer(ServerConfig{
		Card:  testCard(true),
		Store: NewMemoryTaskStore(),
		HandleClientTools: func(_ context.Context, _ *Message, _ []ToolSchema) (*Message, []ToolCall, error) {
			return nil, []ToolCall{{ID: "c1", Name: "calc"}}, nil
		},
		HandleToolResults: func(_ context.Context, _ *Message, _ []ToolResultEntry) (*Message, error) {
			started <- struct{}{}
			<-release
			return NewMessage(RoleAgent, NewTextPart("late")), nil
		},
	})
	h := srv.Handler()

	// Park.
	parkBody, err := json.Marshal(map[string]any{
		"message": AttachClientTools(NewMessage(RoleUser, NewTextPart("open")), []ToolSchema{{Name: "calc"}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	env := postRPC(t, h, rpcBody(MethodMessageSend, string(parkBody), `"1"`))
	if env.Error != nil {
		t.Fatalf("park: %v", env.Error)
	}
	var parked Task
	if err := json.Unmarshal(env.Result, &parked); err != nil {
		t.Fatalf("decode parked task: %v (%s)", err, env.Result)
	}

	// Cancel from another request while the stream's turn is mid-flight.
	var cancelWg sync.WaitGroup
	cancelWg.Add(1)
	var cancelEnv rpcResponse
	go func() {
		defer cancelWg.Done()
		<-started
		body, err := json.Marshal(map[string]any{"id": parked.ID})
		if err != nil {
			t.Error(err)
			return
		}
		cancelEnv = postRPC(t, h, rpcBody(MethodTasksCancel, string(body), `"3"`))
		close(release)
	}()

	resume := BuildToolResultMessage(parked.ID, parked.ContextID,
		[]ToolResultEntry{{ID: "c1", Name: "calc", Content: "r1"}}, nil)
	req := httptest.NewRequest(http.MethodPost, rpcPath, strings.NewReader(
		rpcBody(MethodMessageStream, `{"message":`+marshalMessage(t, resume)+`}`, `"2"`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	cancelWg.Wait()
	if cancelEnv.Error != nil {
		t.Fatalf("cancel mid-stream: %v", cancelEnv.Error)
	}

	events := decodeSSE(t, rec.Body.String())
	if len(events) != 2 {
		t.Fatalf("streamed %d events, want 2: %s", len(events), rec.Body.String())
	}
	var final StatusUpdate
	if err := json.Unmarshal(events[1].Result, &final); err != nil {
		t.Fatalf("decode final: %v", err)
	}
	if !final.Final || final.Status.State != TaskStateCanceled {
		t.Fatalf("final event = %+v, want final canceled (turn resurrected the cancel)", final.Status)
	}

	stored, err := srv.store.Get(context.Background(), parked.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if stored.Status.State != TaskStateCanceled {
		t.Fatalf("stored state = %q, want canceled", stored.Status.State)
	}
}
