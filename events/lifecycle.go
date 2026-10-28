package events

// Lifecycle, alert and compaction-summary events (Python parity: ag2/events/lifecycle.py,
// alert.py; CompactionSummary from _replay/history shaping).

// CompactionSummary replaces a span of older events in the transcript before an LLM call.
type CompactionSummary struct {
	Base
	Content string `json:"content"`
}

func (CompactionSummary) EventType() string    { return "CompactionSummary" }
func (CompactionSummary) Transient() bool      { return false }
func (CompactionSummary) Conversational() bool { return true }

// ObserverStarted / ObserverCompleted bracket an observer's registration on a turn.
type ObserverStarted struct {
	Base
	Name string `json:"name"`
}

func (ObserverStarted) EventType() string    { return "ObserverStarted" }
func (ObserverStarted) Transient() bool      { return true }
func (ObserverStarted) Conversational() bool { return false }

type ObserverCompleted struct {
	Base
	Name string `json:"name"`
}

func (ObserverCompleted) EventType() string    { return "ObserverCompleted" }
func (ObserverCompleted) Transient() bool      { return true }
func (ObserverCompleted) Conversational() bool { return false }

// CompactionStarted / CompactionCompleted / CompactionFailed report history compaction.
type CompactionStarted struct {
	Base
	Agent      string `json:"agent"`
	Strategy   string `json:"strategy"`
	EventCount int    `json:"event_count"`
}

func (CompactionStarted) EventType() string    { return "CompactionStarted" }
func (CompactionStarted) Transient() bool      { return true }
func (CompactionStarted) Conversational() bool { return false }

type CompactionCompleted struct {
	Base
	Agent        string `json:"agent"`
	Strategy     string `json:"strategy"`
	EventsBefore int    `json:"events_before"`
	EventsAfter  int    `json:"events_after"`
	LLMCalls     int    `json:"llm_calls"`
	Usage        Usage  `json:"usage"`
}

func (CompactionCompleted) EventType() string    { return "CompactionCompleted" }
func (CompactionCompleted) Transient() bool      { return true }
func (CompactionCompleted) Conversational() bool { return false }

type CompactionFailed struct {
	Base
	Agent     string `json:"agent"`
	Strategy  string `json:"strategy"`
	ErrorType string `json:"error_type"`
	Error     string `json:"error"`
}

func (CompactionFailed) EventType() string    { return "CompactionFailed" }
func (CompactionFailed) Transient() bool      { return true }
func (CompactionFailed) Conversational() bool { return false }

// AggregationStarted / AggregationCompleted / AggregationFailed report child-stream aggregation.
type AggregationStarted struct {
	Base
	Agent string `json:"agent"`
}

func (AggregationStarted) EventType() string    { return "AggregationStarted" }
func (AggregationStarted) Transient() bool      { return true }
func (AggregationStarted) Conversational() bool { return false }

type AggregationCompleted struct {
	Base
	Agent        string `json:"agent"`
	EventsBefore int    `json:"events_before"`
	EventsAfter  int    `json:"events_after"`
	LLMCalls     int    `json:"llm_calls"`
	Usage        Usage  `json:"usage"`
}

func (AggregationCompleted) EventType() string    { return "AggregationCompleted" }
func (AggregationCompleted) Transient() bool      { return true }
func (AggregationCompleted) Conversational() bool { return false }

type AggregationFailed struct {
	Base
	Agent     string `json:"agent"`
	ErrorType string `json:"error_type"`
	Error     string `json:"error"`
}

func (AggregationFailed) EventType() string    { return "AggregationFailed" }
func (AggregationFailed) Transient() bool      { return true }
func (AggregationFailed) Conversational() bool { return false }

// EventLogFailed reports that persisting an event to the log failed.
type EventLogFailed struct {
	Base
	Agent     string `json:"agent"`
	ErrorType string `json:"error_type"`
	Error     string `json:"error"`
}

func (EventLogFailed) EventType() string    { return "EventLogFailed" }
func (EventLogFailed) Transient() bool      { return true }
func (EventLogFailed) Conversational() bool { return false }

// UnknownEvent wraps an event whose type is not in the registry (forward compatibility).
type UnknownEvent struct {
	Base
	TypeName string         `json:"type_name"`
	Data     map[string]any `json:"data"`
}

func (UnknownEvent) EventType() string    { return "UnknownEvent" }
func (UnknownEvent) Transient() bool      { return false }
func (UnknownEvent) Conversational() bool { return false }

func init() {
	RegisterEvent(CompactionSummary{})
	RegisterEvent(ObserverStarted{})
	RegisterEvent(ObserverCompleted{})
	RegisterEvent(CompactionStarted{})
	RegisterEvent(CompactionCompleted{})
	RegisterEvent(CompactionFailed{})
	RegisterEvent(AggregationStarted{})
	RegisterEvent(AggregationCompleted{})
	RegisterEvent(AggregationFailed{})
	RegisterEvent(EventLogFailed{})
	RegisterEvent(UnknownEvent{})
}
