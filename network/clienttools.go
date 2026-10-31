package network

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/task"
	"github.com/luckystrker/ago/tools"
)

// Client tools port network/client/tools (peers, channels, tasks, context,
// delegate): the LLM-facing surface over a HubClient, bound to one
// AgentClient (the calling agent). Python resolves the per-call client via
// the AgentClientInject dependency; AGO-DEVIATION: AGo binds by closure —
// ClientTools constructs the tools against one (client, self) pair, which
// is the same shape one notify handler produces per agent. Passing a
// *agocontext.Context through tools.NewFunctionTool still works when a
// tenant prefers context-based DI.
//
// Every tool returns a string result: the JSON-ish text the model reads.
// Errors are reported as "Error: ..." strings (Python parity) — a failed
// tool result is data for the model's next turn, not an executor failure.

// ErrNoActiveTask is reported by the tasks tool when progress/complete run
// without an active agent.task block on the context.
var ErrNoActiveTask = errors.New("network: no active task on the context")

// contextDelegationDepth is the delegation depth stamped onto a delegate
// prompt. Python reads the outer handler's depth + 1; the AGo client
// carries no per-turn depth, so the consult always rides one hop deep.
const contextDelegationDepth = 1

// clientTimeoutCapSeconds is the ceiling on the LLM-requested timeout of the
// blocking client tools (tasks wait, delegate). The timeout arrives in
// model-controlled arguments; without a ceiling one call parks the agent for
// hours. It is a variable only so tests can lower it without waiting a real
// quarter hour.
// AGO-DEVIATION: Python AG2 honours any requested timeout; AGo caps it.
var clientTimeoutCapSeconds = 900.0

// clampTimeout defaults a missing timeout (300s) and enforces
// clientTimeoutCapSeconds.
func clampTimeout(secs float64) float64 {
	if secs <= 0 {
		return 300
	}
	if secs > clientTimeoutCapSeconds {
		return clientTimeoutCapSeconds
	}
	return secs
}

// ClientTools builds the five LLM tools over the hub for the calling
// agent. Install them on an agent the usual way (agent.WithTools /
// spec tool pool).
func ClientTools(hc *HubClient, self *AgentClient) ([]tools.Tool, error) {
	if hc == nil || self == nil {
		return nil, fmt.Errorf("%w: clienttools require a hub client and the calling agent", ErrProtocol)
	}
	peers, err := tools.NewFunctionTool("peers",
		"Discover and describe peers.\n"+
			"find:     args query?, capability?, limit → matching peer summaries\n"+
			"describe: args name (or agent_id) → passport + capability claim",
		self.peersFindDescribe(hc),
		tools.WithSource("network:peers"))
	if err != nil {
		return nil, err
	}
	channels, err := tools.NewFunctionTool("channels",
		"Channel lifecycle.\n"+
			"list:  args state=\"active\"|\"all\"\n"+
			"open:  args type, target (peer name or id), knobs?, message? — message seeds\n"+
			"       the first envelope after the channel opens\n"+
			"info:  args channel_id\n"+
			"close: args channel_id",
		self.channelsLifecycle(hc),
		tools.WithSource("network:channels"))
	if err != nil {
		return nil, err
	}
	tasks, err := tools.NewFunctionTool("tasks",
		fmt.Sprintf("Task lifecycle.\n"+
			"progress: args payload — requires an active task (started with task.Start)\n"+
			"complete: args result? — requires an active task\n"+
			"list:     args scope=\"own\"|\"all\", state=\"active\"|\"all\", limit\n"+
			"status:   args task_id\n"+
			"wait:     args task_id, timeout (seconds, default 300, capped at %gs)\n"+
			"cancel:   args task_id, reason? — posts a cancel_request the owner may honour",
			clientTimeoutCapSeconds),
		self.tasksLifecycle(hc),
		tools.WithSource("network:tasks"))
	if err != nil {
		return nil, err
	}
	context, err := tools.NewFunctionTool("context",
		"Read from past content.\n"+
			"search: args query, limit — case-insensitive substring over the channel WAL\n"+
			"quote:  args speaker (name or id), recent_n=1, channel_id? — last envelopes a speaker posted",
		self.contextRead(hc),
		tools.WithSource("network:context"))
	if err != nil {
		return nil, err
	}
	delegate, err := tools.NewFunctionTool("delegate",
		fmt.Sprintf("Open a one-shot consulting channel with target and return its reply.\n"+
			"target: peer name (or agent_id) to consult\n"+
			"prompt: the question or request to send\n"+
			"capability: optional capability tag, recorded as a channel knob\n"+
			"timeout: max seconds to wait for the reply (default 300, capped at %gs)",
			clientTimeoutCapSeconds),
		self.delegateConsult(hc),
		tools.WithSource("network:delegate"))
	if err != nil {
		return nil, err
	}
	return []tools.Tool{peers, channels, tasks, context, delegate}, nil
}

