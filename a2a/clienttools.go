package a2a

import (
	"context"
	"encoding/json"
	"fmt"
)

// Client-side drivers of the second wave: the Task-shaped message/send
// exchange the extension and HITL loops drive, the urn:ag2:client-tools:v1
// round trip with local execution through a callback, and the
// human-in-the-loop continuation loop (parity: the AG2 conversational client
// of ag2/ag2/a2a/client.py driving input_required continuations — the full
// history-shipping LLMClient remains deferred, D-049/D-056).

// wireRoundLimit caps the client-tools / HITL loops: each iteration is one
// wire exchange, and an unbounded loop lets a misbehaving server pin the
// caller forever. Python caps reconnections instead (max_reconnects) and
// relies on the caller's input_required_timeout for the human leg; the fixed
// wire cap is this package's honest equivalent.
const wireRoundLimit = 16

// SendMessageTask exchanges one message and returns the result in its Task
// shape: a server-side park (input-required — client tools requested, HITL
// question pending) comes back as the stored Task record, an immediate
// completion as a synthetic Task carrying the reply Message on status.message
// (parity: the AG2 client absorbing both result shapes, client.py result
// probing this package's Client.SendMessage mirrors).
func (c *Client) SendMessageTask(ctx context.Context, msg *Message) (*Task, error) {
	result, err := c.call(ctx, MethodMessageSend, map[string]any{"message": msg})
	if err != nil {
		return nil, err
	}
	var probe struct {
		Role   string      `json:"role"`
		Status *TaskStatus `json:"status"`
	}
	if err := json.Unmarshal(result, &probe); err != nil {
		return nil, fmt.Errorf("%w: decode reply: %v", ErrWire, err)
	}
	if probe.Status == nil {
		// Message shape: an immediate completion.
		var reply Message
		if err := json.Unmarshal(result, &reply); err != nil {
			return nil, fmt.Errorf("%w: decode reply: %v", ErrWire, err)
		}
		return &Task{
			ID:        reply.TaskID,
			ContextID: reply.ContextID,
			Status:    TaskStatus{State: TaskStateCompleted, Message: &reply},
		}, nil
	}
	var task Task
	if err := json.Unmarshal(result, &task); err != nil {
		return nil, fmt.Errorf("%w: decode task: %v", ErrWire, err)
	}
	return &task, nil
}

// RunClientTools drives the urn:ag2:client-tools:v1 round trip: it announces
// schemas on the opening message (AttachClientTools), and whenever the server
// parks the task input-required with tool-call parts it executes them through
// execute and resumes the task with the results, until the server completes
// the task or the wire round limit is hit (parity: client.py's
// input_required loop around build_tool_result_message continuations).
//
// The reply is the completed status message; a context_update delta on it is
// readable with ExtractContextUpdate. An input-required park without tool
// calls (a plain HITL question) is a wire error here — use
// RunWithHumanInput for that leg.
func (c *Client) RunClientTools(ctx context.Context, msg *Message, schemas []ToolSchema, execute func(ctx context.Context, calls []ToolCall) ([]ToolResultEntry, error)) (*Message, error) {
	if execute == nil {
		return nil, fmt.Errorf("%w: RunClientTools needs an execute callback", ErrNoHandler)
	}
	if msg == nil {
		return nil, fmt.Errorf("%w: message is required", ErrNoHandler)
	}
	if len(schemas) > 0 {
		AttachClientTools(msg, schemas)
	}
	for round := 0; round < wireRoundLimit; round++ {
		task, err := c.SendMessageTask(ctx, msg)
		if err != nil {
			return nil, err
		}
		if task.Status.State != TaskStateInputRequired {
			return absorbTerminal(task)
		}
		calls, err := ParseToolCalls(task.Status.Message)
		if err != nil {
			return nil, err
		}
		if len(calls) == 0 {
			return nil, fmt.Errorf("%w: input-required without client tool calls", ErrWire)
		}
		results, err := execute(ctx, calls)
		if err != nil {
			return nil, fmt.Errorf("execute client tools: %w", err)
		}
		msg = BuildToolResultMessage(task.ID, task.ContextID, results, schemas)
	}
	return nil, fmt.Errorf("%w: exceeded %d client-tools rounds", ErrWire, wireRoundLimit)
}

// RunWithHumanInput drives the HITL-over-the-wire loop: whenever the server
// parks the task input-required, the text of the parked message is the
// question; answer supplies the human's reply, which resumes the task as a
// text continuation under the same task id (parity: build_input_response_message
// resuming TASK_STATE_INPUT_REQUIRED). Tool-call parks are not answered here
// — use RunClientTools.
func (c *Client) RunWithHumanInput(ctx context.Context, msg *Message, answer func(ctx context.Context, question string) (string, error)) (*Message, error) {
	if answer == nil {
		return nil, fmt.Errorf("%w: RunWithHumanInput needs an answer callback", ErrNoHandler)
	}
	if msg == nil {
		return nil, fmt.Errorf("%w: message is required", ErrNoHandler)
	}
	for round := 0; round < wireRoundLimit; round++ {
		task, err := c.SendMessageTask(ctx, msg)
		if err != nil {
			return nil, err
		}
		if task.Status.State != TaskStateInputRequired {
			return absorbTerminal(task)
		}
		question := textOfMessage(task.Status.Message)
		reply, err := answer(ctx, question)
		if err != nil {
			return nil, fmt.Errorf("answer human input: %w", err)
		}
		msg = NewInputResponseMessage(task.ID, task.ContextID, reply, c.extensions)
	}
	return nil, fmt.Errorf("%w: exceeded %d human-input rounds", ErrWire, wireRoundLimit)
}

// absorbTerminal turns a terminal task into the caller's reply: completed →
// the status message; anything else → an error carrying the state.
func absorbTerminal(task *Task) (*Message, error) {
	switch task.Status.State {
	case TaskStateCompleted:
		return task.Status.Message, nil
	case TaskStateFailed:
		return nil, fmt.Errorf("%w: task %s failed", ErrRPC, task.ID)
	default:
		return nil, fmt.Errorf("%w: task %s ended in state %q", ErrWire, task.ID, task.Status.State)
	}
}

// textOfMessage joins the text parts of a message — the wire rendering of a
// question or final answer (parity: parts.py tool_result_to_text's flat-text
// convention).
func textOfMessage(m *Message) string {
	if m == nil {
		return ""
	}
	var b []byte
	for _, p := range m.Parts {
		if tp, ok := p.(*TextPart); ok {
			b = append(b, tp.Text...)
		}
	}
	return string(b)
}
