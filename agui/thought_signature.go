package agui

import (
	"encoding/base64"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/events"
)

// A Gemini tool call's thought signature as an AG-UI encrypted value, and back
// (Python parity: ag_ui/thought_signature.py).
//
// A Gemini 3 call must be replayed with the signature the model attached to it,
// or the next turn is rejected. The signature is opaque provider data, so AG-UI
// carries it as a REASONING_ENCRYPTED_VALUE event tied to the call id; the
// client stores it and puts it back on the assistant message's tool call as
// encryptedValue, which the next turn restores onto the call.
//
// AGo carries the signature on events.ToolCallEvent.VendorMetadata under
// ThoughtSignatureKey: the field is the framework's provider-specific slot, and
// the Gemini client fills it when the response part carries one.

// ThoughtSignatureKey is the VendorMetadata key a tool call's Gemini thought
// signature travels under: the raw bytes (a []byte, or the base64 string the
// wire uses).
const ThoughtSignatureKey = "thought_signature"

// EncryptedSignatureOf returns the call's thought signature as the base64 the
// AG-UI wire carries, and whether it has one (Python parity:
// encrypted_signature_of).
func EncryptedSignatureOf(call events.ToolCallEvent) (string, bool) {
	raw, ok := call.VendorMetadata[ThoughtSignatureKey]
	if !ok {
		return "", false
	}
	switch sig := raw.(type) {
	case []byte:
		if len(sig) == 0 {
			return "", false
		}
		return base64.StdEncoding.EncodeToString(sig), true
	case string:
		if sig == "" {
			return "", false
		}
		return sig, true
	}
	return "", false
}

// SignatureEvent is the encrypted value that carries a call's signature
// (Python parity: signature_event). It goes out after the call starts: a
// consumer may drop a value whose entity it has not seen yet.
func SignatureEvent(callID, signature string, timestampMS *int64) ReasoningEncryptedValueEvent {
	return ReasoningEncryptedValueEvent{
		baseEvent:      baseEvent{Type: EventReasoningEncryptedValue, Timestamp: timestampMS},
		Subtype:        EncryptedSubtypeToolCall,
		EntityID:       callID,
		EncryptedValue: signature,
	}
}

// RestoreToolCall rebuilds a replayed tool call, carrying its signature back
// where the provider needs one (Python parity: restore_tool_call). Only a
// Gemini-family run restores it: every other provider would reject a field it
// never issued, and an empty value means the client kept nothing.
func RestoreToolCall(provider config.Provider, id, name, arguments, encryptedValue string) events.ToolCallEvent {
	call := events.ToolCallEvent{ID: id, Name: name, Arguments: arguments}
	if encryptedValue == "" || !geminiFamily(provider) {
		return call
	}
	sig, err := base64.StdEncoding.DecodeString(encryptedValue)
	if err != nil || len(sig) == 0 {
		// A value the client mangled is not a signature: replay the call
		// without one rather than failing the run over provider-internal data.
		return call
	}
	call.VendorMetadata = map[string]any{ThoughtSignatureKey: sig}
	return call
}
