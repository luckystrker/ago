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
	"net/http"
	"strconv"
	"strings"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/internal/httpx"
	"github.com/luckystrker/ago/response"
	"github.com/luckystrker/ago/tools"
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
	// HTTPClient performs the requests when non-nil (proxies, custom root
	// CAs, mTLS, test transports). The client owns its own Timeout and
	// redirect policy; the Timeout field above applies only to the default
	// client. Nil keeps the engine's default client.
	HTTPClient *http.Client

	// Query is appended to every request URL (SDK parity: default_query).
	Query map[string]string

	// The fields below are the typed Chat Completions options (SDK parity:
	// the create() keyword arguments). The zero value of each means "not
	// sent"; Extra still overrides all of them.
	// ToolChoice is a mode string ("auto", "none", "required") or a map
	// ({"type":"function","function":{"name":...}}).
	ToolChoice any
	// ReasoningEffort travels as reasoning_effort.
	ReasoningEffort string
	// Seed travels as seed; providers whose wire name differs (Mistral
	// random_seed) set Extra instead.
	Seed *int64
	// MaxCompletionTokens travels as max_completion_tokens.
	MaxCompletionTokens               *int64
	FrequencyPenalty, PresencePenalty *float64
	// User travels as user.
	User string
	// N is the number of completions to request; the engine reads the first
	// choice only (SDK parity: ag2 reads choices[0]).
	N *int64
	// Logprobs and TopLogprobs travel as logprobs / top_logprobs.
	Logprobs    *bool
	TopLogprobs *int64
	// LogitBias maps token ids (as strings) to a bias in [-100, 100].
	LogitBias map[string]int64
	// Metadata travels as metadata (string pairs stored with the request).
	Metadata map[string]string
	// Modalities travels as modalities ("text", "audio").
	Modalities []string
	// Prediction travels as prediction (predicted output content).
	Prediction map[string]any
	// PromptCacheKey is the prompt-cache routing hint; PromptCacheOptions the
	// cache tuning object.
	PromptCacheKey     string
	PromptCacheOptions map[string]any
	// SafetyIdentifier travels as safety_identifier.
	SafetyIdentifier string
	// ServiceTier travels as service_tier.
	ServiceTier string
	// Store travels as store.
	Store *bool
	// Verbosity travels as verbosity ("low", "medium", "high").
	Verbosity string
	// WebSearchOptions travels as web_search_options.
	WebSearchOptions map[string]any

	// ResponseFormat sends an explicit response_format (provider config);
	// a per-call ResponseSchema in CallOptions takes precedence over it.
	ResponseFormat map[string]any
	// Extra merges into the request body last (Python parity: extra_body).
	Extra map[string]any

	// MapTool lets a provider map hosted tool schemas the Chat Completions
	// dialect cannot carry (Z.AI retrieval). It is asked first for every tool;
	// handled == false falls through to ToolToAPI.
	MapTool func(tool any) (wire map[string]any, handled bool, err error)
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
	wireMessages, err := BuildMessagesFor(p.provider(), prompt, messages)
	if err != nil {
		return nil, err
	}

	wireTools := make([]map[string]any, 0, len(opts.Tools))
	for _, t := range opts.Tools {
		if p.MapTool != nil {
			wt, handled, err := p.MapTool(t)
			if err != nil {
				return nil, err
			}
			if handled {
				wireTools = append(wireTools, wt)
				continue
			}
		}
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
	p.applyOptions(body)
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

	client := &httpx.Client{HTTP: p.HTTPClient, MaxRetries: p.MaxRetries, Timeout: p.Timeout}
	url := httpx.WithQuery(joinURL(p.BaseURL, p.Path), p.Query)

	if p.Streaming {
		return runChatStream(ctx, client, url, headers, body, conv, p.provider())
	}
	return runChatOnce(ctx, client, url, headers, body, conv, p.provider())
}