// --- peers --------------------------------------------------------------------

type peersArgs struct {
	Action     string `json:"action"`
	Query      string `json:"query,omitempty"`
	Capability string `json:"capability,omitempty"`
	Name       string `json:"name,omitempty"`
	Limit      int    `json:"limit,omitempty"`
}

func (a *AgentClient) peersFindDescribe(hc *HubClient) func(peersArgs) (any, error) {
	return func(args peersArgs) (any, error) {
		hub := hc.Hub()
		switch args.Action {
		case "find":
			limit := args.Limit
			if limit <= 0 {
				limit = 20
			}
			var out []map[string]any
			for _, p := range hub.Registrations() {
				if p.AgentID == a.AgentID() {
					continue // don't include the calling agent
				}
				if args.Query != "" && !strings.Contains(strings.ToLower(p.Name), strings.ToLower(args.Query)) {
					continue
				}
				resume, _ := hub.ResumeOf(p.AgentID)
				if args.Capability != "" && !resumeHasCapability(resume, args.Capability) {
					continue
				}
				out = append(out, map[string]any{
					"name":         p.Name,
					"agent_id":     p.AgentID,
					"capabilities": resume.Capabilities,
				})
				if len(out) >= limit {
					break
				}
			}
			if out == nil {
				out = []map[string]any{}
			}
			return out, nil
		case "describe":
			if args.Name == "" {
				return "Error: describe requires `name` (or agent_id)", nil
			}
			p, err := hub.Passport(args.Name)
			if err != nil {
				return fmt.Sprintf("Error: peer %q not found", args.Name), nil
			}
			resume, _ := hub.ResumeOf(p.AgentID)
			return map[string]any{
				"passport": map[string]any{
					"name":     p.Name,
					"agent_id": p.AgentID,
					"kind":     string(p.EffectiveKind()),
				},
				"resume": map[string]any{
					"capabilities": resume.Capabilities,
				},
			}, nil
		default:
			return fmt.Sprintf("Error: unknown action %q; choose from find, describe", args.Action), nil
		}
	}
}

func resumeHasCapability(r Resume, capability string) bool {
	for _, c := range r.Capabilities {
		if strings.EqualFold(c, capability) {
			return true
		}
	}
	return false
}

// --- channels -----------------------------------------------------------------

type channelsArgs struct {
	Action    string         `json:"action"`
	Type      string         `json:"type,omitempty"`
	Target    string         `json:"target,omitempty"`
	Knobs     map[string]any `json:"knobs,omitempty"`
	Message   string         `json:"message,omitempty"`
	ChannelID string         `json:"channel_id,omitempty"`
	State     string         `json:"state,omitempty"`
}

