package events

import "ago/internal/uuid"

// HumanInputRequest asks the human operator for input (HITL). The answer arrives as a
// HumanMessage whose ParentID matches this request's ID.
type HumanInputRequest struct {
	Base
	ID      string  `json:"id"`
	Content string  `json:"content"`
	Timeout float64 `json:"timeout,omitempty"` // seconds; 0 = no timeout
	TaskID  string  `json:"task_id,omitempty"`
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
}

func (HumanMessage) EventType() string    { return "HumanMessage" }
func (HumanMessage) Transient() bool      { return false }
func (HumanMessage) Conversational() bool { return false }

func init() {
	RegisterEvent(HumanInputRequest{})
	RegisterEvent(ToolApprovalRequest{})
	RegisterEvent(HumanMessage{})
}
