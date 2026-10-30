package ollama

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/internal/uuid"
	"github.com/luckystrker/ago/tools"
)

// convertMessages maps the system prompt plus the event history to /api/chat
// messages (Python parity: ollama/mappers.py convert_messages). Events outside
// the conversational transcript (usage events, streaming chunks, human
// traffic) are skipped.
func convertMessages(prompt []string, evs []events.Event) ([]map[string]any, error) {
	messages := make([]map[string]any, 0, len(prompt)+len(evs))
	// One system message per prompt fragment (Python parity).
	for _, p := range prompt {
		messages = append(messages, map[string]any{"role": "system", "content": p})
	}

	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.ModelRequest:
			msgs, err := userTurn(e.Parts)
			if err != nil {
				return nil, err
			}
			messages = append(messages, msgs...)

		case events.DrainedModelRequest:
			// Same shape as ModelRequest (Python: the subclass matches).
			msgs, err := userTurn(e.Parts)
			if err != nil {
				return nil, err
			}
			messages = append(messages, msgs...)

		case events.CompactionSummary:
			// A user turn keeps the summary visible and gives a valid opening turn.
			messages = append(messages, map[string]any{
				"role":    "user",
				"content": "[Summary of earlier conversation]\n" + e.Content,
			})

		case events.ModelResponse:
			msg := map[string]any{"role": "assistant", "content": e.Content()}
			if len(e.ToolCalls.Calls) > 0 {
				calls := make([]map[string]any, 0, len(e.ToolCalls.Calls))
				for _, call := range e.ToolCalls.Calls {
					args, err := callArgumentsObject(call.Arguments)
					if err != nil {
						return nil, err
					}
					calls = append(calls, map[string]any{
						"function": map[string]any{"name": call.Name, "arguments": args},
					})
				}
				msg["tool_calls"] = calls
			}
			messages = append(messages, msg)

		case events.ToolResultsEvent:
			for _, r := range e.Results {
				var texts []string
				for _, part := range r.Result.Parts {
					switch p := events.AsValue(part).(type) {
					case events.TextInput:
						texts = append(texts, p.Content)
					case events.DataInput:
						b, err := json.Marshal(p.Data)
						if err != nil {
							return nil, fmt.Errorf("ollama: encoding DataInput tool result: %w", err)
						}
						texts = append(texts, string(b))
					default:
						return nil, fmt.Errorf("%w: %T in an ollama tool result", config.ErrUnsupportedInput, part)
					}
				}
				// One tool message per part; an empty result still yields one
				// (empty) message so the call keeps its result (Python parity:
				// `for text in parts or [""]`).
				// tool_name supplements position: results match calls by order,
				// and parallel calls of one tool are told apart only by name.
				if len(texts) == 0 {
					texts = []string{""}
				}
				for _, text := range texts {
					msg := map[string]any{"role": "tool", "content": text}
					if r.Name != "" {
						msg["tool_name"] = r.Name
					}
					messages = append(messages, msg)
				}
			}
		}
	}
	return messages, nil
}

// userTurn maps one user request's parts to messages: one user message per
// text part (the API only accepts plain-string content), images attached to
// the last text message so they stay in the same turn (Python parity).
func userTurn(parts []events.Input) ([]map[string]any, error) {
	var texts, images []string
	for _, part := range parts {
		switch p := events.AsValue(part).(type) {
		case events.TextInput:
			texts = append(texts, p.Content)
		case events.DataInput:
			// Python encodes DataInput through the serializer as text; AGo uses
			// its JSON encoding (docs/specs/config-llm.md §6).
			b, err := json.Marshal(p.Data)
			if err != nil {
				return nil, fmt.Errorf("ollama: encoding DataInput: %w", err)
			}
			texts = append(texts, string(b))
		case events.BinaryInput:
			if p.Kind != events.BinaryImage {
				return nil, fmt.Errorf("%w: BinaryInput(%s) for the ollama chat API", config.ErrUnsupportedInput, p.Kind)
			}
			images = append(images, base64.StdEncoding.EncodeToString(p.Data))
		default:
			return nil, fmt.Errorf("%w: %T for the ollama chat API", config.ErrUnsupportedInput, part)
		}
	}
	msgs := make([]map[string]any, 0, len(texts)+1)
	for _, text := range texts {
		msgs = append(msgs, map[string]any{"role": "user", "content": text})
	}
	if len(images) > 0 {
		if len(texts) > 0 {
			msgs[len(msgs)-1]["images"] = images
		} else {
			msgs = append(msgs, map[string]any{"role": "user", "content": "", "images": images})
		}
	}
	return msgs, nil
}

