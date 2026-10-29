// Command group demonstrates the AGo network layer offline: two participants
// ("writer", "reviewer") meet in a workflow channel on the in-process hub and
// take turns under a declarative TransitionGraph. The writer is a plain
// callback with a fixed script; the reviewer is a real ago/agent whose LLM is
// a scripted test config, bridged into the hub through a client-side handler
// (the caller bridges agent turns; network never imports agent). Two scenarios run
// over the same hub:
//
//  1. RoundRobinTarget + MaxTurns: every turn goes to the next participant
//     and the turn cap closes the channel with reason "max_turns";
//  2. TerminateTarget + ToolCalled: the reviewer's approval reply carries a
//     handoff marker (the round-bridge stand-in for a routing-tool call)
//     and ends the channel with a custom close reason.
//
// Each handler form of the client layer appears once: the writer uses the
// full-control EnvelopeFunc, the demo-1 reviewer the simple MessageFunc, the
// demo-2 reviewer the workflow RoundFunc. Every hub-accepted envelope is
// printed from the channel log (ReadWAL), so the whole protocol
// conversation — invite, ack, opened, one packet per turn, closed — is
// visible, plus the final channel state. A silent round (an empty reply)
// keeps a participant from posting after the channel has already terminated.
//
// Run: go run ./examples/group
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ago/agent"
	"ago/events"
	"ago/network"
	agotest "ago/testing"
)

func main() {
	hub := network.NewHub()
	hub.RegisterAdapter(network.WorkflowAdapter{}, "")
	client := network.NewHubClient(hub)
	ctx := context.Background()

	demoMaxTurns(ctx, client)
	fmt.Println()
	demoTerminate(ctx, client)
}

// demoMaxTurns runs the pure round-robin graph: the two participants
// alternate until the four-turn cap closes the channel. The writer's script
// runs out one turn before the cap, and its silent round keeps it from
// replying to the closing packet.
func demoMaxTurns(ctx context.Context, c *network.HubClient) {
	fmt.Println("=== demo 1: RoundRobinTarget + MaxTurns ===")
	fmt.Println("graph: initial=writer; every turn -> next participant (round-robin); cap = 4 turns")

	writer, err := registerWriter(c, []string{
		"REQ-1: the core stays stdlib-only.",
		"REQ-2: every example runs offline on scripted configs.",
	})
	if err != nil {
		fmt.Println("register writer:", err)
		return
	}
	reviewer, err := registerReviewer(c, ctx,
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: "Question: how do the offline examples prove the LLM path?"},
			Usage:   events.Usage{PromptTokens: events.I64(18), CompletionTokens: events.I64(11), TotalTokens: events.I64(29)},
		},
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: "No further questions from me."},
			Usage:   events.Usage{PromptTokens: events.I64(41), CompletionTokens: events.I64(6), TotalTokens: events.I64(47)},
		},
	)
	if err != nil {
		fmt.Println("register reviewer:", err)
		writer.Close()
		return
	}
	defer func() { writer.Close(); reviewer.Close() }()

	graph := network.TransitionGraph{
		Initial: writer.AgentID(),
		Transitions: []network.Transition{
			{When: network.Always{}, To: &network.RoundRobinTarget{}},
		},
		MaxTurns: 4,
	}
	ch, err := createWorkflowChannel(writer, reviewer, graph)
	if err != nil {
		fmt.Println("create channel:", err)
		return
	}
	runAndReport(ctx, c.Hub(), writer, reviewer, ch,
		"REQ-1: the core stays stdlib-only.")
}

// demoTerminate adds a terminate transition ahead of the round-robin: when
// the reviewer's packet body marks the spec approved, the channel closes
// with a custom reason instead of riding to the cap.
func demoTerminate(ctx context.Context, c *network.HubClient) {
	fmt.Println("=== demo 2: TerminateTarget on reviewer approval ===")
	fmt.Println("graph: reviewer handoff marker -> terminate(reason=\"spec approved\"); otherwise round-robin; cap = 8 (safety)")

	writer, err := registerWriter(c, []string{
		"REQ-1: stdlib-only core. REQ-2: offline examples. Ready for review.",
		"", // after the approval: stay silent, the channel is closing
	})
	if err != nil {
		fmt.Println("register writer:", err)
		return
	}
	reviewer, err := registerReviewingAgent(c, ctx,
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: "APPROVED: scope and constraints are covered; ship it."},
			Usage:   events.Usage{PromptTokens: events.I64(23), CompletionTokens: events.I64(10), TotalTokens: events.I64(33)},
		},
	)
	if err != nil {
		fmt.Println("register reviewer:", err)
		writer.Close()
		return
	}
	defer func() { writer.Close(); reviewer.Close() }()

	// The approval routes on the builtin ToolCalled condition: the round
	// bridge stamps a handoff marker (routing.tool="handoff", no target) on
	// the approval reply, which falls through to the graph. A custom
	// TransitionCondition cannot be used here — graphs persist as dicts in
	// the channel knobs, and only builtin conditions survive that round trip.
	graph := network.TransitionGraph{
		Initial: writer.AgentID(),
		Transitions: []network.Transition{
			{When: network.ToolCalled{Name: "handoff"}, To: network.TerminateTarget{Reason: "spec approved"}},
			{When: network.Always{}, To: &network.RoundRobinTarget{}},
		},
		MaxTurns: 8,
	}
	ch, err := createWorkflowChannel(writer, reviewer, graph)
	if err != nil {
		fmt.Println("create channel:", err)
		return
	}
	runAndReport(ctx, c.Hub(), writer, reviewer, ch,
		"REQ-1: stdlib-only core. REQ-2: offline examples. Ready for review.")
}

