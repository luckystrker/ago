package network

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/luckystrker/ago/knowledge"
)

// --- store layout ------------------------------------------------------------------
//
// The hub persists through a knowledge.Store under Python's per-entity layout
// (ag2/network/hub/layout.py). Paths are Unix-style with a leading "/".
//
//	/agents/<id>/passport.json         commit point of a registration
//	/agents/<id>/rule.json
//	/agents/<id>/resume.json           claimed capabilities + observed track record
//	/agents/<id>/inbox.cursors.json    {channel_id: last-acked envelope id}
//	/agents/<id>/inbox_nacks.jsonl     one nack per line, append-only
//	/registry/by_capability.json       derived cache of the capability index
//	/channels/<id>/metadata.json       latest channel snapshot
//	/channels/<id>/wal.jsonl           accepted envelopes, accept order
//	/tasks/<id>/checkpoint.json        task checkpoint + claimed writer
//
// Python's per-agent SKILL.md and owners.json have no AGo counterpart (no
// skill registry, no wire-registered remote owners), so they are not written.

func agentDir(agentID string) string        { return "/agents/" + agentID }
func passportPath(agentID string) string    { return agentDir(agentID) + "/passport.json" }
func rulePath(agentID string) string        { return agentDir(agentID) + "/rule.json" }
func resumePath(agentID string) string      { return agentDir(agentID) + "/resume.json" }
func inboxCursorPath(agentID string) string { return agentDir(agentID) + "/inbox.cursors.json" }
func inboxNacksPath(agentID string) string  { return agentDir(agentID) + "/inbox_nacks.jsonl" }

const (
	agentsRoot          = "/agents"
	channelsRoot        = "/channels"
	tasksRoot           = "/tasks"
	byCapabilityPath    = "/registry/by_capability.json"
	channelMetadataName = "metadata.json"
	channelWALName      = "wal.jsonl"
	taskCheckpointName  = "checkpoint.json"
)

func channelMetadataPath(channelID string) string {
	return channelsRoot + "/" + channelID + "/" + channelMetadataName
}
func channelWALPath(channelID string) string {
	return channelsRoot + "/" + channelID + "/" + channelWALName
}
func taskCheckpointPath(taskID string) string {
	return tasksRoot + "/" + taskID + "/" + taskCheckpointName
}

// --- StoreWAL ----------------------------------------------------------------------

// StoreWAL is the hub WAL over a knowledge.Store in Python's layout: a
// channel snapshot overwrites /channels/<id>/metadata.json, an accepted
// envelope appends one JSON line to /channels/<id>/wal.jsonl, a checkpoint
// overwrites /tasks/<id>/checkpoint.json. ReadAll yields, per channel
// (sorted by id), the latest snapshot followed by its envelopes in accept
// order, then the checkpoints.
//
// AGO-DEVIATION: the history of channel snapshots is not kept (Python
// overwrites metadata.json too), so Replay sees only the final lifecycle
// state of a channel; the adapter fold therefore also runs for envelopes of a
// channel that ended terminal. Safe for concurrent use.
type StoreWAL struct {
	mu    sync.Mutex
	store knowledge.Store
}

// NewStoreWAL binds a WAL to store.
func NewStoreWAL(store knowledge.Store) *StoreWAL { return &StoreWAL{store: store} }

