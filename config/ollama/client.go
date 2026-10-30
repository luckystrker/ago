package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/internal/httpx"
)

// client performs /api/chat calls for one resolved OllamaConfig. Safe for
// concurrent use (all state is read-only after Create).
type client struct {
	cfg    OllamaConfig
	host   string
	apiKey string
	http   *httpx.Client
}

// Call performs one model call over the full event history (config.LLMClient).
// A nil conv is a headless call: no system prompt, no progress events.
func (c *client) Call(ctx context.Context, messages []events.Event, conv *agocontext.Context, opts config.CallOptions) (*events.ModelResponse, error) {
	var prompt []string
	if conv != nil {
		prompt = conv.PromptLines()
	}
	if opts.ResponseSchema != nil {
		if sp := opts.ResponseSchema.SystemPrompt(); sp != "" {
			prompt = append(prompt, sp)
		}
	}

	wireMessages, err := convertMessages(prompt, messages)
	if err != nil {
		return nil, err
	}

	body := map[string]any{
		"model":    c.cfg.model,
		"messages": wireMessages,
		"stream":   false,
	}
	if o := c.options(); len(o) > 0 {
		body["options"] = o
	}
	if len(opts.Tools) > 0 {
		wireTools := make([]map[string]any, 0, len(opts.Tools))
		for _, t := range opts.Tools {
			wt, err := toolToAPI(t)
			if err != nil {
				return nil, err
			}
			wireTools = append(wireTools, wt)
		}
		body["tools"] = wireTools
	}
	if opts.ResponseSchema != nil {
		// Structured output rides on the format parameter (Python parity:
		// response_proto_to_format — the raw JSON schema, not an envelope).
		if js := opts.ResponseSchema.JSONSchema(); len(js) > 0 {
			body["format"] = js
		}
	}

	var headers map[string]string
	if c.apiKey != "" {
		headers = map[string]string{"Authorization": "Bearer " + c.apiKey}
	}

	url := strings.TrimSuffix(c.host, "/") + "/api/chat"
	status, data, err := c.http.PostJSON(ctx, url, headers, body)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("ollama: chat returned %d: %s", status, snippet(data))
	}
	var raw chatResponse
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("ollama: decoding chat response: %w", err)
	}

	if thinking := raw.Message.Thinking; thinking != "" {
		if err := sendEvent(conv, &events.ModelReasoning{Content: thinking}); err != nil {
			return nil, err
		}
	}

	resp := &events.ModelResponse{
		ToolCalls:    events.ToolCallsEvent{Calls: toolCallEvents(raw.Message.ToolCalls)},
		Usage:        normalizeUsage(&raw),
		Model:        raw.Model,
		Provider:     string(config.ProviderOllama),
		FinishReason: raw.DoneReason,
	}
	if content := raw.Message.Content; content != "" {
		msg := &events.ModelMessage{Content: content}
		if err := sendEvent(conv, msg); err != nil {
			return nil, err
		}
		resp.Message = msg
	}
	return resp, nil
}

// options assembles the sampling block; unset fields are omitted (Python
// parity: CreateOptions filtered to non-None).
func (c *client) options() map[string]any {
	o := map[string]any{}
	if c.cfg.temperature != nil {
		o["temperature"] = *c.cfg.temperature
	}
	if c.cfg.topP != nil {
		o["top_p"] = *c.cfg.topP
	}
	if c.cfg.maxTokens != nil {
		o["num_predict"] = *c.cfg.maxTokens
	}
	if len(c.cfg.stop) > 0 {
		o["stop"] = c.cfg.stop
	}
	if c.cfg.seed != nil {
		o["seed"] = *c.cfg.seed
	}
	return o
}

// snippet renders the first bytes of an error response body.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	const max = 512
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
