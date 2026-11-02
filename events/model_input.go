package events

import "fmt"

// CopyInputs deep-copies input parts through their wire form: every part is
// marshaled and unmarshaled back through the event registry. For wire types
// this is the honest analogue of the Python deepcopy — the copy survives a
// storage round trip by construction, so a snapshot handed onward can never
// alias the caller's buffers. A part that cannot round trip (unregistered
// type, marshal failure) is an error, not a silent alias.
func CopyInputs(parts []Input) ([]Input, error) {
	out := make([]Input, len(parts))
	for i, p := range parts {
		if p == nil {
			return nil, fmt.Errorf("events: copy input: nil part at %d", i)
		}
		b, err := MarshalEvent(p)
		if err != nil {
			return nil, fmt.Errorf("events: copy input %s: %w", p.EventType(), err)
		}
		in, err := UnmarshalInput(b)
		if err != nil {
			return nil, fmt.Errorf("events: copy input %s: %w", p.EventType(), err)
		}
		out[i] = in
	}
	return out, nil
}

// StatusRequest builds the detached service view of a status snapshot: a
// ModelRequest over the already-copied parts, tagged OriginService. The view
// only ever rides the in-memory event list of one model call — it is never
// sent to a stream, so it never becomes durable.
//
// AGO-DEVIATION: upstream projects through a transient non-conversational
// _StatusRequest subclass; AGo reuses ModelRequest with Origin=service, which
// sizes and provider-maps like any request (docs/DECISIONS.md D-081).
func StatusRequest(parts []Input) *ModelRequest {
	return &ModelRequest{Parts: parts, Origin: OriginService}
}

// ObservationRequest projects a persisted StatusObservation into a detached
// service view for one model call: the parts are copied again, so the view is
// detached even from the published observation, and the request keeps the
// observation's CreatedAt as its provenance.
func ObservationRequest(ev *StatusObservation) (*ModelRequest, error) {
	parts, err := CopyInputs(ev.Parts)
	if err != nil {
		return nil, fmt.Errorf("events: project status observation: %w", err)
	}
	return &ModelRequest{Base: Base{CreatedAt: ev.At()}, Parts: parts, Origin: OriginService}, nil
}

// ModelInputs projects a history snapshot into the input of one model call:
// every StatusObservation becomes an ObservationRequest — a detached service
// view; the observation itself never reaches the provider. Everything else
// passes through unchanged. An observation whose parts cannot round trip is
// dropped: its wire form is already broken and forwarding it would poison the
// provider mapping, so the call proceeds without the stale status.
func ModelInputs(evs []Event) []Event {
	out := make([]Event, 0, len(evs))
	for _, ev := range evs {
		if obs := asStatusObservation(ev); obs != nil {
			req, err := ObservationRequest(obs)
			if err != nil {
				continue
			}
			out = append(out, req)
			continue
		}
		out = append(out, ev)
	}
	return out
}

// asStatusObservation extracts the observation behind an event in either form:
// streams and storage hand out pointers, values can arrive from unpacking.
func asStatusObservation(ev Event) *StatusObservation {
	switch o := ev.(type) {
	case *StatusObservation:
		return o
	case StatusObservation:
		return &o
	}
	return nil
}
