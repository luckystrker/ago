// Package task provides the task lifecycle primitive: a trackable unit of work
// that emits TaskStarted/TaskProgress/TaskCompleted/TaskFailed/TaskExpired/
// TaskCancelled events on the conversation stream so any observer (network
// mirror, watcher, UI, test harness) can follow along, plus owner-defined
// checkpointing for crash recovery.
//
// Tasks are agent-owned: the framework does not assign or schedule them, and
// with no observer subscribed the events fly past harmlessly.
//
// Parity with Python AG2 ag2/task.py. AGO-DEVIATION: Python's Task is an async
// context manager (auto-complete on a clean exit, auto-fail on an exception);
// Go has no exception capture, so the terminal transition is an explicit method
// call and the owner (typically the agent turn scope) chooses which one runs.
// Start performs the __aenter__ side, the first terminal transition performs
// the __aexit__ dependency rollback. See docs/DECISIONS.md.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/internal/uuid"
	"github.com/luckystrker/ago/stream"
)

// DepKey is the context dependency id under which the active Task is stamped
// for the duration of its life (Python parity: TaskInject resolves through it).
// The slot is a single register shared by sibling tasks started on one context:
// it holds the last task that entered and is still running, so a reader inside
// an earlier sibling (TaskInject, network task progress/complete) resolves to
// the latest active task. AGO-DEVIATION: Python uses "ag2.task"; AGo framework
// dependency keys use the reserved "ag:" prefix of the context package.
// See docs/DECISIONS.md.
const DepKey = "ag:task"

// State is the lifecycle state of a Task. Terminal states are immutable.
type State string

const (
	StateCreated   State = "created"
	StateRunning   State = "running"
	StateCompleted State = "completed"
	StateFailed    State = "failed"
	StateExpired   State = "expired"
	StateCancelled State = "cancelled"
)

// Terminal reports whether s is immutable (completed/failed/expired/cancelled).
func (s State) Terminal() bool {
	switch s {
	case StateCompleted, StateFailed, StateExpired, StateCancelled:
		return true
	}
	return false
}

// ErrNotStarted is returned by Task methods called on a task that never went
// through Start (a zero Task).
var ErrNotStarted = errors.New("task: not started")

// Spec describes what a Task is doing. Capability tags the task with a
// capability name from the owning agent's track record; untagged tasks are
// still observed but update no capability stat.
type Spec struct {
	Title       string         `json:"title"`
	Description string         `json:"description,omitempty"`
	Payload     map[string]any `json:"payload,omitempty"`
	Capability  string         `json:"capability,omitempty"`
}

// Metadata is the mutable lifecycle record of a Task, updated on every state
// transition. JSON shape follows the Python field names; times render as
// RFC3339 strings (null when unset, mirroring Python's ISO-string fields).
type Metadata struct {
	TaskID         string
	OwnerID        string
	Spec           Spec
	State          State
	CreatedAt      time.Time
	StartedAt      time.Time
	CompletedAt    time.Time
	ExpiresAt      time.Time
	LastProgressAt time.Time
	Progress       map[string]any
	Result         any
	Error          string
	ChannelID      string // set when a network client mirrors this task
}

