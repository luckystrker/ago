package network

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/task"
)

// Task store + task mirror port task_mirror.py: tasks are created and
// driven by the agent itself; the network is one observer of them. The
// mirror subscribes to the agent's Task* stream events and forwards the
// corresponding task.Metadata updates into the hub's store.

// ErrTaskNotFound is returned by Hub.Task for an unknown task id
// (Python NotFoundError parity).
var ErrTaskNotFound = errors.New("network: task not found")

// ObserveTask registers a task observed via an agent's stream: the hub
// does not create, assign or cancel — it stores the metadata and starts
// TTL accounting (expires_at rides in from the agent-side TaskStarted).
// Re-observing an id updates in place (owner re-emitting TaskStarted on a
// retry) and is never capped; a *new* id is refused with ErrAccessDenied when
// its owner already holds Rule.MaxConcurrentTasks non-terminal tasks (Python
// parity: core.py observe_task). AGO-DEVIATION vs Python: no persistence —
// the store is in-memory. Safe for concurrent use.
func (h *Hub) ObserveTask(md task.Metadata) error {
	if md.TaskID == "" {
		return fmt.Errorf("%w: empty task id", ErrProtocol)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	existing, seen := h.tasks[md.TaskID]
	if seen {
		if md.State == "" {
			md.State = existing.State
		}
		if md.OwnerID == "" {
			md.OwnerID = existing.OwnerID
		}
		if md.StartedAt.IsZero() {
			md.StartedAt = existing.StartedAt
		}
		if md.ExpiresAt.IsZero() {
			md.ExpiresAt = existing.ExpiresAt
		}
		if md.ChannelID == "" {
			md.ChannelID = existing.ChannelID
		}
		if md.Progress == nil {
			md.Progress = existing.Progress
		}
	} else if cap := h.rules[md.OwnerID].MaxConcurrentTasks; cap > 0 {
		active := 0
		for _, t := range h.tasks {
			if t.OwnerID == md.OwnerID && !t.State.Terminal() {
				active++
			}
		}
		if active >= cap {
			return fmt.Errorf("%w: owner %q exceeded max_concurrent_tasks (%d >= %d)",
				ErrAccessDenied, md.OwnerID, active, cap)
		}
	}
	if md.State == "" {
		md.State = task.StateCreated
	}
	if md.Progress == nil {
		md.Progress = map[string]any{}
	}
	h.tasks[md.TaskID] = cloneTaskMetadata(md)
	return nil
}

// UpdateTask patches one observed task: a zero state keeps the stored one,
// progress entries merge, a non-nil result replaces, an empty error keeps
// the stored one. Unknown task ids fail with ErrTaskNotFound (Python
// parity: the mirror swallows that — progress for a task the hub never saw
// is dropped).
func (h *Hub) UpdateTask(taskID string, state task.State, progress map[string]any, result any, errMsg string) error {
	var evs hubEvents
	defer h.flushEvents(&evs) // runs after the unlock below
	h.mu.Lock()
	defer h.mu.Unlock()
	md, ok := h.tasks[taskID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrTaskNotFound, taskID)
	}
	if md.State.Terminal() {
		// Terminal states are immutable (Python parity): a late event for a
		// task the hub already expired or completed changes nothing.
		return nil
	}
	if state != "" {
		md.State = state
	}
	if len(progress) > 0 {
		if md.Progress == nil {
			md.Progress = map[string]any{}
		}
		for k, v := range progress {
			md.Progress[k] = v
		}
		md.LastProgressAt = h.now()
	}
	if result != nil {
		md.Result = result
	}
	if errMsg != "" {
		md.Error = errMsg
	}
	if state.Terminal() {
		md.CompletedAt = h.now()
		delete(h.ext.peerCancel, taskID)
		h.queueTaskTerminal(&evs, md, errMsg)
	}
	h.tasks[taskID] = cloneTaskMetadata(md)
	return nil
}

// Task returns a snapshot copy of one observed task's metadata.
func (h *Hub) Task(taskID string) (task.Metadata, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	md, ok := h.tasks[taskID]
	if !ok {
		return task.Metadata{}, fmt.Errorf("%w: %s", ErrTaskNotFound, taskID)
	}
	return cloneTaskMetadata(md), nil
}