// applyOptions writes the typed Chat Completions options onto body; a zero
// value is not sent.
func (p CompatParams) applyOptions(body map[string]any) {
	if p.ToolChoice != nil {
		body["tool_choice"] = p.ToolChoice
	}
	setString := func(key, v string) {
		if v != "" {
			body[key] = v
		}
	}
	setString("reasoning_effort", p.ReasoningEffort)
	setString("user", p.User)
	setString("prompt_cache_key", p.PromptCacheKey)
	setString("safety_identifier", p.SafetyIdentifier)
	setString("service_tier", p.ServiceTier)
	setString("verbosity", p.Verbosity)
	if p.Seed != nil {
		body["seed"] = *p.Seed
	}
	if p.MaxCompletionTokens != nil {
		body["max_completion_tokens"] = *p.MaxCompletionTokens
	}
	if p.FrequencyPenalty != nil {
		body["frequency_penalty"] = *p.FrequencyPenalty
	}
	if p.PresencePenalty != nil {
		body["presence_penalty"] = *p.PresencePenalty
	}
	if p.N != nil {
		body["n"] = *p.N
	}
	if p.Logprobs != nil {
		body["logprobs"] = *p.Logprobs
	}
	if p.TopLogprobs != nil {
		body["top_logprobs"] = *p.TopLogprobs
	}
	if p.Store != nil {
		body["store"] = *p.Store
	}
	if p.LogitBias != nil {
		body["logit_bias"] = p.LogitBias
	}
	if p.Metadata != nil {
		body["metadata"] = p.Metadata
	}
	if len(p.Modalities) > 0 {
		body["modalities"] = p.Modalities
	}
	if p.Prediction != nil {
		body["prediction"] = p.Prediction
	}
	if len(p.PromptCacheOptions) > 0 { // an empty object is not a setting (SDK parity)
		body["prompt_cache_options"] = p.PromptCacheOptions
	}
	if p.WebSearchOptions != nil {
		body["web_search_options"] = p.WebSearchOptions
	}
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
// Chat-Completions wire messages for the OpenAI dialect. It is BuildMessagesFor
// bound to ProviderOpenAI; use BuildMessagesFor to map another vendor's
// dialect.
func BuildMessages(prompt []string, evs []events.Event) ([]map[string]any, error) {
	return BuildMessagesFor(ProviderOpenAI, prompt, evs)
}

// BuildMessagesFor converts the system prompt plus the event history into
// Chat-Completions wire messages: system first, then user turns (ModelRequest),
// assistant turns (ModelResponse, with replayed tool calls), tool answers
// (ToolResultsEvent) and compaction summaries. Everything else (usage events,
// streaming chunks, standalone tool-call events, human traffic) is skipped,
// Python parity.
//
// provider selects the input-part dialect: OpenAI and Mistral map their own
// media vocabulary, while the remaining engine users (Z.AI, DashScope) keep
// the image-only mapping their port carried before (Python parity:
// zai/mappers.py and dashscope/mappers.py accept images only).
func BuildMessagesFor(provider Provider, prompt []string, evs []events.Event) ([]map[string]any, error) {
	dialect := inputDialectFor(provider)
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
			msg, err := userMessage(dialect, e.Parts)
			if err != nil {
				return nil, err
			}
			out = append(out, msg)
		case events.DrainedModelRequest:
			msg, err := userMessage(dialect, e.Parts)
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

// inputDialect is the input-part vocabulary of one engine tenant. The shared
// engine serves vendors whose Python mappers differ, so the part mapping is
// selected per provider rather than guessed from the wire.
type inputDialect int

const (
	// inputDialectCompat is the image-only mapping the engine carried for
	// Z.AI and DashScope, which keep their own mappers in Python.
	inputDialectCompat inputDialect = iota
	// inputDialectOpenAI is openai/mappers.py convert_messages.
	inputDialectOpenAI
	// inputDialectMistral is mistral/mappers.py _content_from_input.
	inputDialectMistral
)

func inputDialectFor(provider Provider) inputDialect {
	// AGO-DEVIATION: the Go engine is shared, so the Z.AI and DashScope
	// bindings keep the pre-existing image-only mapping instead of porting
	// their own mappers (zai maps text only, dashscope has its own chunk
	// shape); changing them is a separate port.
	switch provider {
	case ProviderOpenAI:
		return inputDialectOpenAI
	case ProviderMistral:
		return inputDialectMistral
	default:
		return inputDialectCompat
	}
}

// userMessage maps one ModelRequest's parts to a user turn. A single text part
// collapses to a plain string (Python parity); otherwise the parts travel as an
// array.
func userMessage(dialect inputDialect, parts []events.Input) (map[string]any, error) {
	wire := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		w, err := inputPart(dialect, part)
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

// inputPart maps one input part to one wire content part. Every dialect
// carries text, data-as-text and images; audio, documents and provider files
// differ, and a part the dialect cannot carry fails fast with
// ErrUnsupportedInput (never silently dropped).
func inputPart(dialect inputDialect, part events.Input) (map[string]any, error) {
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
		switch dialect {
		case inputDialectOpenAI:
			return openAIBinaryPart(p)
		case inputDialectMistral:
			return mistralBinaryPart(p)
		}
		if p.Kind != events.BinaryImage {
			return nil, fmt.Errorf("%w: BinaryInput(%s) for the chat-completions API", ErrUnsupportedInput, p.Kind)
		}
		return map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": dataURL(p.MediaType, p.Data)},
		}, nil
	case events.UrlInput:
		switch dialect {
		case inputDialectOpenAI:
			if p.Kind != events.BinaryImage {
				return nil, fmt.Errorf("%w: UrlInput(%s) for the chat-completions API", ErrUnsupportedInput, p.Kind)
			}
		case inputDialectMistral:
			return mistralURLPart(p)
		default:
			if p.Kind != events.BinaryImage {
				return nil, fmt.Errorf("%w: UrlInput(%s) for the chat-completions API", ErrUnsupportedInput, p.Kind)
			}
		}
		return map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": p.URL},
		}, nil
	case events.FileIdInput:
		// An uploaded file travels by reference, in the OpenAI dialect only
		// (Python parity: the openai mapper's file part). The other tenants of
		// the shared engine have their own mappers in Python and fail fast.
		// AGO-DEVIATION: Python refuses a file uploaded through another
		// vendor's Files API; AGo's UploadedFile carries no provider tag, so
		// that check does not exist and the API answers a foreign file id.
		if dialect == inputDialectOpenAI {
			return map[string]any{
				"type": "file",
				"file": map[string]any{"file_id": p.FileID},
			}, nil
		}
		return nil, fmt.Errorf("%w: FileIdInput for the chat-completions API", ErrUnsupportedInput)
	default:
		return nil, fmt.Errorf("%w: %T for the chat-completions API", ErrUnsupportedInput, part)
	}
}

