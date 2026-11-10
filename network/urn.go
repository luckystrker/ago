package network

import "strings"

// HubURNPrefix is the scheme of a canonical hub URN, the address of an agent
// that lives on another hub (Python parity: ids.py _HUB_URN_PREFIX).
const HubURNPrefix = "hub://"

// ParseHubURN splits a "hub://<hub_id>/<agent_id>" URN into its parts
// (Python parity: ids.py parse_hub_urn). A string that is not such a URN —
// including a malformed one (missing slash, empty hub id, empty agent id) —
// yields ("", s): the input comes back unchanged as the agent id, so callers
// that pass a bare agent id get the same value out and the parse is
// idempotent.
//
// The canonical inverse is HubURNPrefix + hubID + "/" + agentID; there is no
// helper for it because the format is trivial and writing it at the call site
// keeps the intent visible.
func ParseHubURN(s string) (hubID, agentID string) {
	rest, ok := strings.CutPrefix(s, HubURNPrefix)
	if !ok {
		return "", s
	}
	hubID, agentID, ok = strings.Cut(rest, "/")
	if !ok || hubID == "" || agentID == "" {
		return "", s
	}
	return hubID, agentID
}

// HubURN is the canonical address of agentID on hubID — the inverse of
// ParseHubURN.
func HubURN(hubID, agentID string) string { return HubURNPrefix + hubID + "/" + agentID }
