package network

import (
	"encoding/json"
	"strings"

	"ago/events"
)

// Handoff is the routing intent of a tool that picks its target at runtime
// (load balancing, state-driven dispatch). Target is the participant's
// Passport name — the client layer resolves name → id before the packet is
// posted. The workflow adapter reads the encoded routing and makes target
// the next speaker.
type Handoff struct {
	Target string `json:"target"`
	Reason string `json:"reason,omitempty"`
}

// Finish is the routing intent of a tool that ends the channel cleanly —
// the tool-driven equivalent of a TerminateTarget rule.
type Finish struct {
	Summary string `json:"summary,omitempty"`
	Reason  string `json:"reason,omitempty"` // default "finished" when encoded
}

// Round is one agent turn's outcome as the client layer encodes it into a
// round envelope: the reply text plus optional tool-driven routing intent
// (a Finish terminates the channel, a Handoff redirects it).
type Round struct {
	Text    string   `json:"text,omitempty"`
	Handoff *Handoff `json:"handoff,omitempty"`
	Finish  *Finish  `json:"finish,omitempty"`
}

// Empty reports whether the round carries nothing worth posting.
func (r Round) Empty() bool { return r.Text == "" && r.Handoff == nil && r.Finish == nil }

// handoffWire / finishWire probe JSON tool results whose typed value was
// lost in transit: a part that decodes to an object with exactly one of the
// marker keys counts as the routing intent.
type handoffWire struct {
	Handoff *Handoff `json:"handoff"`
	Finish  *Finish  `json:"finish"`
}

// HandoffFromResult scans a tool result for a Handoff value: either a
// DataInput part carrying the typed struct (a live Go tool returned it) or
// JSON content encoding {"handoff": {...}} (a result that crossed a wire).
// Returns nil when no handoff intent is present.
func HandoffFromResult(res events.ToolResult) *Handoff {
	for _, part := range res.Parts {
		if h, ok := typedIntent(part); ok {
			if h != nil {
				return h
			}
		}
	}
	return nil
}

// FinishFromResult scans a tool result for a Finish value (see
// HandoffFromResult for the recognition rules). Returns nil when absent.
func FinishFromResult(res events.ToolResult) *Finish {
	for _, part := range res.Parts {
		if f, ok := typedFinish(part); ok {
			if f != nil {
				return f
			}
		}
	}
	return nil
}

// RoundFromEvents builds a routing round from the agent's local-stream tool
// results: first Finish wins over first Handoff (termination is the most
// explicit intent), otherwise the round is plain text.
func RoundFromEvents(text string, evs []events.Event) Round {
	round := Round{Text: text}
	var handoff *Handoff
	for _, ev := range evs {
		var results []events.ToolResultEvent
		switch e := ev.(type) {
		case events.ToolResultsEvent:
			results = e.Results
		case events.ToolResultEvent:
			results = []events.ToolResultEvent{e}
		default:
			continue
		}
		for _, r := range results {
			if round.Finish == nil {
				if f := FinishFromResult(r.Result); f != nil {
					round.Finish = f
					continue
				}
			}
			if handoff == nil {
				handoff = HandoffFromResult(r.Result)
			}
		}
	}
	round.Handoff = handoff
	return round
}

// typedIntent extracts a *Handoff from one input part.
func typedIntent(part events.Input) (*Handoff, bool) {
	di, ok := derefData(part)
	if !ok {
		// JSON text parts count too: a tool result serialized as text.
		if c, isText := derefText(part); isText && strings.Contains(c, "\"handoff\"") {
			if h := decodeJSONIntent[Handoff]([]byte(c), "handoff"); h != nil {
				return h, true
			}
		}
		return nil, false
	}
	switch v := di.Data.(type) {
	case *Handoff:
		return v, true
	case Handoff:
		return &v, true
	case map[string]any:
		if h := decodeJSONIntent[Handoff](marshalAny(v), "handoff"); h != nil {
			return h, true
		}
	case string:
		if h := decodeJSONIntent[Handoff]([]byte(v), "handoff"); h != nil {
			return h, true
		}
	}
	return nil, false
}

// typedFinish extracts a *Finish from one input part.
func typedFinish(part events.Input) (*Finish, bool) {
	di, ok := derefData(part)
	if !ok {
		if c, isText := derefText(part); isText && strings.Contains(c, "\"finish\"") {
			if f := decodeJSONIntent[Finish]([]byte(c), "finish"); f != nil {
				return f, true
			}
		}
		return nil, false
	}
	switch v := di.Data.(type) {
	case *Finish:
		return v, true
	case Finish:
		return &v, true
	case map[string]any:
		if f := decodeJSONIntent[Finish](marshalAny(v), "finish"); f != nil {
			return f, true
		}
	case string:
		if f := decodeJSONIntent[Finish]([]byte(v), "finish"); f != nil {
			return f, true
		}
	}
	return nil, false
}

// derefData unwraps an Input part into its Data payload (EnsureInput hands
// out *DataInput pointers).
func derefData(part events.Input) (events.DataInput, bool) {
	switch p := part.(type) {
	case *events.DataInput:
		return *p, true
	case events.DataInput:
		return p, true
	}
	return events.DataInput{}, false
}

// derefText unwraps an Input part into text content (*TextInput pointers).
func derefText(part events.Input) (string, bool) {
	switch p := part.(type) {
	case *events.TextInput:
		return p.Content, true
	case events.TextInput:
		return p.Content, true
	}
	return "", false
}

func marshalAny(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return data
}

// decodeJSONIntent decodes data as {"<key>": {...}} and returns the typed
// intent, or nil when the shape does not match.
func decodeJSONIntent[T any](data []byte, key string) *T {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &probe); err != nil {
		return nil
	}
	raw, ok := probe[key]
	if !ok {
		return nil
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return &out
}
