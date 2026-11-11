package network

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/luckystrker/ago/task"
)

// Lifecycle errors.
var (
	// ErrHubClosed is returned by operations on a hub after Close.
	ErrHubClosed = errors.New("network: hub closed")
	// ErrSweeperExists means a sweeper with that name is already registered.
	ErrSweeperExists = errors.New("network: sweeper already registered")
	// ErrBadInterval means a sweeper interval was not positive.
	ErrBadInterval = errors.New("network: sweeper interval must be positive")
)

// DefaultTTLSweepInterval is the TTL sweeper period (Python
// ttl_sweep_interval); the expectation sweeper's default is
// DefaultExpectationSweepInterval.
const DefaultTTLSweepInterval = 30 * time.Second

// Names of the built-in sweepers.
const (
	SweeperTTL          = "ttl"
	SweeperExpectations = "expectations"
)

// sweeper is one periodic background worker.
type sweeper struct {
	name     string
	interval time.Duration
	fn       func()
	stop     chan struct{}
	done     chan struct{}
}

func (s *sweeper) start() {
	go func() {
		defer close(s.done)
		t := time.NewTicker(s.interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				s.tick()
			case <-s.stop:
				return
			}
		}
	}()
}

// tick runs one sweep; a panic is logged and never kills the loop.
func (s *sweeper) tick() {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("hub sweeper panicked", "sweeper", s.name, "panic", r)
		}
	}()
	s.fn()
}

func (s *sweeper) halt() {
	close(s.stop)
	<-s.done
}

// lifecycleState is the sweeper registry and the started/closed flags,
// guarded by its own mutex (sweepers call back into the hub).
type lifecycleState struct {
	mu              sync.Mutex
	started, closed bool
	ttlInterval     time.Duration
	expectInterval  time.Duration
	defaultAdapters bool
	sweepers        map[string]*sweeper
	order           []string
}

func (l *lifecycleState) init() {
	l.ttlInterval = DefaultTTLSweepInterval
	l.expectInterval = DefaultExpectationSweepInterval
	l.defaultAdapters = true
	l.sweepers = make(map[string]*sweeper)
}

// WithTTLSweepInterval sets the built-in TTL sweeper's period; 0 disables it.
// Takes effect at Start.
func WithTTLSweepInterval(d time.Duration) HubOption {
	return func(h *Hub) { h.ext.lifecycle.ttlInterval = d }
}

// WithExpectationSweepInterval sets the built-in expectation sweeper's
// period; 0 disables it. Takes effect at Start.
func WithExpectationSweepInterval(d time.Duration) HubOption {
	return func(h *Hub) { h.ext.lifecycle.expectInterval = d }
}

// WithoutDefaultAdapters makes OpenHub skip registering the four built-in
// adapters.
func WithoutDefaultAdapters() HubOption {
	return func(h *Hub) { h.ext.lifecycle.defaultAdapters = false }
}

// RegisterDefaultAdapters registers the built-in consulting, conversation,
// discussion and workflow adapters at version "1". Already-registered
// adapters of those types are replaced.
func (h *Hub) RegisterDefaultAdapters() {
	h.RegisterAdapter(ConsultingAdapter{}, "1")
	h.RegisterAdapter(ConversationAdapter{}, "1")
	h.RegisterAdapter(DiscussionAdapter{}, "1")
	h.RegisterAdapter(WorkflowAdapter{}, "1")
}

// OpenHub is the production entry point: NewHub, the built-in adapters
// (unless WithoutDefaultAdapters), Hydrate (the registry from the store, then
// a Replay of the WAL; either only when attached), then Start. NewHub stays the entry for tests that want no background work.
func OpenHub(opts ...HubOption) (*Hub, error) {
	h := NewHub(opts...)
	if h.ext.lifecycle.defaultAdapters {
		h.RegisterDefaultAdapters()
	}
	if err := h.Hydrate(); err != nil {
		return nil, err
	}
	h.Start()
	return h, nil
}

// RegisterSweeper attaches a custom periodic worker: fn runs every interval
// until Unregister or Close. Before Start it is queued and starts with the
// built-in sweepers; after Start it starts immediately. Re-using a name fails
// with ErrSweeperExists, a non-positive interval with ErrBadInterval, a
// closed hub with ErrHubClosed. fn must not block for long; a panic is
// logged and the loop continues.
func (h *Hub) RegisterSweeper(name string, interval time.Duration, fn func()) error {
	if interval <= 0 {
		return fmt.Errorf("%w: %v", ErrBadInterval, interval)
	}
	l := &h.ext.lifecycle
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrHubClosed
	}
	if _, dup := l.sweepers[name]; dup {
		return fmt.Errorf("%w: %q", ErrSweeperExists, name)
	}
	s := &sweeper{name: name, interval: interval, fn: fn, stop: make(chan struct{}), done: make(chan struct{})}
	l.sweepers[name] = s
	l.order = append(l.order, name)
	if l.started {
		s.start()
	}
	return nil
}

