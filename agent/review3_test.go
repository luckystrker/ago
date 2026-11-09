package agent

import (
	stdcontext "context"
	"errors"
	"testing"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
	agotest "github.com/luckystrker/ago/testing"
)

// failTaskStartedStorage rejects the first write that contains TaskStarted.
type failTaskStartedStorage struct{ stream.Storage }

func (s failTaskStartedStorage) SetHistory(id string, evs []events.Event) error {
	for _, ev := range evs {
		if _, ok := events.AsValue(ev).(events.TaskStarted); ok {
			return errors.New("storage down on TaskStarted")
		}
	}
	return s.Storage.SetHistory(id, evs)
}

// brokenReadStorage fails every transcript read; sends reach the relay first
// and then fail at persistence.
type brokenReadStorage struct{ stream.Storage }

func (s brokenReadStorage) GetHistory(string) ([]events.Event, error) {
	return nil, errors.New("storage read down")
}

// TestNestedSameStreamDelegationDoesNotHang pins the re-entry guard: a child
// configured with its caller's stream is rejected with ErrNestedSameStream
// instead of blocking forever on the non-reentrant per-stream lock.
func TestNestedSameStreamDelegationDoesNotHang(t *testing.T) {
	shared := stream.NewMemoryStream()
	child := New("child", "", WithConfig(agotest.NewTestConfig(textResp("never"))), WithStreamBase(shared))
	root := New("root", "", WithConfig(agotest.NewTestConfig(
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{{
			ID: "a", Name: "task_child", Arguments: `{"objective":"x"}`,
		}}}},
		textResp("root done"),
	)), WithStreamBase(shared), WithTools(child.AsTool(WithToolName("task_child"))))

	done := make(chan struct{})
	var reply *AgentReply
	var err error
	go func() {
		defer close(done)
		reply, err = root.Ask(stdcontext.Background(), "go")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("nested same-stream delegation hung on the turn lock")
	}
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if reply.Body() != "root done" {
		t.Fatalf("body = %q", reply.Body())
	}
}

// TestNestedSameStreamDetectsAncestorStreams pins the context chain: a
// grandchild reusing a stream held higher in the delegation call chain is
// rejected even when its immediate parent uses a different stream.
func TestNestedSameStreamDetectsAncestorStreams(t *testing.T) {
	rootStream := stream.NewMemoryStream(stream.WithStreamID("root-held"))
	middleStream := stream.NewMemoryStream(stream.WithStreamID("middle"))
	parent := agocontext.New(middleStream)
	parent.SetDependency(heldStreamsDependencyKey, []string{rootStream.ID()})
	child := New("grandchild", "", WithConfig(agotest.NewTestConfig(textResp("never"))),
		WithStreamBase(rootStream))

	res, err := runTaskOn(stdcontext.Background(), parent, child, "work", "")
	if err != nil {
		t.Fatalf("runTaskOn returned a turn-ending error: %v", err)
	}
	if !errors.Is(res.err, ErrNestedSameStream) {
		t.Fatalf("result error = %v, want ErrNestedSameStream", res.err)
	}
}

