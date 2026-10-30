// The OpenAI Responses API: the typed config, its client and the wire mapping
// (event history → input[], tool schemas → tools[], hosted tool output items →
// events, usage normalization, strict text.format). Parity with
// ag2/ag2/config/openai/{openai_responses_client,config}.py and mappers.py
// (events_to_responses_input, tool_to_responses_api, response_proto_to_text_config,
// normalize_responses_usage).
//
// This is the one OpenAI surface that carries hosted tools (web_search,
// file_search, code_interpreter, image_generation, shell, mcp, tool_search):
// the Chat-Completions engine in ago/config rejects them by design.

package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/luckystrker/ago/files"
	"github.com/luckystrker/ago/internal/httpx"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/response"
	"github.com/luckystrker/ago/tools"
)

// Compile-time conformance with the provider contracts.
var (
	_ config.ModelConfig = (*OpenAIResponsesConfig)(nil)
	_ config.LLMClient   = (*ResponsesClient)(nil)
)

// responsesPath is the URL path under the base URL (DefaultBaseURL already
// carries /v1).
const responsesPath = "/responses"

// Reasoning is the reasoning parameter of the Responses API (Python parity:
// the Reasoning dict with effort/summary keys). Empty fields are omitted.
type Reasoning struct {
	// Effort is the reasoning effort: "minimal", "low", "medium" or "high".
	Effort string `json:"effort,omitempty"`
	// Summary selects reasoning summaries: "auto", "concise" or "detailed".
	Summary string `json:"summary,omitempty"`
}

// OpenAIResponsesConfig configures the Responses API. Construct it with
// NewOpenAIResponses (the only way to set the model) and assign the exported
// fields. Immutable after construction: Create is safe for concurrent use and
// may be called repeatedly.
//
// AGO-DEVIATION: the model travels behind the Model() accessor like on
// OpenAICompletionsConfig (the config.ModelConfig contract owns the name).
type OpenAIResponsesConfig struct {
	// APIKey authenticates requests. Empty falls back to OPENAI_API_KEY at
	// Create() time; neither set → Create() wraps config.ErrNotProvided.
	APIKey string
	// BaseURL overrides the endpoint root. Empty → DefaultBaseURL.
	BaseURL string

	Temperature *float64
	TopP        *float64
	// MaxOutputTokens travels as max_output_tokens.
	MaxOutputTokens *int64
	// MaxToolCalls caps hosted-tool invocations per response.
	MaxToolCalls *int
	// Store controls server-side persistence. A nil pointer means the Python
	// dataclass default (true): the field always travels on the wire.
	Store *bool
	// ParallelToolCalls travels as parallel_tool_calls. Nil means the Python
	// default (true); the field always travels.
	ParallelToolCalls *bool
	// TopLogprobs travels as top_logprobs (omitted when nil).
	TopLogprobs *int
	// Metadata is the response-level metadata dict (omitted when nil).
	Metadata map[string]string
	// PromptCacheKey is a prompt-cache routing hint (omitted when empty).
	PromptCacheKey string
	// PromptCacheOptions travels as prompt_cache_options (omitted when empty —
	// an empty object is not a setting, and a model without the feature 400s
	// on one).
	PromptCacheOptions map[string]any
	// Reasoning configures reasoning (omitted when nil).
	Reasoning *Reasoning
	// ServiceTier travels as service_tier (omitted when empty).
	ServiceTier string
	// User travels as user when non-empty.
	User string
	// Truncation is "auto" or "disabled" (omitted when empty).
	Truncation string

	// Streaming switches the call to SSE consumption.
	Streaming bool
	// Timeout caps a non-streaming request (0 = no cap; streams rely on the
	// call's context for cancellation).
	Timeout time.Duration
	// MaxRetries is the transport retry count for 429/5xx and connection
	// failures (SDK default, applied by NewOpenAIResponses).
	MaxRetries int

	// Organization and Project become the OpenAI-Organization and
	// OpenAI-Project request headers.
	Organization string
	Project      string

	// DefaultHeaders are sent with every request on top of the auth and
	// organization/project headers; an entry here overrides those.
	DefaultHeaders map[string]string

	// ExtraBody merges into the request body last, overriding any field the
	// config derived (SDK parity: extra_body).
	ExtraBody map[string]any

	model string
}

// NewOpenAIResponses returns a Responses API config for model, with the SDK
// defaults: store and parallel tool calls on, streaming off, no timeout cap,
// 4 transport retries.
func NewOpenAIResponses(model string) *OpenAIResponsesConfig {
	return &OpenAIResponsesConfig{model: model, MaxRetries: 4}
}

// Provider reports the vendor (ModelConfig contract).
func (c *OpenAIResponsesConfig) Provider() config.Provider { return config.ProviderOpenAI }

// Model returns the model identifier sent with every call (ModelConfig
// contract).
func (c *OpenAIResponsesConfig) Model() string { return c.model }

// Create builds a ready-to-call client (ModelConfig contract; Python parity:
// a fresh client per create()). The API key comes from the config, then
// OPENAI_API_KEY; a missing model or key wraps config.ErrNotProvided. The
// BaseURL defaults to DefaultBaseURL.
func (c *OpenAIResponsesConfig) Create() (config.LLMClient, error) {
	key, baseURL, headers, err := c.credentials()
	if err != nil {
		return nil, err
	}
	return &ResponsesClient{
		apiKey:  key,
		baseURL: baseURL,
		headers: headers,
		params:  c,
	}, nil
}

// credentials resolves the API key (config field, then OPENAI_API_KEY), the
// base URL and the request headers shared by Create and CreateFilesClient.
func (c *OpenAIResponsesConfig) credentials() (key, baseURL string, headers map[string]string, err error) {
	if c.model == "" {
		return "", "", nil, fmt.Errorf("config/openai: %w: model", config.ErrNotProvided)
	}
	key = c.APIKey
	if key == "" {
		key = os.Getenv(envAPIKey)
	}
	if key == "" {
		return "", "", nil, fmt.Errorf("config/openai: %w: api key (set APIKey or %s)", config.ErrNotProvided, envAPIKey)
	}
	baseURL = c.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	headers = map[string]string{}
	if c.Organization != "" {
		headers["OpenAI-Organization"] = c.Organization
	}
	if c.Project != "" {
		headers["OpenAI-Project"] = c.Project
	}
	for k, v := range c.DefaultHeaders {
		headers[k] = v
	}
	return key, strings.TrimSuffix(baseURL, "/"), headers, nil
}

// CreateFilesClient returns the Files API client for this configuration
// (Python parity: OpenAIResponsesConfig.create_files_client). The key
// resolution and header defaults match Create.
func (c *OpenAIResponsesConfig) CreateFilesClient() (files.Client, error) {
	key, baseURL, headers, err := c.credentials()
	if err != nil {
		return nil, err
	}
	return &FilesClient{
		apiKey:         key,
		baseURL:        baseURL,
		organization:   c.Organization,
		project:        c.Project,
		defaultHeaders: headers,
		timeout:        c.Timeout,
		maxRetries:     c.MaxRetries,
		http:           &http.Client{Timeout: c.Timeout},
	}, nil
}

