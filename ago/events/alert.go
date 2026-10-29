package events

// Alert events (Python parity: ag2/events/alert.py).

// Severity orders observer alerts.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityError    Severity = "error"
	SeverityCritical Severity = "critical"
	SeverityFatal    Severity = "fatal"
)

// ObserverAlert is emitted by an observer (loop detector, token monitor, ...).
type ObserverAlert struct {
	Base
	Source   string         `json:"source"`
	Severity Severity       `json:"severity"`
	Message  string         `json:"message"`
	Data     map[string]any `json:"data,omitempty"`
}

func (ObserverAlert) EventType() string    { return "ObserverAlert" }
func (ObserverAlert) Transient() bool      { return false }
func (ObserverAlert) Conversational() bool { return false }

// HaltEvent stops the agent turn; the turn scope returns a synthetic "HALTED: <reason>"
// response (parity: _HaltCheckMiddleware).
type HaltEvent struct {
	Base
	Reason string `json:"reason"`
}

func (HaltEvent) EventType() string    { return "HaltEvent" }
func (HaltEvent) Transient() bool      { return false }
func (HaltEvent) Conversational() bool { return false }

func init() {
	RegisterEvent(ObserverAlert{})
	RegisterEvent(HaltEvent{})
}
