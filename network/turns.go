package network

import (
	"fmt"
	"sort"

	"github.com/luckystrker/ago/tools"
)

// ExpectedTurn names who the protocol expects to act next and what put the
// turn on them.
type ExpectedTurn struct {
	// AgentID is the participant expected to send the next substantive
	// envelope.
	AgentID string
	// TriggeringEnvelopeID is the envelope that put the turn on AgentID —
	// typically the previous speaker's send; "" when none did (a fresh
	// channel whose creator speaks first).
	TriggeringEnvelopeID string
}

// TurnExpecter is an optional adapter extension: the protocol-expected next
// speaker given the channel's folded state, or nil when the protocol expects
// nobody (free-form channels, a finished cycle). It must not mutate state.
type TurnExpecter interface {
	ExpectedNext(ch *ChannelHandle) *ExpectedTurn
}

// ExpectedNext implements TurnExpecter: initiator, then respondent, then
// nobody once the 1Q1R cycle is complete.
func (ConsultingAdapter) ExpectedNext(ch *ChannelHandle) *ExpectedTurn {
	st, ok := ch.AdapterState.(*consultingState)
	if !ok || len(ch.Participants) < 2 {
		return nil
	}
	switch {
	case !st.InitiatorSent:
		return &ExpectedTurn{AgentID: ch.Participants[0].AgentID, TriggeringEnvelopeID: st.LastEnvelopeID}
	case !st.RespondentReplied:
		return &ExpectedTurn{AgentID: ch.Participants[1].AgentID, TriggeringEnvelopeID: st.LastEnvelopeID}
	}
	return nil
}

// ExpectedNext implements TurnExpecter: a conversation has no turn order.
func (ConversationAdapter) ExpectedNext(*ChannelHandle) *ExpectedTurn { return nil }

// ExpectedNext implements TurnExpecter: the rotation's next speaker.
func (DiscussionAdapter) ExpectedNext(ch *ChannelHandle) *ExpectedTurn {
	st, ok := ch.AdapterState.(*discussionState)
	if !ok || st.Expected == "" {
		return nil
	}
	return &ExpectedTurn{AgentID: st.Expected, TriggeringEnvelopeID: st.LastEnvelopeID}
}

// ExpectedNext implements TurnExpecter: the speaker the graph selected.
func (WorkflowAdapter) ExpectedNext(ch *ChannelHandle) *ExpectedTurn {
	st, ok := ch.AdapterState.(*workflowState)
	if !ok || st.Expected == "" {
		return nil
	}
	return &ExpectedTurn{AgentID: st.Expected, TriggeringEnvelopeID: st.LastEnvelopeID}
}

var (
	_ TurnExpecter = ConsultingAdapter{}
	_ TurnExpecter = ConversationAdapter{}
	_ TurnExpecter = DiscussionAdapter{}
	_ TurnExpecter = WorkflowAdapter{}
)

// ExpectedNext reports who the channel's protocol expects to speak next. It
// returns (nil, nil) for a channel that is not active or whose adapter
// expects nobody or cannot say, and ErrNotFound for an unknown channel.
func (h *Hub) ExpectedNext(channelID string) (*ExpectedTurn, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch, ok := h.channels[channelID]
	if !ok {
		return nil, fmt.Errorf("%w: channel %s", ErrNotFound, channelID)
	}
	return expectedNextLocked(ch), nil
}

func expectedNextLocked(ch *ChannelHandle) *ExpectedTurn {
	if ch.State != ChannelActive || ch.adapter == nil {
		return nil
	}
	if te, ok := ch.adapter.(TurnExpecter); ok {
		return te.ExpectedNext(ch)
	}
	return nil
}

// PendingTurn is a turn the protocol still expects from an agent.
type PendingTurn struct {
	ChannelID            string `json:"channel_id"`
	TriggeringEnvelopeID string `json:"triggering_envelope_id,omitempty"` // "" when no envelope put the turn on the agent
}

