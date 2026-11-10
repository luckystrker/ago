package network

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/luckystrker/ago/task"
)

// Audit record kinds the hub itself emits. The set is open — tenants may
// append entries with their own kinds (Python parity: AuditLog's open kind
// set).
const (
	AuditExpectationViolated = "expectation_violated"
	AuditAgentRegistered     = "agent_registered"
	AuditAgentUnregistered   = "agent_unregistered"
	AuditResumeSet           = "resume_set"
	AuditRuleSet             = "rule_set"
	AuditChannelCreated      = "channel_created"
	AuditChannelClosed       = "channel_closed"
	AuditChannelExpired      = "channel_expired"
	AuditTaskTerminated      = "task_terminated"
	AuditTurnFailed          = "turn_failed"
)

// Sources of a resume_set audit record.
const (
	AuditSourceTenant   = "tenant"
	AuditSourceObserved = "observed"
)

// AuditEntry is one hub-crossing event not visible on a per-channel WAL: a
// JSON object per line in the file sink. Which fields are set depends on
// Kind; the rest stay off the wire.
type AuditEntry struct {
	At          time.Time      `json:"at"`
	Kind        string         `json:"kind"`
	ChannelID   string         `json:"channel_id,omitempty"`
	Expectation string         `json:"expectation,omitempty"`
	Violators   []string       `json:"violators,omitempty"`
	Detail      map[string]any `json:"detail,omitempty"`
	OnViolation string         `json:"on_violation,omitempty"`
	Params      map[string]any `json:"params,omitempty"`

	AgentID         string   `json:"agent_id,omitempty"`
	Name            string   `json:"name,omitempty"`
	Source          string   `json:"source,omitempty"` // resume_set: AuditSourceTenant | AuditSourceObserved
	Version         int      `json:"version,omitempty"`
	Capability      string   `json:"capability,omitempty"`
	Outcome         string   `json:"outcome,omitempty"`
	Reason          string   `json:"reason,omitempty"`
	ManifestType    string   `json:"manifest_type,omitempty"`
	ManifestVersion string   `json:"manifest_version,omitempty"`
	CreatorID       string   `json:"creator_id,omitempty"`
	Participants    []string `json:"participants,omitempty"`
	TaskID          string   `json:"task_id,omitempty"`
	OwnerID         string   `json:"owner_id,omitempty"`
	EnvelopeID      string   `json:"envelope_id,omitempty"`
	ExcType         string   `json:"exc_type,omitempty"`
	ExcMessage      string   `json:"exc_message,omitempty"`
}

// AuditSink receives the hub's audit entries. Implementations must be safe
// for concurrent use and must not block on the hub's hot path for long.
type AuditSink interface {
	Append(entry AuditEntry) error
}

// FileAuditLog is a JSONL audit sink: one entry per line, appended under a
// mutex (Python parity: AuditLog over audit.jsonl).
type FileAuditLog struct {
	mu   sync.Mutex
	path string
}

// NewFileAuditLog creates a JSONL audit log at path (created on first
// append; parent directories are made as needed).
func NewFileAuditLog(path string) *FileAuditLog { return &FileAuditLog{path: path} }

// Append implements AuditSink.
func (l *FileAuditLog) Append(entry AuditEntry) error {
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// Audit entries carry expectation-violation details and channel context:
	// owner-only file and directory. AGO-DEVIATION: Python's AuditLog leaves
	// the mode to the umask (0755/0644).
	if dir := filepath.Dir(l.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// SliceAuditLog is an in-memory audit sink (tests and ephemeral hubs).
type SliceAuditLog struct {
	mu      sync.Mutex
	entries []AuditEntry
}

// NewSliceAuditLog creates an empty in-memory audit sink.
func NewSliceAuditLog() *SliceAuditLog { return &SliceAuditLog{} }

// Append implements AuditSink.
func (l *SliceAuditLog) Append(entry AuditEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, entry)
	return nil
}

// Entries returns a copy of the recorded entries in append order.
func (l *SliceAuditLog) Entries() []AuditEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]AuditEntry(nil), l.entries...)
}

