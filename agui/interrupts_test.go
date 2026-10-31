package agui

import (
	"errors"
	"testing"
)

// stumpedMetadata is the envelope a failed issueProof leaves behind: the proof
// key is there, but the value never became a secret.
func stumpedMetadata() map[string]any {
	m := withProof(nil)
	m[metadataKey].(map[string]any)[proofKey] = ""
	return m
}

// A required proof must fail closed when the interrupt was issued no proof at
// all — an entropy failure at issue time must cost the resume, not the check
// (security audit L-16).
func TestCheckProofRequiredFailsClosedWithoutIssuedProof(t *testing.T) {
	plain := ResumeEntry{InterruptID: "i1", Status: "resolved"}
	copied := ResumeEntry{InterruptID: "i1", Status: "resolved", Metadata: stumpedMetadata()}
	interrupt := Interrupt{ID: "i1", Metadata: stumpedMetadata()}

	if err := checkProof(plain, interrupt, true); !errors.Is(err, errNotProven) {
		t.Fatalf("a proof-less resume: err = %v, want errNotProven", err)
	}
	// Repeating the empty envelope word for word proves nothing either: there
	// was no secret to present.
	if err := checkProof(copied, interrupt, true); !errors.Is(err, errNotProven) {
		t.Fatalf("a resume copying the empty proof: err = %v, want errNotProven", err)
	}
	// Neither side carrying an envelope at all is the same refusal.
	if err := checkProof(plain, Interrupt{ID: "i1"}, true); !errors.Is(err, errNotProven) {
		t.Fatalf("no envelopes: err = %v, want errNotProven", err)
	}

	// Without required, a proof-less resume is still served: a standard client
	// copies nothing from an interrupt's metadata.
	if err := checkProof(plain, interrupt, false); err != nil {
		t.Fatalf("a proof-less resume, not required: err = %v, want nil", err)
	}
}

// A required proof passes exactly when the resume carries the proof the
// interrupt was issued with.
func TestCheckProofRequiredAcceptsIssuedProof(t *testing.T) {
	issued := withProof(nil)
	proof, ok := proofIn(issued)
	if !ok || proof == "" {
		t.Fatalf("issued metadata = %+v, want a readable proof", issued)
	}
	interrupt := Interrupt{ID: "i1", Metadata: issued}

	if err := checkProof(ResumeEntry{InterruptID: "i1", Status: "resolved", Metadata: issued}, interrupt, true); err != nil {
		t.Fatalf("the issued proof: err = %v, want nil", err)
	}
	forged := map[string]any{metadataKey: map[string]any{proofKey: "forged"}}
	if err := checkProof(ResumeEntry{InterruptID: "i1", Status: "resolved", Metadata: forged}, interrupt, true); !errors.Is(err, errNotProven) {
		t.Fatalf("a forged proof: err = %v, want errNotProven", err)
	}
}
