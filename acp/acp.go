// Package acp speaks the Agent Client Protocol (ACP): JSON-RPC 2.0 over a
// process's stdin/stdout, one message per line (the same newline transport the
// mcp stdio client uses — no Content-Length framing).
//
// Both protocol roles live here:
//
//   - Serve (the agent role) exposes a handler over stdin/stdout so any ACP
//     client can drive it: initialize / session/new / session/prompt, with the
//     handler's output pushed out as session/update notifications.
//   - Bridge (the client role) drives an external ACP agent running as a
//     subprocess (a CLI coding agent in ACP mode): it performs the initialize
//     handshake, opens sessions and collects the agent_message_chunk text of
//     each prompt.
//
// Python parity: the serving half is ag2/ag2/acp/agent.py + executor.py +
// session.py + mappers.py; the consuming half is ag2/acp/bridge.py +
// client.py; auth.py and tool_gateway.py have their own files here. The
// subset is functional on purpose — what is left out and why is recorded in
// docs/DECISIONS.md (D-052, extended by D-066).
package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Protocol identity on the wire. ACP versions the protocol with a plain
// integer; the Python serve side answers initialize with
// min(client's version, ours) (agent.py:341).
const protocolVersion = 1

// How AGo introduces itself in a handshake (client role).
const (
	clientName    = "ago-acp-client"
	clientVersion = "0.1.0"
)

// Defaults advertised by the serve role when ServeConfig leaves them unset
// (Python parity: agent.py _DEFAULT_VERSION).
const (
	defaultAgentName    = "ago-agent"
	defaultAgentVersion = "0.0.0"
)

// JSON-RPC error codes this package puts on the wire.
const (
	codeInvalidRequest   = -32600 // a session method before initialize
	codeMethodNotFound   = -32601 // a method outside the subset (a capability-gated method never advertised)
	codeInvalidParams    = -32602
	codeInternalError    = -32603 // a handler failure (Python parity: internal_error with the reason)
	codeAuthRequired     = -32000 // ACP: Authentication required
	codeResourceNotFound = -32002 // ACP: Resource not found (an unknown session, an unreadable file)
)

// Package-level sentinels. Match with errors.Is.
var (
	// ErrNotProvided reports a Bridge command that is an empty string.
	ErrNotProvided = errors.New("acp: command not provided")
	// ErrProtocol reports a wire-level failure: malformed JSON-RPC, an error
	// object from the peer, or a peer that exited before answering.
	ErrProtocol = errors.New("acp: protocol error")
	// ErrClosed reports an operation on a closed Bridge.
	ErrClosed = errors.New("acp: bridge is closed")
	// ErrTimeout reports a Bridge exchange that outlived
	// Bridge.ExchangeTimeout; the agent subprocess was killed to unblock the
	// wait, so the Bridge is no longer usable for further exchanges.
	ErrTimeout = errors.New("acp: exchange timed out")
)

// killAfter bounds a subprocess shutdown: closing stdin lets a well-behaved
// agent exit; past this deadline it is killed (same teardown as mcp stdio).
const killAfter = 5 * time.Second

// scanBuffer caps one incoming line. A prompt may inline base64 content, and
// an ACP frame is one line of JSON, so the cap matches the mcp stdio client.
const scanBuffer = 8 << 20 // 8 MiB

// --- JSON-RPC 2.0 wire shapes -------------------------------------------------

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcNotification is a message without an id; a request shape carrying "id":0
// would be misread as a request by the peer.
type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("code %d: %s", e.Code, e.Message) }

func rpcErr(code int, format string, args ...any) *rpcError {
	return &rpcError{Code: code, Message: fmt.Sprintf(format, args...)}
}

