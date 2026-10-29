// OpenAI-compatible chat engine shared by every provider speaking the Chat
// Completions wire protocol (openai, zai, mistral, dashscope). It maps the
// event history to wire messages, posts JSON (streaming or not) through
// internal/httpx and normalizes the answer back into a ModelResponse.
//
// Mapping rules follow docs/specs/config-llm.md §6 and the Python reference
// ag2/config/openai/mappers.py (convert_messages, tool_to_api,
// _ensure_object_schema, _strictify_schema, normalize_usage).
package config

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	agocontext "ago/context"
	"ago/events"
	"ago/internal/httpx"
	"ago/response"
	"ago/tools"
)

// CompatParams is the provider-facing configuration of one OpenAI-compatible
// chat client. Provider config structs translate their fields into it.
type CompatParams struct {
	APIKey  string
	BaseURL string
	// Path is the URL path under BaseURL ("/chat/completions" by default).
	Path  string
	Model string
	// Provider stamps ModelResponse.Provider for usage accounting.
	// AGO-DEVIATION: added beyond the wave-1 brief field list — the engine
	// must stamp the tracing field and has no other way to know the vendor.
	Provider Provider

	ExtraHeaders map[string]string

	Temperature, TopP *float64
	MaxTokens         *int64
	Stop              []string
	ParallelToolCalls *bool

	Streaming  bool
	MaxRetries int
	Timeout    time.Duration

	// ResponseFormat sends an explicit response_format (provider config);
	// a per-call ResponseSchema in CallOptions takes precedence over it.
	ResponseFormat map[string]any
	// Extra merges into the request body last (Python parity: extra_body).
	Extra map[string]any
}

// RunChatCompat performs one chat-completions call. messages is the full event
// history; conv carries the system prompt and receives streaming progress
// (a nil conv means a headless call — no progress events are sent). The returned
// ModelResponse is the durable answer; errors wrap the package sentinels.
func RunChatCompat(ctx context.Context, p CompatParams, messages []events.Event, conv *agocontext.Context, opts CallOptions) (*events.ModelResponse, error) {
	if p.Model == "" {
		return nil, fmt.Errorf("%w: model", ErrNotProvided)
	}
	if p.APIKey == "" {
		return nil, fmt.Errorf("%w: api key", ErrNotProvided)
	}
	if p.BaseURL == "" {
		return nil, fmt.Errorf("%w: base url", ErrNotProvided)
	}

	var prompt []string
	if conv != nil { // a nil conv is a headless call
		prompt = conv.PromptLines()
	}
	if opts.ResponseSchema != nil {
		if sp := opts.ResponseSchema.SystemPrompt(); sp != "" {
			prompt = append(prompt, sp)
		}
	}
	wireMessages, err := BuildMessages(prompt, messages)
	if err != nil {
		return nil, err
	}

	wireTools := make([]map[string]any, 0, len(opts.Tools))
	for _, t := range opts.Tools {
		wt, err := ToolToAPI(t)
		if err != nil {
			return nil, err
		}
		wireTools = append(wireTools, wt)
	}

	body := map[string]any{
		"model":    p.Model,
		"messages": wireMessages,
	}
	if p.Temperature != nil {
		body["temperature"] = *p.Temperature
	}
	if p.TopP != nil {
		body["top_p"] = *p.TopP
	}
	if p.MaxTokens != nil {
		body["max_tokens"] = *p.MaxTokens
	}
	if len(p.Stop) > 0 {
		body["stop"] = p.Stop
	}
	if p.ParallelToolCalls != nil {
		body["parallel_tool_calls"] = *p.ParallelToolCalls
	}
	if rf := responseFormat(p, opts.ResponseSchema); rf != nil {
		body["response_format"] = rf
	}
	if len(wireTools) > 0 {
		body["tools"] = wireTools
	}
	if p.Streaming {
		body["stream"] = true
		// Python parity (config.py): usage arrives only with include_usage.
		if _, ok := p.Extra["stream_options"]; !ok {
			body["stream_options"] = map[string]any{"include_usage": true}
		}
	}
	for k, v := range p.Extra {
		body[k] = v
	}

	headers := map[string]string{"Authorization": "Bearer " + p.APIKey}
	for k, v := range p.ExtraHeaders {
		headers[k] = v
	}

	client := &httpx.Client{MaxRetries: p.MaxRetries, Timeout: p.Timeout}
	url := joinURL(p.BaseURL, p.Path)

	if p.Streaming {
		return runChatStream(ctx, client, url, headers, body, conv, p.provider())
	}
	return runChatOnce(ctx, client, url, headers, body, conv, p.provider())
}