// UnregisterSweeper stops and removes a sweeper (custom or built-in) and
// waits for its goroutine to exit. An unknown name is a no-op.
func (h *Hub) UnregisterSweeper(name string) {
	l := &h.ext.lifecycle
	l.mu.Lock()
	s, ok := l.sweepers[name]
	if ok {
		delete(l.sweepers, name)
		for i, n := range l.order {
			if n == name {
				l.order = append(l.order[:i:i], l.order[i+1:]...)
				break
			}
		}
	}
	started := l.started
	l.mu.Unlock()
	if ok && started {
		s.halt()
	}
}

// Sweepers lists the registered sweeper names in registration order.
func (h *Hub) Sweepers() []string {
	l := &h.ext.lifecycle
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.order...)
}

// Start launches the TTL and expectation sweepers (unless their interval is
// 0) and every queued custom sweeper. Idempotent; a no-op after Close.
func (h *Hub) Start() {
	l := &h.ext.lifecycle
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.started || l.closed {
		return
	}
	add := func(name string, interval time.Duration, fn func()) {
		if interval <= 0 {
			return
		}
		if _, dup := l.sweepers[name]; dup {
			return
		}
		l.sweepers[name] = &sweeper{name: name, interval: interval, fn: fn, stop: make(chan struct{}), done: make(chan struct{})}
		l.order = append(l.order, name)
	}
	add(SweeperTTL, l.ttlInterval, h.SweepTTL)
	add(SweeperExpectations, l.expectInterval, h.SweepExpectations)
	l.started = true
	for _, name := range l.order {
		l.sweepers[name].start()
	}
}

// Close stops every sweeper and waits for them to exit. Idempotent. The hub's
// in-memory state stays readable; the WAL (owned by the caller) is not
// closed. Registering sweepers afterwards fails with ErrHubClosed.
func (h *Hub) Close() error {
	l := &h.ext.lifecycle
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	started := l.started
	running := make([]*sweeper, 0, len(l.sweepers))
	for _, name := range l.order {
		running = append(running, l.sweepers[name])
	}
	l.sweepers = map[string]*sweeper{}
	l.order = nil
	l.mu.Unlock()
	if started {
		for _, s := range running {
			s.halt()
		}
	}
	return nil
}

// --- TTL sweeper ---------------------------------------------------------------

// SweepTTL runs one TTL pass at the hub clock: every non-terminal channel
// past its ExpiresAt expires (its live tasks cascade to expired first), then
// every remaining live task past its own ExpiresAt expires with reason
// "ttl_expired". The sweeper registered by Start calls it each interval;
// tests call it directly against a fake clock. Safe for concurrent use.
func (h *Hub) SweepTTL() {
	h.mu.Lock()
	now := h.now()
	var channels []string
	for id, ch := range h.channels {
		if !ch.State.Terminal() && !ch.ExpiresAt.IsZero() && !ch.ExpiresAt.After(now) {
			channels = append(channels, id)
		}
	}
	h.mu.Unlock()
	sort.Strings(channels)
	for _, id := range channels {
		// A channel still in its invite handshake releases the create waiter
		// with an error instead of a half-open handle.
		h.closeChannel(id, "ttl_expired", fmt.Errorf("%w: channel %s", ErrChannelExpired, id), ChannelExpired)
	}

	var evs hubEvents
	h.mu.Lock()
	var tasks []string
	for id, md := range h.tasks {
		if !md.State.Terminal() && !md.ExpiresAt.IsZero() && !md.ExpiresAt.After(now) {
			tasks = append(tasks, id)
		}
	}
	sort.Strings(tasks)
	for _, id := range tasks {
		h.transitionTaskLocked(id, task.StateExpired, "ttl_expired", &evs)
	}
	h.mu.Unlock()
	h.flushEvents(&evs)
}