// audioFormats is the only set of input_audio formats Chat Completions
// accepts, keyed by MIME type (Python parity: _MIME_TO_AUDIO_FORMAT).
var audioFormats = map[string]string{
	"audio/wav":  "wav",
	"audio/mpeg": "mp3",
	"audio/mp3":  "mp3",
}

// chatImageDetails is the detail vocabulary Chat Completions knows (Python
// parity: _CHAT_IMAGE_DETAILS).
var chatImageDetails = map[string]bool{"auto": true, "low": true, "high": true}

// openAIBinaryPart maps inline bytes for the OpenAI Chat Completions dialect:
// audio as input_audio, images as a data URL with optional detail, documents
// as file_data under a filename (Python parity: openai/mappers.py).
func openAIBinaryPart(p events.BinaryInput) (map[string]any, error) {
	switch p.Kind {
	case events.BinaryAudio:
		format, ok := audioFormats[p.MediaType]
		if !ok {
			// The API answers anything else with "Supported values are: 'wav'
			// and 'mp3'"; fail here with the same set.
			return nil, fmt.Errorf("%w: BinaryInput(audio, media_type=%s) for the chat-completions API",
				ErrUnsupportedInput, p.MediaType)
		}
		return map[string]any{
			"type":        "input_audio",
			"input_audio": map[string]any{"data": base64.StdEncoding.EncodeToString(p.Data), "format": format},
		}, nil
	case events.BinaryImage:
		image := map[string]any{"url": dataURL(p.MediaType, p.Data)}
		detail, err := chatImageDetail(p.VendorMetadata)
		if err != nil {
			return nil, err
		}
		if detail != "" {
			image["detail"] = detail
		}
		return map[string]any{"type": "image_url", "image_url": image}, nil
	case events.BinaryDocument:
		return map[string]any{
			"type": "file",
			"file": map[string]any{
				"file_data": dataURL(p.MediaType, p.Data),
				"filename":  inlineFilename(p.VendorMetadata, p.MediaType),
			},
		}, nil
	default:
		return nil, fmt.Errorf("%w: BinaryInput(%s) for the chat-completions API", ErrUnsupportedInput, p.Kind)
	}
}

