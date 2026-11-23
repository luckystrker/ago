package events

import "github.com/luckystrker/ago/internal/uuid"

// HumanInputRequest asks the human operator for input (HITL). The answer arrives as a
// HumanMessage whose ParentID matches this request's ID.
type HumanInputRequest struct {
	Base
	ID      string  `json:"id"`
	Content string  `json:"content"`
	Timeout float64 `json:"timeout,omitempty"` // seconds; 0 = no timeout
	TaskID  string  `json:"task_id,omitempty"`
	// Choices, when present, are the answers on offer: a UI draws one button per
	// choice and the human picks one by Value (see NewApprovalChoiceMessage). It
	// sits here rather than on ToolApprovalRequest because Context.Ask hands hooks
	// and middleware the embedded HumanInputRequest.
	//
	// AGO-DEVIATION: Python declares choices on ToolApprovalRequest; the field is
	// promoted through the embedding, so req.Choices and the JSON key are the same.
	// docs/DECISIONS.md D-132.
	Choices []ApprovalChoice `json:"choices,omitempty"`
}

// ApprovalChoice is one answer a request offers: Value is what picks it, Label
// what a human reads.
type ApprovalChoice struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

func (HumanInputRequest) EventType() string    { return "HumanInputRequest" }
func (HumanInputRequest) Transient() bool      { return false }
func (HumanInputRequest) Conversational() bool { return true }

// NewHumanInputRequest creates a request with a fresh id.
func NewHumanInputRequest(content string, timeout float64) HumanInputRequest {
	return HumanInputRequest{ID: uuid.New(), Content: content, Timeout: timeout}
}

// ToolApprovalRequest asks the human to approve one tool call.
type ToolApprovalRequest struct {
	HumanInputRequest
	ToolCallID string `json:"tool_call_id"`
}

func (ToolApprovalRequest) EventType() string { return "ToolApprovalRequest" }

// HumanMessage is the human's answer to a HumanInputRequest.
type HumanMessage struct {
	Base
	ParentID string `json:"parent_id"`
	Content  string `json:"content"`
	// Choice marks an answer that picks one of the request's Choices: Content is
	// then the picked Value, exactly as offered, and is never read as a word. An
	// unmarked message is words, even when they spell a Value ("y" is a yes, not
	// the choice with value "y").
	//
	// AGO-DEVIATION: Python carries the pick as a HumanMessage subclass
	// (ApprovalChoiceMessage); hitl.HumanHook and the middleware chain return a
	// *HumanMessage, so the pick travels as a marker on it. docs/DECISIONS.md D-132.
	Choice bool `json:"choice,omitempty"`
}

// NewApprovalChoiceMessage is the answer that picks the choice with the given
// value from the request parentID.
func NewApprovalChoiceMessage(parentID, value string) *HumanMessage {
	return &HumanMessage{ParentID: parentID, Content: value, Choice: true}
}

// IsChoice reports whether the message picks a choice rather than answering in
// words.
func (m HumanMessage) IsChoice() bool { return m.Choice }

func (HumanMessage) EventType() string    { return "HumanMessage" }
func (HumanMessage) Transient() bool      { return false }
func (HumanMessage) Conversational() bool { return true }

func init() {
	RegisterEvent(HumanInputRequest{})
	RegisterEvent(ToolApprovalRequest{})
	RegisterEvent(HumanMessage{})
}
