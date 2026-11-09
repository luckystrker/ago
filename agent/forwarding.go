package agent

import (
	"strings"
	"sync"

	"github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/internal/uuid"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/usage"
)

// forwarderDependencyKey is the dependency under which a turn's forwarder is
// visible to nested delegations. A grandchild asks its parent turn for this
// handle to extend the delegation path it stamps on forwarded events.
const forwarderDependencyKey = "ago:forwarder"

// forwarder relays one child stream's events to its parent stream, preserving
// the event tree (upstream parity: tools/subagents/_forwarding.py Forwarder).
// It is framework-internal: applications observe the tree through the parent
// stream and never address a forwarder directly.
//
// Operational events travel upward as ChildEvent/TransientChildEvent
// observations: an ancestor's observers see them, but the ancestor's model
// loop, tool executor and HITL subscribers do not react to them. Inert events
// (UsageEvent and Task*) travel as themselves with delegation provenance: the
// first hop stamps path (and a UsageEvent's task/agent), later hops keep it.
//
// A forwarder is immutable after construction except for its subscription id,
// and is safe for concurrent use: the child stream may dispatch from several
// goroutines.
type forwarder struct {
	parent    *context.Context
	stream    stream.Stream
	taskID    string
	agentName string
	path      []string
	sub       stream.SubID

	accountingMu sync.Mutex
	// accounting retains source UsageEvents relayed (or best-effort persisted)
	// by this invocation. TaskCompleted.Usage combines it with the child
	// history, so spend retained only on the parent side after a delivery
	// failure is still visible in the snapshot (upstream history + accounting).
	accounting []events.Event
}

// newForwarder builds the relay for one delegation. The path extends the
// parent turn's forwarding path, so a leaf's events carry every delegation id
// from the root down; a local delegation path is just its own task id.
func newForwarder(parent *context.Context, childStream stream.Stream, taskID, agentName string) *forwarder {
	path := []string{taskID}
	if parent != nil {
		if v, ok := parent.Dependency(forwarderDependencyKey); ok {
			if ancestor, ok := v.(*forwarder); ok && len(ancestor.path) > 0 {
				path = append(append([]string(nil), ancestor.path...), taskID)
			}
		}
	}
	return &forwarder{
		parent:    parent,
		stream:    childStream,
		taskID:    taskID,
		agentName: agentName,
		path:      path,
	}
}

// start installs the relay as an interrupt subscriber. Interrupt placement
// matters: a child HITL hook swallows its question as ErrHandled, and the relay
// must still observe that question for the parent (upstream registers its
// forwarder with interrupt=True for the same reason).
//
// It is called by AgentRun.open after the per-stream turn lock is held; the
// matching stop runs in AgentRun.Close before the lock is released.
func (f *forwarder) start() {
	if f == nil || f.stream == nil || f.parent == nil || f.parent.Stream == nil {
		return
	}
	if f.stream.ID() == f.parent.Stream.ID() {
		// A delegation that reuses its caller's stream is already observable
		// there; forwarding would echo every event back to the same stream.
		return
	}
	f.sub = f.stream.Subscribe(f.forward, stream.SubscribeOptions{Interrupt: true})
}

// stop removes the relay.
func (f *forwarder) stop() {
	if f == nil || f.sub == 0 {
		return
	}
	f.stream.Unsubscribe(f.sub)
	f.sub = 0
}

// publish sends an event authored by the child on its stream. Once the turn
// scope is open the ordinary stream path persists and the interrupt relay
// forwards it; a failure before the scope opened (for example a variable/merge
// error) has no subscription yet, so the event is relayed directly.
func (f *forwarder) publish(childStream stream.Stream, started bool, ev events.Event) error {
	if f == nil || f.parent == nil || f.parent.Stream == nil {
		return childStream.Send(ev)
	}
	if started || childStream.ID() == f.parent.Stream.ID() {
		return childStream.Send(ev)
	}
	return f.forward(ev)
}

// forward is the subscriber. A failure to publish upward fails the child Send,
// matching upstream: a parent that cannot observe the tree is a broken turn,
// not a silently truncated one. Accounting is best-effort retained on the
// parent: if the upward publication fails, the source UsageEvent is appended to
// the parent history so the spend is not lost from both transcripts (upstream
// _persist_accounting).
func (f *forwarder) forward(ev events.Event) error {
	if f == nil || f.parent == nil || f.parent.Stream == nil || ev == nil {
		return nil
	}
	if f.stream != nil && f.stream.ID() == f.parent.Stream.ID() {
		return nil
	}
	switch e := events.AsValue(ev).(type) {
	case events.ChildEvent:
		// An observation from a deeper delegation keeps the leaf's task id,
		// path and agent name; this hop only relays it.
		cp := e
		return f.parent.Stream.Send(&cp)
	case events.TransientChildEvent:
		cp := e
		return f.parent.Stream.Send(&cp)
	}
	if events.IsInert(ev) {
		// Inert events keep their identity and storage policy. Send through the
		// stream, not Context.Send: AsLocal would strip the provenance and
		// replace a UsageEvent's source id.
		stamped := events.StampInert(ev, clonePath(f.path), f.taskID, f.agentName)
		if _, isUsage := usage.EventFrom(stamped); isUsage {
			f.accountingMu.Lock()
			f.accounting = append(f.accounting, stamped)
			f.accountingMu.Unlock()
		}
		if err := f.parent.Stream.Send(stamped); err != nil {
			if u, isUsage := usage.EventFrom(stamped); isUsage {
				f.persistAccounting(stamped, u.EventID)
			}
			return err
		}
		return nil
	}

	inner := events.AsValue(ev)
	originID := strings.ReplaceAll(uuid.New(), "-", "")
	ce := events.NewChildEvent(inner, f.taskID, clonePath(f.path), f.agentName, originID)
	if inner.Transient() {
		return f.parent.Stream.Send(&events.TransientChildEvent{ChildEvent: *ce})
	}
	return f.parent.Stream.Send(ce)
}

// usage reports this delegation's cumulative spend: the child transcript plus
// source records this relay retained while publishing upward. Both sources
// carry the same event ids, so usage.FromEvents deduplicates a record that
// exists on both sides.
func (f *forwarder) usage(childStream stream.Stream) events.Usage {
	evs, err := childStream.History().GetEvents()
	if err != nil {
		evs = nil
	}
	f.accountingMu.Lock()
	acc := make([]events.Event, len(f.accounting))
	copy(acc, f.accounting)
	f.accountingMu.Unlock()
	return usage.FromEvents(append(evs, acc...)).Total
}

// persistAccounting retains a source UsageEvent in the parent history when the
// upward publication failed: the spend must survive in at least one transcript
// (upstream _persist_accounting). A record already written before a subscriber
// error is not appended twice; readers also deduplicate by event_id.
func (f *forwarder) persistAccounting(ev events.Event, eventID string) {
	if f == nil || f.parent == nil || f.parent.Stream == nil {
		return
	}
	if eventID != "" {
		existing, err := f.parent.Stream.History().GetEvents()
		if err != nil {
			return
		}
		for _, have := range existing {
			if u, ok := usage.EventFrom(have); ok && u.EventID == eventID {
				return
			}
		}
	}
	_ = f.parent.Stream.History().Append(ev)
}

func clonePath(path []string) []string {
	if path == nil {
		return nil
	}
	out := make([]string, len(path))
	copy(out, path)
	return out
}
