package network

import (
	"fmt"
)

// Adapter type keys (manifest dispatch).
const (
	TypeConsulting   = "consulting"
	TypeConversation = "conversation"
	TypeDiscussion   = "discussion"
	TypeWorkflow     = "workflow"
)

// protocolEnvelope reports whether env is channel-machine bookkeeping or
// task-lifecycle traffic — accepted by every adapter without protocol-shape
// validation.
func protocolEnvelope(env Envelope) bool {
	return isProtocolEvent(env.EventType) || isTaskEvent(env.EventType)
}

// --- ConsultingAdapter -------------------------------------------------------

// ConsultingAdapter is the strict 1Q1R channel: the initiator sends exactly
// one text envelope, the respondent answers once, and the channel
// auto-closes on the reply. Exactly 2 participants.
type ConsultingAdapter struct{}

// Type implements ChannelAdapter.
func (ConsultingAdapter) Type() string { return TypeConsulting }

// consultingState is the folded 1Q1R progress.
type consultingState struct {
	InitiatorSent     bool
	RespondentReplied bool
	LastEnvelopeID    string
}

// InitialState implements ChannelAdapter.
func (ConsultingAdapter) InitialState() any { return &consultingState{} }

// ValidateCreate implements ChannelAdapter.
func (ConsultingAdapter) ValidateCreate(manifest ChannelManifest) error {
	if len(manifest.Participants) != 2 {
		return fmt.Errorf("%w: consulting requires exactly 2 participants, got %d",
			ErrProtocol, len(manifest.Participants))
	}
	return nil
}

// OnEnvelope implements ChannelAdapter.
func (ConsultingAdapter) OnEnvelope(ch *ChannelHandle, env Envelope) (Routing, error) {
	if protocolEnvelope(env) || env.EventType != EnvText {
		// Task envelopes and unknown event types ride as informational data.
		return Routing{}, nil
	}
	st, ok := ch.AdapterState.(*consultingState)
	if !ok {
		return Routing{}, fmt.Errorf("%w: consulting channel %s has no folded state", ErrProtocol, ch.ID)
	}
	initiator := ch.Participants[0].AgentID
	respondent := ch.Participants[1].AgentID
	switch {
	case st.InitiatorSent && st.RespondentReplied:
		return Routing{}, fmt.Errorf("%w: consulting channel %s already complete", ErrProtocol, ch.ID)
	case !st.InitiatorSent:
		if env.SenderID != initiator {
			return Routing{}, fmt.Errorf("%w: consulting channel %s expects the first send from %s, got %s",
				ErrProtocol, ch.ID, initiator, env.SenderID)
		}
		st.InitiatorSent = true
	default:
		if env.SenderID != respondent {
			return Routing{}, fmt.Errorf("%w: consulting channel %s expects the reply from %s, got %s",
				ErrProtocol, ch.ID, respondent, env.SenderID)
		}
		st.RespondentReplied = true
	}
	st.LastEnvelopeID = env.ID
	if st.InitiatorSent && st.RespondentReplied {
		return Routing{Close: true, CloseReason: "consulting_complete"}, nil
	}
	return Routing{}, nil
}

// --- ConversationAdapter -----------------------------------------------------

// ConversationAdapter is the bidirectional multi-turn 1+1 channel: either
// side may send at any turn; the channel ends only via an explicit close or
// TTL.
type ConversationAdapter struct{}

// Type implements ChannelAdapter.
func (ConversationAdapter) Type() string { return TypeConversation }

// conversationState is folded bookkeeping (no turn ordering).
type conversationState struct {
	TurnCount      int
	LastSpeakerID  string
	LastEnvelopeID string
}

// InitialState implements ChannelAdapter.
func (ConversationAdapter) InitialState() any { return &conversationState{} }