func (p CompatParams) provider() Provider {
	if p.Provider == "" {
		return ProviderOpenAI // the engine is the OpenAI dialect
	}
	return p.Provider
}

func joinURL(base, path string) string {
	base = strings.TrimSuffix(base, "/")
	if path == "" {
		path = "/chat/completions"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

// responseFormat picks the response_format: an explicit provider config wins,
// otherwise the per-call schema becomes a strict json_schema block (Python
// parity: response_proto_to_schema). A schema without a JSON schema (e.g. a
// PromptedSchema) drives output through its system prompt instead.
func responseFormat(p CompatParams, schema response.Proto) map[string]any {
	if p.ResponseFormat != nil {
		return p.ResponseFormat
	}
	if schema == nil {
		return nil
	}
	js := schema.JSONSchema()
	if len(js) == 0 {
		return nil
	}
	inner := map[string]any{
		"name":   schema.Name(),
		"schema": strictifySchema(js),
		"strict": true,
	}
	if d := schema.Description(); d != "" {
		inner["description"] = d
	}
	return map[string]any{"type": "json_schema", "json_schema": inner}
}

// --- wire message building ---------------------------------------------------

// BuildMessages converts the system prompt plus the event history into
// Chat-Completions wire messages: system first, then user turns (ModelRequest),
// assistant turns (ModelResponse, with replayed tool calls), tool answers
// (ToolResultsEvent) and compaction summaries. Everything else (usage events,
// streaming chunks, standalone tool-call events, human traffic) is skipped,
// Python parity.
func BuildMessages(prompt []string, evs []events.Event) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(evs)+1)
	out = append(out, map[string]any{
		"role":    "system",
		"content": strings.Join(prompt, "\n"),
	})
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.ModelResponse:
			out = append(out, assistantMessage(e))
		case events.ModelRequest:
			msg, err := userMessage(e.Parts)
			if err != nil {
				return nil, err
			}
			out = append(out, msg)
		case events.DrainedModelRequest:
			msg, err := userMessage(e.Parts)
			if err != nil {
				return nil, err
			}
			out = append(out, msg)
		case events.ToolResultsEvent:
			for _, r := range e.Results {
				text, err := resultText(r.Result.Parts)
				if err != nil {
					return nil, err
				}
				out = append(out, map[string]any{
					"role":         "tool",
					"tool_call_id": r.ParentID,
					"content":      text,
				})
			}
		case events.CompactionSummary:
			// A user turn keeps the summary visible and gives a valid opening turn.
			out = append(out, map[string]any{
				"role":    "user",
				"content": "[Summary of earlier conversation]\n" + e.Content,
			})
		}
	}
	return out, nil
}

// assistantMessage replays a model turn: its text, then the function calls it
// made. AGO-DEVIATION: the calls replay with the raw arguments JSON
// (ToolCallEvent.ToAPI) where Python re-serializes the parsed arguments dict —
// the raw form preserves the model's exact JSON.
func assistantMessage(r events.ModelResponse) map[string]any {
	m := map[string]any{"role": "assistant"}
	if content := r.Content(); content != "" {
		m["content"] = content
	}
	if len(r.ToolCalls.Calls) > 0 {
		calls := make([]map[string]any, 0, len(r.ToolCalls.Calls))
		for _, c := range r.ToolCalls.Calls {
			calls = append(calls, c.ToAPI())
		}
		m["tool_calls"] = calls
	}
	return m
}

// userMessage maps one ModelRequest's parts to a user turn. A single text part
// collapses to a plain string (Python parity); otherwise the parts travel as an
// array.
func userMessage(parts []events.Input) (map[string]any, error) {
	wire := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		w, err := inputPart(part)
		if err != nil {
			return nil, err
		}
		wire = append(wire, w)
	}
	var content any
	switch {
	case len(wire) == 0:
		content = ""
	case len(wire) == 1 && wire[0]["type"] == "text":
		content = wire[0]["text"]
	default:
		content = wire
	}
	return map[string]any{"role": "user", "content": content}, nil
}

