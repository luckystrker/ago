package network

import (
	"encoding/json"
	"errors"
	"sort"
	"sync"

	"github.com/luckystrker/ago/network/transport"
)

// Store-and-forward delivery to wire-bound agents (wave-4 invariants 1–4,
// spec/tla/HubCore.tla).
//
// Every accepted envelope is in the channel log (and the WAL); delivery is a
// walk along that log. Each (connection, agent) owns one agentSender holding,
// per channel, a pointer into the log: live dispatch and replay after a
// (re)attach are the same loop advancing that pointer in log order, so a live
// envelope can neither overtake a pending replay nor be sent twice by it
// (model mutation D15). The walk of a channel starts strictly after
// max(persisted cursor, the client's since mark), so nothing is replayed that
// the client already acknowledged (D1, D14), and the filter — participant,
// audience, not the sender, never a hub-absorbed protocol envelope — is one
// function for both paths.

// agentSender is the ordered sender of one agent on one connection. It is
// created when the connection binds the agent (register, or an authenticated
// hello) and dies with the binding, so a reconnect always starts a fresh walk
// and clears a nack stall.
type agentSender struct {
	mu      sync.Mutex
	since   HelloSince
	ptr     map[string]int  // channel id → index of the next log entry to consider
	started map[string]bool // ptr initialised from the cursor/since mark
	stalled map[string]bool // channel id → a nack stopped delivery until reconnect
	// liveFrom is, per channel, the id of the first envelope whose dispatch
	// triggered this sender: from there on entries are live, before it they are
	// replay (backlog from before the agent was reachable).
	liveFrom map[string]string
}

func newAgentSender(since HelloSince) *agentSender {
	return &agentSender{since: since, ptr: map[string]int{}, started: map[string]bool{}, stalled: map[string]bool{}, liveFrom: map[string]string{}}
}

// senderOf returns the sender of agentID on ep (nil when ep does not hold it).
func (h *Hub) senderOf(ep *wireEndpoint, agentID string) *agentSender {
	w := &h.ext.wire
	w.mu.Lock()
	defer w.mu.Unlock()
	if !ep.bound[agentID] {
		return nil
	}
	return ep.senders[agentID]
}

// markLive records that env's dispatch reached the sender: it and every later
// entry of its channel are live.
func (s *agentSender) markLive(env Envelope) {
	s.mu.Lock()
	if _, ok := s.liveFrom[env.ChannelID]; !ok {
		s.liveFrom[env.ChannelID] = env.ID
	}
	s.mu.Unlock()
}

// deliverableLocked reports whether env is addressed to agentID (a participant
// of ch). It is the single send filter of live dispatch and replay; live says
// whether the entry is being dispatched live (true) or replayed (false).
func (h *Hub) deliverableLocked(env Envelope, ch *ChannelHandle, agentID string, live bool) bool {
	if !ch.hasParticipant(agentID) {
		return false
	}
	switch env.EventType {
	case EnvChannelAck, EnvChannelReject:
		// AGO-DEVIATION: the hub absorbs invite acks and rejects and never
		// dispatches them; Python's replay would hand them to their audience.
		// Live and replay must agree (one filter), so they are never delivered.
		return false
	}
	if env.Audience != "" && env.Audience != agentID {
		return false
	}
	if env.SenderID == agentID && !(live && isLifecycleEvent(env.EventType)) {
		// Nobody is sent her own envelope on replay (Python: sender != agent).
		// Live, lifecycle broadcasts (opened/closed/expired) still address the
		// creator too (Python's live dispatch does); a creator that was away
		// when her channel opened does not need it — she made the channel.
		return false
	}
	// Inbound access policy, as at live dispatch: a blocked recipient is
	// skipped silently.
	if env.SenderID != agentID {
		if sender, ok := h.passports[env.SenderID]; ok {
			if p, ok := h.passports[agentID]; ok {
				if d := h.arbiter.AuthorizeDispatch(env, sender, p, h.rules[agentID]); !d.Allowed {
					return false
				}
			}
		}
	}
	return true
}