func (a *AgentClient) channelsLifecycle(hc *HubClient) func(channelsArgs) (any, error) {
	return func(args channelsArgs) (any, error) {
		hub := hc.Hub()
		switch args.Action {
		case "list":
			includeTerminal := args.State == "all"
			var out []map[string]any
			for _, ch := range hub.ListChannels() {
				if !ch.hasParticipant(a.AgentID()) {
					continue
				}
				if !includeTerminal && ch.State.Terminal() {
					continue
				}
				out = append(out, map[string]any{
					"channel_id":   ch.ID,
					"type":         ch.Manifest.Type,
					"state":        string(ch.State),
					"participants": ch.ParticipantIDs(),
				})
			}
			if out == nil {
				out = []map[string]any{}
			}
			return out, nil
		case "open":
			if args.Type == "" || args.Target == "" {
				return "Error: open requires `type` and `target`", nil
			}
			target, err := hub.Passport(args.Target)
			if err != nil {
				return fmt.Sprintf("Error: target %q not found", args.Target), nil
			}
			manifest := ChannelManifest{Type: args.Type, Knobs: args.Knobs}
			ch, err := a.CreateChannel(manifest, target.AgentID)
			if err != nil {
				return fmt.Sprintf("Error: open failed: %v", err), nil
			}
			result := map[string]any{
				"channel_id":   ch.ID,
				"type":         args.Type,
				"participants": ch.ParticipantIDs(),
			}
			if args.Message != "" {
				if err := a.Say(ch.ID, args.Message); err != nil {
					// Rollback: a failed seed must not leave a dangling-open
					// channel no one ever sends into (Python parity).
					_ = hub.CloseChannel(ch.ID, "seed_failed")
					return fmt.Sprintf("Error: seed send failed: %v", err), nil
				}
			}
			return result, nil
		case "info":
			if args.ChannelID == "" {
				return "Error: info requires `channel_id`", nil
			}
			ch, err := hub.Channel(args.ChannelID)
			if err != nil {
				return fmt.Sprintf("Error: channel %q not found", args.ChannelID), nil
			}
			if !ch.hasParticipant(a.AgentID()) {
				return fmt.Sprintf("Error: not a participant of channel %q", args.ChannelID), nil
			}
			return channelInfoMap(ch), nil
		case "close":
			id := args.ChannelID
			if id == "" {
				return "Error: close requires `channel_id`", nil
			}
			ch, err := hub.Channel(id)
			if err != nil {
				return fmt.Sprintf("Error: channel %q not found", id), nil
			}
			// Closing is as privileged as info — a channel the caller does
			// not sit in is not theirs to end (audit M-8).
			//
			// AGO-DEVIATION: Python's channels close closes any channel id;
			// AGo gates it on participation like info.
			if !ch.hasParticipant(a.AgentID()) {
				return fmt.Sprintf("Error: not a participant of channel %q", id), nil
			}
			if err := hub.CloseChannel(id, "closed_by_agent"); err != nil {
				return fmt.Sprintf("Error: close failed: %v", err), nil
			}
			state := ""
			if ch, err := hub.Channel(id); err == nil {
				state = string(ch.State)
			}
			return map[string]any{"channel_id": id, "state": state, "close_reason": "closed_by_agent"}, nil
		default:
			return fmt.Sprintf("Error: unknown action %q; choose from list, open, info, close", args.Action), nil
		}
	}
}

func channelInfoMap(ch *ChannelHandle) map[string]any {
	participants := make([]map[string]any, len(ch.Participants))
	for i, p := range ch.Participants {
		participants[i] = map[string]any{
			"agent_id": p.AgentID,
			"role":     string(p.Role),
			"order":    p.Order,
		}
	}
	expectations := make([]map[string]any, len(ch.Manifest.Expectations))
	for i, e := range ch.Manifest.Expectations {
		expectations[i] = map[string]any{
			"kind":         e.Kind,
			"on_violation": e.OnViolation,
			"params":       e.Params,
		}
	}
	return map[string]any{
		"channel_id":   ch.ID,
		"type":         ch.Manifest.Type,
		"version":      ch.Manifest.Version,
		"state":        string(ch.State),
		"creator_id":   ch.CreatorID,
		"participants": participants,
		"knobs":        ch.Manifest.Knobs,
		"expectations": expectations,
		"created_at":   ch.CreatedAt,
		"expires_at":   ch.ExpiresAt,
		"closed_at":    ch.ClosedAt,
		"close_reason": ch.CloseReason,
	}
}

// --- tasks --------------------------------------------------------------------

type tasksArgs struct {
	Action  string         `json:"action"`
	Payload map[string]any `json:"payload,omitempty"`
	Result  any            `json:"result,omitempty"`
	TaskID  string         `json:"task_id,omitempty"`
	Reason  string         `json:"reason,omitempty"`
	Scope   string         `json:"scope,omitempty"`
	State   string         `json:"state,omitempty"`
	Timeout float64        `json:"timeout,omitempty"`
	Limit   int            `json:"limit,omitempty"`
}

