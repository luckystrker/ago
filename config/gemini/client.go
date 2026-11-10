package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/internal/httpx"
	"github.com/luckystrker/ago/response"
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
	if c.cfg.presencePenalty != nil {
		gen["presencePenalty"] = *c.cfg.presencePenalty
	}
	if c.cfg.frequencyPenalty != nil {
		gen["frequencyPenalty"] = *c.cfg.frequencyPenalty
	}
	if c.cfg.seed != nil {
		gen["seed"] = *c.cfg.seed
	}
	// Both thinking knobs resolve into one thinkingConfig object (Python
	// parity: _resolve_thinking_config — level, budget or both).
	if c.cfg.thinkingLevel != nil || c.cfg.thinkingBudget != nil {
		thinking := map[string]any{}
		if c.cfg.thinkingLevel != nil {
			thinking["thinkingLevel"] = *c.cfg.thinkingLevel
		}
		if c.cfg.thinkingBudget != nil {
			thinking["thinkingBudget"] = *c.cfg.thinkingBudget
		}
		gen["thinkingConfig"] = thinking
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

// generateContent posts generateContent requests until the answer stops carrying
// a resumption token: a response the server truncated at its per-request token
// limit (finishReason CONTINUATION plus a continuationToken) is resumed with the
// top-level "continuationToken" key, and the hops join — text concatenated,
// usage summed, finish reason and response id from the last hop (Python parity:
// the google-genai SDK's _generate_content_with_continuation; the port carries
// the loop because this package speaks raw REST without the SDK).
func (c *client) generateContent(ctx context.Context, body map[string]any, headers map[string]string, conv *agocontext.Context) (*events.ModelResponse, error) {
	var (
		acc        genAccumulator
		usage      events.Usage
		finish     string
		responseID string
	)
	for {
		status, data, err := c.http.PostJSON(ctx, c.endpoint("generateContent"), headers, body)
		if err != nil {
			return nil, err
		}
		if status < 200 || status >= 300 {
			return nil, config.NewAPIError(config.ProviderGemini, "gemini: generateContent", status, nil, data)
		}
		var raw genResponse
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("gemini: decoding generateContent response: %w", err)
		}

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
		// Every hop is billed separately: usage adds up, while finish reason and
		// response id come from the last hop unconditionally
		// (merge_continuation_responses copies them from curr).
		usage = usage.Add(normalizeUsage(raw.UsageMetadata))
		finish = finishReasonOf(&raw)
		if raw.ResponseID != "" {
			responseID = raw.ResponseID
		}

		token, ok := continuationOf(&raw)
		if !c.cfg.continuationEnabled() || !ok {
			break
		}
		body["continuationToken"] = token
	}

	resp := &events.ModelResponse{
		ToolCalls:    events.ToolCallsEvent{Calls: acc.calls},
		Usage:        usage,
		Model:        c.cfg.model,
		Provider:     string(config.ProviderGemini),
		FinishReason: finish,
		Files:        acc.files,
		// AGO-DEVIATION: Python leaves response_id unset; the REST response
		// carries one and the event has the tracing field for it.
		ResponseID: responseID,
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
// latest chunk carrying them, one ModelMessage at the end). A stream whose last
// chunk carries finishReason CONTINUATION plus a continuationToken is resumed
// with the top-level "continuationToken" key; the hops join like the
// non-streaming path.
func (c *client) streamGenerate(ctx context.Context, body map[string]any, headers map[string]string, conv *agocontext.Context) (*events.ModelResponse, error) {
	var (
		acc        genAccumulator
		usage      events.Usage
		finish     string
		responseID string
	)
	url := c.endpoint("streamGenerateContent") + "?alt=sse"
	for {
		// Per-hop state: inside one stream usageMetadata is a running total the
		// latest chunk carrying it replaces, not a sum.
		var (
			hopUsage  events.Usage
			hopFinish string
			hopToken  string
			hopResume bool
		)
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
				hopUsage = normalizeUsage(chunk.UsageMetadata)
			}
			if fr := finishReasonOf(&chunk); fr != "" {
				hopFinish = fr
			}
			if chunk.ResponseID != "" {
				responseID = chunk.ResponseID
			}
			// Resumption keys off the last chunk of the hop.
			hopToken, hopResume = continuationOf(&chunk)
			return nil
		})
		if err != nil {
			return nil, config.WrapStatusError(config.ProviderGemini, "gemini: streamGenerateContent", err)
		}
		usage = usage.Add(hopUsage)
		// The last hop wins even when it carries no finish reason.
		finish = hopFinish

		if !c.cfg.continuationEnabled() || !hopResume {
			break
		}
		body["continuationToken"] = hopToken
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

// continuationOf reports the resumption token of a response and whether the
// answer may be resumed: the token is mandatory, and the finish reason must be
// absent or CONTINUATION (Python parity: should_continue_generation +
// is_resumable_finish_reason). The token travels back verbatim as the
// top-level "continuationToken" of the next request; the
// automatic_continuation flag itself never reaches the wire.
//
// SDK internals invisible through the option's surface are deliberately not
// ported: reconnecting a stream broken mid-hop (the SDK retries it three
// times), the SDK's cross-hop timeout budget, and the SDK's rewriting of
// usage/finish_reason inside the delivered chunk events (AGo's chunk events
// carry neither).
func continuationOf(raw *genResponse) (token string, ok bool) {
	if len(raw.Candidates) == 0 {
		return "", false
	}
	token = raw.Candidates[0].ContinuationToken
	if token == "" {
		return "", false
	}
	fr := raw.Candidates[0].FinishReason
	return token, fr == "" || strings.EqualFold(fr, "CONTINUATION")
}

// sendEvent publishes a progress event on the conversation stream; a nil conv
// (headless call) drops it.
func sendEvent(conv *agocontext.Context, ev events.Event) error {
	if conv == nil {
		return nil
	}
	return conv.Send(ev)
}
