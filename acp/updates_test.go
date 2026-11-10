package acp

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	"github.com/luckystrker/ago/events"
)

// runServeModeAgent is the serving half of the mode/commands round trip: a real
// Serve whose handler tells the client the session switched mode and which
// slash commands exist, then answers.
func runServeModeAgent() {
	_ = Serve(ServeConfig{
		AgentName: "mode-agent",
		Handle: func(_ context.Context, _ string, _ string, emit *Emitter) (string, error) {
			emit.AvailableCommands([]AvailableCommand{
				{Name: "plan", Description: "Make a plan"},
				{Name: "review", Description: "Review the diff", InputHint: "path to review"},
			})
			emit.Mode("code")
			return "done", nil
		},
	})
}

// A Bridge receives the serving agent's mode and available-commands updates
// typed, and EventOf turns them into the events a stream carries.
func TestBridgeReceivesModeAndCommandsUpdates(t *testing.T) {
	var (
		mu      sync.Mutex
		updates []Update
	)
	b := newRoleBridge(t, roleServeMode, func(br *Bridge) {
		br.OnUpdate = func(u Update) {
			mu.Lock()
			updates = append(updates, u)
			mu.Unlock()
		}
	})
	id, err := b.SessionNew(t.TempDir())
	if err != nil {
		t.Fatalf("SessionNew: %v", err)
	}
	if _, err := b.Prompt(id, "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	var cmds, mode *Update
	for i := range updates {
		switch updates[i].Kind {
		case "available_commands_update":
			cmds = &updates[i]
		case "current_mode_update":
			mode = &updates[i]
		}
	}
	if cmds == nil || mode == nil {
		t.Fatalf("updates = %+v, want a commands and a mode update", updates)
	}
	wantCmds := []AvailableCommand{
		{Name: "plan", Description: "Make a plan"},
		{Name: "review", Description: "Review the diff", InputHint: "path to review"},
	}
	if !reflect.DeepEqual(cmds.Commands, wantCmds) {
		t.Errorf("commands = %+v, want %+v", cmds.Commands, wantCmds)
	}
	if mode.ModeID != "code" {
		t.Errorf("mode id = %q, want code", mode.ModeID)
	}

	ev, ok := EventOf(*mode)
	if mc, isMC := ev.(*ModeChange); !ok || !isMC || mc.ModeID != "code" {
		t.Errorf("EventOf(mode) = %#v, %v", ev, ok)
	}
	ev, ok = EventOf(*cmds)
	if ac, isAC := ev.(*AvailableCommandsEvent); !ok || !isAC || !reflect.DeepEqual(ac.Commands, []string{"plan", "review"}) {
		t.Errorf("EventOf(commands) = %#v, %v", ev, ok)
	}
}

// The wire shapes are the SDK's: currentModeId, and availableCommands with the
// nested input hint.
func TestModeAndCommandsWireShape(t *testing.T) {
	raw, err := json.Marshal(Update{Kind: "available_commands_update", Commands: []AvailableCommand{
		{Name: "a", Description: "A"}, {Name: "b", Description: "B", InputHint: "arg"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	const wantCmds = `{"availableCommands":[{"description":"A","name":"a"},{"description":"B","input":{"hint":"arg"},"name":"b"}],"sessionUpdate":"available_commands_update"}`
	if string(raw) != wantCmds {
		t.Errorf("commands wire = %s\nwant          %s", raw, wantCmds)
	}
	raw, _ = json.Marshal(Update{Kind: "current_mode_update", ModeID: "ask"})
	if string(raw) != `{"currentModeId":"ask","sessionUpdate":"current_mode_update"}` {
		t.Errorf("mode wire = %s", raw)
	}
	// An empty list clears the client's commands: it must stay an array, not null.
	raw, _ = json.Marshal(Update{Kind: "available_commands_update"})
	if string(raw) != `{"availableCommands":[],"sessionUpdate":"available_commands_update"}` {
		t.Errorf("empty commands wire = %s", raw)
	}

	var u Update
	if err := json.Unmarshal([]byte(wantCmds), &u); err != nil {
		t.Fatal(err)
	}
	if len(u.Commands) != 2 || u.Commands[1].InputHint != "arg" || u.Commands[0].InputHint != "" {
		t.Errorf("decoded commands = %+v", u.Commands)
	}
}

// The events survive the events package's wire round trip, and other update
// kinds have no typed event.
func TestUpdateEventsMarshalAndOtherKindsHaveNone(t *testing.T) {
	ev, _ := EventOf(Update{Kind: "current_mode_update", ModeID: "code"})
	raw, err := events.MarshalEvent(ev)
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	back, err := events.UnmarshalEvent(raw)
	if err != nil {
		t.Fatalf("UnmarshalEvent: %v", err)
	}
	if mc, ok := back.(*ModeChange); !ok || mc.ModeID != "code" {
		t.Errorf("round trip = %#v", back)
	}
	for _, kind := range []string{"agent_message_chunk", "plan", "usage_update", "tool_call", "brand_new_kind"} {
		if ev, ok := EventOf(Update{Kind: kind}); ok || ev != nil {
			t.Errorf("EventOf(%s) = %#v, %v, want none", kind, ev, ok)
		}
	}
}
