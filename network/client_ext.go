package network

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/luckystrker/ago/plugin"
	"github.com/luckystrker/ago/task"
)

// --- per-channel feeds (WaitForChannelEvent) -----------------------------------

// channelFeedCap bounds one channel's queue; beyond it the oldest envelope is
// dropped. AGO-DEVIATION: Python's per-channel queue is unbounded; a bounded
// one keeps an agent that never waits from growing without limit.
const channelFeedCap = 1024

// channelFeed is the FIFO of the envelopes delivered on one channel (up to
// channelFeedCap), with a wake-up signal for the waiter.
type channelFeed struct {
	mu    sync.Mutex
	queue []Envelope
	wake  chan struct{}
}

// channelFeeds holds one feed per channel a client has seen or was asked to
// track. dropped counts envelopes evicted from a full feed.
type channelFeeds struct {
	mu      sync.Mutex
	feeds   map[string]*channelFeed
	dropped atomic.Int64
	warned  atomic.Bool
}

func (f *channelFeeds) ensure(channelID string) *channelFeed {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.feeds == nil {
		f.feeds = make(map[string]*channelFeed)
	}
	feed := f.feeds[channelID]
	if feed == nil {
		feed = &channelFeed{wake: make(chan struct{}, 1)}
		f.feeds[channelID] = feed
	}
	return feed
}

func (f *channelFeeds) discard(channelID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.feeds, channelID)
}

// push appends env to the feed of its channel, creating the feed on first
// delivery (Python parity: receive creates the per-channel inbox), so a reply
// that lands before anyone waits is not lost.
func (f *channelFeeds) push(env Envelope) {
	feed := f.ensure(env.ChannelID)
	feed.mu.Lock()
	evicted := len(feed.queue) >= channelFeedCap
	if evicted {
		feed.queue = feed.queue[1:]
	}
	feed.queue = append(feed.queue, env)
	feed.mu.Unlock()
	if evicted {
		f.dropped.Add(1)
		// One warning per client; the counter carries the running total.
		if f.warned.CompareAndSwap(false, true) {
			slog.Warn("channel feed full: dropping the oldest envelope; consume it with WaitForChannelEvent "+
				"or release the channel with DiscardChannelInbox",
				"channel", env.ChannelID, "cap", channelFeedCap)
		}
	}
	select {
	case feed.wake <- struct{}{}:
	default:
	}
}

// wait pops envelopes until pred accepts one; the rest are discarded (Python
// parity). It fails with ctx's error or ErrClosed once closed is signalled.
func (feed *channelFeed) wait(ctx context.Context, closed <-chan struct{}, pred func(Envelope) bool) (Envelope, error) {
	for {
		for {
			feed.mu.Lock()
			if len(feed.queue) == 0 {
				feed.mu.Unlock()
				break
			}
			env := feed.queue[0]
			feed.queue = feed.queue[1:]
			feed.mu.Unlock()
			if pred == nil || pred(env) {
				return env, nil
			}
		}
		select {
		case <-feed.wake:
		case <-ctx.Done():
			return Envelope{}, ctx.Err()
		case <-closed:
			return Envelope{}, ErrClosed
		}
	}
}

// EnsureChannelInbox makes sure channelID's queue exists. Every envelope
// delivered on a channel — protocol and task envelopes included — is queued
// from its first delivery (and from CreateChannel on), so open → say → wait
// never loses a fast reply; this call only matters for a channel the client
// has not seen yet. Idempotent. The queue is bounded (channelFeedCap) and
// freed by DiscardChannelInbox.
func (a *AgentClient) EnsureChannelInbox(channelID string) { a.feeds.ensure(channelID) }

// ChannelFeedDropped is the number of envelopes evicted, oldest first, from
// full per-channel queues (channelFeedCap each) since the client was built.
// It stays 0 while WaitForChannelEvent keeps up or channels are discarded.
func (a *AgentClient) ChannelFeedDropped() int64 { return a.feeds.dropped.Load() }

// ChannelFeedDropped is AgentClient.ChannelFeedDropped for a human.
func (h *HumanClient) ChannelFeedDropped() int64 { return h.feeds.dropped.Load() }

// DiscardChannelInbox stops tracking channelID and drops what it queued.
func (a *AgentClient) DiscardChannelInbox(channelID string) { a.feeds.discard(channelID) }

