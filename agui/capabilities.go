package agui

import (
	"github.com/luckystrker/ago/agent"
)

// The capability declarations a GET answers with (Python parity: capabilities.py
// against ag_ui.core.AgentCapabilities). Flags are pointers so an undeclared
// capability stays absent instead of reading as a declared false — the protocol
// reads an omitted field as saying nothing.

// AgentCapabilities is a typed snapshot of what this server tells a client it
// can do, before any run starts.
type AgentCapabilities struct {
	Identity       *IdentityCapabilities       `json:"identity,omitempty"`
	Transport      *TransportCapabilities      `json:"transport,omitempty"`
	Tools          *ToolsCapabilities          `json:"tools,omitempty"`
	State          *StateCapabilities          `json:"state,omitempty"`
	MultiAgent     *MultiAgentCapabilities     `json:"multiAgent,omitempty"`
	Reasoning      *ReasoningCapabilities      `json:"reasoning,omitempty"`
	Multimodal     *MultimodalCapabilities     `json:"multimodal,omitempty"`
	HumanInTheLoop *HumanInTheLoopCapabilities `json:"humanInTheLoop,omitempty"`
}

// IdentityCapabilities names the agent.
type IdentityCapabilities struct {
	Name        string `json:"name,omitempty"`
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
	Version     string `json:"version,omitempty"`
}

// TransportCapabilities declares which transports the server supports.
type TransportCapabilities struct {
	Streaming         *bool `json:"streaming,omitempty"`
	Websocket         *bool `json:"websocket,omitempty"`
	HTTPBinary        *bool `json:"httpBinary,omitempty"`
	PushNotifications *bool `json:"pushNotifications,omitempty"`
	Resumable         *bool `json:"resumable,omitempty"`
}

// ToolsCapabilities declares tool-calling support.
type ToolsCapabilities struct {
	Supported      *bool          `json:"supported,omitempty"`
	ParallelCalls  *bool          `json:"parallelCalls,omitempty"`
	ClientProvided *bool          `json:"clientProvided,omitempty"`
	Subagents      []SubagentInfo `json:"subagents,omitempty"`
}

// StateCapabilities declares state management.
type StateCapabilities struct {
	Snapshots       *bool `json:"snapshots,omitempty"`
	Deltas          *bool `json:"deltas,omitempty"`
	Memory          *bool `json:"memory,omitempty"`
	PersistentState *bool `json:"persistentState,omitempty"`
}

// MultiAgentCapabilities declares coordination support.
type MultiAgentCapabilities struct {
	Supported  *bool          `json:"supported,omitempty"`
	Delegation *bool          `json:"delegation,omitempty"`
	Handoffs   *bool          `json:"handoffs,omitempty"`
	Subagents  []SubagentInfo `json:"subagents,omitempty"`
}

// ReasoningCapabilities declares reasoning support.
type ReasoningCapabilities struct {
	Supported *bool `json:"supported,omitempty"`
	Streaming *bool `json:"streaming,omitempty"`
	Encrypted *bool `json:"encrypted,omitempty"`
}

// MultimodalCapabilities declares input and output modalities.
type MultimodalCapabilities struct {
	Input *MultimodalInputCapabilities `json:"input,omitempty"`
}

type MultimodalInputCapabilities struct {
	Image *bool `json:"image,omitempty"`
	Audio *bool `json:"audio,omitempty"`
	Video *bool `json:"video,omitempty"`
	PDF   *bool `json:"pdf,omitempty"`
	File  *bool `json:"file,omitempty"`
}

// HumanInTheLoopCapabilities declares human-in-the-loop support.
type HumanInTheLoopCapabilities struct {
	Supported     *bool `json:"supported,omitempty"`
	Approvals     *bool `json:"approvals,omitempty"`
	Interventions *bool `json:"interventions,omitempty"`
	Feedback      *bool `json:"feedback,omitempty"`
	Interrupts    *bool `json:"interrupts,omitempty"`
}

// SubagentInfo describes a subagent a client can pick in a selection UI.
type SubagentInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

func boolPtr(v bool) *bool { return &v }

// CapabilitiesFor reads what an agent tells an AG-UI client it can do, off the
// agent alone (Python parity: capabilities.py served_capabilities).
//
// AGO-DEVIATION: Python declares several sections off agent internals AGo does
// not expose (the model config's provider and input modalities, the tasks
// config, the installed human-input hook), so their sections are omitted rather
// than declared wrong: no state (no snapshots are emitted — the served run's
// variables are not readable from outside the agent), no multi_agent, no
// reasoning.encrypted, no multimodal, and humanInTheLoop without interrupts.
// A Handler may supply richer capabilities of its own. Identity.Type says "ago"
// where Python says "ag2".
func CapabilitiesFor(a *agent.Agent) AgentCapabilities {
	return AgentCapabilities{
		Identity:       &IdentityCapabilities{Name: a.Name(), Type: "ago"},
		Transport:      &TransportCapabilities{Streaming: boolPtr(true)},
		Tools:          &ToolsCapabilities{Supported: boolPtr(true)},
		HumanInTheLoop: &HumanInTheLoopCapabilities{Supported: boolPtr(true)},
	}
}
