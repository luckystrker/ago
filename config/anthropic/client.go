// The Messages API client: one LLM call over POST {base}/v1/messages, JSON or
// SSE. Parity with ag2/config/anthropic/anthropic_client.py (__call__,
// _process_response, _process_stream) over the raw internal/httpx transport.

package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/internal/httpx"
)

// mcpClientBeta is the beta flag the MCP connector answers to; the request
// carries it exactly when mcp_servers is non-empty (Python parity:
// anthropic_client.py, the `betas` set).
const mcpClientBeta = "mcp-client-2025-11-20"

// Client performs Messages-API calls with one resolved configuration. Safe for
// concurrent use; created fresh per turn by AnthropicConfig.Create.
type Client struct {
	model         string
	apiKey        string
	baseURL       string
	maxTokens     int64
	stopSequences []string
	temperature   *float64
	topP          *float64
	streaming     bool
	timeout       time.Duration // caps non-streaming requests; 0 = no cap
	maxRetries    int
	promptCaching bool
}

// Call performs one model call over the full event history. conv carries the
// system prompt and receives streaming progress (ModelMessageChunk /
// ModelReasoning); a nil conv is a headless call. The returned ModelResponse is
// the durable transcript entry.
func (c *Client) Call(ctx context.Context, messages []events.Event, conv *agocontext.Context, opts config.CallOptions) (*events.ModelResponse, error) {
	var prompt []string
	if conv != nil { // a nil conv is a headless call
		prompt = conv.PromptLines()
	}
	if opts.ResponseSchema != nil {
		// Structured output is prompt-driven on this provider (Python sends a
		// native output_config; the brief scopes the port to the prompt): the
		// schema's own system prompt carries the JSON requirement, validation
		// happens above via response.Proto.
		if sp := opts.ResponseSchema.SystemPrompt(); sp != "" {
			prompt = append(prompt, sp)
		}
	}

	turns, err := ConvertMessages(messages)
	if err != nil {
		return nil, err
	}

	body := map[string]any{
		"model":      c.model,
		"max_tokens": c.maxTokens,
		"messages":   turns,
	}
	if len(prompt) > 0 {
		text := strings.Join(prompt, "\n")
		if c.promptCaching {
			body["system"] = []map[string]any{
				{"type": "text", "text": text, "cache_control": map[string]any{"type": "ephemeral"}},
			}
		} else {
			body["system"] = text
		}
	}

	wireTools, err := ConvertTools(opts.Tools)
	if err != nil {
		return nil, err
	}
	if len(wireTools) > 0 {
		// The last tool closes the cacheable prefix of the request (brief:
		// cache_control on the last system block and the last tool).
		if c.promptCaching {
			wireTools[len(wireTools)-1]["cache_control"] = map[string]any{"type": "ephemeral"}
		}
		body["tools"] = wireTools
	}
	// The tools[] mcp_toolset entries reference servers defined here (Python
	// parity: extra_body {"mcp_servers": [...]}) — ExtractMCPServers collects
	// them from the same hosted schemas ConvertTools mapped.
	var betas []string
	if servers := ExtractMCPServers(opts.Tools); len(servers) > 0 {
		body["mcp_servers"] = servers
		// The connector's tools[] mcp_toolset blocks only work under the beta
		// flag, so the body field and the header travel together.
		betas = append(betas, mcpClientBeta)
	}
	if len(c.stopSequences) > 0 {
		body["stop_sequences"] = c.stopSequences
	}
	if c.temperature != nil {
		body["temperature"] = *c.temperature
	}
	if c.topP != nil {
		body["top_p"] = *c.topP
	}
	if c.streaming {
		body["stream"] = true
	}

	headers := map[string]string{
		"x-api-key":         c.apiKey,
		"anthropic-version": APIVersion,
	}
	if len(betas) > 0 {
		// Python joins sorted(betas | user-set default betas); the AGo config
		// has no default_headers field to fold in (AGO-DEVIATION, files.go),
		// so the framework-set flags go out alone.
		slices.Sort(betas)
		headers["anthropic-beta"] = strings.Join(betas, ",")
	}
	client := &httpx.Client{MaxRetries: c.maxRetries, Timeout: c.timeout}
	url := c.baseURL + messagesPath

	if c.streaming {
		return c.stream(ctx, client, url, headers, body, conv)
	}
	return c.once(ctx, client, url, headers, body, conv)
}

