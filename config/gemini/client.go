package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"ago/config"
	agocontext "ago/context"
	"ago/events"
	"ago/internal/httpx"
	"ago/response"
)

// client performs generateContent calls for one resolved GeminiConfig. Safe for
// concurrent use (all state is read-only after Create).
type client struct {
	cfg     GeminiConfig
	apiKey  string
	baseURL string
	http    *httpx.Client
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

	contents, err := buildContents(messages)
	if err != nil {
		return nil, err
	}
	wireTools, err := buildTools(opts.Tools)
	if err != nil {
		return nil, err
	}

	body := map[string]any{"contents": contents}
	if si := strings.Join(prompt, "\n"); si != "" {
		body["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": si}},
		}
	}
	if len(wireTools) > 0 {
		body["tools"] = wireTools
	}
	if gen := c.generationConfig(opts.ResponseSchema); len(gen) > 0 {
		body["generationConfig"] = gen
	}

	headers := map[string]string{"x-goog-api-key": c.apiKey}
	if c.cfg.streaming {
		return c.streamGenerate(ctx, body, headers, conv)
	}
	return c.generateContent(ctx, body, headers, conv)
}

// generationConfig assembles the sampling block: config fields plus the
// response schema's structured-output directives (Python parity:
// response_proto_to_config — responseMimeType application/json with the schema).
func (c *client) generationConfig(schema response.Proto) map[string]any {
	gen := map[string]any{}
	if c.cfg.temperature != nil {
		gen["temperature"] = *c.cfg.temperature
	}
	if c.cfg.topP != nil {
		gen["topP"] = *c.cfg.topP
	}
	if c.cfg.topK != nil {
		gen["topK"] = *c.cfg.topK
	}
	if c.cfg.maxOutputTokens != nil {
		gen["maxOutputTokens"] = *c.cfg.maxOutputTokens
	}
	if len(c.cfg.stopSequences) > 0 {
		gen["stopSequences"] = c.cfg.stopSequences
	}
	if schema != nil {
		if js := schema.JSONSchema(); len(js) > 0 {
			gen["responseMimeType"] = "application/json"
			gen["responseJsonSchema"] = js
		}
	}
	return gen
}

func (c *client) endpoint(action string) string {
	return strings.TrimSuffix(c.baseURL, "/") + "/models/" + c.cfg.model + ":" + action
}

// generateContent posts one non-streaming request and folds the response into
// a ModelResponse.
func (c *client) generateContent(ctx context.Context, body map[string]any, headers map[string]string, conv *agocontext.Context) (*events.ModelResponse, error) {
	status, data, err := c.http.PostJSON(ctx, c.endpoint("generateContent"), headers, body)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("gemini: generateContent returned %d: %s", status, snippet(data))
	}
	var raw genResponse
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("gemini: decoding generateContent response: %w", err)
	}

	var acc genAccumulator
	// Python walks every candidate (multi-candidate answers concatenate) but
	// reads the finish reason from the first one only.
	for _, candidate := range raw.Candidates {
		if candidate.Content == nil {
			continue
		}
		for _, part := range candidate.Content.Parts {
			if err := acc.addPart(part, conv, false); err != nil {
				return nil, err
			}
		}
	}

	resp := &events.ModelResponse{
		ToolCalls:    events.ToolCallsEvent{Calls: acc.calls},
		Usage:        normalizeUsage(raw.UsageMetadata),
		Model:        c.cfg.model,
		Provider:     string(config.ProviderGemini),
		FinishReason: finishReasonOf(&raw),
		Files:        acc.files,
		// AGO-DEVIATION: Python leaves response_id unset; the REST response
		// carries one and the event has the tracing field for it.
		ResponseID: raw.ResponseID,
	}
	if acc.content.Len() > 0 {
		msg := &events.ModelMessage{Content: acc.content.String()}
		if err := sendEvent(conv, msg); err != nil {
			return nil, err
		}
		resp.Message = msg
	}
	return resp, nil
}

// streamGenerate posts a streamGenerateContent?alt=sse request: every SSE data
// chunk is one generateContent-shaped JSON document (Python parity: the
// streaming loop — chunk events per text part, usage and finish reason from the
// latest chunk carrying them, one ModelMessage at the end).
func (c *client) streamGenerate(ctx context.Context, body map[string]any, headers map[string]string, conv *agocontext.Context) (*events.ModelResponse, error) {
	var (
		acc        genAccumulator
		usage      events.Usage
		finish     string
		responseID string
	)
	url := c.endpoint("streamGenerateContent") + "?alt=sse"
	err := c.http.PostJSONStream(ctx, url, headers, body, func(data []byte) error {
		var chunk genResponse
		if err := json.Unmarshal(data, &chunk); err != nil {
			return fmt.Errorf("gemini: decoding stream chunk: %w", err)
		}
		for _, candidate := range chunk.Candidates {
			if candidate.Content == nil {
				continue
			}
			for _, part := range candidate.Content.Parts {
				if err := acc.addPart(part, conv, true); err != nil {
					return err
				}
			}
		}
		if chunk.UsageMetadata != nil {
			usage = normalizeUsage(chunk.UsageMetadata)
		}
		if fr := finishReasonOf(&chunk); fr != "" {
			finish = fr
		}
		if chunk.ResponseID != "" {
			responseID = chunk.ResponseID
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	resp := &events.ModelResponse{
		ToolCalls:    events.ToolCallsEvent{Calls: acc.calls},
		Usage:        usage,
		Model:        c.cfg.model,
		Provider:     string(config.ProviderGemini),
		FinishReason: finish,
		Files:        acc.files,
		ResponseID:   responseID,
	}
	if acc.content.Len() > 0 {
		msg := &events.ModelMessage{Content: acc.content.String()}
		if err := sendEvent(conv, msg); err != nil {
			return nil, err
		}
		resp.Message = msg
	}
	return resp, nil
}

// finishReasonOf lowercases the first candidate's finishReason (Python parity:
// fr.name.lower()).
func finishReasonOf(raw *genResponse) string {
	if len(raw.Candidates) == 0 {
		return ""
	}
	return strings.ToLower(raw.Candidates[0].FinishReason)
}

// sendEvent publishes a progress event on the conversation stream; a nil conv
// (headless call) drops it.
func sendEvent(conv *agocontext.Context, ev events.Event) error {
	if conv == nil {
		return nil
	}
	return conv.Send(ev)
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