func (m Metadata) MarshalJSON() ([]byte, error) {
	spec := m.Spec
	if spec.Payload == nil {
		spec.Payload = map[string]any{}
	}
	progress := m.Progress
	if progress == nil {
		progress = map[string]any{}
	}
	channelID := m.ChannelID
	var channel any
	if channelID != "" {
		channel = channelID
	}
	return json.Marshal(struct {
		TaskID         string         `json:"task_id"`
		OwnerID        string         `json:"owner_id"`
		Spec           Spec           `json:"spec"`
		State          string         `json:"state"`
		CreatedAt      string         `json:"created_at"`
		StartedAt      *string        `json:"started_at"`
		CompletedAt    *string        `json:"completed_at"`
		ExpiresAt      *string        `json:"expires_at"`
		LastProgressAt *string        `json:"last_progress_at"`
		Progress       map[string]any `json:"progress"`
		Result         any            `json:"result"`
		Error          string         `json:"error"`
		ChannelID      any            `json:"channel_id"`
	}{
		TaskID:         m.TaskID,
		OwnerID:        m.OwnerID,
		Spec:           spec,
		State:          string(m.State),
		CreatedAt:      isoOrEmpty(m.CreatedAt),
		StartedAt:      isoPtr(m.StartedAt),
		CompletedAt:    isoPtr(m.CompletedAt),
		ExpiresAt:      isoPtr(m.ExpiresAt),
		LastProgressAt: isoPtr(m.LastProgressAt),
		Progress:       progress,
		Result:         m.Result,
		Error:          m.Error,
		ChannelID:      channel,
	})
}

// UnmarshalJSON rebuilds metadata from persisted JSON. It is defensive against
// partial or legacy payloads (Python from_dict parity): missing keys fall back
// to zero values, a missing state defaults to "created", times that do not
// parse become zero, and a malformed spec collapses to an empty Spec — none of
// these raise.
func (m *Metadata) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*m = Metadata{State: StateCreated, Progress: map[string]any{}}

	var s string
	if fieldString(raw, "task_id", &s) {
		m.TaskID = s
	}
	if fieldString(raw, "owner_id", &s) {
		m.OwnerID = s
	}
	if fieldString(raw, "state", &s) && s != "" {
		m.State = State(s)
	}
	if fieldString(raw, "created_at", &s) {
		m.CreatedAt = parseTimeLenient(s)
	}
	if fieldString(raw, "started_at", &s) {
		m.StartedAt = parseTimeLenient(s)
	}
	if fieldString(raw, "completed_at", &s) {
		m.CompletedAt = parseTimeLenient(s)
	}
	if fieldString(raw, "expires_at", &s) {
		m.ExpiresAt = parseTimeLenient(s)
	}
	if fieldString(raw, "last_progress_at", &s) {
		m.LastProgressAt = parseTimeLenient(s)
	}
	if fieldString(raw, "error", &s) {
		m.Error = s
	}
	if fieldString(raw, "channel_id", &s) {
		m.ChannelID = s
	}
	if r, ok := raw["progress"]; ok && string(r) != "null" {
		var progress map[string]any
		if json.Unmarshal(r, &progress) == nil && progress != nil {
			m.Progress = progress
		}
	}
	if r, ok := raw["result"]; ok && string(r) != "null" {
		var result any
		if json.Unmarshal(r, &result) == nil {
			m.Result = result
		}
	}
	if r, ok := raw["spec"]; ok && string(r) != "null" {
		var spec Spec
		if json.Unmarshal(r, &spec) == nil {
			m.Spec = spec
		}
	}
	return nil
}

func fieldString(raw map[string]json.RawMessage, key string, dst *string) bool {
	r, ok := raw[key]
	if !ok || string(r) == "null" {
		return false
	}
	return json.Unmarshal(r, dst) == nil
}

func isoPtr(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
}

func isoOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

