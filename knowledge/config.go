package knowledge

import (
	"github.com/luckystrker/ago/aggregate"
	"github.com/luckystrker/ago/compact"
)

// Config groups the knowledge-related agent parameters (Python parity:
// KnowledgeConfig in config.py). The store is registered into the conversation
// dependencies under DepKey so other consumers can reach it; everything else is
// a side effect of attaching a store, each opt-out via its flag.
//
// Go structs have no per-field defaults: the zero Config disables every side
// effect. NewConfig applies the Python defaults (ExposeTool and WriteEventLog
// on; the strategy fields stay nil — Python also defaults them to None).
type Config struct {
	// Store is the backing knowledge store (required; a nil store makes the
	// agent option a no-op).
	Store Store
	// ExposeTool gives the agent an auto-injected "knowledge" tool letting the
	// model call read / write / list / delete on the store. Turn it off when
	// policies are the only consumer and the model should not see the store.
	ExposeTool bool
	// WriteEventLog persists the stream history to the store's log directory at
	// the end of every ask (logs/<streamID>.jsonl).
	WriteEventLog bool
	// Compact / CompactTrigger arm post-turn compaction (Python parity:
	// KnowledgeConfig.compact / compact_trigger — the agent constructor adds
	// the compaction middleware whenever a strategy is set). Both come from the
	// compact package; a nil trigger leaves the wired middleware a safe no-op.
	//
	// AGO-DEVIATION: Python's CompactStrategy protocol receives the store
	// (compact(events, context, store)) and its built-ins persist dropped
	// segments through EventLogWriter.persist_dropped; the AGo strategies are
	// the transcript-shaped ones of the compact package (D-028) — the middleware
	// cuts and replaces the history itself and no store-aware strategy type
	// exists yet. A nil trigger is a no-op where Python crashes reading
	// trigger.max_events at turn time.
	Compact compact.Strategy
	// CompactTrigger decides when Compact fires; nil disables firing.
	CompactTrigger compact.Trigger
	// Aggregate / AggregateTrigger arm post-turn aggregation (Python parity:
	// KnowledgeConfig.aggregate / aggregate_trigger). The agent constructor
	// adds the aggregation middleware when both are set.
	//
	// AGO-DEVIATION: Python's AggregateStrategy protocol is store-aware
	// (aggregate(events, context, store) writes knowledge files) and its
	// constructor arms the middleware off the trigger's numeric conditions,
	// reporting a nil strategy as AggregationFailed at fire time; the AGo
	// strategies are the transcript-shaped ones of the aggregate package
	// (D-026), the trigger is an interface without inspectable conditions, so
	// the wiring requires both halves non-nil. A store-aware strategy type
	// remains open work.
	Aggregate aggregate.Strategy
	// AggregateTrigger decides when Aggregate fires; nil disables the wiring.
	AggregateTrigger aggregate.Trigger
	// Bootstrap overrides the layout creator. nil falls back to
	// DefaultBootstrap with MentionTool set to ExposeTool, so the SKILL.md text
	// advertises the knowledge tool only when the tool is actually exposed.
	Bootstrap StoreBootstrap
}

// NewConfig returns a Config over store with the Python defaults: the
// knowledge tool exposed and the event log written.
func NewConfig(store Store) Config {
	return Config{Store: store, ExposeTool: true, WriteEventLog: true}
}

// DepKey is the context dependency id under which the store is registered
// (Python parity: context.dependencies[KnowledgeStore], type-keyed there; AGo's
// dependencies are string-keyed, "ag:" being the framework-reserved prefix —
// same convention as task.DepKey).
//
// AGO-DEVIATION: the AGo memory policies take the store in their constructors
// (policies.NewWorkingMemoryPolicy et al.), so no Go consumer reads this key
// yet; it exists so DI consumers (tools.Inject slots) and future policies can
// resolve the store from the context like Python does.
const DepKey = "ag:knowledge"