// inputPart maps one input part to one wire content part. The Chat-Completions
// dialect of this engine carries text and images; every other part fails fast
// with ErrUnsupportedInput (never silently dropped).
func inputPart(part events.Input) (map[string]any, error) {
	switch p := events.AsValue(part).(type) {
	case events.TextInput:
		return map[string]any{"type": "text", "text": p.Content}, nil
	case events.DataInput:
		// AGO-DEVIATION: the brief lists only text/binary/url parts; DataInput
		// maps to its JSON encoding as text (spec §6 / Python serializer parity).
		b, err := json.Marshal(p.Data)
		if err != nil {
			return nil, fmt.Errorf("config: encoding DataInput: %w", err)
		}
		return map[string]any{"type": "text", "text": string(b)}, nil
	case events.BinaryInput:
		if p.Kind != events.BinaryImage {
			return nil, fmt.Errorf("%w: BinaryInput(%s) for the chat-completions API", ErrUnsupportedInput, p.Kind)
		}
		return map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": dataURL(p.MediaType, p.Data)},
		}, nil
	case events.UrlInput:
		if p.Kind != events.BinaryImage {
			return nil, fmt.Errorf("%w: UrlInput(%s) for the chat-completions API", ErrUnsupportedInput, p.Kind)
		}
		return map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": p.URL},
		}, nil
	case events.FileIdInput:
		// AGO-DEVIATION: Python's OpenAI client accepts file references; the
		// shared engine has no Files API, so they fail fast per the brief.
		return nil, fmt.Errorf("%w: FileIdInput for the chat-completions API", ErrUnsupportedInput)
	default:
		return nil, fmt.Errorf("%w: %T for the chat-completions API", ErrUnsupportedInput, part)
	}
}

func dataURL(mediaType string, data []byte) string {
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// resultText renders a tool result's parts as the tool message content:
// TextInput verbatim, DataInput as JSON, concatenated with newlines.
// AGO-DEVIATION: Python sends an array of text parts when a result has several;
// the shared engine always sends one string (per the porting brief).
func resultText(parts []events.Input) (string, error) {
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		switch p := events.AsValue(part).(type) {
		case events.TextInput:
			texts = append(texts, p.Content)
		case events.DataInput:
			b, err := json.Marshal(p.Data)
			if err != nil {
				return "", fmt.Errorf("config: encoding DataInput tool result: %w", err)
			}
			texts = append(texts, string(b))
		default:
			return "", fmt.Errorf("%w: %T in a tool result", ErrUnsupportedInput, part)
		}
	}
	return strings.Join(texts, "\n"), nil
}

// --- tool schemas -------------------------------------------------------------

// ToolToAPI renders one tool schema as a Chat-Completions tool parameter.
// Function schemas become {"type":"function","function":{...}}; hosted tools
// and anything else fail fast with ErrUnsupportedTool.
func ToolToAPI(tool any) (map[string]any, error) {
	switch t := tool.(type) {
	case tools.Schema:
		return functionToolToAPI(t)
	case *tools.Schema:
		if t == nil {
			return nil, fmt.Errorf("%w: nil function schema", ErrUnsupportedTool)
		}
		return functionToolToAPI(*t)
	case tools.ToolSchema:
		return nil, fmt.Errorf("%w: hosted tool %q for the chat-completions API", ErrUnsupportedTool, t.SchemaType())
	default:
		return nil, fmt.Errorf("%w: unsupported schema type %T", ErrUnsupportedTool, tool)
	}
}

func functionToolToAPI(t tools.Schema) (map[string]any, error) {
	if t.DeferLoading {
		// Deferred loading is a Responses-API feature; sending the tool eagerly
		// would defeat it, so fail fast instead (Python parity).
		return nil, fmt.Errorf("%w: function %q with defer_loading (use the Responses API)", ErrUnsupportedTool, t.Name)
	}
	fn := map[string]any{
		"name":       t.Name,
		"parameters": EnsureObjectSchema(t.Parameters),
	}
	if t.Description != "" {
		fn["description"] = t.Description
	}
	return map[string]any{"type": "function", "function": fn}, nil
}

// EnsureObjectSchema copies params into the object form providers require:
// type "object", a properties map, additionalProperties false unless set.
// A nil params yields the empty object schema.
func EnsureObjectSchema(params map[string]any) map[string]any {
	out := make(map[string]any, len(params)+3)
	for k, v := range params {
		out[k] = v
	}
	out["type"] = "object"
	if _, ok := out["properties"]; !ok {
		out["properties"] = map[string]any{}
	}
	if _, ok := out["additionalProperties"]; !ok {
		out["additionalProperties"] = false
	}
	return out
}

