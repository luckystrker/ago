package events

// Task lifecycle events (Python parity: ag2/events/task_events.py).

// TaskSpec mirrors the Python TaskSpec dataclass (ag2/task.py) on the event
// wire. It lives here rather than in the task package because task imports
// events — a Spec type owned by task would make the field unreadable without
// an import cycle. JSON names match the Python dataclass fields.
type TaskSpec struct {
	Title       string         `json:"title"`
	Description string         `json:"description,omitempty"`
	Payload     map[string]any `json:"payload"`
	Capability  string         `json:"capability,omitempty"`
}

// TaskStarted announces a tracked task. ParentToolCallID links the task to the
// tool call that requested it (Python parity: run_task stamps
// parent_context.tool_call_id); the Task primitive leaves it empty.
type TaskStarted struct {
	Base
	TaskID           string    `json:"task_id"`
	AgentName        string    `json:"agent_name"`
	Objective        string    `json:"objective,omitempty"`
	ParentToolCallID string    `json:"parent_tool_call_id,omitempty"`
	Spec             *TaskSpec `json:"spec,omitempty"`
	ExpiresAt        string    `json:"expires_at,omitempty"`
}

func (TaskStarted) EventType() string    { return "TaskStarted" }
func (TaskStarted) Transient() bool      { return false }
func (TaskStarted) Conversational() bool { return true }

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

// TaskCompleted closes a task successfully. Usage is the cumulative snapshot
// of the task's stream at completion (Python parity: run_task reports
// reply.usage().total; the Task primitive's own TaskCompleted carries the
// zero value, matching the Python default_factory).
type TaskCompleted struct {
	Base
	TaskID     string `json:"task_id"`
	AgentName  string `json:"agent_name"`
	Objective  string `json:"objective,omitempty"`
	Result     string `json:"result,omitempty"`
	TaskStream string `json:"task_stream,omitempty"`
	Usage      Usage  `json:"usage"`
}

func (TaskCompleted) EventType() string    { return "TaskCompleted" }
func (TaskCompleted) Transient() bool      { return false }
func (TaskCompleted) Conversational() bool { return true }

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
func (TaskFailed) Conversational() bool { return true }

// TaskExpired closes a task past its TTL.
type TaskExpired struct {
	Base
	TaskID    string `json:"task_id"`
	AgentName string `json:"agent_name"`
}

func (TaskExpired) EventType() string    { return "TaskExpired" }
func (TaskExpired) Transient() bool      { return false }
func (TaskExpired) Conversational() bool { return true }

// TaskCancelled closes a task on request.
type TaskCancelled struct {
	Base
	TaskID    string `json:"task_id"`
	AgentName string `json:"agent_name"`
	Reason    string `json:"reason,omitempty"`
}

func (TaskCancelled) EventType() string    { return "TaskCancelled" }
func (TaskCancelled) Transient() bool      { return false }
func (TaskCancelled) Conversational() bool { return true }

func init() {
	RegisterEvent(TaskStarted{})
	RegisterEvent(TaskProgress{})
	RegisterEvent(TaskCompleted{})
	RegisterEvent(TaskFailed{})
	RegisterEvent(TaskExpired{})
	RegisterEvent(TaskCancelled{})
}