type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      int64     `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

// incoming is the discriminating shape used while scanning a stream of lines:
// a message with a method is a request (id set) or a notification (id unset);
// one with an id and no method is the reply to something we sent.
type incoming struct {
	ID     *int64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// writeLine marshals one message and writes it newline-terminated. json.Marshal
// never emits raw newlines, so one Write is one message.
func writeLine(w io.Writer, msg any) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("%w: encode message: %v", ErrProtocol, err)
	}
	if _, err := w.Write(append(payload, '\n')); err != nil {
		return fmt.Errorf("acp: write: %w", err)
	}
	return nil
}

// --- content blocks -----------------------------------------------------------

// contentBlock is one prompt content block. This subset carries text only;
// image/audio/resource blocks in a prompt are ignored while their text
// siblings are kept (Python parity: mappers.prompt_to_inputs skips blocks with
// no representation rather than failing the turn).
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func textBlock(s string) contentBlock { return contentBlock{Type: "text", Text: s} }

// blocksText concatenates the text of text blocks, ignoring the rest (Python
// parity: mappers.content_blocks_to_text).
func blocksText(blocks []contentBlock) string {
	var b []byte
	for _, blk := range blocks {
		if blk.Type == "text" {
			b = append(b, blk.Text...)
		}
	}
	return string(b)
}

// --- session/update -----------------------------------------------------------

// Stop reasons a session/prompt answer may carry. The subset reports the two
// it can know truthfully (Python parity: executor.py STOP_END_TURN /
// STOP_CANCELLED); max_tokens, max_turn_requests and refusal are protocol
// names for signals a turn carries no unambiguous mark of.
const (
	StopEndTurn   = "end_turn"
	StopCancelled = "cancelled"
)

// Tool call status values (Python parity: the acp schema ToolCallStatus).
const (
	StatusPending    = "pending"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
)

// Tool call kinds. AG2 tool events carry no kind, and guessing one from a
// tool's name would be wrong more often than useful — every update this
// package emits uses KindOther (mappers.py DEFAULT_TOOL_KIND).
const (
	KindRead       = "read"
	KindEdit       = "edit"
	KindDelete     = "delete"
	KindMove       = "move"
	KindSearch     = "search"
	KindExecute    = "execute"
	KindThink      = "think"
	KindFetch      = "fetch"
	KindSwitchMode = "switch_mode"
	KindOther      = "other"
)

// Plan entry status and priority values (Python parity: the acp schema
// PlanEntryStatus / PlanEntryPriority).
const (
	PlanPending    = "pending"
	PlanInProgress = "in_progress"
	PlanCompleted  = "completed"

	PriorityHigh   = "high"
	PriorityMedium = "medium"
	PriorityLow    = "low"
)

// Update is one "session/update" payload — the whole vocabulary this subset
// speaks, not just the message chunk. Exported because it crosses the boundary
// in both directions: the serve role emits it (Emitter) and the Bridge hands
// every received update to OnUpdate.
//
// Chunks carry Text (and MessageID on a replay); tool_call / tool_call_update
// carry ToolCallID, Title, ToolKind, Status, RawInput and ContentText; plan
// carries Entries; usage_update carries Used, Size and Cost. Marshalling puts
// each kind's fields under the keys the Python SDK serializes (schema.py
// aliases: sessionUpdate, messageId, toolCallId, rawInput, used, size, cost),
// with the earlier revision's "contentBlock" spelling still accepted on input.
type Update struct {
	Kind string `json:"sessionUpdate"`

	// chunk kinds: user_message_chunk / agent_message_chunk / agent_thought_chunk
	MessageID string `json:"messageId,omitempty"`
	Text      string `json:"-"`

	// tool_call / tool_call_update
	ToolCallID  string          `json:"-"`
	Title       string          `json:"-"`
	ToolKind    string          `json:"-"`
	Status      string          `json:"-"`
	RawInput    json.RawMessage `json:"-"`
	ContentText string          `json:"-"`

	// plan
	Entries []PlanEntry `json:"-"`

	// usage_update
	Used int64      `json:"-"`
	Size int64      `json:"-"`
	Cost *UsageCost `json:"-"`
}

// chunkKinds are the updates whose payload is one content block.
var chunkKinds = map[string]bool{
	"user_message_chunk":  true,
	"agent_message_chunk": true,
	"agent_thought_chunk": true,
}

func (u Update) isChunk() bool { return chunkKinds[u.Kind] }

// MarshalJSON emits the union: per kind, exactly the fields that kind carries.
func (u Update) MarshalJSON() ([]byte, error) {
	m := map[string]any{"sessionUpdate": u.Kind}
	switch {
	case u.isChunk():
		blk := textBlock(u.Text)
		m["content"] = blk
		if u.MessageID != "" {
			m["messageId"] = u.MessageID
		}
	case u.Kind == "tool_call" || u.Kind == "tool_call_update":
		m["toolCallId"] = u.ToolCallID
		if u.Title != "" {
			m["title"] = u.Title
		}
		if u.ToolKind != "" {
			m["kind"] = u.ToolKind
		}
		if u.Status != "" {
			m["status"] = u.Status
		}
		if len(u.RawInput) > 0 {
			m["rawInput"] = u.RawInput
		}
		if u.ContentText != "" {
			m["content"] = []map[string]any{{"type": "content", "content": textBlock(u.ContentText)}}
		}
	case u.Kind == "plan":
		if u.Entries == nil {
			u.Entries = []PlanEntry{}
		}
		m["entries"] = u.Entries
	case u.Kind == "usage_update":
		m["used"] = u.Used
		m["size"] = u.Size
		if u.Cost != nil {
			m["cost"] = u.Cost
		}
	default:
		return nil, fmt.Errorf("acp: unknown session/update kind %q", u.Kind)
	}
	return json.Marshal(m)
}

// updateWire is the decode shape: one struct for every field any kind carries,
// with the legacy "contentBlock" spelling of the earlier revision of this
// package still accepted. "content" is a single block on the chunk kinds and a
// list on the tool-call kinds, so it arrives raw and is decoded per kind.
type updateWire struct {
	SessionUpdate string          `json:"sessionUpdate"`
	MessageID     string          `json:"messageId"`
	ContentBlock  *contentBlock   `json:"contentBlock"` // legacy input dialect, never emitted
	ContentRaw    json.RawMessage `json:"content"`
	ToolCallID    string          `json:"toolCallId"`
	Title         string          `json:"title"`
	ToolKind      string          `json:"kind"`
	Status        string          `json:"status"`
	RawInput      json.RawMessage `json:"rawInput"`
	Entries       []PlanEntry     `json:"entries"`
	Used          *int64          `json:"used"`
	Size          *int64          `json:"size"`
	Cost          *UsageCost      `json:"cost"`
}

// toolContentItem is one item of a tool call's content list (Python parity:
// the acp schema ContentToolCallContent — {"type": "content", "content": block}).
type toolContentItem struct {
	Type    string        `json:"type"`
	Content *contentBlock `json:"content"`
}

// UnmarshalJSON decodes every kind this subset knows. A kind outside the
// subset decodes to an Update the emitters can never produce — the Bridge
// hands it to OnUpdate (kind name intact, payload dropped) so a peer's richer
// vocabulary stays visible instead of vanishing.
func (u *Update) UnmarshalJSON(data []byte) error {
	var w updateWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	u.Kind = w.SessionUpdate
	u.MessageID = w.MessageID
	switch {
	case u.isChunk():
		blk := w.ContentBlock // legacy dialect first: "content" below overrides
		if err := json.Unmarshal(w.ContentRaw, &blk); err != nil {
			blk = w.ContentBlock
		}
		if blk != nil {
			u.Text = blk.Text
		}
	case u.Kind == "tool_call" || u.Kind == "tool_call_update":
		u.ToolCallID, u.Title, u.ToolKind, u.Status = w.ToolCallID, w.Title, w.ToolKind, w.Status
		u.RawInput = w.RawInput
		var items []toolContentItem
		if err := json.Unmarshal(w.ContentRaw, &items); err == nil {
			for _, item := range items {
				if item.Content != nil {
					u.ContentText += item.Content.Text
				}
			}
		}
	case u.Kind == "plan":
		u.Entries = w.Entries
	case u.Kind == "usage_update":
		if w.Used != nil {
			u.Used = *w.Used
		}
		if w.Size != nil {
			u.Size = *w.Size
		}
		u.Cost = w.Cost
	}
	return nil
}

// text returns the chunk's text, whichever block field carried it.
func (u *Update) text() string { return u.Text }

// agentMessageChunk builds the message-chunk update (Python parity:
// acp.update_agent_message_text).
func agentMessageChunk(text string) Update {
	return Update{Kind: "agent_message_chunk", Text: text}
}

// PlanEntry is one entry of a plan update (Python parity: the acp schema
// PlanEntry — content, priority, status).
type PlanEntry struct {
	Content  string `json:"content"`
	Priority string `json:"priority,omitempty"`
	Status   string `json:"status"`
}

// UsageCost is the optional cost block of a usage_update (Python parity: the
// acp schema Cost).
type UsageCost struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

type updateParams struct {
	SessionID string `json:"sessionId"`
	Update    Update `json:"update"`
}

// decodeUpdate parses one session/update notification's params, or nil when
// the params are not an update at all (a foreign notification).
func decodeUpdate(params json.RawMessage) *updateParams {
	var p updateParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil
	}
	if p.Update.Kind == "" {
		return nil
	}
	return &p
}

// --- session/request_permission ----------------------------------------------

// Permission option kinds an agent may offer (Python parity:
// permissions.py _ALLOW_KINDS/_REJECT_KINDS).
const (
	kindAllowOnce    = "allow_once"
	kindAllowAlways  = "allow_always"
	kindRejectOnce   = "reject_once"
	kindRejectAlways = "reject_always"
)

// The inner outcome values of a session/request_permission answer (Python
// parity: the acp schema AllowedOutcome/DeniedOutcome "selected"/"cancelled").
const (
	OutcomeSelected  = "selected"
	OutcomeCancelled = "cancelled"
)

// Decision is the resolved outcome of one permission request, on the wire as a
// session/request_permission result and in memory as Session.RequestPermission's
// return value. Emission nests the outcome object under the "outcome" key —
// {"outcome": {"outcome": "selected", "optionId": …}} — the shape the Python
// SDK puts on the wire (bridge.py: RequestPermissionResponse(outcome=
// AllowedOutcome(...))). Input accepts the flat {"outcome": …, "optionId": …}
// spelling of the earlier revision of this package too.
type Decision struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

// decisionOutcome is the inner outcome object of the nested wire answer.
type decisionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

// MarshalJSON nests the outcome object per the SDK schema. A zero Decision
// emits the cancelled outcome: an absent selection denies, never allows.
func (d Decision) MarshalJSON() ([]byte, error) {
	inner := decisionOutcome{Outcome: d.Outcome, OptionID: d.OptionID}
	if inner.Outcome == "" {
		inner.Outcome = OutcomeCancelled
	}
	return json.Marshal(map[string]decisionOutcome{"outcome": inner})
}

// UnmarshalJSON decodes the nested SDK shape, falling back to the flat
// {"outcome": "selected", "optionId": …} dialect of the earlier revision of
// this package.
func (d *Decision) UnmarshalJSON(data []byte) error {
	var nested struct {
		Outcome *decisionOutcome `json:"outcome"`
	}
	if err := json.Unmarshal(data, &nested); err == nil && nested.Outcome != nil {
		d.Outcome = nested.Outcome.Outcome
		d.OptionID = nested.Outcome.OptionID
		return nil
	}
	var flat struct {
		Outcome  string `json:"outcome"`
		OptionID string `json:"optionId,omitempty"`
	}
	if err := json.Unmarshal(data, &flat); err != nil {
		return err
	}
	d.Outcome, d.OptionID = flat.Outcome, flat.OptionID
	return nil
}

// Allowed reports whether the decision selects an allow option. "reject" is
// the optionId the subset synthesizes when a policy denies (see
// ServeConfig.Permission); anything else selected counts as allowed.
func (d Decision) Allowed() bool {
	return d.Outcome == OutcomeSelected && d.OptionID != "" && d.OptionID != "reject"
}

// rejectDecision is the deny the serve role synthesizes from its policy so a
// denial never touches the wire.
func rejectDecision() Decision { return Decision{Outcome: OutcomeSelected, OptionID: "reject"} }

type permissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

type permToolCall struct {
	ToolCallID string          `json:"toolCallId,omitempty"`
	Title      string          `json:"title"`
	RawInput   json.RawMessage `json:"rawInput,omitempty"`
}

// inputString renders the tool call's raw input for a policy: a JSON string
// unquotes, anything else keeps its raw bytes.
func (t permToolCall) inputString() string {
	if len(t.RawInput) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(t.RawInput, &s); err == nil {
		return s
	}
	return string(t.RawInput)
}

type requestPermissionParams struct {
	SessionID string             `json:"sessionId"`
	ToolCall  permToolCall       `json:"toolCall"`
	Options   []permissionOption `json:"options"`
}

// --- elicitation/create -------------------------------------------------------

// The three actions of an elicitation answer (Python parity:
// elicitation.py — the user answered / refused / nobody to ask).
const (
	ElicitAccept  = "accept"
	ElicitDecline = "decline"
	ElicitCancel  = "cancel"
)

type elicitationParams struct {
	Message string `json:"message"`
}

type elicitationResult struct {
	Action  string `json:"action"`
	Content any    `json:"content,omitempty"`
}
