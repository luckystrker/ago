package network

import (
	"fmt"
	"sort"
	"strings"

	"github.com/luckystrker/ago/events"
)

// ViewPolicy projects the WAL slice a participant should see into model
// events, once per turn before the participant's LLM call (Python parity:
// network/views/base.py + builtin.py). The projection is a pure function of
// its inputs — calling Project twice with the same arguments produces the
// same events. The caller appends the current turn's ModelRequest itself;
// the same Project method serves both the historical slice and the
// single triggering envelope so labeling stays consistent.
//
// Call pattern (the adapter folds): a client-side handler renders the
// channel's ReadWAL output through Project before invoking the agent.
type ViewPolicy interface {
	// Name identifies the policy (Python class attribute `name`).
	Name() string
	// Project converts wal into model events for participant.
	Project(wal []Envelope, participant string, opts ViewOptions) []events.Event
}

// ViewOptions carries the per-call collaborators of Project.
type ViewOptions struct {
	// Channel is the channel metadata snapshot (Python ChannelMetadata).
	// The built-in views never read it; adapters with channel-shape-aware
	// projections can.
	Channel *ChannelHandle
	// RenderEnvelope projects one envelope to its LLM-visible input parts;
	// an empty result skips the envelope. Supplied by the channel's
	// adapter so views stay adapter-neutral. Nil falls back to
	// DefaultRenderEnvelope.
	RenderEnvelope func(env Envelope) []events.Input
	// NameFor resolves an agent id to its human-facing Passport name for
	// views that label projection lines. Nil falls back to
	// DefaultNameResolver.
	NameFor func(agentID string) string
}

// EnvelopeRenderer projects one envelope to its LLM-visible parts (the
// adapter's render_envelope). Each part stays its own events.Input so a
// view keeps the boundary between, say, a handoff signal and the body
// that follows it.
type EnvelopeRenderer = func(env Envelope) []events.Input

// NameResolver resolves an agent id to its human-facing Passport name.
type NameResolver = func(agentID string) string

// DefaultNameResolver is the identity NameResolver: the id verbatim. Test
// and headless call sites use it; production paths pass a hub-backed
// resolver (Hub.NameFor).
func DefaultNameResolver(agentID string) string { return agentID }

// NameFor resolves a registration id or name to its Passport name; an
// unknown id resolves to the id itself so projection never fails.
func (h *Hub) NameFor(nameOrID string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.agentNameLocked(nameOrID)
}

// DefaultRenderEnvelope is the Go analog of the Python adapters'
// default_render_envelope plus the packet-shape handling the AGo adapters
// fold: EnvText renders the text payload as a single TextInput part,
// EnvPacket renders the handoff signal line and the body as separate parts
// (mirroring client.renderInbound), every other event type renders nothing
// (hub bookkeeping the LLM does not reason about).
func DefaultRenderEnvelope(env Envelope) []events.Input {
	switch env.EventType {
	case EnvText:
		// The decoded text projects whatever it is — "" included.
		return []events.Input{&events.TextInput{Content: env.Text()}}
	case EnvPacket:
		p := decodePacket(env)
		var parts []events.Input
		if p.Routing["kind"] == "handoff" {
			tool := "handoff"
			if s, ok := p.Routing["tool"].(string); ok && s != "" {
				tool = s
			}
			reason, _ := p.Routing["reason"].(string)
			if reason != "" {
				parts = append(parts, &events.TextInput{Content: fmt.Sprintf("[Handed off via %s] %s", tool, reason)})
			} else {
				parts = append(parts, &events.TextInput{Content: fmt.Sprintf("[Handed off via %s]", tool)})
			}
		}
		if p.Body != "" {
			parts = append(parts, &events.TextInput{Content: p.Body})
		}
		return parts
	default:
		return nil
	}
}

func (o ViewOptions) render() EnvelopeRenderer {
	if o.RenderEnvelope != nil {
		return o.RenderEnvelope
	}
	return DefaultRenderEnvelope
}

func (o ViewOptions) nameFor() NameResolver {
	if o.NameFor != nil {
		return o.NameFor
	}
	return DefaultNameResolver
}

// visibleParts collects the (envelope, parts) pairs the participant may
// see, in WAL order.
func visibleParts(wal []Envelope, participant string, render EnvelopeRenderer) []visibleEnvelope {
	var out []visibleEnvelope
	for _, env := range wal {
		if !VisibleTo(env, participant) {
			continue
		}
		parts := render(env)
		if len(parts) == 0 {
			continue
		}
		out = append(out, visibleEnvelope{env: env, parts: parts})
	}
	return out
}

type visibleEnvelope struct {
	env   Envelope
	parts []events.Input
}

// FullTranscript translates every envelope visible to the participant
// verbatim. Inbound envelopes become ModelRequest (a user turn); the
// participant's own envelopes become ModelMessage. The role bit is the
// speaker disambiguator — sufficient for 2-party channels; use
// NamedTranscript for N-party.
type FullTranscript struct{}

// Name implements ViewPolicy.
func (FullTranscript) Name() string { return "full_transcript" }

// Project implements ViewPolicy.
func (FullTranscript) Project(wal []Envelope, participant string, opts ViewOptions) []events.Event {
	return projectEvents(visibleParts(wal, participant, opts.render()), participant)
}

// WindowedSummary keeps the last recentN visible envelopes verbatim and
// folds everything older into one CompactionSummary at the head of the
// projection — at most recentN+1 events regardless of WAL length. The
// summary is a static stat line, no LLM call.
type WindowedSummary struct {
	RecentN int
}