// Tasks returns snapshots of every observed task in task-id order. A
// non-empty owner restricts to that owner's tasks; activeOnly drops
// terminal ones.
func (h *Hub) Tasks(owner string, activeOnly bool) []task.Metadata {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]task.Metadata, 0, len(h.tasks))
	for _, md := range h.tasks {
		if owner != "" && md.OwnerID != owner {
			continue
		}
		if activeOnly && md.State.Terminal() {
			continue
		}
		out = append(out, cloneTaskMetadata(md))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TaskID < out[j].TaskID })
	return out
}

// cloneTaskMetadata deep-copies the map fields so stored snapshots never
// alias a caller's memory.
func cloneTaskMetadata(m task.Metadata) task.Metadata {
	if m.Progress != nil {
		p := make(map[string]any, len(m.Progress))
		for k, v := range m.Progress {
			p[k] = v
		}
		m.Progress = p
	}
	if m.Spec.Payload != nil {
		p := make(map[string]any, len(m.Spec.Payload))
		for k, v := range m.Spec.Payload {
			p[k] = v
		}
		m.Spec.Payload = p
	}
	return m
}

// --- mirror -------------------------------------------------------------------

// TaskMirror forwards one agent's Task* stream events into the hub's task
// store (Python parity: task_mirror.py). The owner is the agent's
// registration id; ChannelID, when set, stamps the mirrored tasks with the
// channel they ran in (the tasks tool's cancel action needs it to route a
// cancel request).
//
// Forwarding failures never crash the agent's turn: they are logged at
// ERROR and reported through the hub's task-event hook as "mirror_failed"
// (WithTaskEventHook). A terminal task whose spec carries a Capability also
// updates the owner's Resume.Observed (Hub.RecordObservation).
type TaskMirror struct {
	hub       *Hub
	ownerID   string
	channelID string

	mu   sync.Mutex
	subs []stream.SubID
}

// NewTaskMirror builds a mirror over a hub. It binds to the owner's
// registration id immediately; Attach subscribes on the agent's stream.
func NewTaskMirror(hub *Hub, ownerID, channelID string) *TaskMirror {
	return &TaskMirror{hub: hub, ownerID: ownerID, channelID: channelID}
}

// Attach subscribes to the stream's Task* events (started / progress /
// completed / failed / expired / cancelled) and returns the mirror for
// chaining. Detach unsubscribes.
func (m *TaskMirror) Attach(s stream.Stream) *TaskMirror {
	on := func(f func(ev events.Event)) func(events.Event) error {
		return func(ev events.Event) error { f(ev); return nil }
	}
	ids := []stream.SubID{
		s.Subscribe(on(m.onStarted), stream.SubscribeOptions{Condition: events.TypeOf[events.TaskStarted]()}),
		s.Subscribe(on(m.onProgress), stream.SubscribeOptions{Condition: events.TypeOf[events.TaskProgress]()}),
		s.Subscribe(on(m.onCompleted), stream.SubscribeOptions{Condition: events.TypeOf[events.TaskCompleted]()}),
		s.Subscribe(on(m.onFailed), stream.SubscribeOptions{Condition: events.TypeOf[events.TaskFailed]()}),
		s.Subscribe(on(m.onExpired), stream.SubscribeOptions{Condition: events.TypeOf[events.TaskExpired]()}),
		s.Subscribe(on(m.onCancelled), stream.SubscribeOptions{Condition: events.TypeOf[events.TaskCancelled]()}),
	}
	m.mu.Lock()
	m.subs = append(m.subs, ids...)
	m.mu.Unlock()
	return m
}

// Detach unsubscribes everything Attach registered. Idempotent.
func (m *TaskMirror) Detach(s stream.Stream) {
	m.mu.Lock()
	subs := m.subs
	m.subs = nil
	m.mu.Unlock()
	for _, id := range subs {
		s.Unsubscribe(id)
	}
}

