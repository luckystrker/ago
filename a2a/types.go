// Package a2a implements a functional subset of the A2A (Agent-to-Agent)
// protocol: an HTTP server that exposes a Go agent over JSON-RPC 2.0 with
// agent-card discovery, a REST twin of the core methods, and a matching
// client. It is the Go counterpart of ag2/ag2/a2a on the a2a-sdk wire
// shapes, stdlib only.
//
// Scope: protocol types, agent-card discovery, message/send,
// message/stream (server SSE), tasks/get, tasks/list, tasks/cancel, an
// in-memory task store (D-049); push-notification configs with server dispatch
// and a webhook receiver, the urn:ag2:client-tools:v1 extension with client-side
// execution drivers, context-variable sync and HITL-over-the-wire on
// Message.metadata, the REST transport routes, and the declarative security
// schemes of security.py (D-056). The client also speaks the REST binding,
// selects the card interface (with the 1.0 protocol-version check), takes a
// preset card and request interceptors (clientconn.go, clientrest.go), and the
// server rewrites the served card per request (ServerConfig.CardModifier,
// CardCacheControl) and serves the authenticated extended card
// (ServerConfig.ExtendedCard, agent/getAuthenticatedExtendedCard and
// /extendedAgentCard), with optional input-mode validation
// (ServerConfig.ValidateInputModes), full AG2 history shipping with the server
// restore hook (history.go, ServerConfig.RestoreHistory), tenants — the claim
// on the wire plus the request-aware authorization of it (tenant.go,
// ServerConfig.TenantResolver/StoreForTenant, AuthorizeIdentity) — and
// agent-card JWS signing/verification over the same canonical payload the
// a2a-sdk signs (jws.go). Still deferred: the conversational A2AClient
// turn machinery (reconnect/resubscribe, the LLMClient adapter; D-049) and
// gRPC (D-071).
package a2a

import (
	"encoding/json"
	"fmt"

	"github.com/luckystrker/ago/internal/uuid"
)

// newID returns a fresh identifier for messages and tasks.
func newID() string { return uuid.New() }

// JSON tag note: A2A is a wire protocol with camelCase field names; tags here
// follow the A2A spec (unlike the snake_case event tags elsewhere in AGo,
// which mirror AG2's Python event objects).

// Role values carried on Message.Role.
const (
	RoleUser  = "user"
	RoleAgent = "agent"
)

// Part kind discriminators carried in the "kind" field of every Part.
const (
	PartKindText = "text"
	PartKindData = "data"
	PartKindFile = "file"
)

// Wire values of the discovery fields a 1.x client requires. The protocol
// version is the a2a-sdk PROTOCOL_VERSION_CURRENT (constants.py: "1.0"); the
// binding spelling is TransportProtocol.JSONRPC.
const (
	ProtocolVersionCurrent = "1.0"
	ProtocolBindingJSONRPC = "JSONRPC"
)

// AgentInterface is one transport binding an AgentCard can be reached
// through (parity: a2a-sdk AgentInterface — url, protocolBinding,
// protocolVersion, tenant).
type AgentInterface struct {
	URL             string `json:"url,omitempty"`
	ProtocolBinding string `json:"protocolBinding"`
	ProtocolVersion string `json:"protocolVersion"`
	Tenant          string `json:"tenant,omitempty"`
}