// callArgumentsObject parses a ToolCallEvent's raw arguments JSON into the
// object the tool_calls wire shape expects (Python parity:
// json.loads(c.arguments) if c.arguments else {}).
func callArgumentsObject(arguments string) (map[string]any, error) {
	if strings.TrimSpace(arguments) == "" {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(arguments), &m); err != nil {
		return nil, fmt.Errorf("ollama: invalid tool call arguments %q: %w", arguments, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// --- tool schemas ---------------------------------------------------------------

// toolToAPI renders one tool schema as an /api/chat tool parameter. Function
// schemas become {"type":"function","function":{...}}; hosted tools fail fast
// (config.ErrUnsupportedTool) — Ollama speaks only function calling.
func toolToAPI(tool any) (map[string]any, error) {
	switch t := tool.(type) {
	case tools.Schema:
		return functionToolToAPI(t), nil
	case *tools.Schema:
		if t == nil {
			return nil, fmt.Errorf("%w: nil function schema", config.ErrUnsupportedTool)
		}
		return functionToolToAPI(*t), nil
	case tools.ToolSchema:
		return nil, fmt.Errorf("%w: hosted tool %q for the ollama chat API", config.ErrUnsupportedTool, t.SchemaType())
	default:
		return nil, fmt.Errorf("%w: unsupported schema type %T", config.ErrUnsupportedTool, tool)
	}
}

func functionToolToAPI(t tools.Schema) map[string]any {
	// The SDK requires type: object parameters; parameterless functions get an
	// empty properties map (Python parity: _ensure_object_schema).
	params := make(map[string]any, len(t.Parameters)+2)
	for k, v := range t.Parameters {
		params[k] = v
	}
	params["type"] = "object"
	if _, ok := params["properties"]; !ok {
		params["properties"] = map[string]any{}
	}
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"parameters":  params,
		},
	}
}

// --- response parsing -------------------------------------------------------------

type chatResponse struct {
	Model   string `json:"model"`
	Message struct {
		Content   string `json:"content"`
		Thinking  string `json:"thinking"`
		ToolCalls []struct {
			Function struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"message"`
	DoneReason      string `json:"done_reason"`
	PromptEvalCount *int64 `json:"prompt_eval_count"`
	EvalCount       *int64 `json:"eval_count"`
}

// normalizeUsage maps the counters onto events.Usage. Missing counts count as
// zero (Python parity: `response.prompt_eval_count or 0`), so the three fields
// are always reported.
func normalizeUsage(r *chatResponse) events.Usage {
	prompt := int64(0)
	if r.PromptEvalCount != nil {
		prompt = *r.PromptEvalCount
	}
	completion := int64(0)
	if r.EvalCount != nil {
		completion = *r.EvalCount
	}
	total := prompt + completion
	return events.Usage{
		PromptTokens:     &prompt,
		CompletionTokens: &completion,
		TotalTokens:      &total,
	}
}

// toolCallEvents folds the response's tool_calls into ToolCallEvents: the wire
// carries an arguments OBJECT, the event carries raw JSON text, and the id is
// synthesized (Ollama returns none, but tool results must reference one).
func toolCallEvents(calls []struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}) []events.ToolCallEvent {
	out := make([]events.ToolCallEvent, 0, len(calls))
	for _, tc := range calls {
		args := string(tc.Function.Arguments)
		if strings.TrimSpace(args) == "" || args == "null" {
			args = "{}"
		}
		out = append(out, events.ToolCallEvent{
			ID:        uuid.New(),
			Name:      tc.Function.Name,
			Arguments: args,
		})
	}
	return out
}

// sendEvent publishes a progress event on the conversation stream; a nil conv
// (headless call) drops it.
func sendEvent(conv *agocontext.Context, ev events.Event) error {
	if conv == nil {
		return nil
	}
	return conv.Send(ev)
}
