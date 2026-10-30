package observers

import (
	"errors"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

func TestTokenMonitorAlerts(t *testing.T) {
	tests := []struct {
		name       string
		budget     int64
		usage      []int64 // TotalTokens of the UsageEvents sent in order
		wantAlerts int
		wantTotal  int64
	}{
		{
			name:       "fires when the total exceeds the budget",
			budget:     1000,
			usage:      []int64{600, 500},
			wantAlerts: 1,
			wantTotal:  1100,
		},
		{
			name:       "exact budget does not fire (strictly greater)",
			budget:     1100,
			usage:      []int64{600, 500},
			wantAlerts: 0,
			wantTotal:  1100,
		},
		{
			name:       "below budget never fires",
			budget:     5000,
			usage:      []int64{100, 200, 300, 400},
			wantAlerts: 0,
			wantTotal:  1000,
		},
		{
			name:       "alert fires once even as usage keeps growing",
			budget:     100,
			usage:      []int64{150, 50, 50},
			wantAlerts: 1,
			wantTotal:  250,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, cx := newConversation()
			alerts := collectAlerts(s)

			monitor := NewTokenMonitor(tt.budget)
			if err := monitor.Register(cx); err != nil {
				t.Fatalf("Register: %v", err)
			}
			for _, total := range tt.usage {
				if err := s.Send(usageEvt(total)); err != nil {
					t.Fatalf("Send(%d): %v", total, err)
				}
			}

			got := alerts()
			if len(got) != tt.wantAlerts {
				t.Fatalf("got %d alerts, want %d", len(got), tt.wantAlerts)
			}
			if monitor.Total() != tt.wantTotal {
				t.Fatalf("Total() = %d, want %d", monitor.Total(), tt.wantTotal)
			}
			if tt.wantAlerts > 0 {
				a := got[0]
				if a.Source != "token_monitor" {
					t.Errorf("Source = %q, want %q", a.Source, "token_monitor")
				}
				if a.Severity != events.SeverityError {
					t.Errorf("Severity = %q, want error", a.Severity)
				}
				if a.Message == "" {
					t.Error("Message is empty")
				}
			}
		})
	}
}

func TestTokenMonitorFallbackWhenTotalUnset(t *testing.T) {
	s, cx := newConversation()
	alerts := collectAlerts(s)

	monitor := NewTokenMonitor(400)
	if err := monitor.Register(cx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Hand-assembled usage without TotalTokens counts prompt+completion
	// instead of reading as zero.
	ev := &events.UsageEvent{
		Usage: events.Usage{PromptTokens: events.I64(300), CompletionTokens: events.I64(200)},
		Kind:  events.UsageKindModelCall,
	}
	if err := s.Send(ev); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if monitor.Total() != 500 {
		t.Fatalf("Total() = %d, want 500", monitor.Total())
	}
	if got := alerts(); len(got) != 1 {
		t.Fatalf("got %d alerts, want 1", len(got))
	}
}

func TestTokenMonitorIgnoresOtherEvents(t *testing.T) {
	s, cx := newConversation()
	alerts := collectAlerts(s)

	monitor := NewTokenMonitor(10)
	if err := monitor.Register(cx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Send(resp("text, not usage")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := s.Send(&events.TextInput{Content: "question"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// A usage record with no counts at all adds zero and must not fire.
	if err := s.Send(&events.UsageEvent{Kind: events.UsageKindModelCall}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if monitor.Total() != 0 {
		t.Fatalf("Total() = %d, want 0", monitor.Total())
	}
	if got := alerts(); len(got) != 0 {
		t.Fatalf("got %d alerts, want 0", len(got))
	}
}

func TestTokenMonitorReset(t *testing.T) {
	s, cx := newConversation()
	alerts := collectAlerts(s)

	monitor := NewTokenMonitor(100)
	if err := monitor.Register(cx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Send(usageEvt(200)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := alerts(); len(got) != 1 {
		t.Fatalf("got %d alerts before reset, want 1", len(got))
	}

	monitor.Reset()
	if monitor.Total() != 0 {
		t.Fatalf("Total() after reset = %d, want 0", monitor.Total())
	}
	if err := s.Send(usageEvt(200)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := alerts(); len(got) != 2 {
		t.Fatalf("got %d alerts after reset, want 2", len(got))
	}
}

func TestTokenMonitorRegisterErrors(t *testing.T) {
	monitor := NewTokenMonitor(100)
	if err := monitor.Register(nil); !errors.Is(err, ErrNilContext) {
		t.Fatalf("Register(nil) = %v, want ErrNilContext", err)
	}
	cx := &agocontext.Context{} // Stream unset
	if err := monitor.Register(cx); !errors.Is(err, ErrNoStream) {
		t.Fatalf("Register(no stream) = %v, want ErrNoStream", err)
	}
}