// AgentCard advertises one agent for A2A discovery. Server fills the
// discovery fields a 1.x client requires when the config leaves them unset
// (ProtocolVersion, SupportedInterfaces — parity: ag2/ag2/a2a/card.py
// build_card, which always emits supported_interfaces stamped with the
// current protocol version); a card built by hand is served verbatim.
//
// SecuritySchemes/Security carry the declarative auth requirements built by
// the security.go helpers (parity: ag2/ag2/a2a/security.py attaching
// Requirement/Scheme output to the card; wire fields follow the a2a-sdk
// AgentCard proto — security_schemes/security_requirements → camelCase).
type AgentCard struct {
	Name                string                     `json:"name"`
	Description         string                     `json:"description,omitempty"`
	URL                 string                     `json:"url,omitempty"`
	Version             string                     `json:"version,omitempty"`
	ProtocolVersion     string                     `json:"protocolVersion,omitempty"`
	SupportedInterfaces []AgentInterface           `json:"supportedInterfaces,omitempty"`
	Capabilities        AgentCapabilities          `json:"capabilities"`
	DefaultInputModes   []string                   `json:"defaultInputModes,omitempty"`
	DefaultOutputModes  []string                   `json:"defaultOutputModes,omitempty"`
	Skills              []AgentSkill               `json:"skills,omitempty"`
	SecuritySchemes     map[string]*SecurityScheme `json:"securitySchemes,omitempty"`
	Security            []SecurityRequirement      `json:"securityRequirements,omitempty"`
	// Provider is the producing organization (parity: a2a-sdk AgentProvider).
	// Modelled even though nothing in this package fills it: a card served by
	// another implementation may carry it, and every field of the signed card
	// has to survive the round trip through this struct or the signature would
	// be computed over a different payload.
	Provider *AgentProvider `json:"provider,omitempty"`
	// DocumentationURL and IconURL are card metadata (parity: a2a-sdk
	// documentation_url / icon_url).
	DocumentationURL string `json:"documentationUrl,omitempty"`
	IconURL          string `json:"iconUrl,omitempty"`
	// Signatures are the card's JWS signatures, verified by a client that was
	// configured with a CardVerifier and produced by a server configured with
	// a CardSigner (jws.go). Empty serves an unsigned card.
	Signatures []CardSignature `json:"signatures,omitempty"`
}

// AgentProvider names the organization behind an agent (parity: a2a-sdk
// AgentProvider — url, organization).
type AgentProvider struct {
	URL          string `json:"url,omitempty"`
	Organization string `json:"organization,omitempty"`
}

// AgentCapabilities declares what the served endpoints support. Extensions
// carries the protocol extensions advertised on the card (parity:
// a2a-sdk AgentCapabilities.extensions / AgentExtension); a server card
// advertising urn:ag2:client-tools:v1 must list it here, and
// ValidateExtensionActivation checks both directions against it.
//
// The three flags are proto3 `optional` fields in the a2a-sdk descriptor
// (synthetic oneofs _streaming / _push_notifications / _extended_agent_card),
// i.e. they have explicit presence: MessageToDict writes an explicitly set
// `false` and omits an unset field, and _clean_empty never touches a boolean.
// Presence therefore changes the signed payload, and the card has to carry it:
// a served card that says nothing about push support signs differently from one
// that says "false".
//
// The plain bool fields keep the old API. A true value is always present
// (nothing else can be meant by setting it); false is present only when the
// Set* method marked it, or when the card came from JSON that spelled the key
// out (UnmarshalJSON records presence). The wire form follows the same rule, so
// a card round-trips through JSON with its presence intact.
type AgentCapabilities struct {
	Streaming         bool             `json:"streaming,omitempty"`
	PushNotifications bool             `json:"pushNotifications,omitempty"`
	ExtendedAgentCard bool             `json:"extendedAgentCard,omitempty"`
	Extensions        []AgentExtension `json:"extensions,omitempty"`

	// presence bits (proto3 optional); unexported so the bool API stays.
	streamingPresent         bool
	pushNotificationsPresent bool
	extendedAgentCardPresent bool
}

// SetStreaming sets the streaming capability and marks it present, so an
// explicit false reaches the signed payload. Returns c for chaining.
func (c *AgentCapabilities) SetStreaming(v bool) *AgentCapabilities {
	c.Streaming, c.streamingPresent = v, true
	return c
}

// SetPushNotifications sets the push-notification capability and marks it
// present (parity: server.py always passes push_notifications=<bool> to
// build_card, so AG2 signs an explicit false too).
func (c *AgentCapabilities) SetPushNotifications(v bool) *AgentCapabilities {
	c.PushNotifications, c.pushNotificationsPresent = v, true
	return c
}

