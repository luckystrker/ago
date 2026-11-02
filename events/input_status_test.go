package events

import (
	"encoding/json"
	"testing"
	"time"
)

// Error-path pins for StatusObservation's JSON codec: every guard in
// MarshalJSON/UnmarshalJSON must propagate, and the success path must populate
// the fields before returning.

func TestStatusObservationMarshalError(t *testing.T) {
	obs := StatusObservation{Parts: []Input{&DataInput{Data: make(chan int)}}, Source: "cb"}
	if _, err := json.Marshal(obs); err == nil {
		t.Fatal("MarshalJSON: want an error for an unmarshalable part, got nil")
	}
}

func TestStatusObservationUnmarshalErrors(t *testing.T) {
	var obs StatusObservation
	if err := json.Unmarshal([]byte("{broken"), &obs); err == nil {
		t.Error("UnmarshalJSON: want an error for malformed JSON, got nil")
	}
	// A part with an unregistered @type must fail the whole observation.
	in := `{"@type":"StatusObservation","parts":[{"@type":"NoSuchPart"}],"source":"cb"}`
	if err := json.Unmarshal([]byte(in), &obs); err == nil {
		t.Error("UnmarshalJSON: want an error for an unknown part type, got nil")
	}
}

func TestStatusObservationUnmarshalPopulates(t *testing.T) {
	at := time.Now()
	obs := StatusObservation{Base: Base{CreatedAt: at}, Parts: []Input{&TextInput{Content: "s"}}, Source: "cb"}
	data, err := json.Marshal(obs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out StatusObservation
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Source != "cb" {
		t.Errorf("source = %q, want %q", out.Source, "cb")
	}
	if !out.At().Equal(at) {
		t.Errorf("created_at = %v, want %v", out.At(), at)
	}
	if len(out.Parts) != 1 {
		t.Fatalf("parts: got %d, want 1", len(out.Parts))
	}
	if got, ok := out.Parts[0].(*TextInput); !ok || got.Content != "s" {
		t.Fatalf("parts[0] = %#v, want TextInput{s}", out.Parts[0])
	}
}
