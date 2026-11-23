package acp

import "strings"

// PermissionOption is one choice the agent offers in a session/request_permission.
// Name, not Kind, says what an *_always option covers ("Allow for this session").
type PermissionOption struct {
	OptionID string
	Name     string
	// Kind is allow_once, allow_always, reject_once or reject_always.
	Kind string
}

// PermissionRequest is one session/request_permission as the agent sent it: the
// tool call it is for and the options, in the agent's order.
type PermissionRequest struct {
	ToolCallID string
	Title      string
	// Kind is the tool call's ACP kind ("execute", "edit", ...); empty if the
	// agent sent none.
	Kind      string
	Locations []ToolLocation
	// RawInput is the tool call's raw input as the agent sent it.
	RawInput string
	Options  []PermissionOption
}

// PermissionReply is the human's answer to a PermissionRequest. Choice false:
// Text is words, read against the approval words only and never as an option id
// (yes/y/1 allow once, always allows remembered, anything else refuses). Choice
// true: Text is the OptionID of the option picked, matched against the offered
// ids only and never as words. The zero reply refuses.
type PermissionReply struct {
	Text   string
	Choice bool
}

// The approval words a plain answer is read against.
var (
	onceWords   = map[string]bool{"y": true, "yes": true, "1": true, "ok": true, "allow": true, "approve": true, "allow_once": true}
	alwaysWords = map[string]bool{"always": true, "allow_always": true}
)

// resolvePermission turns the reply into the decision on the wire. The agent's
// option order never decides between a one-time and a remembered option, and a
// yes is never widened to allow_always: with no one-time option on offer a yes
// refuses. No fitting option cancels.
//
// AGO-DEVIATION: Python's auto policy falls back from allow_once to allow_always;
// a boolean yes here never does (see providerSession.permission). A person who
// wants the remembered option says so: the word always, or a pick.
// docs/DECISIONS.md D-132.
func resolvePermission(options []permissionOption, reply PermissionReply) Decision {
	if reply.Choice {
		for _, o := range options {
			if o.OptionID == reply.Text {
				return selected(o)
			}
		}
		return firstOfKind(options, kindRejectOnce, kindRejectAlways)
	}
	word := strings.ToLower(strings.TrimSpace(reply.Text))
	switch {
	case alwaysWords[word]:
		// Nothing remembered on offer: this call only, less than asked and never more.
		return firstOfKind(options, kindAllowAlways, kindAllowOnce, kindRejectOnce, kindRejectAlways)
	case onceWords[word]:
		return firstOfKind(options, kindAllowOnce, kindRejectOnce, kindRejectAlways)
	}
	return firstOfKind(options, kindRejectOnce, kindRejectAlways)
}

func selected(o permissionOption) Decision {
	return Decision{Outcome: OutcomeSelected, OptionID: o.OptionID, Kind: o.Kind}
}

// firstOfKind selects the first offered option of the earliest kind listed:
// kinds is a preference order, and the agent's own order only breaks ties
// within one kind.
func firstOfKind(options []permissionOption, kinds ...string) Decision {
	for _, kind := range kinds {
		for _, o := range options {
			if o.Kind == kind {
				return selected(o)
			}
		}
	}
	return Decision{Outcome: OutcomeCancelled}
}

func permissionRequestOf(p requestPermissionParams) PermissionRequest {
	req := PermissionRequest{
		ToolCallID: p.ToolCall.ToolCallID,
		Title:      p.ToolCall.Title,
		Kind:       p.ToolCall.Kind,
		Locations:  p.ToolCall.Locations,
		RawInput:   p.ToolCall.inputString(),
	}
	for _, o := range p.Options {
		req.Options = append(req.Options, PermissionOption{OptionID: o.OptionID, Name: o.Name, Kind: o.Kind})
	}
	return req
}