// SetExtendedAgentCard sets the extended-card capability and marks it present.
func (c *AgentCapabilities) SetExtendedAgentCard(v bool) *AgentCapabilities {
	c.ExtendedAgentCard, c.extendedAgentCardPresent = v, true
	return c
}

// MarshalJSON writes each flag when it is present — set to true, or explicitly
// marked by a Set* call or by a previous decode (parity: protobuf JSON omits an
// unset optional field and keeps a set false).
func (c AgentCapabilities) MarshalJSON() ([]byte, error) {
	type wire struct {
		Streaming         *bool            `json:"streaming,omitempty"`
		PushNotifications *bool            `json:"pushNotifications,omitempty"`
		ExtendedAgentCard *bool            `json:"extendedAgentCard,omitempty"`
		Extensions        []AgentExtension `json:"extensions,omitempty"`
	}
	out := wire{Extensions: c.Extensions}
	if c.streamingPresent || c.Streaming {
		v := c.Streaming
		out.Streaming = &v
	}
	if c.pushNotificationsPresent || c.PushNotifications {
		v := c.PushNotifications
		out.PushNotifications = &v
	}
	if c.extendedAgentCardPresent || c.ExtendedAgentCard {
		v := c.ExtendedAgentCard
		out.ExtendedAgentCard = &v
	}
	return json.Marshal(out)
}