// ValidateCreate implements ChannelAdapter.
func (ConversationAdapter) ValidateCreate(manifest ChannelManifest) error {
	if len(manifest.Participants) != 2 {
		return fmt.Errorf("%w: conversation requires exactly 2 participants, got %d",
			ErrProtocol, len(manifest.Participants))
	}
	return nil
}

// OnEnvelope implements ChannelAdapter.
func (ConversationAdapter) OnEnvelope(ch *ChannelHandle, env Envelope) (Routing, error) {
	if protocolEnvelope(env) || env.EventType != EnvText {
		return Routing{}, nil
	}
	if !ch.hasParticipant(env.SenderID) {
		return Routing{}, fmt.Errorf("%w: conversation channel %s only accepts sends from participants, got %s",
			ErrProtocol, ch.ID, env.SenderID)
	}
	st, ok := ch.AdapterState.(*conversationState)
	if !ok {
		return Routing{}, fmt.Errorf("%w: conversation channel %s has no folded state", ErrProtocol, ch.ID)
	}
	st.TurnCount++
	st.LastSpeakerID = env.SenderID
	st.LastEnvelopeID = env.ID
	return Routing{}, nil
}

// --- DiscussionAdapter -------------------------------------------------------

// DiscussionAdapter is the multi-party turn-taking channel: participants
// speak in strict round-robin rotation starting from the creator.
type DiscussionAdapter struct{}

// OrderingRoundRobin is the only supported ordering knob value.
const OrderingRoundRobin = "round_robin"

// Type implements ChannelAdapter.
func (DiscussionAdapter) Type() string { return TypeDiscussion }

// discussionState is the folded rotation.
type discussionState struct {
	Order          []string
	Expected       string
	LastSpeakerID  string
	LastEnvelopeID string
	TurnCount      int
}

// InitialState implements ChannelAdapter.
func (DiscussionAdapter) InitialState() any {
	return &discussionState{}
}

// InitialStateFor implements StateInitializer: the rotation seeds from the
// participant order; the creator speaks first.
func (DiscussionAdapter) InitialStateFor(ch *ChannelHandle) any {
	order := make([]string, len(ch.Participants))
	for i, p := range ch.Participants {
		order[i] = p.AgentID
	}
	return &discussionState{Order: order, Expected: ch.CreatorID}
}

// ValidateCreate implements ChannelAdapter.
func (DiscussionAdapter) ValidateCreate(manifest ChannelManifest) error {
	if len(manifest.Participants) < 2 {
		return fmt.Errorf("%w: discussion requires at least 2 participants, got %d",
			ErrProtocol, len(manifest.Participants))
	}
	ordering := OrderingRoundRobin
	if v, ok := manifest.Knobs["ordering"].(string); ok {
		ordering = v
	}
	if ordering != OrderingRoundRobin {
		return fmt.Errorf("%w: discussion knobs.ordering=%q not supported; choose %q",
			ErrProtocol, ordering, OrderingRoundRobin)
	}
	return nil
}

// OnEnvelope implements ChannelAdapter.
func (DiscussionAdapter) OnEnvelope(ch *ChannelHandle, env Envelope) (Routing, error) {
	if protocolEnvelope(env) || env.EventType != EnvText {
		return Routing{}, nil
	}
	st, ok := ch.AdapterState.(*discussionState)
	if !ok {
		return Routing{}, fmt.Errorf("%w: discussion channel %s has no folded state", ErrProtocol, ch.ID)
	}
	if env.SenderID != st.Expected {
		return Routing{}, fmt.Errorf("%w: discussion channel %s expects %s to speak, got %s",
			ErrProtocol, ch.ID, st.Expected, env.SenderID)
	}
	idx := 0
	for i, id := range st.Order {
		if id == env.SenderID {
			idx = i
			break
		}
	}
	st.Expected = st.Order[(idx+1)%len(st.Order)]
	st.LastSpeakerID = env.SenderID
	st.LastEnvelopeID = env.ID
	st.TurnCount++
	return Routing{NextSender: st.Expected}, nil
}

