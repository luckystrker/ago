package task

import (
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
)

// recorder captures every event flowing on a stream, transient or not.
type recorder struct {
	mu   sync.Mutex
	seen []events.Event
}

func subscribe(s stream.Stream) *recorder {
	r := &recorder{}
	s.Subscribe(func(ev events.Event) error {
		r.mu.Lock()
		r.seen = append(r.seen, ev)
		r.mu.Unlock()
		return nil
	}, stream.SubscribeOptions{})
	return r
}

func (r *recorder) events() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]events.Event(nil), r.seen...)
}

func (r *recorder) count(eventType string) int {
	n := 0
	for _, ev := range r.events() {
		if ev.EventType() == eventType {
			n++
		}
	}
	return n
}

func (r *recorder) find(eventType string) events.Event {
	for _, ev := range r.events() {
		if ev.EventType() == eventType {
			return ev
		}
	}
	return nil
}

// countingStore wraps MemoryCheckpointStore counting writes (for no-op checks).
type countingStore struct {
	mem    *MemoryCheckpointStore
	writes int
}

func newCountingStore() *countingStore { return &countingStore{mem: NewMemoryCheckpointStore()} }

func (c *countingStore) Write(taskID string, state []byte) error {
	c.writes++
	return c.mem.Write(taskID, state)
}

func (c *countingStore) Read(taskID string) ([]byte, error) { return c.mem.Read(taskID) }

func newTestContext(t *testing.T) (*stream.MemoryStream, *agocontext.Context) {
	t.Helper()
	s := stream.NewMemoryStream()
	return s, agocontext.New(s)
}

func TestStartEmitsTaskStartedAndStampsDependency(t *testing.T) {
	s, c := newTestContext(t)
	rec := subscribe(s)

	tsk, err := Start(c, "agent-1", Spec{Title: "research X", Description: "dig", Payload: map[string]any{"depth": 2}})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if tsk.State() != StateRunning {
		t.Fatalf("state = %q, want running", tsk.State())
	}
	if tsk.TaskID() == "" {
		t.Fatal("task id empty")
	}
	md := tsk.Metadata()
	if md.OwnerID != "agent-1" || md.Spec.Title != "research X" || md.Spec.Payload["depth"] != 2 {
		t.Fatalf("metadata = %+v", md)
	}
	if md.CreatedAt.IsZero() || md.StartedAt.IsZero() {
		t.Fatal("created/started timestamps not set")
	}
	if md.Progress == nil {
		t.Fatal("progress map nil")
	}
	if dep, ok := c.Dependency(DepKey); !ok || dep != tsk {
		t.Fatalf("dependency not stamped: ok=%v", ok)
	}
	started, ok := rec.find("TaskStarted").(*events.TaskStarted)
	if !ok {
		t.Fatal("TaskStarted not emitted")
	}
	if started.TaskID != tsk.TaskID() || started.AgentName != "agent-1" || started.Objective != "research X" || started.ExpiresAt != "" {
		t.Fatalf("TaskStarted = %+v", started)
	}
	select {
	case <-tsk.Done():
		t.Fatal("done closed while running")
	default:
	}
}