// PendingTurnsFor lists the turns in active channels where the protocol
// expects agentID to act, ordered by channel creation: the input to
// AgentClient.ResumePendingTurns. Channels with no specific expected speaker
// or another expected speaker are skipped.
func (h *Hub) PendingTurnsFor(agentID string) []PendingTurn {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.passports[agentID]; !ok {
		return nil
	}
	var chans []*ChannelHandle
	for _, ch := range h.channels {
		if ch.State == ChannelActive && ch.hasParticipant(agentID) {
			chans = append(chans, ch)
		}
	}
	sort.Slice(chans, func(i, j int) bool {
		if !chans[i].CreatedAt.Equal(chans[j].CreatedAt) {
			return chans[i].CreatedAt.Before(chans[j].CreatedAt)
		}
		return chans[i].ID < chans[j].ID
	})
	var out []PendingTurn
	for _, ch := range chans {
		if exp := expectedNextLocked(ch); exp != nil && exp.AgentID == agentID {
			out = append(out, PendingTurn{ChannelID: ch.ID, TriggeringEnvelopeID: exp.TriggeringEnvelopeID})
		}
	}
	return out
}

// envelopeByID finds one accepted envelope of a channel by hub-stamped id.
func (h *Hub) envelopeByID(channelID, envelopeID string) (Envelope, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	log := h.logs[channelID]
	for i := len(log) - 1; i >= 0; i-- {
		if log[i].ID == envelopeID {
			return log[i], true
		}
	}
	return Envelope{}, false
}

// --- per-adapter LLM tools ---------------------------------------------------------

// ToolProvider is an optional adapter extension: the LLM tools the adapter
// offers participant right now, resolved per turn from the folded state. The
// hub holds its lock while calling it, so it must only build tools — never
// call back into the hub.
type ToolProvider interface {
	ToolsFor(self *AgentClient, ch *ChannelHandle, participantID string) ([]tools.Tool, error)
}

// ToolsFor implements ToolProvider: say, to whoever holds the floor of the
// 1Q1R cycle.
func (ConsultingAdapter) ToolsFor(self *AgentClient, ch *ChannelHandle, participantID string) ([]tools.Tool, error) {
	st, ok := ch.AdapterState.(*consultingState)
	if !ok || len(ch.Participants) < 2 {
		return nil, nil
	}
	if consultingGate(ch, st, participantID) != nil {
		return nil, nil
	}
	return oneSayTool(self, ch.ID)
}

// ToolsFor implements ToolProvider: say, always — a conversation has no turn
// order.
func (ConversationAdapter) ToolsFor(self *AgentClient, ch *ChannelHandle, _ string) ([]tools.Tool, error) {
	return oneSayTool(self, ch.ID)
}

// ToolsFor implements ToolProvider: none. Discussion replies ride the round
// envelope (Python parity: its tools_for returns []).
func (DiscussionAdapter) ToolsFor(*AgentClient, *ChannelHandle, string) ([]tools.Tool, error) {
	return nil, nil
}

// ToolsFor implements ToolProvider: none. Workflow routing is encoded by
// user-authored handoff tools, so say stays hidden from workflow
// participants.
func (WorkflowAdapter) ToolsFor(*AgentClient, *ChannelHandle, string) ([]tools.Tool, error) {
	return nil, nil
}

var (
	_ ToolProvider = ConsultingAdapter{}
	_ ToolProvider = ConversationAdapter{}
	_ ToolProvider = DiscussionAdapter{}
	_ ToolProvider = WorkflowAdapter{}
)

func oneSayTool(self *AgentClient, channelID string) ([]tools.Tool, error) {
	t, err := SayTool(self, channelID)
	if err != nil {
		return nil, err
	}
	return []tools.Tool{t}, nil
}

// SayArgs are the arguments of the say tool.
type SayArgs struct {
	Content   string   `json:"content"`
	Audience  []string `json:"audience,omitempty"`   // peer names; empty broadcasts
	ChannelID string   `json:"channel_id,omitempty"` // only when the tool is not bound to a channel
}

