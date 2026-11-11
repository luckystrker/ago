package eval

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/usage"
)

// LLMCall is one model call inside a trace (Python parity: the span→ModelResponse
// projection in eval/sources/_spans.py). Usage mirrors what the run's accounting
// recorded for that call; InputText is the user prompt that kicked the round (tool
// rounds are triggered by results, so their input is the preceding ToolCalls).
type LLMCall struct {
	InputText  string       `json:"input_text,omitempty"`
	OutputText string       `json:"output_text,omitempty"`
	Usage      events.Usage `json:"usage"`
	Model      string       `json:"model,omitempty"`
	Provider   string       `json:"provider,omitempty"`
}

// ToolCall is one tool invocation and its outcome. Err is set (and Result empty)
// when the tool errored or was not found.
type ToolCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
	Result    string `json:"result,omitempty"`
	Err       string `json:"err,omitempty"`
}

// Trace is the read-only record of one graded unit of work, reconstructed from the
// stream's event history — every scorer sees exactly this (Python parity: eval.Trace;
// AGO-DEVIATION: built from events, not OTEL spans — D-051). StartedAt/FinishedAt
// bound the first and last event timestamps.
type Trace struct {
	AgentName     string         `json:"agent_name,omitempty"`
	StartedAt     time.Time      `json:"started_at,omitempty"`
	FinishedAt    time.Time      `json:"finished_at,omitempty"`
	LLMCalls      []LLMCall      `json:"llm_calls"`
	ToolCalls     []ToolCall     `json:"tool_calls"`
	FinalResponse string         `json:"final_response,omitempty"`
	Err           string         `json:"err,omitempty"` // the ask failure, if any (Python parity: Trace.exception)
	Events        []events.Event `json:"events,omitempty"`
}

// BuildTrace reconstructs a Trace from a stream history (AgentReply.History).
// ModelResponse events become LLMCalls (paired with the model_call UsageEvent
// emitted just before each), ToolCallEvent-family events become ToolCalls with
// their results/errors matched by call id, and the last ModelResponse's content
// is the FinalResponse. Token totals come from UsageEvents only (see Tokens).
func BuildTrace(evs []events.Event) *Trace {
	t := &Trace{LLMCalls: []LLMCall{}, ToolCalls: []ToolCall{}, Events: evs}
	pendingInput := ""
	// lastUsage holds the model_call UsageEvent issued immediately before the
	// next ModelResponse (the turn loop emits that order, agent/turn.go:172).
	lastUsage := events.Usage{}
	indexByCallID := map[string]int{}
	final := ""

	for _, ev := range evs {
		if at := ev.At(); !at.IsZero() {
			if t.StartedAt.IsZero() {
				t.StartedAt = at
			}
			t.FinishedAt = at
		}
		observed := isChildObservation(ev)
		source := events.SourceEvent(ev)
		switch e := events.AsValue(source).(type) {
		case events.ModelRequest:
			pendingInput = renderParts(e.Parts)
		case events.DrainedModelRequest:
			pendingInput = renderParts(e.Parts)
		case events.UsageEvent:
			// Only per-call accounting pairs with a response; subtask/compaction
			// rollups stay in Events and count through Tokens.
			if e.Kind == events.UsageKindModelCall {
				lastUsage = e.Usage
			}
		case events.ModelResponse:
			u := lastUsage
			if !usage.Reported(u) {
				u = e.Usage // histories without accounting events fall back to the response itself
			}
			t.LLMCalls = append(t.LLMCalls, LLMCall{
				InputText:  pendingInput,
				OutputText: e.Content(),
				Usage:      u,
				Model:      e.Model,
				Provider:   e.Provider,
			})
			pendingInput, lastUsage = "", events.Usage{}
			if !observed {
				// The graded answer is the root agent's; a descendant's response
				// is an LLMCall in the trajectory, never the final answer.
				final = e.Content()
			}
		case events.ToolCallEvent:
			addToolCall(t, indexByCallID, e.ID, e.Name, e.Arguments)
		case events.BuiltinToolCallEvent:
			addToolCall(t, indexByCallID, e.ID, e.Name, e.Arguments)
		case events.ClientToolCallEvent:
			addToolCall(t, indexByCallID, e.ID, e.Name, e.Arguments)
		case events.ToolResultEvent:
			if i, ok := indexByCallID[e.ParentID]; ok {
				t.ToolCalls[i].Result = renderParts(e.Result.Parts)
			}
		case events.BuiltinToolResultEvent:
			if i, ok := indexByCallID[e.ParentID]; ok {
				t.ToolCalls[i].Result = renderParts(e.Result.Parts)
			}
		case events.ToolErrorEvent:
			if i, ok := indexByCallID[e.ParentID]; ok {
				t.ToolCalls[i].Err = e.Error
			}
		case events.ToolNotFoundEvent:
			if i, ok := indexByCallID[e.ParentID]; ok {
				t.ToolCalls[i].Err = e.Error
			}
		}
	}
	t.FinalResponse = final
	return t
}

