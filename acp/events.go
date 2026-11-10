package acp

import "github.com/luckystrker/ago/events"

// ModeChange is the event a peer's current_mode_update becomes: the session's
// mode changed (Python parity: ACPModeChange).
type ModeChange struct {
	events.Base
	ModeID string `json:"mode_id"`
}

func (ModeChange) EventType() string    { return "ACPModeChange" }
func (ModeChange) Transient() bool      { return false }
func (ModeChange) Conversational() bool { return false }

// AvailableCommandsEvent is the event a peer's available_commands_update
// becomes: the names of the slash commands the agent now advertises (Python
// parity: ACPAvailableCommands, which carries names only).
type AvailableCommandsEvent struct {
	events.Base
	Commands []string `json:"commands"`
}

func (AvailableCommandsEvent) EventType() string    { return "ACPAvailableCommands" }
func (AvailableCommandsEvent) Transient() bool      { return false }
func (AvailableCommandsEvent) Conversational() bool { return false }

func init() {
	events.RegisterEvent(ModeChange{})
	events.RegisterEvent(AvailableCommandsEvent{})
}

// EventOf maps a session/update a Bridge received onto the typed event that
// carries it, for the update kinds that have one: current_mode_update →
// *ModeChange, available_commands_update → *AvailableCommandsEvent (Python
// parity: mappers.map_session_update). Every other kind reports false; text,
// tool-call, plan and usage updates keep reaching the caller through
// Bridge.OnUpdate as Updates.
func EventOf(u Update) (events.Event, bool) {
	switch u.Kind {
	case "current_mode_update":
		return &ModeChange{ModeID: u.ModeID}, true
	case "available_commands_update":
		names := make([]string, 0, len(u.Commands))
		for _, c := range u.Commands {
			names = append(names, c.Name)
		}
		return &AvailableCommandsEvent{Commands: names}, true
	}
	return nil, false
}
