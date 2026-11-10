package network

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/luckystrker/ago/task"
)

// hubExt holds the hub state added for the network data model: auth,
// capability index and observed track record, inbox accounting, rate
// limiting and the observation hooks. Everything except the immutable auth
// registry and the hooks is guarded by Hub.mu.
type hubExt struct {
	auth            *AuthRegistry
	capIndex        map[string]map[string]struct{} // capability → agent ids
	observed        map[string]struct{}            // "owner\x00task" already counted
	inboxPending    map[string]int                 // agent id → unacknowledged substantive envelopes
	rateBuckets     map[string]*TokenBucket        // sender agent id → bucket
	onInboxPressure func(agentID string, pending, max int)
	onTaskEvent     func(taskID, kind string, payload map[string]any)
}

func (e *hubExt) init() {
	e.auth = DefaultAuthRegistry()
	e.capIndex = make(map[string]map[string]struct{})
	e.observed = make(map[string]struct{})
	e.inboxPending = make(map[string]int)
	e.rateBuckets = make(map[string]*TokenBucket)
}

// WithHubAuth replaces the auth registry consulted at Register (default:
// NoAuth only). A nil registry keeps the default.
func WithHubAuth(r *AuthRegistry) HubOption {
	return func(h *Hub) {
		if r != nil {
			h.ext.auth = r
		}
	}
}

// WithInboxPressureHook installs fn, called once each time a recipient's
// pending count crosses Rule.Inbox's high-water mark upward. It runs on the
// sender's goroutine outside the hub lock and must not block.
func WithInboxPressureHook(fn func(agentID string, pending, max int)) HubOption {
	return func(h *Hub) { h.ext.onInboxPressure = fn }
}

// WithTaskEventHook installs fn, called by FireTaskEvent — the hub's channel
// for task-side operational signals such as "mirror_failed". It runs outside
// the hub lock and must not block.
func WithTaskEventHook(fn func(taskID, kind string, payload map[string]any)) HubOption {
	return func(h *Hub) { h.ext.onTaskEvent = fn }
}

// TaskEventMirrorFailed is the FireTaskEvent kind a TaskMirror raises when
// forwarding a task event into the hub fails.
const TaskEventMirrorFailed = "mirror_failed"

// FireTaskEvent reports a task-side event to the task-event hook (no-op
// without one).
func (h *Hub) FireTaskEvent(taskID, kind string, payload map[string]any) {
	if fn := h.ext.onTaskEvent; fn != nil {
		fn(taskID, kind, payload)
	}
}

// Authenticate validates a passport's auth claim against the registry: the
// scheme comes from p.Auth["scheme"] (absent = "none"). Remote-agent
// passports are not validated locally — their scheme is a routing label for
// the remote proxy. Register calls it with the claim carried in p.Auth;
// wire handshakes call it with the claim a peer presents.
func (h *Hub) Authenticate(ctx context.Context, p Passport, claim map[string]any) error {
	if p.Kind == KindRemoteAgent {
		return nil
	}
	scheme, _ := authSchemeAndClaim(p)
	adapter, err := h.ext.auth.Get(scheme)
	if err != nil {
		return err
	}
	return adapter.Validate(ctx, p, claim)
}

// --- capability index ---------------------------------------------------------

// AgentsWithCapability lists the ids of agents that claim or have been
// observed performing capability, sorted.
func (h *Hub) AgentsWithCapability(capability string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.ext.capIndex[capability]))
	for id := range h.ext.capIndex[capability] {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// reindexLocked moves agentID from the capabilities of old to those of cur.
func (h *Hub) reindexLocked(agentID string, old, cur Resume) {
	oldSet, curSet := old.allCapabilities(), cur.allCapabilities()
	for c := range oldSet {
		if _, keep := curSet[c]; keep {
			continue
		}
		delete(h.ext.capIndex[c], agentID)
		if len(h.ext.capIndex[c]) == 0 {
			delete(h.ext.capIndex, c)
		}
	}
	for c := range curSet {
		if h.ext.capIndex[c] == nil {
			h.ext.capIndex[c] = make(map[string]struct{})
		}
		h.ext.capIndex[c][agentID] = struct{}{}
	}
}

// RecordObservation folds one terminal task outcome into the owner's
// Resume.Observed[capability] and the capability index, bumping the resume
// version. Non-terminal outcomes and a (owner, taskID) pair already counted
// are ignored, so a cascade of terminal events counts once. latency < 0
// means "not measured"; otherwise it replaces the stored p50 (single-sample
// stand-in). A resume is created on demand for a registered agent that never
// set one.
//
// AGO-DEVIATION: an unknown owner fails with ErrNotFound and a missing resume
// is created, whereas Python returns silently when the resume is absent
// (every Python registration has one; AGo registrations may not).
func (h *Hub) RecordObservation(ownerID, capability string, outcome task.State, latency time.Duration, taskID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.passports[ownerID]; !ok {
		return fmt.Errorf("%w: agent %s", ErrNotFound, ownerID)
	}
	if capability == "" || !outcome.Terminal() {
		return nil
	}
	key := ownerID + "\x00" + taskID
	if taskID != "" {
		if _, dup := h.ext.observed[key]; dup {
			return nil
		}
	}
	old, had := h.resumes[ownerID]
	cur := old.Clone()
	if !had {
		cur = Resume{AgentID: ownerID, Version: 1}
	}
	stat := cur.Observed[capability]
	stat.N++
	switch outcome {
	case task.StateCompleted:
		stat.Completed++
	case task.StateFailed:
		stat.Failed++
	case task.StateExpired:
		stat.Expired++
	}
	if latency >= 0 {
		ms := int(latency / time.Millisecond)
		stat.P50LatencyMS = &ms
	}
	if cur.Observed == nil {
		cur.Observed = make(map[string]ObservedStat)
	}
	cur.Observed[capability] = stat
	cur.LastUpdated = h.now()
	cur.Version++
	h.resumes[ownerID] = cur
	h.reindexLocked(ownerID, old, cur)
	if taskID != "" {
		h.ext.observed[key] = struct{}{}
	}
	return nil
}

// AddResumeExample appends an example to the agent's resume without
// clobbering concurrent updates (the read-modify-write happens under the hub
// lock). An empty When is stamped with the hub clock.
func (h *Hub) AddResumeExample(agentID string, ex ResumeExample) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.passports[agentID]; !ok {
		return fmt.Errorf("%w: agent %s", ErrNotFound, agentID)
	}
	if ex.When == "" {
		ex.When = h.now().UTC().Format(time.RFC3339)
	}
	old, had := h.resumes[agentID]
	cur := old.Clone()
	if !had {
		cur = Resume{AgentID: agentID, Version: 1}
	}
	cur.Examples = append(cur.Examples, ex)
	cur.LastUpdated = h.now()
	cur.Version++
	h.resumes[agentID] = cur
	return nil
}