// --- WorkflowAdapter ---------------------------------------------------------

// WorkflowAdapter is the orchestrated multi-party channel driven by a
// declarative TransitionGraph: substantive rounds are EnvPacket envelopes,
// the sender must be the expected speaker, and routing resolves through the
// graph (finish/handoff intents short-circuit).
//
// Knobs: "graph" (TransitionGraph.ToDict output, required),
// "context_vars" (optional initial channel context).
type WorkflowAdapter struct{}

// Type implements ChannelAdapter.
func (WorkflowAdapter) Type() string { return TypeWorkflow }

// workflowState is the folded orchestration state.
type workflowState struct {
	Order          []string
	Expected       string
	LastSpeakerID  string
	LastEnvelopeID string
	TurnCount      int
	CloseReason    string
	CreatorID      string
	Graph          TransitionGraph
	ContextVars    map[string]any
}

// InitialState implements ChannelAdapter.
func (WorkflowAdapter) InitialState() any { return &workflowState{} }

// InitialStateFor implements StateInitializer: the graph and initial context
// come from the manifest knobs (validated at create time).
func (WorkflowAdapter) InitialStateFor(ch *ChannelHandle) any {
	st := &workflowState{
		CreatorID:   ch.CreatorID,
		ContextVars: map[string]any{},
	}
	st.Order = ch.ParticipantIDs()
	if graph, err := workflowGraph(ch.Manifest); err == nil {
		st.Graph = graph
		st.Expected = graph.Initial
	}
	if raw, ok := ch.Manifest.Knobs["context_vars"].(map[string]any); ok {
		for k, v := range raw {
			st.ContextVars[k] = v
		}
	}
	return st
}

// ValidateCreate implements ChannelAdapter.
func (WorkflowAdapter) ValidateCreate(manifest ChannelManifest) error {
	if len(manifest.Participants) < 2 {
		return fmt.Errorf("%w: workflow requires at least 2 participants, got %d",
			ErrProtocol, len(manifest.Participants))
	}
	graph, err := workflowGraph(manifest)
	if err != nil {
		return err
	}
	for _, id := range manifest.Participants {
		if id == graph.Initial {
			return nil
		}
	}
	return fmt.Errorf("%w: workflow initial_speaker %q not in participants %v",
		ErrProtocol, graph.Initial, manifest.Participants)
}

func workflowGraph(manifest ChannelManifest) (TransitionGraph, error) {
	raw, ok := manifest.Knobs["graph"]
	if !ok {
		return TransitionGraph{}, fmt.Errorf("%w: workflow requires knobs['graph']", ErrProtocol)
	}
	d, ok := raw.(map[string]any)
	if !ok {
		return TransitionGraph{}, fmt.Errorf("%w: workflow knobs['graph'] must be a dict (TransitionGraph.ToDict)",
			ErrProtocol)
	}
	graph, err := GraphFromDict(d)
	if err != nil {
		return TransitionGraph{}, fmt.Errorf("%w: invalid workflow graph: %v", ErrProtocol, err)
	}
	return graph, nil
}