// TestTaskStartedCarriesSpec pins the task-event parity: TaskStarted carries
// the full Spec (Python task.py stamps spec=self._spec, so description, payload
// and capability reach observers) and TaskCompleted carries the usage field on
// the wire, else nothing downstream can see what a task does nor account for
// its tokens.
func TestTaskStartedCarriesSpec(t *testing.T) {
	s, c := newTestContext(t)
	rec := subscribe(s)

	payload := map[string]any{"depth": 2}
	tsk, err := Start(c, "agent-1", Spec{
		Title:       "research X",
		Description: "dig deep",
		Payload:     payload,
		Capability:  "search",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	started, ok := rec.find("TaskStarted").(*events.TaskStarted)
	if !ok {
		t.Fatal("TaskStarted not emitted")
	}
	if started.Spec == nil {
		t.Fatal("TaskStarted.Spec is nil")
	}
	want := events.TaskSpec{Title: "research X", Description: "dig deep",
		Payload: map[string]any{"depth": 2}, Capability: "search"}
	if !reflect.DeepEqual(*started.Spec, want) {
		t.Fatalf("TaskStarted.Spec = %+v, want %+v", *started.Spec, want)
	}
	// The event snapshots the payload: later owner mutations must not rewrite
	// an already-sent event.
	payload["depth"] = 99
	if started.Spec.Payload["depth"] != 2 {
		t.Fatalf("TaskStarted.Spec.Payload tracks the owner map: %+v", started.Spec.Payload)
	}
	// The Task primitive has no parent tool call (Python leaves
	// parent_tool_call_id None on this path).
	if started.ParentToolCallID != "" {
		t.Fatalf("ParentToolCallID = %q, want empty", started.ParentToolCallID)
	}
	data, err := events.MarshalEvent(started)
	if err != nil {
		t.Fatalf("marshal TaskStarted: %v", err)
	}
	var wireStarted map[string]any
	if err := json.Unmarshal(data, &wireStarted); err != nil {
		t.Fatalf("unmarshal TaskStarted: %v", err)
	}
	specWire, ok := wireStarted["spec"].(map[string]any)
	if !ok || specWire["title"] != "research X" || specWire["capability"] != "search" {
		t.Fatalf("TaskStarted wire spec = %v", wireStarted["spec"])
	}
	if specWire["payload"].(map[string]any)["depth"] != float64(2) {
		t.Fatalf("TaskStarted wire spec payload = %v", specWire["payload"])
	}

	if err := tsk.Complete("done"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	done, ok := rec.find("TaskCompleted").(*events.TaskCompleted)
	if !ok {
		t.Fatal("TaskCompleted not emitted")
	}
	data, err = events.MarshalEvent(done)
	if err != nil {
		t.Fatalf("marshal TaskCompleted: %v", err)
	}
	var wireDone map[string]any
	if err := json.Unmarshal(data, &wireDone); err != nil {
		t.Fatalf("unmarshal TaskCompleted: %v", err)
	}
	if _, ok := wireDone["usage"]; !ok {
		t.Fatalf("TaskCompleted wire has no usage key: %s", data)
	}
}

func TestProgressMergesAndEmits(t *testing.T) {
	s, c := newTestContext(t)
	rec := subscribe(s)
	tsk, err := Start(c, "agent", Spec{Title: "job"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	if err := tsk.Progress(map[string]any{"step": "search"}); err != nil {
		t.Fatalf("progress: %v", err)
	}
	if rec.count("TaskProgress") != 1 {
		t.Fatalf("progress events = %d, want 1", rec.count("TaskProgress"))
	}
	pg, ok := rec.find("TaskProgress").(*events.TaskProgress)
	if !ok || pg.Payload["step"] != "search" || pg.TaskID != tsk.TaskID() || pg.AgentName != "agent" || pg.Objective != "job" {
		t.Fatalf("TaskProgress = %+v", pg)
	}
	if tsk.Metadata().LastProgressAt.IsZero() {
		t.Fatal("last_progress_at not set")
	}

	if err := tsk.Progress(map[string]any{"found": 3}); err != nil {
		t.Fatalf("progress 2: %v", err)
	}
	md := tsk.Metadata()
	if md.Progress["step"] != "search" || md.Progress["found"] != 3 {
		t.Fatalf("progress not merged: %+v", md.Progress)
	}
}

func TestCompleteLifecycle(t *testing.T) {
	s, c := newTestContext(t)
	rec := subscribe(s)
	store := newCountingStore()
	tsk, err := Start(c, "agent", Spec{Title: "job"}, WithCheckpointStore(store))
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	if err := tsk.Complete("done"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	md := tsk.Metadata()
	if tsk.State() != StateCompleted || md.Result != "done" || md.CompletedAt.IsZero() {
		t.Fatalf("after complete: state=%q md=%+v", tsk.State(), md)
	}
	done, ok := rec.find("TaskCompleted").(*events.TaskCompleted)
	if !ok {
		t.Fatal("TaskCompleted not emitted")
	}
	if done.TaskID != tsk.TaskID() || done.Result != "done" || done.TaskStream != s.ID() {
		t.Fatalf("TaskCompleted = %+v", done)
	}
	select {
	case <-tsk.Done():
	default:
		t.Fatal("done not closed after terminal state")
	}
	if dep, ok := c.Dependency(DepKey); !ok || dep != nil {
		t.Fatalf("dependency not rolled back: ok=%v dep=%v", ok, dep)
	}

	// Terminal is immutable: further transitions and progress are no-ops.
	before := len(rec.events())
	if err := tsk.Complete("again"); err != nil {
		t.Fatalf("second complete: %v", err)
	}
	if err := tsk.Progress(map[string]any{"late": true}); err != nil {
		t.Fatalf("progress after terminal: %v", err)
	}
	if err := tsk.Checkpoint([]byte("late")); err != nil {
		t.Fatalf("checkpoint after terminal: %v", err)
	}
	if writes := store.writes; writes != 0 {
		t.Fatalf("checkpoint wrote after terminal: %d", writes)
	}
	if got := len(rec.events()); got != before {
		t.Fatalf("terminal no-ops emitted events: %d -> %d", before, got)
	}
}

func TestFail(t *testing.T) {
	_, c := newTestContext(t)
	tsk, err := Start(c, "agent", Spec{Title: "job"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := tsk.Fail(errors.New("boom")); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if tsk.State() != StateFailed {
		t.Fatalf("state = %q", tsk.State())
	}
	if md := tsk.Metadata(); md.Error != "boom" || md.CompletedAt.IsZero() {
		t.Fatalf("metadata = %+v", md)
	}
	select {
	case <-tsk.Done():
	default:
		t.Fatal("done not closed")
	}
	// A nil error must not panic; the record just stays empty.
	tsk2, err := Start(c, "agent", Spec{Title: "job2"})
	if err != nil {
		t.Fatalf("start 2: %v", err)
	}
	if err := tsk2.Fail(nil); err != nil {
		t.Fatalf("fail(nil): %v", err)
	}
	if tsk2.Metadata().Error != "" {
		t.Fatalf("error = %q, want empty", tsk2.Metadata().Error)
	}
}

func TestCancelEventOnOwnStream(t *testing.T) {
	s, c := newTestContext(t)
	rec := subscribe(s)
	tsk, err := Start(c, "agent", Spec{Title: "job"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := tsk.Cancel("user asked"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	cc, ok := rec.find("TaskCancelled").(*events.TaskCancelled)
	if !ok {
		t.Fatal("TaskCancelled not emitted")
	}
	if cc.Reason != "user asked" || cc.TaskID != tsk.TaskID() || cc.AgentName != "agent" {
		t.Fatalf("TaskCancelled = %+v", cc)
	}

	// An empty reason leaves metadata.Error untouched.
	tsk2, err := Start(c, "agent", Spec{Title: "job2"})
	if err != nil {
		t.Fatalf("start 2: %v", err)
	}
	if err := tsk2.Cancel(""); err != nil {
		t.Fatalf("cancel 2: %v", err)
	}
	if md := tsk2.Metadata(); md.Error != "" {
		t.Fatalf("error = %q, want empty for empty reason", md.Error)
	}
}

func TestExpire(t *testing.T) {
	s, c := newTestContext(t)
	rec := subscribe(s)
	tsk, err := Start(c, "agent", Spec{Title: "job"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := tsk.Expire(); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if tsk.State() != StateExpired {
		t.Fatalf("state = %q", tsk.State())
	}
	if _, ok := rec.find("TaskExpired").(*events.TaskExpired); !ok {
		t.Fatal("TaskExpired not emitted")
	}
	select {
	case <-tsk.Done():
	default:
		t.Fatal("done not closed")
	}
}

func TestCheckpointAndResume(t *testing.T) {
	_, c := newTestContext(t)
	store := NewMemoryCheckpointStore()
	first, err := Start(c, "agent", Spec{Title: "first"}, WithCheckpointStore(store))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := first.Checkpoint([]byte(`{"n":1}`)); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := first.Checkpoint([]byte(`{"n":2}`)); err != nil {
		t.Fatalf("checkpoint 2: %v", err)
	}
	got, err := store.Read(first.TaskID())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != `{"n":2}` {
		t.Fatalf("stored = %s", got)
	}

	second, err := Start(c, "agent", Spec{Title: "second"}, WithCheckpointStore(store), WithResumeFrom(first.TaskID()))
	if err != nil {
		t.Fatalf("resume start: %v", err)
	}
	resumed := second.ResumedState()
	if string(resumed) != `{"n":2}` {
		t.Fatalf("resumed = %s", resumed)
	}
	// The returned slice must not alias the store.
	resumed[0] = 'X'
	if string(second.ResumedState()) != `{"n":2}` {
		t.Fatal("ResumedState aliases internal state")
	}
	if err := second.Checkpoint([]byte(`{"n":3}`)); err != nil {
		t.Fatalf("checkpoint on resumed: %v", err)
	}
	if got, _ := store.Read(second.TaskID()); string(got) != `{"n":3}` {
		t.Fatalf("resumed task checkpoint = %s", got)
	}
}

func TestResumeFromUnknownTask(t *testing.T) {
	_, c := newTestContext(t)
	store := NewMemoryCheckpointStore()
	tsk, err := Start(c, "agent", Spec{Title: "t"}, WithCheckpointStore(store), WithResumeFrom("missing-id"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if tsk.ResumedState() != nil {
		t.Fatalf("resumed = %q, want nil", tsk.ResumedState())
	}
}

func TestCheckpointWithoutStoreIsNoop(t *testing.T) {
	_, c := newTestContext(t)
	tsk, err := Start(c, "agent", Spec{Title: "t"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := tsk.Checkpoint([]byte(`{}`)); err != nil {
		t.Fatalf("checkpoint without store: %v", err)
	}
}

func TestCheckpointStoreErrorPropagates(t *testing.T) {
	_, c := newTestContext(t)
	want := errors.New("disk full")
	tsk, err := Start(c, "agent", Spec{Title: "t"}, WithCheckpointStore(failStore{err: want}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := tsk.Checkpoint([]byte(`{}`)); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

type failStore struct{ err error }

func (f failStore) Write(string, []byte) error  { return f.err }
func (f failStore) Read(string) ([]byte, error) { return nil, f.err }

func TestResumeReadErrorPropagates(t *testing.T) {
	_, c := newTestContext(t)
	want := errors.New("disk corrupt")
	_, err := Start(c, "agent", Spec{Title: "t"}, WithCheckpointStore(failStore{err: want}), WithResumeFrom("x"))
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestTerminalStateIsImmutable(t *testing.T) {
	_, c := newTestContext(t)
	tsk, err := Start(c, "agent", Spec{Title: "t"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := tsk.Complete("done"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := tsk.Cancel("late"); err != nil {
		t.Fatalf("cancel after complete: %v", err)
	}
	if err := tsk.Fail(errors.New("late")); err != nil {
		t.Fatalf("fail after complete: %v", err)
	}
	if err := tsk.Expire(); err != nil {
		t.Fatalf("expire after complete: %v", err)
	}
	if tsk.State() != StateCompleted {
		t.Fatalf("state = %q, want completed", tsk.State())
	}
	if md := tsk.Metadata(); md.Error != "" {
		t.Fatalf("error = %q, want empty", md.Error)
	}
}

func TestNilContext(t *testing.T) {
	if _, err := Start(nil, "owner", Spec{Title: "t"}); err == nil {
		t.Fatal("nil context accepted")
	}
}

func TestTaskStartedSendFailure(t *testing.T) {
	s := stream.NewMemoryStream()
	s.Subscribe(func(ev events.Event) error { return errors.New("bus refused") },
		stream.SubscribeOptions{Interrupt: true})
	c := agocontext.New(s)
	tsk, err := Start(c, "owner", Spec{Title: "t"})
	if err == nil {
		t.Fatal("send failure swallowed")
	}
	if tsk != nil {
		t.Fatal("task returned on failed start")
	}
}

func TestWithTTL(t *testing.T) {
	s, c := newTestContext(t)
	rec := subscribe(s)
	tsk, err := Start(c, "agent", Spec{Title: "t"}, WithTTL(90*time.Second))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	md := tsk.Metadata()
	if md.ExpiresAt.IsZero() {
		t.Fatal("expires_at not set")
	}
	if delta := time.Until(md.ExpiresAt); delta < 80*time.Second || delta > 91*time.Second {
		t.Fatalf("expires_at delta = %v, want ~90s", delta)
	}
	started, ok := rec.find("TaskStarted").(*events.TaskStarted)
	if !ok {
		t.Fatal("TaskStarted not emitted")
	}
	if _, err := time.Parse(time.RFC3339, started.ExpiresAt); err != nil {
		t.Fatalf("TaskStarted.expires_at not RFC3339: %q (%v)", started.ExpiresAt, err)
	}

	plain, err := Start(c, "agent", Spec{Title: "plain"})
	if err != nil {
		t.Fatalf("start 2: %v", err)
	}
	if !plain.Metadata().ExpiresAt.IsZero() {
		t.Fatal("expires_at set without TTL")
	}
}

func TestWithChannelID(t *testing.T) {
	_, c := newTestContext(t)
	tsk, err := Start(c, "agent", Spec{Title: "t"}, WithChannelID("ch-7"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if tsk.Metadata().ChannelID != "ch-7" {
		t.Fatalf("channel_id = %q", tsk.Metadata().ChannelID)
	}
}

// TestDurableEventsOnHistory: TaskStarted/TaskCompleted are persisted, the
// transient TaskProgress is not.
func TestDurableEventsOnHistory(t *testing.T) {
	_, c := newTestContext(t)
	tsk, err := Start(c, "agent", Spec{Title: "t"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := tsk.Progress(map[string]any{"step": 1}); err != nil {
		t.Fatalf("progress: %v", err)
	}
	if err := tsk.Complete("done"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	history, err := c.Stream.History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var started, completed, progress int
	for _, ev := range history {
		switch ev.EventType() {
		case "TaskStarted":
			started++
		case "TaskCompleted":
			completed++
		case "TaskProgress":
			progress++
		}
	}
	if started != 1 || completed != 1 || progress != 0 {
		t.Fatalf("history: started=%d completed=%d progress=%d", started, completed, progress)
	}
}

func TestConcurrentTransitions(t *testing.T) {
	_, c := newTestContext(t)
	tsk, err := Start(c, "agent", Spec{Title: "race"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 4 {
			case 0:
				_ = tsk.Progress(map[string]any{"i": i})
			case 1:
				_ = tsk.Complete("winner")
			case 2:
				_ = tsk.Fail(errors.New("loser"))
			case 3:
				_ = tsk.Cancel("nope")
			}
		}(i)
	}
	wg.Wait()
	state := tsk.State()
	if !state.Terminal() {
		t.Fatalf("state = %q, want terminal", state)
	}
	history, _ := c.Stream.History().GetEvents()
	terminal := 0
	for _, ev := range history {
		switch ev.EventType() {
		case "TaskCompleted", "TaskFailed", "TaskExpired", "TaskCancelled":
			terminal++
		}
	}
	if terminal != 1 {
		t.Fatalf("terminal events = %d, want 1", terminal)
	}
	select {
	case <-tsk.Done():
	default:
		t.Fatal("done not closed")
	}
}

func TestZeroTaskMethods(t *testing.T) {
	var tsk Task
	if tsk.State() != StateCreated {
		t.Fatalf("state = %q", tsk.State())
	}
	if tsk.TaskID() != "" {
		t.Fatal("task id on zero task")
	}
	if tsk.ResumedState() != nil {
		t.Fatal("resumed state on zero task")
	}
	if tsk.Done() != nil {
		t.Fatal("done on zero task should be nil")
	}
	md := tsk.Metadata()
	if md.State != StateCreated || md.Progress == nil {
		t.Fatalf("zero metadata = %+v", md)
	}
	for name, err := range map[string]error{
		"progress":   tsk.Progress(nil),
		"complete":   tsk.Complete(nil),
		"fail":       tsk.Fail(nil),
		"cancel":     tsk.Cancel(""),
		"expire":     tsk.Expire(),
		"checkpoint": tsk.Checkpoint(nil),
	} {
		if !errors.Is(err, ErrNotStarted) {
			t.Fatalf("%s: err = %v, want ErrNotStarted", name, err)
		}
	}
}

func TestStateTerminalClassification(t *testing.T) {
	cases := map[State]bool{
		StateCreated:   false,
		StateRunning:   false,
		StateCompleted: true,
		StateFailed:    true,
		StateExpired:   true,
		StateCancelled: true,
	}
	for state, want := range cases {
		if got := state.Terminal(); got != want {
			t.Fatalf("%q.Terminal() = %v, want %v", state, got, want)
		}
	}
}

func TestMetadataJSONRoundTrip(t *testing.T) {
	ts := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	md := Metadata{
		TaskID:         "t-1",
		OwnerID:        "agent",
		Spec:           Spec{Title: "T", Description: "D", Payload: map[string]any{"k": "v"}, Capability: "search"},
		State:          StateRunning,
		CreatedAt:      ts,
		StartedAt:      ts,
		ExpiresAt:      ts.Add(time.Minute),
		LastProgressAt: ts.Add(30 * time.Second),
		Progress:       map[string]any{"step": "a"},
		Result:         "done",
		Error:          "",
		ChannelID:      "ch-1",
	}
	data, err := json.Marshal(md)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("wire: %v", err)
	}
	if wire["task_id"] != "t-1" || wire["state"] != "running" || wire["channel_id"] != "ch-1" {
		t.Fatalf("wire fields = %v", wire)
	}
	if s, _ := wire["started_at"].(string); s == "" {
		t.Fatal("started_at not rendered")
	}

	var back Metadata
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(md, back) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", back, md)
	}
	remarshaled, err := json.Marshal(back)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(remarshaled) != string(data) {
		t.Fatalf("re-marshal differs:\n%s\n%s", remarshaled, data)
	}
}

func TestMetadataZeroFieldsRenderAsPython(t *testing.T) {
	md := Metadata{TaskID: "t", OwnerID: "o", State: StateCreated, Progress: map[string]any{}}
	data, err := json.Marshal(md)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("wire: %v", err)
	}
	if wire["created_at"] != "" {
		t.Fatalf("created_at = %v, want empty string", wire["created_at"])
	}
	for _, key := range []string{"started_at", "completed_at", "expires_at", "last_progress_at", "channel_id"} {
		if v, ok := wire[key]; !ok || v != nil {
			t.Fatalf("%s = %v (present=%v), want null", key, v, ok)
		}
	}
	if wire["error"] != "" || wire["result"] != nil {
		t.Fatalf("error/result = %v/%v", wire["error"], wire["result"])
	}
}

func TestMetadataUnmarshalIsLenient(t *testing.T) {
	in := `{
		"task_id": "t9",
		"state": "running",
		"created_at": "2026-10-29T10:00:00Z",
		"started_at": null,
		"expires_at": "not-a-time",
		"spec": {"title": "Only"},
		"progress": null,
		"result": {"a": 1},
		"channel_id": null
	}`
	var md Metadata
	if err := json.Unmarshal([]byte(in), &md); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if md.TaskID != "t9" || md.State != StateRunning {
		t.Fatalf("md = %+v", md)
	}
	if !md.CreatedAt.Equal(time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("created_at = %v", md.CreatedAt)
	}
	if !md.StartedAt.IsZero() || !md.ExpiresAt.IsZero() {
		t.Fatalf("bad times must collapse to zero: %+v", md)
	}
	if !reflect.DeepEqual(md.Spec, Spec{Title: "Only"}) {
		t.Fatalf("spec = %+v", md.Spec)
	}
	if md.Progress == nil {
		t.Fatal("progress must be non-nil after unmarshal")
	}
	if md.ChannelID != "" {
		t.Fatalf("channel_id = %q", md.ChannelID)
	}
	if m, ok := md.Result.(map[string]any); !ok || m["a"] != float64(1) {
		t.Fatalf("result = %#v", md.Result)
	}

	// Missing state defaults to created; a malformed spec collapses to empty.
	var partial Metadata
	if err := json.Unmarshal([]byte(`{"task_id":"x","spec":5}`), &partial); err != nil {
		t.Fatalf("partial unmarshal: %v", err)
	}
	if partial.State != StateCreated || !reflect.DeepEqual(partial.Spec, Spec{}) || partial.Progress == nil {
		t.Fatalf("partial = %+v", partial)
	}
}

func TestMemoryCheckpointStoreIsolation(t *testing.T) {
	store := NewMemoryCheckpointStore()
	src := []byte(`{"a":1}`)
	if err := store.Write("t", src); err != nil {
		t.Fatalf("write: %v", err)
	}
	src[0] = 'X'
	got, err := store.Read("t")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != `{"a":1}` {
		t.Fatalf("read = %s, write aliases caller buffer", got)
	}
	got[1] = 'Y'
	again, _ := store.Read("t")
	if string(again) != `{"a":1}` {
		t.Fatal("read aliases stored buffer")
	}
	missing, err := store.Read("nope")
	if err != nil || missing != nil {
		t.Fatalf("missing = %q, %v; want nil, nil", missing, err)
	}
	if err := store.Write("t", nil); err != nil {
		t.Fatalf("write nil: %v", err)
	}
	empty, _ := store.Read("t")
	if len(empty) != 0 {
		t.Fatalf("empty = %q", empty)
	}
}