// registerReviewingAgent wires the reviewer as a workflow round handler
// (RoundFunc has precedence over the other handler forms): the reply text
// plus a routing intent travels as one packet. The scripted reply stands in
// for the agent's routing-tool call, so an approval reply is stamped with a
// handoff marker (no target) that the graph's ToolCalled condition routes on.
func registerReviewingAgent(c *network.HubClient, ctx context.Context, scripted ...*events.ModelResponse) (*network.AgentClient, error) {
	cfg := agotest.NewTestConfig(scripted...)
	a := agent.New("spec-reviewer", "You review one-page specs. Answer in one short line.", agent.WithConfig(cfg))
	round := func(env network.Envelope) (network.Round, error) {
		msg := body(env)
		fmt.Printf("[reviewer] <- %q (agent turn)\n", msg)
		rep, err := a.Ask(ctx, msg)
		if err != nil {
			return network.Round{}, err
		}
		text := rep.Body()
		if strings.Contains(text, "APPROVED") {
			return network.Round{Text: text, Handoff: &network.Handoff{Reason: "review complete"}}, nil
		}
		return network.Round{Text: text}, nil
	}
	return c.Register("reviewer", network.WithRoundFunc(round))
}

// registerWriter wires the callback participant: a fixed script consumed one
// line per handled envelope.
func registerWriter(c *network.HubClient, script []string) (*network.AgentClient, error) {
	return c.Register("writer", network.WithEnvironment(writerHandler(script)))
}

// writerHandler answers from the script and prints both sides. The handler
// runs on the client's single pump goroutine, so the call counter needs no
// lock. An empty script slot is a silent round ("" = nothing worth posting).
func writerHandler(script []string) network.EnvelopeFunc {
	calls := 0
	return func(env network.Envelope) (string, error) {
		calls++
		reply := ""
		if calls <= len(script) {
			reply = script[calls-1]
		}
		inbound := body(env)
		if env.SenderID == "" {
			// The creator's own kickoff: Ask runs the handler on a synthetic
			// envelope with no sender.
			inbound = "(kickoff) " + inbound
		}
		fmt.Printf("[writer  ] <- %q -> %q\n", inbound, reply)
		return reply, nil
	}
}

// registerReviewer wires a real agent over a scripted test config: one
// scripted ModelResponse per channel turn, consumed in order.
func registerReviewer(c *network.HubClient, ctx context.Context, scripted ...*events.ModelResponse) (*network.AgentClient, error) {
	cfg := agotest.NewTestConfig(scripted...)
	a := agent.New("spec-reviewer", "You review one-page specs. Answer in one short line.", agent.WithConfig(cfg))
	return c.Register("reviewer", network.WithAgentFunc(reviewerBridge(ctx, a)))
}

// reviewerBridge is the caller-side agent bridge: the rendered inbound text
// goes into Agent.Ask (a full local turn), the reply body goes back on the
// channel.
func reviewerBridge(ctx context.Context, a *agent.Agent) network.MessageFunc {
	return func(msg string) (string, error) {
		fmt.Printf("[reviewer] <- %q (agent turn)\n", msg)
		rep, err := a.Ask(ctx, msg)
		if err != nil {
			return "", err
		}
		return rep.Body(), nil
	}
}

// createWorkflowChannel opens a workflow channel owned by the writer. The
// graph travels in the manifest knobs as its dict form; the adapter parses
// it back at create time (and would on WAL replay).
func createWorkflowChannel(writer, reviewer *network.AgentClient, graph network.TransitionGraph) (*network.ChannelHandle, error) {
	return writer.CreateChannel(network.ChannelManifest{
		Type:  network.TypeWorkflow,
		Knobs: map[string]any{"graph": graph.ToDict()},
	}, reviewer.AgentID())
}