// chatImageDetail reads BinaryInput.vendor_metadata["detail"], refusing a value
// the API does not know (Python parity: _image_detail). An absent key means
// "no detail" and leaves the API default in place.
func chatImageDetail(vendor map[string]any) (string, error) {
	raw, ok := vendor["detail"]
	if !ok {
		return "", nil
	}
	detail, ok := raw.(string)
	if !ok || !chatImageDetails[detail] {
		return "", fmt.Errorf("%w: BinaryInput(image, detail=%v) for the chat-completions API",
			ErrUnsupportedInput, raw)
	}
	return detail, nil
}

// inlineFilename is the name inline file data travels under; the API requires
// one (Python parity: _filename). No vendor name means the media subtype, so
// application/pdf becomes file.pdf.
func inlineFilename(vendor map[string]any, mediaType string) string {
	if s, ok := vendor["filename"].(string); ok && s != "" {
		return s
	}
	suffix := mediaType
	if i := strings.LastIndex(suffix, "/"); i >= 0 {
		suffix = suffix[i+1:]
	}
	if i := strings.Index(suffix, "+"); i >= 0 {
		suffix = suffix[:i]
	}
	return "file." + suffix
}

// mistralBinaryPart maps inline bytes for the Mistral dialect: images as a
// data URL with optional detail, documents (and untyped binaries) as
// document_url with an optional document_name (Python parity:
// mistral/mappers.py _content_from_input).
func mistralBinaryPart(p events.BinaryInput) (map[string]any, error) {
	switch p.Kind {
	case events.BinaryImage:
		return mistralImagePart(dataURL(p.MediaType, p.Data), p.VendorMetadata)
	case events.BinaryDocument, events.BinaryBinary:
		chunk := map[string]any{
			"type":         "document_url",
			"document_url": dataURL(p.MediaType, p.Data),
		}
		if name, ok := p.VendorMetadata["filename"].(string); ok && name != "" {
			chunk["document_name"] = name
		}
		return chunk, nil
	default:
		return nil, fmt.Errorf("%w: BinaryInput(%s) for the Mistral chat API", ErrUnsupportedInput, p.Kind)
	}
}

// mistralURLPart maps a URL part for the Mistral dialect.
//
// AGO-DEVIATION: UrlInput has no metadata container in the Go event
// vocabulary, so a document URL travels without document_name and an image URL
// without detail (Python reads UrlInput.metadata for both).
func mistralURLPart(p events.UrlInput) (map[string]any, error) {
	switch p.Kind {
	case events.BinaryImage:
		return map[string]any{"type": "image_url", "image_url": p.URL}, nil
	case events.BinaryDocument, events.BinaryBinary:
		return map[string]any{"type": "document_url", "document_url": p.URL}, nil
	default:
		return nil, fmt.Errorf("%w: UrlInput(%s) for the Mistral chat API", ErrUnsupportedInput, p.Kind)
	}
}

// mistralImagePart renders one image chunk: a bare URL unless vendor_metadata
// carries a detail level (Python parity: _image_chunk). Any string detail is
// forwarded as-is; a non-string one cannot be put on the wire.
func mistralImagePart(url string, vendor map[string]any) (map[string]any, error) {
	raw, ok := vendor["detail"]
	if !ok {
		return map[string]any{"type": "image_url", "image_url": url}, nil
	}
	detail, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("%w: BinaryInput(image, detail=%v) for the Mistral chat API", ErrUnsupportedInput, raw)
	}
	return map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": url, "detail": detail},
	}, nil
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
		// Hosted/builtin tools (web_search, file_search, image_generation,
		// shell, ...) are mapped only by the Responses API (Python
		// tool_to_responses_api in ag2/config/openai/mappers.py); the Chat
		// Completions dialect this engine speaks rejects them the same way
		// (Python tool_to_api: UnsupportedToolError for every hosted schema).
		// This port carries Completions only, so hosted tools fail fast here.
		return nil, fmt.Errorf("%w: hosted tool %q for the chat-completions API "+
			"(hosted tools need the Responses API, which is not ported)", ErrUnsupportedTool, t.SchemaType())
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
// completion_tokens_details.reasoning_tokens → ThinkingTokens. Chat
// Completions reports no per-retention cache-write breakdown, so
// CacheCreationExtendedInputTokens stays nil. Fractional values are rounded
// (D-006: token counts are integers).
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
		// Content is a plain string in most dialects and a chunk array when
		// Mistral answers with reasoning (ThinkChunk) or prompt-mode chunks.
		Content   json.RawMessage `json:"content"`
		Reasoning string          `json:"reasoning"`
		ToolCalls []wireToolCall  `json:"tool_calls"`
	} `json:"message"`
}