// UnmarshalJSON decodes the flags and records which ones the card spelled out,
// so presence survives the wire (a JSON false is present, an absent key is not).
func (c *AgentCapabilities) UnmarshalJSON(data []byte) error {
	var wire struct {
		Streaming         *bool            `json:"streaming"`
		PushNotifications *bool            `json:"pushNotifications"`
		ExtendedAgentCard *bool            `json:"extendedAgentCard"`
		Extensions        []AgentExtension `json:"extensions"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*c = AgentCapabilities{Extensions: wire.Extensions}
	if wire.Streaming != nil {
		c.Streaming, c.streamingPresent = *wire.Streaming, true
	}
	if wire.PushNotifications != nil {
		c.PushNotifications, c.pushNotificationsPresent = *wire.PushNotifications, true
	}
	if wire.ExtendedAgentCard != nil {
		c.ExtendedAgentCard, c.extendedAgentCardPresent = *wire.ExtendedAgentCard, true
	}
	return nil
}

// AgentExtension is one protocol extension advertised on the card
// (parity: a2a-sdk AgentExtension — uri, description, required, params).
type AgentExtension struct {
	URI         string         `json:"uri"`
	Description string         `json:"description,omitempty"`
	Required    bool           `json:"required,omitempty"`
	Params      map[string]any `json:"params,omitempty"`
}

// AgentSkill describes one capability listed on an AgentCard.
type AgentSkill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Examples    []string `json:"examples,omitempty"`
	// InputModes narrows the card's defaultInputModes for this skill (parity:
	// a2a-sdk AgentSkill.inputModes). ValidateInputModes reads them as part of
	// the card's declared set.
	InputModes []string `json:"inputModes,omitempty"`
	// OutputModes narrows defaultOutputModes for this skill, and
	// SecurityRequirements narrows the card's requirements (parity: a2a-sdk
	// AgentSkill.output_modes / security_requirements). Modelled for signature
	// round-trip fidelity like Provider above.
	OutputModes          []string              `json:"outputModes,omitempty"`
	SecurityRequirements []SecurityRequirement `json:"securityRequirements,omitempty"`
}

// Part is one typed payload inside a Message. The concrete type is selected by
// the wire "kind" discriminator: TextPart, DataPart or FilePart.
type Part interface {
	PartKind() string
}

// TextPart carries UTF-8 text; the A2A workhorse part.
type TextPart struct {
	Text string `json:"text"`
}

// NewTextPart builds a TextPart.
func NewTextPart(text string) *TextPart { return &TextPart{Text: text} }

// PartKind implements Part.
func (*TextPart) PartKind() string { return PartKindText }

// MarshalJSON stamps the "kind" discriminator — the field is structural and
// cannot be set to anything else by a caller.
func (p TextPart) MarshalJSON() ([]byte, error) {
	type plain TextPart
	return json.Marshal(struct {
		Kind string `json:"kind"`
		plain
	}{Kind: PartKindText, plain: plain(p)})
}

// DataPart carries an arbitrary JSON value. MimeType selects an extension
// payload riding on the data part (the urn:ag2:client-tools:v1 parts are
// data parts with a vnd.ag2.* media type — extension.go); Metadata is the
// part-level free-form map of the A2A part shape.
type DataPart struct {
	Data     any            `json:"data"`
	MimeType string         `json:"mediaType,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// NewDataPart builds a DataPart.
func NewDataPart(data any) *DataPart { return &DataPart{Data: data} }

// PartKind implements Part.
func (*DataPart) PartKind() string { return PartKindData }

// MarshalJSON stamps the "kind" discriminator.
func (p DataPart) MarshalJSON() ([]byte, error) {
	type plain DataPart
	return json.Marshal(struct {
		Kind string `json:"kind"`
		plain
	}{Kind: PartKindData, plain: plain(p)})
}

// FileBlock locates or embeds one file: exactly one of URI or Bytes is
// expected, per the A2A file part.
type FileBlock struct {
	Name     string `json:"name,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	URI      string `json:"uri,omitempty"`
	Bytes    string `json:"bytes,omitempty"` // base64
}

// FilePart references one file by URI or inline base64 bytes.
type FilePart struct {
	File FileBlock `json:"file"`
}

// NewFilePart builds a FilePart.
func NewFilePart(file FileBlock) *FilePart { return &FilePart{File: file} }

// PartKind implements Part.
func (*FilePart) PartKind() string { return PartKindFile }

// MarshalJSON stamps the "kind" discriminator.
func (p FilePart) MarshalJSON() ([]byte, error) {
	type plain FilePart
	return json.Marshal(struct {
		Kind string `json:"kind"`
		plain
	}{Kind: PartKindFile, plain: plain(p)})
}

// decodePart unmarshals one wire part into its concrete type by "kind".
func decodePart(data []byte) (Part, error) {
	var probe struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, err
	}
	switch probe.Kind {
	case PartKindText:
		var p TextPart
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, err
		}
		return &p, nil
	case PartKindData:
		var p DataPart
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, err
		}
		return &p, nil
	case PartKindFile:
		var p FilePart
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, err
		}
		return &p, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrWire, probe.Kind)
	}
}

// Message is one wire message: the unit exchanged by message/send and carried
// in Task history and status. Role is RoleUser or RoleAgent. Extensions lists
// activated protocol extension URIs (the A2A-Extensions header is the
// transport-level twin — extension.go); Metadata is the free-form map the
// context-variable sync rides on under ContextUpdateKey (contextwire.go).
type Message struct {
	Role       string         `json:"role"`
	Parts      []Part         `json:"parts"`
	MessageID  string         `json:"messageId,omitempty"`
	ContextID  string         `json:"contextId,omitempty"`
	TaskID     string         `json:"taskId,omitempty"`
	Extensions []string       `json:"extensions,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// NewMessage builds a Message with a fresh MessageID. Role defaults to
// RoleUser when empty.
func NewMessage(role string, parts ...Part) *Message {
	if role == "" {
		role = RoleUser
	}
	return &Message{Role: role, Parts: parts, MessageID: newID()}
}

// UnmarshalJSON decodes parts by their "kind" discriminator.
func (m *Message) UnmarshalJSON(data []byte) error {
	var wire struct {
		Role       string            `json:"role"`
		Parts      []json.RawMessage `json:"parts"`
		MessageID  string            `json:"messageId"`
		ContextID  string            `json:"contextId"`
		TaskID     string            `json:"taskId"`
		Extensions []string          `json:"extensions"`
		Metadata   map[string]any    `json:"metadata"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	m.Role, m.MessageID, m.ContextID, m.TaskID = wire.Role, wire.MessageID, wire.ContextID, wire.TaskID
	m.Extensions, m.Metadata = wire.Extensions, wire.Metadata
	m.Parts = nil
	for _, raw := range wire.Parts {
		p, err := decodePart(raw)
		if err != nil {
			return err
		}
		m.Parts = append(m.Parts, p)
	}
	return nil
}

// TaskState enumerates the task lifecycle states of this subset.
//
// AGO-DEVIATION: "unknown" exists as a constant for a client decoding a state
// it does not know, but the server never stores it — it has no server-side
// meaning. input-required came in the second wave because the
// urn:ag2:client-tools:v1 extension and HITL-over-the-wire are meaningless
// without a state that says "answer pending on the caller" (executor.py
// updater.requires_input); auth-required and rejected came with the third.
type TaskState string

// Task states.
const (
	TaskStateSubmitted     TaskState = "submitted"
	TaskStateWorking       TaskState = "working"
	TaskStateInputRequired TaskState = "input-required"
	// TaskStateAuthRequired parks a task until the caller presents
	// credentials (A2A spec §4.5). Like input-required it is interrupted, not
	// terminal: a continuation with the same task id resumes it.
	TaskStateAuthRequired TaskState = "auth-required"
	TaskStateCompleted    TaskState = "completed"
	TaskStateFailed       TaskState = "failed"
	TaskStateCanceled     TaskState = "canceled"
	// TaskStateRejected is a terminal refusal: the agent would not take the
	// task at all (parity: a2a TASK_STATE_REJECTED, A2ATaskRejectedError).
	TaskStateRejected TaskState = "rejected"
	// TaskStateUnknown is the forward-compatibility state: a client that
	// receives a state it does not know treats the task as unreadable. The
	// server never stores it; it exists so a decoder has a constant to map to.
	TaskStateUnknown TaskState = "unknown"
)

// terminalTaskState reports whether state closes the task: a terminal task
// cannot be canceled (parity: a2a TaskNotCancelableError on terminal states).
// rejected is terminal; auth-required and input-required are interruptions and
// stay resumable.
func terminalTaskState(state TaskState) bool {
	switch state {
	case TaskStateCompleted, TaskStateFailed, TaskStateCanceled, TaskStateRejected:
		return true
	}
	return false
}

// TaskStatus is the current state of a Task, optionally carrying the message
// attached to the latest transition (the agent's reply on completed, the
// failure text on failed).
type TaskStatus struct {
	State   TaskState `json:"state"`
	Message *Message  `json:"message,omitempty"`
}

// Task is the server-side record of one exchange: identity, status, the
// message history that produced it, and artifacts emitted along the way.
type Task struct {
	ID        string     `json:"id"`
	ContextID string     `json:"contextId"`
	Status    TaskStatus `json:"status"`
	History   []Message  `json:"history,omitempty"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
}

// Artifact is one output bundle attached to a Task.
type Artifact struct {
	ArtifactID string `json:"artifactId"`
	Name       string `json:"name,omitempty"`
	Parts      []Part `json:"parts"`
}

// UnmarshalJSON decodes parts by their "kind" discriminator.
func (a *Artifact) UnmarshalJSON(data []byte) error {
	var wire struct {
		ArtifactID string            `json:"artifactId"`
		Name       string            `json:"name"`
		Parts      []json.RawMessage `json:"parts"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	a.ArtifactID, a.Name = wire.ArtifactID, wire.Name
	a.Parts = nil
	for _, raw := range wire.Parts {
		p, err := decodePart(raw)
		if err != nil {
			return err
		}
		a.Parts = append(a.Parts, p)
	}
	return nil
}

// StatusUpdate is the streamed result payload of message/stream. Kind is
// always the A2A discriminator "status-update"; Final marks the last event of
// a stream.
type StatusUpdate struct {
	Kind      string     `json:"kind"`
	TaskID    string     `json:"taskId"`
	ContextID string     `json:"contextId"`
	Status    TaskStatus `json:"status"`
	Final     bool       `json:"final"`
}

// KindStatusUpdate is the wire discriminator of StatusUpdate.
const KindStatusUpdate = "status-update"