// runAndReport kicks the workflow off from the creator, waits for the hub to
// close the channel, then prints the accepted envelope log and the outcome.
func runAndReport(ctx context.Context, hub *network.Hub, writer, reviewer *network.AgentClient, ch *network.ChannelHandle, kickoff string) {
	if _, err := writer.Ask(ch.ID, kickoff); err != nil {
		fmt.Println("kickoff:", err)
		return
	}
	done, err := waitTerminal(hub, ch.ID, 5*time.Second)
	if err != nil {
		fmt.Println("wait:", err)
		return
	}
	// Let both pumps finish their last round so handler output does not
	// interleave with the log below.
	settle(ctx, writer, reviewer)

	printLog(hub, ch.ID)
	fmt.Printf("result: state=%s close_reason=%q substantive_turns=%d\n",
		done.State, done.CloseReason, countTurns(hub, ch.ID))
	for _, c := range []*network.AgentClient{writer, reviewer} {
		if err := c.Err(); err != nil {
			fmt.Printf("note: %s last handler error: %v\n", nameOf(hub, c.AgentID()), err)
		}
	}
}

// waitTerminal polls the channel until it reaches a terminal state (the
// workflow ends asynchronously on the participants' pump goroutines).
func waitTerminal(hub *network.Hub, channelID string, limit time.Duration) (*network.ChannelHandle, error) {
	deadline := time.Now().Add(limit)
	for {
		ch, err := hub.Channel(channelID)
		if err != nil {
			return nil, err
		}
		if ch.State.Terminal() {
			return ch, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("channel %s still %s after %s", channelID, ch.State, limit)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// settle waits for the client pumps to drain so the printed log is stable.
func settle(ctx context.Context, cs ...*network.AgentClient) {
	for _, c := range cs {
		wctx, cancel := context.WithTimeout(ctx, time.Second)
		_ = c.WaitIdle(wctx)
		cancel()
	}
}

// printLog dumps the channel's accepted envelopes in accept order — the full
// protocol conversation: invite, ack, opened, one packet per turn, closed.
func printLog(hub *network.Hub, channelID string) {
	log, err := hub.ReadWAL(channelID)
	if err != nil {
		fmt.Println("read log:", err)
		return
	}
	fmt.Println("-- channel log --")
	for i, env := range log {
		to := nameOf(hub, env.Audience)
		if env.Audience == "" {
			to = "*"
		}
		fmt.Printf("%2d  %-25s %-8s -> %-8s %s\n",
			i+1, env.EventType, nameOf(hub, env.SenderID), to, renderPayload(env))
	}
}

// renderPayload projects an envelope into the one-line text shown in the log.
func renderPayload(env network.Envelope) string {
	if b := body(env); b != "" {
		// Packets may carry a routing intent next to the body — the marker
		// the graph's ToolCalled condition routes on (demo 2).
		var p struct {
			Routing map[string]any `json:"routing"`
		}
		_ = env.Decode(&p)
		if kind, _ := p.Routing["kind"].(string); kind == "handoff" || kind == "finish" {
			tool, _ := p.Routing["tool"].(string)
			return fmt.Sprintf("%s [routing: %s via %s]", b, kind, tool)
		}
		return b
	}
	var p struct {
		Reason       string `json:"reason"`
		ManifestType string `json:"manifest_type"`
	}
	_ = env.Decode(&p)
	switch {
	case p.Reason != "":
		return "reason=" + p.Reason
	case p.ManifestType != "":
		return "manifest=" + p.ManifestType
	default:
		return ""
	}
}

// body returns the human-readable payload of a substantive envelope.
func body(env network.Envelope) string {
	switch env.EventType {
	case network.EnvText:
		return env.Text()
	case network.EnvPacket:
		var p struct {
			Body string `json:"body"`
		}
		_ = env.Decode(&p)
		return p.Body
	default:
		return ""
	}
}

// countTurns counts the substantive envelopes — exactly one per workflow turn.
func countTurns(hub *network.Hub, channelID string) int {
	log, err := hub.ReadWAL(channelID)
	if err != nil {
		return 0
	}
	n := 0
	for _, env := range log {
		if env.EventType == network.EnvText || env.EventType == network.EnvPacket {
			n++
		}
	}
	return n
}

// nameOf resolves a participant id to its passport name, falling back to a
// truncated raw id when the registration is gone.
func nameOf(hub *network.Hub, id string) string {
	if id == "" {
		return ""
	}
	if p, err := hub.Passport(id); err == nil {
		return p.Name
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