// --- non-streaming ----------------------------------------------------------------

// wireBlock is one content block of a response; only the fields the mapped
// block kinds (text / tool_use / thinking) carry are decoded.
type wireBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
}

type wireMessage struct {
	ID         string      `json:"id"`
	Model      string      `json:"model"`
	Content    []wireBlock `json:"content"`
	StopReason string      `json:"stop_reason"`
	Usage      *wireUsage  `json:"usage"`
}

type wireError struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) once(ctx context.Context, client *httpx.Client, url string, headers map[string]string, body map[string]any, conv *agocontext.Context) (*events.ModelResponse, error) {
	status, data, err := client.PostJSON(ctx, url, headers, body)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("anthropic: messages API returned %d: %s", status, apiErrorSnippet(data))
	}
	var raw wireMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("anthropic: decoding message response: %w", err)
	}

	resp := &events.ModelResponse{
		Model:        raw.Model,
		Provider:     string(config.ProviderAnthropic),
		FinishReason: MapStopReason(raw.StopReason),
		ResponseID:   raw.ID,
	}
	for _, block := range raw.Content {
		switch block.Type {
		case "thinking":
			if block.Thinking != "" {
				if err := sendEvent(conv, &events.ModelReasoning{Content: block.Thinking}); err != nil {
					return nil, err
				}
			}
		case "text":
			// Python parity: every text block is announced, the last one stays
			// on the durable response.
			msg := &events.ModelMessage{Content: block.Text}
			if err := sendEvent(conv, msg); err != nil {
				return nil, err
			}
			resp.Message = msg
		case "tool_use":
			args := string(block.Input)
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			resp.ToolCalls.Calls = append(resp.ToolCalls.Calls, events.ToolCallEvent{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: args,
			})
		default:
			// Unknown block kinds (server_tool_use, redacted_thinking,
			// container_upload, ...) have no AGo replay event yet: dropped.
			// AGO-DEVIATION: Python re-emits them as provider-specific replay
			// events; the AGo vocabulary has no carrier for them.
		}
	}
	if raw.Usage != nil {
		resp.Usage = NormalizeUsage(*raw.Usage)
	}
	return resp, nil
}

// --- streaming ---------------------------------------------------------------------