// WaitForChannelEvent blocks until an envelope delivered on channelID
// satisfies pred (nil = the next one), tracking the channel if needed.
// Envelopes that do not match are consumed. It fails with ctx's error on
// cancellation or timeout and with ErrClosed once the client is closed.
// Concurrent waits on one channel compete for the same envelopes.
func (a *AgentClient) WaitForChannelEvent(ctx context.Context, channelID string, pred func(Envelope) bool) (Envelope, error) {
	return a.feeds.ensure(channelID).wait(ctx, a.done, pred)
}

// EnsureChannelInbox is AgentClient.EnsureChannelInbox for a human.
func (h *HumanClient) EnsureChannelInbox(channelID string) { h.feeds.ensure(channelID) }

// DiscardChannelInbox is AgentClient.DiscardChannelInbox for a human.
func (h *HumanClient) DiscardChannelInbox(channelID string) { h.feeds.discard(channelID) }

// WaitForChannelEvent is AgentClient.WaitForChannelEvent for a human: a UI
// awaits a specific reply without wiring a callback.
func (h *HumanClient) WaitForChannelEvent(ctx context.Context, channelID string, pred func(Envelope) bool) (Envelope, error) {
	return h.feeds.ensure(channelID).wait(ctx, h.done, pred)
}

// --- agent: SetRule, ResumePendingTurns ----------------------------------------------

// SetRule replaces the agent's stored rule while registered; access globs,
// delegation depth, rate and TTL defaults apply to the next send. A new Rate
// starts with a full bucket. The delivery buffer keeps its size.
func (h *Hub) SetRule(agentID string, r Rule) error {
	if err := r.Validate(); err != nil {
		return err
	}
	var evs hubEvents
	defer h.flushEvents(&evs) // runs after the unlock below
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.passports[agentID]; !ok {
		return fmt.Errorf("%w: agent %s", ErrNotFound, agentID)
	}
	cur := r.Clone()
	if err := h.persistRuleLocked(agentID, cur); err != nil {
		return err
	}
	h.rules[agentID] = cur
	delete(h.ext.rateBuckets, agentID)
	evs.add(func(l HubListener) {
		l.OnAgentEvent(agentID, AgentEventRuleSet, map[string]any{"at": h.at()})
	})
	return nil
}

// SetRule changes this agent's own rule (Hub.SetRule) and keeps the client's
// copy in sync.
func (a *AgentClient) SetRule(r Rule) error {
	if err := a.hub.SetRule(a.AgentID(), r); err != nil {
		return err
	}
	a.ruleMu.Lock()
	a.rule = r.Clone()
	a.ruleMu.Unlock()
	return nil
}

// bookInbox counts one envelope against the agent's pending inbox outside a
// dispatch (a redelivery).
func (h *Hub) bookInbox(agentID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.passports[agentID]; ok {
		h.ext.inboxPending[agentID]++
	}
}

// ResumePendingTurns re-fires the turns the protocol still expects from this
// agent — after a restart nothing else would re-process the envelope a turn
// is waiting on. It asks the hub for PendingTurnsFor, loads each turn's
// triggering envelope from the channel log and feeds it back through the
// delivery path (so the handler, suppression and channel feeds all apply).
// Turns without a triggering envelope — a freshly opened channel whose
// creator has nothing to react to — are skipped. It returns the number of
// turns re-fired.
//
// Safe under at-least-once delivery: the handler skips an envelope whose
// reply already landed (Hub.FindEnvelopeByCausation), so a turn is never
// answered twice.
func (a *AgentClient) ResumePendingTurns() (int, error) {
	fired := 0
	for _, turn := range a.hub.PendingTurnsFor(a.AgentID()) {
		if turn.TriggeringEnvelopeID == "" {
			continue
		}
		env, ok := a.hub.envelopeByID(turn.ChannelID, turn.TriggeringEnvelopeID)
		if !ok {
			continue
		}
		if substantive(env) {
			a.hub.bookInbox(a.AgentID())
		}
		a.deliver(env)
		fired++
	}
	return fired, nil
}

// --- human: push callbacks and filtered pull -------------------------------------------

// envelopeCallback is one registered push callback; the pointer is its
// identity.
type envelopeCallback struct{ fn func(Envelope) }

// OnEnvelope registers fn to run for every envelope delivered to this human
// (protocol lifecycle included), in registration order, on the sender's
// goroutine: it must not block. A panic is logged and skipped. It returns the
// function that removes the callback.
func (h *HumanClient) OnEnvelope(fn func(Envelope)) (remove func()) {
	cb := &envelopeCallback{fn: fn}
	h.cbMu.Lock()
	h.callbacks = append(h.callbacks, cb)
	h.cbMu.Unlock()
	return func() {
		h.cbMu.Lock()
		defer h.cbMu.Unlock()
		for i, have := range h.callbacks {
			if have == cb {
				h.callbacks = append(h.callbacks[:i:i], h.callbacks[i+1:]...)
				return
			}
		}
	}
}