// Append implements WAL.
func (w *StoreWAL) Append(rec WALRecord) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch rec.Kind {
	case "channel":
		if rec.Channel == nil {
			return fmt.Errorf("%w: channel record missing snapshot", ErrProtocol)
		}
		data, err := json.Marshal(rec.Channel)
		if err != nil {
			return err
		}
		return w.store.Write(channelMetadataPath(rec.Channel.ID), data)
	case "envelope":
		if rec.Envelope == nil {
			return fmt.Errorf("%w: envelope record missing envelope", ErrProtocol)
		}
		line, err := json.Marshal(rec.Envelope)
		if err != nil {
			return err
		}
		return w.store.Append(channelWALPath(rec.Envelope.ChannelID), append(line, '\n'))
	case "checkpoint":
		if rec.Checkpoint == nil {
			return fmt.Errorf("%w: checkpoint record missing payload", ErrProtocol)
		}
		data, err := json.Marshal(rec.Checkpoint)
		if err != nil {
			return err
		}
		return w.store.Write(taskCheckpointPath(rec.Checkpoint.TaskID), data)
	}
	return fmt.Errorf("%w: unknown WAL record kind %q", ErrProtocol, rec.Kind)
}

// ReadAll implements WAL.
func (w *StoreWAL) ReadAll() ([]WALRecord, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []WALRecord
	channels, err := w.store.List(channelsRoot)
	if err != nil {
		return nil, err
	}
	for _, child := range channels {
		id, isDir := strings.CutSuffix(child, "/")
		if !isDir {
			continue
		}
		raw, err := w.store.Read(channelMetadataPath(id))
		if errors.Is(err, knowledge.ErrNotFound) {
			continue // no snapshot: not a channel (Python skips it too)
		}
		if err != nil {
			return nil, err
		}
		var snap ChannelSnapshot
		if err := json.Unmarshal(raw, &snap); err != nil {
			return nil, fmt.Errorf("wal: channel %s metadata: %w", id, err)
		}
		out = append(out, WALRecord{Kind: "channel", Channel: &snap})
		body, err := w.store.Read(channelWALPath(id))
		if errors.Is(err, knowledge.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, line := range bytes.Split(body, []byte{'\n'}) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var env Envelope
			if err := json.Unmarshal(line, &env); err != nil {
				return nil, fmt.Errorf("wal: channel %s envelope: %w", id, err)
			}
			out = append(out, WALRecord{Kind: "envelope", Envelope: &env})
		}
	}
	tasks, err := w.store.List(tasksRoot)
	if err != nil {
		return nil, err
	}
	for _, child := range tasks {
		id, isDir := strings.CutSuffix(child, "/")
		if !isDir {
			continue
		}
		raw, err := w.store.Read(taskCheckpointPath(id))
		if errors.Is(err, knowledge.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var cp CheckpointRecord
		if err := json.Unmarshal(raw, &cp); err != nil {
			return nil, fmt.Errorf("wal: task %s checkpoint: %w", id, err)
		}
		out = append(out, WALRecord{Kind: "checkpoint", Checkpoint: &cp})
	}
	return out, nil
}

var _ WAL = (*StoreWAL)(nil)

// --- hub options ---------------------------------------------------------------------

// WithHubStore makes the hub's registry durable: passports, rules, resumes
// (incl. the observed track record), the capability index, inbox cursors and
// nacks persist through store in Python's per-entity layout, and a hub built
// without WithHubWAL logs channels and envelopes into the same store
// (StoreWAL). Hydrate rebuilds the hub from the store after a restart. Every
// write persists first and updates memory second, so memory is never ahead
// of the store; a failed write fails the operation and leaves memory
// untouched.
func WithHubStore(store knowledge.Store) HubOption {
	return func(h *Hub) { h.store = store }
}

// --- inbox cursors and nacks -----------------------------------------------------------

// Nack is one recorded negative delivery receipt. A nack never moves the
// inbox cursor: the envelope stays eligible for replay.
type Nack struct {
	ChannelID  string    `json:"channel_id"`
	EnvelopeID string    `json:"envelope_id"`
	Reason     string    `json:"reason,omitempty"`
	At         time.Time `json:"at,omitzero"`
}

// AdvanceInboxCursor moves agentID's cursor for channelID forward to
// envelopeID when it sorts strictly above the current value (ids sort in
// accept order); an older or equal id, or an empty argument, is a no-op.
// The agent's whole cursor map is persisted before memory changes, so the
// cursor never decreases across a restart and an ack in one channel never
// touches another channel's cursor. ErrNotFound for an unregistered agent.
func (h *Hub) AdvanceInboxCursor(agentID, channelID, envelopeID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.passports[agentID]; !ok {
		return fmt.Errorf("%w: agent %s", ErrNotFound, agentID)
	}
	if channelID == "" || envelopeID == "" || envelopeID <= h.ext.cursors[agentID][channelID] {
		return nil
	}
	next := make(map[string]string, len(h.ext.cursors[agentID])+1)
	for c, e := range h.ext.cursors[agentID] {
		next[c] = e
	}
	next[channelID] = envelopeID
	// AGO-DEVIATION: Python (and HubRestart.tla's Ack) update memory first and
	// persist second; here the store is written first and memory only after it
	// succeeds. Safe for invariant 5: memory is never ahead of the store (so
	// MemoryEqualsPersisted holds between the two steps and a failed write
	// leaves the live hub exactly as a restart would see it); a crash between
	// the steps loses only memory, and the restart reads the newer durable
	// cursor; the durable cursor still only moves forward
	// (PersistedCursorMonotone). The same order is used for every registry
	// write here.
	if h.store != nil {
		data, err := json.Marshal(next) // sorted keys: deterministic file
		if err != nil {
			return err
		}
		if err := h.store.Write(inboxCursorPath(agentID), data); err != nil {
			return fmt.Errorf("network: persist inbox cursor of %s: %w", agentID, err)
		}
	}
	h.ext.cursors[agentID] = next
	return nil
}

// InboxCursor returns the last envelope id agentID acked in channelID ("" =
// nothing acked, or unknown agent).
func (h *Hub) InboxCursor(agentID, channelID string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ext.cursors[agentID][channelID]
}

// RecordNack appends a negative receipt to agentID's nack log. The cursor is
// left where it is. ErrNotFound for an unregistered agent.
func (h *Hub) RecordNack(agentID, channelID, envelopeID, reason string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.passports[agentID]; !ok {
		return fmt.Errorf("%w: agent %s", ErrNotFound, agentID)
	}
	n := Nack{ChannelID: channelID, EnvelopeID: envelopeID, Reason: reason, At: h.now()}
	if h.store != nil {
		line, err := json.Marshal(n)
		if err != nil {
			return err
		}
		if err := h.store.Append(inboxNacksPath(agentID), append(line, '\n')); err != nil {
			return fmt.Errorf("network: persist nack of %s: %w", agentID, err)
		}
	}
	h.ext.nacks[agentID] = append(h.ext.nacks[agentID], n)
	return nil
}

// Nacks returns a copy of agentID's nack log in arrival order.
func (h *Hub) Nacks(agentID string) []Nack {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Nack(nil), h.ext.nacks[agentID]...)
}