// onStarted seeds the store: state running, the agent-side absolute
// deadline carried through so networked task TTLs stay enforceable.
func (m *TaskMirror) onStarted(ev events.Event) {
	started, ok := events.AsValue(ev).(events.TaskStarted)
	if !ok {
		return
	}
	now := m.hub.now()
	md := task.Metadata{
		TaskID:    started.TaskID,
		OwnerID:   m.ownerID,
		State:     task.StateRunning,
		CreatedAt: now,
		StartedAt: now,
		ChannelID: m.channelID,
	}
	if started.Spec != nil {
		md.Spec = task.Spec{
			Title:       started.Spec.Title,
			Description: started.Spec.Description,
			Payload:     started.Spec.Payload,
			Capability:  started.Spec.Capability,
		}
	} else {
		md.Spec = task.Spec{Title: started.Objective}
	}
	if started.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, started.ExpiresAt); err == nil {
			md.ExpiresAt = t
		}
	}
	if err := m.hub.ObserveTask(md); err != nil {
		m.escalate(started.TaskID, "started", err)
	}
}

// escalate reports a forwarding failure without disturbing the agent's turn:
// an ERROR log line plus a "mirror_failed" task event.
func (m *TaskMirror) escalate(taskID, op string, err error) {
	slog.Error("task mirror failed", "op", op, "task_id", taskID, "owner_id", m.ownerID, "error", err)
	m.hub.FireTaskEvent(taskID, TaskEventMirrorFailed, map[string]any{
		"op":          op,
		"owner_id":    m.ownerID,
		"channel_id":  m.channelID,
		"exc_type":    fmt.Sprintf("%T", err),
		"exc_message": err.Error(),
	})
}

// updateSwallowingMissing runs UpdateTask; ErrTaskNotFound is expected
// (progress/completion for a task the hub never observed) and dropped, any
// other failure escalates.
func (m *TaskMirror) updateSwallowingMissing(taskID, op string, state task.State, progress map[string]any, result any, errMsg string) {
	if err := m.hub.UpdateTask(taskID, state, progress, result, errMsg); err != nil && !errors.Is(err, ErrTaskNotFound) {
		m.escalate(taskID, op, err)
	}
}

// recordObservationIfTagged pushes a terminal task's outcome into the
// owner's track record when the spec carried a Capability. The accepted
// state wins over the proposed outcome and latency is measured on the hub
// clock from the observed start. An unregistered owner has no resume to
// update, so the failure is dropped (Python parity: the call is suppressed).
func (m *TaskMirror) recordObservationIfTagged(taskID string, outcome task.State) {
	md, err := m.hub.Task(taskID)
	if err != nil || md.Spec.Capability == "" {
		return
	}
	if md.State.Terminal() {
		outcome = md.State
	}
	latency := time.Duration(-1)
	if !md.StartedAt.IsZero() {
		if latency = m.hub.now().Sub(md.StartedAt); latency < 0 {
			latency = 0
		}
	}
	_ = m.hub.RecordObservation(md.OwnerID, md.Spec.Capability, outcome, latency, taskID)
}

func (m *TaskMirror) onProgress(ev events.Event) {
	p, ok := events.AsValue(ev).(events.TaskProgress)
	if !ok {
		return
	}
	m.updateSwallowingMissing(p.TaskID, "progress", "", p.Payload, nil, "")
}

func (m *TaskMirror) onCompleted(ev events.Event) {
	c, ok := events.AsValue(ev).(events.TaskCompleted)
	if !ok {
		return
	}
	var result any
	if c.Result != "" {
		result = c.Result
	}
	m.updateSwallowingMissing(c.TaskID, "completed", task.StateCompleted, nil, result, "")
	m.recordObservationIfTagged(c.TaskID, task.StateCompleted)
}

func (m *TaskMirror) onFailed(ev events.Event) {
	f, ok := events.AsValue(ev).(events.TaskFailed)
	if !ok {
		return
	}
	m.updateSwallowingMissing(f.TaskID, "failed", task.StateFailed, nil, nil, f.Error)
	m.recordObservationIfTagged(f.TaskID, task.StateFailed)
}

func (m *TaskMirror) onExpired(ev events.Event) {
	e, ok := events.AsValue(ev).(events.TaskExpired)
	if !ok {
		return
	}
	m.updateSwallowingMissing(e.TaskID, "expired", task.StateExpired, nil, nil, "")
	m.recordObservationIfTagged(e.TaskID, task.StateExpired)
}

func (m *TaskMirror) onCancelled(ev events.Event) {
	c, ok := events.AsValue(ev).(events.TaskCancelled)
	if !ok {
		return
	}
	m.updateSwallowingMissing(c.TaskID, "cancelled", task.StateCancelled, nil, nil, c.Reason)
	m.recordObservationIfTagged(c.TaskID, task.StateCancelled)
}
