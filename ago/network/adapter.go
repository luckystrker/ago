package network

// ChannelAdapter is the code half of the manifest/adapter split. The hub
// looks adapters up by (manifest type, version) at channel-create time;
// re-registering at a new version never mutates in-flight channels (they
// keep their manifest snapshot).
//
// Contract for OnEnvelope — called by the hub under the channel lock, once
// per envelope:
//
//  1. validate first: on error the envelope is rejected and the channel
//     state must be untouched (the hub appends the envelope to the WAL only
//     after a nil error);
//  2. then fold: merge env into ch.AdapterState and return the Routing
//     verdict.
//
// OnEnvelope must be deterministic — Hub.Replay re-folds the WAL through it
// to rebuild state. Protocol (ag2.channel.*) and task (ag2.task.*) envelopes
// are hub bookkeeping: adapters must accept them without touching protocol
// shape.
//
// AGO-DEVIATION vs Python: the five Protocol methods
// (validate_create/validate_send/fold/on_accepted/initial_state) collapse
// into the three brief methods — send validation and folding merge into
// OnEnvelope. The brief's `ch *ChannelState` parameter is the hub's
// channel-state record (*ChannelHandle here); the literal ChannelState type
// is the lifecycle enum.
type ChannelAdapter interface {
	// Type is the manifest dispatch key (e.g. "consulting").
	Type() string
	// ValidateCreate rejects invalid creation (bad participant count,
	// missing knobs, ...). manifest.Participants is the full ordered list.
	ValidateCreate(manifest ChannelManifest) error
	// OnEnvelope validates env against the channel state, folds it in, and
	// reports the routing verdict.
	OnEnvelope(ch *ChannelHandle, env Envelope) (Routing, error)
	// InitialState returns the empty fold state for a fresh channel.
	InitialState() any
}

// Routing is the adapter's verdict after an envelope is accepted.
type Routing struct {
	// NextSender is the participant expected to send next; "" means no
	// specific expectation (free-form channels) or protocol end.
	NextSender string `json:"next_sender,omitempty"`
	// Close closes the channel after this envelope is dispatched.
	Close bool `json:"close,omitempty"`
	// CloseReason labels the close (channel metadata + closed envelope).
	CloseReason string `json:"close_reason,omitempty"`
}

// StateInitializer is an optional adapter extension: adapters whose fold
// state derives from the channel record (the discussion rotation, the
// workflow graph + context) implement it. The hub calls InitialStateFor at
// channel-create time and during WAL replay; adapters without it keep the
// empty InitialState().
type StateInitializer interface {
	InitialStateFor(ch *ChannelHandle) any
}

// initAdapterState installs the right initial state for a fresh handle.
func initAdapterState(adapter ChannelAdapter, ch *ChannelHandle) {
	if init, ok := adapter.(StateInitializer); ok {
		ch.AdapterState = init.InitialStateFor(ch)
		return
	}
	ch.AdapterState = adapter.InitialState()
}