// splitWireContent separates a message content value into answer text and
// reasoning text (Python parity: mistral/mappers.py split_content).
//
// AGO-DEVIATION: Python splits content only in the Mistral client; the Go
// engine is shared, so the split applies to every dialect. Only a "thinking"
// chunk produces reasoning and no other dialect emits one, so the difference
// is unobservable for them.
func splitWireContent(raw json.RawMessage) (text, reasoning string, err error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", "", nil
	}
	var plain string
	if err := json.Unmarshal(raw, &plain); err == nil {
		return plain, "", nil
	}
	var chunks []json.RawMessage
	if err := json.Unmarshal(raw, &chunks); err != nil {
		return "", "", fmt.Errorf("config: decoding message content: %w", err)
	}
	var (
		texts  strings.Builder
		traces strings.Builder
	)
	for i, rawChunk := range chunks {
		var str string
		if err := json.Unmarshal(rawChunk, &str); err == nil {
			texts.WriteString(str)
			continue
		}
		var chunk struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking json.RawMessage `json:"thinking"`
		}
		if err := json.Unmarshal(rawChunk, &chunk); err != nil {
			return "", "", fmt.Errorf("config: decoding message content chunk %d: %w", i, err)
		}
		switch chunk.Type {
		case "text":
			texts.WriteString(chunk.Text)
		case "thinking":
			// ThinkChunk.thinking is itself a chunk list (usually TextChunk),
			// occasionally a bare string.
			think, err := thinkText(chunk.Thinking)
			if err != nil {
				return "", "", err
			}
			traces.WriteString(think)
		}
		// Reference, file and tool chunks carry citation metadata, not answer
		// text (Python parity: skipped).
	}
	return texts.String(), traces.String(), nil
}

// thinkText joins the text of a ThinkChunk's thinking payload.
func thinkText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var plain string
	if err := json.Unmarshal(raw, &plain); err == nil {
		return plain, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return "", fmt.Errorf("config: decoding thinking chunk: %w", err)
	}
	var out strings.Builder
	for i, item := range items {
		var str string
		if err := json.Unmarshal(item, &str); err == nil {
			out.WriteString(str)
			continue
		}
		var chunk struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(item, &chunk); err != nil {
			return "", fmt.Errorf("config: decoding thinking chunk %d: %w", i, err)
		}
		if chunk.Type == "text" {
			out.WriteString(chunk.Text)
		}
	}
	return out.String(), nil
}

func runChatOnce(ctx context.Context, client *httpx.Client, url string, headers map[string]string, body map[string]any, conv *agocontext.Context, provider Provider) (*events.ModelResponse, error) {
	status, data, err := client.PostJSON(ctx, url, headers, body)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, NewAPIError(provider, "config: chat completions", status, nil, data)
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
		text, reasoning, err := splitWireContent(choice.Message.Content)
		if err != nil {
			return nil, err
		}
		if reasoning != "" {
			if err := sendEvent(conv, &events.ModelReasoning{Content: reasoning}); err != nil {
				return nil, err
			}
		}
		if text != "" {
			msg := &events.ModelMessage{Content: text}
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
			Content          json.RawMessage `json:"content"`
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
			text, reasoning, err := splitWireContent(choice.Delta.Content)
			if err != nil {
				return err
			}
			if reasoning != "" {
				if err := sendEvent(conv, &events.ModelReasoning{Content: reasoning}); err != nil {
					return err
				}
			}
			if text != "" {
				content.WriteString(text)
				if err := sendEvent(conv, &events.ModelMessageChunk{Content: text}); err != nil {
					return err
				}
			}
			for _, tc := range choice.Delta.ToolCalls {
				ix := 0
				switch {
				case tc.Index != nil:
					ix = *tc.Index
				case (tc.ID != nil && *tc.ID != "") || len(calls) == 0:
					// Some OpenAI-compatible servers (e.g. Gemini) omit the
					// index: a delta carrying an id starts a new call.
					ix = len(calls)
				default:
					// A delta without an index or id continues the last call.
					ix = len(calls) - 1
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
		return nil, WrapStatusError(provider, "config: chat completions", err)
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
