package network

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// --- wire vocabulary -------------------------------------------------------------------
//
// Frames travel as transport.Frame; the network layer fills them like this
// (Python parity: network/transport/frames.py, with AGo's Go data model for
// the records inside):
//
//	hello     client → hub  RequestID, Payload = HelloBody
//	welcome   hub → client  RequestID (echo), Payload = WelcomeBody
//	error     hub → client  RequestID (echo of a hello), Code, Message
//	request   client → hub  RequestID, Op, Payload = params (a JSON object)
//	response  hub → client  RequestID (echo); Code == "" ⇒ ok and Payload = result,
//	                         else Code/Message describe the failure
//	notify    hub → client  RecipientID, ChannelID, EnvelopeID, Payload = Envelope
//	receipt   client → hub  RecipientID, ChannelID, EnvelopeID, Code = "ack"|"nack",
//	                         Message = reason
//
// Frame.From and Frame.To are not part of the protocol: a link is one
// connection (transport.NewPipe, one WebSocket), the hub derives who is
// talking from the connection alone and never from what a frame says about
// itself.

// Control-plane operations the hub serves over the wire (every op of Python's
// Hub._dispatch_request_op).
const (
	OpRegister    = "register"
	OpGetAgent    = "get_agent"
	OpGetResume   = "get_resume"
	OpGetSkill    = "get_skill"
	OpGetRule     = "get_rule"
	OpFindAgentID = "find_agent_id"
	OpNamesFor    = "names_for"
	OpListAgents  = "list_agents"
	OpSetResume   = "set_resume"
	OpSetSkill    = "set_skill"
	OpSetRule     = "set_rule"
	// OpAddResumeExample is an AGo addition: Python has no atomic example op
	// (a client reads, extends and sets the whole resume, which can overwrite a
	// concurrent update).
	OpAddResumeExample        = "add_resume_example"
	OpUnregister              = "unregister"
	OpCreateChannel           = "create_channel"
	OpGetChannel              = "get_channel"
	OpListChannels            = "list_channels"
	OpCloseChannel            = "close_channel"
	OpPostEnvelope            = "post_envelope"
	OpReadWAL                 = "read_wal"
	OpFindEnvelopeByCausation = "find_envelope_by_causation"
	OpCanSend                 = "can_send"
	OpPendingTurnsFor         = "pending_turns_for"
	OpReportTurnFailure       = "report_turn_failure"
	OpGetTask                 = "get_task"
	OpListTasks               = "list_tasks"
	OpObserveTask             = "observe_task"
	OpUpdateTask              = "update_task"
	OpRecordObservation       = "record_observation"
	OpFireTaskEvent           = "fire_task_event"
	OpCheckpointTask          = "checkpoint_task"
	OpReadTaskCheckpoint      = "read_task_checkpoint"
)

// WireOps lists every control-plane op the hub serves, in the order of
// Python's _dispatch_request_op (plus AGo's add_resume_example).
var WireOps = []string{
	OpRegister, OpGetAgent, OpGetResume, OpGetSkill, OpGetRule, OpFindAgentID, OpNamesFor, OpListAgents,
	OpSetResume, OpSetSkill, OpSetRule, OpAddResumeExample, OpUnregister,
	OpCreateChannel, OpGetChannel, OpListChannels, OpCloseChannel, OpPostEnvelope, OpReadWAL,
	OpFindEnvelopeByCausation, OpCanSend, OpPendingTurnsFor, OpReportTurnFailure,
	OpGetTask, OpListTasks, OpObserveTask, OpUpdateTask, OpRecordObservation, OpFireTaskEvent,
	OpCheckpointTask, OpReadTaskCheckpoint,
}

// Receipt statuses (Frame.Code of a receipt).
const (
	ReceiptAck  = "ack"
	ReceiptNack = "nack"
)

// WelcomeBody is the payload of a welcome frame.
type WelcomeBody struct {
	EndpointID string `json:"endpoint_id"`
	HubTime    string `json:"hub_time"`
}

// HelloBody is the payload of a hello frame: it binds the connection to the
// identity registered under Name (a reconnect) after validating the auth
// claim.
//
// AGO-DEVIATION: Python's hello carries ONE since_envelope_id compared
// against every channel's cursor, which lets a high value in one channel hide
// an undelivered envelope in another (wave-4 model D8). AGo's hello carries
// Since, a per-channel high-water map; the hub still accepts Python's single
// since_envelope_id (the legacy form), which then applies to every channel
// that Since does not name. The replay itself is not part of this type.
type HelloBody struct {
	Name       string         `json:"name"`
	AuthScheme string         `json:"auth_scheme,omitempty"`
	AuthClaim  map[string]any `json:"auth_claim,omitempty"`
	// SinceEnvelopeID is the legacy single high-water mark: nil = skip replay,
	// "" = replay everything, an id = replay strictly past it.
	SinceEnvelopeID *string `json:"since_envelope_id,omitempty"`
	// Since maps channel id → the client's high-water envelope id there.
	Since map[string]string `json:"since,omitempty"`
}

