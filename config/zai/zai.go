// Package zai wires Z.AI (Zhipu) GLM models into the AGo config contract.
// Z.AI exposes an OpenAI Chat-Completions dialect, so its client is a thin
// binding of ZAIConfig onto the shared engine config.RunChatCompat.
//
// Parity with Python AG2 ag2/config/zai/config.py. The request surface carries
// the thinking toggle, reasoning_effort, the token-cache opt-out, the vendor
// extras (sensitive_word_check, watermark_enabled, tool_stream, extra), the
// sampled fields and the request knobs (stop, seed, tool_choice, user_id,
// request_id, do_sample, meta, extra_headers, extra_body) plus the retrieval
// tool. The Files API stays out of scope (D-021).
// See docs/specs/config-llm.md §1.
package zai

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/tools"
)

// Defaults (Python parity: the Z.AI SDK endpoint and max_retries=3).
const (
	// DefaultBaseURL is the Z.AI OpenAI-compatible API root used when
	// ZAIConfig.BaseURL is empty.
	DefaultBaseURL = "https://api.z.ai/api/paas/v4"
	// EnvAPIKey names the environment variable consulted when ZAIConfig.APIKey
	// is empty (Python parity: the SDK reads it at client construction).
	EnvAPIKey = "ZAI_API_KEY"
	// DefaultMaxRetries is the transport retry count (Python parity).
	DefaultMaxRetries = 3
	// chatPath is the Chat-Completions endpoint under BaseURL.
	chatPath = "/chat/completions"
	// DefaultSourceChannel is the x-source-channel header value the SDK always
	// sends (Python parity: the zai SDK's "python-sdk").
	//
	// AGO-DEVIATION: AGo identifies itself as "ago" instead of claiming to be
	// the Python SDK; Z.AI treats the header as telemetry only.
	DefaultSourceChannel = "ago"
)

// ZAIConfig configures one Z.AI model (Python parity: the ZAIConfig dataclass).
// Construct with NewZAI — the only way to set the model — and assign the
// exported fields. A config is immutable once built: Create and Clone never
// mutate the receiver, so one value is safe to share across goroutines.
//
// AGO-DEVIATION: the Python dataclass carries the model as the public field
// `model`; AGo holds it privately behind the Model() accessor because the
// config.ModelConfig contract requires a Model() method and Go forbids a
// field and a method sharing one name.
type ZAIConfig struct {
	// APIKey authenticates requests. Empty falls back to EnvAPIKey at Create()
	// time; neither set → Create() wraps config.ErrNotProvided.
	APIKey string
	// BaseURL overrides the endpoint root. Empty → DefaultBaseURL.
	BaseURL string

	// MaxRetries counts transport retries per call; 0 = single attempt (Go zero
	// value — NewZAI applies the Python default of 3).
	MaxRetries int

	// Timeout caps a non-streaming request (0 = no cap; streams rely on the
	// call's context for cancellation). Python parity: timeout /
	// request_timeout.
	Timeout time.Duration
	// HTTPClient performs the requests when non-nil (proxy, custom root CAs,
	// mTLS). It owns its own Timeout and redirect policy; Timeout above
	// applies only to the default client. Python parity: http_client.
	HTTPClient *http.Client

	Temperature *float64 // nil = unset, not sent
	TopP        *float64 // nil = unset, not sent
	MaxTokens   *int64   // nil = unset, not sent

	// The request knobs below are not sent when unset (Python parity: the
	// ZAIConfig fields of the same names).
	// Stop sequences. AGO-DEVIATION: Python accepts `str | list[str]`; AGo takes
	// a list, and a single sequence is a one-element slice (the wire value is
	// always a JSON array, which every provider accepts).
	Stop []string
	Seed *int64
	// ToolChoice is a mode string ("auto") or a map selecting one tool.
	ToolChoice any
	// UserID is the end-user id; RequestID a caller-chosen request id.
	UserID    string
	RequestID string
	// DoSample toggles sampling (false = greedy decoding).
	DoSample *bool
	// Meta is the request metadata dict.
	Meta map[string]string
	// ExtraHeaders are sent on top of the auth header.
	ExtraHeaders map[string]string
	// ExtraBody merges into the request body. As in Python, the typed fields
	// above win over a same-named ExtraBody key.
	ExtraBody map[string]any

	// DisableTokenCache selects how the bearer token is built. nil keeps the
	// Python default (true): the API key travels as-is. An explicit false mints
	// a short-lived HS256 token from the key's "<id>.<secret>" form (Python
	// parity: the zai SDK's disable_token_cache=False).
	DisableTokenCache *bool
	// SourceChannel is the x-source-channel header value. Empty sends
	// DefaultSourceChannel; an ExtraHeaders entry of that name wins over both.
	SourceChannel string
	// Thinking toggles GLM reasoning; nil leaves the provider default
	// (Python parity: thinking={"type": "enabled"|"disabled"}).
	Thinking *bool
	// ReasoningEffort is the effort level sent as reasoning_effort ("none",
	// "minimal", "low", "medium", "high", "xhigh", "max"; models from glm-5.2).
	ReasoningEffort string
	// SensitiveWordCheck carries the sensitive-word filter request object
	// (Python parity: sensitive_word_check, an untyped SDK model).
	SensitiveWordCheck any
	// WatermarkEnabled toggles a watermark on generated audio; an explicit
	// false is sent (nil omits the field).
	WatermarkEnabled *bool
	// ToolStream streams tool-call deltas.
	ToolStream *bool
	// Extra carries the vendor extra block (Python parity: extra, CodeGeex
	// parameters).
	Extra any

	Streaming bool

	model string
}