// AuditLog is the hub's audit trail as a HubListener: every agent, channel,
// expectation, task-termination and turn-failure event becomes one AuditEntry
// appended to the sink. Live subscribers see each appended entry; a failing
// sink or subscriber never disturbs the flow being audited. Safe for
// concurrent use.
//
// Register it with WithAuditLog (or Hub.RegisterListener), or use
// WithAuditSink for the narrow expectation-only trail.
type AuditLog struct {
	BaseHubListener
	sink     AuditSink
	now      func() time.Time
	kinds    map[string]bool // nil = record every kind
	clockSet bool

	bytes atomic.Int64

	mu   sync.Mutex
	subs map[int]func(AuditEntry)
	next int
}

// AuditOption configures NewAuditLog.
type AuditOption func(*AuditLog)

// WithAuditClock sets the clock stamped on records that carry no event time
// of their own (default time.Now; a hub-registered log follows the hub clock
// through the payload "at").
func WithAuditClock(now func() time.Time) AuditOption {
	return func(l *AuditLog) { l.now, l.clockSet = now, true }
}

// OnlyAuditKinds restricts the log to the listed entry kinds.
func OnlyAuditKinds(kinds ...string) AuditOption {
	return func(l *AuditLog) {
		l.kinds = make(map[string]bool, len(kinds))
		for _, k := range kinds {
			l.kinds[k] = true
		}
	}
}

// NewAuditLog builds an audit listener over sink.
func NewAuditLog(sink AuditSink, opts ...AuditOption) *AuditLog {
	l := &AuditLog{sink: sink, now: time.Now, subs: make(map[int]func(AuditEntry))}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// Append records one entry: sink first, then every live subscriber in
// subscription order. Public so tenants can append entries of their own
// kinds. A sink error is returned and the entry is not shown to subscribers;
// a subscriber panic is logged and skipped. Kinds excluded by OnlyAuditKinds
// are dropped silently.
func (l *AuditLog) Append(entry AuditEntry) error {
	if l.kinds != nil && !l.kinds[entry.Kind] {
		return nil
	}
	if entry.At.IsZero() {
		entry.At = l.now()
	}
	if err := l.sink.Append(entry); err != nil {
		return err
	}
	if line, err := json.Marshal(entry); err == nil {
		l.bytes.Add(int64(len(line)) + 1)
	}
	l.mu.Lock()
	ids := make([]int, 0, len(l.subs))
	for id := range l.subs {
		ids = append(ids, id)
	}
	l.mu.Unlock()
	sortInts(ids)
	for _, id := range ids {
		l.mu.Lock()
		cb := l.subs[id]
		l.mu.Unlock()
		if cb == nil {
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("audit subscriber panicked", "kind", entry.Kind, "panic", r)
				}
			}()
			cb(entry)
		}()
	}
	return nil
}

func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}

// BytesWritten is the process-local count of JSON bytes (newline included)
// appended since construction; Hub.Health reports it as AuditLogBytes.
func (l *AuditLog) BytesWritten() int64 { return l.bytes.Load() }

// Subscribe attaches a live callback fired per appended entry (a dashboard or
// alert tail) and returns the function that detaches it.
func (l *AuditLog) Subscribe(fn func(AuditEntry)) (unsubscribe func()) {
	l.mu.Lock()
	id := l.next
	l.next++
	l.subs[id] = fn
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		delete(l.subs, id)
		l.mu.Unlock()
	}
}

// payloadTime reads the hub-stamped event time from a listener payload.
func payloadTime(payload map[string]any) time.Time {
	t, _ := payload["at"].(time.Time)
	return t
}

func payloadString(payload map[string]any, key string) string {
	s, _ := payload[key].(string)
	return s
}

func payloadInt(payload map[string]any, key string) int {
	n, _ := payload[key].(int)
	return n
}

// OnAgentEvent implements HubListener.
func (l *AuditLog) OnAgentEvent(agentID, kind string, payload map[string]any) {
	e := AuditEntry{At: payloadTime(payload), AgentID: agentID}
	switch kind {
	case AgentEventRegistered:
		e.Kind = AuditAgentRegistered
		e.Name = payloadString(payload, "name")
	case AgentEventUnregistered:
		e.Kind = AuditAgentUnregistered
		e.Name = payloadString(payload, "name")
	case AgentEventResumeSet:
		e.Kind, e.Source, e.Version = AuditResumeSet, AuditSourceTenant, payloadInt(payload, "version")
	case AgentEventRuleSet:
		e.Kind, e.Version = AuditRuleSet, payloadInt(payload, "version")
	case AgentEventObservationRecorded:
		e.Kind, e.Source, e.Version = AuditResumeSet, AuditSourceObserved, payloadInt(payload, "version")
		e.Capability = payloadString(payload, "capability")
		e.Outcome = payloadString(payload, "outcome")
	default:
		return
	}
	_ = l.Append(e)
}

