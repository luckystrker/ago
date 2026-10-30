package network

import "time"

// ChannelState is the lifecycle state of a channel. The state machine is
// hub-enforced: pending → active → closing → closed, with expired reachable
// from any non-terminal state (TTL).
type ChannelState string

const (
	ChannelPending ChannelState = "pending" // invites sent, waiting on acks
	ChannelActive  ChannelState = "active"
	ChannelClosing ChannelState = "closing"
	ChannelClosed  ChannelState = "closed"
	ChannelExpired ChannelState = "expired"
)

// Terminal reports whether the channel can no longer accept envelopes.
func (s ChannelState) Terminal() bool {
	return s == ChannelClosed || s == ChannelExpired
}

// ParticipantRole is the role of a participant within a channel. Consulting
// uses initiator + respondent (2-party); conversation, discussion and
// workflow use initiator + participant (multi-party).
type ParticipantRole string

const (
	RoleInitiator   ParticipantRole = "initiator"
	RoleRespondent  ParticipantRole = "respondent"  // 2-party non-initiator
	RoleParticipant ParticipantRole = "participant" // multi-party non-initiator
)

// Participant is one channel member. Roles are positional: participants[0]
// is the creator (initiator); in a 2-party channel participants[1] is the
// respondent.
type Participant struct {
	AgentID  string          `json:"agent_id"`
	Role     ParticipantRole `json:"role"`
	Order    int             `json:"order"`
	JoinedAt time.Time       `json:"joined_at,omitempty"`
}

// ChannelManifest describes what a channel is: the adapter dispatch key, the
// participants, adapter knobs and the declared protocol-shape expectations
// (evaluated by the expectation sweeper, see expectations.go).
//
// AGO-DEVIATION vs Python: Version is a string (brief pins it; Python uses
// an int) and there is no participant-schema declaration. Expectations
// carry the Python name/on_violation/params shape (applies_to carried,
// ignored by the built-in evaluators — Python parity).
type ChannelManifest struct {
	Type         string         `json:"type"`
	Version      string         `json:"version,omitempty"`
	Participants []string       `json:"participants,omitempty"`
	Expectations []Expectation  `json:"expectations,omitempty"`
	Knobs        map[string]any `json:"knobs,omitempty"`
}

// Expectation is a protocol-shape contract (Python: acks_within /
// reply_within / max_silence). Kind selects a built-in evaluator;
// OnViolation selects a built-in violation handler ("audit" |
// "notify_channel" | "auto_close" — unknown values are silently ignored,
// Python parity). Evaluated by the hub's expectation sweeper
// (Hub.SweepExpectations / ExpectationSweeper), not by the send path.
type Expectation struct {
	Kind        string         `json:"kind"`
	OnViolation string         `json:"on_violation,omitempty"`
	Params      map[string]any `json:"params,omitempty"`
	AppliesTo   []string       `json:"applies_to,omitempty"` // role names or agent ids; empty = all
}

// isProtocolEvent reports whether an event type is channel-machine
// bookkeeping. Protocol envelopes bypass the active-state send gate and are
// absorbed by the hub instead of dispatched (except lifecycle broadcasts).
func isProtocolEvent(eventType string) bool {
	switch eventType {
	case EnvChannelInvite, EnvChannelAck, EnvChannelReject,
		EnvChannelOpened, EnvChannelClosed, EnvChannelExpired:
		return true
	}
	return false
}

// isTaskEvent reports whether the envelope mirrors task lifecycle
// (ag2.task.*). Task envelopes ride on channels as informational data and
// bypass protocol-shape validation in the adapters.
func isTaskEvent(eventType string) bool {
	return len(eventType) >= len("ag2.task.") && eventType[:len("ag2.task.")] == "ag2.task."
}