// --- inbox accounting ---------------------------------------------------------

// substantive reports whether an envelope counts against inbox capacity and
// the rate limit: protocol bookkeeping and task mirrors never do (the
// channel machine must always advance).
func substantive(env Envelope) bool {
	return !isProtocolEvent(env.EventType) && !isTaskEvent(env.EventType)
}

// InboxPending is the recipient's count of substantive envelopes dispatched
// and not yet acknowledged.
func (h *Hub) InboxPending(agentID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ext.inboxPending[agentID]
}

// AckInbox tells the hub the recipient finished with — processed, dropped or
// discarded — n delivered envelopes, freeing that much inbox capacity.
// AgentClient and HumanClient.Recv do this themselves; a bare Notifier that
// sits behind Rule.Inbox must call it. The count never goes below zero.
//
// AGO-DEVIATION: Python decrements a recipient's pending count whenever it
// posts any substantive envelope ("any outbound activity = processing");
// AGo has the queue owner acknowledge each envelope explicitly, so a silent
// round still frees its slot.
func (h *Hub) AckInbox(agentID string, n int) {
	if n <= 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if cur := h.ext.inboxPending[agentID]; cur > n {
		h.ext.inboxPending[agentID] = cur - n
	} else {
		delete(h.ext.inboxPending, agentID)
	}
}

// pressureEvent is a high-water crossing to report once the lock releases.
type pressureEvent struct {
	agentID      string
	pending, max int
}

// admitLocked gates a substantive send before anything is recorded: the
// sender's token bucket, then every recipient's inbox via the arbiter.
func (h *Hub) admitLocked(env Envelope, ch *ChannelHandle, sender Passport, senderRule Rule) error {
	if !substantive(env) {
		return nil
	}
	if senderRule.Rate.PerMinute > 0 {
		tb, ok := h.ext.rateBuckets[sender.AgentID]
		if !ok {
			tb = NewTokenBucket(senderRule.Rate, h.now)
			h.ext.rateBuckets[sender.AgentID] = tb
		}
		if !tb.Allow() {
			return fmt.Errorf("%w: sender %q exceeds %d/min (burst %d)",
				ErrRateLimited, sender.Name, senderRule.Rate.PerMinute, senderRule.Rate.Burst)
		}
	}
	var audience []string
	if env.Audience != "" {
		audience = []string{env.Audience}
	} else {
		for _, p := range ch.Participants {
			audience = append(audience, p.AgentID)
		}
	}
	for _, id := range audience {
		if id == env.SenderID {
			continue
		}
		recipient, ok := h.passports[id]
		if !ok {
			continue
		}
		if d := h.arbiter.AuthorizeInbox(env, recipient, h.rules[id], h.ext.inboxPending[id]); !d.Allowed {
			return denyErr(d)
		}
	}
	return nil
}

// countDispatchLocked books one pending envelope against every attached,
// non-sender target of a substantive dispatch and returns the high-water
// crossings.
func (h *Hub) countDispatchLocked(env Envelope, targets []dispatchTarget) []pressureEvent {
	if !substantive(env) {
		return nil
	}
	var events []pressureEvent
	for _, t := range targets {
		if t.notify == nil || t.isSelf {
			continue
		}
		prev := h.ext.inboxPending[t.id]
		cur := prev + 1
		h.ext.inboxPending[t.id] = cur
		inbox := h.rules[t.id].Inbox
		if hw := inbox.highWaterMark(); hw > 0 && prev < hw && hw <= cur {
			events = append(events, pressureEvent{agentID: t.id, pending: cur, max: inbox.MaxPending})
		}
	}
	return events
}

// firePressure reports high-water crossings; called outside the lock.
func (h *Hub) firePressure(events []pressureEvent) {
	if h.ext.onInboxPressure == nil {
		return
	}
	for _, e := range events {
		h.ext.onInboxPressure(e.agentID, e.pending, e.max)
	}
}

// forgetLocked drops the per-agent extension state on unregister.
func (h *Hub) forgetLocked(agentID string, resume Resume) {
	h.reindexLocked(agentID, resume, Resume{})
	delete(h.ext.inboxPending, agentID)
	delete(h.ext.rateBuckets, agentID)
}
