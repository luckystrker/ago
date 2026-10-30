package network

import (
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/internal/uuid"
	"github.com/luckystrker/ago/knowledge"
	"github.com/luckystrker/ago/task"
)

// Hub operation errors. Wrap with %w at the raise site; match with
// errors.Is on consumption.
var (
	ErrNotFound       = errors.New("network: not found")
	ErrAccessDenied   = errors.New("network: access denied")
	ErrProtocol       = errors.New("network: protocol violation")
	ErrDepthExceeded  = errors.New("network: delegation depth exceeded")
	ErrTTLExpired     = errors.New("network: envelope ttl expired")
	ErrChannelClosed  = errors.New("network: channel closed")
	ErrChannelExpired = errors.New("network: channel expired")
	ErrAckTimeout     = errors.New("network: invite ack timeout")
	ErrNameTaken      = errors.New("network: name already registered")
	ErrBadIdentity    = errors.New("network: invalid passport")
)

// ChannelHandle is the hub-side runtime record of one channel: lifecycle
// metadata plus the adapter's folded state. The hub mutates a handle only
// under its lock; readers outside the hub must treat every field as a
// snapshot.
type ChannelHandle struct {
	ID           string
	Manifest     ChannelManifest
	CreatorID    string
	Participants []Participant
	State        ChannelState
	CreatedAt    time.Time
	ExpiresAt    time.Time // zero = no expiry
	ClosedAt     time.Time
	CloseReason  string
	PendingAcks  []string // meaningful only while pending
	RejectedBy   []string

	// AdapterState is the adapter's fold state (whatever InitialState
	// produced). Owned by the adapter; mutated inside OnEnvelope under the
	// hub lock.
	AdapterState any

	adapter ChannelAdapter // resolved at create time; nil after a raw snapshot decode
}

// hasParticipant reports whether id is a channel member.
func (ch *ChannelHandle) hasParticipant(id string) bool {
	for _, p := range ch.Participants {
		if p.AgentID == id {
			return true
		}
	}
	return false
}

// ParticipantIDs lists member ids in join order.
func (ch *ChannelHandle) ParticipantIDs() []string {
	out := make([]string, len(ch.Participants))
	for i, p := range ch.Participants {
		out[i] = p.AgentID
	}
	return out
}

// Adapter returns the resolved adapter (nil when the handle came from a
// snapshot without the adapter registered).
func (ch *ChannelHandle) Adapter() ChannelAdapter { return ch.adapter }

// AdapterType returns the manifest's adapter dispatch key.
func (ch *ChannelHandle) AdapterType() string { return ch.Manifest.Type }

// --- WAL ---------------------------------------------------------------------
//
// AGO-DEVIATION: the brief pins knowledge.EventLogWriter as the WAL backend.
// That package landed mid-wave, so the hub speaks the small WAL interface
// below and KnowledgeWAL binds it to knowledge.EventLogWriter (records
// travel as DataInput events); MemoryWAL/FileWAL keep in-process and
// file-only use dependency-light.

// WALRecord is one WAL entry: a channel lifecycle snapshot or an accepted
// envelope.
type WALRecord struct {
	Kind     string           `json:"kind"` // "channel" | "envelope"
	Channel  *ChannelSnapshot `json:"channel,omitempty"`
	Envelope *Envelope        `json:"envelope,omitempty"`
}

// ChannelSnapshot is the persisted channel metadata (manifest snapshot,
// participants, lifecycle position).
type ChannelSnapshot struct {
	ID           string          `json:"id"`
	Manifest     ChannelManifest `json:"manifest"`
	CreatorID    string          `json:"creator_id"`
	Participants []Participant   `json:"participants"`
	State        ChannelState    `json:"state"`
	CreatedAt    time.Time       `json:"created_at"`
	ExpiresAt    time.Time       `json:"expires_at,omitempty"`
	ClosedAt     time.Time       `json:"closed_at,omitempty"`
	CloseReason  string          `json:"close_reason,omitempty"`
	PendingAcks  []string        `json:"pending_acks,omitempty"`
	RejectedBy   []string        `json:"rejected_by,omitempty"`
}

// WAL is the append-only store the hub writes every accepted envelope and
// channel transition to. Implementations must be safe for concurrent use.
type WAL interface {
	Append(rec WALRecord) error
	ReadAll() ([]WALRecord, error)
}

// MemoryWAL is an in-process WAL (tests and ephemeral hubs).
type MemoryWAL struct {
	mu   sync.Mutex
	recs []WALRecord
}

// NewMemoryWAL creates an empty in-memory WAL.
func NewMemoryWAL() *MemoryWAL { return &MemoryWAL{} }

// Append implements WAL.
func (w *MemoryWAL) Append(rec WALRecord) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.recs = append(w.recs, rec)
	return nil
}

// ReadAll implements WAL.
func (w *MemoryWAL) ReadAll() ([]WALRecord, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]WALRecord(nil), w.recs...), nil
}

// FileWAL is a JSONL-file WAL: one record per line, appended under a mutex.
type FileWAL struct {
	mu   sync.Mutex
	path string
}

// NewFileWAL creates a JSONL WAL at path (created on first append).
func NewFileWAL(path string) *FileWAL { return &FileWAL{path: path} }

