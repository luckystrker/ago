package events

import "encoding/json"

// Usage is the normalized token record (Python parity: field-wise add, nil = not reported).
// AGO-DEVIATION: Python uses float|None; token counts are integers, so AGo uses *int64 (D-006).
type Usage struct {
	PromptTokens             *int64 `json:"prompt_tokens,omitempty"`
	CompletionTokens         *int64 `json:"completion_tokens,omitempty"`
	TotalTokens              *int64 `json:"total_tokens,omitempty"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens,omitempty"`
	ThinkingTokens           *int64 `json:"thinking_tokens,omitempty"`
}

func addPtr(a, b *int64) *int64 {
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		return b
	case b == nil:
		return a
	}
	s := *a + *b
	return &s
}

// Add returns the field-wise sum; nil+nil stays nil.
func (u Usage) Add(o Usage) Usage {
	return Usage{
		PromptTokens:             addPtr(u.PromptTokens, o.PromptTokens),
		CompletionTokens:         addPtr(u.CompletionTokens, o.CompletionTokens),
		TotalTokens:              addPtr(u.TotalTokens, o.TotalTokens),
		CacheReadInputTokens:     addPtr(u.CacheReadInputTokens, o.CacheReadInputTokens),
		CacheCreationInputTokens: addPtr(u.CacheCreationInputTokens, o.CacheCreationInputTokens),
		ThinkingTokens:           addPtr(u.ThinkingTokens, o.ThinkingTokens),
	}
}

func I64(v int64) *int64 { return &v }

// BinaryResult is binary content produced by a model call (e.g. generated images).
type BinaryResult struct {
	Data     []byte         `json:"data"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Name returns the filename recorded in metadata ("" if absent).
func (b BinaryResult) Name() string {
	if s, ok := b.Metadata["filename"].(string); ok {
		return s
	}
	return ""
}

// --- model events -----------------------------------------------------------

// ModelMessage is the assistant turn content (transient: the durable copy lives on ModelResponse).
type ModelMessage struct {
	Base
	Content  string         `json:"content"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

func (ModelMessage) EventType() string    { return "ModelMessage" }
func (ModelMessage) Transient() bool      { return true }
func (ModelMessage) Conversational() bool { return false }

// ModelMessageChunk is a streaming content delta.
type ModelMessageChunk struct {
	Base
	Content string `json:"content"`
}

func (ModelMessageChunk) EventType() string    { return "ModelMessageChunk" }
func (ModelMessageChunk) Transient() bool      { return true }
func (ModelMessageChunk) Conversational() bool { return false }

// ModelReasoning is a streaming reasoning/thinking delta.
type ModelReasoning struct {
	Base
	Content string `json:"content"`
}

func (ModelReasoning) EventType() string    { return "ModelReasoning" }
func (ModelReasoning) Transient() bool      { return true }
func (ModelReasoning) Conversational() bool { return false }

// ModelResponse is the model's answer for one LLM call. It is the durable transcript
// entry and the terminal value of the agent turn loop. ResponseForce=true means a
// final=true tool forced this response, which ends the loop without another model call.
type ModelResponse struct {
	Base
	Message       *ModelMessage  `json:"message,omitempty"`
	ToolCalls     ToolCallsEvent `json:"tool_calls"`
	Usage         Usage          `json:"usage"`
	ResponseForce bool           `json:"response_force,omitempty"`
	Files         []BinaryResult `json:"files,omitempty"`

	// tracing
	Model        string `json:"model,omitempty"`
	Provider     string `json:"provider,omitempty"`
	FinishReason string `json:"finish_reason,omitempty"`
	ResponseID   string `json:"response_id,omitempty"`
}

func (ModelResponse) EventType() string    { return "ModelResponse" }
func (ModelResponse) Transient() bool      { return false }
func (ModelResponse) Conversational() bool { return true }

// Content returns the text of the message ("" when the response carries none).
func (r ModelResponse) Content() string {
	if r.Message == nil {
		return ""
	}
	return r.Message.Content
}

// Usage kinds (Python UsageEvent.kind).
const (
	UsageKindModelCall   = "model_call"
	UsageKindSubtask     = "subtask"
	UsageKindCompaction  = "compaction"
	UsageKindAggregation = "aggregation"
)

// UsageEvent is persisted token telemetry; not part of the conversational transcript.
type UsageEvent struct {
	Base
	Usage        Usage        `json:"usage"`
	Kind         string       `json:"kind"`
	Model        string       `json:"model,omitempty"`
	Provider     string       `json:"provider,omitempty"`
	FinishReason string       `json:"finish_reason,omitempty"`
	Label        string       `json:"label,omitempty"`
	Parts        []UsageEvent `json:"parts,omitempty"`
}

func (UsageEvent) EventType() string    { return "UsageEvent" }
func (UsageEvent) Transient() bool      { return false }
func (UsageEvent) Conversational() bool { return false }

func (u *UsageEvent) UnmarshalJSON(data []byte) error {
	type alias UsageEvent
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*u = UsageEvent(a)
	return nil
}

func init() {
	RegisterEvent(ModelMessage{})
	RegisterEvent(ModelMessageChunk{})
	RegisterEvent(ModelReasoning{})
	RegisterEvent(ModelResponse{})
	RegisterEvent(UsageEvent{})
}
