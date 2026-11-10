package network

import (
	"fmt"
	"sort"
	"strings"
)

// ChannelQuery filters Hub.QueryChannels.
type ChannelQuery struct {
	AgentID string       // only channels this agent participates in
	State   ChannelState // only channels in this state
	Limit   int          // 0 = no limit
}

// QueryChannels returns snapshot copies of the channels matching q, oldest
// first. The zero query matches every channel (ListChannels' view, ordered).
func (h *Hub) QueryChannels(q ChannelQuery) []*ChannelHandle {
	h.mu.Lock()
	defer h.mu.Unlock()
	matched := make([]*ChannelHandle, 0, len(h.channels))
	for _, ch := range h.channels {
		if q.State != "" && ch.State != q.State {
			continue
		}
		if q.AgentID != "" && !ch.hasParticipant(q.AgentID) {
			continue
		}
		matched = append(matched, ch)
	}
	sort.Slice(matched, func(i, j int) bool {
		if !matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			return matched[i].CreatedAt.Before(matched[j].CreatedAt)
		}
		return matched[i].ID < matched[j].ID
	})
	if q.Limit > 0 && len(matched) > q.Limit {
		matched = matched[:q.Limit]
	}
	out := make([]*ChannelHandle, len(matched))
	for i, ch := range matched {
		out[i] = snapshotHandle(ch)
	}
	return out
}

// ReadWALRange returns the accepted envelopes of a channel at positions
// [since, until) in accept order. A negative until reads to the end; a
// negative since reads from the start; positions past the end clamp.
func (h *Hub) ReadWALRange(channelID string, since, until int) ([]Envelope, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	log, ok := h.logs[channelID]
	if !ok {
		if _, exists := h.channels[channelID]; !exists {
			return nil, fmt.Errorf("%w: channel %s", ErrNotFound, channelID)
		}
		return nil, nil
	}
	if since < 0 {
		since = 0
	}
	if until < 0 || until > len(log) {
		until = len(log)
	}
	if since >= until {
		return nil, nil
	}
	return append([]Envelope(nil), log[since:until]...), nil
}

// Agent sort orders for AgentQuery.SortBy.
const (
	SortByRegistration = "" // registration order
	SortByName         = "name"
)

// AgentQuery filters Hub.QueryAgents.
type AgentQuery struct {
	Capability string       // claimed or observed capability tag, exact
	Query      string       // case-insensitive substring of the resume summary
	Kind       PassportKind // "" = every kind; KindAgent also matches the "" alias
	SortBy     string       // SortByRegistration (default) | SortByName
	Limit      int          // 0 = no limit
}

// QueryAgents lists the registrations matching q. Python parity: Query
// matches the resume summary only, and Capability uses the claimed and
// observed capability set.
func (h *Hub) QueryAgents(q AgentQuery) []Passport {
	h.mu.Lock()
	defer h.mu.Unlock()
	needle := strings.ToLower(q.Query)
	var out []Passport
	for id, p := range h.passports {
		if q.Kind != "" && p.Kind.Effective() != q.Kind.Effective() {
			continue
		}
		resume, hasResume := h.resumes[id]
		if q.Capability != "" {
			if !hasResume {
				continue
			}
			if _, ok := resume.allCapabilities()[q.Capability]; !ok {
				continue
			}
		}
		if needle != "" && !strings.Contains(strings.ToLower(resume.Summary), needle) {
			continue
		}
		out = append(out, p)
	}
	if q.SortBy == SortByName {
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	} else {
		sort.Slice(out, func(i, j int) bool { return h.ext.regSeq[out[i].AgentID] < h.ext.regSeq[out[j].AgentID] })
	}
	if q.Limit > 0 && len(out) >= q.Limit {
		out = out[:q.Limit]
	}
	return out
}

// SendProber is an optional adapter extension: a pure validation of whether
// the adapter would accept env from env.SenderID against the channel's
// current state. It must not mutate state. Hub.CanSend uses it.
type SendProber interface {
	CanSend(ch *ChannelHandle, env Envelope) error
}