// cascadeTasksLocked expires the live tasks riding channelID (reason is the
// channel's close reason context; the task error is "channel_closed").
func (h *Hub) cascadeTasksLocked(channelID, _ string, evs *hubEvents) {
	var ids []string
	for id, md := range h.tasks {
		if md.ChannelID == channelID && !md.State.Terminal() {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		h.transitionTaskLocked(id, task.StateExpired, "channel_closed", evs)
	}
}

// transitionTaskLocked is the hub-driven terminal transition (TTL, cascade):
// state, completion time, error text for an expiry, and the OnTaskEvent.
func (h *Hub) transitionTaskLocked(taskID string, state task.State, reason string, evs *hubEvents) {
	md, ok := h.tasks[taskID]
	if !ok || md.State.Terminal() {
		return
	}
	md.State = state
	md.CompletedAt = h.now()
	if state == task.StateExpired {
		md.Error = nonEmpty(reason, nonEmpty(md.Error, "expired"))
	}
	delete(h.ext.peerCancel, taskID)
	h.tasks[taskID] = md
	h.queueTaskTerminal(evs, md, reason)
}

// queueTaskTerminal queues the OnTaskEvent of a task that reached a terminal
// state.
func (h *Hub) queueTaskTerminal(evs *hubEvents, md task.Metadata, reason string) {
	payload := map[string]any{
		"owner_id":   md.OwnerID,
		"channel_id": md.ChannelID,
		"reason":     reason,
		"capability": md.Spec.Capability,
		"outcome":    string(md.State),
		"started_at": md.StartedAt,
		"at":         md.CompletedAt,
	}
	id, kind := md.TaskID, string(md.State)
	evs.add(func(l HubListener) { l.OnTaskEvent(id, kind, payload) })
}

// --- health ----------------------------------------------------------------------

// HubHealth is the cheap in-memory operational snapshot behind Hub.Health.
type HubHealth struct {
	ActiveChannels       int   `json:"active_channels"` // pending + active
	ActiveTasks          int   `json:"active_tasks"`
	RegisteredAgents     int   `json:"registered_agents"` // agents and humans
	PendingInboxTotal    int   `json:"pending_inbox_total"`
	MaxPendingInboxDepth int   `json:"max_pending_inbox_depth"` // 0 when nothing is pending
	RegisteredListeners  int   `json:"registered_listeners"`
	AdaptersLoaded       int   `json:"adapters_loaded"`
	Sweepers             int   `json:"sweepers"`
	AuditLogBytes        int64 `json:"audit_log_bytes"`
}

// Health returns an operational snapshot: a handful of indicative numbers to
// wire to a /health endpoint or dashboard. The audit log counts as a
// listener.
//
// AGO-DEVIATION: Python's telemetry_log_bytes is absent (no telemetry spans);
// active_tasks and sweepers are added.
func (h *Hub) Health() HubHealth {
	h.mu.Lock()
	out := HubHealth{
		RegisteredAgents: len(h.passports),
		AdaptersLoaded:   len(h.adapters),
	}
	for _, ch := range h.channels {
		if !ch.State.Terminal() {
			out.ActiveChannels++
		}
	}
	for _, md := range h.tasks {
		if !md.State.Terminal() {
			out.ActiveTasks++
		}
	}
	for _, n := range h.ext.inboxPending {
		out.PendingInboxTotal += n
		if n > out.MaxPendingInboxDepth {
			out.MaxPendingInboxDepth = n
		}
	}
	audit := h.ext.audit
	h.mu.Unlock()
	out.RegisteredListeners = h.ListenerCount()
	if audit != nil {
		out.AuditLogBytes = audit.BytesWritten()
	}
	l := &h.ext.lifecycle
	l.mu.Lock()
	out.Sweepers = len(l.sweepers)
	l.mu.Unlock()
	return out
}

// --- peer cancel -------------------------------------------------------------------

// maxWireText bounds the reason of a peer cancel request (Python
// _MAX_WIRE_TEXT).
const maxWireText = 500

// isPeerCancelRequestLocked reports whether env is a cancel request shaped
// exactly as the tasks tool sends it: an EnvTaskCancelRequest on an active
// channel for a live task the hub observed riding that same channel,
// addressed only to the task's owner, whose payload is exactly
// {"task_id", "reason"} naming the envelope's TaskID with a string reason of
// at most 500 bytes.
func (h *Hub) isPeerCancelRequestLocked(env Envelope, ch *ChannelHandle) bool {
	if env.EventType != EnvTaskCancelRequest || env.TaskID == "" || ch.State != ChannelActive {
		return false
	}
	md, ok := h.tasks[env.TaskID]
	if !ok || md.State.Terminal() || md.ChannelID != env.ChannelID || env.Audience != md.OwnerID {
		return false
	}
	var payload map[string]any
	if env.Decode(&payload) != nil || len(payload) != 2 {
		return false
	}
	reason, isString := payload["reason"].(string)
	return isString && len(reason) <= maxWireText && payload["task_id"] == env.TaskID
}

// markPeerCancelLocked records that env's sender used its one cancel request
// for the task.
func (h *Hub) markPeerCancelLocked(env Envelope) {
	senders := h.ext.peerCancel[env.TaskID]
	if senders == nil {
		senders = make(map[string]bool)
		h.ext.peerCancel[env.TaskID] = senders
	}
	senders[env.SenderID] = true
}
