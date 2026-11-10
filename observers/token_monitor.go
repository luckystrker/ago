package observers

import (
	"fmt"
	"sync"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// Python's TokenMonitor defaults (ag2/observers/token_monitor.py).
const (
	DefaultWarnThreshold  int64 = 50_000
	DefaultAlertThreshold int64 = 100_000
)

// TokenMonitorOption configures a TokenMonitor.
type TokenMonitorOption func(*tokenMonitorOptions)

type tokenMonitorOptions struct {
	name    string
	timeout time.Duration
}

// WithTokenMonitorName sets the observer's display name, used as the alert
// source (Python parity: name=). An empty name keeps "token_monitor".
//
// AGO-DEVIATION: Python's default name is "token-monitor" (hyphen); AGo keeps
// the established "token_monitor" source so existing alert consumers match.
func WithTokenMonitorName(name string) TokenMonitorOption {
	return func(o *tokenMonitorOptions) {
		if name != "" {
			o.name = name
		}
	}
}

// WithTokenMonitorTimeout bounds one process call (Python parity: timeout=).
// Expiry is logged and the batch dropped. d <= 0 waits indefinitely.
func WithTokenMonitorTimeout(d time.Duration) TokenMonitorOption {
	return func(o *tokenMonitorOptions) {
		if d > 0 {
			o.timeout = d
		}
	}
}

// TokenMonitor accumulates billable tokens across UsageEvent telemetry and emits
// ObserverAlert events at its thresholds. It reads UsageEvent — the same
// accounting source as the usage package's reports — so every billable unit
// counts: the main model loop, forwarded descendant calls, compaction and
// aggregation. A source record is counted once per monitor, keyed by its
// event_id: a record observed both locally and through a delegation hop cannot
// spend the budget twice.
//
// Two modes exist:
//   - NewTokenMonitor keeps the original single-budget guard: one severity
//     error alert when the total rises strictly above the budget.
//   - NewTokenMonitorThresholds is the Python-parity guard: a severity warning
//     alert at warnThreshold and a severity critical alert at alertThreshold,
//     each fired once, with "at or above" comparison.
//
// Safe for concurrent use.
type TokenMonitor struct {
	*BaseObserver

	// strict selects the legacy single-budget comparison.
	strict         bool
	budget         int64
	warnThreshold  int64
	alertThreshold int64

	mu      sync.Mutex // guards the fields below
	total   int64
	warned  bool
	alerted bool
	seen    map[string]struct{}
}

// NewTokenMonitor watches UsageEvent events against budget; the alert fires when
// the accumulated billable total rises strictly above it (the pre-existing
// single-budget behaviour).
func NewTokenMonitor(budget int64, opts ...TokenMonitorOption) *TokenMonitor {
	m := newTokenMonitor("token_monitor", opts)
	m.strict = true
	m.budget = budget
	return m
}

// NewTokenMonitorThresholds is the Python-parity constructor: a warning when the
// total reaches warnThreshold and a critical alert when it reaches
// alertThreshold, each fired once per session. Use DefaultWarnThreshold and
// DefaultAlertThreshold for Python's defaults.
func NewTokenMonitorThresholds(warnThreshold, alertThreshold int64, opts ...TokenMonitorOption) *TokenMonitor {
	m := newTokenMonitor("token_monitor", opts)
	m.warnThreshold = warnThreshold
	m.alertThreshold = alertThreshold
	return m
}

// newTokenMonitor builds the shared state and its base observer.
func newTokenMonitor(defaultName string, opts []TokenMonitorOption) *TokenMonitor {
	o := tokenMonitorOptions{name: defaultName}
	for _, opt := range opts {
		opt(&o)
	}
	m := &TokenMonitor{seen: map[string]struct{}{}}
	m.BaseObserver = NewBaseObserver(o.name, m.process, WithObserverCondition(watchedUsageEvent), WithBatchSize(1), WithProcessTimeout(o.timeout))
	return m
}

// Total returns the accumulated billable token count.
func (m *TokenMonitor) Total() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.total
}

// watchedUsageEvent matches a source UsageEvent and an observation envelope
// that may carry one (archived transcripts); SourceEvent decides which.
func watchedUsageEvent(ev events.Event) bool {
	switch events.AsValue(ev).(type) {
	case events.UsageEvent, events.ChildEvent, events.TransientChildEvent:
		return true
	}
	return false
}

// process folds one batch of usage records into the total and returns the alert
// to emit, if any.
func (m *TokenMonitor) process(_ *agocontext.Context, evs []events.Event) (*events.ObserverAlert, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ev := range evs {
		u, ok := events.AsValue(events.SourceEvent(ev)).(events.UsageEvent)
		if !ok {
			continue
		}
		if u.EventID != "" {
			if _, dup := m.seen[u.EventID]; dup {
				continue
			}
			m.seen[u.EventID] = struct{}{}
		}
		m.total += billable(u.Usage)
	}
	if m.strict {
		if m.alerted || m.total <= m.budget {
			return nil, nil
		}
		m.alerted = true
		return &events.ObserverAlert{
			Source:   m.Name(),
			Severity: events.SeverityError,
			Message: fmt.Sprintf("Token budget exceeded: %d tokens used (budget: %d). Consider wrapping up to control costs.",
				m.total, m.budget),
		}, nil
	}
	if !m.alerted && m.total >= m.alertThreshold {
		m.alerted = true
		return &events.ObserverAlert{
			Source:   m.Name(),
			Severity: events.SeverityCritical,
			Message: fmt.Sprintf("Token usage critical: %d tokens (threshold: %d). Consider wrapping up to control costs.",
				m.total, m.alertThreshold),
		}, nil
	}
	if !m.warned && m.total >= m.warnThreshold {
		m.warned = true
		return &events.ObserverAlert{
			Source:   m.Name(),
			Severity: events.SeverityWarning,
			Message: fmt.Sprintf("Token usage warning: %d tokens (threshold: %d). Be mindful of remaining budget.",
				m.total, m.warnThreshold),
		}, nil
	}
	return nil, nil
}

// Reset zeroes the counter and re-arms every alert for a fresh session.
func (m *TokenMonitor) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.total, m.warned, m.alerted = 0, false, false
	m.seen = map[string]struct{}{}
}

// billable returns the tokens a usage record charges against the budget.
//
// AGO-DEVIATION: AGo accumulates TotalTokens; Python's _billable_tokens takes
// max(total, prompt+completion). A reported TotalTokens wins here; the
// prompt+completion fallback covers hand-assembled usage (fixtures, custom
// clients) whose total is unset, so the guard cannot silently read zero.
func billable(u events.Usage) int64 {
	if u.TotalTokens != nil {
		return *u.TotalTokens
	}
	var n int64
	if u.PromptTokens != nil {
		n += *u.PromptTokens
	}
	if u.CompletionTokens != nil {
		n += *u.CompletionTokens
	}
	return n
}