type streamEvent struct {
	Type string `json:"type"`

	// message_start
	Message *wireMessage `json:"message"`

	// content_block_start / content_block_delta / content_block_stop
	Index        int       `json:"index"`
	ContentBlock wireBlock `json:"content_block"`
	Delta        struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`

	// message_delta
	Usage *wireUsage `json:"usage"`

	// error (in-band: e.g. an overloaded_error delivered mid-stream)
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// toolAcc accumulates one streamed tool_use block between its
// content_block_start and content_block_stop.
type toolAcc struct {
	id, name, arguments string
}

func (c *Client) stream(ctx context.Context, client *httpx.Client, url string, headers map[string]string, body map[string]any, conv *agocontext.Context) (*events.ModelResponse, error) {
	var (
		content      strings.Builder
		calls        []events.ToolCallEvent
		accs         = map[int]*toolAcc{}
		startUsage   *wireUsage // message_start: input side + an output_tokens placeholder
		deltaUsage   *wireUsage // message_delta: the final output side
		finishReason string
		responseID   string
		resolved     string
	)
	err := client.PostJSONStream(ctx, url, headers, body, func(data []byte) error {
		var ev streamEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("anthropic: decoding stream event: %w", err)
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				responseID = ev.Message.ID
				resolved = ev.Message.Model
				if ev.Message.Usage != nil {
					u := *ev.Message.Usage
					startUsage = &u
				}
			}
		case "content_block_start":
			if ev.ContentBlock.Type == "tool_use" {
				accs[ev.Index] = &toolAcc{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
			}
			// text / thinking blocks carry no state to open.
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				content.WriteString(ev.Delta.Text)
				if err := sendEvent(conv, &events.ModelMessageChunk{Content: ev.Delta.Text}); err != nil {
					return err
				}
			case "thinking_delta":
				if err := sendEvent(conv, &events.ModelReasoning{Content: ev.Delta.Thinking}); err != nil {
					return err
				}
			case "input_json_delta":
				if acc, ok := accs[ev.Index]; ok {
					acc.arguments += ev.Delta.PartialJSON
				}
			}
		case "content_block_stop":
			if acc, ok := accs[ev.Index]; ok {
				args := acc.arguments
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				calls = append(calls, events.ToolCallEvent{
					ID:        acc.id,
					Name:      acc.name,
					Arguments: args,
				})
				delete(accs, ev.Index)
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				finishReason = MapStopReason(ev.Delta.StopReason)
			}
			if ev.Usage != nil {
				// The delta reports the final output side; it replaces the
				// message_start fields (whose output_tokens:1 placeholder must
				// not accumulate) — the merge happens once, after the stream.
				u := *ev.Usage
				deltaUsage = &u
			}
		case "error":
			// An in-band error fails the call — Python parity: the SDK's
			// stream iterator raises the typed exception instead of ending
			// the stream as a successful empty response.
			if ev.Error == nil {
				return config.ErrStreamError
			}
			if ev.Error.Type != "" {
				return fmt.Errorf("%w: %s: %s", config.ErrStreamError, ev.Error.Type, ev.Error.Message)
			}
			return fmt.Errorf("%w: %s", config.ErrStreamError, ev.Error.Message)
		default:
			// ping / message_stop / unknown: no state.
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	resp := &events.ModelResponse{
		Usage:        mergeStreamUsage(startUsage, deltaUsage),
		Model:        resolved,
		Provider:     string(config.ProviderAnthropic),
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
	resp.ToolCalls = events.ToolCallsEvent{Calls: calls}
	return resp, nil
}

// mergeStreamUsage assembles the call's usage the way the final message of a
// Messages-API stream reports it: message_start carries the input side with an
// output_tokens placeholder, message_delta the final output side. The delta's
// output fields replace the start's (never add to them); the input side comes
// from the start, with a delta-reported field filling a gap. TotalTokens is
// recomputed from the merged sides.
func mergeStreamUsage(start, delta *wireUsage) events.Usage {
	switch {
	case start == nil && delta == nil:
		return events.Usage{}
	case start == nil:
		return NormalizeUsage(*delta)
	case delta == nil:
		return NormalizeUsage(*start)
	}
	u := NormalizeUsage(*start)
	d := NormalizeUsage(*delta)
	if d.CompletionTokens != nil {
		u.CompletionTokens = d.CompletionTokens
	}
	if d.ThinkingTokens != nil {
		u.ThinkingTokens = d.ThinkingTokens
	}
	if u.PromptTokens == nil {
		u.PromptTokens = d.PromptTokens
	}
	if u.CacheReadInputTokens == nil {
		u.CacheReadInputTokens = d.CacheReadInputTokens
	}
	if u.CacheCreationInputTokens == nil {
		u.CacheCreationInputTokens = d.CacheCreationInputTokens
	}
	u.TotalTokens = nil
	if u.PromptTokens != nil || u.CompletionTokens != nil {
		total := int64(0)
		if u.PromptTokens != nil {
			total += *u.PromptTokens
		}
		if u.CompletionTokens != nil {
			total += *u.CompletionTokens
		}
		u.TotalTokens = &total
	}
	return u
}

// sendEvent publishes a progress event on the conversation stream; a nil conv
// (headless call) drops it.
func sendEvent(conv *agocontext.Context, ev events.Event) error {
	if conv == nil {
		return nil
	}
	return conv.Send(ev)
}

// apiErrorSnippet renders an API error body, preferring the Anthropic error
// envelope's message over the raw bytes.
func apiErrorSnippet(data []byte) string {
	var env wireError
	if err := json.Unmarshal(data, &env); err == nil && env.Error.Message != "" {
		if env.Error.Type != "" {
			return env.Error.Type + ": " + env.Error.Message
		}
		return env.Error.Message
	}
	const max = 512
	s := strings.TrimSpace(string(data))
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
