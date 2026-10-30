package observers

import (
	"errors"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

func TestLoopDetectorAlerts(t *testing.T) {
	tests := []struct {
		name       string
		repeats    int
		sequence   []string // response contents sent in order
		wantAlerts int
	}{
		{
			name:       "fires on the repeats-th identical response",
			repeats:    3,
			sequence:   []string{"a", "a", "a"},
			wantAlerts: 1,
		},
		{
			name:       "no alert below the threshold",
			repeats:    3,
			sequence:   []string{"a", "a"},
			wantAlerts: 0,
		},
		{
			name:       "different content resets the run",
			repeats:    3,
			sequence:   []string{"a", "a", "b", "a", "a"},
			wantAlerts: 0,
		},
		{
			name:       "run restarts after a different response",
			repeats:    3,
			sequence:   []string{"a", "a", "b", "a", "a", "a"},
			wantAlerts: 1,
		},
		{
			name:       "dedup: second run of the same content stays silent",
			repeats:    2,
			sequence:   []string{"a", "a", "b", "a", "a"},
			wantAlerts: 1,
		},
		{
			name:       "distinct contents never alert",
			repeats:    2,
			sequence:   []string{"a", "b", "c", "d", "e"},
			wantAlerts: 0,
		},
		{
			name:       "alternating contents never alert",
			repeats:    2,
			sequence:   []string{"a", "b", "a", "b", "a", "b"},
			wantAlerts: 0,
		},
		{
			name:       "empty content is not a loop signal",
			repeats:    2,
			sequence:   []string{"", "", "   ", "\n\t"},
			wantAlerts: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, cx := newConversation()
			alerts := collectAlerts(s)

			detector := NewLoopDetector(tt.repeats)
			if err := detector.Register(cx); err != nil {
				t.Fatalf("Register: %v", err)
			}
			for _, content := range tt.sequence {
				if err := s.Send(resp(content)); err != nil {
					t.Fatalf("Send(%q): %v", content, err)
				}
			}
			if got := len(alerts()); got != tt.wantAlerts {
				t.Fatalf("got %d alerts, want %d", got, tt.wantAlerts)
			}
		})
	}
}

func TestLoopDetectorAlertShape(t *testing.T) {
	s, cx := newConversation()
	alerts := collectAlerts(s)

	detector := NewLoopDetector(3)
	if err := detector.Register(cx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Send(resp("please reconsider")); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	got := alerts()
	if len(got) != 1 {
		t.Fatalf("got %d alerts, want 1", len(got))
	}
	a := got[0]
	if a.Source != "loop_detector" {
		t.Errorf("Source = %q, want %q", a.Source, "loop_detector")
	}
	if a.Severity != events.SeverityWarning {
		t.Errorf("Severity = %q, want warning", a.Severity)
	}
	if a.Message == "" {
		t.Error("Message is empty")
	}
}

func TestLoopDetectorNormalizesWhitespace(t *testing.T) {
	s, cx := newConversation()
	alerts := collectAlerts(s)

	detector := NewLoopDetector(2)
	if err := detector.Register(cx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Send(resp("hello   wide\nworld")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := s.Send(resp(" hello wide world ")); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := alerts(); len(got) != 1 {
		t.Fatalf("got %d alerts, want 1 (whitespace-normalized contents are identical)", len(got))
	}
}

func TestLoopDetectorIgnoresOtherEvents(t *testing.T) {
	s, cx := newConversation()
	alerts := collectAlerts(s)

	detector := NewLoopDetector(2)
	if err := detector.Register(cx); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Foreign events interleave with the identical responses; they neither
	// count toward the run nor break it, and the detector must not panic on
	// value-form events.
	if err := s.Send(&events.TextInput{Content: "question"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := s.Send(events.ModelResponse{Message: &events.ModelMessage{Content: "loop"}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := s.Send(usageEvt(10)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := s.Send(resp("loop")); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := alerts(); len(got) != 1 {
		t.Fatalf("got %d alerts, want 1", len(got))
	}
}

func TestLoopDetectorReset(t *testing.T) {
	s, cx := newConversation()
	alerts := collectAlerts(s)

	detector := NewLoopDetector(2)
	if err := detector.Register(cx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := s.Send(resp("again")); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	if got := alerts(); len(got) != 1 {
		t.Fatalf("got %d alerts before reset, want 1", len(got))
	}

	detector.Reset()
	for i := 0; i < 2; i++ {
		if err := s.Send(resp("again")); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	if got := alerts(); len(got) != 2 {
		t.Fatalf("got %d alerts after reset, want 2 (dedup memory cleared)", len(got))
	}
}

func TestLoopDetectorRegisterErrors(t *testing.T) {
	detector := NewLoopDetector(3)
	if err := detector.Register(nil); !errors.Is(err, ErrNilContext) {
		t.Fatalf("Register(nil) = %v, want ErrNilContext", err)
	}
	cx := &agocontext.Context{} // Stream unset
	if err := detector.Register(cx); !errors.Is(err, ErrNoStream) {
		t.Fatalf("Register(no stream) = %v, want ErrNoStream", err)
	}
}

func TestLoopDetectorClampsRepeats(t *testing.T) {
	if got := NewLoopDetector(0).Repeats(); got != 2 {
		t.Fatalf("NewLoopDetector(0).Repeats() = %d, want 2", got)
	}
	if got := NewLoopDetector(5).Repeats(); got != 5 {
		t.Fatalf("NewLoopDetector(5).Repeats() = %d, want 5", got)
	}
}
