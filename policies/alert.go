package policies

import (
	"errors"
	"fmt"
	"sync"

	agocontext "ago/context"
	"ago/events"
)

// alertKey identifies an alert by semantic content, so dedup survives history
// replay, compaction and serialization round-trips (Python parity: alert.py
// delivered keys on (source, severity, message)).
type alertKey struct {
	source   string
	severity string
	message  string
}

// AlertPolicy delivers observer alerts to the model. Unseen ObserverAlert
// events are appended to the prompts, one line each; a fatal alert halts the
// turn through a single HaltEvent sent on the conversation stream.
//
// Each alert is delivered once: a repeated Apply that sees the same
// (source, severity, message) in the history is dropped, so cross-turn
// accumulation does not duplicate injections. Safe for concurrent use.
type AlertPolicy struct {
	mu        sync.Mutex
	delivered map[alertKey]bool
}

// NewAlertPolicy returns an alert policy with an empty delivery record.
func NewAlertPolicy() *AlertPolicy { return &AlertPolicy{delivered: map[alertKey]bool{}} }

// Apply injects prompt lines for unseen non-fatal alerts and sends one
// HaltEvent naming the first unseen fatal alert. The event list passes through
// unchanged.
func (p *AlertPolicy) Apply(prompts []string, evs []events.Event, ctx *agocontext.Context) ([]string, []events.Event, error) {
	p.mu.Lock()
	if p.delivered == nil {
		p.delivered = map[alertKey]bool{}
	}
	var fresh []events.ObserverAlert
	for _, ev := range evs {
		a, ok := events.AsValue(ev).(events.ObserverAlert)
		if !ok {
			continue
		}
		k := alertKey{source: a.Source, severity: string(a.Severity), message: a.Message}
		if p.delivered[k] {
			continue
		}
		p.delivered[k] = true
		fresh = append(fresh, a)
	}
	p.mu.Unlock()

	for _, a := range fresh {
		if a.Severity == events.SeverityFatal {
			break
		}
		prompts = append(prompts, fmt.Sprintf("alert(%s) from %s: %s", a.Severity, a.Source, a.Message))
	}
	for _, a := range fresh {
		if a.Severity != events.SeverityFatal {
			continue
		}
		if ctx == nil {
			return nil, nil, errors.New("policies: fatal alert without a context to send HaltEvent on")
		}
		if err := ctx.Send(&events.HaltEvent{Reason: "FATAL: " + a.Message}); err != nil {
			return nil, nil, fmt.Errorf("policies: send halt event: %w", err)
		}
		break // one halt per Apply, naming the first fatal alert (Python parity)
	}
	return prompts, evs, nil
}