func (a *AgentClient) tasksLifecycle(hc *HubClient) func(tasksArgs, *agocontext.Context) (any, error) {
	return func(args tasksArgs, cx *agocontext.Context) (any, error) {
		hub := hc.Hub()
		switch args.Action {
		case "progress", "complete":
			// The active task comes from the context dependency the task
			// package stamps (task.DepKey); there is no TaskInject analogue
			// beyond it.
			if cx == nil {
				return "Error: " + args.Action + " requires an active task", nil
			}
			dep, ok := cx.Dependency(task.DepKey)
			if !ok {
				return "Error: " + args.Action + " requires an active `task.Start(...)` block", nil
			}
			active, ok := dep.(*task.Task)
			if !ok {
				return "Error: " + args.Action + " requires an active `task.Start(...)` block", nil
			}
			if args.Action == "progress" {
				if err := active.Progress(args.Payload); err != nil {
					return fmt.Sprintf("Error: progress failed: %v", err), nil
				}
				return fmt.Sprintf("progress emitted on %s", active.TaskID()), nil
			}
			if err := active.Complete(args.Result); err != nil {
				return fmt.Sprintf("Error: complete failed: %v", err), nil
			}
			return fmt.Sprintf("completed %s", active.TaskID()), nil
		case "list":
			owner := ""
			if args.Scope != "all" {
				owner = a.AgentID()
			}
			limit := args.Limit
			if limit <= 0 {
				limit = 20
			}
			metas := hub.Tasks(owner, args.State != "all")
			if owner == "" {
				metas = participantTasks(hub, metas, a.AgentID())
			}
			if len(metas) > limit {
				metas = metas[:limit]
			}
			out := make([]map[string]any, 0, len(metas))
			for _, md := range metas {
				out = append(out, taskSummary(md))
			}
			return out, nil
		case "status":
			if args.TaskID == "" {
				return "Error: status requires `task_id`", nil
			}
			md, err := hub.Task(args.TaskID)
			if err != nil {
				return fmt.Sprintf("Error: task %q not found", args.TaskID), nil
			}
			return taskSummary(md), nil
		case "wait":
			if args.TaskID == "" {
				return "Error: wait requires `task_id`", nil
			}
			timeout := clampTimeout(args.Timeout)
			deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
			for {
				md, err := hub.Task(args.TaskID)
				if err != nil {
					return fmt.Sprintf("Error: task %q not found", args.TaskID), nil
				}
				if md.State.Terminal() {
					return taskSummary(md), nil
				}
				if !time.Now().Before(deadline) {
					return fmt.Sprintf("Error: task %q did not complete within %gs", args.TaskID, timeout), nil
				}
				time.Sleep(100 * time.Millisecond)
			}
		case "cancel":
			if args.TaskID == "" {
				return "Error: cancel requires `task_id`", nil
			}
			md, err := hub.Task(args.TaskID)
			if err != nil {
				return fmt.Sprintf("Error: task %q not found", args.TaskID), nil
			}
			if md.State.Terminal() {
				return fmt.Sprintf("task %q is already %s", args.TaskID, md.State), nil
			}
			if md.ChannelID == "" {
				return fmt.Sprintf("Error: task %q has no associated channel; cancel_request envelopes need a channel to ride on", args.TaskID), nil
			}
			payload := mustJSON(map[string]any{"task_id": args.TaskID, "reason": args.Reason})
			env := Envelope{
				ChannelID: md.ChannelID,
				SenderID:  a.AgentID(),
				Audience:  md.OwnerID,
				EventType: EnvTaskCancelRequest,
				Payload:   payload,
			}
			if _, err := hub.Send(env); err != nil {
				return fmt.Sprintf("Error: cancel_request post failed: %v", err), nil
			}
			return fmt.Sprintf("cancel_request posted to %s for task %s", md.OwnerID, args.TaskID), nil
		default:
			return fmt.Sprintf("Error: unknown action %q; choose from progress, complete, list, status, wait, cancel", args.Action), nil
		}
	}
}

func taskSummary(md task.Metadata) map[string]any {
	return map[string]any{
		"task_id":    md.TaskID,
		"owner_id":   md.OwnerID,
		"title":      md.Spec.Title,
		"capability": md.Spec.Capability,
		"state":      string(md.State),
		"result":     md.Result,
		"error":      md.Error,
		"channel_id": md.ChannelID,
	}
}

// participantTasks narrows a hub-wide task listing (tasks list scope=all) to
// the tasks the caller may legitimately see: their own, plus those riding a
// channel they participate in. Metadata of strangers' tasks — titles,
// payloads of results — is not directory information (audit M-8).
//
// AGO-DEVIATION: Python's tasks list scope=all passes no owner filter and
// returns every task the hub mirrors; AGo gates it on ownership or channel
// participation.
func participantTasks(hub *Hub, metas []task.Metadata, agentID string) []task.Metadata {
	out := make([]task.Metadata, 0, len(metas))
	for _, md := range metas {
		if md.OwnerID == agentID {
			out = append(out, md)
			continue
		}
		if md.ChannelID == "" {
			continue
		}
		if ch, err := hub.Channel(md.ChannelID); err == nil && ch.hasParticipant(agentID) {
			out = append(out, md)
		}
	}
	return out
}