// OnChannelEvent implements HubListener: created, closed and expired are
// audited; opened shows on the channel WAL.
func (l *AuditLog) OnChannelEvent(channelID, kind string, payload map[string]any) {
	e := AuditEntry{At: payloadTime(payload), ChannelID: channelID}
	switch kind {
	case ChannelEventCreated:
		e.Kind = AuditChannelCreated
		e.ManifestType = payloadString(payload, "manifest_type")
		e.ManifestVersion = payloadString(payload, "manifest_version")
		e.CreatorID = payloadString(payload, "creator_id")
		e.Participants, _ = payload["participants"].([]string)
	case ChannelEventClosed:
		e.Kind, e.Reason = AuditChannelClosed, payloadString(payload, "reason")
	case ChannelEventExpired:
		e.Kind, e.Reason = AuditChannelExpired, payloadString(payload, "reason")
	default:
		return
	}
	_ = l.Append(e)
}

// OnExpectationFired implements HubListener.
func (l *AuditLog) OnExpectationFired(channelID string, exp Expectation, v Violation) {
	_ = l.Append(AuditEntry{
		Kind:        AuditExpectationViolated,
		ChannelID:   channelID,
		Expectation: exp.Kind,
		OnViolation: exp.OnViolation,
		Params:      exp.Params,
		Violators:   append([]string(nil), v.ViolatorIDs...),
		Detail:      v.Detail,
	})
}

// OnTaskEvent implements HubListener: only terminal transitions are audited;
// started and progress are observed-only, and mirror_failed is a delivery
// problem rather than a task outcome.
func (l *AuditLog) OnTaskEvent(taskID, kind string, payload map[string]any) {
	if !task.State(kind).Terminal() {
		return
	}
	outcome := payloadString(payload, "outcome")
	if outcome == "" {
		outcome = kind
	}
	_ = l.Append(AuditEntry{
		At:         payloadTime(payload),
		Kind:       AuditTaskTerminated,
		TaskID:     taskID,
		OwnerID:    payloadString(payload, "owner_id"),
		ChannelID:  payloadString(payload, "channel_id"),
		Outcome:    outcome,
		Capability: payloadString(payload, "capability"),
		Reason:     payloadString(payload, "reason"),
	})
}

// OnTurnFailed implements HubListener.
func (l *AuditLog) OnTurnFailed(channelID, agentID, envelopeID string, err error) {
	e := AuditEntry{Kind: AuditTurnFailed, ChannelID: channelID, AgentID: agentID, EnvelopeID: envelopeID}
	if err != nil {
		e.ExcType, e.ExcMessage = fmt.Sprintf("%T", err), err.Error()
	}
	_ = l.Append(e)
}

var (
	_ HubListener = (*AuditLog)(nil)
	_ AuditSink   = (*AuditLog)(nil)
)

// WithAuditLog registers log as the hub's audit trail: it becomes the first
// listener, so an event is recorded before tenant listeners observe it, and
// its clock follows the hub clock. Health reports its byte counter.
func WithAuditLog(log *AuditLog) HubOption {
	return func(h *Hub) {
		if log == nil {
			return
		}
		h.ext.audit = log
	}
}

// WithAuditSink attaches sink as an expectation-only audit trail: one entry
// per expectation-violation fire, as before the listener seam existed. For
// agent, channel, task and turn-failure records use WithAuditLog.
func WithAuditSink(sink AuditSink) HubOption {
	return func(h *Hub) {
		if sink == nil {
			return
		}
		h.ext.audit = NewAuditLog(sink, OnlyAuditKinds(AuditExpectationViolated))
	}
}

// AuditLog returns the hub's audit log (nil when none is attached).
func (h *Hub) AuditLog() *AuditLog { return h.ext.audit }
