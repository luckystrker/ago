package network

import (
	"fmt"
	"reflect"
	"sort"
	"sync"
)

// Transition vocabulary for orchestrated workflows (Python:
// network/transitions.py). The graph is data: it persists in channel knobs
// (ToDict / GraphFromDict) and drives the WorkflowAdapter's speaker
// selection.
//
// AGO-DEVIATION vs Python: the graph is serialized under the Go key names
// ("initial"/"when"/"to" instead of "initial_speaker"/"when"/"then") and
// carries no TransitionRegistry — custom targets and conditions cannot be
// stored in channel knobs, because the built-in set is a closed Go type
// switch.

// TurnContext is what a target/condition sees when the workflow adapter
// folds one substantive envelope: the envelope itself plus the snapshotted
// channel bookkeeping. Terminate-style targets report their close reason by
// writing CloseReason before returning "".
type TurnContext struct {
	Envelope         Envelope       // the just-accepted envelope
	ParticipantOrder []string       // participant ids in join order
	LastSpeakerID    string         // sender of the previous substantive envelope
	CreatorID        string         // channel creator (initiator)
	TurnCount        int            // substantive envelopes folded so far (incl. this one)
	ContextVars      map[string]any // channel-scoped context variables
	CloseReason      string         // written by terminating targets; read by the adapter
}

// TransitionTarget resolves where the next turn goes. A returned speaker of
// "" terminates the channel (set ctx.CloseReason first to label it).
type TransitionTarget interface {
	Next(ctx *TurnContext) (string, error)
}

// TransitionCondition says when a transition fires. Pure predicate.
type TransitionCondition interface {
	Match(ctx *TurnContext) bool
}

// Transition pairs a condition with a target. Lower Priority is checked
// first; equal priorities resolve in list order (Python parity:
// transitions.py Transition.priority, workflow.py _select's stable sort).
type Transition struct {
	When     TransitionCondition
	To       TransitionTarget
	Priority int
}

// TransitionGraph is the orchestrator script: an initial speaker, ordered
// transitions, an optional fall-through target and an optional turn cap
// (0 = unlimited). A nil DefaultTarget terminates with "no_transition", which
// is the graph behavior before default targets existed.
type TransitionGraph struct {
	Initial       string
	Transitions   []Transition
	DefaultTarget TransitionTarget
	MaxTurns      int
}

// Select runs the graph against ctx: the lowest-priority matching
// transition's target decides (ties in list order); with no match the
// graph's DefaultTarget decides, or — when it is nil — the channel terminates
// with "no_transition" (Python parity: workflow.py _select).
func (g TransitionGraph) Select(ctx *TurnContext) Routing {
	for _, tr := range g.sortedTransitions() {
		if !tr.When.Match(ctx) {
			continue
		}
		return applyTarget(tr.To, ctx)
	}
	if g.DefaultTarget == nil {
		return Routing{Close: true, CloseReason: "no_transition"}
	}
	return applyTarget(g.DefaultTarget, ctx)
}