// --- context ------------------------------------------------------------------

type contextArgs struct {
	Action    string `json:"action"`
	Query     string `json:"query,omitempty"`
	Scope     string `json:"scope,omitempty"`
	Speaker   string `json:"speaker,omitempty"`
	RecentN   int    `json:"recent_n,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
}

const contextExcerptChars = 240

func (a *AgentClient) contextRead(hc *HubClient) func(contextArgs) (any, error) {
	return func(args contextArgs) (any, error) {
		hub := hc.Hub()
		switch args.Action {
		case "search":
			if args.Query == "" {
				return "Error: search requires `query`", nil
			}
			if args.ChannelID == "" {
				return "Error: search scope=channel requires `channel_id`", nil
			}
			wal, err := hub.ReadWAL(args.ChannelID)
			if err != nil {
				return fmt.Sprintf("Error: channel %q not found", args.ChannelID), nil
			}
			limit := args.Limit
			if limit <= 0 {
				limit = 10
			}
			needle := strings.ToLower(args.Query)
			var out []map[string]any
			for _, env := range wal {
				if env.EventType != EnvText || !VisibleTo(env, a.AgentID()) {
					continue
				}
				text := env.Text()
				if !strings.Contains(strings.ToLower(text), needle) {
					continue
				}
				out = append(out, map[string]any{
					"envelope_id": env.ID,
					"sender_id":   env.SenderID,
					"when":        env.CreatedAt,
					"excerpt":     excerpt(text, contextExcerptChars),
				})
				if len(out) >= limit {
					break
				}
			}
			if out == nil {
				out = []map[string]any{}
			}
			return out, nil
		case "quote":
			if args.Speaker == "" {
				return "Error: quote requires `speaker`", nil
			}
			if args.ChannelID == "" {
				return "Error: quote requires `channel_id`", nil
			}
			speaker, err := hub.Passport(args.Speaker)
			if err != nil {
				return fmt.Sprintf("Error: speaker %q not found", args.Speaker), nil
			}
			wal, err := hub.ReadWAL(args.ChannelID)
			if err != nil {
				return fmt.Sprintf("Error: channel %q not found", args.ChannelID), nil
			}
			recentN := args.RecentN
			if recentN <= 0 {
				recentN = 1
			}
			var picks []map[string]any
			for i := len(wal) - 1; i >= 0 && len(picks) < recentN; i-- {
				env := wal[i]
				if env.EventType != EnvText || env.SenderID != speaker.AgentID {
					continue
				}
				if !VisibleTo(env, a.AgentID()) {
					continue
				}
				picks = append(picks, map[string]any{
					"envelope_id": env.ID,
					"when":        env.CreatedAt,
					"text":        env.Text(),
				})
			}
			// Reverse back into chronological order.
			for i, j := 0, len(picks)-1; i < j; i, j = i+1, j-1 {
				picks[i], picks[j] = picks[j], picks[i]
			}
			if picks == nil {
				picks = []map[string]any{}
			}
			return picks, nil
		default:
			return fmt.Sprintf("Error: unknown action %q; choose from search, quote", args.Action), nil
		}
	}
}

// excerpt truncates text to maxChars with an ellipsis.
func excerpt(text string, maxChars int) string {
	if len(text) <= maxChars {
		return text
	}
	return text[:maxChars-1] + "…"
}

// --- delegate -----------------------------------------------------------------

type delegateArgs struct {
	Target     string  `json:"target"`
	Prompt     string  `json:"prompt"`
	Capability string  `json:"capability,omitempty"`
	Timeout    float64 `json:"timeout,omitempty"`
}

// functionsNamespacePrefix is echoed by models that mistake the OpenAI
// tool namespace for part of the target name (Python parity).
const functionsNamespacePrefix = "functions."

// The delegate reply is observed by re-reading the channel WAL, which copies
// the whole accepted log per poll; the interval therefore grows instead of
// re-copying the WAL 50 times a second for the full wait. delegatePollStart
// keeps a fresh consult snappy, delegatePollMax bounds reply latency.
const (
	delegatePollStart = 20 * time.Millisecond
	delegatePollMax   = 500 * time.Millisecond
)

// nextPollInterval doubles the poll delay up to delegatePollMax.
func nextPollInterval(d time.Duration) time.Duration {
	if next := d * 2; next < delegatePollMax {
		return next
	}
	return delegatePollMax
}

// delegateCandidates lists target resolution candidates, most literal
// first (Python parity: _target_candidates).
func delegateCandidates(target string) []string {
	if strings.HasPrefix(target, functionsNamespacePrefix) && len(target) > len(functionsNamespacePrefix) {
		return []string{target, target[len(functionsNamespacePrefix):]}
	}
	return []string{target}
}

func (a *AgentClient) delegateConsult(hc *HubClient) func(delegateArgs) (any, error) {
	return func(args delegateArgs) (any, error) {
		hub := hc.Hub()
		if args.Target == "" || args.Prompt == "" {
			return "Error: delegate requires `target` and `prompt`", nil
		}
		// Resolve the target; a miss lists the live directory so the next
		// turn picks a real name instead of re-guessing (Python parity).
		var target Passport
		found := false
		for _, candidate := range delegateCandidates(args.Target) {
			p, err := hub.Passport(candidate)
			if err != nil {
				continue
			}
			target, found = p, true
			break
		}
		if !found {
			var names []string
			for _, p := range hub.Registrations() {
				if p.AgentID != a.AgentID() {
					names = append(names, p.Name)
				}
			}
			sort.Strings(names)
			if len(names) > 0 {
				return fmt.Sprintf("Error: target %q not found. Available targets: [%s]", args.Target, strings.Join(names, ", ")), nil
			}
			return fmt.Sprintf("Error: target %q not found", args.Target), nil
		}
		if target.AgentID == a.AgentID() {
			return fmt.Sprintf("Error: cannot delegate to self (target %q is this agent)", args.Target), nil
		}

		// Open the consulting channel — the handshake is awaited inside
		// CreateChannel.
		var knobs map[string]any
		if args.Capability != "" {
			knobs = map[string]any{"capability": args.Capability}
		}
		ch, err := a.CreateChannel(ChannelManifest{Type: TypeConsulting, Knobs: knobs}, target.AgentID)
		if err != nil {
			return fmt.Sprintf("Error: failed to open consulting channel: %v", err), nil
		}

		// This tool owns the channel's lifecycle: the local handler must
		// not also run a turn when the reply lands. Every exit path below
		// resumes the handler before returning.
		a.SuppressHandler(ch.ID)

		prompt := NewTextEnvelope(ch.ID, a.AgentID(), target.AgentID, args.Prompt)
		prompt.Depth = contextDelegationDepth
		if _, err := hub.Send(prompt); err != nil {
			a.ResumeHandler(ch.ID)
			_ = hub.CloseChannel(ch.ID, "prompt_send_failed")
			return fmt.Sprintf("Error: prompt send failed: %v", err), nil
		}

		timeout := clampTimeout(args.Timeout)
		deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
		reply := ""
		timedOut := true
		channelClosed := ""
		poll := delegatePollStart
		for time.Now().Before(deadline) {
			wal, err := hub.ReadWAL(ch.ID)
			if err != nil {
				a.ResumeHandler(ch.ID)
				return fmt.Sprintf("Error: delegate to %q failed: %v", args.Target, err), nil
			}
			for _, env := range wal {
				switch {
				case env.EventType == EnvText && env.SenderID == target.AgentID:
					reply, timedOut = env.Text(), false
				case env.EventType == EnvChannelClosed || env.EventType == EnvChannelExpired || env.EventType == EnvChannelReject:
					var p struct {
						Reason string `json:"reason"`
					}
					_ = env.Decode(&p)
					if p.Reason == "" {
						p.Reason = env.EventType
					}
					channelClosed = p.Reason
				}
			}
			if reply != "" || channelClosed != "" {
				break
			}
			time.Sleep(poll)
			poll = nextPollInterval(poll)
		}
		a.ResumeHandler(ch.ID)

		switch {
		case channelClosed != "" && reply == "":
			return fmt.Sprintf("Error: delegate to %q channel closed: %s", args.Target, channelClosed), nil
		case timedOut:
			_ = hub.CloseChannel(ch.ID, "delegate_timeout")
			return fmt.Sprintf("Error: delegate to %q timed out after %gs", args.Target, timeout), nil
		}
		return reply, nil
	}
}
