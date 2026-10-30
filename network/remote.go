package network

// RemoteAgentProxy is the federation dispatch seam (Python parity:
// network/remote.py, interface only). A proxy carries envelopes from the
// local hub to a participant whose passport has KindRemoteAgent: the hub
// would look proxies up by Scheme (typically the remote participant's
// Auth scheme — "a2a", "grpc", ...) at dispatch time and delegate instead
// of sending to a local endpoint.
//
// INTERFACE STUB — no implementations ship and the hub does not consult
// this seam in the current subset (the dispatch path routes local
// passports only; resolving remote audiences is the arbiter's
// ResolveUnknownAudience hook). Federation stays deferred (D-010/D-031;
// D-056 records the stub).
type RemoteAgentProxy interface {
	// Scheme is the registry key; the hub matches it against the remote
	// participant's Auth scheme.
	Scheme() string
	// Dispatch delivers env to the recipient over the proxy's transport.
	// Implementations encode the envelope into the target wire protocol
	// (A2A, gRPC, message bus, ...) and return once the envelope has been
	// handed off; errors propagate back to the caller.
	Dispatch(env Envelope, recipient Passport) error
	// Close releases transport resources (connection pools, background
	// tasks). Idempotent.
	Close() error
}