var timeLayouts = []string{
	time.RFC3339Nano, // also parses RFC3339 and Python datetime.isoformat() output
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

func parseTimeLenient(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// Task is the lifecycle handle for a unit of work. Created via Start (the
// Agent.task wrapper delegates here) or via New followed by Start when the
// owner needs the handle before the first event (the run/delegation lifecycle).
// Safe for concurrent use.
type Task struct {
	ctx     *agocontext.Context
	owner   string
	spec    Spec
	opts    options
	store   CheckpointStore
	resumed []byte
	// prevDep is the task the "ag:task" slot held when this task entered,
	// resolved past already-exited siblings (Python parity: _previous_dep +
	// _nearest_active). It is written once in Start before the task is
	// published to the slot, and every reader reached the task through the
	// context dependency, whose mutex orders that write before any read — no
	// extra lock.
	prevDep *Task

	mu       sync.Mutex
	md       Metadata
	started  bool
	done     chan struct{}
	doneOnce sync.Once
	// sub observes the stream's terminal Task events so an external terminal
	// (for example an observer-driven TaskExpired) is adopted instead of being
	// contradicted by a late owner transition (upstream _observe_terminal).
	sub stream.SubID
}

type options struct {
	ttl              time.Duration
	store            CheckpointStore
	resumeFrom       string
	channelID        string
	taskID           string
	parentToolCallID string
	publisher        func(events.Event) error
	noInject         bool
}

// Option configures Start.
type Option func(*options)

// WithTTL sets the task's expiry deadline. It only populates metadata and the
// TaskStarted event: an external TTL observer calls Expire when it elapses
// (Python parity: no internal timer).
func WithTTL(ttl time.Duration) Option {
	return func(o *options) { o.ttl = ttl }
}

// WithCheckpointStore wires the store persisting owner-defined resume state.
func WithCheckpointStore(store CheckpointStore) Option {
	return func(o *options) {
		if store != nil {
			o.store = store
		}
	}
}

// WithResumeFrom reads the checkpoint of a prior task at start so the owner can
// pick up mid-flow after a restart (see ResumedState).
func WithResumeFrom(taskID string) Option {
	return func(o *options) { o.resumeFrom = taskID }
}

// WithChannelID associates the task with a network channel (mirror bookkeeping).
func WithChannelID(id string) Option {
	return func(o *options) { o.channelID = id }
}

// WithTaskID assigns an external task id instead of a fresh uuid. The run and
// delegation lifecycle supplies the id it already published to the caller
// (upstream `_start(task_id=...)`).
func WithTaskID(id string) Option {
	return func(o *options) { o.taskID = id }
}

// WithParentToolCallID records the tool call that requested the task; it rides
// on TaskStarted so observers can reconstruct the delegation tree.
func WithParentToolCallID(id string) Option {
	return func(o *options) { o.parentToolCallID = id }
}

// WithPublisher routes every lifecycle event through fn instead of the bound
// stream. A delegation rejected before it entered its stream publishes straight
// to the parent this way, leaving the (possibly busy) child stream untouched
// (upstream RunLifecycle(publisher=forwarder.forward)).
func WithPublisher(fn func(events.Event) error) Option {
	return func(o *options) {
		if fn != nil {
			o.publisher = fn
		}
	}
}

// WithoutInjection keeps the task out of the "ag:task" dependency slot. Model
// runs and delegations use it: they report a lifecycle but do not steal the
// enclosing TaskInject binding (upstream RunLifecycle starts its Task with
// inject=False).
func WithoutInjection() Option {
	return func(o *options) { o.noInject = true }
}

// New builds an unstarted Task handle over ctx. The caller drives it with Start;
// this is the direct-construction path ADR 0034 replaces Agent.task with. A nil
// context is reported by Start, not by New, so callers can keep the handle.
func New(ctx *agocontext.Context, owner string, spec Spec, opts ...Option) *Task {
	var o options
	for _, apply := range opts {
		apply(&o)
	}
	return &Task{ctx: ctx, owner: owner, spec: spec, opts: o, store: o.store, done: make(chan struct{})}
}

// Start opens a task: stamps "ag:task" into the context dependencies (keeping
// the nearest still-running predecessor for rollback, unless WithoutInjection
// was set), emits TaskStarted and leaves the task in the running state. The
// context is required — standalone streams without a conversation should build
// a Context over a fresh stream first.
//
// A failed TaskStarted send leaves the handle in the running state: the owner
// can still publish a terminal through it, so a storage failure cannot swallow
// the task's outcome (upstream runs the same finally on a failed _start).
func (t *Task) Start() error {
	if t == nil || t.ctx == nil {
		return errors.New("task: Start requires a conversation context")
	}
	t.mu.Lock()
	if t.started {
		t.mu.Unlock()
		return errors.New("task: already started")
	}
	t.mu.Unlock()

	if t.opts.resumeFrom != "" && t.store != nil {
		state, err := t.store.Read(t.opts.resumeFrom)
		if err != nil {
			return fmt.Errorf("task: read checkpoint %q: %w", t.opts.resumeFrom, err)
		}
		t.resumed = state
	}

	now := time.Now().UTC()
	id := t.opts.taskID
	if id == "" {
		id = uuid.New()
	}
	t.mu.Lock()
	t.md = Metadata{
		TaskID:    id,
		OwnerID:   t.owner,
		Spec:      t.spec,
		State:     StateRunning,
		CreatedAt: now,
		StartedAt: now,
		ExpiresAt: expiry(t.opts.ttl, now),
		Progress:  map[string]any{},
		ChannelID: t.opts.channelID,
	}
	t.started = true
	t.mu.Unlock()

	// The "ag:task" slot is a single register shared by sibling tasks on one
	// context: entering keeps the last still-running predecessor, walking the
	// chain past already-exited siblings, so a reader inside an earlier sibling
	// resolves to the latest active task (Python parity: __aenter__ with
	// _nearest_active). A non-*Task slot value counts as absent — the slot is
	// reserved for the framework (upstream types it Task | None).
	if !t.opts.noInject {
		if prev, ok := t.ctx.Dependency(DepKey); ok {
			if pt, isTask := prev.(*Task); isTask {
				t.prevDep = nearestActive(pt)
			}
		}
		t.ctx.SetDependency(DepKey, t)
	}

	if t.ctx.Stream != nil {
		id := t.ctx.Stream.Subscribe(func(ev events.Event) error {
			t.observeTerminal(ev)
			return nil
		}, stream.SubscribeOptions{Condition: terminalTaskCondition})
		t.mu.Lock()
		t.sub = id
		t.mu.Unlock()
	}

	if err := t.send(&events.TaskStarted{
		TaskID:           t.md.TaskID,
		AgentName:        t.owner,
		Objective:        t.spec.Title,
		Spec:             specEvent(t.spec),
		ExpiresAt:        isoOrEmpty(t.md.ExpiresAt),
		ParentToolCallID: t.opts.parentToolCallID,
	}); err != nil {
		t.restoreDep()
		t.unsubscribe()
		return fmt.Errorf("task: send TaskStarted: %w", err)
	}
	return nil
}

// Start builds and starts a task in one call (the Agent.task path). It returns a
// nil handle on failure, preserving the pre-New contract; use New followed by
// (*Task).Start when the handle is needed even after a failed start.
func Start(ctx *agocontext.Context, owner string, spec Spec, opts ...Option) (*Task, error) {
	t := New(ctx, owner, spec, opts...)
	if err := t.Start(); err != nil {
		return nil, err
	}
	return t, nil
}

// specEvent mirrors the owner's Spec onto the TaskStarted wire shape. The
// payload map is shallow-copied so later owner mutations cannot rewrite an
// already-sent event, and a nil payload serialises as the empty dict Python's
// default_factory produces.
func specEvent(spec Spec) *events.TaskSpec {
	payload := make(map[string]any, len(spec.Payload))
	for k, v := range spec.Payload {
		payload[k] = v
	}
	return &events.TaskSpec{
		Title:       spec.Title,
		Description: spec.Description,
		Payload:     payload,
		Capability:  spec.Capability,
	}
}

func expiry(ttl time.Duration, now time.Time) time.Time {
	if ttl <= 0 {
		return time.Time{}
	}
	return now.Add(ttl)
}

// --- read accessors ---------------------------------------------------------

// TaskID returns the task id ("" on a task that never started).
func (t *Task) TaskID() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.started {
		return ""
	}
	return t.md.TaskID
}

// State returns the lifecycle state (StateCreated on a task that never started).
func (t *Task) State() State {
	if t == nil {
		return StateCreated
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.started {
		return StateCreated
	}
	return t.md.State
}

// Metadata returns a snapshot copy of the lifecycle record.
func (t *Task) Metadata() Metadata {
	if t == nil {
		return Metadata{State: StateCreated, Progress: map[string]any{}}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.started {
		return Metadata{State: StateCreated, Progress: map[string]any{}}
	}
	return copyMetadata(&t.md)
}

// ResumedState returns the checkpoint loaded for the WithResumeFrom task, nil
// when there was no resume target, no store, or no checkpoint on file. The
// value is the exact bytes the prior owner passed to Checkpoint.
func (t *Task) ResumedState() []byte {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.resumed == nil {
		return nil
	}
	out := make([]byte, len(t.resumed))
	copy(out, t.resumed)
	return out
}

// Done is closed when the task reaches a terminal state. A handle built by New
// but never started never closes it; a nil task returns nil.
func (t *Task) Done() <-chan struct{} {
	if t == nil {
		return nil
	}
	return t.done
}

// --- lifecycle --------------------------------------------------------------

// Progress merges update into the metadata progress record and emits
// TaskProgress. No-op (nil error) when the task is already terminal.
func (t *Task) Progress(update map[string]any) error {
	t.mu.Lock()
	if !t.started {
		t.mu.Unlock()
		return ErrNotStarted
	}
	if t.md.State.Terminal() {
		t.mu.Unlock()
		return nil
	}
	if t.md.Progress == nil {
		t.md.Progress = map[string]any{}
	}
	for k, v := range update {
		t.md.Progress[k] = v
	}
	t.md.LastProgressAt = time.Now().UTC()
	payload := make(map[string]any, len(update))
	for k, v := range update {
		payload[k] = v
	}
	md := copyMetadata(&t.md)
	t.mu.Unlock()

	return t.send(&events.TaskProgress{
		TaskID:    md.TaskID,
		AgentName: md.OwnerID,
		Objective: md.Spec.Title,
		Payload:   payload,
	})
}

// Complete closes the task successfully with result (nil = no result) and
// emits TaskCompleted. No-op when already terminal.
func (t *Task) Complete(result any) error {
	return t.complete(result, events.Usage{})
}

// CompleteWithUsage is Complete plus the cumulative usage snapshot the run and
// delegation lifecycle attaches to TaskCompleted. The snapshot is inspection
// data, never an additive usage record (source UsageEvents already travelled as
// themselves).
func (t *Task) CompleteWithUsage(result any, usage events.Usage) error {
	return t.complete(result, usage)
}

func (t *Task) complete(result any, usage events.Usage) error {
	md, out := t.transition(StateCompleted, func(m *Metadata) { m.Result = result })
	if out != transitioned {
		return out.err()
	}
	return t.send(&events.TaskCompleted{
		TaskID:     md.TaskID,
		AgentName:  md.OwnerID,
		Objective:  md.Spec.Title,
		Result:     resultString(result),
		TaskStream: t.streamID(),
		Usage:      usage,
	})
}

// Fail closes the task with an error and emits TaskFailed. No-op when already
// terminal.
func (t *Task) Fail(err error) error {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	md, out := t.transition(StateFailed, func(m *Metadata) { m.Error = msg })
	if out != transitioned {
		return out.err()
	}
	return t.send(&events.TaskFailed{
		TaskID:    md.TaskID,
		AgentName: md.OwnerID,
		Objective: md.Spec.Title,
		Error:     msg,
	})
}

// Expire closes a task past its TTL and emits TaskExpired; called by an
// external TTL observer. No-op when already terminal.
func (t *Task) Expire() error {
	md, out := t.transition(StateExpired, nil)
	if out != transitioned {
		return out.err()
	}
	return t.send(&events.TaskExpired{
		TaskID:    md.TaskID,
		AgentName: md.OwnerID,
		Objective: md.Spec.Title,
	})
}

// Cancel closes the task on request and emits TaskCancelled. Reason is a
// free-form diagnostic also recorded in metadata.Error. No-op when already
// terminal.
func (t *Task) Cancel(reason string) error {
	md, out := t.transition(StateCancelled, func(m *Metadata) {
		if reason != "" {
			m.Error = reason
		}
	})
	if out != transitioned {
		return out.err()
	}
	return t.send(&events.TaskCancelled{
		TaskID:    md.TaskID,
		AgentName: md.OwnerID,
		Objective: md.Spec.Title,
		Reason:    reason,
	})
}

// Checkpoint persists owner-defined resume state via the configured store.
// Distinct from Progress: a checkpoint is a crash-recovery snapshot read back
// through WithResumeFrom on the next run. Silent no-op without a store or when
// the task is already terminal.
func (t *Task) Checkpoint(state []byte) error {
	t.mu.Lock()
	if !t.started {
		t.mu.Unlock()
		return ErrNotStarted
	}
	if t.md.State.Terminal() || t.store == nil {
		t.mu.Unlock()
		return nil
	}
	id := t.md.TaskID
	t.mu.Unlock()
	if err := t.store.Write(id, state); err != nil {
		return fmt.Errorf("task: write checkpoint: %w", err)
	}
	return nil
}

type transitionOutcome int

const (
	transitioned transitionOutcome = iota
	alreadyTerminal
	neverStarted
)

func (o transitionOutcome) err() error {
	if o == neverStarted {
		return ErrNotStarted
	}
	return nil
}

// transition moves the task to a terminal state exactly once: it stamps the
// state and CompletedAt, closes Done and rolls the "ag:task" dependency back.
// A task already terminal (or never started) is left untouched.
func (t *Task) transition(state State, apply func(*Metadata)) (Metadata, transitionOutcome) {
	t.mu.Lock()
	if !t.started {
		t.mu.Unlock()
		return Metadata{}, neverStarted
	}
	if t.md.State.Terminal() {
		t.mu.Unlock()
		return Metadata{}, alreadyTerminal
	}
	t.md.State = state
	t.md.CompletedAt = time.Now().UTC()
	if apply != nil {
		apply(&t.md)
	}
	t.closeDone()
	snapshot := copyMetadata(&t.md)
	t.mu.Unlock()
	t.unsubscribe()
	t.restoreDep()
	return snapshot, transitioned
}

// terminalTaskCondition matches the four immutable terminals of any Task on the
// stream; observeTerminal filters by task id and current state.
func terminalTaskCondition(ev events.Event) bool {
	switch events.AsValue(ev).(type) {
	case events.TaskCompleted, events.TaskFailed, events.TaskExpired, events.TaskCancelled:
		return true
	}
	return false
}

// observeTerminal adopts an externally published terminal for this task so a
// late owner transition cannot contradict it (upstream _observe_terminal). It
// never emits a terminal of its own.
func (t *Task) observeTerminal(ev events.Event) {
	if t == nil {
		return
	}
	var taskID string
	var apply func(*Metadata)
	switch e := events.AsValue(ev).(type) {
	case events.TaskCompleted:
		taskID = e.TaskID
		apply = func(m *Metadata) { m.State = StateCompleted; m.Result = e.Result }
	case events.TaskFailed:
		taskID = e.TaskID
		apply = func(m *Metadata) { m.State = StateFailed; m.Error = e.Error }
	case events.TaskExpired:
		taskID = e.TaskID
		apply = func(m *Metadata) { m.State = StateExpired }
	case events.TaskCancelled:
		taskID = e.TaskID
		apply = func(m *Metadata) { m.State = StateCancelled; m.Error = e.Reason }
	default:
		return
	}
	t.mu.Lock()
	if !t.started || taskID != t.md.TaskID || t.md.State.Terminal() {
		t.mu.Unlock()
		return
	}
	apply(&t.md)
	t.md.CompletedAt = time.Now().UTC()
	t.closeDone()
	t.mu.Unlock()
	t.unsubscribe()
	t.restoreDep()
}

// closeDone closes Done exactly once; the caller holds t.mu (or is the only
// writer), and sync.Once keeps the transition and adoption paths from racing.
func (t *Task) closeDone() {
	t.doneOnce.Do(func() {
		if t.done != nil {
			close(t.done)
		}
	})
}

// unsubscribe releases the terminal-observation subscription, if any.
func (t *Task) unsubscribe() {
	t.mu.Lock()
	id := t.sub
	t.sub = 0
	t.mu.Unlock()
	if id != 0 && t.ctx != nil && t.ctx.Stream != nil {
		t.ctx.Stream.Unsubscribe(id)
	}
}

// restoreDep rolls the "ag:task" dependency back — the role of the Python
// context manager's __aexit__. The slot is touched only while this task still
// owns it: when a later sibling entered afterwards, the slot keeps that sibling
// (Python parity: dependencies.get(_TASK_DEP_KEY) is self). Otherwise the slot
// is restored to the nearest still-running predecessor, walking the chain past
// exited tasks so later entries keep resolving. AGO-DEVIATION: the context
// package cannot delete keys, so with no predecessor the key is set to nil;
// readers must treat a nil dependency value as absent.
func (t *Task) restoreDep() {
	if t.ctx == nil {
		return
	}
	if cur, ok := t.ctx.Dependency(DepKey); !ok || cur != any(t) {
		return
	}
	if next := nearestActive(t.prevDep); next != nil {
		t.ctx.SetDependency(DepKey, next)
		return
	}
	// Untyped nil, not a typed (*Task)(nil): the deviation contract is that a
	// nil slot value reads as absent, and a typed nil would fail `dep != nil`.
	t.ctx.SetDependency(DepKey, nil)
}

// nearestActive walks the previous-task chain past tasks that already reached a
// terminal state (Python parity: _nearest_active, where _exited is the first
// terminal transition — the Go analogue of __aexit__, see the package doc on
// the context-manager deviation).
func nearestActive(t *Task) *Task {
	for t != nil && t.State().Terminal() {
		t = t.prevDep
	}
	return t
}

func (t *Task) send(ev events.Event) error {
	if t.opts.publisher != nil {
		if err := t.opts.publisher(ev); err != nil {
			return fmt.Errorf("task: send %s: %w", ev.EventType(), err)
		}
		return nil
	}
	if t.ctx == nil {
		return errors.New("task: no context bound")
	}
	if err := t.ctx.Send(ev); err != nil {
		return fmt.Errorf("task: send %s: %w", ev.EventType(), err)
	}
	return nil
}

func (t *Task) streamID() string {
	if t.ctx == nil || t.ctx.Stream == nil {
		return ""
	}
	return t.ctx.Stream.ID()
}

// resultString renders the result for the TaskCompleted event; the full value
// lives in Metadata.Result. AGO-DEVIATION: Python's TaskCompleted.result is
// Any, the AGo event vocabulary carries a string, so non-string results are
// JSON-encoded here.
func resultString(result any) string {
	switch v := result.(type) {
	case nil:
		return ""
	case string:
		return v
	}
	b, err := json.Marshal(result)
	if err != nil {
		return fmt.Sprintf("%v", result)
	}
	return string(b)
}

func copyMetadata(m *Metadata) Metadata {
	out := *m
	out.Progress = make(map[string]any, len(m.Progress))
	for k, v := range m.Progress {
		out.Progress[k] = v
	}
	if m.Spec.Payload != nil {
		payload := make(map[string]any, len(m.Spec.Payload))
		for k, v := range m.Spec.Payload {
			payload[k] = v
		}
		out.Spec.Payload = payload
	}
	return out
}
