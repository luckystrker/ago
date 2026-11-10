package network

import "fmt"

// SetContext sets one workflow context variable by posting an EnvContextSet
// envelope from senderID. Loose semantics: any participant may call it
// regardless of turn order; every participant sees the variable after the
// fold. The channel must be a workflow channel — other adapter types have no
// context variables, so the call fails early with ErrProtocol instead of
// posting an envelope the adapter would reject or ignore.
func SetContext(h *Hub, channelID, senderID, key string, value any) error {
	if err := requireWorkflowChannel(h, channelID, "SetContext"); err != nil {
		return err
	}
	_, err := h.Send(NewContextSetEnvelope(channelID, senderID, map[string]any{key: value}))
	return err
}

// DeleteContext removes one workflow context variable (a no-op when unset),
// under the same guard as SetContext.
func DeleteContext(h *Hub, channelID, senderID, key string) error {
	if err := requireWorkflowChannel(h, channelID, "DeleteContext"); err != nil {
		return err
	}
	_, err := h.Send(NewContextSetEnvelope(channelID, senderID, nil, key))
	return err
}

// SetContext sets one workflow context variable from this agent; see the
// package-level SetContext.
func (a *AgentClient) SetContext(channelID, key string, value any) error {
	return SetContext(a.hub, channelID, a.AgentID(), key, value)
}

// DeleteContext removes one workflow context variable from this agent; see
// the package-level DeleteContext.
func (a *AgentClient) DeleteContext(channelID, key string) error {
	return DeleteContext(a.hub, channelID, a.AgentID(), key)
}

// requireWorkflowChannel is the type guard shared by the context helpers.
func requireWorkflowChannel(h *Hub, channelID, op string) error {
	ch, err := h.Channel(channelID)
	if err != nil {
		return err
	}
	if ch.Manifest.Type != TypeWorkflow {
		return fmt.Errorf("%w: %s requires a workflow channel (manifest type %q); got %q",
			ErrProtocol, op, TypeWorkflow, ch.Manifest.Type)
	}
	return nil
}