// CanSend is the non-mutating "would this send be accepted?" probe: the
// channel is active, senderID participates, and the channel's adapter would
// take a substantive envelope of eventType ("" = EnvText) now — e.g. it is
// the sender's turn. It checks protocol shape only; access rules, rate and
// inbox limits are not consulted and nothing is recorded or counted. An
// adapter without SendProber is assumed to accept.
func (h *Hub) CanSend(channelID, senderID, eventType string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch, ok := h.channels[channelID]
	if !ok || ch.State != ChannelActive || !ch.hasParticipant(senderID) || ch.adapter == nil {
		return false
	}
	prober, ok := ch.adapter.(SendProber)
	if !ok {
		return true
	}
	if eventType == "" {
		eventType = EnvText
	}
	probe := Envelope{ChannelID: channelID, SenderID: senderID, EventType: eventType, Payload: mustJSON(map[string]string{"text": ""})}
	return prober.CanSend(ch, probe) == nil
}

// CanSend implements SendProber: the initiator before the question, the
// respondent before the reply.
func (ConsultingAdapter) CanSend(ch *ChannelHandle, env Envelope) error {
	if protocolEnvelope(env) || env.EventType != EnvText {
		return nil
	}
	st, ok := ch.AdapterState.(*consultingState)
	if !ok {
		return fmt.Errorf("%w: consulting channel %s has no folded state", ErrProtocol, ch.ID)
	}
	return consultingGate(ch, st, env.SenderID)
}

// CanSend implements SendProber: any participant may speak at any time.
func (ConversationAdapter) CanSend(ch *ChannelHandle, env Envelope) error {
	if !ch.hasParticipant(env.SenderID) {
		return fmt.Errorf("%w: conversation channel %s only accepts sends from participants", ErrProtocol, ch.ID)
	}
	return nil
}

// CanSend implements SendProber: only the speaker the rotation expects.
func (DiscussionAdapter) CanSend(ch *ChannelHandle, env Envelope) error {
	if protocolEnvelope(env) || env.EventType != EnvText {
		return nil
	}
	st, ok := ch.AdapterState.(*discussionState)
	if !ok {
		return fmt.Errorf("%w: discussion channel %s has no folded state", ErrProtocol, ch.ID)
	}
	return discussionGate(ch, st, env.SenderID)
}

// CanSend implements SendProber: only the expected speaker, and not after the
// workflow terminated. Standalone context updates are always allowed.
func (WorkflowAdapter) CanSend(ch *ChannelHandle, env Envelope) error {
	if env.EventType == EnvContextSet {
		return nil
	}
	if protocolEnvelope(env) || (env.EventType != EnvText && env.EventType != EnvPacket) {
		return nil
	}
	st, ok := ch.AdapterState.(*workflowState)
	if !ok {
		return fmt.Errorf("%w: workflow channel %s has no folded state", ErrProtocol, ch.ID)
	}
	return workflowTurnGate(ch, st, env.SenderID)
}

var (
	_ SendProber = ConsultingAdapter{}
	_ SendProber = ConversationAdapter{}
	_ SendProber = DiscussionAdapter{}
	_ SendProber = WorkflowAdapter{}
)

// --- causation index ---------------------------------------------------------------

// causeKey identifies an accepted reply by its sender and the envelope it
// answers.
type causeKey struct{ sender, causation string }

// indexCausationLocked records env under (channel, sender, causation) so a
// retry of the same logical reply can be found. logPos is env's position in
// the channel log.
func (h *Hub) indexCausationLocked(env Envelope, logPos int) {
	if env.CausationID == "" || env.ID == "" {
		return
	}
	byChannel := h.ext.causation[env.ChannelID]
	if byChannel == nil {
		byChannel = make(map[causeKey]int)
		h.ext.causation[env.ChannelID] = byChannel
	}
	byChannel[causeKey{env.SenderID, env.CausationID}] = logPos
}

// FindEnvelopeByCausation returns the envelope senderID previously posted in
// the channel in answer to causationID — the retry-dedupe lookup: after an
// at-least-once redelivery a handler checks it and skips work whose reply
// already landed. When a sender posted several replies to one causation the
// latest wins. ok is false when none was accepted or the channel has closed
// (the index is dropped with the channel).
func (h *Hub) FindEnvelopeByCausation(channelID, senderID, causationID string) (Envelope, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	pos, ok := h.ext.causation[channelID][causeKey{senderID, causationID}]
	if !ok {
		return Envelope{}, false
	}
	log := h.logs[channelID]
	if pos < 0 || pos >= len(log) {
		return Envelope{}, false
	}
	return log[pos], true
}