// SayTool builds the say tool for self: post a text envelope into a channel,
// optionally addressed to one named peer, so a model can speak mid-turn. A
// non-empty channelID binds the tool to that channel (what ToolsFor does);
// "" leaves it generic and the model must pass channel_id.
//
// AGO-DEVIATION: Python's say takes a list of peer names (envelope audience
// is a list); AGo's envelope addresses a single recipient, so audience takes
// at most one name — naming several is an error result, omitting it
// broadcasts. Python resolves the current channel through ChannelInject; AGo
// binds it by closure.
func SayTool(self *AgentClient, channelID string) (tools.Tool, error) {
	if self == nil {
		return nil, fmt.Errorf("%w: say tool requires the calling agent", ErrProtocol)
	}
	return tools.NewFunctionTool("say",
		"Post a text message into the channel. audience: at most one peer name to address "+
			"(omit to broadcast); channel_id: only when no channel is current.",
		func(args SayArgs) (any, error) {
			target := channelID
			if target == "" {
				target = args.ChannelID
			}
			if target == "" {
				return "Error: no current channel and no channel_id provided", nil
			}
			if len(args.Audience) > 1 {
				return "Error: audience takes at most one peer name; omit it to broadcast", nil
			}
			audience := ""
			if len(args.Audience) == 1 {
				p, err := self.hub.Passport(args.Audience[0])
				if err != nil {
					return fmt.Sprintf("Error: peer %q not found", args.Audience[0]), nil
				}
				audience = p.AgentID
			}
			env, err := self.hub.Send(NewTextEnvelope(target, self.AgentID(), audience, args.Content))
			if err != nil {
				return fmt.Sprintf("Error: say failed: %v", err), nil
			}
			return env.ID, nil
		},
		tools.WithSource("network:say"))
}

// ChannelTools returns the LLM tools the channel's adapter offers this agent
// right now (say for a consulting participant holding the floor, say always
// on a conversation, none on workflow and discussion). Empty for a channel
// whose adapter offers none.
func (a *AgentClient) ChannelTools(channelID string) ([]tools.Tool, error) {
	h, ok := a.hub.(*Hub)
	if !ok {
		return a.remoteChannelTools(channelID)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	ch, ok := h.channels[channelID]
	if !ok {
		return nil, fmt.Errorf("%w: channel %s", ErrNotFound, channelID)
	}
	if ch.State != ChannelActive || ch.adapter == nil {
		return nil, nil
	}
	if tp, ok := ch.adapter.(ToolProvider); ok {
		return tp.ToolsFor(a, ch, a.AgentID())
	}
	return nil, nil
}

// remoteChannelTools is ChannelTools against a remote hub, which sends the
// channel record without its adapter state: the client resolves the built-in
// adapter of the channel's manifest and folds the state from the channel log —
// the same deterministic fold the hub performs on Replay. A channel whose
// adapter the client does not know offers no tools.
func (a *AgentClient) remoteChannelTools(channelID string) ([]tools.Tool, error) {
	ch, err := a.hub.Channel(channelID)
	if err != nil {
		return nil, err
	}
	if ch.State != ChannelActive {
		return nil, nil
	}
	adapter := builtinAdapter(ch.Manifest.Type, ch.Manifest.Version)
	if adapter == nil {
		return nil, nil
	}
	ch.adapter = adapter
	initAdapterState(adapter, ch)
	wal, err := a.hub.ReadWAL(channelID)
	if err != nil {
		return nil, err
	}
	for _, env := range wal {
		switch env.EventType {
		case EnvChannelAck, EnvChannelReject, EnvChannelOpened, EnvChannelClosed, EnvChannelExpired:
			// Lifecycle state arrives with the channel record; nothing to fold.
		default:
			if _, err := adapter.OnEnvelope(ch, env); err != nil {
				return nil, fmt.Errorf("fold of channel %s failed: %w", channelID, err)
			}
		}
	}
	if tp, ok := adapter.(ToolProvider); ok {
		return tp.ToolsFor(a, ch, a.AgentID())
	}
	return nil, nil
}

// builtinAdapter returns the built-in adapter for a manifest type (version ""
// = "1"), or nil.
func builtinAdapter(manifestType, version string) ChannelAdapter {
	if version != "" && version != "1" {
		return nil
	}
	switch manifestType {
	case TypeConsulting:
		return ConsultingAdapter{}
	case TypeConversation:
		return ConversationAdapter{}
	case TypeDiscussion:
		return DiscussionAdapter{}
	case TypeWorkflow:
		return WorkflowAdapter{}
	}
	return nil
}
