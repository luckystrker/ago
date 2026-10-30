package a2a

// Context-variable sync over the wire (parity: ag2/ag2/a2a/mappers/messages.py
// extract_context_update / _build_message + executor.py's
// merged_variables / _build_final_message). Deltas of the conversation's
// context variables piggy-back on Message.metadata under one key: the client
// puts one on the opening turn, the server merges it into its variables and
// returns the final set on the reply, the client merges that back locally.
//
// The server engages the sync only when ServerConfig.SyncVariables is set —
// the flag the ask's "по флагам конфига" maps onto; a server without the
// callback ignores inbound context_update metadata entirely.
const (
	// ContextUpdateKey is the Message.metadata key carrying context.variables
	// deltas (parity: extension.py CONTEXT_UPDATE_METADATA_KEY).
	ContextUpdateKey = "ag2.context_update"
)

// ReservedVariablePrefixes are the framework's own control-plane prefixes,
// stripped from every variables payload crossing the wire (parity:
// ag2/ag2/context.py:289 RESERVED_VARIABLE_PREFIXES).
//
// AGO-DEVIATION: declared locally instead of reusing ago/context — the Go
// core does not export the strip helper yet, and keeping a2a a
// transport-level leaf package outweighs the one-string duplication (D-056).
var ReservedVariablePrefixes = []string{"ag:", "a2a:"}

// ExtractContextUpdate returns the context.variables delta carried on msg's
// metadata; an empty map when absent or not an object — callers can merge the
// result unconditionally (parity: mappers/messages.py extract_context_update).
func ExtractContextUpdate(msg *Message) map[string]any {
	if msg == nil || len(msg.Metadata) == 0 {
		return map[string]any{}
	}
	payload, ok := msg.Metadata[ContextUpdateKey].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return payload
}

// WithContextUpdate attaches vars as msg's context.variables delta,
// overwriting any previous delta on the message (parity: _build_message —
// the AG2-namespaced key wins over caller metadata).
func WithContextUpdate(msg *Message, vars map[string]any) *Message {
	if msg == nil {
		msg = &Message{}
	}
	if msg.Metadata == nil {
		msg.Metadata = map[string]any{}
	}
	msg.Metadata[ContextUpdateKey] = vars
	return msg
}

// StripReservedVariables returns payload without the reserved prefixes, so
// control-plane state stays locally authored on both wire directions
// (parity: ag2/ag2/context.py strip_reserved_variables — transports strip on
// every payload arriving from, or leaving for, a remote peer).
func StripReservedVariables(payload map[string]any) map[string]any {
	out := make(map[string]any, len(payload))
	for k, v := range payload {
		reserved := false
		for _, p := range ReservedVariablePrefixes {
			if len(k) >= len(p) && k[:len(p)] == p {
				reserved = true
				break
			}
		}
		if !reserved {
			out[k] = v
		}
	}
	return out
}