// TestAskWaitingOnTurnLockHonorsContext pins the context-aware acquire: a
// caller queued behind a busy stream returns its deadline instead of parking.
func TestAskWaitingOnTurnLockHonorsContext(t *testing.T) {
	g := &gatedConfig{release: make(chan struct{}), entered: make(chan struct{}, 1)}
	base := stream.NewMemoryStream()
	a := New("a", "", WithConfig(g), WithStreamBase(base))
	first, err := a.Run(stdcontext.Background(), "first")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := first.Start(stdcontext.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-g.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first turn never reached the model call")
	}
	ctx, cancel := stdcontext.WithTimeout(stdcontext.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = a.Ask(ctx, "second")
	if !errors.Is(err, stdcontext.DeadlineExceeded) {
		t.Fatalf("queued Ask = %v, want deadline", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("queued Ask took %v", elapsed)
	}
	close(g.release)
	if _, err := first.Result(stdcontext.Background()); err != nil {
		t.Fatalf("first result: %v", err)
	}
	first.Close()
}

// TestStartedNotDuplicatedWhenChildPersistFails pins upstream started=True
// before Send: the relay forwards TaskStarted, then a failing child storage
// returns an error; fail must not publish the opening event a second time.
func TestStartedNotDuplicatedWhenChildPersistFails(t *testing.T) {
	parent := agocontext.New(stream.NewMemoryStream())
	childStream := stream.NewMemoryStream(stream.WithStorage(failTaskStartedStorage{stream.NewMemoryStorage()}))
	child := New("child", "", WithConfig(agotest.NewTestConfig(textResp("never"))), WithStreamBase(childStream))
	res, err := runTaskOn(stdcontext.Background(), parent, child, "work", "")
	if err != nil {
		t.Fatalf("runTaskOn returned a turn-ending error: %v", err)
	}
	if res.completed || res.err == nil {
		t.Fatalf("result = %+v, want failure", res)
	}
	evs, _ := parent.Stream.History().GetEvents()
	var started, failed int
	for _, ev := range evs {
		switch events.AsValue(ev).(type) {
		case events.TaskStarted:
			started++
		case events.TaskFailed:
			failed++
		}
	}
	if started != 1 || failed != 1 {
		t.Fatalf("lifecycle = started %d failed %d, want 1/1", started, failed)
	}
}

// TestTerminalIsLastEventOnCancellation pins the forwarder-finally ordering:
// awaitTurn settles the turn goroutine while the relay and lock are still held,
// the terminal is published through that live relay, and only then is the scope
// closed; the terminal is the last event the parent observes.
func TestTerminalIsLastEventOnCancellation(t *testing.T) {
	var n int64
	parent := agocontext.New(stream.NewMemoryStream())
	childStream := stream.NewMemoryStream()
	child := New("child", "", WithConfig(slowLateConfig{n: &n}), WithStreamBase(childStream))
	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	res, err := runTaskOn(ctx, parent, child, "work", "")
	if err != nil || !res.cancelled {
		t.Fatalf("cancelled result = %+v %v", res, err)
	}
	evs, _ := parent.Stream.History().GetEvents()
	terminal := -1
	for i, ev := range evs {
		if _, ok := events.AsValue(ev).(events.TaskCancelled); ok {
			terminal = i
		}
	}
	if terminal < 0 {
		t.Fatal("TaskCancelled not published")
	}
	for _, ev := range evs[terminal+1:] {
		switch events.AsValue(ev).(type) {
		case events.ChildEvent, events.TransientChildEvent:
			t.Fatalf("child event after terminal: %T", ev)
		}
	}
}

// TestSnapshotUsageFallsBackToAccounting pins the storage-error branch: when
// the child history cannot be read, the lifecycle's terminal usage snapshot
// still includes the source usage the relay retained.
func TestSnapshotUsageFallsBackToAccounting(t *testing.T) {
	parent := agocontext.New(stream.NewMemoryStream())
	childStream := stream.NewMemoryStream(stream.WithStorage(brokenReadStorage{stream.NewMemoryStorage()}))
	f := newForwarder(parent, childStream, "t", "child")
	f.start()
	defer f.stop()

	src := events.NewUsageEvent(events.Usage{TotalTokens: events.I64(10)}, events.UsageKindModelCall)
	if err := childStream.Send(src); err == nil {
		t.Fatal("child send succeeded despite the broken storage")
	}
	got := snapshotUsage(childStream, f.accountingEvents()).TotalTokens
	if got == nil || *got != 10 {
		t.Fatalf("child usage = %v, want 10 from accounting", got)
	}
}

// TestForwarderRejectPublishesBeforeScopeOpens pins the pre-turn reject path:
// a delegation that never installed a relay still publishes started/terminal
// straight to the parent as observations (upstream Forwarder.reject).
func TestForwarderRejectPublishesBeforeScopeOpens(t *testing.T) {
	parent := agocontext.New(stream.NewMemoryStream())
	childStream := stream.NewMemoryStream()
	f := newForwarder(parent, childStream, "t", "child")
	if err := f.reject(&events.ModelResponse{Message: &events.ModelMessage{Content: "early"}}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	parentEvents, _ := parent.Stream.History().GetEvents()
	if len(parentEvents) != 1 {
		t.Fatalf("parent events = %d, want one ChildEvent", len(parentEvents))
	}
	if _, ok := events.AsValue(parentEvents[0]).(events.ChildEvent); !ok {
		t.Fatalf("parent event = %T, want ChildEvent", parentEvents[0])
	}
	if childEvents, _ := childStream.History().GetEvents(); len(childEvents) != 0 {
		t.Fatalf("child stream persisted the event locally: %d", len(childEvents))
	}
}