// NewZAI returns a config for model with the Python defaults: the Z.AI base
// URL and 3 transport retries.
func NewZAI(model string) *ZAIConfig {
	return &ZAIConfig{
		model:      model,
		BaseURL:    DefaultBaseURL,
		MaxRetries: DefaultMaxRetries,
	}
}

// Provider reports the vendor (ModelConfig contract).
func (c *ZAIConfig) Provider() config.Provider { return config.ProviderZAI }

// Model returns the model identifier sent to the API (ModelConfig contract).
func (c *ZAIConfig) Model() string { return c.model }

// Create implements config.ModelConfig: it validates the required values and
// binds them into a fresh client (Python parity: fresh client per create()).
// The API key falls back to EnvAPIKey when the field is empty; a missing model
// or key is an error wrapping config.ErrNotProvided.
func (c *ZAIConfig) Create() (config.LLMClient, error) {
	params, err := c.params()
	if err != nil {
		return nil, err
	}
	cl := &chatClient{params: params}
	if c.DisableTokenCache != nil && !*c.DisableTokenCache {
		// The token is minted per request; validate the key form now so a
		// malformed key fails here instead of mid-turn.
		if _, _, err := splitAPIKey(params.APIKey); err != nil {
			return nil, err
		}
		cl.tokenAuth = true
	}
	return cl, nil
}

// params renders the shared-engine parameters for this config.
func (c *ZAIConfig) params() (config.CompatParams, error) {
	if c.model == "" {
		return config.CompatParams{}, fmt.Errorf("%w: zai: model", config.ErrNotProvided)
	}
	key := c.APIKey
	if key == "" {
		key = os.Getenv(EnvAPIKey)
	}
	if key == "" {
		return config.CompatParams{}, fmt.Errorf("%w: zai: api key (set %s)", config.ErrNotProvided, EnvAPIKey)
	}
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	// The SDK always sends x-source-channel; callers may override it, and an
	// ExtraHeaders entry of that name wins (net/http canonicalizes header names,
	// so the comparison is case-insensitive).
	headers := make(map[string]string, len(c.ExtraHeaders)+1)
	channelSet := false
	for k, v := range c.ExtraHeaders {
		headers[k] = v
		if strings.EqualFold(k, "x-source-channel") {
			channelSet = true
		}
	}
	if !channelSet {
		channel := c.SourceChannel
		if channel == "" {
			channel = DefaultSourceChannel
		}
		headers["x-source-channel"] = channel
	}

	return config.CompatParams{
		APIKey:       key,
		BaseURL:      base,
		Path:         chatPath,
		Model:        c.model,
		Provider:     config.ProviderZAI,
		Temperature:  c.Temperature,
		TopP:         c.TopP,
		MaxTokens:    c.MaxTokens,
		Streaming:    c.Streaming,
		MaxRetries:   c.MaxRetries,
		Timeout:      c.Timeout,
		HTTPClient:   c.HTTPClient,
		ExtraHeaders: headers,
		Extra:        c.extraBody(),
		MapTool:      mapTool,
	}, nil
}

// extraBody builds the body fields the engine has no Z.AI-specific slot for.
// Python parity: the SDK call starts from extra_body and the typed options
// overwrite it, so a typed field wins over a same-named ExtraBody key. The
// sampled fields the engine carries typed (temperature, top_p, max_tokens)
// are repeated here for the same reason: the engine merges Extra last.
func (c *ZAIConfig) extraBody() map[string]any {
	extra := make(map[string]any, len(c.ExtraBody)+10)
	for k, v := range c.ExtraBody {
		extra[k] = v
	}
	if c.Temperature != nil {
		extra["temperature"] = *c.Temperature
	}
	if c.TopP != nil {
		extra["top_p"] = *c.TopP
	}
	if c.MaxTokens != nil {
		extra["max_tokens"] = *c.MaxTokens
	}
	if len(c.Stop) > 0 {
		extra["stop"] = c.Stop
	}
	if c.Seed != nil {
		extra["seed"] = *c.Seed
	}
	if c.ToolChoice != nil {
		extra["tool_choice"] = c.ToolChoice
	}
	if c.UserID != "" {
		extra["user_id"] = c.UserID
	}
	if c.RequestID != "" {
		extra["request_id"] = c.RequestID
	}
	if c.DoSample != nil {
		extra["do_sample"] = *c.DoSample
	}
	if c.Meta != nil {
		extra["meta"] = c.Meta
	}
	if c.Thinking != nil {
		// Python parity: the wire object, not a bare bool.
		kind := "disabled"
		if *c.Thinking {
			kind = "enabled"
		}
		extra["thinking"] = map[string]any{"type": kind}
	}
	if c.ReasoningEffort != "" {
		extra["reasoning_effort"] = c.ReasoningEffort
	}
	if c.SensitiveWordCheck != nil {
		extra["sensitive_word_check"] = c.SensitiveWordCheck
	}
	if c.WatermarkEnabled != nil {
		extra["watermark_enabled"] = *c.WatermarkEnabled
	}
	if c.ToolStream != nil {
		extra["tool_stream"] = *c.ToolStream
	}
	if c.Extra != nil {
		extra["extra"] = c.Extra
	}
	if len(extra) == 0 {
		return nil
	}
	return extra
}

