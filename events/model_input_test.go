package events

import (
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"
)

// quickCopyParts generates text/data part batches whose payloads are plain wire
// values, so CopyInputs' JSON round trip must reproduce them exactly.
type quickCopyParts struct{ parts []Input }

func (quickCopyParts) Generate(r *rand.Rand, size int) reflect.Value {
	var c quickCopyParts
	if n := r.Intn(5); n > 0 {
		c.parts = make([]Input, n)
	}
	for i := range c.parts {
		switch r.Intn(3) {
		case 0:
			c.parts[i] = &TextInput{Content: string(rune('a' + r.Intn(5)))}
		case 1:
			c.parts[i] = &DataInput{Data: map[string]any{"n": r.Intn(100)}}
		default:
			c.parts[i] = &UrlInput{URL: "https://example.com/x", Kind: BinaryImage}
		}
	}
	return reflect.ValueOf(c)
}

// TestQuickCopyInputsWireFaithful sweeps random part batches: the copy must be
// wire-identical to the source and never alias it.
func TestQuickCopyInputsWireFaithful(t *testing.T) {
	prop := func(c quickCopyParts) bool {
		cp, err := CopyInputs(c.parts)
		if err != nil {
			return false
		}
		if len(cp) != len(c.parts) {
			return false
		}
		for i := range cp {
			if cp[i] == c.parts[i] {
				return false // alias, not a copy
			}
			a, errA := MarshalEvent(c.parts[i])
			b, errB := MarshalEvent(cp[i])
			if errA != nil || errB != nil || string(a) != string(b) {
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, nil); err != nil {
		t.Fatalf("CopyInputs wire fidelity: %v", err)
	}
}

func TestCopyInputsDetachesSources(t *testing.T) {
	src := []Input{
		&TextInput{Content: "hello"},
		&DataInput{Data: map[string]any{"k": "v"}},
		&BinaryInput{Data: []byte{1, 2}, MediaType: "image/png", Kind: BinaryImage},
	}
	cp, err := CopyInputs(src)
	if err != nil {
		t.Fatalf("CopyInputs: %v", err)
	}
	// Mutating the sources after the copy must not reach it.
	src[0].(*TextInput).Content = "mutated"
	src[1].(*DataInput).Data.(map[string]any)["k"] = "rewritten"
	src[2].(*BinaryInput).Data[0] = 9

	if cp[0].(*TextInput).Content != "hello" {
		t.Fatalf("copy saw the text mutation: %q", cp[0].(*TextInput).Content)
	}
	if got := cp[1].(*DataInput).Data.(map[string]any)["k"]; got != "v" {
		t.Fatalf("copy saw the data mutation: %v", got)
	}
	if cp[2].(*BinaryInput).Data[0] != 1 {
		t.Fatalf("copy saw the bytes mutation: %v", cp[2].(*BinaryInput).Data)
	}

	if _, err := CopyInputs(nil); err != nil {
		t.Fatalf("CopyInputs(nil): %v", err)
	}
	if _, err := CopyInputs([]Input{nil}); err == nil {
		t.Fatal("nil part accepted, want error")
	}
}

func TestStatusRequestView(t *testing.T) {
	parts := []Input{&TextInput{Content: "status"}}
	view := StatusRequest(parts)
	if view.Origin != OriginService {
		t.Fatalf("view origin = %q, want %q", view.Origin, OriginService)
	}
	if len(view.Parts) != 1 || view.Parts[0] != parts[0] {
		t.Fatal("view must wrap the given parts without copying")
	}
	// On the wire it is a plain request: provider mappers need no new case.
	if view.EventType() != "ModelRequest" {
		t.Fatalf("view type = %q, want ModelRequest", view.EventType())
	}
}

func TestObservationRequestProjection(t *testing.T) {
	at := time.Now()
	obs := &StatusObservation{Base: Base{CreatedAt: at}, Parts: []Input{&TextInput{Content: "s"}}, Source: "cb"}
	req, err := ObservationRequest(obs)
	if err != nil {
		t.Fatalf("ObservationRequest: %v", err)
	}
	if !req.At().Equal(at) {
		t.Fatalf("view created_at = %v, want the observation's %v", req.At(), at)
	}
	if req.Origin != OriginService {
		t.Fatalf("view origin = %q, want %q", req.Origin, OriginService)
	}
	// Mutating the observation after projection leaves the detached view intact.
	obs.Parts[0].(*TextInput).Content = "changed"
	if got := req.Parts[0].(*TextInput).Content; got != "s" {
		t.Fatalf("view saw the observation mutation: %q", got)
	}
}

func TestModelInputsProjectsObservations(t *testing.T) {
	obs := &StatusObservation{Parts: []Input{&TextInput{Content: "mode: x"}}, Source: "cb"}
	req := &ModelRequest{Parts: []Input{&TextInput{Content: "q"}}, Origin: OriginUser}
	resp := &ModelResponse{Message: &ModelMessage{Content: "a"}}
	out := ModelInputs([]Event{req, resp, obs})
	if len(out) != 3 {
		t.Fatalf("projection has %d events, want 3", len(out))
	}
	if out[0] != req || out[1] != resp {
		t.Fatal("non-observational events must pass through unchanged")
	}
	view, ok := out[2].(*ModelRequest)
	if !ok {
		t.Fatalf("projected event = %T, want *ModelRequest", out[2])
	}
	if view.Origin != OriginService || len(view.Parts) != 1 {
		t.Fatalf("projected view = %+v, want a service request over one part", view)
	}
	for i, ev := range out {
		if _, ok := AsValue(ev).(StatusObservation); ok {
			t.Fatalf("event %d: the observation itself reached the model input", i)
		}
	}
	// Value-form observations project the same way.
	out = ModelInputs([]Event{*obs})
	if v, ok := AsValue(out[0]).(ModelRequest); !ok || v.Origin != OriginService {
		t.Fatalf("value-form projection = %T (%+v), want a service ModelRequest", out[0], out[0])
	}
}