// Append implements WAL.
func (w *FileWAL) Append(rec WALRecord) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// ReadAll implements WAL.
func (w *FileWAL) ReadAll() ([]WALRecord, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	data, err := os.ReadFile(w.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var recs []WALRecord
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec WALRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("wal %s: %w", w.path, err)
		}
		recs = append(recs, rec)
	}
	return recs, nil
}

// KnowledgeWAL is the hub WAL over knowledge.EventLogWriter (the brief's
// pinned backend): each record appends as a DataInput event whose Data is
// the record itself; Replay decodes it back generically after the JSON round
// trip. Appends serialize under a mutex.
type KnowledgeWAL struct {
	w        *knowledge.EventLogWriter
	streamID string
	mu       sync.Mutex
}

// NewKnowledgeWAL binds a WAL to a knowledge.EventLogWriter under one log
// stream (store path "logs/<streamID>.jsonl").
func NewKnowledgeWAL(w *knowledge.EventLogWriter, streamID string) *KnowledgeWAL {
	return &KnowledgeWAL{w: w, streamID: streamID}
}

// Append implements WAL.
func (w *KnowledgeWAL) Append(rec WALRecord) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.AppendEvent(w.streamID, events.DataInput{Data: rec})
}

// ReadAll implements WAL.
func (w *KnowledgeWAL) ReadAll() ([]WALRecord, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	evs, err := w.w.Replay(w.streamID)
	if err != nil {
		return nil, err
	}
	out := make([]WALRecord, 0, len(evs))
	for _, ev := range evs {
		input, ok := ev.(events.Input)
		if !ok {
			continue
		}
		di, isData := derefData(input)
		if !isData {
			continue
		}
		raw, err := json.Marshal(di.Data)
		if err != nil {
			return nil, fmt.Errorf("wal: decode record: %w", err)
		}
		var rec WALRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, fmt.Errorf("wal: decode record: %w", err)
		}
		out = append(out, rec)
	}
	return out, nil
}

// --- id minting --------------------------------------------------------------

// idMinter mints strictly time-ordered 32-hex ids (8-byte ns timestamp +
// 8 random bytes, the UUIDv7 monotonicity clamp). Envelope ids must sort in
// accept order even when the wall clock stalls.
type idMinter struct {
	mu   sync.Mutex
	last int64
	seq  int64
}

func (m *idMinter) Next() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ns := time.Now().UnixNano()
	if ns <= m.last {
		ns = m.last + 1
	}
	m.last = ns
	var rnd [8]byte
	if _, err := crand.Read(rnd[:]); err != nil {
		// crypto/rand unavailable: fall back to a process counter — ids stay
		// unique and ordered, just guessable.
		m.seq++
		return fmt.Sprintf("%016x%016x", ns, m.seq)
	}
	return fmt.Sprintf("%016x%s", ns, hex.EncodeToString(rnd[:]))
}

// --- hub ---------------------------------------------------------------------

// Notifier is the per-registration delivery callback the hub calls
// synchronously with each dispatched envelope. Implementations must not
// block (an LLM turn belongs in a goroutine) and must not hold hub locks —
// calling back into the hub from a notifier is allowed.
type Notifier func(env Envelope)

// Hub is the in-process message hub: passport/rule registries, the channel
// state machine, adapter dispatch and the optional WAL. Safe for concurrent
// use.
type Hub struct {
	mu               sync.Mutex
	passports        map[string]Passport // agent_id → passport
	byName           map[string]string   // name → agent_id
	rules            map[string]Rule
	resumes          map[string]Resume
	channels         map[string]*ChannelHandle
	adapters         map[string]ChannelAdapter // "type@version"
	notifiers        map[string]Notifier
	logs             map[string][]Envelope // per-channel accepted envelopes (in-memory WAL mirror)
	waiters          map[string]chan *ChannelHandle
	arbiter          HubArbiter
	audit            AuditSink
	tasks            map[string]task.Metadata // observed tasks (task_mirror forwards agent-side Task* events)
	expectEvaluators map[string]ExpectationEvaluator
	expectHandlers   map[string]ViolationHandler
	firedViolations  map[string]map[string]bool // channel → "(idx|kind|violator)" fires (in-memory dedup)
	wal              WAL
	now              func() time.Time
	ids              *idMinter
	ackWait          time.Duration
}

// HubOption configures a Hub.
type HubOption func(*Hub)

// WithHubWAL attaches the write-ahead log (every accepted envelope and
// channel transition is appended).
func WithHubWAL(w WAL) HubOption { return func(h *Hub) { h.wal = w } }

// WithHubClock overrides the hub clock (tests).
func WithHubClock(now func() time.Time) HubOption { return func(h *Hub) { h.now = now } }

// WithAckTimeout sets how long CreateChannel waits for invite acks
// (default 30s).
func WithAckTimeout(d time.Duration) HubOption { return func(h *Hub) { h.ackWait = d } }

// WithHubArbiter replaces the access/routing arbiter (default
// RuleBasedArbiter). Only one arbiter is active; the most recent
// registration wins. A nil arbiter keeps the default.
func WithHubArbiter(a HubArbiter) HubOption {
	return func(h *Hub) {
		if a != nil {
			h.arbiter = a
		}
	}
}

