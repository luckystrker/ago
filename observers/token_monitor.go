package observers

import (
	"fmt"
	"sync"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
)

// TokenMonitor accumulates billable tokens across UsageEvent telemetry and emits
// one ObserverAlert (severity error) when the accumulated total rises above the
// budget. The alert fires once; subsequent usage does not repeat it (Reset
// re-arms). It reads UsageEvent — the same accounting source as the usage
// package's reports — so every billable unit counts: the main model loop, subtask
// rollups, compaction and aggregation.
type TokenMonitor struct {
	mu      sync.Mutex
	budget  int64
	total   int64
	alerted bool
}

// NewTokenMonitor watches UsageEvent events against budget; the alert fires when
// the accumulated billable total rises strictly above it.
func NewTokenMonitor(budget int64) *TokenMonitor {
	return &TokenMonitor{budget: budget}
}

// Name returns "token_monitor" — the alert source, fixed by the package contract.
func (m *TokenMonitor) Name() string { return "token_monitor" }

// Total returns the accumulated billable token count.
func (m *TokenMonitor) Total() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.total
}

// Register subscribes the monitor on the conversation stream. Alerts are sent
// on the same stream; a send failure is reported to the dispatcher like any
// subscriber error.
func (m *TokenMonitor) Register(ctx *agocontext.Context) error {
	if err := validateTarget(ctx); err != nil {
		return err
	}
	ctx.Stream.Subscribe(func(ev events.Event) error {
		u, ok := events.AsValue(ev).(events.UsageEvent)
		if !ok {
			return nil
		}
		alert, fire := m.observe(u.Usage)
		if !fire {
			return nil
		}
		return ctx.Send(alert)
	}, stream.SubscribeOptions{Condition: events.TypeOf[events.UsageEvent]()})
	return nil
}

// observe folds one usage record into the total and returns the alert to emit,
// if any.
func (m *TokenMonitor) observe(u events.Usage) (*events.ObserverAlert, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.total += billable(u)
	if m.alerted || m.total <= m.budget {
		return nil, false
	}
	m.alerted = true
	return &events.ObserverAlert{
		Source:   m.Name(),
		Severity: events.SeverityError,
		Message: fmt.Sprintf("Token budget exceeded: %d tokens used (budget: %d). Consider wrapping up to control costs.",
			m.total, m.budget),
	}, true
}

// Reset zeroes the counter and re-arms the alert for a fresh session.
func (m *TokenMonitor) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.total, m.alerted = 0, false
}

// billable returns the tokens a usage record charges against the budget.
//
// AGO-DEVIATION: the AGo contract accumulates TotalTokens; Python's
// _billable_tokens takes max(total, prompt+completion). A reported TotalTokens
// wins here; the prompt+completion fallback covers hand-assembled usage
// (fixtures, custom clients) whose total is unset, so the guard cannot silently
// read zero.
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