// HelloSince is the parsed replay position of a hello.
type HelloSince struct {
	legacy     *string
	perChannel map[string]string
}

// Lookup returns the high-water mark for channelID: the per-channel entry if
// present, else the legacy single value. ok is false when the hello asks for
// no replay of that channel.
func (s HelloSince) Lookup(channelID string) (id string, ok bool) {
	if v, has := s.perChannel[channelID]; has {
		return v, true
	}
	if s.legacy != nil {
		return *s.legacy, true
	}
	return "", false
}

// SinceMarks returns the parsed replay position of the hello.
func (b HelloBody) SinceMarks() HelloSince {
	return HelloSince{legacy: b.SinceEnvelopeID, perChannel: b.Since}
}

// parseHello decodes and type-checks a hello payload: every field has to be
// of its JSON type (a malformed frame must answer a protocol error, not drop
// the connection).
func parseHello(payload []byte) (HelloBody, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return HelloBody{}, fmt.Errorf("%w: malformed hello frame: %v", ErrProtocol, err)
	}
	var b HelloBody
	str := func(field string, dst *string, optional bool) error {
		v, ok := raw[field]
		if !ok || string(v) == "null" {
			if optional {
				return nil
			}
			return fmt.Errorf("%w: malformed hello frame: %s", ErrProtocol, field)
		}
		if err := json.Unmarshal(v, dst); err != nil {
			return fmt.Errorf("%w: malformed hello frame: %s", ErrProtocol, field)
		}
		return nil
	}
	if err := str("name", &b.Name, false); err != nil {
		return HelloBody{}, err
	}
	if err := str("auth_scheme", &b.AuthScheme, true); err != nil {
		return HelloBody{}, err
	}
	if v, ok := raw["auth_claim"]; ok && string(v) != "null" {
		if err := json.Unmarshal(v, &b.AuthClaim); err != nil {
			return HelloBody{}, fmt.Errorf("%w: auth_claim must be a JSON object", ErrAuth)
		}
	}
	if v, ok := raw["since_envelope_id"]; ok && string(v) != "null" {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return HelloBody{}, fmt.Errorf("%w: malformed hello frame: since_envelope_id", ErrProtocol)
		}
		b.SinceEnvelopeID = &s
	}
	if v, ok := raw["since"]; ok && string(v) != "null" {
		if err := json.Unmarshal(v, &b.Since); err != nil {
			return HelloBody{}, fmt.Errorf("%w: malformed hello frame: since", ErrProtocol)
		}
	}
	return b, nil
}

// --- error codes ---------------------------------------------------------------------------

// Stable wire error codes. The first four are Python's (_ERROR_CODE_MAP);
// the others carry AGo's remaining sentinels so a remote caller can still
// errors.Is against them.
const (
	CodeNotFound         = "not_found"
	CodeAccessDenied     = "access_denied"
	CodeProtocol         = "protocol_error"
	CodeAuthFailed       = "auth_failed"
	CodeTaskNotFound     = "task_not_found"
	CodeDepthExceeded    = "depth_exceeded"
	CodeTTLExpired       = "ttl_expired"
	CodeChannelClosed    = "channel_closed"
	CodeChannelExpired   = "channel_expired"
	CodeAckTimeout       = "ack_timeout"
	CodeNameTaken        = "name_taken"
	CodeBadIdentity      = "bad_identity"
	CodeInboxFull        = "inbox_full"
	CodeRateLimited      = "rate_limited"
	CodeCheckpointDenied = "checkpoint_denied"
	CodeError            = "error"
)

// wireCodes lists the sentinel ↔ code pairs; the more specific sentinel comes
// first (ErrCheckpointDenied wraps ErrAccessDenied).
var wireCodes = []struct {
	err  error
	code string
}{
	{ErrCheckpointDenied, CodeCheckpointDenied},
	{ErrTaskNotFound, CodeTaskNotFound},
	{ErrNotFound, CodeNotFound},
	{ErrAccessDenied, CodeAccessDenied},
	{ErrProtocol, CodeProtocol},
	{ErrAuth, CodeAuthFailed},
	{ErrDepthExceeded, CodeDepthExceeded},
	{ErrTTLExpired, CodeTTLExpired},
	{ErrChannelClosed, CodeChannelClosed},
	{ErrChannelExpired, CodeChannelExpired},
	{ErrAckTimeout, CodeAckTimeout},
	{ErrNameTaken, CodeNameTaken},
	{ErrBadIdentity, CodeBadIdentity},
	{ErrInboxFull, CodeInboxFull},
	{ErrRateLimited, CodeRateLimited},
}

// wireErrorCode maps an error to its stable wire code ("error" when it
// matches no sentinel).
func wireErrorCode(err error) string {
	for _, c := range wireCodes {
		if errors.Is(err, c.err) {
			return c.code
		}
	}
	return CodeError
}

