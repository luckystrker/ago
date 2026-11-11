package network

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// HubListener observes hub state transitions — the single observability seam
// the audit log, health snapshot and any tenant metrics hang off. The hub
// fans each event out synchronously, in registration order, after releasing
// its own lock, so a listener may call back into the hub; it must still not
// block. A panicking listener is logged and skipped, never allowed to disturb
// the hub or the other listeners.
//
// Embed BaseHubListener and override only the hooks you need.
type HubListener interface {
	// OnEnvelopePosted: an envelope was validated, recorded, folded and
	// dispatched. ch is a snapshot of the channel after the fold.
	OnEnvelopePosted(env Envelope, ch *ChannelHandle)
	// OnEnvelopeRejected: Send failed before the envelope was recorded;
	// reason is the error the sender saw (ErrAccessDenied, ErrProtocol,
	// ErrInboxFull, ErrRateLimited, ...).
	OnEnvelopeRejected(env Envelope, reason error)
	// OnDispatchFailed: delivering an accepted envelope to one recipient
	// failed (its Notifier panicked). The rest of the audience still
	// received it and the sender still sees success.
	OnDispatchFailed(env Envelope, recipientID string, reason error)
	// OnChannelEvent: kind is ChannelEventCreated, ChannelEventOpened,
	// ChannelEventClosed or ChannelEventExpired.
	OnChannelEvent(channelID, kind string, payload map[string]any)
	// OnAgentEvent: kind is AgentEventRegistered, AgentEventUnregistered,
	// AgentEventResumeSet, AgentEventRuleSet or AgentEventObservationRecorded.
	OnAgentEvent(agentID, kind string, payload map[string]any)
	// OnExpectationFired: an expectation evaluator emitted a violation, once
	// per (channel, expectation, violator).
	OnExpectationFired(channelID string, exp Expectation, v Violation)
	// OnTurnFailed: a client's turn handler failed on an inbound envelope
	// (Hub.ReportTurnFailure). The channel stays alive.
	OnTurnFailed(channelID, agentID, envelopeID string, err error)
	// OnTaskEvent: kind is the terminal task.State a task reached
	// (completed, failed, expired, cancelled; payload carries owner_id,
	// channel_id, reason, capability, outcome, started_at, at) or
	// TaskEventMirrorFailed, or any kind a tenant passes to FireTaskEvent.
	OnTaskEvent(taskID, kind string, payload map[string]any)
	// OnInboxPressure: a recipient's pending count crossed its high-water
	// mark upward.
	OnInboxPressure(agentID string, pending, max int)
}

// Channel event kinds passed to OnChannelEvent.
//
// AGO-DEVIATION: Python also fires participant_removed and participant_hidden
// (from the warn/hide/remove violation handlers); AGo has no such handlers or
// operations, so those two kinds never occur.
const (
	ChannelEventCreated = "created"
	ChannelEventOpened  = "opened"
	ChannelEventClosed  = "closed"
	ChannelEventExpired = "expired"
)

// Agent event kinds passed to OnAgentEvent. Python's skill_set is absent: AGo
// has no SKILL.md registry.
const (
	AgentEventRegistered          = "registered"
	AgentEventUnregistered        = "unregistered"
	AgentEventResumeSet           = "resume_set"
	AgentEventRuleSet             = "rule_set"
	AgentEventObservationRecorded = "observation_recorded"
)

// BaseHubListener is the no-op HubListener to embed.
type BaseHubListener struct{}

func (BaseHubListener) OnEnvelopePosted(Envelope, *ChannelHandle)         {}
func (BaseHubListener) OnEnvelopeRejected(Envelope, error)                {}
func (BaseHubListener) OnDispatchFailed(Envelope, string, error)          {}
func (BaseHubListener) OnChannelEvent(string, string, map[string]any)     {}
func (BaseHubListener) OnAgentEvent(string, string, map[string]any)       {}
func (BaseHubListener) OnExpectationFired(string, Expectation, Violation) {}
func (BaseHubListener) OnTurnFailed(string, string, string, error)        {}
func (BaseHubListener) OnTaskEvent(string, string, map[string]any)        {}
func (BaseHubListener) OnInboxPressure(string, int, int)                  {}

var _ HubListener = BaseHubListener{}

// listenerSet is the hub's registered listeners; guarded by its own lock so
// fan-out never needs the hub lock.
type listenerSet struct {
	mu   sync.RWMutex
	list []HubListener
}

// hubEvents collects listener notifications raised while the hub lock is
// held; flushEvents delivers them once it is released.
type hubEvents []func(HubListener)

func (e *hubEvents) add(f func(HubListener)) { *e = append(*e, f) }

// RegisterListener attaches l; it receives events from now on, after every
// listener registered earlier. Registering the same listener twice delivers
// twice.
func (h *Hub) RegisterListener(l HubListener) {
	if l == nil {
		return
	}
	h.ext.listeners.mu.Lock()
	defer h.ext.listeners.mu.Unlock()
	h.ext.listeners.list = append(h.ext.listeners.list, l)
}

// UnregisterListener detaches l (comparable listeners only — pointers are).
// A listener that was never registered is a no-op.
func (h *Hub) UnregisterListener(l HubListener) {
	h.ext.listeners.mu.Lock()
	defer h.ext.listeners.mu.Unlock()
	for i, have := range h.ext.listeners.list {
		if comparableEqual(have, l) {
			h.ext.listeners.list = append(h.ext.listeners.list[:i:i], h.ext.listeners.list[i+1:]...)
			return
		}
	}
}

// comparableEqual compares two listeners without panicking on a
// non-comparable dynamic type (such a listener can be registered but never
// unregistered).
func comparableEqual(a, b HubListener) (eq bool) {
	defer func() {
		if recover() != nil {
			eq = false
		}
	}()
	return a == b
}

// registerListenerFirst inserts l ahead of every other listener (the audit
// log goes first so it records an event before tenant listeners observe it).
func (h *Hub) registerListenerFirst(l HubListener) {
	h.ext.listeners.mu.Lock()
	defer h.ext.listeners.mu.Unlock()
	h.ext.listeners.list = append([]HubListener{l}, h.ext.listeners.list...)
}

// ListenerCount reports the number of registered listeners.
func (h *Hub) ListenerCount() int {
	h.ext.listeners.mu.RLock()
	defer h.ext.listeners.mu.RUnlock()
	return len(h.ext.listeners.list)
}

// fanOut calls f on a snapshot of the listeners, isolating panics.
func (h *Hub) fanOut(f func(HubListener)) {
	h.ext.listeners.mu.RLock()
	snapshot := append([]HubListener(nil), h.ext.listeners.list...)
	h.ext.listeners.mu.RUnlock()
	for _, l := range snapshot {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("hub listener panicked", "listener", fmt.Sprintf("%T", l), "panic", r)
				}
			}()
			f(l)
		}()
	}
}

// flushEvents delivers the collected notifications.
func (h *Hub) flushEvents(events *hubEvents) {
	for _, f := range *events {
		h.fanOut(f)
	}
	*events = nil
}

// ReportTurnFailure fans an OnTurnFailed event out to the listeners: the
// public entry point for client turn handlers that trapped an error, so it
// reaches the audit log and any tenant listener without touching hub
// internals. AgentClient reports its handler failures through it.
func (h *Hub) ReportTurnFailure(channelID, agentID, envelopeID string, err error) {
	h.fanOut(func(l HubListener) { l.OnTurnFailed(channelID, agentID, envelopeID, err) })
}

// at stamps listener payloads with the hub clock.
func (h *Hub) at() time.Time { return h.now() }