// WithAuditSink attaches the audit sink: the hub appends one entry per
// expectation-violation fire (and other hub-crossing events as they gain
// audit coverage). Nil disables auditing.
func WithAuditSink(sink AuditSink) HubOption { return func(h *Hub) { h.audit = sink } }

// NewHub creates an empty hub. Register adapters before creating channels.
func NewHub(opts ...HubOption) *Hub {
	h := &Hub{
		passports:        make(map[string]Passport),
		byName:           make(map[string]string),
		rules:            make(map[string]Rule),
		resumes:          make(map[string]Resume),
		channels:         make(map[string]*ChannelHandle),
		adapters:         make(map[string]ChannelAdapter),
		notifiers:        make(map[string]Notifier),
		logs:             make(map[string][]Envelope),
		waiters:          make(map[string]chan *ChannelHandle),
		arbiter:          RuleBasedArbiter{},
		tasks:            make(map[string]task.Metadata),
		expectEvaluators: make(map[string]ExpectationEvaluator),
		expectHandlers:   make(map[string]ViolationHandler),
		firedViolations:  make(map[string]map[string]bool),
		now:              time.Now,
		ids:              &idMinter{},
		ackWait:          30 * time.Second,
	}
	for _, e := range DefaultExpectationEvaluators() {
		h.expectEvaluators[e.Name()] = e
	}
	for _, vh := range DefaultViolationHandlers() {
		h.expectHandlers[vh.Name()] = vh
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// RegisterAdapter registers an adapter under type@version (version "" →
// "1"). Re-registering the same key replaces.
func (h *Hub) RegisterAdapter(a ChannelAdapter, version string) {
	if version == "" {
		version = "1"
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.adapters[a.Type()+"@"+version] = a
}

func (h *Hub) adapterFor(manifestType, version string) ChannelAdapter {
	if version == "" {
		version = "1"
	}
	return h.adapters[manifestType+"@"+version]
}

// Register stamps a fresh passport (AgentID) and stores it with its rule
// (nil → DefaultRule). The name must be unique per hub.
func (h *Hub) Register(p Passport, r *Rule) (Passport, error) {
	if p.Name == "" {
		return Passport{}, fmt.Errorf("%w: empty name", ErrBadIdentity)
	}
	if !p.Kind.Valid() {
		return Passport{}, fmt.Errorf("%w: kind %q", ErrBadIdentity, p.Kind)
	}
	if r == nil {
		def := DefaultRule()
		r = &def
	}
	stamped := p
	stamped.Kind = p.Kind.Effective()
	stamped.AgentID = uuid.New()

	h.mu.Lock()
	defer h.mu.Unlock()
	if _, taken := h.byName[stamped.Name]; taken {
		return Passport{}, fmt.Errorf("%w: %q", ErrNameTaken, stamped.Name)
	}
	h.passports[stamped.AgentID] = stamped
	h.byName[stamped.Name] = stamped.AgentID
	h.rules[stamped.AgentID] = r.Clone()
	return stamped, nil
}

// Unregister removes a registration; delivery to it stops (unknown
// recipients are dropped silently at dispatch).
func (h *Hub) Unregister(agentID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.passports[agentID]
	if !ok {
		return fmt.Errorf("%w: agent %s", ErrNotFound, agentID)
	}
	delete(h.passports, agentID)
	delete(h.byName, p.Name)
	delete(h.rules, agentID)
	delete(h.resumes, agentID)
	delete(h.notifiers, agentID)
	return nil
}

// Attach binds the delivery callback for a registered identity (nil
// detaches). The hub calls the notifier synchronously — see Notifier.
func (h *Hub) Attach(agentID string, fn Notifier) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.passports[agentID]; !ok {
		return fmt.Errorf("%w: agent %s", ErrNotFound, agentID)
	}
	if fn == nil {
		delete(h.notifiers, agentID)
	} else {
		h.notifiers[agentID] = fn
	}
	return nil
}

// Passport looks a registration up by id or name.
func (h *Hub) Passport(nameOrID string) (Passport, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.passportLocked(nameOrID)
}

// Registrations returns a snapshot of every passport in the hub (the peers
// tool's directory view), ordered by name.
func (h *Hub) Registrations() []Passport {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Passport, 0, len(h.passports))
	for _, p := range h.passports {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (h *Hub) passportLocked(nameOrID string) (Passport, error) {
	if p, ok := h.passports[nameOrID]; ok {
		return p, nil
	}
	if id, ok := h.byName[nameOrID]; ok {
		return h.passports[id], nil
	}
	return Passport{}, fmt.Errorf("%w: agent %q", ErrNotFound, nameOrID)
}

// RuleOf returns the stored rule for an agent id.
func (h *Hub) RuleOf(agentID string) (Rule, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.passports[agentID]; !ok {
		return Rule{}, fmt.Errorf("%w: agent %s", ErrNotFound, agentID)
	}
	return h.rules[agentID].Clone(), nil
}

// SetResume stores or replaces the capability claim for an agent.
func (h *Hub) SetResume(r Resume) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.passports[r.AgentID]; !ok {
		return fmt.Errorf("%w: agent %s", ErrNotFound, r.AgentID)
	}
	h.resumes[r.AgentID] = r
	return nil
}

// ResumeOf returns the stored resume (zero value when none was set).
func (h *Hub) ResumeOf(agentID string) (Resume, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.passports[agentID]; !ok {
		return Resume{}, fmt.Errorf("%w: agent %s", ErrNotFound, agentID)
	}
	return h.resumes[agentID], nil
}

// agentNameLocked resolves a participant id to its passport name ("" when
// unregistered — ids stay addressable even after unregister).
func (h *Hub) agentNameLocked(agentID string) string {
	if p, ok := h.passports[agentID]; ok {
		return p.Name
	}
	return ""
}

// CreateChannel allocates a channel, posts invites and blocks until every
// invitee acks (state pending → active) or the ack timeout hits. The first
// participant is the creator (initiator); the rest are invitees. TTL comes
// from manifest.Knobs["ttl_seconds"] (seconds; absent = no expiry).
func (h *Hub) CreateChannel(manifest ChannelManifest, participants []string) (*ChannelHandle, error) {
	if len(participants) == 0 {
		return nil, fmt.Errorf("%w: channel requires at least one participant", ErrProtocol)
	}
	seen := make(map[string]bool, len(participants))
	for _, id := range participants {
		if seen[id] {
			return nil, fmt.Errorf("%w: participant listed twice: %s", ErrProtocol, id)
		}
		seen[id] = true
	}

	h.mu.Lock()
	creatorID := participants[0]
	creator, err := h.passportLocked(creatorID)
	if err != nil {
		h.mu.Unlock()
		return nil, err
	}
	creatorRule := h.rules[creatorID]
	for _, id := range participants[1:] {
		if _, ok := h.passports[id]; !ok {
			h.mu.Unlock()
			return nil, fmt.Errorf("%w: participant not registered: %s", ErrNotFound, id)
		}
	}

	adapter := h.adapterFor(manifest.Type, manifest.Version)
	if adapter == nil {
		h.mu.Unlock()
		return nil, fmt.Errorf("%w: no adapter registered for manifest %q@%s", ErrNotFound, manifest.Type, manifest.Version)
	}

	// Pre-flight gate (arbiter): concurrency cap on the creator plus the
	// invitees' inbound access. The dispatch path silently filters
	// envelopes from blocked senders, so without this pre-check an invite
	// to a recipient who blocks the creator would be dropped and the
	// creator would hang until the ack timeout.
	active := 0
	for _, ch := range h.channels {
		if ch.CreatorID == creatorID && !ch.State.Terminal() {
			active++
		}
	}
	invitees := participants[1:]
	inviteePassports := make([]Passport, len(invitees))
	inviteeRules := make([]Rule, len(invitees))
	for i, id := range invitees {
		inviteePassports[i] = h.passports[id]
		inviteeRules[i] = h.rules[id]
	}
	if d := h.arbiter.AuthorizeChannelOpen(manifest, creator, creatorRule, inviteePassports, inviteeRules, active); !d.Allowed {
		h.mu.Unlock()
		return nil, denyErr(d)
	}

	channelID := uuid.New()
	now := h.now()
	fullManifest := manifest
	fullManifest.Participants = append([]string(nil), participants...)

	var expiresAt time.Time
	if ttl, ok := toInt(manifest.Knobs["ttl_seconds"]); ok && ttl > 0 {
		expiresAt = now.Add(time.Duration(ttl) * time.Second)
	}

	roles := make([]Participant, len(participants))
	for i, id := range participants {
		role := RoleParticipant
		switch {
		case i == 0:
			role = RoleInitiator
		case len(participants) == 2:
			role = RoleRespondent
		}
		roles[i] = Participant{AgentID: id, Role: role, Order: i, JoinedAt: now}
	}

	ch := &ChannelHandle{
		ID:           channelID,
		Manifest:     fullManifest,
		CreatorID:    creatorID,
		Participants: roles,
		State:        ChannelPending,
		CreatedAt:    now,
		ExpiresAt:    expiresAt,
		PendingAcks:  append([]string(nil), participants[1:]...),
		AdapterState: adapter.InitialState(),
		adapter:      adapter,
	}
	if err := adapter.ValidateCreate(fullManifest); err != nil {
		h.mu.Unlock()
		return nil, err
	}
	initAdapterState(adapter, ch)
	h.channels[channelID] = ch
	waiter := make(chan *ChannelHandle, 1)
	h.waiters[channelID] = waiter
	// Lifecycle snapshot: a WAL failure here must not fail the create (see
	// recordLocked).
	_ = h.recordLocked(channelSnapshot(ch))
	h.mu.Unlock()

	invites := participants[1:]
	if len(invites) == 0 {
		// Self-only channel: no handshake needed.
		if err := h.activate(channelID); err != nil {
			return nil, err
		}
		return h.Channel(channelID)
	}

	invitePayload, _ := json.Marshal(map[string]any{
		"channel_id":       channelID,
		"manifest_type":    manifest.Type,
		"manifest_version": manifest.Version,
		"creator_id":       creatorID,
		"knobs":            fullManifest.Knobs,
	})
	for _, invitee := range invites {
		inv := Envelope{
			ChannelID: channelID,
			SenderID:  creatorID,
			Audience:  invitee,
			EventType: EnvChannelInvite,
			Payload:   invitePayload,
		}
		if _, err := h.post(inv); err != nil {
			h.closeChannel(channelID, "invite_failed", err)
			return nil, err
		}
	}

	select {
	case done := <-waiter:
		if done == nil {
			return nil, fmt.Errorf("%w: channel %s closed during handshake", ErrProtocol, channelID)
		}
		return done, nil
	case <-time.After(h.ackWait):
		h.closeChannel(channelID, "invite_timeout", ErrAckTimeout)
		return nil, fmt.Errorf("%w: channel %s ack timeout", ErrAckTimeout, channelID)
	}
}

// activate completes the handshake: pending → active, broadcast
// EnvChannelOpened, release the create waiter. No-op unless still pending.
func (h *Hub) activate(channelID string) error {
	h.mu.Lock()
	ch, ok := h.channels[channelID]
	if !ok || ch.State != ChannelPending {
		h.mu.Unlock()
		_, cerr := h.Channel(channelID)
		return cerr
	}
	ch.State = ChannelActive
	ch.PendingAcks = nil
	// Lifecycle snapshot: a WAL failure here must not fail the handshake (see
	// recordLocked).
	_ = h.recordLocked(channelSnapshot(ch))
	h.mu.Unlock()

	opened := Envelope{
		ChannelID: channelID,
		SenderID:  ch.CreatorID,
		Audience:  "", // broadcast: every participant incl. the creator
		EventType: EnvChannelOpened,
		Payload:   mustJSON(map[string]any{"channel_id": channelID}),
	}
	_, err := h.post(opened)
	if err != nil {
		return err
	}
	h.mu.Lock()
	if w, ok := h.waiters[channelID]; ok {
		delete(h.waiters, channelID)
		snapshot := *ch
		w <- &snapshot
	}
	h.mu.Unlock()
	return nil
}

// absorbAck advances the invite handshake for one ack/reject envelope.
// Returns the follow-up action (activate / close reason) or "".
func (h *Hub) absorbAck(ch *ChannelHandle, env Envelope) string {
	if ch.State != ChannelPending {
		return ""
	}
	switch env.EventType {
	case EnvChannelAck:
		for i, id := range ch.PendingAcks {
			if id == env.SenderID {
				ch.PendingAcks = append(ch.PendingAcks[:i], ch.PendingAcks[i+1:]...)
				break
			}
		}
		if len(ch.PendingAcks) == 0 && len(ch.RejectedBy) == 0 {
			return "activate"
		}
	case EnvChannelReject:
		for i, id := range ch.PendingAcks {
			if id == env.SenderID {
				ch.PendingAcks = append(ch.PendingAcks[:i], ch.PendingAcks[i+1:]...)
				break
			}
		}
		found := false
		for _, id := range ch.RejectedBy {
			if id == env.SenderID {
				found = true
			}
		}
		if !found {
			ch.RejectedBy = append(ch.RejectedBy, env.SenderID)
		}
		// All-or-nothing handshake: any reject fails the channel.
		return "reject"
	}
	return ""
}

// Send validates and accepts an envelope: sender/channel checks, rule
// enforcement (outbound globs, delegation depth, TTL), adapter
// validation+fold, WAL append, then dispatch to the audience. Returns the
// hub-stamped envelope. Ack/reject envelopes are absorbed by the hub (not
// dispatched).
func (h *Hub) Send(env Envelope) (Envelope, error) {
	return h.post(env)
}

// postAction carries what must happen after the post critical section
// releases the hub lock.
type postAction struct {
	// kind: "dispatch" (default), "activate", "reject".
	kind    string
	env     Envelope
	targets []dispatchTarget
	channel string
	reason  string
	// closeReason is set when the adapter requested a channel close; it
	// runs after dispatch, outside the lock.
	closeReason string
}

// dispatchTarget is one resolved recipient (captured under the lock so
// dispatch never touches hub maps).
type dispatchTarget struct {
	id     string
	name   string
	rule   Rule
	notify Notifier
	isSelf bool
}

func (h *Hub) post(env Envelope) (Envelope, error) {
	var action postAction
	stamped, err := func() (Envelope, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.postLocked(env, &action)
	}()
	// Lazy expiry requests a broadcast even though the send failed.
	if action.kind == "expire" {
		h.closeChannel(action.channel, "ttl", nil, ChannelExpired)
		action.kind = ""
	}
	if err != nil {
		return stamped, err
	}
	// Outside the lock, in order: dispatch → adapter-requested close →
	// handshake follow-ups (activate posts the opened envelope; a reject
	// closes the channel and fails the create waiter).
	if action.kind == "dispatch" {
		h.dispatch(action.env, action.targets)
	}
	if action.closeReason != "" {
		h.closeChannel(action.channel, action.closeReason, nil)
	}
	switch action.kind {
	case "activate":
		if err := h.activate(action.channel); err != nil {
			return stamped, err
		}
	case "reject":
		h.closeChannel(action.channel, "invite_rejected",
			fmt.Errorf("%w: channel %s rejected by an invitee", ErrProtocol, action.channel))
	}
	return stamped, nil
}

// postLocked is the critical section: validate → adapter fold → stamp →
// WAL/log append → decide follow-up. Everything here runs under h.mu.
func (h *Hub) postLocked(env Envelope, action *postAction) (Envelope, error) {
	sender, err := h.passportLocked(env.SenderID)
	if err != nil {
		return Envelope{}, err
	}
	senderRule := h.rules[sender.AgentID]

	ch, ok := h.channels[env.ChannelID]
	if !ok {
		return Envelope{}, fmt.Errorf("%w: channel %s", ErrNotFound, env.ChannelID)
	}
	if ch.State.Terminal() {
		if ch.State == ChannelExpired {
			return Envelope{}, fmt.Errorf("%w: channel %s", ErrChannelExpired, ch.ID)
		}
		return Envelope{}, fmt.Errorf("%w: channel %s", ErrChannelClosed, ch.ID)
	}

	// Lazy channel expiry: the first send past ExpiresAt expires the
	// channel (this subset runs no background sweeper). The state flips
	// here; the expired-envelope broadcast happens after the lock releases
	// (post runs the "expire" action even on the error path).
	if !ch.ExpiresAt.IsZero() && h.now().After(ch.ExpiresAt) {
		ch.State = ChannelExpired
		ch.ClosedAt = h.now()
		ch.CloseReason = "ttl"
		// Lifecycle snapshot on the error path: swallowed (see recordLocked).
		_ = h.recordLocked(channelSnapshot(ch))
		action.kind = "expire"
		action.channel = ch.ID
		return Envelope{}, fmt.Errorf("%w: channel %s", ErrChannelExpired, ch.ID)
	}

	protocol := isProtocolEvent(env.EventType)
	if !protocol && ch.State != ChannelActive {
		return Envelope{}, fmt.Errorf("%w: channel %s not active (state=%s)", ErrProtocol, ch.ID, ch.State)
	}
	if !ch.hasParticipant(env.SenderID) {
		return Envelope{}, fmt.Errorf("%w: channel %s only accepts sends from participants, got %s",
			ErrProtocol, ch.ID, env.SenderID)
	}
	if ch.adapter == nil {
		return Envelope{}, fmt.Errorf("%w: channel %s has no registered adapter (manifest %q@%s)",
			ErrProtocol, ch.ID, ch.Manifest.Type, ch.Manifest.Version)
	}

	// Per-envelope TTL (sender-asserted: only meaningful when the sender
	// supplied a CreatedAt to measure from).
	if env.TTLSecs > 0 && !env.CreatedAt.IsZero() && h.now().After(env.CreatedAt.Add(time.Duration(env.TTLSecs)*time.Second)) {
		return Envelope{}, fmt.Errorf("%w: envelope on channel %s", ErrTTLExpired, ch.ID)
	}

	// Outbound access + delegation depth (arbiter). Explicit audiences
	// only; broadcasts skip (protocol broadcasts address the creator too).
	var recipients []Passport
	if env.Audience != "" && env.Audience != env.SenderID {
		if recipient, ok := h.passports[env.Audience]; ok {
			recipients = append(recipients, recipient)
		}
	}
	if d := h.arbiter.AuthorizeSend(env, sender, senderRule, recipients); !d.Allowed {
		return Envelope{}, denyErr(d)
	}

	// Adapter validate+fold. On error the state stays untouched and the
	// envelope never reaches the WAL.
	routing, err := ch.adapter.OnEnvelope(ch, env)
	if err != nil {
		return Envelope{}, err
	}

	// Stamp, persist, then decide dispatch. The WAL append is the acceptance
	// barrier (Python parity: post_envelope awaits _wal_append before the
	// fold): when the backend fails, the send fails and the envelope never
	// enters the in-memory log — ReadWAL only ever shows accepted envelopes
	// and Replay only ever sees persisted ones.
	env.ID = h.ids.Next()
	env.CreatedAt = h.now()
	if err := h.recordLocked(WALRecord{Kind: "envelope", Envelope: &env}); err != nil {
		return Envelope{}, fmt.Errorf("network: append envelope %s to wal: %w", env.ID, err)
	}
	h.logs[ch.ID] = append(h.logs[ch.ID], env)

	if protocol {
		switch env.EventType {
		case EnvChannelAck, EnvChannelReject:
			// Absorbed by the hub: never dispatched.
			switch act := h.absorbAck(ch, env); act {
			case "activate":
				action.kind = "activate"
				action.channel = ch.ID
			case "reject":
				action.kind = "reject"
				action.channel = ch.ID
			}
			return env, nil
		}
	}

	action.kind = "dispatch"
	action.env = env
	action.targets = h.resolveTargetsLocked(ch, env)

	// Adapter-requested close runs after dispatch (Python parity): the
	// closing envelope itself is appended directly by closeChannel.
	if routing.Close {
		action.closeReason = nonEmpty(routing.CloseReason, "closed")
		action.channel = ch.ID
	}
	return env, nil
}
func (h *Hub) resolveTargetsLocked(ch *ChannelHandle, env Envelope) []dispatchTarget {
	var ids []string
	switch {
	case env.Audience != "":
		ids = []string{env.Audience}
	case isLifecycleEvent(env.EventType):
		// Lifecycle broadcasts (opened / closed / expired) address the full
		// participant list in Python — including the creator. The AGo
		// audience field is a single id, so the self-inclusion is resolved
		// here instead of on the envelope.
		ids = ch.ParticipantIDs()
	default:
		for _, p := range ch.Participants {
			if p.AgentID != env.SenderID {
				ids = append(ids, p.AgentID)
			}
		}
	}
	// Federation hook: audience members no passport covers can be
	// redirected by the arbiter (default: dropped silently).
	ids = h.resolveUnknownLocked(env, ids)

	targets := make([]dispatchTarget, 0, len(ids))
	sender, hasSender := h.passports[env.SenderID]
	for _, id := range ids {
		p, ok := h.passports[id]
		if !ok {
			continue // replacement ids that stay unknown drop silently
		}
		t := dispatchTarget{id: id, name: p.Name, isSelf: id == env.SenderID}
		if !t.isSelf && hasSender {
			// Inbound access (arbiter): silently skip blocked recipients —
			// the rest of the audience still receives.
			t.rule = h.rules[id]
			if d := h.arbiter.AuthorizeDispatch(env, sender, p, t.rule); !d.Allowed {
				continue
			}
		}
		t.notify = h.notifiers[id]
		targets = append(targets, t)
	}
	return targets
}

// isLifecycleEvent reports the hub-emitted channel broadcasts.
func isLifecycleEvent(eventType string) bool {
	switch eventType {
	case EnvChannelOpened, EnvChannelClosed, EnvChannelExpired:
		return true
	}
	return false
}

func (h *Hub) dispatch(env Envelope, targets []dispatchTarget) {
	for _, t := range targets {
		if t.notify != nil {
			t.notify(env)
		}
	}
}

// CloseChannel transitions a channel to closed and broadcasts
// EnvChannelClosed. Idempotent for already-terminal channels.
func (h *Hub) CloseChannel(channelID string, reason string) error {
	_, err := h.Channel(channelID)
	if err != nil {
		return err
	}
	h.closeChannel(channelID, nonEmpty(reason, "explicit_close"), nil)
	return nil
}

// ExpireChannel transitions a channel to expired (TTL semantics).
func (h *Hub) ExpireChannel(channelID string) error {
	_, err := h.Channel(channelID)
	if err != nil {
		return err
	}
	h.closeChannel(channelID, "expired", nil, ChannelExpired)
	return nil
}

// closeChannel flips the state to a terminal one, appends + dispatches the
// terminal envelope directly (bypassing post — the channel no longer
// accepts), and resolves a pending create waiter (waiterErr, or a generic
// protocol error).
func (h *Hub) closeChannel(channelID string, reason string, waiterErr error, state ...ChannelState) {
	target := ChannelClosed
	kind := EnvChannelClosed
	if len(state) > 0 && state[0] == ChannelExpired {
		target = ChannelExpired
		kind = EnvChannelExpired
	}

	h.mu.Lock()
	ch, ok := h.channels[channelID]
	if !ok || ch.State.Terminal() {
		h.mu.Unlock()
		return
	}
	ch.State = target
	ch.ClosedAt = h.now()
	ch.CloseReason = reason
	// Terminal channel: the fired-violation dedup cache is dead weight.
	delete(h.firedViolations, channelID)
	// Lifecycle snapshot: swallowed (see recordLocked).
	_ = h.recordLocked(channelSnapshot(ch))

	closeEnv := Envelope{
		ChannelID: channelID,
		SenderID:  ch.CreatorID,
		Audience:  "", // broadcast to every participant incl. the creator
		EventType: kind,
		Payload: mustJSON(map[string]any{
			"reason":     reason,
			"channel_id": channelID,
		}),
	}
	closeEnv.ID = h.ids.Next()
	closeEnv.CreatedAt = h.now()
	h.logs[channelID] = append(h.logs[channelID], closeEnv)
	// AGO-DEVIATION: Python surfaces the close envelope's WAL append failure
	// through close_channel; AGo's closeChannel is void and also runs as a
	// post-send follow-up where no caller could receive the error, so the
	// failure is swallowed (channel state is already terminal in memory; see
	// docs/DECISIONS.md D-033).
	_ = h.recordLocked(WALRecord{Kind: "envelope", Envelope: &closeEnv})

	targets := h.resolveTargetsLocked(ch, closeEnv)
	if w, ok := h.waiters[channelID]; ok {
		delete(h.waiters, channelID)
		if waiterErr == nil {
			snapshot := *ch
			w <- &snapshot
		} else {
			w <- nil
		}
	}
	h.mu.Unlock()

	h.dispatch(closeEnv, targets)
}

// Channel returns a snapshot copy of the channel record.
func (h *Hub) Channel(channelID string) (*ChannelHandle, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch, ok := h.channels[channelID]
	if !ok {
		return nil, fmt.Errorf("%w: channel %s", ErrNotFound, channelID)
	}
	out := *ch
	out.Participants = append([]Participant(nil), ch.Participants...)
	out.PendingAcks = append([]string(nil), ch.PendingAcks...)
	out.RejectedBy = append([]string(nil), ch.RejectedBy...)
	if ch.Manifest.Participants != nil {
		out.Manifest.Participants = append([]string(nil), ch.Manifest.Participants...)
	}
	if ch.Manifest.Knobs != nil {
		out.Manifest.Knobs = cloneKnobs(ch.Manifest.Knobs)
	}
	return &out, nil
}

// ListChannels returns snapshot copies of every channel.
func (h *Hub) ListChannels() []*ChannelHandle {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*ChannelHandle, 0, len(h.channels))
	for _, ch := range h.channels {
		cp := *ch
		cp.Participants = append([]Participant(nil), ch.Participants...)
		cp.PendingAcks = append([]string(nil), ch.PendingAcks...)
		cp.RejectedBy = append([]string(nil), ch.RejectedBy...)
		out = append(out, &cp)
	}
	return out
}

// ReadWAL returns the accepted envelopes of a channel, in accept order.
func (h *Hub) ReadWAL(channelID string) ([]Envelope, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	log, ok := h.logs[channelID]
	if !ok {
		if _, exists := h.channels[channelID]; !exists {
			return nil, fmt.Errorf("%w: channel %s", ErrNotFound, channelID)
		}
		return nil, nil
	}
	return append([]Envelope(nil), log...), nil
}

// Replay rebuilds hub state from the WAL into this hub: channel snapshots
// restore handles, envelopes re-fold through their adapter (deterministic)
// and land in the in-memory log. Adapters referenced by the WAL must be
// registered first. A hub with no WAL is a no-op.
func (h *Hub) Replay() error {
	if h.wal == nil {
		return nil
	}
	recs, err := h.wal.ReadAll()
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, rec := range recs {
		switch rec.Kind {
		case "channel":
			if rec.Channel == nil {
				return fmt.Errorf("%w: channel record missing snapshot", ErrProtocol)
			}
			snap := *rec.Channel
			adapter := h.adapterFor(snap.Manifest.Type, snap.Manifest.Version)
			if adapter == nil {
				return fmt.Errorf("%w: adapter %q@%s not registered before replay",
					ErrNotFound, snap.Manifest.Type, snap.Manifest.Version)
			}
			ch := &ChannelHandle{
				ID:           snap.ID,
				Manifest:     snap.Manifest,
				CreatorID:    snap.CreatorID,
				Participants: snap.Participants,
				State:        snap.State,
				CreatedAt:    snap.CreatedAt,
				ExpiresAt:    snap.ExpiresAt,
				ClosedAt:     snap.ClosedAt,
				CloseReason:  snap.CloseReason,
				PendingAcks:  snap.PendingAcks,
				RejectedBy:   snap.RejectedBy,
				adapter:      adapter,
			}
			// A later snapshot of an already-rebuilt channel must not
			// discard the fold state accumulated from the envelopes in
			// between.
			if existing, ok := h.channels[snap.ID]; ok {
				ch.AdapterState = existing.AdapterState
			} else {
				initAdapterState(adapter, ch)
			}
			h.channels[ch.ID] = ch
			if h.logs[ch.ID] == nil {
				h.logs[ch.ID] = nil
			}
		case "envelope":
			if rec.Envelope == nil {
				return fmt.Errorf("%w: envelope record missing envelope", ErrProtocol)
			}
			env := *rec.Envelope
			ch, ok := h.channels[env.ChannelID]
			if !ok {
				return fmt.Errorf("%w: WAL envelope references unknown channel %s", ErrProtocol, env.ChannelID)
			}
			switch env.EventType {
			case EnvChannelAck:
				h.absorbAck(ch, env)
			case EnvChannelReject:
				h.absorbAck(ch, env)
			case EnvChannelOpened:
				if ch.State == ChannelPending {
					ch.State = ChannelActive
					ch.PendingAcks = nil
				}
			case EnvChannelClosed, EnvChannelExpired:
				// State arrives via the channel snapshot; nothing to fold.
			default:
				if ch.adapter != nil && !ch.State.Terminal() {
					if _, err := ch.adapter.OnEnvelope(ch, env); err != nil {
						return fmt.Errorf("replay fold failed on channel %s: %w", ch.ID, err)
					}
				}
			}
			h.logs[ch.ID] = append(h.logs[ch.ID], env)
		default:
			return fmt.Errorf("%w: unknown WAL record kind %q", ErrProtocol, rec.Kind)
		}
	}
	return nil
}

// recordLocked appends to the WAL and reports the backend failure. The
// caller decides what a failure means: envelope records posted through Send
// must surface it — an envelope reported as sent but never persisted is lost
// for Replay (Python parity: post_envelope awaits _wal_append without
// try/except) — while lifecycle snapshots keep the send-path working when the
// backend is down (the in-memory log remains the source for in-process
// reads).
func (h *Hub) recordLocked(rec WALRecord) error {
	if h.wal == nil {
		return nil
	}
	return h.wal.Append(rec)
}

func channelSnapshot(ch *ChannelHandle) WALRecord {
	snap := ChannelSnapshot{
		ID:           ch.ID,
		Manifest:     ch.Manifest,
		CreatorID:    ch.CreatorID,
		Participants: ch.Participants,
		State:        ch.State,
		CreatedAt:    ch.CreatedAt,
		ExpiresAt:    ch.ExpiresAt,
		ClosedAt:     ch.ClosedAt,
		CloseReason:  ch.CloseReason,
		PendingAcks:  ch.PendingAcks,
		RejectedBy:   ch.RejectedBy,
	}
	return WALRecord{Kind: "channel", Channel: &snap}
}

func mustJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return data
}

func cloneKnobs(src map[string]any) map[string]any {
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}
