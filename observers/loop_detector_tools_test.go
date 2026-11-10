package observers

import (
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/ago/events"
)

// toolCall builds a ToolCallEvent with the given name and argument JSON.
func toolCall(name, args string) events.ToolCallEvent {
	return events.ToolCallEvent{ID: name + "-call", Name: name, Arguments: args}
}

// TestLoopDetectorToolCalls pins Python parity: the same tool called with the
// same arguments repeatedly fires one warning naming the tool.
func TestLoopDetectorToolCalls(t *testing.T) {
	tests := []struct {
		name       string
		repeats    int
		calls      []events.ToolCallEvent
		wantAlerts int
	}{
		{
			name:       "same call three times",
			repeats:    3,
			calls:      []events.ToolCallEvent{toolCall("search", `{"q":"x"}`), toolCall("search", `{"q":"x"}`), toolCall("search", `{"q":"x"}`)},
			wantAlerts: 1,
		},
		{
			name:       "below the threshold",
			repeats:    3,
			calls:      []events.ToolCallEvent{toolCall("search", `{"q":"x"}`), toolCall("search", `{"q":"x"}`)},
			wantAlerts: 0,
		},
		{
			name:    "different arguments break the run",
			repeats: 2,
			calls: []events.ToolCallEvent{
				toolCall("search", `{"q":"x"}`),
				toolCall("search", `{"q":"y"}`),
				toolCall("search", `{"q":"x"}`),
			},
			wantAlerts: 0,
		},
		{
			name:    "different tools break the run",
			repeats: 2,
			calls: []events.ToolCallEvent{
				toolCall("search", `{"q":"x"}`),
				toolCall("fetch", `{"q":"x"}`),
			},
			wantAlerts: 0,
		},
		{
			name:       "non-consecutive repetitions inside the window do not fire",
			repeats:    2,
			calls:      []events.ToolCallEvent{toolCall("search", `{"q":"x"}`), toolCall("other", `{}`), toolCall("search", `{"q":"x"}`)},
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
			for _, call := range tt.calls {
				if err := s.Send(call); err != nil {
					t.Fatalf("Send: %v", err)
				}
			}
			if got := len(alerts()); got != tt.wantAlerts {
				t.Fatalf("alerts = %d, want %d", got, tt.wantAlerts)
			}
		})
	}
}