// strictifySchema recursively coerces a JSON schema into the strict subset
// OpenAI requires (Python parity: _strictify_schema): every object gains
// additionalProperties=false and required listing all its properties.
func strictifySchema(schema map[string]any) map[string]any {
	out := make(map[string]any, len(schema))
	for k, v := range schema {
		out[k] = v
	}
	if t, ok := out["type"].(string); ok && t == "object" {
		if _, ok := out["additionalProperties"]; !ok {
			out["additionalProperties"] = false
		}
	}
	if props, ok := out["properties"].(map[string]any); ok {
		strictProps := make(map[string]any, len(props))
		keys := make([]string, 0, len(props))
		for k, v := range props {
			keys = append(keys, k)
			if sv, ok := v.(map[string]any); ok {
				strictProps[k] = strictifySchema(sv)
			} else {
				strictProps[k] = v
			}
		}
		out["properties"] = strictProps
		out["required"] = keys
	}
	if defs, ok := out["$defs"].(map[string]any); ok {
		strictDefs := make(map[string]any, len(defs))
		for k, v := range defs {
			if sv, ok := v.(map[string]any); ok {
				strictDefs[k] = strictifySchema(sv)
			} else {
				strictDefs[k] = v
			}
		}
		out["$defs"] = strictDefs
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		items, ok := out[key].([]any)
		if !ok {
			continue
		}
		strictItems := make([]any, len(items))
		for i, item := range items {
			if sv, ok := item.(map[string]any); ok {
				strictItems[i] = strictifySchema(sv)
			} else {
				strictItems[i] = item
			}
		}
		out[key] = strictItems
	}
	if items, ok := out["items"].(map[string]any); ok {
		out["items"] = strictifySchema(items)
	}
	return out
}

// --- usage ---------------------------------------------------------------------

// NormalizeUsage maps the Chat-Completions usage object onto events.Usage:
// prompt/completion/total tokens, prompt_tokens_details.cached_tokens →
// CacheReadInputTokens, cache_write_tokens → CacheCreationInputTokens,
// completion_tokens_details.reasoning_tokens → ThinkingTokens. Fractional
// values are rounded (D-006: token counts are integers).
func NormalizeUsage(usage map[string]any) events.Usage {
	var u events.Usage
	u.PromptTokens = tokenField(usage, "prompt_tokens")
	u.CompletionTokens = tokenField(usage, "completion_tokens")
	u.TotalTokens = tokenField(usage, "total_tokens")
	if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		u.CacheReadInputTokens = tokenField(details, "cached_tokens")
		u.CacheCreationInputTokens = tokenField(details, "cache_write_tokens")
	}
	if details, ok := usage["completion_tokens_details"].(map[string]any); ok {
		u.ThinkingTokens = tokenField(details, "reasoning_tokens")
	}
	return u
}

// tokenField reads one numeric field tolerating the JSON decoder's float64,
// whole-number Go ints and json.Number (decoders using UseNumber).
func tokenField(m map[string]any, key string) *int64 {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	switch n := v.(type) {
	case float64:
		i := int64(math.Round(n))
		return &i
	case int64:
		return &n
	case int:
		i := int64(n)
		return &i
	case json.Number:
		if i, err := strconv.ParseInt(string(n), 10, 64); err == nil {
			return &i
		}
		if f, err := n.Float64(); err == nil {
			i := int64(math.Round(f))
			return &i
		}
	}
	return nil
}

// --- non-streaming call ---------------------------------------------------------

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireChoice struct {
	FinishReason string `json:"finish_reason"`
	Message      struct {
		Content   string         `json:"content"`
		Reasoning string         `json:"reasoning"`
		ToolCalls []wireToolCall `json:"tool_calls"`
	} `json:"message"`
}