func (h *HumanClient) runCallbacks(env Envelope) {
	h.cbMu.Lock()
	cbs := append([]*envelopeCallback(nil), h.callbacks...)
	h.cbMu.Unlock()
	for _, cb := range cbs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("human envelope callback panicked", "agent", h.AgentID(), "panic", r)
				}
			}()
			cb.fn(env)
		}()
	}
}

// ForChannel is the predicate matching envelopes of one channel, for
// NextEnvelope and WaitForChannelEvent.
func ForChannel(channelID string) func(Envelope) bool {
	return func(e Envelope) bool { return e.ChannelID == channelID }
}

// NextEnvelope blocks until the next inbound envelope satisfying pred
// (nil = any) is pulled from the queue. Envelopes that do not match are
// discarded — register OnEnvelope as well to see both. It fails with ctx's
// error on cancellation and with ErrClosed once the client is closed.
func (h *HumanClient) NextEnvelope(ctx context.Context, pred func(Envelope) bool) (Envelope, error) {
	for {
		env, err := h.Recv(ctx)
		if err != nil {
			return Envelope{}, err
		}
		if pred == nil || pred(env) {
			return env, nil
		}
	}
}

// --- NetworkPlugin and identity line ---------------------------------------------------

// IdentityLine is the sentence that names the agent to its model.
func (a *AgentClient) IdentityLine() string {
	return fmt.Sprintf("You are %s (agent_id: %s).", a.Passport.Name, a.AgentID())
}

// NetworkPluginName is the name of the plugin NewNetworkPlugin returns.
const NetworkPluginName = "network"

// NewNetworkPlugin attaches an agent to the network in one call: the five
// identity-level tools of ClientTools (peers, channels, tasks, context,
// delegate) plus the identity line (IdentityLine) as a system-prompt
// fragment, bundled as a plugin.Plugin an agent applies at construction
// (agent.WithPlugin). Channel-level tools such as say are not included —
// they come from AgentClient.ChannelTools per turn, so protocol-specific
// verbs stay hidden when they do not apply.
//
// AGO-DEVIATION: Python's NetworkContextPolicy is an assembly policy that
// prepends the line on every LLM call; the plugin package has no policy slot
// (D-044), so the line rides as a static prompt fragment.
func NewNetworkPlugin(hc *HubClient, self *AgentClient) (plugin.Plugin, error) {
	ts, err := ClientTools(hc, self)
	if err != nil {
		return nil, err
	}
	b := plugin.NewBundle(NetworkPluginName).WithPrompt(self.IdentityLine())
	for _, t := range ts {
		b.WithTool(t)
	}
	return b.Plugin(), nil
}

// --- ClientTask ----------------------------------------------------------------------

// ClientTask is a typed handle on a task the hub observes: id, owner, state,
// with Info refreshing from the hub.
type ClientTask struct {
	hub *Hub

	mu sync.Mutex
	md task.Metadata
}

// Task returns a handle on an observed task, loading its metadata; unknown
// ids fail with ErrTaskNotFound.
func (a *AgentClient) Task(taskID string) (*ClientTask, error) {
	md, err := a.hub.Task(taskID)
	if err != nil {
		return nil, err
	}
	return &ClientTask{hub: a.hub, md: md}, nil
}

// TaskID is the task's id.
func (t *ClientTask) TaskID() string { return t.snapshot().TaskID }

// OwnerID is the registration id of the agent that runs the task.
func (t *ClientTask) OwnerID() string { return t.snapshot().OwnerID }

// State is the state at the last load or Info.
func (t *ClientTask) State() task.State { return t.snapshot().State }

// Metadata is a copy of the metadata at the last load or Info.
func (t *ClientTask) Metadata() task.Metadata { return t.snapshot() }

// Info re-fetches the metadata from the hub and returns it.
func (t *ClientTask) Info() (task.Metadata, error) {
	md, err := t.hub.Task(t.TaskID())
	if err != nil {
		return task.Metadata{}, err
	}
	t.mu.Lock()
	t.md = md
	t.mu.Unlock()
	return md, nil
}

func (t *ClientTask) snapshot() task.Metadata {
	t.mu.Lock()
	defer t.mu.Unlock()
	return cloneTaskMetadata(t.md)
}