// TestLoopDetectorToolAlertShape pins the Python message and source.
func TestLoopDetectorToolAlertShape(t *testing.T) {
	s, cx := newConversation()
	alerts := collectAlerts(s)
	detector := NewLoopDetector(3)
	if err := detector.Register(cx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Send(toolCall("search", `{"q":"x"}`)); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	got := alerts()
	if len(got) != 1 {
		t.Fatalf("alerts = %d, want 1", len(got))
	}
	a := got[0]
	if a.Source != "loop_detector" {
		t.Errorf("Source = %q, want loop_detector", a.Source)
	}
	if a.Severity != events.SeverityWarning {
		t.Errorf("Severity = %q, want warning", a.Severity)
	}
	if !strings.Contains(a.Message, "tool 'search'") || !strings.Contains(a.Message, "3 times") {
		t.Errorf("Message = %q, want the Python tool-loop wording", a.Message)
	}
	if a.Data["task_id"] != nil {
		t.Errorf("local alert data = %v, want none", a.Data)
	}
}

// TestLoopDetectorToolWindowSize pins window_size: a window shorter than the
// threshold can never accumulate enough history to fire.
func TestLoopDetectorToolWindowSize(t *testing.T) {
	s, cx := newConversation()
	alerts := collectAlerts(s)
	detector := NewLoopDetector(3, WithLoopWindowSize(2))
	if err := detector.Register(cx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for i := 0; i < 6; i++ {
		if err := s.Send(toolCall("search", `{"q":"x"}`)); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	if got := len(alerts()); got != 0 {
		t.Fatalf("alerts = %d, want 0 for a window shorter than the threshold", got)
	}
	if detector.WindowSize() != 2 {
		t.Fatalf("WindowSize() = %d, want 2", detector.WindowSize())
	}
}

// TestLoopDetectorWindowSizeZeroKeepsDefault pins the option guard: a
// non-positive window keeps the default instead of disabling history, and a
// positive value applies.
func TestLoopDetectorWindowSizeZeroKeepsDefault(t *testing.T) {
	if got := NewLoopDetector(2, WithLoopWindowSize(0)).WindowSize(); got != defaultLoopWindow {
		t.Fatalf("WindowSize() = %d, want the default %d", got, defaultLoopWindow)
	}
	if got := NewLoopDetector(2, WithLoopWindowSize(-5)).WindowSize(); got != defaultLoopWindow {
		t.Fatalf("WindowSize() = %d for -5, want the default %d", got, defaultLoopWindow)
	}
	if got := NewLoopDetector(2, WithLoopWindowSize(3)).WindowSize(); got != 3 {
		t.Fatalf("WindowSize() = %d, want 3", got)
	}

	// The default window is usable: two identical calls still fire.
	s, cx := newConversation()
	alerts := collectAlerts(s)
	detector := NewLoopDetector(2, WithLoopWindowSize(0))
	if err := detector.Register(cx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := s.Send(toolCall("search", `{"q":"x"}`)); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	if got := len(alerts()); got != 1 {
		t.Fatalf("alerts = %d, want 1 with the default window", got)
	}
}

// TestPreviewTruncationBoundary pins the boundary: content of exactly
// previewLimit runes is returned verbatim, one rune more is shortened. Counting
// is by rune, so a multi-byte boundary is never split.
func TestPreviewTruncationBoundary(t *testing.T) {
	exact := strings.Repeat("ж", previewLimit)
	if got := preview(exact); got != exact {
		t.Fatalf("preview(%d runes) changed content: %d runes", previewLimit, len([]rune(got)))
	}
	longer := strings.Repeat("ж", previewLimit+1)
	want := strings.Repeat("ж", previewLimit) + "…"
	if got := preview(longer); got != want {
		t.Fatalf("preview(%d runes) = %q, want the first %d runes plus an ellipsis", previewLimit+1, got, previewLimit)
	}
	if got := preview("short"); got != "short" {
		t.Fatalf("preview(short) = %q", got)
	}
}

// TestLoopDetectorReset pins that Reset clears tool-call history and flags too.
func TestLoopDetectorToolReset(t *testing.T) {
	d := NewLoopDetector(2)
	s, cx := newConversation()
	alerts := collectAlerts(s)
	if err := d.Register(cx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := s.Send(toolCall("search", `{"q":"x"}`)); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	if got := len(alerts()); got != 1 {
		t.Fatalf("alerts = %d, want 1", got)
	}
	d.Reset()
	for i := 0; i < 2; i++ {
		if err := s.Send(toolCall("search", `{"q":"x"}`)); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	if got := len(alerts()); got != 2 {
		t.Fatalf("alerts after Reset = %d, want 2", got)
	}
}

// TestLoopDetectorToolDedup pins that a flagged call stays quiet until Reset
// and speaks again afterwards.
func TestLoopDetectorToolDedup(t *testing.T) {
	s, cx := newConversation()
	alerts := collectAlerts(s)
	detector := NewLoopDetector(2)
	if err := detector.Register(cx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	sendPair := func() {
		for i := 0; i < 2; i++ {
			if err := s.Send(toolCall("search", `{"q":"x"}`)); err != nil {
				t.Fatalf("Send: %v", err)
			}
		}
	}
	sendPair()
	if got := len(alerts()); got != 1 {
		t.Fatalf("alerts = %d, want 1", got)
	}
	sendPair()
	if got := len(alerts()); got != 1 {
		t.Fatalf("alerts = %d, want 1 while deduplicated", got)
	}
	detector.Reset()
	sendPair()
	if got := len(alerts()); got != 2 {
		t.Fatalf("alerts = %d, want 2 after Reset", got)
	}
}

// TestLoopDetectorToolCallInChildEvent pins delegation provenance: a
// descendant's repeated call alerts under its own task id and path, and a
// sibling's history stays separate.
func TestLoopDetectorToolCallInChildEvent(t *testing.T) {
	s, cx := newConversation()
	alerts := collectAlerts(s)
	detector := NewLoopDetector(2)
	if err := detector.Register(cx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	child := func(task string, path []string) *events.ChildEvent {
		ev := toolCall("search", `{"q":"x"}`)
		return events.NewChildEvent(&ev, task, path, "leaf", task+"-event")
	}
	for i := 0; i < 2; i++ {
		if err := s.Send(child("a", []string{"root", "a"})); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	got := alerts()
	if len(got) != 1 {
		t.Fatalf("alerts = %d, want 1", len(got))
	}
	if got[0].Data["task_id"] != "a" {
		t.Fatalf("task_id = %v, want a", got[0].Data["task_id"])
	}
	if path, ok := got[0].Data["path"].([]string); !ok || len(path) != 2 || path[1] != "a" {
		t.Fatalf("path = %v", got[0].Data["path"])
	}

	// One call from sibling b must not combine with a's repeated calls.
	if err := s.Send(child("b", []string{"root", "b"})); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := len(alerts()); got != 1 {
		t.Fatalf("alerts = %d after sibling, want 1", got)
	}
}

// TestLoopDetectorTimeoutOption pins the per-call deadline knob: a
// non-positive value leaves the observer unbounded (off), a positive value
// applies.
func TestLoopDetectorTimeoutOption(t *testing.T) {
	if got := NewLoopDetector(2).timeout; got != 0 {
		t.Fatalf("default timeout = %v, want 0 (off)", got)
	}
	if got := NewLoopDetector(2, WithLoopTimeout(0)).timeout; got != 0 {
		t.Fatalf("timeout for 0 = %v, want the default 0", got)
	}
	if got := NewLoopDetector(2, WithLoopTimeout(-time.Second)).timeout; got != 0 {
		t.Fatalf("timeout for -1s = %v, want the default 0", got)
	}
	if got := NewLoopDetector(2, WithLoopTimeout(25*time.Millisecond)).timeout; got != 25*time.Millisecond {
		t.Fatalf("timeout = %v, want 25ms", got)
	}
}

// TestLoopDetectorNameOption pins the guard: an empty name keeps the default.
func TestLoopDetectorNameOption(t *testing.T) {
	if got := NewLoopDetector(2, WithLoopName("")).Name(); got != "loop_detector" {
		t.Fatalf("Name() = %q, want the default loop_detector", got)
	}
}

// TestLoopDetectorOptions pins the remaining Python knobs.
func TestLoopDetectorOptions(t *testing.T) {
	detector := NewLoopDetector(4, WithLoopName("watcher"), WithLoopTimeout(25*time.Millisecond), WithLoopWindowSize(0))
	if detector.Name() != "watcher" {
		t.Fatalf("Name() = %q, want watcher", detector.Name())
	}
	if detector.timeout != 25*time.Millisecond {
		t.Fatalf("timeout = %v, want 25ms", detector.timeout)
	}
	if detector.WindowSize() != defaultLoopWindow {
		t.Fatalf("WindowSize() = %d, want the default %d for a non-positive option", detector.WindowSize(), defaultLoopWindow)
	}

	s, cx := newConversation()
	alerts := collectAlerts(s)
	named := NewLoopDetector(2, WithLoopName("watcher"))
	if err := named.Register(cx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := s.Send(toolCall("search", `{"q":"x"}`)); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	got := alerts()
	if len(got) != 1 || got[0].Source != "watcher" {
		t.Fatalf("alerts = %v, want one from watcher", got)
	}
}