// participantChannelsLocked lists the channels agentID participates in, in
// creation order. AGO-DEVIATION: terminal (closed, expired) channels are
// included, whereas Python's replay walks only active channels: an envelope
// that arrived while the agent was offline — a final message, the closed
// broadcast — is still owed to it (invariant 1), and once acked the cursor
// keeps it from being replayed again.
func (h *Hub) participantChannelsLocked(agentID string) []string {
	var chans []*ChannelHandle
	for _, ch := range h.channels {
		if ch.hasParticipant(agentID) {
			chans = append(chans, ch)
		}
	}
	sort.Slice(chans, func(i, j int) bool {
		if !chans[i].CreatedAt.Equal(chans[j].CreatedAt) {
			return chans[i].CreatedAt.Before(chans[j].CreatedAt)
		}
		return chans[i].ID < chans[j].ID
	})
	out := make([]string, len(chans))
	for i, ch := range chans {
		out[i] = ch.ID
	}
	return out
}

// pump delivers every pending log entry addressed to agentID over ep, channel
// by channel, each channel strictly in log order. It is called at bind (replay),
// for every live dispatch to the agent and after every receipt (flow control:
// a link that was full resumes when the client has drained it).
//
// A send failure ends the walk of that channel for this call and reports
// OnDispatchFailed (Python: a replay failure stops that channel's replay): the
// entry is not skipped — the pointer stays — so a full link resumes where it
// stopped, and a dead link releases the connection and leaves the cursor for the
// next reconnect to replay from.
func (h *Hub) pump(ep *wireEndpoint, agentID string) {
	s := h.senderOf(ep, agentID)
	if s == nil {
		return
	}
	type failure struct {
		env Envelope
		err error
	}
	var failures []failure
	dead := false
	s.mu.Lock()
	h.mu.Lock()
	channels := h.participantChannelsLocked(agentID)
	h.mu.Unlock()
	for _, chID := range channels {
		if s.stalled[chID] {
			continue
		}
		for {
			h.mu.Lock()
			ch, log := h.channels[chID], h.logs[chID]
			if ch == nil {
				h.mu.Unlock()
				break
			}
			if !s.started[chID] {
				mark := h.ext.cursors[agentID][chID]
				if v, ok := s.since.Lookup(chID); ok && v > mark {
					mark = v
				}
				s.ptr[chID] = sort.Search(len(log), func(i int) bool { return log[i].ID > mark })
				s.started[chID] = true
			}
			i := s.ptr[chID]
			if i >= len(log) {
				h.mu.Unlock()
				break
			}
			env := log[i]
			from, isLive := s.liveFrom[chID]
			send := h.deliverableLocked(env, ch, agentID, isLive && env.ID >= from)
			h.mu.Unlock()
			if send {
				payload, err := json.Marshal(env)
				if err == nil {
					err = ep.link.Send(transport.Frame{
						Kind:        transport.FrameNotify,
						RecipientID: agentID, ChannelID: env.ChannelID, EnvelopeID: env.ID, Payload: payload,
					})
				}
				if err != nil {
					failures = append(failures, failure{env, err})
					if errors.Is(err, transport.ErrLinkClosed) {
						dead = true
					}
					break // stop this channel here; the pointer stays on env
				}
			}
			s.ptr[chID] = i + 1
		}
		if dead {
			break
		}
	}
	s.mu.Unlock()
	if dead {
		h.dropEndpoint(ep)
	}
	for _, f := range failures {
		h.fanOut(func(l HubListener) { l.OnDispatchFailed(f.env, agentID, f.err) })
	}
}

// stallChannel stops delivery of one channel to agentID on ep until the
// connection re-binds the agent (a nack: the client could not process the
// envelope; a later ack must not move the cursor past it — model D19).
func (h *Hub) stallChannel(ep *wireEndpoint, agentID, channelID string) {
	if s := h.senderOf(ep, agentID); s != nil {
		s.mu.Lock()
		s.stalled[channelID] = true
		s.mu.Unlock()
	}
}