// Clone returns a copy with the overrides applied (ModelConfig contract;
// Python parity: dataclasses.replace). Keys are the Go field names, with the
// Python snake_case wire names accepted as aliases; values must keep the
// field's type. An unknown key or a mismatched value is an error and the
// original config is left untouched.
func (c *OpenAIResponsesConfig) Clone(overrides map[string]any) (config.ModelConfig, error) {
	cp := *c
	for k, v := range overrides {
		if err := cp.set(k, v); err != nil {
			return nil, err
		}
	}
	return &cp, nil
}

// set applies one override to cp (see Clone). A nil value clears the field
// (Python parity: dataclasses.replace(api_key=None)).
func (c *OpenAIResponsesConfig) set(key string, v any) error {
	str := func(dst *string) error {
		if v == nil {
			*dst = ""
			return nil
		}
		s, ok := v.(string)
		if !ok {
			return overrideTypeErr(key, v, "string")
		}
		*dst = s
		return nil
	}
	strMap := func(dst *map[string]string) error {
		if v == nil {
			*dst = nil
			return nil
		}
		m, ok := v.(map[string]string)
		if !ok {
			return overrideTypeErr(key, v, "map[string]string")
		}
		*dst = m
		return nil
	}
	anyMap := func(dst *map[string]any) error {
		if v == nil {
			*dst = nil
			return nil
		}
		m, ok := v.(map[string]any)
		if !ok {
			return overrideTypeErr(key, v, "map[string]any")
		}
		*dst = m
		return nil
	}
	ptrFloat := func(dst **float64) error {
		if v == nil {
			*dst = nil
			return nil
		}
		f, ok := v.(*float64)
		if !ok {
			return overrideTypeErr(key, v, "*float64")
		}
		*dst = f
		return nil
	}
	switch key {
	case "Model", "model":
		return str(&c.model)
	case "APIKey", "api_key":
		return str(&c.APIKey)
	case "BaseURL", "base_url":
		return str(&c.BaseURL)
	case "Temperature", "temperature":
		return ptrFloat(&c.Temperature)
	case "TopP", "top_p":
		return ptrFloat(&c.TopP)
	case "MaxOutputTokens", "max_output_tokens":
		if v == nil {
			c.MaxOutputTokens = nil
			return nil
		}
		n, ok := v.(*int64)
		if !ok {
			return overrideTypeErr(key, v, "*int64")
		}
		c.MaxOutputTokens = n
	case "MaxToolCalls", "max_tool_calls":
		if v == nil {
			c.MaxToolCalls = nil
			return nil
		}
		n, ok := v.(*int)
		if !ok {
			return overrideTypeErr(key, v, "*int")
		}
		c.MaxToolCalls = n
	case "Store", "store":
		if v == nil {
			c.Store = nil
			return nil
		}
		bv, ok := v.(*bool)
		if !ok {
			return overrideTypeErr(key, v, "*bool")
		}
		c.Store = bv
	case "ParallelToolCalls", "parallel_tool_calls":
		if v == nil {
			c.ParallelToolCalls = nil
			return nil
		}
		bv, ok := v.(*bool)
		if !ok {
			return overrideTypeErr(key, v, "*bool")
		}
		c.ParallelToolCalls = bv
	case "TopLogprobs", "top_logprobs":
		if v == nil {
			c.TopLogprobs = nil
			return nil
		}
		n, ok := v.(*int)
		if !ok {
			return overrideTypeErr(key, v, "*int")
		}
		c.TopLogprobs = n
	case "Metadata", "metadata":
		return strMap(&c.Metadata)
	case "PromptCacheKey", "prompt_cache_key":
		return str(&c.PromptCacheKey)
	case "PromptCacheOptions", "prompt_cache_options":
		return anyMap(&c.PromptCacheOptions)
	case "Reasoning", "reasoning":
		if v == nil {
			c.Reasoning = nil
			return nil
		}
		r, ok := v.(*Reasoning)
		if !ok {
			return overrideTypeErr(key, v, "*Reasoning")
		}
		c.Reasoning = r
	case "ServiceTier", "service_tier":
		return str(&c.ServiceTier)
	case "User", "user":
		return str(&c.User)
	case "Truncation", "truncation":
		return str(&c.Truncation)
	case "Streaming", "streaming":
		bv, ok := v.(bool)
		if !ok {
			return overrideTypeErr(key, v, "bool")
		}
		c.Streaming = bv
	case "Timeout", "timeout":
		d, ok := v.(time.Duration)
		if !ok {
			return overrideTypeErr(key, v, "time.Duration")
		}
		c.Timeout = d
	case "MaxRetries", "max_retries":
		n, ok := v.(int)
		if !ok {
			return overrideTypeErr(key, v, "int")
		}
		c.MaxRetries = n
	case "Organization", "organization":
		return str(&c.Organization)
	case "Project", "project":
		return str(&c.Project)
	case "DefaultHeaders", "default_headers":
		return strMap(&c.DefaultHeaders)
	case "ExtraBody", "extra_body":
		return anyMap(&c.ExtraBody)
	default:
		return fmt.Errorf("config/openai: unknown override key %q", key)
	}
	return nil
}

// ResponsesClient is the Responses API client built by Create: one configured
// endpoint with its auth and headers, stateless per call. Safe for concurrent
// use. AGO-DEVIATION: Python tracks the last response id on the client for the
// prompt-cache-diagnostics chaining; AGo does not — the diagnostics event has
// no AGo carrier (docs/DECISIONS.md D-060), so the id travels only on each
// ModelResponse. Implements config.LLMClient.
type ResponsesClient struct {
	apiKey  string
	baseURL string
	headers map[string]string
	params  *OpenAIResponsesConfig
}