// sortedTransitions returns the transitions in evaluation order: ascending
// priority, ties in list order (a stable sort of a copy, so Select stays
// side-effect free).
func (g TransitionGraph) sortedTransitions() []Transition {
	out := append([]Transition(nil), g.Transitions...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out
}

// applyTarget runs one target and turns its answer into a Routing.
func applyTarget(t TransitionTarget, ctx *TurnContext) Routing {
	next, err := t.Next(ctx)
	if err != nil {
		return Routing{Close: true, CloseReason: fmt.Sprintf("transition_error: %v", err)}
	}
	if next == "" {
		return Routing{Close: true, CloseReason: nonEmpty(ctx.CloseReason, "workflow_terminated")}
	}
	return Routing{NextSender: next}
}

func nonEmpty(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

// --- convenience builders ----------------------------------------------------

// NewRoundRobinGraph cycles through the channel's participants in join order
// starting with participants[0], ending after maxTurns substantive turns
// (0 = unlimited). It needs at least one participant.
func NewRoundRobinGraph(participants []string, maxTurns int) (TransitionGraph, error) {
	if len(participants) == 0 {
		return TransitionGraph{}, fmt.Errorf("%w: round robin requires at least 1 participant", ErrProtocol)
	}
	return TransitionGraph{
		Initial:     participants[0],
		Transitions: []Transition{{When: Always{}, To: &RoundRobinTarget{}}},
		MaxTurns:    maxTurns,
	}, nil
}

// NewSequenceGraph is a pipeline: steps[0] → steps[1] → … then the channel
// terminates with reason "sequence_complete". Any speaker outside the
// sequence terminates it the same way (the fall-through target). MaxTurns is
// len(steps).
func NewSequenceGraph(steps []string) (TransitionGraph, error) {
	if len(steps) == 0 {
		return TransitionGraph{}, fmt.Errorf("%w: sequence requires at least 1 step", ErrProtocol)
	}
	transitions := make([]Transition, 0, len(steps))
	for i := 0; i < len(steps)-1; i++ {
		transitions = append(transitions, Transition{
			When: FromSpeaker{AgentID: steps[i]},
			To:   AgentTarget{AgentID: steps[i+1]},
		})
	}
	return TransitionGraph{
		Initial:       steps[0],
		Transitions:   transitions,
		DefaultTarget: TerminateTarget{Reason: "sequence_complete"},
		MaxTurns:      len(steps),
	}, nil
}

// --- built-in targets --------------------------------------------------------

// AgentTarget resolves to a named participant.
type AgentTarget struct {
	AgentID string
}

func (t AgentTarget) Next(_ *TurnContext) (string, error) { return t.AgentID, nil }

// RoundRobinTarget cycles to the participant after the last speaker. Agents
// overrides the participant order when non-empty. Use a pointer so the
// internal cursor is shared, not copied.
type RoundRobinTarget struct {
	Agents []string

	mu    sync.Mutex
	state int // fallback cursor when no speaker is known yet
}

func (t *RoundRobinTarget) Next(ctx *TurnContext) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	order := t.Agents
	if len(order) == 0 {
		order = ctx.ParticipantOrder
	}
	if len(order) == 0 {
		ctx.CloseReason = "no_participants"
		return "", nil
	}
	anchor := ctx.LastSpeakerID
	if anchor == "" {
		anchor = ctx.Envelope.SenderID
	}
	next := 0
	found := false
	if anchor != "" {
		for i, id := range order {
			if id == anchor {
				next = (i + 1) % len(order)
				found = true
				break
			}
		}
	}
	if !found {
		if anchor != "" {
			// Unknown speaker: start from the head (Python parity).
			next = 0
		} else {
			// No speaker history at all: hand out participants round-robin
			// from the internal cursor (defensive path; real channels always
			// have a sender).
			next = (t.state + 1) % len(order)
		}
	}
	t.state = next
	return order[next], nil
}

// StayTarget keeps the same speaker.
type StayTarget struct{}

func (StayTarget) Next(ctx *TurnContext) (string, error) {
	if ctx.LastSpeakerID != "" {
		return ctx.LastSpeakerID, nil
	}
	return ctx.Envelope.SenderID, nil
}

// RevertToInitiatorTarget sends the turn back to the channel creator.
type RevertToInitiatorTarget struct{}

func (RevertToInitiatorTarget) Next(ctx *TurnContext) (string, error) {
	return ctx.CreatorID, nil
}

// TerminateTarget ends the channel with a reason.
type TerminateTarget struct {
	Reason string
}

func (t TerminateTarget) Next(ctx *TurnContext) (string, error) {
	if t.Reason != "" {
		ctx.CloseReason = t.Reason
	}
	return "", nil
}

// --- built-in conditions -----------------------------------------------------

// Always fires.
type Always struct{}

func (Always) Match(*TurnContext) bool { return true }

// FromSpeaker fires when the folded envelope was sent by AgentID.
type FromSpeaker struct {
	AgentID string
}

func (c FromSpeaker) Match(ctx *TurnContext) bool { return ctx.Envelope.SenderID == c.AgentID }

// ToolCalled fires when the folded packet's routing tool matches — the
// framework populates routing.tool from the agent's routing-tool calls.
type ToolCalled struct {
	Name string
}

func (c ToolCalled) Match(ctx *TurnContext) bool {
	if ctx.Envelope.EventType != EnvPacket {
		return false
	}
	p := decodePacket(ctx.Envelope)
	tool, _ := p.Routing["tool"].(string)
	return tool == c.Name
}

// ContextEquals fires when the channel-scoped context variable Key holds
// Value (Python parity: transitions.py ContextEquals). A key that was never
// set compares as nil, so ContextEquals{Key: "foo"} fires while foo is unset
// or deleted. Comparison is deep (maps and slices included), which is what
// Python's == does.
//
// AGO-DEVIATION: a value restored from channel knobs arrives as generic JSON
// (numbers as float64), so a Value built as an int compares unequal to the
// same number after a round trip — the same trap Python's == sets for int vs
// float only where it matters (it compares them equal; AGo does not).
type ContextEquals struct {
	Key   string
	Value any
}

func (c ContextEquals) Match(ctx *TurnContext) bool {
	var have any
	if ctx.ContextVars != nil {
		have = ctx.ContextVars[c.Key]
	}
	return reflect.DeepEqual(have, c.Value)
}

// --- serialization (channel knobs) -------------------------------------------

// ToDict serialises the graph into a JSON-friendly dict for
// ChannelManifest.Knobs["graph"]. Priority is omitted at its zero value and
// the default target only when one is set, so a graph built the old way
// serialises exactly as before.
func (g TransitionGraph) ToDict() map[string]any {
	transitions := make([]any, 0, len(g.Transitions))
	for _, tr := range g.Transitions {
		entry := map[string]any{
			"when": conditionToDict(tr.When),
			"to":   targetToDict(tr.To),
		}
		if tr.Priority != 0 {
			entry["priority"] = tr.Priority
		}
		transitions = append(transitions, entry)
	}
	d := map[string]any{
		"initial":     g.Initial,
		"transitions": transitions,
	}
	if g.DefaultTarget != nil {
		d["default_target"] = targetToDict(g.DefaultTarget)
	}
	if g.MaxTurns > 0 {
		d["max_turns"] = g.MaxTurns
	}
	return d
}

// GraphFromDict rebuilds a graph produced by ToDict (knobs travel as
// generic maps after a JSON round trip).
func GraphFromDict(d map[string]any) (TransitionGraph, error) {
	var g TransitionGraph
	if d == nil {
		return g, fmt.Errorf("%w: nil graph dict", ErrProtocol)
	}
	g.Initial, _ = d["initial"].(string)
	if g.Initial == "" {
		return g, fmt.Errorf("%w: graph requires initial speaker", ErrProtocol)
	}
	if mt, ok := toInt(d["max_turns"]); ok {
		g.MaxTurns = mt
	}
	if dt, ok := d["default_target"].(map[string]any); ok {
		target, err := targetFromDict(dt)
		if err != nil {
			return g, err
		}
		g.DefaultTarget = target
	}
	raw, ok := d["transitions"].([]any)
	if !ok {
		return g, nil
	}
	for i, r := range raw {
		tr, ok := r.(map[string]any)
		if !ok {
			return g, fmt.Errorf("%w: transition %d is not an object", ErrProtocol, i)
		}
		when, err := conditionFromDict(mapOf(tr["when"]))
		if err != nil {
			return g, err
		}
		to, err := targetFromDict(mapOf(tr["to"]))
		if err != nil {
			return g, err
		}
		priority, _ := toInt(tr["priority"])
		g.Transitions = append(g.Transitions, Transition{When: when, To: to, Priority: priority})
	}
	return g, nil
}

func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// toInt normalises JSON numbers (float64) and plain ints.
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

func toString(v any) string {
	s, _ := v.(string)
	return s
}

func toStringSlice(v any) []string {
	switch raw := v.(type) {
	case []string:
		return append([]string(nil), raw...)
	case []any:
		out := make([]string, 0, len(raw))
		for _, r := range raw {
			if s, ok := r.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func conditionToDict(c TransitionCondition) map[string]any {
	switch c := c.(type) {
	case Always:
		return map[string]any{"name": "always"}
	case FromSpeaker:
		return map[string]any{"name": "from_speaker", "agent_id": c.AgentID}
	case ToolCalled:
		return map[string]any{"name": "tool_called", "tool": c.Name}
	case ContextEquals:
		return map[string]any{"name": "context_equals", "key": c.Key, "value": c.Value}
	default:
		return map[string]any{"name": fmt.Sprintf("%T", c)}
	}
}

func conditionFromDict(d map[string]any) (TransitionCondition, error) {
	if d == nil {
		return Always{}, nil
	}
	switch d["name"] {
	case "always", "":
		return Always{}, nil
	case "from_speaker":
		return FromSpeaker{AgentID: toString(d["agent_id"])}, nil
	case "tool_called":
		return ToolCalled{Name: toString(d["tool"])}, nil
	case "context_equals":
		return ContextEquals{Key: toString(d["key"]), Value: d["value"]}, nil
	default:
		return nil, fmt.Errorf("%w: unknown condition %q", ErrProtocol, d["name"])
	}
}

func targetToDict(t TransitionTarget) map[string]any {
	switch t := t.(type) {
	case AgentTarget:
		return map[string]any{"name": "agent", "agent_id": t.AgentID}
	case *RoundRobinTarget:
		d := map[string]any{"name": "round_robin"}
		if len(t.Agents) > 0 {
			d["agents"] = t.Agents
		}
		return d
	case StayTarget:
		return map[string]any{"name": "stay"}
	case RevertToInitiatorTarget:
		return map[string]any{"name": "revert_to_initiator"}
	case TerminateTarget:
		return map[string]any{"name": "terminate", "reason": t.Reason}
	default:
		return map[string]any{"name": fmt.Sprintf("%T", t)}
	}
}

func targetFromDict(d map[string]any) (TransitionTarget, error) {
	if d == nil {
		return TerminateTarget{Reason: "workflow_terminated"}, nil
	}
	switch d["name"] {
	case "agent":
		return AgentTarget{AgentID: toString(d["agent_id"])}, nil
	case "round_robin":
		return &RoundRobinTarget{Agents: toStringSlice(d["agents"])}, nil
	case "stay":
		return StayTarget{}, nil
	case "revert_to_initiator":
		return RevertToInitiatorTarget{}, nil
	case "terminate", "":
		return TerminateTarget{Reason: toString(d["reason"])}, nil
	default:
		return nil, fmt.Errorf("%w: unknown target %q", ErrProtocol, d["name"])
	}
}