// OnEnvelope implements ChannelAdapter.
func (WorkflowAdapter) OnEnvelope(ch *ChannelHandle, env Envelope) (Routing, error) {
	st, ok := ch.AdapterState.(*workflowState)
	if !ok {
		return Routing{}, fmt.Errorf("%w: workflow channel %s has no folded state", ErrProtocol, ch.ID)
	}

	// Standalone context mutations are auxiliary: they never advance turn
	// bookkeeping.
	if env.EventType == EnvContextSet {
		if !ch.hasParticipant(env.SenderID) {
			return Routing{}, fmt.Errorf("%w: workflow %s only accepts context updates from participants, got %s",
				ErrProtocol, ch.ID, env.SenderID)
		}
		applyContextSet(st.ContextVars, env)
		return Routing{}, nil
	}

	substantive := env.EventType == EnvText || env.EventType == EnvPacket
	if protocolEnvelope(env) || !substantive {
		return Routing{}, nil
	}

	// Turn gate: the expected speaker owns the next substantive envelope.
	// After a termination (expected == "" with turns folded) nothing more
	// travels — the close envelope is in flight.
	if st.Expected != "" && env.SenderID != st.Expected {
		return Routing{}, fmt.Errorf("%w: workflow %s expects %s to speak, got %s",
			ErrProtocol, ch.ID, st.Expected, env.SenderID)
	}
	if st.Expected == "" && st.TurnCount > 0 {
		return Routing{}, fmt.Errorf("%w: workflow %s has terminated", ErrProtocol, ch.ID)
	}

	// A pre-resolved handoff target must be a channel member: the hub
	// rejects sends from non-participants, so Expected would point at an
	// agent that can never speak, bricking the channel until its TTL. The
	// check runs before the fold so a rejected envelope leaves the adapter
	// state untouched. AGO-DEVIATION: Python trusts the tool's pick.
	p := decodePacket(env)
	if p.Routing["kind"] == "handoff" {
		if target, _ := p.Routing["target"].(string); target != "" && !ch.hasParticipant(target) {
			return Routing{}, fmt.Errorf("%w: workflow %s handoff target %q is not a participant",
				ErrProtocol, ch.ID, target)
		}
	}

	// Fold: bookkeeping advances first, then routing resolves against the
	// post-fold state (transitions see the incremented turn count).
	st.TurnCount++
	st.LastSpeakerID = env.SenderID
	st.LastEnvelopeID = env.ID
	st.CloseReason = ""

	// Apply packet context mutations BEFORE selection so a same-packet set
	// is visible to conditions (atomic update + speaker advance).
	if env.EventType == EnvPacket {
		for _, k := range p.ContextUpdates.Delete {
			delete(st.ContextVars, k)
		}
		for k, v := range p.ContextUpdates.Set {
			st.ContextVars[k] = v
		}
	}

	// Routing resolution: an explicit finish short-circuits to termination,
	// a pre-resolved handoff trusts the tool's pick, otherwise the static
	// graph decides.
	switch {
	case p.Routing["kind"] == "finish":
		reason := "finished"
		if r, ok := p.Routing["reason"].(string); ok && r != "" {
			reason = r
		}
		st.Expected = ""
		st.CloseReason = reason
		return Routing{Close: true, CloseReason: reason}, nil
	case p.Routing["kind"] == "handoff":
		if target, _ := p.Routing["target"].(string); target != "" {
			st.Expected = target
			return Routing{NextSender: target}, nil
		}
	}

	ctx := &TurnContext{
		Envelope:         env,
		ParticipantOrder: st.Order,
		LastSpeakerID:    env.SenderID,
		CreatorID:        st.CreatorID,
		TurnCount:        st.TurnCount,
		ContextVars:      st.ContextVars,
	}
	decision := st.Graph.Select(ctx)
	st.Expected = decision.NextSender
	st.CloseReason = decision.CloseReason
	if decision.Close {
		return Routing{Close: true, CloseReason: nonEmpty(decision.CloseReason, "workflow_terminated")}, nil
	}

	// Max-turns cap: reached after a normal advance (graph termination
	// above takes precedence).
	if st.Graph.MaxTurns > 0 && st.TurnCount >= st.Graph.MaxTurns {
		st.Expected = ""
		return Routing{Close: true, CloseReason: "max_turns"}, nil
	}
	return Routing{NextSender: st.Expected}, nil
}

// applyContextSet folds an EnvContextSet payload into vars.
func applyContextSet(vars map[string]any, env Envelope) {
	var p struct {
		Set    map[string]any `json:"set"`
		Delete []string       `json:"delete"`
	}
	if env.Decode(&p) != nil {
		return
	}
	for _, k := range p.Delete {
		delete(vars, k)
	}
	for k, v := range p.Set {
		vars[k] = v
	}
}