func runChatOnce(ctx context.Context, client *httpx.Client, url string, headers map[string]string, body map[string]any, conv *agocontext.Context, provider Provider) (*events.ModelResponse, error) {
	status, data, err := client.PostJSON(ctx, url, headers, body)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("config: chat completions returned %d: %s", status, bodySnippet(data))
	}
	var raw struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Choices []wireChoice    `json:"choices"`
		Usage   *map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("config: decoding chat completion: %w", err)
	}

	resp := &events.ModelResponse{
		Model:      raw.Model,
		Provider:   string(provider),
		ResponseID: raw.ID,
	}
	if len(raw.Choices) > 0 {
		// A completion can arrive with no choices — a content filter answers
		// that way. The turn still spent tokens and still returns a response.
		choice := raw.Choices[0]
		resp.FinishReason = choice.FinishReason
		if r := choice.Message.Reasoning; r != "" {
			if err := sendEvent(conv, &events.ModelReasoning{Content: r}); err != nil {
				return nil, err
			}
		}
		if c := choice.Message.Content; c != "" {
			msg := &events.ModelMessage{Content: c}
			if err := sendEvent(conv, msg); err != nil {
				return nil, err
			}
			resp.Message = msg
		}
		calls := make([]events.ToolCallEvent, 0, len(choice.Message.ToolCalls))
		for _, tc := range choice.Message.ToolCalls {
			if tc.Type != "" && tc.Type != "function" {
				return nil, fmt.Errorf("%w: tool call of type %q for the chat-completions API", ErrUnsupportedTool, tc.Type)
			}
			calls = append(calls, events.ToolCallEvent{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
		resp.ToolCalls = events.ToolCallsEvent{Calls: calls}
	}
	if raw.Usage != nil && *raw.Usage != nil {
		resp.Usage = NormalizeUsage(*raw.Usage)
	}
	return resp, nil
}

// --- streaming call ---------------------------------------------------------------

type wireToolDelta struct {
	Index    *int    `json:"index"`
	ID       *string `json:"id"`
	Function *struct {
		Name      string  `json:"name"`
		Arguments *string `json:"arguments"`
	} `json:"function"`
}

type wireChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		FinishReason *string `json:"finish_reason"`
		Delta        struct {
			Content          string          `json:"content"`
			ReasoningContent string          `json:"reasoning_content"`
			ToolCalls        []wireToolDelta `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *map[string]any `json:"usage"`
	// Error is the in-band error object some dialects deliver as a chunk of a
	// 200 stream instead of a failed response; a non-nil one fails the call.
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// toolAcc accumulates one streamed tool call; deltas merge by index.
type toolAcc struct {
	id, name, arguments string
}

func runChatStream(ctx context.Context, client *httpx.Client, url string, headers map[string]string, body map[string]any, conv *agocontext.Context, provider Provider) (*events.ModelResponse, error) {
	var (
		content       strings.Builder
		usage         events.Usage
		finishReason  string
		resolvedModel string
		responseID    string
		calls         []toolAcc
	)
	err := client.PostJSONStream(ctx, url, headers, body, func(data []byte) error {
		if string(data) == "[DONE]" {
			return nil
		}
		var chunk wireChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			return fmt.Errorf("config: decoding stream chunk: %w", err)
		}
		if chunk.Error != nil {
			// In-band error chunk: the call fails — Python parity (the SDK
			// iterator raises instead of ending the stream as a success).
			if chunk.Error.Type != "" {
				return fmt.Errorf("%w: %s: %s", ErrStreamError, chunk.Error.Type, chunk.Error.Message)
			}
			return fmt.Errorf("%w: %s", ErrStreamError, chunk.Error.Message)
		}
		if chunk.ID != "" {
			responseID = chunk.ID
		}
		if chunk.Model != "" {
			resolvedModel = chunk.Model
		}
		if chunk.Usage != nil {
			// Usage is available only in the final chunk.
			usage = NormalizeUsage(*chunk.Usage)
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finishReason = *choice.FinishReason
			}
			if r := choice.Delta.ReasoningContent; r != "" {
				if err := sendEvent(conv, &events.ModelReasoning{Content: r}); err != nil {
					return err
				}
			}
			if c := choice.Delta.Content; c != "" {
				content.WriteString(c)
				if err := sendEvent(conv, &events.ModelMessageChunk{Content: c}); err != nil {
					return err
				}
			}
			for _, tc := range choice.Delta.ToolCalls {
				ix := 0
				if tc.Index != nil {
					ix = *tc.Index
				}
				for ix >= len(calls) {
					calls = append(calls, toolAcc{})
				}
				acc := &calls[ix]
				if tc.ID != nil {
					acc.id = *tc.ID
				}
				if tc.Function != nil {
					if tc.Function.Name != "" {
						acc.name = tc.Function.Name
					}
					if tc.Function.Arguments != nil {
						acc.arguments += *tc.Function.Arguments
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	resp := &events.ModelResponse{
		Usage:        usage,
		Model:        resolvedModel,
		Provider:     string(provider),
		FinishReason: finishReason,
		ResponseID:   responseID,
	}
	if content.Len() > 0 {
		msg := &events.ModelMessage{Content: content.String()}
		if err := sendEvent(conv, msg); err != nil {
			return nil, err
		}
		resp.Message = msg
	}
	resp.ToolCalls = events.ToolCallsEvent{Calls: make([]events.ToolCallEvent, 0, len(calls))}
	for _, acc := range calls {
		resp.ToolCalls.Calls = append(resp.ToolCalls.Calls, events.ToolCallEvent{
			ID:        acc.id,
			Name:      acc.name,
			Arguments: acc.arguments,
		})
	}
	return resp, nil
}

// sendEvent publishes a progress event on the conversation stream; a nil conv
// (headless call) drops it.
func sendEvent(conv *agocontext.Context, ev events.Event) error {
	if conv == nil {
		return nil
	}
	return conv.Send(ev)
}

// bodySnippet renders the first bytes of an error response body.
func bodySnippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	const max = 512
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