// isChildObservation reports a forwarded descendant event. Its source event
// contributes calls/tools to the trace; only local events may set FinalResponse.
func isChildObservation(ev events.Event) bool {
	switch events.AsValue(ev).(type) {
	case events.ChildEvent, events.TransientChildEvent:
		return true
	}
	return false
}

func addToolCall(t *Trace, indexByCallID map[string]int, id, name, arguments string) {
	indexByCallID[id] = len(t.ToolCalls)
	t.ToolCalls = append(t.ToolCalls, ToolCall{Name: name, Arguments: arguments})
}

// Tokens sums the run's UsageEvents (ago/usage). Python parity: UsageEvent is the
// framework's accounting record and ModelResponse.Usage is excluded there too —
// a model call emits both, and counting both would double every main-loop call
// (ag2/eval/trace.py:97). Cache tokens are reported separately by the Usage
// fields and are not part of the input+output total.
func (t *Trace) Tokens() events.Usage {
	return usage.FromEvents(t.Events).Total
}

func renderParts(parts []events.Input) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, events.RenderForPrompt(p))
	}
	return strings.Join(out, "\n")
}

// MarshalJSON serializes events through events.MarshalEvent so the "@type"
// discriminator survives (plain json.Marshal would drop the concrete type).
func (t Trace) MarshalJSON() ([]byte, error) {
	evs := make([]json.RawMessage, len(t.Events))
	for i, ev := range t.Events {
		b, err := events.MarshalEvent(ev)
		if err != nil {
			return nil, fmt.Errorf("eval: marshal trace event %d: %w", i, err)
		}
		evs[i] = b
	}
	type alias struct {
		AgentName     string            `json:"agent_name,omitempty"`
		StartedAt     time.Time         `json:"started_at,omitempty"`
		FinishedAt    time.Time         `json:"finished_at,omitempty"`
		LLMCalls      []LLMCall         `json:"llm_calls"`
		ToolCalls     []ToolCall        `json:"tool_calls"`
		FinalResponse string            `json:"final_response,omitempty"`
		Err           string            `json:"err,omitempty"`
		Events        []json.RawMessage `json:"events,omitempty"`
	}
	return json.Marshal(alias{
		AgentName:     t.AgentName,
		StartedAt:     t.StartedAt,
		FinishedAt:    t.FinishedAt,
		LLMCalls:      t.LLMCalls,
		ToolCalls:     t.ToolCalls,
		FinalResponse: t.FinalResponse,
		Err:           t.Err,
		Events:        evs,
	})
}

// UnmarshalJSON is the reverse of MarshalJSON. AGO-DEVIATION: Python's store
// drops events on load ("a record for cross-time comparison, not a full trace
// round-trip", results/store.py:159); AGo round-trips them, so Tokens and the
// event-based scorers keep working on a loaded run.
func (t *Trace) UnmarshalJSON(data []byte) error {
	var w struct {
		AgentName     string            `json:"agent_name,omitempty"`
		StartedAt     time.Time         `json:"started_at,omitempty"`
		FinishedAt    time.Time         `json:"finished_at,omitempty"`
		LLMCalls      []LLMCall         `json:"llm_calls"`
		ToolCalls     []ToolCall        `json:"tool_calls"`
		FinalResponse string            `json:"final_response,omitempty"`
		Err           string            `json:"err,omitempty"`
		Events        []json.RawMessage `json:"events,omitempty"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	t.AgentName = w.AgentName
	t.StartedAt = w.StartedAt
	t.FinishedAt = w.FinishedAt
	t.LLMCalls = w.LLMCalls
	t.ToolCalls = w.ToolCalls
	t.FinalResponse = w.FinalResponse
	t.Err = w.Err
	t.Events = make([]events.Event, 0, len(w.Events))
	for i, raw := range w.Events {
		ev, err := events.UnmarshalEvent(raw)
		if err != nil {
			return fmt.Errorf("eval: decode trace event %d: %w", i, err)
		}
		t.Events = append(t.Events, ev)
	}
	return nil
}

// Duration is the wall-clock span of the trace — first event to last (Python
// parity: Trace.duration_ms). A trace with no events measures zero.
func (t *Trace) Duration() time.Duration {
	if t == nil || t.StartedAt.IsZero() || t.FinishedAt.Before(t.StartedAt) {
		return 0
	}
	return t.FinishedAt.Sub(t.StartedAt)
}

// TotalTokens is the input-plus-output token count of every model call in the
// trace — what a per-task token budget measures (Python parity:
// trace.tokens.total; cache and thinking tokens are reported separately and
// are not part of it).
func (t *Trace) TotalTokens() int {
	if t == nil {
		return 0
	}
	u := t.Tokens()
	var n int64
	if u.PromptTokens != nil {
		n += *u.PromptTokens
	}
	if u.CompletionTokens != nil {
		n += *u.CompletionTokens
	}
	return int(n)
}