// Call performs one Responses API call: the full event history replays as
// input[], streaming progress is published on conv, and the returned
// ModelResponse is the durable answer.
func (c *ResponsesClient) Call(ctx context.Context, messages []events.Event, conv *agocontext.Context, opts config.CallOptions) (*events.ModelResponse, error) {
	p := c.params

	input, err := responsesInput(messages)
	if err != nil {
		return nil, err
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

	wireTools := make([]map[string]any, 0, len(opts.Tools))
	for _, t := range opts.Tools {
		wt, err := toolToResponsesAPI(t)
		if err != nil {
			return nil, err
		}
		wireTools = append(wireTools, wt)
	}

	includes := responsesIncludes(opts.Tools)
	store := true
	if p.Store != nil {
		store = *p.Store
	}
	if !store {
		// Stateless mode: the API persists nothing, so replaying a reasoning
		// item by id on a later turn (a tool loop always does) would fail.
		// Asking for the encrypted payload makes those items self-contained.
		includes = append(includes, "reasoning.encrypted_content")
	}

	body := map[string]any{
		"model": p.model,
		"input": input,
	}
	// Python parity: instructions stay unset when the joined prompt is empty.
	if instructions := strings.Join(prompt, "\n"); instructions != "" {
		body["instructions"] = instructions
	}
	if len(wireTools) > 0 {
		body["tools"] = wireTools
	}
	if len(includes) > 0 {
		body["include"] = includes
	}
	if tc := responsesTextConfig(opts.ResponseSchema); tc != nil {
		body["text"] = tc
	}
	if p.Streaming {
		body["stream"] = true
	}
	// store and parallel_tool_calls always travel: the Python dataclass
	// defaults (true) are sent unless overridden.
	body["store"] = store
	parallel := true
	if p.ParallelToolCalls != nil {
		parallel = *p.ParallelToolCalls
	}
	body["parallel_tool_calls"] = parallel
	if p.Temperature != nil {
		body["temperature"] = *p.Temperature
	}
	if p.TopP != nil {
		body["top_p"] = *p.TopP
	}
	if p.MaxOutputTokens != nil {
		body["max_output_tokens"] = *p.MaxOutputTokens
	}
	if p.MaxToolCalls != nil {
		body["max_tool_calls"] = *p.MaxToolCalls
	}
	if p.TopLogprobs != nil {
		body["top_logprobs"] = *p.TopLogprobs
	}
	if p.Metadata != nil {
		body["metadata"] = p.Metadata
	}
	if p.PromptCacheKey != "" {
		body["prompt_cache_key"] = p.PromptCacheKey
	}
	if len(p.PromptCacheOptions) > 0 {
		body["prompt_cache_options"] = p.PromptCacheOptions
	}
	if p.Reasoning != nil {
		body["reasoning"] = p.Reasoning
	}
	if p.ServiceTier != "" {
		body["service_tier"] = p.ServiceTier
	}
	if p.User != "" {
		body["user"] = p.User
	}
	if p.Truncation != "" {
		body["truncation"] = p.Truncation
	}
	for k, v := range p.ExtraBody {
		body[k] = v
	}

	headers := map[string]string{"Authorization": "Bearer " + c.apiKey}
	for k, v := range c.headers {
		headers[k] = v
	}

	client := &httpx.Client{MaxRetries: p.MaxRetries, Timeout: p.Timeout}
	url := joinURL(c.baseURL, responsesPath)

	var resp *events.ModelResponse
	if p.Streaming {
		resp, err = runResponsesStream(ctx, client, url, headers, body, conv)
	} else {
		resp, err = runResponsesOnce(ctx, client, url, headers, body, conv)
	}
	if err != nil {
		return nil, err
	}
	resp.Provider = string(config.ProviderOpenAI)
	return resp, nil
}

// --- non-streaming response ---------------------------------------------------

func runResponsesOnce(ctx context.Context, client *httpx.Client, url string, headers map[string]string, body map[string]any, conv *agocontext.Context) (*events.ModelResponse, error) {
	status, data, err := client.PostJSON(ctx, url, headers, body)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("config/openai: responses returned %d: %s", status, bodySnippet(data))
	}
	var raw struct {
		ID     string           `json:"id"`
		Model  string           `json:"model"`
		Status string           `json:"status"`
		Output []map[string]any `json:"output"`
		Usage  *map[string]any  `json:"usage"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("config/openai: decoding response: %w", err)
	}
	if raw.Error != nil && raw.Error.Message != "" {
		return nil, fmt.Errorf("%w: %s", config.ErrStreamError, raw.Error.Message)
	}

	acc := newTurnAcc()
	for _, item := range raw.Output {
		if err := acc.processItem(item, conv); err != nil {
			return nil, err
		}
	}
	if raw.Usage != nil {
		acc.usage = normalizeResponsesUsage(*raw.Usage)
	}
	resp := acc.response(raw.Model, raw.Status, raw.ID)
	resp.Message = acc.lastMessage
	return resp, nil
}

// --- streaming response ---------------------------------------------------------

func runResponsesStream(ctx context.Context, client *httpx.Client, url string, headers map[string]string, body map[string]any, conv *agocontext.Context) (*events.ModelResponse, error) {
	acc := newTurnAcc()
	err := client.PostJSONStream(ctx, url, headers, body, func(data []byte) error {
		var ev struct {
			Type    string          `json:"type"`
			Delta   string          `json:"delta"`
			Item    map[string]any  `json:"item"`
			Resp    *streamResponse `json:"response"`
			Message string          `json:"message"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("config/openai: decoding stream event: %w", err)
		}
		switch ev.Type {
		case "response.output_text.delta":
			if ev.Delta != "" {
				acc.content.WriteString(ev.Delta)
				if err := sendEvent(conv, &events.ModelMessageChunk{Content: ev.Delta}); err != nil {
					return err
				}
			}
		case "response.output_item.done":
			// Builtin and reasoning events are emitted on Done so the item is
			// fully populated (Added fires before a server-side tool ran).
			if ev.Item != nil {
				if err := acc.processItem(ev.Item, conv); err != nil {
					return err
				}
			}
		case "response.completed":
			if ev.Resp == nil {
				return nil
			}
			if ev.Resp.Usage != nil {
				acc.usage = normalizeResponsesUsage(*ev.Resp.Usage)
			}
			acc.finish, acc.model, acc.respID = ev.Resp.Status, ev.Resp.Model, ev.Resp.ID
		case "response.failed":
			msg := ""
			if ev.Resp != nil && ev.Resp.Error != nil {
				msg = ev.Resp.Error.Message
			}
			if msg == "" {
				msg = "response failed"
			}
			// An in-band failure fails the call — the SDK iterator raises the
			// same way instead of ending the stream as a success.
			return fmt.Errorf("%w: %s", config.ErrStreamError, msg)
		case "error":
			if ev.Message != "" {
				return fmt.Errorf("%w: %s", config.ErrStreamError, ev.Message)
			}
			return fmt.Errorf("%w: stream error event", config.ErrStreamError)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	resp := acc.response(acc.model, acc.finish, acc.respID)
	if acc.content.Len() > 0 {
		msg := &events.ModelMessage{Content: acc.content.String()}
		if err := sendEvent(conv, msg); err != nil {
			return nil, err
		}
		resp.Message = msg
	}
	return resp, nil
}

type streamResponse struct {
	ID     string          `json:"id"`
	Model  string          `json:"model"`
	Status string          `json:"status"`
	Usage  *map[string]any `json:"usage"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// --- turn accumulation ------------------------------------------------------------

// turnAcc accumulates one response turn; the same item walk serves the
// non-streaming output[] and the stream's output_item.done events.
type turnAcc struct {
	content     strings.Builder // streamed text deltas
	calls       []events.ToolCallEvent
	files       []events.BinaryResult
	usage       events.Usage
	finish      string
	model       string
	respID      string
	lastMessage *events.ModelMessage
	shells      shellTracker
}

func newTurnAcc() *turnAcc { return &turnAcc{shells: shellTracker{open: map[string]shellOpen{}}} }

// response renders the accumulated state as the durable ModelResponse. The
// message slot is filled by the caller (stream: the combined text; non-stream:
// the last output_text part).
func (a *turnAcc) response(model, status, id string) *events.ModelResponse {
	resp := &events.ModelResponse{
		Usage:        a.usage,
		Model:        model,
		FinishReason: status,
		ResponseID:   id,
		Files:        a.files,
	}
	resp.ToolCalls = events.ToolCallsEvent{Calls: a.calls}
	return resp
}

// processItem handles one output item: assistant text, reasoning summaries,
// function calls and hosted server-tool items. Item kinds this port does not
// model travel silently.
func (a *turnAcc) processItem(item map[string]any, conv *agocontext.Context) error {
	switch item["type"] {
	case "message":
		if role, _ := item["role"].(string); role != "" && role != "assistant" {
			return nil
		}
		parts, _ := item["content"].([]any)
		for _, p := range parts {
			part, ok := p.(map[string]any)
			if !ok || part["type"] != "output_text" {
				continue
			}
			text, _ := part["text"].(string)
			if text == "" {
				continue
			}
			msg := &events.ModelMessage{Content: text}
			if err := sendEvent(conv, msg); err != nil {
				return err
			}
			a.lastMessage = msg
		}
	case "reasoning":
		// AGO-DEVIATION: Python persists a vendor reasoning event carrying the
		// whole item and replays it into later input[] calls (the API wants it
		// back before a replayed web_search_call). AGo events.ModelReasoning is
		// transient and carries no item slot, so summaries surface on the
		// stream but reasoning items are not stored or replayed (D-060).
		for _, text := range reasoningSummaries(item) {
			if err := sendEvent(conv, &events.ModelReasoning{Content: text}); err != nil {
				return err
			}
		}
	case "function_call":
		a.calls = append(a.calls, events.ToolCallEvent{
			ID:        stringField(item, "call_id"),
			Name:      stringField(item, "name"),
			Arguments: stringField(item, "arguments"),
		})
	default:
		return a.processHostedItem(item, conv)
	}
	return nil
}

// processHostedItem maps one hosted server-tool output item to its
// BuiltinToolCallEvent (with the raw item kept for replay) and, when the item
// carries an outcome, its BuiltinToolResultEvent. Generated images also become
// Files entries.
func (a *turnAcc) processHostedItem(item map[string]any, conv *agocontext.Context) error {
	itemType := stringField(item, "type")

	if itemType == "shell_call_output" {
		// Paired with its shell_call by the tracker, which supplies the parent
		// id and the commands; a call that was never seen stays unclosed.
		if res := a.shells.close(item); res != nil {
			return sendEvent(conv, res)
		}
		return nil
	}

	name, hosted := hostedToolName(itemType)
	if !hosted {
		return nil
	}

	call := hostedCallEvent(itemType, name, item)
	if call == nil {
		return nil
	}
	if err := sendEvent(conv, call); err != nil {
		return err
	}
	if itemType == "shell_call" {
		action, _ := item["action"].(map[string]any)
		commands, _ := action["commands"].([]any)
		a.shells.opened(stringField(item, "call_id"), call.ID, commands)
	}

	res := hostedResultEvent(itemType, name, item, call.ID)
	if res == nil {
		return nil
	}
	if err := sendEvent(conv, res); err != nil {
		return err
	}
	if itemType == "image_generation_call" {
		if data, err := base64.StdEncoding.DecodeString(stringField(item, "result")); err == nil {
			a.files = append(a.files, events.BinaryResult{Data: data, Metadata: res.Result.Metadata})
		}
	}
	return nil
}

// shellTracker pairs a hosted shell_call with the shell_call_output answering
// it: the two arrive as separate items linked by call_id, and the result needs
// the commands, which only the call carries.
type shellTracker struct {
	open map[string]shellOpen
}

type shellOpen struct {
	eventID  string
	commands []any
}

func (t *shellTracker) opened(callID, eventID string, commands []any) {
	if t.open == nil {
		t.open = map[string]shellOpen{}
	}
	t.open[callID] = shellOpen{eventID: eventID, commands: commands}
}

// close returns the result event for a shell_call_output item, or nil when its
// call was never seen.
func (t *shellTracker) close(output map[string]any) *events.BuiltinToolResultEvent {
	callID := stringField(output, "call_id")
	opened, ok := t.open[callID]
	if !ok {
		return nil
	}
	delete(t.open, callID)

	parts := make([]events.Input, 0, 2)
	entries, _ := output["output"].([]any)
	outputs := make([]any, 0, len(entries))
	for _, e := range entries {
		entry, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if s := stringField(entry, "stdout"); s != "" {
			parts = append(parts, events.TextInput{Content: s})
		}
		if s := stringField(entry, "stderr"); s != "" {
			parts = append(parts, events.TextInput{Content: s})
		}
		outputs = append(outputs, entry)
	}
	return &events.BuiltinToolResultEvent{ToolResultEvent: events.ToolResultEvent{
		ParentID: opened.eventID,
		Name:     tools.ShellToolName,
		Result: events.ToolResult{
			Parts: parts,
			// AGO-DEVIATION: the raw item rides in Metadata["item"] (Python
			// keeps it on the vendor event type, which AGo has no slot for);
			// it is what gets replayed into input[] on the next call.
			Metadata: map[string]any{
				"commands": opened.commands,
				"status":   output["status"],
				"outputs":  outputs,
				"item":     output,
			},
		},
	}}
}

// hostedToolName maps a wire item type to its provider-neutral tool name.
func hostedToolName(itemType string) (string, bool) {
	switch itemType {
	case "web_search_call":
		return tools.WebSearchToolName, true
	case "code_interpreter_call":
		return tools.CodeExecutionToolName, true
	case "image_generation_call":
		return tools.ImageGenerationToolName, true
	case "file_search_call":
		return tools.FileSearchToolName, true
	case "mcp_call", "mcp_list_tools":
		return tools.MCPServerToolName, true
	case "shell_call":
		return tools.ShellToolName, true
	}
	return "", false
}

// hostedCallEvent builds the BuiltinToolCallEvent for one hosted item. The raw
// item travels in VendorMetadata["item"] — it is what gets replayed into
// input[] on the next call (Python parity: the vendor event's item dump). A nil
// result means the item produces no call (an image_generation_call without a
// result).
func hostedCallEvent(itemType, name string, item map[string]any) *events.BuiltinToolCallEvent {
	var args string
	switch itemType {
	case "web_search_call":
		action, ok := item["action"].(map[string]any)
		if !ok {
			args = "{}"
			break
		}
		b, _ := json.Marshal(action)
		args = string(b)
	case "code_interpreter_call":
		if code := stringField(item, "code"); code != "" {
			b, _ := json.Marshal(map[string]any{"code": code})
			args = string(b)
		} else {
			args = "{}"
		}
	case "image_generation_call":
		if stringField(item, "result") == "" {
			return nil
		}
	case "file_search_call":
		queries, _ := item["queries"].([]any)
		b, _ := json.Marshal(map[string]any{"queries": queries})
		args = string(b)
	case "mcp_call":
		args = stringField(item, "arguments")
	case "mcp_list_tools":
		b, _ := json.Marshal(map[string]any{"server_label": item["server_label"]})
		args = string(b)
	case "shell_call":
		action, _ := item["action"].(map[string]any)
		commands, _ := action["commands"].([]any)
		b, _ := json.Marshal(map[string]any{"commands": commands})
		args = string(b)
	}
	return &events.BuiltinToolCallEvent{ToolCallEvent: events.ToolCallEvent{
		ID:             stringField(item, "id"),
		Name:           name,
		Arguments:      args,
		VendorMetadata: map[string]any{"item": item},
	}}
}

// hostedResultEvent builds the BuiltinToolResultEvent for one hosted item; nil
// when the item carries no outcome of its own (a shell_call pairs with its
// separate shell_call_output).
func hostedResultEvent(itemType, name string, item map[string]any, parentID string) *events.BuiltinToolResultEvent {
	var parts []events.Input
	metadata := map[string]any{}

	switch itemType {
	case "web_search_call":
		action, _ := item["action"].(map[string]any)
		metadata["action_type"] = action["type"]
		metadata["status"] = item["status"]
		switch action["type"] {
		case "search":
			sources, _ := action["sources"].([]any)
			for _, s := range sources {
				source, ok := s.(map[string]any)
				if !ok {
					continue
				}
				// sources entries with an empty url are synthesised/internal;
				// skip them rather than carry a null reference.
				if u := stringField(source, "url"); u != "" {
					parts = append(parts, events.UrlInput{URL: u, Kind: events.BinaryBinary})
				}
			}
			if queries, ok := action["queries"].([]any); ok && len(queries) > 0 {
				metadata["queries"] = queries
			}
		case "open_page":
			if u := stringField(action, "url"); u != "" {
				parts = append(parts, events.UrlInput{URL: u, Kind: events.BinaryBinary})
			}
		case "find":
			if u := stringField(action, "url"); u != "" {
				parts = append(parts, events.UrlInput{URL: u, Kind: events.BinaryBinary})
			}
			metadata["pattern"] = action["pattern"]
		}
	case "code_interpreter_call":
		outputs, _ := item["outputs"].([]any)
		for _, o := range outputs {
			output, ok := o.(map[string]any)
			if !ok {
				continue
			}
			switch output["type"] {
			case "logs":
				parts = append(parts, events.TextInput{Content: stringField(output, "logs")})
			case "image":
				if u := stringField(output, "url"); u != "" {
					parts = append(parts, events.UrlInput{URL: u, Kind: events.BinaryImage})
				}
			}
		}
		metadata["container_id"] = item["container_id"]
		metadata["status"] = item["status"]
	case "image_generation_call":
		if stringField(item, "result") == "" {
			return nil
		}
		for k, v := range item {
			switch k {
			case "result", "status", "type":
				continue
			}
			metadata[k] = v
		}
	case "file_search_call":
		metadata["status"] = item["status"]
		results, _ := item["results"].([]any)
		resultsMeta := make([]any, 0, len(results))
		for _, r := range results {
			result, ok := r.(map[string]any)
			if !ok {
				continue
			}
			// text is populated only when the request asked for it via
			// include=["file_search_call.results"].
			if text := stringField(result, "text"); text != "" {
				parts = append(parts, events.TextInput{Content: text})
			}
			resultsMeta = append(resultsMeta, map[string]any{
				"file_id":  result["file_id"],
				"filename": result["filename"],
				"score":    result["score"],
			})
		}
		if len(resultsMeta) > 0 {
			metadata["results"] = resultsMeta
		}
	case "mcp_call":
		metadata["server_label"] = item["server_label"]
		metadata["tool"] = item["name"]
		metadata["status"] = item["status"]
		if out := stringField(item, "output"); out != "" {
			parts = append(parts, events.TextInput{Content: out})
		}
		if item["error"] != nil {
			metadata["error"] = item["error"]
		}
	case "mcp_list_tools":
		metadata["server_label"] = item["server_label"]
		toolsList, _ := item["tools"].([]any)
		names := make([]any, 0, len(toolsList))
		for _, tl := range toolsList {
			if t, ok := tl.(map[string]any); ok {
				names = append(names, t["name"])
			}
		}
		metadata["tools"] = names
		if item["error"] != nil {
			// Shaped like mcp_call's error, so one metadata["error"]["type"]
			// tells a caller what failed whichever item carried it.
			metadata["error"] = map[string]any{"type": "mcp_list_tools_error", "message": item["error"]}
		}
	default:
		return nil
	}

	return &events.BuiltinToolResultEvent{ToolResultEvent: events.ToolResultEvent{
		ParentID: parentID,
		Name:     name,
		Result:   events.ToolResult{Parts: parts, Metadata: metadata},
	}}
}

// reasoningSummaries extracts the summary texts of a reasoning item.
func reasoningSummaries(item map[string]any) []string {
	summary, _ := item["summary"].([]any)
	texts := make([]string, 0, len(summary))
	for _, s := range summary {
		if entry, ok := s.(map[string]any); ok {
			if text := stringField(entry, "text"); text != "" {
				texts = append(texts, text)
			}
		}
	}
	return texts
}

// stringField reads a string field tolerating a missing or non-string value.
func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// --- input mapping -----------------------------------------------------------------

// responsesInput converts the event history into Responses API input items
// (Python parity: events_to_responses_input). Turns other than the ones mapped
// below are skipped, Python parity.
func responsesInput(messages []events.Event) ([]any, error) {
	answered := answeredShellCalls(messages)
	out := make([]any, 0, len(messages))
	for _, ev := range messages {
		switch e := events.AsValue(ev).(type) {
		case events.ModelResponse:
			if e.Message != nil {
				out = append(out, map[string]any{"role": "assistant", "content": e.Message.Content})
			}
			for _, call := range e.ToolCalls.Calls {
				out = append(out, map[string]any{
					"type":      "function_call",
					"call_id":   call.ID,
					"name":      call.Name,
					"arguments": call.Arguments,
				})
			}
		case events.ToolResultsEvent:
			for _, r := range e.Results {
				blocks := make([]any, 0, len(r.Result.Parts))
				for _, part := range r.Result.Parts {
					b, err := toolOutputBlock(part)
					if err != nil {
						return nil, err
					}
					blocks = append(blocks, b)
				}
				out = append(out, functionCallOutput(r.ParentID, blocks))
			}
		case events.BuiltinToolCallEvent:
			item, _ := e.VendorMetadata["item"].(map[string]any)
			if item == nil {
				continue // a hosted call without its item: nothing to replay
			}
			// A shell_call is the one hosted item the API will not accept
			// alone: its outcome lives in a separate shell_call_output, so a
			// turn that ended between the two would 400 the next request.
			if item["type"] == "shell_call" {
				if _, ok := answered[stringField(item, "call_id")]; !ok {
					continue
				}
			}
			out = append(out, replayedItem(item))
		case events.BuiltinToolResultEvent:
			item, _ := e.Result.Metadata["item"].(map[string]any)
			if item == nil {
				continue
			}
			out = append(out, replayedItem(item))
		case events.ModelRequest:
			if err := appendUserParts(&out, e.Parts); err != nil {
				return nil, err
			}
		case events.DrainedModelRequest:
			if err := appendUserParts(&out, e.Parts); err != nil {
				return nil, err
			}
		case events.CompactionSummary:
			// A user turn keeps the summary visible and gives a valid opening turn.
			out = append(out, map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "[Summary of earlier conversation]\n" + e.Content},
			}})
		}
	}
	return out, nil
}

// appendUserParts maps one ModelRequest's parts, each as its own user message
// carrying a single content part (Python parity: _user_message).
func appendUserParts(out *[]any, parts []events.Input) error {
	for _, part := range parts {
		b, err := userMessagePart(part)
		if err != nil {
			return err
		}
		*out = append(*out, map[string]any{"role": "user", "content": []any{b}})
	}
	return nil
}

// answeredShellCalls collects the call_ids of hosted shell calls whose output
// is present in the history.
func answeredShellCalls(messages []events.Event) map[string]bool {
	answered := map[string]bool{}
	for _, ev := range messages {
		e, ok := events.AsValue(ev).(events.BuiltinToolResultEvent)
		if !ok {
			continue
		}
		item, _ := e.Result.Metadata["item"].(map[string]any)
		if item != nil && item["type"] == "shell_call_output" {
			answered[stringField(item, "call_id")] = true
		}
	}
	return answered
}

// functionCallOutput renders one tool result. A single plain-text block
// collapses to its string (Python parity: the most common wire form).
func functionCallOutput(callID string, blocks []any) map[string]any {
	var output any = blocks
	if len(blocks) == 1 {
		if b, ok := blocks[0].(map[string]any); ok && b["type"] == "input_text" {
			output = b["text"]
		}
	}
	return map[string]any{"type": "function_call_output", "call_id": callID, "output": output}
}

// replayedItem returns a wire-safe copy of a hosted item for replay: fields
// the API puts on an output item but rejects on the way back in are dropped
// (Python parity: the _OUTPUT_ONLY_FIELDS guard).
func replayedItem(item map[string]any) map[string]any {
	out := make(map[string]any, len(item))
	for k, v := range item {
		out[k] = v
	}
	delete(out, "created_by")
	return out
}

// userMessagePart maps one ModelRequest input part to one wire content part.
func userMessagePart(part events.Input) (map[string]any, error) {
	switch p := events.AsValue(part).(type) {
	case events.TextInput:
		return map[string]any{"type": "input_text", "text": p.Content}, nil
	case events.DataInput:
		b, err := json.Marshal(p.Data)
		if err != nil {
			return nil, fmt.Errorf("config/openai: encoding DataInput: %w", err)
		}
		return map[string]any{"type": "input_text", "text": string(b)}, nil
	case events.FileIdInput:
		// The Responses API takes file_id and filename as mutually exclusive;
		// a file_id reference carries no filename.
		return map[string]any{"type": "input_file", "file_id": p.FileID}, nil
	case events.BinaryInput:
		switch p.Kind {
		case events.BinaryImage:
			block := map[string]any{"type": "input_image", "image_url": dataURL(p.MediaType, p.Data)}
			detail, err := imageDetail(p, responsesImageDetails)
			if err != nil {
				return nil, err
			}
			if detail != "" {
				block["detail"] = detail
			}
			return block, nil
		case events.BinaryDocument, events.BinaryBinary:
			// input_file with file_data requires a filename.
			return map[string]any{
				"type":      "input_file",
				"file_data": dataURL(p.MediaType, p.Data),
				"filename":  binaryFilename(p.VendorMetadata, p.MediaType),
			}, nil
		default:
			return nil, fmt.Errorf("%w: BinaryInput(%s) for the responses API", config.ErrUnsupportedInput, p.Kind)
		}
	case events.UrlInput:
		switch p.Kind {
		case events.BinaryImage:
			return map[string]any{"type": "input_image", "image_url": p.URL}, nil
		case events.BinaryDocument, events.BinaryBinary:
			// file_url forbids a filename (API mutual-exclusion).
			return map[string]any{"type": "input_file", "file_url": p.URL}, nil
		default:
			return nil, fmt.Errorf("%w: UrlInput(%s) for the responses API", config.ErrUnsupportedInput, p.Kind)
		}
	default:
		return nil, fmt.Errorf("%w: %T for the responses API", config.ErrUnsupportedInput, part)
	}
}

// toolOutputBlock maps one tool-result part to one function_call_output block.
// A file_id reference forbids a filename here (the user-message form allows
// both), and output images must use input_image (input_file rejects image/*
// MIME).
func toolOutputBlock(part events.Input) (map[string]any, error) {
	switch p := events.AsValue(part).(type) {
	case events.TextInput:
		return map[string]any{"type": "input_text", "text": p.Content}, nil
	case events.DataInput:
		b, err := json.Marshal(p.Data)
		if err != nil {
			return nil, fmt.Errorf("config/openai: encoding DataInput tool result: %w", err)
		}
		return map[string]any{"type": "input_text", "text": string(b)}, nil
	case events.FileIdInput:
		return map[string]any{"type": "input_file", "file_id": p.FileID}, nil
	case events.BinaryInput:
		switch p.Kind {
		case events.BinaryImage:
			return map[string]any{"type": "input_image", "image_url": dataURL(p.MediaType, p.Data)}, nil
		case events.BinaryDocument, events.BinaryBinary:
			return map[string]any{
				"type":      "input_file",
				"file_data": dataURL(p.MediaType, p.Data),
				"filename":  binaryFilename(p.VendorMetadata, p.MediaType),
			}, nil
		default:
			return nil, fmt.Errorf("%w: BinaryInput(%s) in a tool result for the responses API", config.ErrUnsupportedInput, p.Kind)
		}
	case events.UrlInput:
		switch p.Kind {
		case events.BinaryImage:
			return map[string]any{"type": "input_image", "image_url": p.URL}, nil
		case events.BinaryDocument, events.BinaryBinary:
			return map[string]any{"type": "input_file", "file_url": p.URL}, nil
		default:
			return nil, fmt.Errorf("%w: UrlInput(%s) in a tool result for the responses API", config.ErrUnsupportedInput, p.Kind)
		}
	default:
		return nil, fmt.Errorf("%w: %T in a tool result for the responses API", config.ErrUnsupportedInput, part)
	}
}

// responsesImageDetails are the image detail levels the Responses API knows
// (it has the "original" level the Chat-Completions API lacks).
var responsesImageDetails = map[string]bool{"auto": true, "low": true, "high": true, "original": true}

// imageDetail reads the detail a caller set in VendorMetadata, refusing a
// value the API does not know.
func imageDetail(p events.BinaryInput, known map[string]bool) (string, error) {
	v, ok := p.VendorMetadata["detail"]
	if !ok {
		return "", nil
	}
	detail, ok := v.(string)
	if !ok || !known[detail] {
		return "", fmt.Errorf("%w: BinaryInput(image, detail=%v) for the responses API", config.ErrUnsupportedInput, v)
	}
	return detail, nil
}

// binaryFilename is the name inline file data travels under; the API requires
// one (VendorMetadata["filename"] wins, then the media type's subtype).
func binaryFilename(vendor map[string]any, mediaType string) string {
	if s, ok := vendor["filename"].(string); ok && s != "" {
		return s
	}
	sub := mediaType
	if i := strings.LastIndex(sub, "/"); i >= 0 {
		sub = sub[i+1:]
	}
	if i := strings.Index(sub, "+"); i >= 0 {
		sub = sub[:i]
	}
	return "file." + sub
}

// --- tool schemas ----------------------------------------------------------------

// toolToResponsesAPI renders one tool schema as a Responses API tools[] entry
// (Python parity: tool_to_responses_api): function schemas carry name and
// description at top level, hosted schemas map by their wire type. Everything
// else fails fast with config.ErrUnsupportedTool — never silently dropped.
func toolToResponsesAPI(tool any) (map[string]any, error) {
	switch t := tool.(type) {
	case tools.Schema:
		return functionToolToResponses(t)
	case *tools.Schema:
		if t == nil {
			return nil, fmt.Errorf("%w: nil function schema", config.ErrUnsupportedTool)
		}
		return functionToolToResponses(*t)
	case tools.ToolSchema:
		return hostedToolToResponses(t)
	default:
		return nil, fmt.Errorf("%w: unsupported schema type %T", config.ErrUnsupportedTool, tool)
	}
}

func functionToolToResponses(t tools.Schema) (map[string]any, error) {
	// Built by hand rather than a typed struct: the API accepts the tool
	// without a strict key, and ag2 omits it so the API applies its default.
	fn := map[string]any{
		"type":       "function",
		"name":       t.Name,
		"parameters": config.EnsureObjectSchema(t.Parameters),
	}
	if t.Description != "" {
		fn["description"] = t.Description
	}
	if t.DeferLoading {
		// Deferred loading is a Responses-API feature; this is the client
		// that carries it.
		fn["defer_loading"] = true
	}
	return fn, nil
}

// hostedToolToResponses dispatches one hosted schema (value and pointer forms;
// the carriers return values).
func hostedToolToResponses(schema tools.ToolSchema) (map[string]any, error) {
	switch t := schema.(type) {
	case tools.WebSearchToolSchema:
		return webSearchToResponses(t)
	case *tools.WebSearchToolSchema:
		if t == nil {
			break
		}
		return webSearchToResponses(*t)
	case tools.FileSearchToolSchema:
		return fileSearchToResponses(t)
	case *tools.FileSearchToolSchema:
		if t == nil {
			break
		}
		return fileSearchToResponses(*t)
	case tools.CodeExecutionToolSchema:
		return map[string]any{"type": "code_interpreter", "container": map[string]any{"type": "auto"}}, nil
	case *tools.CodeExecutionToolSchema:
		if t == nil {
			break
		}
		return map[string]any{"type": "code_interpreter", "container": map[string]any{"type": "auto"}}, nil
	case tools.ShellToolSchema:
		return shellToResponses(t)
	case *tools.ShellToolSchema:
		if t == nil {
			break
		}
		return shellToResponses(*t)
	case tools.ImageGenerationToolSchema:
		return imageGenerationToResponses(t)
	case *tools.ImageGenerationToolSchema:
		if t == nil {
			break
		}
		return imageGenerationToResponses(*t)
	case tools.MCPServerToolSchema:
		return mcpServerToResponses(t)
	case *tools.MCPServerToolSchema:
		if t == nil {
			break
		}
		return mcpServerToResponses(*t)
	case tools.ToolSearchToolSchema:
		// OpenAI exposes a single server-side tool-search tool; the mode field
		// is Anthropic-only.
		return map[string]any{"type": "tool_search"}, nil
	case *tools.ToolSearchToolSchema:
		if t == nil {
			break
		}
		return map[string]any{"type": "tool_search"}, nil
	}
	return nil, fmt.Errorf("%w: hosted tool %q for the responses API", config.ErrUnsupportedTool, schema.SchemaType())
}

func webSearchToResponses(t tools.WebSearchToolSchema) (map[string]any, error) {
	ws := map[string]any{"type": "web_search"}
	if t.SearchContextSize != "" {
		ws["search_context_size"] = t.SearchContextSize
	}
	if t.UserLocation != nil {
		// Only the fields that were set travel (Python parity:
		// _user_location_to_api).
		loc := map[string]any{"type": "approximate"}
		if t.UserLocation.City != nil {
			loc["city"] = *t.UserLocation.City
		}
		if t.UserLocation.Region != nil {
			loc["region"] = *t.UserLocation.Region
		}
		if t.UserLocation.Country != nil {
			loc["country"] = *t.UserLocation.Country
		}
		if t.UserLocation.Timezone != nil {
			loc["timezone"] = *t.UserLocation.Timezone
		}
		ws["user_location"] = loc
	}
	if t.AllowedDomains != nil {
		ws["filters"] = map[string]any{"allowed_domains": t.AllowedDomains}
	}
	if t.MaxUses != nil {
		// max_uses is Anthropic's cap; the OpenAI API documents no such field,
		// but ag2 has always sent it, so this port keeps sending it.
		ws["max_uses"] = *t.MaxUses
	}
	// BlockedDomains is an Anthropic-side restriction; the Responses API has
	// no wire key for it (Python parity: the mapper drops it).
	return ws, nil
}

func fileSearchToResponses(t tools.FileSearchToolSchema) (map[string]any, error) {
	fs := map[string]any{"type": "file_search", "vector_store_ids": t.VectorStoreIDs}
	if t.MaxNumResults != nil {
		fs["max_num_results"] = *t.MaxNumResults
	}
	if t.Filters != nil {
		// A comparison or compound filter, authored as JSON by the caller.
		fs["filters"] = t.Filters
	}
	return fs, nil
}

func shellToResponses(t tools.ShellToolSchema) (map[string]any, error) {
	shell := map[string]any{"type": "shell"}
	switch {
	case t.Environment == nil || (t.Environment.Auto == nil && t.Environment.Reference == nil):
		// No hosted environment means the API would hand the shell to the
		// client to execute, which this port does not answer (Python parity:
		// reject_client_executed_shell).
		return nil, fmt.Errorf("%w: hosted shell without a hosted environment "+
			"(set Environment.Auto or Environment.Reference; local execution is not ported)", config.ErrUnsupportedTool)
	case t.Environment.Auto != nil:
		env := map[string]any{"type": "container_auto"}
		if t.Environment.Auto.NetworkPolicy != nil {
			env["network_policy"] = map[string]any{
				"type":            "allowlist",
				"allowed_domains": t.Environment.Auto.NetworkPolicy.AllowedDomains,
			}
		}
		shell["environment"] = env
	default:
		shell["environment"] = map[string]any{
			"type":         "container_reference",
			"container_id": t.Environment.Reference.ContainerID,
		}
	}
	return shell, nil
}

func imageGenerationToResponses(t tools.ImageGenerationToolSchema) (map[string]any, error) {
	ig := map[string]any{"type": "image_generation"}
	if t.Quality != "" {
		ig["quality"] = t.Quality
	}
	if t.Size != "" {
		ig["size"] = t.Size
	}
	if t.Background != "" {
		ig["background"] = t.Background
	}
	if t.OutputFormat != "" {
		ig["output_format"] = t.OutputFormat
	}
	if t.OutputCompression != nil {
		ig["output_compression"] = *t.OutputCompression
	}
	if t.PartialImages != nil {
		ig["partial_images"] = *t.PartialImages
	}
	return ig, nil
}

func mcpServerToResponses(t tools.MCPServerToolSchema) (map[string]any, error) {
	if len(t.BlockedTools) > 0 {
		// The Responses API has no blocked_tools key (Python parity:
		// BlockedToolsUnsupportedError).
		return nil, fmt.Errorf("%w: blocked_tools on mcp_server %q for the responses API", config.ErrUnsupportedTool, t.ServerLabel)
	}
	mcp := map[string]any{
		"type":             "mcp",
		"server_label":     t.ServerLabel,
		"server_url":       t.ServerURL,
		"require_approval": "never",
	}
	if t.Description != "" {
		mcp["server_description"] = t.Description
	}
	if t.AllowedTools != nil {
		mcp["allowed_tools"] = t.AllowedTools
	}
	headers := map[string]string{}
	for k, v := range t.Headers {
		headers[k] = v
	}
	// authorization_token becomes a bearer Authorization header; an entry
	// already in Headers (matched case-insensitively) outranks it, so a
	// request never carries two credentials.
	if t.AuthorizationToken != "" {
		if _, has := lookupCI(headers, "Authorization"); !has {
			headers["Authorization"] = "Bearer " + t.AuthorizationToken
		}
	}
	if len(headers) > 0 {
		mcp["headers"] = headers
	}
	return mcp, nil
}

// lookupCI finds a map key case-insensitively.
func lookupCI(m map[string]string, key string) (string, bool) {
	for k, v := range m {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}

// responsesIncludes lists the include[] entries the tool schemas ask for:
// web_search sources (the result parts need them) and file_search results (off
// by default: full-text chunks inflate responses).
func responsesIncludes(schemas []any) []string {
	var inc []string
	for _, s := range schemas {
		switch t := s.(type) {
		case tools.WebSearchToolSchema:
			inc = append(inc, "web_search_call.action.sources")
		case *tools.WebSearchToolSchema:
			if t != nil {
				inc = append(inc, "web_search_call.action.sources")
			}
		case tools.FileSearchToolSchema:
			if t.IncludeResults {
				inc = append(inc, "file_search_call.results")
			}
		case *tools.FileSearchToolSchema:
			if t != nil && t.IncludeResults {
				inc = append(inc, "file_search_call.results")
			}
		}
	}
	return inc
}

// --- text format -----------------------------------------------------------------

// responsesTextConfig converts a response schema into the Responses API text
// config: {"format": {json_schema, strict}} carrying the schema description
// when present (Python parity: response_proto_to_text_config). A nil result
// means the schema has no native JSON schema — output is driven by its system
// prompt instead.
func responsesTextConfig(schema response.Proto) map[string]any {
	if schema == nil {
		return nil
	}
	js := schema.JSONSchema()
	if len(js) == 0 {
		return nil
	}
	format := map[string]any{
		"type":   "json_schema",
		"name":   schema.Name(),
		"schema": StrictifySchema(js),
		"strict": true,
	}
	if d := schema.Description(); d != "" {
		format["description"] = d
	}
	return map[string]any{"format": format}
}

// --- usage -----------------------------------------------------------------------

// normalizeResponsesUsage maps the Responses usage object onto events.Usage:
// input/output/total tokens, input_tokens_details.cached_tokens →
// CacheReadInputTokens, cache_write_tokens → CacheCreationInputTokens,
// output_tokens_details.reasoning_tokens → ThinkingTokens (Python parity:
// normalize_responses_usage).
func normalizeResponsesUsage(usage map[string]any) events.Usage {
	var u events.Usage
	u.PromptTokens = tokenField(usage, "input_tokens")
	u.CompletionTokens = tokenField(usage, "output_tokens")
	u.TotalTokens = tokenField(usage, "total_tokens")
	if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		u.CacheReadInputTokens = tokenField(details, "cached_tokens")
		u.CacheCreationInputTokens = tokenField(details, "cache_write_tokens")
	}
	if details, ok := usage["output_tokens_details"].(map[string]any); ok {
		u.ThinkingTokens = tokenField(details, "reasoning_tokens")
	}
	return u
}

// tokenField reads one numeric field tolerating the JSON decoder's float64,
// whole-number Go ints and json.Number (decoders using UseNumber). Fractional
// values are rounded — token counts are integers (D-006).
func tokenField(m map[string]any, key string) *int64 {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	switch n := v.(type) {
	case float64:
		i := int64(n + 0.5)
		return &i
	case int64:
		return &n
	case int:
		i := int64(n)
		return &i
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return &i
		}
		if f, err := n.Float64(); err == nil {
			i := int64(f + 0.5)
			return &i
		}
	}
	return nil
}

// --- small shared helpers ---------------------------------------------------------

// joinURL joins the endpoint root and a path under it.
func joinURL(base, path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return strings.TrimSuffix(base, "/") + path
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

// dataURL renders inline bytes as a data: URL.
func dataURL(mediaType string, data []byte) string {
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// sendEvent publishes a progress event on the conversation stream; a nil conv
// (headless call) drops it.
func sendEvent(conv *agocontext.Context, ev events.Event) error {
	if conv == nil {
		return nil
	}
	return conv.Send(ev)
}
