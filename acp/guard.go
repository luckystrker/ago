package acp

import (
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
)

// The _meta merge guard (Python parity: acp/guard.py).
//
// ACP's router expands a request into named parameters and then merges the
// request's "_meta" map on top of them, so a metadata key that collides with a
// protocol field silently replaces that field. Metadata is the one part of a
// request an application fills with data from elsewhere (provenance, tenant
// tags), and such data must not be able to name the session a prompt runs
// against. Go's dispatcher decodes into typed structs and never merges _meta,
// so nothing is replaced here — the guard is still ported because a colliding
// _meta is a malformed request on the wire, and silently accepting it hides a
// client bug (and any future merge path would inherit it).
//
// Rejection is the only honest answer: once a merge has happened the
// substitution is indistinguishable from a legitimate request.

// metaKey is _meta as it appears on the wire.
const metaKey = "_meta"

// reservedMetaNames are the field names (and JSON aliases) each request method
// declares, minus _meta itself. Python builds this table from the SDK's own
// models, so a protocol addition is covered automatically; AGo's dispatcher is
// the schema, so the table is maintained beside it.
//
// AGO-DEVIATION: Python's table also carries the SDK's snake_case field names
// (session_id, mcp_servers, client_info, ...) because a request model accepts
// either spelling, so a `_meta: {"session_id": ...}` would displace the field
// there too. AGo's dispatcher decodes only the camelCase JSON spellings and
// never merges _meta, so the snake_case names have nothing to displace and are
// not reserved: refusing them would reject metadata that is harmless here.
var reservedMetaNames = map[string][]string{
	"initialize":     {"protocolVersion", "clientCapabilities", "clientInfo"},
	"authenticate":   {"methodId"},
	"session/new":    {"cwd", "mcpServers"},
	"session/load":   {"sessionId", "cwd", "mcpServers"},
	"session/prompt": {"sessionId", "prompt"},
	"session/cancel": {"sessionId"},
}

// collidingMetaKeys reports the _meta keys in params that would displace one of
// method's fields. Unknown methods and params that are not an object have no
// reserved names and no collisions.
func collidingMetaKeys(method string, params json.RawMessage) []string {
	reserved := reservedMetaNames[method]
	if len(reserved) == 0 || len(params) == 0 {
		return nil
	}
	var envelope struct {
		Meta map[string]any `json:"_meta"`
	}
	if err := json.Unmarshal(params, &envelope); err != nil || len(envelope.Meta) == 0 {
		return nil
	}
	var out []string
	for _, name := range reserved {
		if _, clash := envelope.Meta[name]; clash {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// metaGuardError is the invalid-params error a colliding request is refused
// with (Python parity: RequestError.invalid_params with the reserved names).
func metaGuardError(collisions []string) *rpcError {
	return rpcErr(codeInvalidParams,
		"_meta may not contain the reserved request field(s): %s", strings.Join(collisions, ", "))
}

// guardNotification drops a notification whose _meta collides: a notification
// has no response channel, so dropping it is the whole remedy — and acting on
// it is exactly what must not happen.
func guardNotification(method string, params json.RawMessage) bool {
	collisions := collidingMetaKeys(method, params)
	if len(collisions) == 0 {
		return false
	}
	slog.Warn("acp: dropped a notification whose _meta would overwrite request fields",
		"method", method, "fields", strings.Join(collisions, ", "))
	return true
}
