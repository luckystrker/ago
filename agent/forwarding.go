package agent

import (
	"strings"

	"github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/internal/uuid"
	"github.com/luckystrker/ago/stream"
)

// forwarderDependencyKey is the dependency under which a turn's Forwarder is
// visible to nested delegations. A grandchild asks its parent turn for this
// handle to extend the delegation path it stamps on forwarded events.
const forwarderDependencyKey = "ago:forwarder"

// Forwarder relays one child stream's events to its parent stream, preserving
// the event tree (upstream parity: tools/subagents/_forwarding.py Forwarder).
//
// Operational events travel upward as ChildEvent/TransientChildEvent
// observations: an ancestor's observers see them, but the ancestor's model
// loop, tool executor and HITL subscribers do not react to them. Inert events
// (UsageEvent and Task*) travel as themselves with delegation provenance: the
// first hop stamps path (and a UsageEvent's task/agent), later hops keep it.
//
// A Forwarder is immutable after construction except for its subscription id,
// and is safe for concurrent use: the child stream may dispatch from several
// goroutines.
type Forwarder struct {
	parent    *context.Context
	stream    stream.Stream
	taskID    string
	agentName string
	path      []string
	sub       stream.SubID
}

// newForwarder builds the relay for one delegation. The path extends the
// parent turn's forwarding path, so a leaf's events carry every delegation id
// from the root down; a local delegation path is just its own task id.
func newForwarder(parent *context.Context, childStream stream.Stream, taskID, agentName string) *Forwarder {
	path := []string{taskID}
	if parent != nil {
		if v, ok := parent.Dependency(forwarderDependencyKey); ok {
			if ancestor, ok := v.(*Forwarder); ok && len(ancestor.path) > 0 {
				path = append(append([]string(nil), ancestor.path...), taskID)
			}
		}
	}
	return &Forwarder{
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
func (f *Forwarder) start() {
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
func (f *Forwarder) stop() {
	if f == nil || f.sub == 0 {
		return
	}
	f.stream.Unsubscribe(f.sub)
	f.sub = 0
}

// forward is the subscriber. A failure to publish upward fails the child Send,
// matching upstream: a parent that cannot observe the tree is a broken turn,
// not a silently truncated one.
func (f *Forwarder) forward(ev events.Event) error {
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
		return f.parent.Stream.Send(events.StampInert(ev, f.path, f.taskID, f.agentName))
	}

	inner := events.AsValue(ev)
	originID := ""
	if u, ok := events.SourceEvent(inner).(events.UsageEvent); ok {
		originID = u.EventID
	}
	if originID == "" {
		originID = strings.ReplaceAll(uuid.New(), "-", "")
	}
	ce := events.NewChildEvent(inner, f.taskID, f.path, f.agentName, originID)
	if inner.Transient() {
		return f.parent.Stream.Send(&events.TransientChildEvent{ChildEvent: *ce})
	}
	return f.parent.Stream.Send(ce)
}