// --- write helpers (callers hold h.mu) ---------------------------------------------------

// persistRegistrationLocked writes a new registration: rule first, passport
// last. The passport file is the commit point — hydrate ignores (and deletes)
// an agent directory without one — so a crash anywhere in between leaves
// either no registration or a complete one.
func (h *Hub) persistRegistrationLocked(p Passport, r Rule) error {
	if h.store == nil {
		return nil
	}
	rule, err := json.Marshal(r)
	if err != nil {
		return err
	}
	pass, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := h.store.Write(rulePath(p.AgentID), rule); err != nil {
		return fmt.Errorf("network: persist rule of %s: %w", p.Name, err)
	}
	if err := h.store.Write(passportPath(p.AgentID), pass); err != nil {
		_ = h.store.Delete(agentDir(p.AgentID)) // best effort; hydrate collects leftovers
		return fmt.Errorf("network: persist passport of %s: %w", p.Name, err)
	}
	return nil
}

// persistRuleLocked overwrites the agent's rule.json.
func (h *Hub) persistRuleLocked(agentID string, r Rule) error {
	if h.store == nil {
		return nil
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := h.store.Write(rulePath(agentID), data); err != nil {
		return fmt.Errorf("network: persist rule of %s: %w", agentID, err)
	}
	return nil
}

// persistResumeLocked overwrites the agent's resume.json.
func (h *Hub) persistResumeLocked(agentID string, r Resume) error {
	if h.store == nil {
		return nil
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := h.store.Write(resumePath(agentID), data); err != nil {
		return fmt.Errorf("network: persist resume of %s: %w", agentID, err)
	}
	return nil
}

// deleteRegistrationLocked removes an agent from the store: the passport
// first (the commit point; its failure fails the unregister), then the rest
// of the agent's directory (rule, resume, cursors, nacks) best effort —
// leftovers without a passport are never loaded and hydrate deletes them.
func (h *Hub) deleteRegistrationLocked(agentID string) error {
	if h.store == nil {
		return nil
	}
	if err := h.store.Delete(passportPath(agentID)); err != nil {
		return fmt.Errorf("network: delete passport of %s: %w", agentID, err)
	}
	_ = h.store.Delete(agentDir(agentID))
	return nil
}

// persistCapIndexLocked writes the derived capability index cache. It is
// best effort: the resumes are authoritative and hydrate rebuilds the index
// from them (Python parity), so a failed write only leaves a stale cache.
func (h *Hub) persistCapIndexLocked() {
	if h.store == nil {
		return
	}
	snap := make(map[string][]string, len(h.ext.capIndex))
	for c, ids := range h.ext.capIndex {
		list := make([]string, 0, len(ids))
		for id := range ids {
			list = append(list, id)
		}
		sort.Strings(list)
		snap[c] = list
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return
	}
	_ = h.store.Write(byCapabilityPath, data)
}

// --- hydrate -------------------------------------------------------------------------------

// Hydrate rebuilds the hub from its durable state after a restart: the
// registry from the store (passports, rules, resumes, cursors, nacks, then
// the capability index from the resumes), then channels, the in-memory
// envelope log, the causation index and checkpoints from the WAL (Replay).
// Adapters referenced by the WAL must be registered first. Idempotent: it
// rebuilds from scratch, so running it again on a live hub yields the same
// state. A hub without a store only replays its WAL.
func (h *Hub) Hydrate() error {
	if h.store != nil {
		if err := h.loadRegistry(); err != nil {
			return err
		}
	}
	return h.Replay()
}

func (h *Hub) loadRegistry() error {
	children, err := h.store.List(agentsRoot)
	if err != nil {
		return fmt.Errorf("network: hydrate: list agents: %w", err)
	}
	type loaded struct {
		p       Passport
		rule    Rule
		resume  Resume
		hasRes  bool
		cursors map[string]string
		nacks   []Nack
	}
	var agents []loaded
	for _, child := range children {
		id, isDir := strings.CutSuffix(child, "/")
		if !isDir {
			continue
		}
		raw, err := h.store.Read(passportPath(id))
		if errors.Is(err, knowledge.ErrNotFound) {
			// A registration that never committed or an unregister that was
			// interrupted after its passport delete: collect the leftovers.
			// AGO-DEVIATION: Python's hydrate skips such a directory and leaves
			// it in the store; AGo deletes it, so a stale cursor or rule can
			// never reappear (invariant 5, NoOrphanCursor). Safe: the passport is
			// the commit point, so a directory without one holds no live agent;
			// a failed delete is ignored and retried by the next hydrate.
			_ = h.store.Delete(agentDir(id))
			continue
		}
		if err != nil {
			return fmt.Errorf("network: hydrate: read passport %s: %w", id, err)
		}
		var a loaded
		if err := json.Unmarshal(raw, &a.p); err != nil {
			return fmt.Errorf("network: hydrate: passport %s: %w", id, err)
		}
		a.p.AgentID = id
		// Decode into a zero Rule: omitted fields must stay empty (a stored
		// rule with no TTL means "no expiry", not DefaultRule's 2h). Only a
		// missing file falls back to the default.
		if raw, err := h.store.Read(rulePath(id)); err == nil {
			if err := json.Unmarshal(raw, &a.rule); err != nil {
				return fmt.Errorf("network: hydrate: rule %s: %w", id, err)
			}
		} else if errors.Is(err, knowledge.ErrNotFound) {
			a.rule = DefaultRule()
		} else {
			return fmt.Errorf("network: hydrate: read rule %s: %w", id, err)
		}
		if raw, err := h.store.Read(resumePath(id)); err == nil {
			if err := json.Unmarshal(raw, &a.resume); err != nil {
				return fmt.Errorf("network: hydrate: resume %s: %w", id, err)
			}
			a.resume.AgentID = id
			a.hasRes = true
		} else if !errors.Is(err, knowledge.ErrNotFound) {
			return fmt.Errorf("network: hydrate: read resume %s: %w", id, err)
		}
		if raw, err := h.store.Read(inboxCursorPath(id)); err == nil {
			var parsed map[string]string
			if err := json.Unmarshal(raw, &parsed); err != nil {
				return fmt.Errorf("network: hydrate: cursors %s: %w", id, err)
			}
			for c, e := range parsed {
				if c != "" && e != "" {
					if a.cursors == nil {
						a.cursors = make(map[string]string)
					}
					a.cursors[c] = e
				}
			}
		} else if !errors.Is(err, knowledge.ErrNotFound) {
			return fmt.Errorf("network: hydrate: read cursors %s: %w", id, err)
		}
		if raw, err := h.store.Read(inboxNacksPath(id)); err == nil {
			for _, line := range bytes.Split(raw, []byte{'\n'}) {
				if len(bytes.TrimSpace(line)) == 0 {
					continue
				}
				var n Nack
				if err := json.Unmarshal(line, &n); err != nil {
					return fmt.Errorf("network: hydrate: nacks %s: %w", id, err)
				}
				a.nacks = append(a.nacks, n)
			}
		} else if !errors.Is(err, knowledge.ErrNotFound) {
			return fmt.Errorf("network: hydrate: read nacks %s: %w", id, err)
		}
		agents = append(agents, a)
	}
	// Registration order: creation time, then id.
	sort.Slice(agents, func(i, j int) bool {
		if !agents[i].p.CreatedAt.Equal(agents[j].p.CreatedAt) {
			return agents[i].p.CreatedAt.Before(agents[j].p.CreatedAt)
		}
		return agents[i].p.AgentID < agents[j].p.AgentID
	})

	h.mu.Lock()
	defer h.mu.Unlock()
	h.passports = make(map[string]Passport, len(agents))
	h.byName = make(map[string]string, len(agents))
	h.rules = make(map[string]Rule, len(agents))
	h.resumes = make(map[string]Resume)
	h.ext.capIndex = make(map[string]map[string]struct{})
	h.ext.regSeq = make(map[string]int, len(agents))
	h.ext.nextSeq = 0
	h.ext.cursors = make(map[string]map[string]string)
	h.ext.nacks = make(map[string][]Nack)
	for _, a := range agents {
		id := a.p.AgentID
		h.passports[id] = a.p
		h.byName[a.p.Name] = id
		h.rules[id] = a.rule
		if a.hasRes {
			h.resumes[id] = a.resume
			h.reindexLocked(id, Resume{}, a.resume)
		}
		if len(a.cursors) > 0 {
			h.ext.cursors[id] = a.cursors
		}
		if len(a.nacks) > 0 {
			h.ext.nacks[id] = a.nacks
		}
		h.ext.regSeq[id] = h.ext.nextSeq
		h.ext.nextSeq++
	}
	h.persistCapIndexLocked() // repair the derived cache after a crash
	return nil
}
