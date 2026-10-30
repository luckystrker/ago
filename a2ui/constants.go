// Package a2ui implements the v1.0 subset of the A2UI generative-UI protocol:
// the server→client message vocabulary, the response parser that extracts
// A2UI JSON from model text, schema validation of component trees against the
// vendored v1.0 basic catalog, clickable server actions, and an HTTP server
// that dispatches client→server envelopes to those actions or to a prompt
// callback.
//
// Only protocol version v1.0 is ported; v0.9/v0.9.1 and the A2A transport are
// deferred (D-076).
package a2ui

// VersionV1 is the only protocol version this package speaks. Every emitted
// message carries it; every parsed message must declare it.
const VersionV1 = "v1.0"

const (
	// MimeType is the A2UI media type (constants.A2UI_MIME_TYPE).
	MimeType = "application/a2ui+json"

	// JSONOpenTag / JSONCloseTag are the official A2UI standard prompt tags:
	// an LLM wraps its A2UI output between them for deterministic parsing.
	JSONOpenTag  = "<a2ui-json>"
	JSONCloseTag = "</a2ui-json>"
)

// DefaultCatalogID is the canonical "basic" catalog id for v1.0. It equals the
// $id of the vendored catalog schema, which the validator's $refs are built
// against ("catalog.json" resolves to the same document).
const DefaultCatalogID = "https://a2ui.org/specification/v1_0/catalogs/basic/catalog.json"

// payloadKeys lists the server→client message payload keys, sorted (the same
// order the Python parser reports in its not-a-message error).
var payloadKeys = [...]string{
	"agentFunctionResponse",
	"callRendererFunction",
	"createSurface",
	"deleteSurface",
	"updateComponents",
	"updateDataModel",
}

func isPayloadKey(key string) bool {
	for _, k := range payloadKeys {
		if k == key {
			return true
		}
	}
	return false
}
