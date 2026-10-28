package events

// Task lifecycle events (Python parity: ag2/events/task_events.py).

// TaskStarted announces a tracked task.
type TaskStarted struct {
	Base
	TaskID    string `json:"task_id"`
	AgentName string `json:"agent_name"`
	Objective string `json:"objective,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

func (TaskStarted) EventType() string    { return "TaskStarted" }
func (TaskStarted) Transient() bool      { return false }
func (TaskStarted) Conversational() bool { return false }

// TaskProgress is a mid-flight progress update (transient).
type TaskProgress struct {
	Base
	TaskID    string         `json:"task_id"`
	AgentName string         `json:"agent_name"`
	Objective string         `json:"objective,omitempty"`
	Content   string         `json:"content,omitempty"`
	Payload   map[string]any `json:"payload,omitempty"`
}

func (TaskProgress) EventType() string    { return "TaskProgress" }
func (TaskProgress) Transient() bool      { return true }
func (TaskProgress) Conversational() bool { return false }

// TaskCompleted closes a task successfully.
type TaskCompleted struct {
	Base
	TaskID     string `json:"task_id"`
	AgentName  string `json:"agent_name"`
	Objective  string `json:"objective,omitempty"`
	Result     string `json:"result,omitempty"`
	TaskStream string `json:"task_stream,omitempty"`
}

func (TaskCompleted) EventType() string    { return "TaskCompleted" }
func (TaskCompleted) Transient() bool      { return false }
func (TaskCompleted) Conversational() bool { return false }

// TaskFailed closes a task with an error.
type TaskFailed struct {
	Base
	TaskID    string `json:"task_id"`
	AgentName string `json:"agent_name"`
	Objective string `json:"objective,omitempty"`
	Error     string `json:"error"`
}

func (TaskFailed) EventType() string    { return "TaskFailed" }
func (TaskFailed) Transient() bool      { return false }
func (TaskFailed) Conversational() bool { return false }

// TaskExpired closes a task past its TTL.
type TaskExpired struct {
	Base
	TaskID    string `json:"task_id"`
	AgentName string `json:"agent_name"`
}

func (TaskExpired) EventType() string    { return "TaskExpired" }
func (TaskExpired) Transient() bool      { return false }
func (TaskExpired) Conversational() bool { return false }

// TaskCancelled closes a task on request.
type TaskCancelled struct {
	Base
	TaskID    string `json:"task_id"`
	AgentName string `json:"agent_name"`
	Reason    string `json:"reason,omitempty"`
}

func (TaskCancelled) EventType() string    { return "TaskCancelled" }
func (TaskCancelled) Transient() bool      { return false }
func (TaskCancelled) Conversational() bool { return false }

func init() {
	RegisterEvent(TaskStarted{})
	RegisterEvent(TaskProgress{})
	RegisterEvent(TaskCompleted{})
	RegisterEvent(TaskFailed{})
	RegisterEvent(TaskExpired{})
	RegisterEvent(TaskCancelled{})
}