// errorFromWire rebuilds the sentinel-wrapping error a response or error
// frame describes (inverse of wireErrorCode). The message already carries the
// sentinel's own text when the hub's error was %w-wrapped, so it is not
// prefixed twice.
func errorFromWire(code, message string) error {
	for _, c := range wireCodes {
		if c.code == code {
			if strings.Contains(message, c.err.Error()) {
				return &wireError{sentinel: c.err, text: message}
			}
			return fmt.Errorf("%w: %s", c.err, message)
		}
	}
	return fmt.Errorf("network: hub error: %s", message)
}

// wireError is a remote error whose text is the hub's verbatim message and
// that still matches its sentinel under errors.Is.
type wireError struct {
	sentinel error
	text     string
}

func (e *wireError) Error() string { return e.text }
func (e *wireError) Unwrap() error { return e.sentinel }

// --- control-plane parameter shapes ---------------------------------------------------------
//
// AGO-DEVIATION: the op names are Python's, the parameters follow AGo's data
// model (Envelope, Passport, Rule, Resume, ChannelManifest and the channel
// snapshot are the JSON the rest of the package already speaks), not
// Python's dict shapes.

type registerParams struct {
	Passport Passport `json:"passport"`
	Resume   *Resume  `json:"resume,omitempty"`
	Rule     *Rule    `json:"rule,omitempty"`
}

type nameOrIDParams struct {
	NameOrID string `json:"name_or_id"`
}
type agentIDParams struct {
	AgentID string `json:"agent_id"`
}
type nameParams struct {
	Name string `json:"name"`
}
type agentIDsParams struct {
	AgentIDs []string `json:"agent_ids"`
}
type listAgentsParams struct {
	Capability string `json:"capability,omitempty"`
	Query      string `json:"query,omitempty"`
	Kind       string `json:"kind,omitempty"`
	SortBy     string `json:"sort_by,omitempty"`
	Limit      *int   `json:"limit,omitempty"`
}
type setResumeParams struct {
	AgentID string `json:"agent_id"`
	Resume  Resume `json:"resume"`
}
type setSkillParams struct {
	AgentID string  `json:"agent_id"`
	SkillMD *string `json:"skill_md"`
}
type addResumeExampleParams struct {
	AgentID string        `json:"agent_id"`
	Example ResumeExample `json:"example"`
}
type setRuleParams struct {
	AgentID string `json:"agent_id"`
	Rule    Rule   `json:"rule"`
}
type createChannelParams struct {
	Manifest     ChannelManifest `json:"manifest"`
	Participants []string        `json:"participants"` // [0] is the creator
}
type channelIDParams struct {
	ChannelID string `json:"channel_id"`
}
type listChannelsParams struct {
	AgentID string `json:"agent_id,omitempty"`
	Limit   *int   `json:"limit,omitempty"`
}
type closeChannelParams struct {
	ChannelID string `json:"channel_id"`
	Reason    string `json:"reason,omitempty"`
}
type postEnvelopeParams struct {
	Envelope Envelope `json:"envelope"`
}
type readWALParams struct {
	ChannelID string `json:"channel_id"`
	Since     int    `json:"since,omitempty"`
	Until     *int   `json:"until,omitempty"`
}
type causationParams struct {
	ChannelID   string `json:"channel_id"`
	SenderID    string `json:"sender_id"`
	CausationID string `json:"causation_id"`
}
type canSendParams struct {
	ChannelID string `json:"channel_id"`
	SenderID  string `json:"sender_id"`
	EventType string `json:"event_type,omitempty"`
}
type turnFailureParams struct {
	ChannelID  string `json:"channel_id"`
	AgentID    string `json:"agent_id"`
	EnvelopeID string `json:"envelope_id"`
	Error      string `json:"error,omitempty"`
}
type taskIDParams struct {
	TaskID string `json:"task_id"`
}
type listTasksParams struct {
	AgentID   string `json:"agent_id,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
	State     string `json:"state,omitempty"`
	Limit     *int   `json:"limit,omitempty"`
}
type updateTaskParams struct {
	TaskID   string         `json:"task_id"`
	State    string         `json:"state,omitempty"`
	Progress map[string]any `json:"progress,omitempty"`
	Result   any            `json:"result,omitempty"`
	Error    string         `json:"error,omitempty"`
}
type recordObservationParams struct {
	OwnerID    string `json:"owner_id"`
	Capability string `json:"capability"`
	Outcome    string `json:"outcome"`
	LatencyMS  *int   `json:"latency_ms,omitempty"`
	TaskID     string `json:"task_id,omitempty"`
}
type fireTaskEventParams struct {
	TaskID  string         `json:"task_id"`
	Kind    string         `json:"kind"`
	Payload map[string]any `json:"payload,omitempty"`
}
type checkpointParams struct {
	TaskID   string `json:"task_id"`
	State    []byte `json:"state,omitempty"`
	WriterID string `json:"writer_id,omitempty"` // needed when the connection holds several agents
}
