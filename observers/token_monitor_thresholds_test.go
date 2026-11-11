package observers

import (
	"testing"
	"time"

	"github.com/luckystrker/ago/events"
)

// TestTokenMonitorThresholds pins the Python two-threshold guard: warning at
// warn_threshold, critical at alert_threshold, each once, with "at or above".
func TestTokenMonitorThresholds(t *testing.T) {
	tests := []struct {
		name      string
		warn      int64
		alert     int64
		usage     []int64
		severity  []events.Severity // alerts in arrival order
		wantTotal int64
	}{
		{
			name:      "below both thresholds is silent",
			warn:      50_000,
			alert:     100_000,
			usage:     []int64{10_000, 20_000},
			wantTotal: 30_000,
		},
		{
			name:      "warning fires at exactly the warn threshold",
			warn:      50_000,
			alert:     100_000,
			usage:     []int64{50_000},
			severity:  []events.Severity{events.SeverityWarning},
			wantTotal: 50_000,
		},
		{
			name:      "warning fires once",
			warn:      50_000,
			alert:     100_000,
			usage:     []int64{60_000, 1_000},
			severity:  []events.Severity{events.SeverityWarning},
			wantTotal: 61_000,
		},
		{
			name:      "critical fires once, then the outstanding warning follows",
			warn:      50_000,
			alert:     100_000,
			usage:     []int64{110_000, 1_000},
			severity:  []events.Severity{events.SeverityCritical, events.SeverityWarning},
			wantTotal: 111_000,
		},
		{
			name:      "warning then critical",
			warn:      50_000,
			alert:     100_000,
			usage:     []int64{60_000, 50_000},
			severity:  []events.Severity{events.SeverityWarning, events.SeverityCritical},
			wantTotal: 110_000,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, cx := newConversation()
			alerts := collectAlerts(s)
			monitor := NewTokenMonitorThresholds(tt.warn, tt.alert)
			if err := monitor.Register(cx); err != nil {
				t.Fatalf("Register: %v", err)
			}
			for _, total := range tt.usage {
				if err := s.Send(usageEvt(total)); err != nil {
					t.Fatalf("Send(%d): %v", total, err)
				}
			}
			got := alerts()
			if len(got) != len(tt.severity) {
				t.Fatalf("alerts = %d, want %d: %v", len(got), len(tt.severity), got)
			}
			for i, want := range tt.severity {
				if got[i].Severity != want {
					t.Fatalf("alert %d severity = %q, want %q", i, got[i].Severity, want)
				}
			}
			if monitor.Total() != tt.wantTotal {
				t.Fatalf("Total() = %d, want %d", monitor.Total(), tt.wantTotal)
			}
		})
	}
}

// TestTokenMonitorThresholdsDefaults pins the Python default constants.
func TestTokenMonitorThresholdsDefaults(t *testing.T) {
	if DefaultWarnThreshold != 50_000 || DefaultAlertThreshold != 100_000 {
		t.Fatalf("defaults = %d/%d, want 50000/100000", DefaultWarnThreshold, DefaultAlertThreshold)
	}
}

// TestTokenMonitorThresholdsReset pins that Reset re-arms both alerts.
func TestTokenMonitorThresholdsReset(t *testing.T) {
	s, cx := newConversation()
	alerts := collectAlerts(s)
	monitor := NewTokenMonitorThresholds(10, 20)
	if err := monitor.Register(cx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Send(usageEvt(25)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := alerts(); len(got) != 1 || got[0].Severity != events.SeverityCritical {
		t.Fatalf("alerts = %v, want one critical", got)
	}
	monitor.Reset()
	if monitor.Total() != 0 {
		t.Fatalf("Total() after Reset = %d, want 0", monitor.Total())
	}
	if err := s.Send(usageEvt(25)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := alerts()
	if len(got) != 2 || got[1].Severity != events.SeverityCritical {
		t.Fatalf("alerts after Reset = %v, want a second critical", got)
	}
}

// TestTokenMonitorOptions pins the name and timeout knobs.
func TestTokenMonitorOptions(t *testing.T) {
	monitor := NewTokenMonitorThresholds(1, 2, WithTokenMonitorName("costs"), WithTokenMonitorTimeout(25*time.Millisecond))
	if monitor.Name() != "costs" {
		t.Fatalf("Name() = %q, want costs", monitor.Name())
	}
	if monitor.timeout != 25*time.Millisecond {
		t.Fatalf("timeout = %v, want 25ms", monitor.timeout)
	}

	s, cx := newConversation()
	alerts := collectAlerts(s)
	named := NewTokenMonitor(10, WithTokenMonitorName("costs"))
	if err := named.Register(cx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Send(usageEvt(11)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := alerts()
	if len(got) != 1 || got[0].Source != "costs" {
		t.Fatalf("alerts = %v, want one from costs", got)
	}
}
