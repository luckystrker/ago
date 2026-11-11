// Provider-native progress events of the OpenAI Responses API. Parity with
// ag2/config/openai/events.py (OpenAIShellCommandChunk, OpenAIShellOutputChunk).

package openai

import "github.com/luckystrker/ago/events"

// ShellCommandChunk is a slice of the shell command the model is composing in
// a streamed hosted shell call. Transient: the finished shell_call supersedes
// it, and it is kept out of events.ModelMessageChunk so a command never lands
// in the assistant's reply.
type ShellCommandChunk struct {
	events.Base
	// Content is the next piece of the command text.
	Content string `json:"content"`
	// CommandIndex numbers the command inside the shell call.
	CommandIndex int `json:"command_index"`
	// OutputIndex is the output item the call occupies in the response.
	OutputIndex int `json:"output_index"`
}

func (ShellCommandChunk) EventType() string    { return "OpenAIShellCommandChunk" }
func (ShellCommandChunk) Transient() bool      { return true }
func (ShellCommandChunk) Conversational() bool { return false }

// ShellOutputChunk is a slice of the output a container produced running one
// shell command. Transient. The two streams are independent: a delta may carry
// either, or both; an absent one is nil (not the empty string).
type ShellOutputChunk struct {
	events.Base
	CommandIndex int `json:"command_index"`
	OutputIndex  int `json:"output_index"`
	// ItemID names the shell_call_output item the slice belongs to.
	ItemID string  `json:"item_id"`
	Stdout *string `json:"stdout,omitempty"`
	Stderr *string `json:"stderr,omitempty"`
}

func (ShellOutputChunk) EventType() string    { return "OpenAIShellOutputChunk" }
func (ShellOutputChunk) Transient() bool      { return true }
func (ShellOutputChunk) Conversational() bool { return false }

func init() {
	events.RegisterEvent(ShellCommandChunk{})
	events.RegisterEvent(ShellOutputChunk{})
}
