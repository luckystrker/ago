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

// ParticipantSchema declares the participant count and role names a channel
// type accepts. Python's field is ChannelManifest.participants; AGo's
// Participants is the concrete id list, so the schema travels as
// participant_schema.
type ParticipantSchema struct {
	Min   int      `json:"min"`
	Max   int      `json:"max,omitempty"` // inclusive; 0 = unbounded
	Roles []string `json:"roles,omitempty"`
}

// Allows reports whether n participants satisfy the schema.
func (s ParticipantSchema) Allows(n int) bool {
	return n >= s.Min && (s.Max == 0 || n <= s.Max)
}

// Default view policy names (Python view classes' `name`).
const (
	ViewFullTranscript       = "full_transcript"
	ViewWindowedSummary      = "windowed_summary"
	ViewNamedTranscript      = "named_transcript"
	ViewNamedWindowedSummary = "named_windowed_summary"
	LabelIntent              = "intent" // conventional Labels key for the creator's stated purpose
)

// ChannelManifest describes what a channel is: the adapter dispatch key, the
// participants, adapter knobs and the declared protocol-shape expectations
// (evaluated by the expectation sweeper, see expectations.go).
//
// DefaultViewPolicy, KnobsSchema, ParticipantSchema and Expectations left
// empty at CreateChannel are filled from the adapter's DefaultManifest (see
// ManifestProvider); an explicit empty, non-nil Expectations slice opts out
// of the adapter's default expectations.
//
// AGO-DEVIATION vs Python: Version is a string (brief pins it; Python uses
// an int); the participant schema travels as participant_schema because
// Participants is the concrete id list; Labels live on the manifest (like
// Knobs) instead of on a separate metadata record; the manifest is
// caller-supplied and merged with adapter defaults rather than owned by the
// adapter. Expectations carry the Python name/on_violation/params shape
// (applies_to carried, ignored by the built-in evaluators — Python parity).
type ChannelManifest struct {
	Type              string             `json:"type"`
	Version           string             `json:"version,omitempty"`
	Participants      []string           `json:"participants,omitempty"`
	ParticipantSchema *ParticipantSchema `json:"participant_schema,omitempty"`
	Expectations      []Expectation      `json:"expectations,omitempty"`
	Knobs             map[string]any     `json:"knobs,omitempty"`
	KnobsSchema       map[string]string  `json:"knobs_schema,omitempty"` // knob name → type hint
	DefaultViewPolicy string             `json:"default_view_policy,omitempty"`
	// Labels are free-form string annotations set at creation (Python
	// ChannelMetadata.labels); LabelIntent is the conventional key.
	Labels map[string]string `json:"labels,omitempty"`
}

// applyDefaults fills the empty descriptive fields from def (the adapter's
// default manifest). Caller-set fields win; a non-nil Expectations slice,
// even if empty, is kept as given.
func (m ChannelManifest) applyDefaults(def ChannelManifest) ChannelManifest {
	if m.ParticipantSchema == nil && def.ParticipantSchema != nil {
		schema := *def.ParticipantSchema
		schema.Roles = append([]string(nil), schema.Roles...)
		m.ParticipantSchema = &schema
	}
	if m.DefaultViewPolicy == "" {
		m.DefaultViewPolicy = def.DefaultViewPolicy
	}
	if m.KnobsSchema == nil && len(def.KnobsSchema) > 0 {
		m.KnobsSchema = make(map[string]string, len(def.KnobsSchema))
		for k, v := range def.KnobsSchema {
			m.KnobsSchema[k] = v
		}
	}
	if m.Expectations == nil && len(def.Expectations) > 0 {
		m.Expectations = make([]Expectation, len(def.Expectations))
		for i, e := range def.Expectations {
			e.Params = cloneKnobs(e.Params)
			e.AppliesTo = append([]string(nil), e.AppliesTo...)
			m.Expectations[i] = e
		}
	}
	return m
}

// cloneExtras deep-copies the maps and slices the manifest owns beyond the
// ones Hub.Channel already clones.
func (m *ChannelManifest) cloneExtras() {
	if m.ParticipantSchema != nil {
		schema := *m.ParticipantSchema
		schema.Roles = append([]string(nil), schema.Roles...)
		m.ParticipantSchema = &schema
	}
	if m.KnobsSchema != nil {
		cp := make(map[string]string, len(m.KnobsSchema))
		for k, v := range m.KnobsSchema {
			cp[k] = v
		}
		m.KnobsSchema = cp
	}
	if m.Labels != nil {
		cp := make(map[string]string, len(m.Labels))
		for k, v := range m.Labels {
			cp[k] = v
		}
		m.Labels = cp
	}
}

// ManifestProvider is an optional adapter extension: the adapter's default
// manifest (participant schema, knob types, default view, default
// expectations). Hub.CreateChannel merges it into every new channel's
// manifest.
type ManifestProvider interface {
	DefaultManifest() ChannelManifest
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