// NewWindowedSummary validates recentN (must be >= 1) and returns the view.
func NewWindowedSummary(recentN int) (WindowedSummary, error) {
	if recentN < 1 {
		return WindowedSummary{}, fmt.Errorf("network: recent_n must be >= 1, got %d", recentN)
	}
	return WindowedSummary{RecentN: recentN}, nil
}

// Name implements ViewPolicy.
func (WindowedSummary) Name() string { return "windowed_summary" }

// Project implements ViewPolicy.
func (v WindowedSummary) Project(wal []Envelope, participant string, opts ViewOptions) []events.Event {
	visible := visibleParts(wal, participant, opts.render())
	head, recent := splitWindow(visible, v.RecentN)
	var out []events.Event
	if head != nil {
		out = append(out, head)
	}
	return append(out, projectEvents(recent, participant)...)
}

// NamedTranscript is FullTranscript with a "[<sender name>]:" prefix on
// every non-self request — necessary in N-party channels where every
// non-self envelope would otherwise collapse into one unattributed user
// stream. Self envelopes stay unlabeled ModelMessages.
type NamedTranscript struct{}

// Name implements ViewPolicy.
func (NamedTranscript) Name() string { return "named_transcript" }

// Project implements ViewPolicy.
func (NamedTranscript) Project(wal []Envelope, participant string, opts ViewOptions) []events.Event {
	return projectNamedEvents(visibleParts(wal, participant, opts.render()), participant, opts.nameFor())
}

// NamedWindowedSummary is WindowedSummary with sender labels on non-self
// lines (the default view for the discussion and workflow adapters in
// Python). The head CompactionSummary lists speakers by name.
type NamedWindowedSummary struct {
	RecentN int
}

// NewNamedWindowedSummary validates recentN (must be >= 1).
func NewNamedWindowedSummary(recentN int) (NamedWindowedSummary, error) {
	if recentN < 1 {
		return NamedWindowedSummary{}, fmt.Errorf("network: recent_n must be >= 1, got %d", recentN)
	}
	return NamedWindowedSummary{RecentN: recentN}, nil
}

// Name implements ViewPolicy.
func (NamedWindowedSummary) Name() string { return "named_windowed_summary" }

// Project implements ViewPolicy.
func (v NamedWindowedSummary) Project(wal []Envelope, participant string, opts ViewOptions) []events.Event {
	visible := visibleParts(wal, participant, opts.render())
	head, recent := splitWindowNamed(visible, v.RecentN, opts.nameFor())
	var out []events.Event
	if head != nil {
		out = append(out, head)
	}
	return append(out, projectNamedEvents(recent, participant, opts.nameFor())...)
}

// splitWindow splits visible at recentN: the elided head becomes one
// CompactionSummary (nil when nothing is elided), recent is the verbatim
// tail.
func splitWindow(visible []visibleEnvelope, recentN int) (events.Event, []visibleEnvelope) {
	return splitWindowNamed(visible, recentN, nil)
}

func splitWindowNamed(visible []visibleEnvelope, recentN int, nameFor NameResolver) (events.Event, []visibleEnvelope) {
	if len(visible) <= recentN {
		return nil, visible
	}
	cutoff := len(visible) - recentN
	older := make([]Envelope, cutoff)
	for i, v := range visible[:cutoff] {
		older[i] = v.env
	}
	summary := &events.CompactionSummary{Content: summarizeOlder(older, nameFor)}
	return summary, visible[cutoff:]
}

// projectEvents maps visible envelopes to events: own text parts become one
// ModelMessage each (an assistant message is text — non-text own parts do
// not project); inbound ones a single ModelRequest over all parts.
func projectEvents(visible []visibleEnvelope, participant string) []events.Event {
	var out []events.Event
	for _, v := range visible {
		if v.env.SenderID == participant {
			for _, part := range v.parts {
				if ti, ok := part.(*events.TextInput); ok {
					out = append(out, events.ModelMessage{Content: ti.Content})
				}
			}
			continue
		}
		out = append(out, events.ModelRequest{Parts: v.parts})
	}
	return out
}

// projectNamedEvents is projectEvents with the sender label leading an
// inbound request. The label joins a leading text part; ahead of any other
// kind of part it stands alone.
func projectNamedEvents(visible []visibleEnvelope, participant string, nameFor NameResolver) []events.Event {
	var out []events.Event
	for _, v := range visible {
		if v.env.SenderID == participant {
			out = append(out, projectEvents([]visibleEnvelope{v}, participant)...)
			continue
		}
		label := fmt.Sprintf("[%s]:", nameFor(v.env.SenderID))
		parts := make([]events.Input, len(v.parts))
		copy(parts, v.parts)
		if len(parts) > 0 {
			if ti, ok := parts[0].(*events.TextInput); ok {
				parts[0] = &events.TextInput{Content: label + " " + ti.Content}
			} else {
				parts = append([]events.Input{&events.TextInput{Content: label}}, parts...)
			}
		}
		out = append(out, events.ModelRequest{Parts: parts})
	}
	return out
}

// summarizeOlder renders the static stat line for the elided head:
// "Earlier in this channel: N messages from a, b." A nil nameFor labels by
// raw id (the unnamed window); otherwise by resolved Passport name.
func summarizeOlder(older []Envelope, nameFor NameResolver) string {
	speakers := make(map[string]bool, len(older))
	for _, env := range older {
		if nameFor != nil {
			speakers[nameFor(env.SenderID)] = true
		} else {
			speakers[env.SenderID] = true
		}
	}
	names := make([]string, 0, len(speakers))
	for s := range speakers {
		names = append(names, s)
	}
	sort.Strings(names)
	plural := "s"
	if len(older) == 1 {
		plural = ""
	}
	return fmt.Sprintf("Earlier in this channel: %d message%s from %s.", len(older), plural, strings.Join(names, ", "))
}