// mapTool maps the hosted schemas Z.AI executes server-side. Only retrieval is
// carried so far (Python parity: zai/mappers.py tool_to_api); every other
// hosted schema falls through to the engine's fail-fast rejection.
func mapTool(tool any) (map[string]any, bool, error) {
	var schema tools.RetrievalToolSchema
	switch t := tool.(type) {
	case tools.RetrievalToolSchema:
		schema = t
	case *tools.RetrievalToolSchema:
		if t == nil {
			return nil, false, fmt.Errorf("%w: nil retrieval schema", config.ErrUnsupportedTool)
		}
		schema = *t
	default:
		return nil, false, nil
	}
	retrieval := map[string]any{"knowledge_id": schema.KnowledgeID}
	if schema.PromptTemplate != "" {
		retrieval["prompt_template"] = schema.PromptTemplate
	}
	return map[string]any{"type": "retrieval", "retrieval": retrieval}, true, nil
}

// Clone implements config.ModelConfig (Python parity: dataclasses.replace): a
// copy with overrides applied, receiver untouched. Keys are Go field
// names, matched case-insensitively; an unknown key or a wrongly typed value
// is an error. A pointer field
// also accepts its value type (wrapped) and nil (cleared).
func (c *ZAIConfig) Clone(overrides map[string]any) (config.ModelConfig, error) {
	out := *c
	for k, v := range overrides {
		var err error
		switch strings.ToLower(k) {
		case "model":
			err = setString(&out.model, k, v)
		case "apikey":
			err = setString(&out.APIKey, k, v)
		case "baseurl":
			err = setString(&out.BaseURL, k, v)
		case "maxretries":
			err = setInt(&out.MaxRetries, k, v)
		case "temperature":
			err = setF64(&out.Temperature, k, v)
		case "topp":
			err = setF64(&out.TopP, k, v)
		case "maxtokens":
			err = setI64(&out.MaxTokens, k, v)
		case "streaming":
			err = setBool(&out.Streaming, k, v)
		case "timeout":
			err = setDuration(&out.Timeout, k, v)
		case "httpclient", "http_client":
			err = setHTTPClient(&out.HTTPClient, k, v)
		case "stop":
			err = setStrings(&out.Stop, k, v)
		case "seed":
			err = setI64(&out.Seed, k, v)
		case "toolchoice", "tool_choice":
			out.ToolChoice = v
		case "userid", "user_id":
			err = setString(&out.UserID, k, v)
		case "requestid", "request_id":
			err = setString(&out.RequestID, k, v)
		case "dosample", "do_sample":
			err = setBoolPtr(&out.DoSample, k, v)
		case "meta":
			err = setStringMap(&out.Meta, k, v)
		case "extraheaders", "extra_headers":
			err = setStringMap(&out.ExtraHeaders, k, v)
		case "extrabody", "extra_body":
			err = setAnyMap(&out.ExtraBody, k, v)
		case "disabletokencache", "disable_token_cache":
			err = setBoolPtr(&out.DisableTokenCache, k, v)
		case "sourcechannel", "source_channel":
			err = setString(&out.SourceChannel, k, v)
		case "thinking":
			err = setBoolPtr(&out.Thinking, k, v)
		case "reasoningeffort", "reasoning_effort":
			err = setString(&out.ReasoningEffort, k, v)
		case "sensitivewordcheck", "sensitive_word_check":
			out.SensitiveWordCheck = v
		case "watermarkenabled", "watermark_enabled":
			err = setBoolPtr(&out.WatermarkEnabled, k, v)
		case "toolstream", "tool_stream":
			err = setBoolPtr(&out.ToolStream, k, v)
		case "extra":
			out.Extra = v
		default:
			err = fmt.Errorf("config/zai: unknown config field %q", k)
		}
		if err != nil {
			return nil, err
		}
	}
	return &out, nil
}
